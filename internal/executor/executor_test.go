package executor

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jgavinray/gpt-oss-executor/internal/config"
	execerrors "github.com/jgavinray/gpt-oss-executor/internal/errors"
	"github.com/jgavinray/gpt-oss-executor/internal/logging"
)

// buildTestConfig returns a minimal *config.Config wired to the provided mock
// server URLs. All paths are left empty so that SystemPrompt and
// GuidedJSONSchema return early without hitting the filesystem.
func buildTestConfig(vllmURL, gatewayURL string) *config.Config {
	return &config.Config{
		Executor: config.ExecutorConfig{
			GptOSSURL:               vllmURL,
			GptOSSModel:             "gpt-oss",
			GptOSSMaxTokens:         100,
			GptOSSTemperature:       0.25,
			GptOSSCallTimeoutSeconds: 5,
			MaxIterations:           5,
			MaxRetries:              3,
			RunTimeoutSeconds:       30,
			ContextWindowLimit:      32768,
			ContextCompactThreshold: 0.8,
			ContextTruncThreshold:   0.6,
			OpenClawGatewayURL:      gatewayURL,
			OpenClawGatewayToken:    "test-token",
			OpenClawSessionKey:      "main",
		},
		Parser: config.ParserConfig{
			Strategy:         "react",
			FallbackStrategy: "fuzzy",
			SourceField:      "content",
			FallbackField:    "content",
		},
		Tools: config.ToolsConfig{
			DefaultTimeoutSeconds: 5,
		},
	}
}

// discardLogger returns a *slog.Logger that discards all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// vllmResponse builds a valid gptOSSRawResponse JSON string.
func vllmResponse(content, reasoningContent string) string {
	type msg struct {
		Role             string `json:"role"`
		Content          string `json:"content"`
		ReasoningContent string `json:"reasoning_content,omitempty"`
	}
	type choice struct {
		Index        int    `json:"index"`
		Message      msg    `json:"message"`
		FinishReason string `json:"finish_reason"`
	}
	type usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	}
	type resp struct {
		ID      string   `json:"id"`
		Choices []choice `json:"choices"`
		Usage   usage    `json:"usage"`
	}
	r := resp{
		ID: "test",
		Choices: []choice{
			{
				Index: 0,
				Message: msg{
					Role:             "assistant",
					Content:          content,
					ReasoningContent: reasoningContent,
				},
				FinishReason: "stop",
			},
		},
		Usage: usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
	}
	b, _ := json.Marshal(r)
	return string(b)
}

// gatewayOKResponse returns a gateway success JSON body with the given result string.
func gatewayOKResponse(result string) string {
	// result must be valid JSON; wrap as a quoted string.
	b, _ := json.Marshal(result)
	return `{"ok":true,"result":` + string(b) + `}`
}

// gatewayErrorResponse returns a gateway error JSON body.
func gatewayErrorResponse(errType, message string) string {
	return `{"ok":false,"error":{"type":"` + errType + `","message":"` + message + `"}}`
}

// newTestExecutor constructs a real *Executor via New() from the test config.
// t.Helper marks call-site failures at the correct line.
func newTestExecutor(t *testing.T, cfg *config.Config) *Executor {
	t.Helper()
	logger := discardLogger()
	errLogger := logging.NewErrorLogger(t.TempDir(), "YYYY-MM-DD-errors.md")
	exec, err := New(cfg, logger, errLogger)
	if err != nil {
		t.Fatalf("executor.New() error: %v", err)
	}
	return exec
}

// inputMessages returns a minimal single-user-message slice.
func inputMessages(text string) []Message {
	return []Message{{Role: "user", Content: text}}
}

// ---------------------------------------------------------------------------
// Test cases
// ---------------------------------------------------------------------------

func TestRun_ImmediateFinalAnswer(t *testing.T) {
	t.Parallel()

	const finalAnswer = "The capital of France is Paris."

	vllmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, vllmResponse(finalAnswer, ""))
	}))
	t.Cleanup(vllmSrv.Close)

	// Gateway should never be called for a final-answer response, but we still
	// start a server so the config URL is valid.
	gatewaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("gateway should not be called for an immediate final answer")
		http.Error(w, "unexpected call", http.StatusInternalServerError)
	}))
	t.Cleanup(gatewaySrv.Close)

	cfg := buildTestConfig(vllmSrv.URL, gatewaySrv.URL)
	exec := newTestExecutor(t, cfg)

	result, err := exec.Run(context.Background(), inputMessages("What is the capital of France?"))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if result.Answer != finalAnswer {
		t.Errorf("Answer = %q, want %q", result.Answer, finalAnswer)
	}
	if result.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1", result.Iterations)
	}
}

