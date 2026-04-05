# Nexus-Match: Real-Time Matchmaking & Session Engine

A production-grade, fully in-memory gRPC matchmaking system built in Go. Implements three microservices with bi-directional streaming, thread-safe state management, and a 50-client concurrent stress test simulator.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    gRPC Server (:50051)                      │
│  ┌──────────────────┐ ┌──────────────┐ ┌─────────────────┐  │
│  │ PlayerGateway    │ │ Matchmaker   │ │ SessionTracker  │  │
│  │ (Bi-di Stream)   │ │ (Worker)     │ │ (Unary RPC)     │  │
│  └────────┬─────────┘ └──────┬───────┘ └────────┬────────┘  │
│           │                  │                   │           │
│  ┌────────▼──────────────────▼───────────────────▼────────┐  │
│  │              In-Memory State (sync.RWMutex)            │  │
│  │         Pool (MMR Brackets)  │  SessionStore           │  │
│  └────────────────────────────────────────────────────────┘  │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  Interceptors: PanicRecovery │ Logger │ RateLimiter    │  │
│  └────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

## Features

- **Bi-directional streaming** — persistent client connections with heartbeat & queue status broadcast
- **MMR-based matchmaking** — bracket bucketing with sliding-window extraction for tight skill matches
- **Thread-safe** — single-mutex design, zero race conditions verified with `go run -race`
- **gRPC interceptors** — panic recovery, structured logging, channel-based concurrency limiter
- **50-client stress test** — concurrent goroutine simulator exercising the full lifecycle

## Quick Start

```bash
# Start the server
go run ./cmd/server

# Run the 50-player stress test (separate terminal)
go run ./client

# With race detector
go run -race ./cmd/server
go run -race ./client
```

## Project Structure

```
├── proto/nexus.proto           # Protobuf definitions (3 services)
├── pkg/pb/                     # Generated Go + gRPC code
├── internal/
│   ├── state/pool.go           # Thread-safe matchmaking pool
│   ├── state/session.go        # Session result store
│   ├── services/gateway.go     # Bi-directional streaming service
│   ├── services/matchmaker.go  # Background worker + admin RPCs
│   ├── services/tracker.go     # Unary result submission
│   └── interceptor/            # Panic recovery, logging, rate limit
├── cmd/server/main.go          # Server entrypoint
├── client/main.go              # Multi-client simulator
└── Makefile                    # Build, proto gen, race test
```

## gRPC Services

| Service | Type | Purpose |
|---------|------|---------|
| `PlayerGatewayService` | Bi-di Stream | Client entry, queue status, match delivery |
| `MatchmakerService` | Unary | Admin stats, force-match trigger |
| `SessionTrackerService` | Unary | Submit match results, query sessions |

## Tech Stack

- **Go 1.22+**
- **gRPC** with protobuf v3
- **In-memory state** with `sync.RWMutex`
- **Structured logging** via `log/slog`
