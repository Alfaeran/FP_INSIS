// Package state implements the core in-memory matchmaking pool.
//
// Design decisions:
//   - Players are bucketed by MMR brackets (every 500 MMR = one bracket).
//     This avoids a full O(N) scan on every match tick; we only need to look
//     at adjacent brackets when a single bracket is under-populated.
//   - A single sync.RWMutex guards the entire pool. We considered per-bracket
//     mutexes but the match worker must atomically pull players across brackets,
//     making fine-grained locking error prone with no real throughput gain at
//     our expected scale (< 100k concurrent).
//   - Players removed mid-search (cancel / disconnect) are tombstoned first so
//     concurrent readers see a consistent snapshot while the writer drains.
//
// Deadlock prevention:
//   - There is exactly ONE mutex (pool.mu). No nested locking.
//   - All exported methods acquire and release the lock within the same call frame.
//   - The match worker calls ExtractMatch which holds the lock for the duration
//     of player selection + removal. This is O(bracket_size) and bounded by the
//     bracket cap, so the critical section is short.
//
// Anti-starvation (v2):
//   - ExtractMatch now uses a weighted score instead of raw MMR spread.
//     Score = MMR_Spread - (AvgWaitSeconds * WaitWeight).
//     Windows with long-waiting players get a lower (better) score even if
//     their MMR spread is slightly wider, preventing indefinite starvation.
//
// Slice memory management (v2):
//   - removeFromSlice now shrinks the backing array when len < cap/4,
//     preventing long-term memory leaks from bracket slices that grew large
//     during peak load and then drained.
package state

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────
// Constants
// ─────────────────────────────────────────────────────────────

const (
	// BracketSize is the MMR range per bracket. Smaller = tighter matches,
	// larger = faster queue times.
	BracketSize = 500

	// MaxMMR is the upper bound for ratings, used to pre-size the bracket map.
	MaxMMR = 10000

	// PlayersPerMatch is the number of players required to form one match.
	PlayersPerMatch = 10

	// MaxBracketExpansion controls how many adjacent brackets the matcher
	// may search when a single bracket is under-populated.
	MaxBracketExpansion = 2

	// WaitWeight controls how aggressively the matcher favours long-waiting
	// players. Each second of average wait time in a candidate window reduces
	// the effective score by this many MMR points. A value of 5 means that
	// after 20 seconds, a window is treated as if its spread were 100 MMR
	// tighter, making it likely to be chosen over a fresher, tighter cluster.
	WaitWeight float64 = 5.0
)

// ─────────────────────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────────────────────

// QueuedPlayer holds everything about a player currently in the pool.
// Embedding a channel allows the gateway service to push match assignments
// directly without polling.
type QueuedPlayer struct {
	PlayerID    string
	DisplayName string
	MMR         int32
	EnqueuedAt  time.Time

	// MatchCh is written to exactly once when the player is assigned a match.
	// Buffer size 1 prevents the match worker from blocking if the player's
	// stream handler hasn't started reading yet.
	MatchCh chan MatchEvent
}

// MatchEvent is the payload delivered through QueuedPlayer.MatchCh.
//
// Channel protocol for the Ready Check flow:
//   - AcceptCh (buffered 1): The gateway handler writes once when the client
//     sends AcceptMatchAction. The coordinator reads it within the timeout.
//   - ResultCh (unbuffered): The coordinator closes this channel after writing
//     one bool: true = room confirmed, false = room cancelled. All gateway
//     handlers select on it to learn the outcome.
type MatchEvent struct {
	SessionID string
	PlayerIDs []string
	CreatedAt time.Time

	// AcceptCh: gateway → coordinator. Buffered(1) so the gateway
	// never blocks even if the coordinator hasn't started reading.
	AcceptCh chan struct{}

	// ResultCh: coordinator → gateways. The coordinator sends exactly one
	// bool (true=confirmed, false=cancelled), then closes the channel.
	ResultCh chan bool
}

// Pool is the thread-safe in-memory matchmaking queue.
type Pool struct {
	// mu guards both `players` and `brackets`.
	// Invariant: every player in `players` appears in exactly one bracket slice.
	mu sync.RWMutex

	// players is the primary index: playerID → *QueuedPlayer.
	players map[string]*QueuedPlayer

	// brackets maps bracketKey → ordered slice of players in that bracket.
	// Slice is kept sorted by MMR ascending for efficient nearest-neighbor extraction.
	brackets map[int][]*QueuedPlayer
}

