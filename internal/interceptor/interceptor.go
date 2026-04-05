// Package interceptor provides gRPC server interceptors for:
//   1. Panic recovery — catches panics in any handler, logs the stack, and
//      returns codes.Internal instead of crashing the server process.
//   2. Structured logging — logs every RPC call with method, duration, and
//      status code using slog.
//   3. Stream-aware variants for bi-directional streaming RPCs.
//
// These interceptors are installed at server construction time via
// grpc.ChainUnaryInterceptor / grpc.ChainStreamInterceptor.
package interceptor

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ─────────────────────────────────────────────────────────────
// Unary Interceptors
// ─────────────────────────────────────────────────────────────

// UnaryPanicRecovery catches panics in unary handlers and converts them to
// gRPC Internal errors. The full stack trace is logged for post-mortem
// debugging but never leaked to the client.
func UnaryPanicRecovery(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (resp interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("PANIC recovered in unary handler",
				"method", info.FullMethod,
				"panic", r,
				"stack", string(debug.Stack()),
			)
			err = status.Errorf(codes.Internal, "internal server error")
		}
	}()
	return handler(ctx, req)
}

// UnaryLogger logs the start and completion of every unary call with
// timing information and the resulting gRPC status code.
func UnaryLogger(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	start := time.Now()

	resp, err := handler(ctx, req)

	duration := time.Since(start)
	code := status.Code(err)

	// Use appropriate log level based on status code.
	level := slog.LevelInfo
	if code != codes.OK {
		level = slog.LevelWarn
	}

	slog.Log(ctx, level, "unary RPC completed",
		"method", info.FullMethod,
		"code", code.String(),
		"duration_ms", duration.Milliseconds(),
	)

	return resp, err
}

// ─────────────────────────────────────────────────────────────
// Stream Interceptors
// ─────────────────────────────────────────────────────────────

// StreamPanicRecovery wraps the stream handler to catch panics.
// Because the stream handler runs for the lifetime of the connection,
// a panic here would be catastrophic — this interceptor prevents that.
func StreamPanicRecovery(
	srv interface{},
	ss grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("PANIC recovered in stream handler",
				"method", info.FullMethod,
				"panic", r,
				"stack", string(debug.Stack()),
			)
			err = status.Errorf(codes.Internal, "internal server error")
		}
	}()
	return handler(srv, ss)
}

// StreamLogger logs the lifecycle of streaming RPCs.
func StreamLogger(
	srv interface{},
	ss grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) error {
	start := time.Now()

	slog.Info("stream RPC started",
		"method", info.FullMethod,
		"client_stream", info.IsClientStream,
		"server_stream", info.IsServerStream,
	)

	err := handler(srv, ss)

	duration := time.Since(start)
	code := status.Code(err)

	level := slog.LevelInfo
	if code != codes.OK && code != codes.Canceled {
		level = slog.LevelWarn
	}

	slog.Log(ss.Context(), level, "stream RPC ended",
		"method", info.FullMethod,
		"code", code.String(),
		"duration_ms", duration.Milliseconds(),
	)

	return err
}

// ─────────────────────────────────────────────────────────────
// Resource Exhaustion Guard
// ─────────────────────────────────────────────────────────────

// MaxConcurrentStreams returns a stream interceptor that limits the number
// of concurrent streaming RPCs. If the limit is reached, new streams get
// RESOURCE_EXHAUSTED.
func MaxConcurrentStreams(limit int) grpc.StreamServerInterceptor {
	// Buffered channel used as a semaphore. No mutex needed.
	sem := make(chan struct{}, limit)

	return func(
		srv interface{},
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		select {
		case sem <- struct{}{}:
			// Acquired a slot.
			defer func() { <-sem }()
			return handler(srv, ss)
		default:
			slog.Warn("stream rejected: resource exhausted",
				"method", info.FullMethod,
				"limit", limit,
			)
			return status.Errorf(codes.ResourceExhausted,
				"server at capacity (%d concurrent streams)", limit)
		}
	}
}
