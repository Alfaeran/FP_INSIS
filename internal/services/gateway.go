// Package services — gateway.go implements the PlayerGatewayService.
//
// The bi-directional stream lifecycle:
//   1. Client opens stream and sends JoinQueueAction (first message).
//   2. Server enqueues the player and starts pushing QueueStatus updates.
//   3. Client sends periodic HeartbeatAction to keep the connection alive.
//   4. When the match worker assigns a match, it writes to QueuedPlayer.MatchCh.
//      The gateway handler reads from MatchCh and pushes MATCHED + MatchAssignment
//      to the client, then closes the server side of the stream.
//   5. If the client disconnects (EOF / context cancelled), the handler removes
//      the player from the pool and returns.
//
// Context-based cancellation:
//   stream.Context().Done() is selected alongside MatchCh and a heartbeat ticker.
//   This ensures we never leak goroutines even if the client's TCP connection
//   drops silently.
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
// The generated interface uses the generic stream type:
//   grpc.BidiStreamingServer[pb.GatewayRequest, pb.GatewayResponse]
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
		// If the player is still in the pool at this point, remove them.
		// This handles client disconnect, server error, etc.
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

	// ── Step 3: Main event loop ─────────────────────────────────────────
	// We run a goroutine to drain client messages (heartbeats, cancel)
	// and signal via channels.

	// clientDone is closed when the client stream ends (EOF or error).
	clientDone := make(chan struct{})
	// cancelReq is sent when the client explicitly requests cancellation.
	cancelReq := make(chan string, 1)

	// Client reader goroutine.
	// This goroutine terminates when:
	//   - stream.Recv() returns io.EOF (client closed)
	//   - stream.Recv() returns a non-EOF error (transport failure)
	//   - ctx is cancelled
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
				// Heartbeat received; we could track last-seen for timeout,
				// but for now we just acknowledge silently.
				slog.Debug("gateway: heartbeat", "player_id", player.PlayerId)

			case msg.GetCancelQueue() != nil:
				slog.Info("gateway: cancel requested", "player_id", player.PlayerId)
				cancelReq <- player.PlayerId
				return
			}
		}
	}()

	// Status broadcast ticker: every 2 seconds, push a WAITING update
	// so the client knows the connection is alive.
	statusTicker := time.NewTicker(2 * time.Second)
	defer statusTicker.Stop()

	for {
		select {

		// ── Match found ─────────────────────────────────────────
		case evt, ok := <-qp.MatchCh:
			if !ok {
				// Channel was closed → player was dequeued externally (cancel/disconnect).
				// Send CANCELLED and terminate.
				_ = stream.Send(&pb.GatewayResponse{
					Status:            pb.QueueStatus_QUEUE_STATUS_CANCELLED,
					Message:           "Queue cancelled.",
					ServerTimestampMs: time.Now().UnixMilli(),
				})
				return status.Error(codes.Aborted, "player removed from queue")
			}

			slog.Info("gateway: match found, notifying client",
				"player_id", player.PlayerId,
				"session_id", evt.SessionID,
			)

			if err := stream.Send(&pb.GatewayResponse{
				Status:            pb.QueueStatus_QUEUE_STATUS_MATCHED,
				Message:           "Match found! Entering session.",
				ServerTimestampMs: time.Now().UnixMilli(),
				Match: &pb.MatchAssignment{
					SessionId:   evt.SessionID,
					PlayerIds:   evt.PlayerIDs,
					CreatedAtMs: evt.CreatedAt.UnixMilli(),
				},
			}); err != nil {
				return status.Errorf(codes.Internal, "failed to send match: %v", err)
			}

			// Stream complete — client should now connect to the game session.
			return nil

		// ── Periodic status push ────────────────────────────────
		case <-statusTicker.C:
			if err := stream.Send(&pb.GatewayResponse{
				Status:            pb.QueueStatus_QUEUE_STATUS_WAITING,
				Message:           "Still searching...",
				QueuePosition:     int32(g.pool.Size()),
				ServerTimestampMs: time.Now().UnixMilli(),
			}); err != nil {
				// Client probably disconnected; the clientDone channel
				// will fire shortly.
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

		// ── Client disconnected (EOF / transport error) ─────────
		case <-clientDone:
			slog.Info("gateway: client disconnected, cleaning up", "player_id", player.PlayerId)
			return status.Error(codes.Aborted, "client disconnected")

		// ── Server-side context done (e.g. deadline / shutdown) ─
		case <-ctx.Done():
			slog.Info("gateway: context cancelled", "player_id", player.PlayerId)
			return status.Error(codes.DeadlineExceeded, "server context cancelled")
		}
	}
}
