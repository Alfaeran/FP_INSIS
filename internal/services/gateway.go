// Package services — gateway.go implements the PlayerGatewayService.
//
// Stream lifecycle (v2 — with Ready Check):
//   1. Client sends JoinQueueAction (first message).
//   2. Server enqueues and pushes WAITING updates on a ticker.
//   3. Matchmaker extracts 10 players → sends ReadyCheckEvent via MatchCh.
//   4. Gateway pushes QUEUE_STATUS_READY_CHECK to client with MatchAssignment.
//   5. Client has 10 seconds to reply with AcceptMatchAction.
//   6. Gateway signals acceptance on the ReadyCheckEvent.AcceptCh.
//   7. The matchmaker's readyCheckCoordinator waits for all 10.
//      - All accept → session created → IN_GAME pushed.
//      - Any timeout/decline → room cancelled → remaining 9 re-enqueued.
//   8. If client disconnects at any point, deferred cleanup removes from pool.
//
// Channel discipline:
//   - MatchCh: buffered(1), written once by matchmaker, read once by gateway.
//   - ReadyCheckEvent.AcceptCh: buffered(1), written once by gateway per player.
//   - ReadyCheckEvent.ResultCh: unbuffered broadcast channel, closed by coordinator.
//   - No goroutine holds pool.mu while waiting on a channel → no deadlock.
package services

import (
	"io"
	"log/slog"
	"time"

	pb "nexus-match/pkg/pb"
	"nexus-match/internal/state"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// readyCheckTimeout is how long the server waits for a client to accept.
	readyCheckTimeout = 10 * time.Second
)

// ─────────────────────────────────────────────────────────────
// GatewayServer
// ─────────────────────────────────────────────────────────────

// GatewayServer implements pb.PlayerGatewayServiceServer.
type GatewayServer struct {
	pb.UnimplementedPlayerGatewayServiceServer
	pool *state.Pool
}

// NewGatewayServer wires the gateway to the shared pool.
func NewGatewayServer(pool *state.Pool) *GatewayServer {
	return &GatewayServer{pool: pool}
}

