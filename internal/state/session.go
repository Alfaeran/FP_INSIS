// Package state — session.go manages completed/active session records in memory.
//
// Design:
//   - A sync.RWMutex guards the session map. Reads (GetSession) use RLock,
//     writes (CreateSession, RecordResult) use Lock.
//   - Each session tracks which players have submitted results. Once all
//     players submit, the session is marked finalized.
//   - MMR adjustment is a simplified Elo-like delta: winners gain points,
//     losers lose points, draws are neutral. In production this would delegate
//     to a dedicated rating engine.
//
// There is no nested locking — session.go never calls into pool.go while
// holding its own lock, and vice versa.
package state

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────
// Constants
// ─────────────────────────────────────────────────────────────

const (
	// MMR deltas for a simplified rating system.
	MMRWinDelta  = 25
	MMRLossDelta = -25
	MMRDrawDelta = 0
)

// ─────────────────────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────────────────────

// ResultEntry captures one player's submission for a session.
type ResultEntry struct {
	PlayerID string
	Result   int32 // maps to MatchResult enum value
	Score    int32
	MMRDelta int32
}

// Session holds the full lifecycle of a single match.
type Session struct {
	SessionID   string
	PlayerIDs   []string
	Results     map[string]*ResultEntry // playerID → result
	StartedAt   time.Time
	EndedAt     time.Time
	Finalized   bool
}

// SessionStore is the thread-safe registry for all sessions.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewSessionStore returns an initialized store.
func NewSessionStore() *SessionStore {
	return &SessionStore{
		sessions: make(map[string]*Session, 128),
	}
}

// ─────────────────────────────────────────────────────────────
// Public API
// ─────────────────────────────────────────────────────────────

// CreateSession registers a new session. Called by the match worker after
// extracting players from the pool.
func (ss *SessionStore) CreateSession(sessionID string, playerIDs []string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	ss.sessions[sessionID] = &Session{
		SessionID: sessionID,
		PlayerIDs: playerIDs,
		Results:   make(map[string]*ResultEntry, len(playerIDs)),
		StartedAt: time.Now(),
	}

	slog.Info("session created",
		"session_id", sessionID,
		"players", len(playerIDs),
	)
}

// RecordResult writes a player's match outcome into the session.
// Returns the computed MMR delta and an error if validation fails.
func (ss *SessionStore) RecordResult(sessionID, playerID string, result int32, score int32) (int32, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	session, ok := ss.sessions[sessionID]
	if !ok {
		return 0, fmt.Errorf("session %s not found", sessionID)
	}

	// Check that the player actually belongs to this session.
	found := false
	for _, pid := range session.PlayerIDs {
		if pid == playerID {
			found = true
			break
		}
	}
	if !found {
		return 0, fmt.Errorf("player %s is not part of session %s", playerID, sessionID)
	}

	// Prevent duplicate submissions.
	if _, dup := session.Results[playerID]; dup {
		return 0, fmt.Errorf("player %s already submitted result for session %s", playerID, sessionID)
	}

	// Compute MMR delta.
	var delta int32
	switch result {
	case 1: // WIN
		delta = MMRWinDelta
	case 2: // LOSS
		delta = MMRLossDelta
	case 3: // DRAW
		delta = MMRDrawDelta
	default:
		return 0, fmt.Errorf("invalid result value: %d", result)
	}

	session.Results[playerID] = &ResultEntry{
		PlayerID: playerID,
		Result:   result,
		Score:    score,
		MMRDelta: delta,
	}

	// Check if all players have submitted.
	if len(session.Results) == len(session.PlayerIDs) {
		session.Finalized = true
		session.EndedAt = time.Now()
		slog.Info("session finalized",
			"session_id", sessionID,
		)
	}

	return delta, nil
}

// GetSession returns a read-only snapshot of a session.
// Returns nil if not found.
func (ss *SessionStore) GetSession(sessionID string) *Session {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	s, ok := ss.sessions[sessionID]
	if !ok {
		return nil
	}
	return s
}

// ActiveCount returns the number of non-finalized sessions.
func (ss *SessionStore) ActiveCount() int {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	count := 0
	for _, s := range ss.sessions {
		if !s.Finalized {
			count++
		}
	}
	return count
}