// NewPool allocates an empty pool ready for use.
func NewPool() *Pool {
	return &Pool{
		players:  make(map[string]*QueuedPlayer, 256),
		brackets: make(map[int][]*QueuedPlayer, MaxMMR/BracketSize+1),
	}
}

// ─────────────────────────────────────────────────────────────
// Bracket helpers (no lock; callers must hold mu)
// ─────────────────────────────────────────────────────────────

// bracketKey maps an MMR value to its bracket index.
func bracketKey(mmr int32) int {
	if mmr < 0 {
		return 0
	}
	return int(mmr) / BracketSize
}

// ─────────────────────────────────────────────────────────────
// Public API
// ─────────────────────────────────────────────────────────────

// Enqueue adds a player to the pool. Returns an error if the player is
// already queued (duplicate join protection). The caller receives the
// QueuedPlayer so it can select{} on MatchCh.
func (p *Pool) Enqueue(id, name string, mmr int32) (*QueuedPlayer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.players[id]; exists {
		return nil, fmt.Errorf("player %s already in queue", id)
	}

	qp := &QueuedPlayer{
		PlayerID:    id,
		DisplayName: name,
		MMR:         mmr,
		EnqueuedAt:  time.Now(),
		MatchCh:     make(chan MatchEvent, 1), // buffered: non-blocking send
	}

	p.players[id] = qp

	bk := bracketKey(mmr)
	p.brackets[bk] = insertSorted(p.brackets[bk], qp)

	slog.Info("player enqueued",
		"player_id", id,
		"mmr", mmr,
		"bracket", bk,
		"pool_size", len(p.players),
	)

	return qp, nil
}

// ReEnqueue places a player back into the pool after a failed ready-check.
// It directly inserts the existing QueuedPlayer struct so the gateway's
// select loop can continue listening on the exact same MatchCh.
func (p *Pool) ReEnqueue(qp *QueuedPlayer) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.players[qp.PlayerID]; exists {
		return fmt.Errorf("player %s already in queue", qp.PlayerID)
	}

	p.players[qp.PlayerID] = qp

	bk := bracketKey(qp.MMR)
	p.brackets[bk] = insertSorted(p.brackets[bk], qp)

	slog.Info("player re-enqueued (ready-check fail)",
		"player_id", qp.PlayerID,
		"mmr", qp.MMR,
		"pool_size", len(p.players),
	)

	return nil
}

// Dequeue removes a player from the pool (e.g. cancel or disconnect).
// Returns true if the player was found and removed.
func (p *Pool) Dequeue(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	qp, exists := p.players[id]
	if !exists {
		return false
	}

	// Remove from bracket slice.
	bk := bracketKey(qp.MMR)
	p.brackets[bk] = removeFromSlice(p.brackets[bk], id)
	if len(p.brackets[bk]) == 0 {
		delete(p.brackets, bk)
	}

	// Close channel to unblock any select{} in the gateway handler.
	// A closed channel returns the zero value, which the gateway interprets
	// as "cancelled / no match".
	close(qp.MatchCh)

	delete(p.players, id)

	slog.Info("player dequeued",
		"player_id", id,
		"pool_size", len(p.players),
	)

	return true
}

// Contains checks if a player is currently in the pool.
func (p *Pool) Contains(id string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.players[id]
	return ok
}

// Size returns the current number of players waiting in the pool.
func (p *Pool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.players)
}