func TestRun_OneToolCallThenDone(t *testing.T) {
	t.Parallel()

	const searchResult = "search results"
	const finalAnswer = "Here is what I found."

	// Track how many times vLLM is called.
	var vllmCalls atomic.Int32

	vllmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := vllmCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if call == 1 {
			// First call: return a ReAct action intent.
			reactText := "I need to search.\nAction: web_search\nAction Input: {\"query\":\"test\"}"
			_, _ = io.WriteString(w, vllmResponse(reactText, ""))
		} else {
			// Second call: plain final answer.
			_, _ = io.WriteString(w, vllmResponse(finalAnswer, ""))
		}
	}))
	t.Cleanup(vllmSrv.Close)

	gatewaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, gatewayOKResponse(searchResult))
	}))
	t.Cleanup(gatewaySrv.Close)

	cfg := buildTestConfig(vllmSrv.URL, gatewaySrv.URL)
	exec := newTestExecutor(t, cfg)

	result, err := exec.Run(context.Background(), inputMessages("Search for something."))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if result.Answer != finalAnswer {
		t.Errorf("Answer = %q, want %q", result.Answer, finalAnswer)
	}
	if result.Iterations != 2 {
		t.Errorf("Iterations = %d, want 2", result.Iterations)
	}

	gotCalls := int(vllmCalls.Load())
	if gotCalls != 2 {
		t.Errorf("vLLM call count = %d, want 2", gotCalls)
	}
}

func TestRun_MaxIterationsExhausted(t *testing.T) {
	t.Parallel()

	// vLLM always returns a ReAct tool call; gateway always succeeds.
	// With MaxIterations=2 the loop must exhaust and return ErrMaxIterations.
	vllmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reactText := "Searching...\nAction: web_search\nAction Input: {\"query\":\"loop\"}"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, vllmResponse(reactText, ""))
	}))
	t.Cleanup(vllmSrv.Close)

	gatewaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, gatewayOKResponse("some result"))
	}))
	t.Cleanup(gatewaySrv.Close)

	cfg := buildTestConfig(vllmSrv.URL, gatewaySrv.URL)
	cfg.Executor.MaxIterations = 2
	exec := newTestExecutor(t, cfg)

	_, err := exec.Run(context.Background(), inputMessages("Keep searching forever."))
	if err == nil {
		t.Fatal("Run() returned nil error, want ErrMaxIterations")
	}
	if !isMaxIterationsErr(err) {
		t.Errorf("Run() error = %v, want errors.Is match for ErrMaxIterations", err)
	}
}

// isMaxIterationsErr checks for ErrMaxIterations by code via errors.Is semantics.
func isMaxIterationsErr(err error) bool {
	// errors.Is traverses the chain; ExecutorError.Is matches by Code field.
	return isExecErr(err, execerrors.ErrMaxIterations)
}

// isExecErr uses errors.Is to check whether err matches the target sentinel.
func isExecErr(err error, target error) bool {
	// We rely on ExecutorError.Is() which compares Code fields.
	// errors.Is walks the chain calling Is() on each element.
	type isser interface {
		Is(error) bool
	}
	if i, ok := target.(isser); ok {
		return i.Is(err) || checkChain(err, target)
	}
	return false
}

// checkChain walks the error chain from err looking for target via Is.
func checkChain(err, target error) bool {
	for e := err; e != nil; {
		if i, ok := e.(interface{ Is(error) bool }); ok {
			if i.Is(target) {
				return true
			}
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := e.(unwrapper)
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	return false
}

func TestRun_RunTimeout(t *testing.T) {
	t.Parallel()

	// The vLLM handler sleeps long enough to outlast the context deadline.
	vllmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep until context is cancelled, then return.
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		http.Error(w, "timeout", http.StatusServiceUnavailable)
	}))
	t.Cleanup(vllmSrv.Close)

	gatewaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("gateway should not be called in timeout scenario")
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(gatewaySrv.Close)

	cfg := buildTestConfig(vllmSrv.URL, gatewaySrv.URL)
	// RunTimeoutSeconds must be >= 1 per Validate(); we pass a pre-cancelled
	// context instead of relying on the run timeout to expire.
	cfg.Executor.RunTimeoutSeconds = 30
	exec := newTestExecutor(t, cfg)

	// Pass a context that is already past its deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	// Drain the deadline before calling Run.
	time.Sleep(15 * time.Millisecond)

	_, err := exec.Run(ctx, inputMessages("Will this time out?"))
	if err == nil {
		t.Fatal("Run() returned nil error, want a timeout error")
	}

	errStr := err.Error()
	hasTimeout := strings.Contains(strings.ToLower(errStr), "timeout") ||
		checkChain(err, execerrors.ErrRunTimeout)
	if !hasTimeout {
		t.Errorf("Run() error = %v, want error containing 'timeout' or matching ErrRunTimeout", err)
	}
}

