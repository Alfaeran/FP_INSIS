// cmd/server/main.go — Nexus-Match server entrypoint.
//
// Wires together:
//   - Shared in-memory state (Pool + SessionStore)
//   - Three gRPC services
//   - Interceptors (panic recovery, logging, concurrency limit)
//   - Graceful shutdown on SIGINT/SIGTERM
//
// Single-process architecture: all three services run on the same gRPC
// listener. In production, they could be split across processes/pods, but
// the shared Pool pointer would then need to be replaced with an IPC mechanism.
package main

import (
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	pb "nexus-match/pkg/pb"
	"nexus-match/internal/interceptor"
	"nexus-match/internal/services"
	"nexus-match/internal/state"
)

const (
	listenAddr    = ":50051"
	matchTickRate = 1 * time.Second
	maxStreams    = 500 // concurrent bi-di streams before RESOURCE_EXHAUSTED
)

func main() {
	// ── Structured logging setup ────────────────────────────────────────
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	slog.SetDefault(slog.New(handler))

	slog.Info("nexus-match server starting",
		"addr", listenAddr,
		"match_tick", matchTickRate,
		"max_streams", maxStreams,
	)

	// ── Shared state ────────────────────────────────────────────────────
	pool := state.NewPool()
	sessionStore := state.NewSessionStore()

	// ── Service instances ───────────────────────────────────────────────
	gatewaySrv := services.NewGatewayServer(pool)
	matchmakerSrv := services.NewMatchmakerServer(pool, sessionStore, matchTickRate)
	trackerSrv := services.NewTrackerServer(sessionStore)

	// ── gRPC server with interceptors ───────────────────────────────────
	grpcServer := grpc.NewServer(
		// Unary interceptor chain: logging → panic recovery.
		grpc.ChainUnaryInterceptor(
			interceptor.UnaryLogger,
			interceptor.UnaryPanicRecovery,
		),
		// Stream interceptor chain: logging → panic recovery → concurrency limit.
		grpc.ChainStreamInterceptor(
			interceptor.StreamLogger,
			interceptor.StreamPanicRecovery,
			interceptor.MaxConcurrentStreams(maxStreams),
		),
		// Keepalive policy to detect dead clients.
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
			Time:              30 * time.Second,
			Timeout:           10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)

	// Register all services on the same server.
	pb.RegisterPlayerGatewayServiceServer(grpcServer, gatewaySrv)
	pb.RegisterMatchmakerServiceServer(grpcServer, matchmakerSrv)
	pb.RegisterSessionTrackerServiceServer(grpcServer, trackerSrv)

	// ── Start the matchmaker background worker ──────────────────────────
	matchmakerSrv.StartWorker()

	// ── TCP listener ────────────────────────────────────────────────────
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		slog.Error("failed to listen", "addr", listenAddr, "err", err)
		os.Exit(1)
	}

	// ── Graceful shutdown ───────────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		slog.Info("received shutdown signal", "signal", sig)
		matchmakerSrv.StopWorker()
		grpcServer.GracefulStop()
	}()

	// ── Serve ───────────────────────────────────────────────────────────
	slog.Info("gRPC server listening", "addr", listenAddr)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("server exited with error", "err", err)
		os.Exit(1)
	}

	slog.Info("server shut down cleanly")
}
