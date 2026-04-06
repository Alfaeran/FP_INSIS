// Package services — matchmaker.go implements the MatchmakerService.
//
// Architecture (v2 — with Ready Check coordination):
//   - Background worker scans pool on a tick, calls pool.ExtractMatch().
//   - On extraction, instead of immediately creating a session, the worker
//     sends a MatchEvent with AcceptCh/ResultCh to each player's MatchCh,
//     then launches a readyCheckCoordinator goroutine.
//   - The coordinator waits up to 10 seconds for all 10 players to signal
//     acceptance on their AcceptCh. On success, it creates the session and
//     writes true to ResultCh. On failure (timeout / missing acceptance),
//     it writes false and re-enqueues the 9 players who DID accept.
//
// Channel closing protocol:
//   - stopCh: closed by StopWorker() → worker exits.
//   - MatchCh: buffered(1), written once by worker, read once by gateway.
//   - AcceptCh: buffered(1), written by gateway, read by coordinator.
//   - ResultCh: written once by coordinator (true/false), then closed.
//
// Deadlock-free: the coordinator never holds pool.mu while waiting on
// channels. All pool operations (ReEnqueue) acquire + release the lock
// within one call frame.
package services

import (
	"context"
	"log/slog"
	"sync"
	"time"

	pb "nexus-match/pkg/pb"
	"nexus-match/internal/state"

	"github.com/google/uuid"
)

const (
	// readyCheckCoordTimeout is the total time the coordinator waits for
	// all players to accept. Must match or slightly exceed the gateway's
	// readyCheckTimeout to account for network latency.
	readyCheckCoordTimeout = 12 * time.Second
)

// ─────────────────────────────────────────────────────────────
// MatchmakerServer
// ─────────────────────────────────────────────────────────────

// MatchmakerServer implements pb.MatchmakerServiceServer and runs the
// background match worker.
type MatchmakerServer struct {
	pb.UnimplementedMatchmakerServiceServer

	pool     *state.Pool
	sessions *state.SessionStore

	// tickInterval controls how frequently the worker scans the pool.
	tickInterval time.Duration

	// stopCh is closed to signal the worker to exit.
	// Never written to — only closed.
	stopCh chan struct{}

	// wg tracks the worker goroutine AND all coordinator goroutines
	// for clean shutdown.
	wg sync.WaitGroup

	// matchesCreated is a simple monotonic counter for observability.
	matchesCreated int64
	counterMu      sync.Mutex
}

// NewMatchmakerServer creates a new matchmaker wired to the shared state.
func NewMatchmakerServer(pool *state.Pool, sessions *state.SessionStore, tickInterval time.Duration) *MatchmakerServer {
	return &MatchmakerServer{
		pool:         pool,
		sessions:     sessions,
		tickInterval: tickInterval,
		stopCh:       make(chan struct{}),
	}
}

// StartWorker launches the background match scanning goroutine.
func (m *MatchmakerServer) StartWorker() {
	m.wg.Add(1)
	go m.worker()
	slog.Info("matchmaker: worker started", "tick_interval", m.tickInterval)
}

// StopWorker signals the worker to exit and waits for it + all coordinators.
func (m *MatchmakerServer) StopWorker() {
	defer func() { recover() }() // guard double-close
	close(m.stopCh)
	m.wg.Wait()
	slog.Info("matchmaker: worker stopped")
}

// ─────────────────────────────────────────────────────────────
// Worker loop
// ─────────────────────────────────────────────────────────────

func (m *MatchmakerServer) worker() {
	defer m.wg.Done()

	ticker := time.NewTicker(m.tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.tick()
		}
	}
}

// tick runs one cycle of match extraction. It loops until the pool can no
// longer produce a full match, draining as many matches as possible per tick.
func (m *MatchmakerServer) tick() {
	for {
		players, ok := m.pool.ExtractMatch()
		if !ok {
			return
		}

		sessionID := uuid.New().String()
		now := time.Now()

		playerIDs := make([]string, len(players))
		for i, p := range players {
			playerIDs[i] = p.PlayerID
		}

		// Create per-player ResponseCh channels and per-player ResultCh channels.
		resultChannels := make([]chan bool, len(players))
		responseChannels := make([]chan bool, len(players))

		for i := range players {
			responseChannels[i] = make(chan bool, 1)
			resultChannels[i] = make(chan bool, 1)
		}

		// Build MatchEvents with individual ResponseCh and ResultCh.
		for i, p := range players {
			evt := state.MatchEvent{
				SessionID:  sessionID,
				PlayerIDs:  playerIDs,
				CreatedAt:  now,
				ResponseCh: responseChannels[i],
				ResultCh:   resultChannels[i],
			}

			select {
			case p.MatchCh <- evt:
				// Delivered.
			default:
				// Player disconnected between extraction and notification.
				slog.Warn("matchmaker: failed to notify player",
					"player_id", p.PlayerID,
					"session_id", sessionID,
				)
			}
		}

		// Launch the ready-check coordinator in a separate goroutine.
		// It is tracked by m.wg so StopWorker waits for it.
		m.wg.Add(1)
		go m.readyCheckCoordinator(sessionID, players, responseChannels, resultChannels)

		m.counterMu.Lock()
		m.matchesCreated++
		total := m.matchesCreated
		m.counterMu.Unlock()

		slog.Info("matchmaker: ready-check initiated",
			"session_id", sessionID,
			"players", len(playerIDs),
			"total_matches_attempted", total,
		)
	}
}

