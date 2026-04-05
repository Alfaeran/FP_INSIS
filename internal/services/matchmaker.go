// Package services — matchmaker.go implements the MatchmakerService.
//
// Architecture:
//   - A background worker goroutine runs on a configurable tick interval.
//   - On each tick, it calls pool.ExtractMatch() which atomically pulls
//     PlayersPerMatch players from the pool.
//   - For each successful extraction, it generates a UUID session, creates
//     a Session in the SessionStore, and then writes a MatchEvent to each
//     player's MatchCh channel.
//   - The worker is started via StartWorker() and stopped via StopWorker().
//
// Channel closing protocol:
//   - The stopCh channel is closed by StopWorker() to signal the worker
//     goroutine to exit. We never write to stopCh; we only close it.
//   - Each QueuedPlayer.MatchCh is a buffered(1) channel. The worker writes
//     exactly once. If the player's gateway handler is slow, the write won't
//     block because the buffer absorbs it.
//
// Deadlock-free: the worker holds no locks itself. All locking is internal
// to pool.ExtractMatch() calls, which is a single-mutex design.
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

	// wg tracks the worker goroutine for clean shutdown.
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
// Call this once after creating the server.
func (m *MatchmakerServer) StartWorker() {
	m.wg.Add(1)
	go m.worker()
	slog.Info("matchmaker: worker started", "tick_interval", m.tickInterval)
}

// StopWorker signals the worker to exit and waits for it to finish.
// Safe to call multiple times (close on already-closed channel is caught).
func (m *MatchmakerServer) StopWorker() {
	defer func() {
		// Guard against double-close panic.
		recover()
	}()
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
			return // not enough players for another match
		}

		sessionID := uuid.New().String()
		now := time.Now()

		// Gather player IDs.
		playerIDs := make([]string, len(players))
		for i, p := range players {
			playerIDs[i] = p.PlayerID
		}

		// Register the session.
		m.sessions.CreateSession(sessionID, playerIDs)

		// Build the event.
		evt := state.MatchEvent{
			SessionID: sessionID,
			PlayerIDs: playerIDs,
			CreatedAt: now,
		}

		// Notify each player through their buffered channel.
		// Because MatchCh has capacity 1 and is written exactly once,
		// this send will never block.
		for _, p := range players {
			select {
			case p.MatchCh <- evt:
				// Delivered successfully.
			default:
				// Channel is full or closed — player disconnected between
				// extraction and notification. Log and move on.
				slog.Warn("matchmaker: failed to notify player",
					"player_id", p.PlayerID,
					"session_id", sessionID,
				)
			}
		}

		m.counterMu.Lock()
		m.matchesCreated++
		total := m.matchesCreated
		m.counterMu.Unlock()

		slog.Info("matchmaker: match created",
			"session_id", sessionID,
			"players", len(playerIDs),
			"total_matches", total,
		)
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
