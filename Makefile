.PHONY: build run test test-verbose test-integration lint clean tidy

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

# Integration tests require real services. Set env vars before running:
#   GPTOSS_URL, OPENCLAW_URL, GPTOSS_EXECUTOR_GATEWAY_TOKEN
test-integration:
	go test -tags integration -race -v -timeout 120s ./tests/

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
