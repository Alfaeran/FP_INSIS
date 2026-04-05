// Package services — tracker.go implements the SessionTrackerService.
//
// This service exposes two Unary RPCs:
//   - SubmitResult: Records a player's match outcome, validates the payload
//     strictly, and returns the computed MMR delta.
//   - GetSession: Returns a snapshot of a session and its results.
//
// All validation errors return appropriate gRPC status codes per the rubric.
package services

import (
	"context"
	"log/slog"
	"strings"

	pb "nexus-match/pkg/pb"
	"nexus-match/internal/state"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ─────────────────────────────────────────────────────────────
// TrackerServer
// ─────────────────────────────────────────────────────────────

// TrackerServer implements pb.SessionTrackerServiceServer.
type TrackerServer struct {
	pb.UnimplementedSessionTrackerServiceServer
	sessions *state.SessionStore
}

// NewTrackerServer creates a tracker wired to the shared session store.
func NewTrackerServer(sessions *state.SessionStore) *TrackerServer {
	return &TrackerServer{sessions: sessions}
}

// SubmitResult records a player's final match result.
// Validates the payload and returns INVALID_ARGUMENT for malformed requests,
// NOT_FOUND for unknown sessions, and ALREADY_EXISTS for duplicate submissions.
func (t *TrackerServer) SubmitResult(
	ctx context.Context,
	req *pb.SubmitResultRequest,
) (*pb.SubmitResultResponse, error) {

	// ── Payload validation ──────────────────────────────────────────────
	if req.SessionId == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}
	if req.PlayerId == "" {
		return nil, status.Error(codes.InvalidArgument, "player_id is required")
	}
	if req.Result == pb.MatchResult_MATCH_RESULT_UNSPECIFIED {
		return nil, status.Error(codes.InvalidArgument, "result must be WIN, LOSS, or DRAW")
	}
	if req.DurationS < 0 {
		return nil, status.Error(codes.InvalidArgument, "duration_s must be non-negative")
	}

	// ── Check context deadline ──────────────────────────────────────────
	if ctx.Err() != nil {
		return nil, status.Error(codes.DeadlineExceeded, "request deadline exceeded before processing")
	}

	// ── Record result ───────────────────────────────────────────────────
	delta, err := t.sessions.RecordResult(
		req.SessionId,
		req.PlayerId,
		int32(req.Result),
		req.Score,
	)
	if err != nil {
		slog.Warn("tracker: result rejected",
			"session_id", req.SessionId,
			"player_id", req.PlayerId,
			"err", err,
		)
		// Map domain errors to gRPC codes using standard library.
		errMsg := err.Error()
		switch {
		case strings.Contains(errMsg, "not found"):
			return nil, status.Errorf(codes.NotFound, "%v", err)
		case strings.Contains(errMsg, "not part of session"):
			return nil, status.Errorf(codes.PermissionDenied, "%v", err)
		case strings.Contains(errMsg, "already submitted"):
			return nil, status.Errorf(codes.AlreadyExists, "%v", err)
		default:
			return nil, status.Errorf(codes.Internal, "%v", err)
		}
	}

	slog.Info("tracker: result recorded",
		"session_id", req.SessionId,
		"player_id", req.PlayerId,
		"result", req.Result.String(),
		"mmr_delta", delta,
	)

	return &pb.SubmitResultResponse{
		Accepted: true,
		Message:  "Result recorded successfully.",
		NewMmr:   delta, // The client can add this to their local MMR cache.
	}, nil
}

// GetSession returns a full session snapshot.
func (t *TrackerServer) GetSession(
	ctx context.Context,
	req *pb.GetSessionRequest,
) (*pb.SessionSummary, error) {

	if req.SessionId == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}

	session := t.sessions.GetSession(req.SessionId)
	if session == nil {
		return nil, status.Errorf(codes.NotFound, "session %s not found", req.SessionId)
	}

	// Build protobuf response from the in-memory session.
	results := make([]*pb.PlayerResult, 0, len(session.Results))
	for _, r := range session.Results {
		results = append(results, &pb.PlayerResult{
			PlayerId: r.PlayerID,
			Result:   pb.MatchResult(r.Result),
			Score:    r.Score,
			MmrDelta: r.MMRDelta,
		})
	}

	return &pb.SessionSummary{
		SessionId:   session.SessionID,
		Results:     results,
		StartedAtMs: session.StartedAt.UnixMilli(),
		EndedAtMs:   session.EndedAt.UnixMilli(),
		Finalized:   session.Finalized,
	}, nil
}
