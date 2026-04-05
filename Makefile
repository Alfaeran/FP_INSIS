.PHONY: proto build run-server run-client test-race clean

# ─────────────────────────────────────────────────────────────
# Proto compilation
# Requires: protoc, protoc-gen-go, protoc-gen-go-grpc
# Install:
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
# ─────────────────────────────────────────────────────────────
proto:
	@echo ">>> Compiling protobuf..."
	protoc --go_out=. --go_opt=paths=source_relative \
	       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	       proto/nexus.proto
	@echo ">>> Moving generated files to pkg/pb..."
	@mkdir -p pkg/pb
	@if exist proto\*.go move /Y proto\*.go pkg\pb\ 2>nul || mv -f proto/*.go pkg/pb/ 2>/dev/null || true
	@echo ">>> Done."

# ─────────────────────────────────────────────────────────────
# Build
# ─────────────────────────────────────────────────────────────
build:
	go build -o bin/server.exe ./cmd/server
	go build -o bin/client.exe ./client

# ─────────────────────────────────────────────────────────────
# Run
# ─────────────────────────────────────────────────────────────
run-server:
	go run ./cmd/server

run-client:
	go run ./client

# ─────────────────────────────────────────────────────────────
# Race-detector stress test — the principal engineer's best friend
# ─────────────────────────────────────────────────────────────
test-race:
	go run -race ./cmd/server &
	sleep 2
	go run -race ./client
	@echo ">>> If no DATA RACE warnings appeared, the code is clean."

clean:
	rm -rf bin/
