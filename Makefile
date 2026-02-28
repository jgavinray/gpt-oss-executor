.PHONY: build run test lint clean tidy

BIN := bin/gpt-oss-executor
CONFIG := config/executor.yaml

build:
	@mkdir -p bin
	go build -o $(BIN) ./cmd/...

run: build
	@$(BIN) --config $(CONFIG)

test:
	go test -race ./...

test-verbose:
	go test -race -v ./...

lint:
	go vet ./...

tidy:
	go mod tidy

clean:
	@rm -rf bin/

# Build with version info
release:
	@mkdir -p bin
	go build -ldflags="-s -w" -o $(BIN) ./cmd/...

# Smoke test against local executor
smoke:
	curl -s -X POST http://localhost:8001/v1/chat/completions \
		-H "Content-Type: application/json" \
		-d '{"model":"gpt-oss","messages":[{"role":"user","content":"Say hello"}],"max_tokens":50}' | jq .

health:
	curl -s http://localhost:8001/health | jq .
