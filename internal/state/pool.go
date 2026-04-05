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
type MatchEvent struct {
	SessionID string
	PlayerIDs []string
	CreatedAt time.Time
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
// MMR values are as close together as possible. It searches the bracket with
// the most players first, then expands to neighbours.
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

	// Sliding window: find the window of PlayersPerMatch with the smallest
	// MMR spread (max - min). This runs in O(N) where N ≤ bracket_size * 5.
	bestStart := 0
	bestSpread := int32(MaxMMR + 1)
	for i := 0; i <= len(candidates)-PlayersPerMatch; i++ {
		spread := candidates[i+PlayersPerMatch-1].MMR - candidates[i].MMR
		if spread < bestSpread {
			bestSpread = spread
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

	slog.Info("match extracted",
		"count", PlayersPerMatch,
		"mmr_spread", bestSpread,
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
func removeFromSlice(s []*QueuedPlayer, id string) []*QueuedPlayer {
	for i, qp := range s {
		if qp.PlayerID == id {
			// Remove without allocating a new slice.
			copy(s[i:], s[i+1:])
			s[len(s)-1] = nil // allow GC of the pointer
			return s[:len(s)-1]
		}
	}
	return s
}