// ExtractMatch attempts to pull PlayersPerMatch players from the pool whose
// MMR values are as close together as possible, with a bias toward players
// who have been waiting longer (anti-starvation).
//
// Scoring formula per sliding window:
//   Score = MMR_Spread - (Avg_Wait_Seconds * WaitWeight)
// Lower score = better window. A 10-player window where everyone has waited
// 30 seconds gets a -150 point bonus (at WaitWeight=5), so it beats a
// zero-spread window of players who just joined.
//
// On success, the returned players are REMOVED from the pool and their
// MatchCh channels are still open (the caller is responsible for writing the
// MatchEvent and then the channel will be GC'd).
//
// This method holds the write lock for the entire extraction so no other
// goroutine can pull the same players — this eliminates double-match races.
func (p *Pool) ExtractMatch() ([]*QueuedPlayer, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	// Find the bracket with the most players.
	bestBK := -1
	bestCount := 0
	for bk, slice := range p.brackets {
		if len(slice) > bestCount {
			bestCount = len(slice)
			bestBK = bk
		}
	}

	if bestBK == -1 {
		return nil, false
	}

	// Collect candidates from the best bracket ± expansion range.
	candidates := make([]*QueuedPlayer, 0, PlayersPerMatch*2)
	for delta := 0; delta <= MaxBracketExpansion; delta++ {
		if s, ok := p.brackets[bestBK+delta]; ok {
			candidates = append(candidates, s...)
		}
		if delta > 0 {
			if s, ok := p.brackets[bestBK-delta]; ok {
				candidates = append(candidates, s...)
			}
		}
	}

	if len(candidates) < PlayersPerMatch {
		return nil, false
	}

	// Sort candidates by MMR so we can pick the tightest cluster.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].MMR < candidates[j].MMR
	})

	// Sliding window with anti-starvation scoring.
	// For each window of PlayersPerMatch candidates, compute:
	//   score = float64(mmr_spread) - (avg_wait_seconds * WaitWeight)
	// The window with the LOWEST score wins.
	bestStart := 0
	bestScore := float64(MaxMMR+1) * 10 // sentinel: impossibly high

	for i := 0; i <= len(candidates)-PlayersPerMatch; i++ {
		spread := float64(candidates[i+PlayersPerMatch-1].MMR - candidates[i].MMR)

		// Compute average wait time for the window.
		var totalWaitSec float64
		for j := i; j < i+PlayersPerMatch; j++ {
			totalWaitSec += now.Sub(candidates[j].EnqueuedAt).Seconds()
		}
		avgWait := totalWaitSec / float64(PlayersPerMatch)

		// Lower score = better: tight spread is good, long waits make score lower.
		score := spread - (avgWait * WaitWeight)

		if score < bestScore {
			bestScore = score
			bestStart = i
		}
	}

	selected := candidates[bestStart : bestStart+PlayersPerMatch]

	// Atomically remove all selected players from the pool.
	for _, qp := range selected {
		bk := bracketKey(qp.MMR)
		p.brackets[bk] = removeFromSlice(p.brackets[bk], qp.PlayerID)
		if len(p.brackets[bk]) == 0 {
			delete(p.brackets, bk)
		}
		delete(p.players, qp.PlayerID)
	}

	mmrSpread := selected[PlayersPerMatch-1].MMR - selected[0].MMR

	slog.Info("match extracted",
		"count", PlayersPerMatch,
		"mmr_spread", mmrSpread,
		"weighted_score", bestScore,
		"pool_remaining", len(p.players),
	)

	return selected, true
}

// ─────────────────────────────────────────────────────────────
// Sorted-slice helpers (allocation-conscious)
// ─────────────────────────────────────────────────────────────

// insertSorted inserts qp into a slice that is already sorted by MMR ascending.
// Uses binary search for O(log N) index finding, then a single copy for insertion.
func insertSorted(s []*QueuedPlayer, qp *QueuedPlayer) []*QueuedPlayer {
	idx := sort.Search(len(s), func(i int) bool {
		return s[i].MMR >= qp.MMR
	})
	// Grow slice by one.
	s = append(s, nil)
	copy(s[idx+1:], s[idx:])
	s[idx] = qp
	return s
}

// removeFromSlice removes the first player with the given ID.
// Preserves order (stable removal). O(N) but N is bounded by bracket population.
//
// Memory management: after removal, if the slice length has dropped below 1/4
// of its capacity, we allocate a new smaller backing array and copy the data.
// This prevents long-term memory leaks where a bracket slice grew large during
// peak load (e.g., cap=1000) but now only holds a few players (len=20).
// The old backing array becomes unreachable and will be collected by the GC.
func removeFromSlice(s []*QueuedPlayer, id string) []*QueuedPlayer {
	for i, qp := range s {
		if qp.PlayerID == id {
			// Remove element, preserving order.
			copy(s[i:], s[i+1:])
			s[len(s)-1] = nil // nil the dangling pointer so GC can collect the QueuedPlayer
			s = s[:len(s)-1]

			// Shrink check: if capacity is 4x larger than length AND there are
			// elements remaining, reallocate to free the oversized backing array.
			if newLen := len(s); newLen > 0 && newLen < cap(s)/4 {
				shrunk := make([]*QueuedPlayer, newLen)
				copy(shrunk, s)
				return shrunk
			}

			return s
		}
	}
	return s
}
