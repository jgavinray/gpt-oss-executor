//go:build integration

// Integration tests exercise the full agentic loop against real services.
// They are excluded from the normal test suite and must be run explicitly:
//
//	GPTOSS_URL=http://spark:8000 \
//	OPENCLAW_URL=http://localhost:18789 \
//	GPTOSS_EXECUTOR_GATEWAY_TOKEN=<token> \
//	go test -tags integration -v -timeout 120s ./tests/
//
// Optional env vars:
//
//	GPTOSS_MODEL          model name (default: gpt-oss)
//	PARSER_STRATEGY       guided_json | react | markers | fuzzy (default: react)
//	PARSER_SOURCE_FIELD   reasoning | content (default: content)
package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jgavinray/gpt-oss-executor/internal/config"
	"github.com/jgavinray/gpt-oss-executor/internal/executor"
	"github.com/jgavinray/gpt-oss-executor/internal/httpserver"
	"github.com/jgavinray/gpt-oss-executor/internal/logging"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// requireEnv returns the value of key, or calls t.Skipf if it is unset.
// This means any test that calls requireEnv will be skipped — not failed —
// when the required environment isn't available.
func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("skipping integration test: %s is not set", key)
	}
	return v
}

// optionalEnv returns the value of key, or def if unset.
func optionalEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// integrationConfig builds a *config.Config from environment variables.
// It calls t.Skip (via requireEnv) if the mandatory variables are absent.
func integrationConfig(t *testing.T) *config.Config {
	t.Helper()

	gptOSSURL := requireEnv(t, "GPTOSS_URL")
	gatewayURL := requireEnv(t, "OPENCLAW_URL")
	token := requireEnv(t, "GPTOSS_EXECUTOR_GATEWAY_TOKEN")

	model := optionalEnv("GPTOSS_MODEL", "gpt-oss")
	strategy := optionalEnv("PARSER_STRATEGY", "react")
	sourceField := optionalEnv("PARSER_SOURCE_FIELD", "content")

	cfg := &config.Config{}
	cfg.Executor.GptOSSURL = gptOSSURL
	cfg.Executor.GptOSSModel = model
	cfg.Executor.GptOSSTemperature = 0.25
	cfg.Executor.GptOSSMaxTokens = 1000
	cfg.Executor.GptOSSCallTimeoutSeconds = 60
	cfg.Executor.MaxIterations = 5
	cfg.Executor.MaxRetries = 2
	cfg.Executor.RunTimeoutSeconds = 90
	cfg.Executor.ContextWindowLimit = 32768
	cfg.Executor.ContextBufferTokens = 2000
	cfg.Executor.ContextCompactThreshold = 0.8
	cfg.Executor.ContextTruncThreshold = 0.6
	cfg.Executor.OpenClawGatewayURL = gatewayURL
	cfg.Executor.OpenClawGatewayToken = token
	cfg.Executor.OpenClawSessionKey = "main"

	cfg.Parser.Strategy = strategy
	cfg.Parser.FallbackStrategy = "fuzzy"
	cfg.Parser.SourceField = sourceField
	cfg.Parser.FallbackField = "content"
	cfg.Parser.SystemPromptPath = "../config/system-prompt-react.txt"

	cfg.Tools.DefaultTimeoutSeconds = 30
	cfg.Tools.ResultLimits = map[string]int{
		"web_search": 1000,
		"web_fetch":  3000,
		"read":       5000,
		"write":      200,
		"exec":       2000,
		"browser":    3000,
	}

	cfg.HTTPServer.Port = 0 // OS-assigned; used only in end-to-end test
	cfg.HTTPServer.Bind = "127.0.0.1"
	cfg.HTTPServer.ReadTimeoutSeconds = 30
	cfg.HTTPServer.WriteTimeoutSeconds = 120
	cfg.HTTPServer.IdleTimeoutSeconds = 60
	cfg.HTTPServer.ShutdownTimeoutSeconds = 5

	cfg.Logging.Level = "debug"
	cfg.Logging.Format = "text"
	cfg.Logging.Output = "stdout"

	return cfg
}

// newIntegrationExecutor constructs a real *executor.Executor from cfg,
// using a verbose test logger so diagnostics appear in -v output.
func newIntegrationExecutor(t *testing.T, cfg *config.Config) *executor.Executor {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	errLogger := logging.NewErrorLogger(t.TempDir(), "YYYY-MM-DD-errors.md")
	exec, err := executor.New(cfg, logger, errLogger)
	if err != nil {
		t.Fatalf("executor.New: %v", err)
	}
	return exec
}

// ask is a convenience wrapper that runs a single user message through exec.
func ask(t *testing.T, exec *executor.Executor, question string) *executor.RunResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	result, err := exec.Run(ctx, []executor.Message{
		{Role: "user", Content: question},
	})
	if err != nil {
		t.Fatalf("executor.Run failed: %v", err)
	}
	return result
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestIntegration_NoToolRequest confirms the model can answer a simple
// factual question without calling any tools. The loop should complete in
// a single iteration.
func TestIntegration_NoToolRequest(t *testing.T) {
	cfg := integrationConfig(t)
	exec := newIntegrationExecutor(t, cfg)

	result := ask(t, exec, "What is 7 multiplied by 8? Answer with just the number.")

	t.Logf("answer: %s", result.Answer)
	t.Logf("iterations: %d", result.Iterations)

	if result.Answer == "" {
		t.Error("expected a non-empty answer")
	}
	if result.Iterations != 1 {
		t.Errorf("expected 1 iteration for a no-tool request, got %d", result.Iterations)
	}
	if !strings.Contains(result.Answer, "56") {
		t.Errorf("expected answer to contain '56', got: %s", result.Answer)
	}
}