// ─────────────────────────────────────────────────────────────
// Ready Check Coordinator
// ─────────────────────────────────────────────────────────────

// readyCheckCoordinator uses a fan-in pattern to wait for all players simultaneously.
// On success (all 10 true), it creates the session. On failure (ANY false or timeout),
// it FAST-FAILS immediately, aborting the room and re-enqueuing players who accepted.
//
// This goroutine holds NO locks while waiting. All pool/session operations
// are short, single-frame lock acquisitions.
func (m *MatchmakerServer) readyCheckCoordinator(
	sessionID string,
	players []*state.QueuedPlayer,
	responseChannels []chan bool,
	resultChannels []chan bool,
) {
	defer m.wg.Done()

	ctx, cancel := context.WithTimeout(context.Background(), readyCheckCoordTimeout)
	defer cancel()

	type fanInResp struct {
		idx      int
		accepted bool
	}
	fanInCh := make(chan fanInResp, len(players))

	// Fan-in: launch one goroutine per player to listen to their ResponseCh
	// concurrently. This prevents index 0 from holding the coordinator hostage.
	for i, ch := range responseChannels {
		go func(index int, respCh chan bool) {
			select {
			case val := <-respCh:
				fanInCh <- fanInResp{idx: index, accepted: val}
			case <-ctx.Done():
				// Main coordinator timeout triggered. Send false.
				fanInCh <- fanInResp{idx: index, accepted: false}
			}
		}(i, ch)
	}

	accepted := make([]bool, len(players))
	allAccepted := true
	responsesReceived := 0

	// Gather responses as they arrive.
	for responsesReceived < len(players) {
		select {
		case resp := <-fanInCh:
			responsesReceived++
			accepted[resp.idx] = resp.accepted

			if resp.accepted {
				slog.Info("matchmaker: player accepted",
					"player_id", players[resp.idx].PlayerID,
					"session_id", sessionID,
				)
			} else {
				// FAST FAIL! Someone actively declined or disconnected.
				// We do not wait for the others.
				allAccepted = false
				slog.Warn("matchmaker: fast-fail ready check (player declined)",
					"player_id", players[resp.idx].PlayerID,
					"session_id", sessionID,
				)
				goto verdict
			}
		case <-ctx.Done():
			// Timeout reached. Anyone who hasn't answered is considered false.
			allAccepted = false
			slog.Warn("matchmaker: room fast-fail (timeout)",
				"session_id", sessionID,
			)
			goto verdict
		}
	}

verdict:
	// Cancel the context early to free the remaining fan-in goroutines
	// if we fast-failed before they finished.
	cancel()

	if allAccepted {
		// All 10 players accepted → create the session.
		m.sessions.CreateSession(sessionID, func() []string {
			ids := make([]string, len(players))
			for i, p := range players {
				ids[i] = p.PlayerID
			}
			return ids
		}())

		slog.Info("matchmaker: session confirmed",
			"session_id", sessionID,
			"players", len(players),
		)

		// Notify all gateways: room confirmed.
		for _, ch := range resultChannels {
			ch <- true
			close(ch)
		}
		return
	}

	// At least one player did not accept → cancel the room.
	slog.Warn("matchmaker: room cancelled (ready-check failed)",
		"session_id", sessionID,
	)

	// Notify all gateways: room cancelled.
	for _, ch := range resultChannels {
		ch <- false
		close(ch)
	}

	// Re-enqueue players who DID accept. They should not lose their
	// queue position or their open channels.
	for i, p := range players {
		if accepted[i] {
			if err := m.pool.ReEnqueue(p); err != nil {
				slog.Warn("matchmaker: re-enqueue failed",
					"player_id", p.PlayerID,
					"err", err,
				)
			} else {
				slog.Info("matchmaker: player re-enqueued after failed ready-check",
					"player_id", p.PlayerID,
				)
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────
// gRPC Endpoints (admin / debug)
// ─────────────────────────────────────────────────────────────

// GetQueueStats returns current pool and session statistics.
func (m *MatchmakerServer) GetQueueStats(
	_ context.Context,
	_ *pb.QueueStatsRequest,
) (*pb.QueueStatsResponse, error) {
	return &pb.QueueStatsResponse{
		TotalWaiting:   int32(m.pool.Size()),
		ActiveSessions: int32(m.sessions.ActiveCount()),
	}, nil
}

// ForceMatch triggers an immediate match cycle and reports how many matches
// were created.
func (m *MatchmakerServer) ForceMatch(
	_ context.Context,
	_ *pb.ForceMatchRequest,
) (*pb.ForceMatchResponse, error) {
	countBefore := m.getMatchCount()
	m.tick()
	countAfter := m.getMatchCount()

	return &pb.ForceMatchResponse{
		MatchesCreated: int32(countAfter - countBefore),
	}, nil
}

func (m *MatchmakerServer) getMatchCount() int64 {
	m.counterMu.Lock()
	defer m.counterMu.Unlock()
	return m.matchesCreated
}