// EnterMatchmaking is the bi-directional streaming RPC.
func (g *GatewayServer) EnterMatchmaking(stream grpc.BidiStreamingServer[pb.GatewayRequest, pb.GatewayResponse]) error {
	ctx := stream.Context()

	// ── Step 1: Read the first message; must be JoinQueueAction ──────────
	firstMsg, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to receive initial message: %v", err)
	}

	join := firstMsg.GetJoinQueue()
	if join == nil || join.Player == nil {
		return status.Error(codes.InvalidArgument, "first message must be JoinQueueAction with PlayerInfo")
	}

	player := join.Player
	if player.PlayerId == "" {
		return status.Error(codes.InvalidArgument, "player_id must not be empty")
	}
	if player.Mmr < 0 || player.Mmr > state.MaxMMR {
		return status.Errorf(codes.InvalidArgument, "mmr must be in [0, %d]", state.MaxMMR)
	}

	// ── Step 2: Enqueue ──────────────────────────────────────────────────
	qp, enqErr := g.pool.Enqueue(player.PlayerId, player.DisplayName, player.Mmr)
	if enqErr != nil {
		return status.Errorf(codes.AlreadyExists, "enqueue failed: %v", enqErr)
	}

	slog.Info("gateway: player joined stream",
		"player_id", player.PlayerId,
		"mmr", player.Mmr,
	)

	// Ensure cleanup on any exit path.
	defer func() {
		g.pool.Dequeue(player.PlayerId)
	}()

	// Send initial WAITING acknowledgment.
	if err := stream.Send(&pb.GatewayResponse{
		Status:            pb.QueueStatus_QUEUE_STATUS_WAITING,
		Message:           "You are in the queue. Searching for opponents...",
		ServerTimestampMs: time.Now().UnixMilli(),
	}); err != nil {
		return status.Errorf(codes.Internal, "send failed: %v", err)
	}

	// ── Step 3: Client reader goroutine ──────────────────────────────────
	// clientDone is closed on EOF / transport error.
	clientDone := make(chan struct{})
	// cancelReq signals an explicit CancelQueueAction.
	cancelReq := make(chan string, 1)
	// acceptReq signals an AcceptMatchAction from the client.
	acceptReq := make(chan struct{}, 1)

	go func() {
		defer close(clientDone)
		for {
			msg, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					slog.Info("gateway: client closed stream", "player_id", player.PlayerId)
				} else {
					slog.Warn("gateway: recv error", "player_id", player.PlayerId, "err", err)
				}
				return
			}

			switch {
			case msg.GetHeartbeat() != nil:
				slog.Debug("gateway: heartbeat", "player_id", player.PlayerId)

			case msg.GetCancelQueue() != nil:
				slog.Info("gateway: cancel requested", "player_id", player.PlayerId)
				cancelReq <- player.PlayerId
				return

			case msg.GetAcceptMatch() != nil:
				slog.Info("gateway: accept match received", "player_id", player.PlayerId)
				select {
				case acceptReq <- struct{}{}:
				default:
					// Already accepted; ignore duplicate.
				}
			}
		}
	}()

	// Status broadcast ticker.
	statusTicker := time.NewTicker(2 * time.Second)
	defer statusTicker.Stop()

	for {
		select {

		// ── Match found → Ready Check ───────────────────────────
		case evt, ok := <-qp.MatchCh:
			if !ok {
				// Channel closed → player dequeued externally.
				_ = stream.Send(&pb.GatewayResponse{
					Status:            pb.QueueStatus_QUEUE_STATUS_CANCELLED,
					Message:           "Queue cancelled.",
					ServerTimestampMs: time.Now().UnixMilli(),
				})
				return status.Error(codes.Aborted, "player removed from queue")
			}

			slog.Info("gateway: ready-check started",
				"player_id", player.PlayerId,
				"session_id", evt.SessionID,
			)

			// Push READY_CHECK to client.
			if err := stream.Send(&pb.GatewayResponse{
				Status:            pb.QueueStatus_QUEUE_STATUS_READY_CHECK,
				Message:           "Match found! Accept within 10 seconds.",
				ServerTimestampMs: time.Now().UnixMilli(),
				Match: &pb.MatchAssignment{
					SessionId:   evt.SessionID,
					PlayerIds:   evt.PlayerIDs,
					CreatedAtMs: evt.CreatedAt.UnixMilli(),
				},
			}); err != nil {
				return status.Errorf(codes.Internal, "failed to send ready-check: %v", err)
			}

			// Wait for client acceptance within the timeout.
			readyTimer := time.NewTimer(readyCheckTimeout)
			accepted := false

			select {
			case <-acceptReq:
				accepted = true
				readyTimer.Stop()
			case <-readyTimer.C:
				// Timeout — client did not accept.
				slog.Warn("gateway: ready-check timeout", "player_id", player.PlayerId)
			case <-clientDone:
				readyTimer.Stop()
				slog.Info("gateway: client disconnected during ready-check", "player_id", player.PlayerId)
			case <-ctx.Done():
				readyTimer.Stop()
			}

			// Signal acceptance (or not) to the coordinator.
			if accepted {
				select {
				case evt.ResponseCh <- true:
				default:
				}
			} else {
				select {
				case evt.ResponseCh <- false:
				default:
				}
			}

			if !accepted {
				// Notify client of cancellation.
				_ = stream.Send(&pb.GatewayResponse{
					Status:            pb.QueueStatus_QUEUE_STATUS_CANCELLED,
					Message:           "Ready check failed. Returning to queue or disconnecting.",
					ServerTimestampMs: time.Now().UnixMilli(),
				})
				return status.Error(codes.Aborted, "ready check not accepted")
			}

			// Wait for the coordinator's final verdict.
			result, resultOk := <-evt.ResultCh
			if !resultOk || !result {
				// Room was cancelled because someone else didn't accept.
				// The coordinator will re-enqueue this player.
				_ = stream.Send(&pb.GatewayResponse{
					Status:            pb.QueueStatus_QUEUE_STATUS_WAITING,
					Message:           "Another player failed ready check. You have been re-queued.",
					ServerTimestampMs: time.Now().UnixMilli(),
				})
				// IMPORTANT: Do NOT return here. The coordinator re-enqueued the
				// QueuedPlayer struct directly, so qp.MatchCh is still valid.
				// We just loop around and wait for the next match event.
				continue
			}

			// All 10 accepted → session confirmed.
			slog.Info("gateway: session confirmed",
				"player_id", player.PlayerId,
				"session_id", evt.SessionID,
			)

			if err := stream.Send(&pb.GatewayResponse{
				Status:            pb.QueueStatus_QUEUE_STATUS_IN_GAME,
				Message:           "All players accepted. Session starting!",
				ServerTimestampMs: time.Now().UnixMilli(),
				Match: &pb.MatchAssignment{
					SessionId:   evt.SessionID,
					PlayerIds:   evt.PlayerIDs,
					CreatedAtMs: evt.CreatedAt.UnixMilli(),
				},
			}); err != nil {
				return status.Errorf(codes.Internal, "failed to send IN_GAME: %v", err)
			}

			return nil

		// ── Periodic status push ────────────────────────────────
		case <-statusTicker.C:
			if err := stream.Send(&pb.GatewayResponse{
				Status:            pb.QueueStatus_QUEUE_STATUS_WAITING,
				Message:           "Still searching...",
				QueuePosition:     int32(g.pool.Size()),
				ServerTimestampMs: time.Now().UnixMilli(),
			}); err != nil {
				return status.Errorf(codes.Unavailable, "status push failed: %v", err)
			}

		// ── Client explicitly cancelled ─────────────────────────
		case <-cancelReq:
			g.pool.Dequeue(player.PlayerId)
			_ = stream.Send(&pb.GatewayResponse{
				Status:            pb.QueueStatus_QUEUE_STATUS_CANCELLED,
				Message:           "Queue cancelled by player.",
				ServerTimestampMs: time.Now().UnixMilli(),
			})
			return status.Error(codes.Canceled, "player cancelled matchmaking")

		// ── Client disconnected ─────────────────────────────────
		case <-clientDone:
			slog.Info("gateway: client disconnected, cleaning up", "player_id", player.PlayerId)
			return status.Error(codes.Aborted, "client disconnected")

		// ── Server context done ─────────────────────────────────
		case <-ctx.Done():
			slog.Info("gateway: context cancelled", "player_id", player.PlayerId)
			return status.Error(codes.DeadlineExceeded, "server context cancelled")
		}
	}
}