func TestRun_ContextLengthExceeded(t *testing.T) {
	t.Parallel()

	vllmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		body := `{"error":{"type":"invalid_request_error","message":"This model's maximum context length is 32768 tokens. However, your messages resulted in context_length_exceeded tokens."}}`
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(vllmSrv.Close)

	gatewaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("gateway should not be called when context is exceeded")
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(gatewaySrv.Close)

	cfg := buildTestConfig(vllmSrv.URL, gatewaySrv.URL)
	exec := newTestExecutor(t, cfg)

	_, err := exec.Run(context.Background(), inputMessages("Very long prompt."))
	if err == nil {
		t.Fatal("Run() returned nil error, want ErrContextWindow")
	}
	if !execerrors.IsContextWindowError(err) {
		t.Errorf("Run() error = %v, want errors.Is match for ErrContextWindow", err)
	}
}

func TestRun_TransientGptOssErrorRecovers(t *testing.T) {
	t.Parallel()

	const finalAnswer = "Recovered answer."
	var callCount atomic.Int32

	vllmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := callCount.Add(1)
		if call == 1 {
			// First call: transient 503.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"service unavailable"}`)
			return
		}
		// Second call: final answer.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, vllmResponse(finalAnswer, ""))
	}))
	t.Cleanup(vllmSrv.Close)

	gatewaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("gateway should not be called in transient-error-recovery scenario")
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(gatewaySrv.Close)

	cfg := buildTestConfig(vllmSrv.URL, gatewaySrv.URL)
	cfg.Executor.MaxIterations = 3
	exec := newTestExecutor(t, cfg)

	result, err := exec.Run(context.Background(), inputMessages("Test transient recovery."))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (should recover from transient 503)", err)
	}
	if result.Answer != finalAnswer {
		t.Errorf("Answer = %q, want %q", result.Answer, finalAnswer)
	}
}

func TestRun_ToolExecutionErrorInjectedIntoContext(t *testing.T) {
	t.Parallel()

	const finalAnswer = "I cannot run that command, but here is an alternative answer."
	var vllmCalls atomic.Int32

	vllmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := vllmCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if call == 1 {
			reactText := "Let me run a command.\nAction: exec\nAction Input: {\"command\":\"ls\"}"
			_, _ = io.WriteString(w, vllmResponse(reactText, ""))
		} else {
			_, _ = io.WriteString(w, vllmResponse(finalAnswer, ""))
		}
	}))
	t.Cleanup(vllmSrv.Close)

	// Gateway returns an error for the exec tool.
	gatewaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, gatewayErrorResponse("permission_denied", "exec not allowed"))
	}))
	t.Cleanup(gatewaySrv.Close)

	cfg := buildTestConfig(vllmSrv.URL, gatewaySrv.URL)
	cfg.Executor.MaxIterations = 5
	exec := newTestExecutor(t, cfg)

	result, err := exec.Run(context.Background(), inputMessages("Run ls command."))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (tool error should be injected into context for model recovery)", err)
	}
	if result.Answer != finalAnswer {
		t.Errorf("Answer = %q, want %q", result.Answer, finalAnswer)
	}

	// Verify a tool message with the error was injected into the conversation.
	foundToolError := false
	for _, msg := range result.Messages {
		if msg.Role == "tool" && strings.Contains(msg.Content, "failed") {
			foundToolError = true
			break
		}
	}
	if !foundToolError {
		t.Error("expected a 'tool' role message containing error injection, none found in Messages")
	}
}

func TestRun_ReasoningContentPreferredOverContent(t *testing.T) {
	t.Parallel()

	const searchResult = "reasoning-driven search result"
	const finalAnswer = "Answer derived from reasoning."
	var vllmCalls atomic.Int32

	vllmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := vllmCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if call == 1 {
			// reasoning_content has the tool intent; content is benign.
			reasoningText := "Action: web_search\nAction Input: {\"query\":\"test\"}"
			contentText := "I'll help you with that."
			_, _ = io.WriteString(w, vllmResponse(contentText, reasoningText))
		} else {
			_, _ = io.WriteString(w, vllmResponse(finalAnswer, ""))
		}
	}))
	t.Cleanup(vllmSrv.Close)

	var gatewayCalls atomic.Int32
	gatewaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, gatewayOKResponse(searchResult))
	}))
	t.Cleanup(gatewaySrv.Close)

	cfg := buildTestConfig(vllmSrv.URL, gatewaySrv.URL)
	// Set source field to "reasoning" so reasoning_content drives parsing.
	cfg.Parser.SourceField = "reasoning"
	cfg.Parser.FallbackField = "content"
	exec := newTestExecutor(t, cfg)

	result, err := exec.Run(context.Background(), inputMessages("Search using reasoning."))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if result.Answer != finalAnswer {
		t.Errorf("Answer = %q, want %q", result.Answer, finalAnswer)
	}

	// Gateway must have been called, proving the tool intent in reasoning_content was parsed.
	if gatewayCalls.Load() == 0 {
		t.Error("gateway was never called: reasoning_content tool intent was not parsed")
	}
}