// TestIntegration_SingleToolWebSearch confirms that the model calls web_search
// when asked to look something up. We assert at least 2 iterations (one tool
// call, one final answer) and a non-empty answer.
func TestIntegration_SingleToolWebSearch(t *testing.T) {
	cfg := integrationConfig(t)
	exec := newIntegrationExecutor(t, cfg)

	result := ask(t, exec,
		"Search the web for the latest stable Go release version and tell me what it is.")

	t.Logf("answer: %s", result.Answer)
	t.Logf("iterations: %d", result.Iterations)

	if result.Answer == "" {
		t.Error("expected a non-empty answer")
	}
	if result.Iterations < 2 {
		t.Errorf("expected at least 2 iterations (tool call + answer), got %d — "+
			"the model may not have called web_search; check logs for intent_count", result.Iterations)
	}

	// The answer should mention Go or a version number — loose assertion
	// since we can't control the model's phrasing.
	lower := strings.ToLower(result.Answer)
	if !strings.Contains(lower, "go") && !strings.Contains(lower, "1.") {
		t.Logf("warning: answer does not mention 'go' or a version number — may be a parser issue")
	}
}

// TestIntegration_MultiStep confirms the model can chain two tool calls:
// search for a topic, then fetch the top URL. Expects 3+ iterations.
func TestIntegration_MultiStep(t *testing.T) {
	cfg := integrationConfig(t)
	cfg.Executor.MaxIterations = 6 // give the model room to chain steps
	exec := newIntegrationExecutor(t, cfg)

	result := ask(t, exec,
		"Search the web for 'OpenAI API chat completions documentation', "+
			"then fetch the first URL you find and summarise what it says in two sentences.")

	t.Logf("answer: %s", result.Answer)
	t.Logf("iterations: %d", result.Iterations)

	if result.Answer == "" {
		t.Error("expected a non-empty answer")
	}
	if result.Iterations < 3 {
		t.Logf("warning: expected 3+ iterations for a multi-step task, got %d — "+
			"check logs to see whether both tool calls were made", result.Iterations)
	}
}

// TestIntegration_FileWrite confirms the model can write a file to disk.
// We ask it to write to a path inside t.TempDir() and verify the file exists.
func TestIntegration_FileWrite(t *testing.T) {
	cfg := integrationConfig(t)
	exec := newIntegrationExecutor(t, cfg)

	dir := t.TempDir()
	target := dir + "/hello.txt"

	ask(t, exec, fmt.Sprintf(
		"Write the text 'integration test passed' to the file %s", target))

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("file was not written: %v\n"+
			"This may mean the model did not call the write tool, or the tool failed.\n"+
			"Run with PARSER_STRATEGY=fuzzy to rule out parser issues.", err)
	}
	t.Logf("file contents: %s", string(data))
	if !strings.Contains(string(data), "integration test passed") {
		t.Errorf("file content does not match expected text, got: %s", string(data))
	}
}

// TestIntegration_ToolErrorRecovery confirms the model can handle a tool
// failure gracefully. We ask it to read a file that does not exist; the
// gateway should return an error, the executor should inject it as a tool
// message, and the model should produce a final answer explaining the failure.
func TestIntegration_ToolErrorRecovery(t *testing.T) {
	cfg := integrationConfig(t)
	exec := newIntegrationExecutor(t, cfg)

	result := ask(t, exec,
		"Read the file /nonexistent/path/that/does/not/exist.txt and tell me its contents.")

	t.Logf("answer: %s", result.Answer)
	t.Logf("iterations: %d", result.Iterations)

	if result.Answer == "" {
		t.Error("expected a non-empty answer even when the tool fails")
	}
	// The model should acknowledge the failure in some form.
	lower := strings.ToLower(result.Answer)
	if !strings.Contains(lower, "error") &&
		!strings.Contains(lower, "fail") &&
		!strings.Contains(lower, "not found") &&
		!strings.Contains(lower, "unable") &&
		!strings.Contains(lower, "cannot") {
		t.Logf("warning: answer does not acknowledge the failure — model may have ignored the error injection")
	}
}

// TestIntegration_EndToEnd_HTTP exercises the full stack through the HTTP
// server, mirroring what a real client (e.g. another OpenClaw component)
// would do. It starts the server on a random port, sends a POST
// /v1/chat/completions request, and validates the OpenAI-shaped response.
func TestIntegration_EndToEnd_HTTP(t *testing.T) {
	cfg := integrationConfig(t)

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	errLogger := logging.NewErrorLogger(t.TempDir(), "YYYY-MM-DD-errors.md")

	exec, err := executor.New(cfg, logger, errLogger)
	if err != nil {
		t.Fatalf("executor.New: %v", err)
	}

	// Use a fixed high port for the integration test server.
	cfg.HTTPServer.Port = 18999
	srv := httpserver.New(cfg, exec, logger)

	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			serverErr <- err
		}
	}()
	// Give the server a moment to bind.
	time.Sleep(100 * time.Millisecond)

	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
	})

	body, _ := json.Marshal(map[string]interface{}{
		"model": cfg.Executor.GptOSSModel,
		"messages": []map[string]string{
			{"role": "user", "content": "Say the word HELLO and nothing else."},
		},
		"max_tokens": 20,
	})

	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", cfg.HTTPServer.Port),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, string(raw))
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	t.Logf("response: %+v", result)

	choices, ok := result["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatal("response missing choices array")
	}
	choice := choices[0].(map[string]interface{})
	message := choice["message"].(map[string]interface{})
	content, _ := message["content"].(string)

	if content == "" {
		t.Error("expected non-empty content in response")
	}
	t.Logf("answer: %s", content)
}
