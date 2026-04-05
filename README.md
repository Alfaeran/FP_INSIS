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

## Architecture Limitations & Future Scalability

While the current architecture leverages an in-memory `sync.RWMutex` which is extremely fast and suitable for single-node deployments, it has limitations when deployed as a Commercial App that requires horizontal scaling across multiple server pods.

To migrate this architecture for distributed scalability, the following technologies should be introduced:

- **Redis Sorted Sets (ZSET)**: To centralize the player pool across all pods. The MMR score would act as the ZSET `score`, enabling O(log(N)) nearest-neighbor searches directly within Redis. This eliminates the constraint of having a single process manage the entire memory pool.
- **Redis Pub/Sub or Kafka**: Since clients might be spread across multiple *Gateway Pods* while the *Matchmaker Worker* runs on a dedicated, separate pod, a distributed event bus is required. When a match is found, the worker would publish a `Ready Check` or `Match Found` event. The gateway holding the specific client's stream would consume this event and push it down the TCP/gRPC connection, decoupling connection management from matchmaking logic.
