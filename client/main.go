// client/main.go — Nexus-Match multi-client stress test simulator.
//
// Launches 50 concurrent goroutines, each simulating a player that:
//   1. Connects to the Gateway via bi-directional stream.
//   2. Sends JoinQueueAction with a randomized MMR.
//   3. Sends periodic heartbeats while waiting.
//   4. Receives the MATCHED assignment.
//   5. Submits a result to the SessionTracker via Unary RPC.
//
// The simulator exercises:
//   - Concurrent stream establishment (50 simultaneous dials)
//   - In-memory pool contention (50 enqueue + heartbeat + dequeue operations)
//   - Match notification delivery (MatchCh channel writes under load)
//   - Unary result submission (50 sequential submits per match)
//
// Run with `go run -race ./client` to detect data races.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	pb "nexus-match/pkg/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	serverAddr    = "localhost:50051"
	numPlayers    = 50
	heartbeatRate = 1 * time.Second
	matchTimeout  = 30 * time.Second
)

// stats tracks aggregate outcomes across all goroutines.
var (
	matched  atomic.Int64
	failed   atomic.Int64
	results  atomic.Int64
)

func main() {
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	slog.SetDefault(slog.New(handler))

	slog.Info("nexus-match client simulator starting",
		"players", numPlayers,
		"server", serverAddr,
	)

	start := time.Now()

	var wg sync.WaitGroup
	wg.Add(numPlayers)

	for i := 0; i < numPlayers; i++ {
		go func(idx int) {
			defer wg.Done()
			simulatePlayer(idx)
		}(i)

		// Stagger connections slightly to avoid thundering herd on the listener.
		time.Sleep(20 * time.Millisecond)
	}

	wg.Wait()

	elapsed := time.Since(start)
	slog.Info("simulation complete",
		"elapsed", elapsed,
		"matched", matched.Load(),
		"failed", failed.Load(),
		"results_submitted", results.Load(),
	)
}

func simulatePlayer(idx int) {
	playerID := fmt.Sprintf("player-%04d", idx)
	displayName := fmt.Sprintf("Bot_%04d", idx)
	// Randomize MMR in [1000, 5000] to create bracket diversity.
	mmr := int32(1000 + rand.Intn(4001))

	logger := slog.With("player_id", playerID, "mmr", mmr)

	// ── Dial the server ─────────────────────────────────────────────────
	conn, err := grpc.NewClient(
		serverAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		logger.Error("dial failed", "err", err)
		failed.Add(1)
		return
	}
	defer conn.Close()

	gatewayClient := pb.NewPlayerGatewayServiceClient(conn)
	trackerClient := pb.NewSessionTrackerServiceClient(conn)

	// ── Open bi-directional stream ──────────────────────────────────────
	ctx, cancel := context.WithTimeout(context.Background(), matchTimeout)
	defer cancel()

	stream, err := gatewayClient.EnterMatchmaking(ctx)
	if err != nil {
		logger.Error("stream open failed", "err", err)
		failed.Add(1)
		return
	}

	// ── Send JoinQueue ──────────────────────────────────────────────────
	if err := stream.Send(&pb.GatewayRequest{
		Action: &pb.GatewayRequest_JoinQueue{
			JoinQueue: &pb.JoinQueueAction{
				Player: &pb.PlayerInfo{
					PlayerId:    playerID,
					DisplayName: displayName,
					Mmr:         mmr,
				},
			},
		},
	}); err != nil {
		logger.Error("join_queue send failed", "err", err)
		failed.Add(1)
		return
	}

	logger.Info("joined queue")

	// ── Heartbeat sender goroutine ──────────────────────────────────────
	// This goroutine exits when ctx is cancelled (match found / timeout)
	// or when the stream is closed.
	go func() {
		ticker := time.NewTicker(heartbeatRate)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := stream.Send(&pb.GatewayRequest{
					Action: &pb.GatewayRequest_Heartbeat{
						Heartbeat: &pb.HeartbeatAction{
							ClientTimestampMs: time.Now().UnixMilli(),
						},
					},
				}); err != nil {
					// Stream probably closed; exit silently.
					return
				}
			}
		}
	}()

	// ── Read server responses until MATCHED or error ────────────────────
	var sessionID string
	for {
		resp, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				logger.Info("stream closed by server")
			} else {
				st, _ := status.FromError(err)
				logger.Warn("recv error", "code", st.Code(), "msg", st.Message())
			}
			break
		}

		switch resp.Status {
		case pb.QueueStatus_QUEUE_STATUS_WAITING:
			logger.Debug("status update: waiting",
				"queue_position", resp.QueuePosition,
			)

		case pb.QueueStatus_QUEUE_STATUS_MATCHED:
			sessionID = resp.Match.SessionId
			logger.Info("MATCHED!",
				"session_id", sessionID,
				"players", resp.Match.PlayerIds,
			)
			matched.Add(1)

		case pb.QueueStatus_QUEUE_STATUS_CANCELLED:
			logger.Info("queue cancelled", "message", resp.Message)
			failed.Add(1)
			return

		default:
			logger.Info("received status", "status", resp.Status, "message", resp.Message)
		}

		if sessionID != "" {
			break
		}
	}

	// Cancel the context to stop the heartbeat goroutine.
	cancel()

	// ── Submit result to SessionTracker ──────────────────────────────────
	if sessionID == "" {
		logger.Warn("no session ID received, skipping result submission")
		failed.Add(1)
		return
	}

	// Simulate a short "game" duration.
	gameDuration := time.Duration(1+rand.Intn(3)) * time.Second
	time.Sleep(gameDuration)

	// Randomly pick a result.
	possibleResults := []pb.MatchResult{
		pb.MatchResult_MATCH_RESULT_WIN,
		pb.MatchResult_MATCH_RESULT_LOSS,
		pb.MatchResult_MATCH_RESULT_DRAW,
	}
	chosenResult := possibleResults[rand.Intn(len(possibleResults))]
	score := int32(rand.Intn(100))

	submitCtx, submitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer submitCancel()

	submitResp, err := trackerClient.SubmitResult(submitCtx, &pb.SubmitResultRequest{
		SessionId: sessionID,
		PlayerId:  playerID,
		Result:    chosenResult,
		Score:     score,
		DurationS: int32(gameDuration.Seconds()),
	})
	if err != nil {
		st, _ := status.FromError(err)
		logger.Error("submit result failed",
			"code", st.Code(),
			"msg", st.Message(),
		)
		failed.Add(1)
		return
	}

	results.Add(1)
	logger.Info("result submitted",
		"accepted", submitResp.Accepted,
		"mmr_delta", submitResp.NewMmr,
	)
}