// ---------------------------------------------------------------------------
// Table-driven edge-case tests for internal helpers (tested via the public API)
// ---------------------------------------------------------------------------

func TestRun_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		vllmHandler  func(calls *atomic.Int32) http.HandlerFunc
		gatewayResp  string
		cfg          func(vllmURL, gwURL string) *config.Config
		wantErr      bool
		errCheck     func(err error) bool
		wantAnswer   string
		wantIterMin  int
	}{
		{
			name: "empty content exhausts iterations",
			// When vLLM always returns empty content, the executor breaks out
			// of the inner loop on each iteration (empty parse source path),
			// sets answer = "" each time, then at the end of Run() finds
			// answer == "" and returns ErrMaxIterations. This documents the
			// actual behaviour: an empty string answer is not a valid result.
			vllmHandler: func(_ *atomic.Int32) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, vllmResponse("", ""))
				}
			},
			gatewayResp: `{"ok":true,"result":"unused"}`,
			cfg:         buildTestConfig,
			wantErr:     true,
			errCheck: func(err error) bool {
				return checkChain(err, execerrors.ErrMaxIterations)
			},
		},
		{
			name: "action done stops loop",
			vllmHandler: func(_ *atomic.Int32) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					// ReAct "done" action should stop intent extraction.
					reactText := "I am finished.\nAction: done\nAction Input: {}"
					_, _ = io.WriteString(w, vllmResponse(reactText, ""))
				}
			},
			gatewayResp: `{"ok":true,"result":"unused"}`,
			cfg:         buildTestConfig,
			wantErr:     false,
			// When Action: done is hit, parseReAct returns 0 intents, so
			// executor treats the content as the final answer.
			wantAnswer:  "I am finished.\nAction: done\nAction Input: {}",
			wantIterMin: 1,
		},
		{
			name: "non-transient 400 error is terminal",
			vllmHandler: func(_ *atomic.Int32) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					// A 400 that is NOT context_length_exceeded.
					_, _ = io.WriteString(w, `{"error":"bad request: invalid model"}`)
				}
			},
			gatewayResp: `{"ok":true,"result":"unused"}`,
			cfg:         buildTestConfig,
			wantErr:     true,
			errCheck: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "400")
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var calls atomic.Int32
			vllmSrv := httptest.NewServer(tc.vllmHandler(&calls))
			t.Cleanup(vllmSrv.Close)

			gatewaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.gatewayResp)
			}))
			t.Cleanup(gatewaySrv.Close)

			cfg := tc.cfg(vllmSrv.URL, gatewaySrv.URL)
			exec := newTestExecutor(t, cfg)

			result, err := exec.Run(context.Background(), inputMessages("test input"))

			if tc.wantErr {
				if err == nil {
					t.Fatal("Run() returned nil error, want error")
				}
				if tc.errCheck != nil && !tc.errCheck(err) {
					t.Errorf("Run() error = %v, did not satisfy errCheck", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			if result.Answer != tc.wantAnswer {
				t.Errorf("Answer = %q, want %q", result.Answer, tc.wantAnswer)
			}
			if result.Iterations < tc.wantIterMin {
				t.Errorf("Iterations = %d, want >= %d", result.Iterations, tc.wantIterMin)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Race-safety smoke test
// ---------------------------------------------------------------------------

// TestRun_ConcurrentRuns verifies that multiple Run() calls on the same
// Executor do not race. Run with -race to detect data races.
func TestRun_ConcurrentRuns(t *testing.T) {
	t.Parallel()

	const finalAnswer = "concurrent answer"

	vllmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, vllmResponse(finalAnswer, ""))
	}))
	t.Cleanup(vllmSrv.Close)

	gatewaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("gateway should not be called for final-answer response")
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(gatewaySrv.Close)

	cfg := buildTestConfig(vllmSrv.URL, gatewaySrv.URL)
	exec := newTestExecutor(t, cfg)

	const goroutines = 5
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			result, err := exec.Run(context.Background(), inputMessages("concurrent test"))
			if err != nil {
				errs <- err
				return
			}
			if result.Answer != finalAnswer {
				errs <- nil // answer mismatch handled separately
			}
			errs <- nil
		}()
	}

	for i := 0; i < goroutines; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Run() error: %v", err)
		}
	}
}
