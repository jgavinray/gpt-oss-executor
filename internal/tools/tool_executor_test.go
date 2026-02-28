package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jgavinray/gpt-oss-executor/internal/parser"
)

// ---------------------------------------------------------------------------
// Mock gateway helpers
// ---------------------------------------------------------------------------

// capturedRequest records the decoded JSON body that the mock gateway received.
type capturedRequest struct {
	Tool       string                 `json:"tool"`
	Action     string                 `json:"action,omitempty"`
	Args       map[string]interface{} `json:"args,omitempty"`
	SessionKey string                 `json:"sessionKey,omitempty"`
}

// gatewayResponse is a convenience struct for building mock responses.
type gatewayResponse struct {
	OK     bool        `json:"ok"`
	Result interface{} `json:"result,omitempty"`
	Error  interface{} `json:"error,omitempty"`
}

// mockGatewayServer spins up an httptest.Server that serves POST /tools/invoke.
// Callers supply a handler function that receives the decoded request and
// returns the HTTP status code and response body to send back.
func mockGatewayServer(
	t *testing.T,
	handle func(req capturedRequest) (statusCode int, resp gatewayResponse),
) (*httptest.Server, *capturedRequest) {
	t.Helper()

	var mu sync.Mutex
	var last capturedRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tools/invoke" || r.Method != http.MethodPost {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		var req capturedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}

		mu.Lock()
		last = req
		mu.Unlock()

		statusCode, resp := handle(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_ = json.NewEncoder(w).Encode(resp)
	}))

	t.Cleanup(srv.Close)
	return srv, &last
}

// successHandler returns a simple OK gateway response with a string result.
func successHandler(result string) func(capturedRequest) (int, gatewayResponse) {
	encoded, _ := json.Marshal(result)
	raw := json.RawMessage(encoded)
	return func(_ capturedRequest) (int, gatewayResponse) {
		return http.StatusOK, gatewayResponse{OK: true, Result: raw}
	}
}

// newToolExecutor builds a ToolExecutor wired to a real GatewayClient pointing
// at baseURL. logger is discarded (no output during tests).
func newToolExecutor(t *testing.T, baseURL string, resultLimits map[string]int, maxRetries int) *ToolExecutor {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(noopWriter{}, nil))
	return &ToolExecutor{
		Gateway: &GatewayClient{
			BaseURL:    baseURL,
			Token:      "test-token",
			SessionKey: "main",
			Client:     &http.Client{},
		},
		ResultLimits: resultLimits,
		MaxRetries:   maxRetries,
		Logger:       logger,
	}
}

// noopWriter discards all log output.
type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }

// getFloat64 extracts a float64 from a map[string]interface{}, which is what
// json.Unmarshal uses for JSON numbers.
func getFloat64(t *testing.T, m map[string]interface{}, key string) float64 {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("key %q missing from args map", key)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("key %q: expected float64, got %T (%v)", key, v, v)
	}
	return f
}

// getString extracts a string from a map[string]interface{}.
func getString(t *testing.T, m map[string]interface{}, key string) string {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("key %q missing from args map", key)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("key %q: expected string, got %T (%v)", key, v, v)
	}
	return s
}

// hasKey reports whether key exists in the map.
func hasKey(m map[string]interface{}, key string) bool {
	_, ok := m[key]
	return ok
}

// ---------------------------------------------------------------------------
// Argument mapping tests
// ---------------------------------------------------------------------------

func TestToolExecutor_ArgumentMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		intent      parser.ToolIntent
		checkArgs   func(t *testing.T, captured capturedRequest)
		checkTool   string
		checkSessKey string
	}{
		{
			name: "web_search maps query and count as int",
			intent: parser.ToolIntent{
				Name: "web_search",
				Args: map[string]string{"query": "test", "count": "5"},
			},
			checkTool:    "web_search",
			checkSessKey: "main",
			checkArgs: func(t *testing.T, captured capturedRequest) {
				t.Helper()
				if got := getString(t, captured.Args, "query"); got != "test" {
					t.Errorf("args.query: got %q, want %q", got, "test")
				}
				if got := getFloat64(t, captured.Args, "count"); got != 5 {
					t.Errorf("args.count: got %v, want 5", got)
				}
			},
		},
		{
			name: "web_fetch maps url and max_chars as camelCase int",
			intent: parser.ToolIntent{
				Name: "web_fetch",
				Args: map[string]string{"url": "https://example.com", "max_chars": "1000"},
			},
			checkTool: "web_fetch",
			checkArgs: func(t *testing.T, captured capturedRequest) {
				t.Helper()
				if got := getString(t, captured.Args, "url"); got != "https://example.com" {
					t.Errorf("args.url: got %q, want %q", got, "https://example.com")
				}
				if got := getString(t, captured.Args, "extractMode"); got != "markdown" {
					t.Errorf("args.extractMode: got %q, want %q", got, "markdown")
				}
				if got := getFloat64(t, captured.Args, "maxChars"); got != 1000 {
					t.Errorf("args.maxChars: got %v, want 1000", got)
				}
			},
		},
		{
			name: "web_fetch without max_chars omits maxChars key",
			intent: parser.ToolIntent{
				Name: "web_fetch",
				Args: map[string]string{"url": "https://example.com"},
			},
			checkTool: "web_fetch",
			checkArgs: func(t *testing.T, captured capturedRequest) {
				t.Helper()
				if got := getString(t, captured.Args, "extractMode"); got != "markdown" {
					t.Errorf("args.extractMode: got %q, want %q", got, "markdown")
				}
				if hasKey(captured.Args, "maxChars") {
					t.Errorf("args.maxChars should be absent when max_chars not provided")
				}
			},
		},
		{
			name: "read maps path unchanged",
			intent: parser.ToolIntent{
				Name: "read",
				Args: map[string]string{"path": "/tmp/file.txt"},
			},
			checkTool: "read",
			checkArgs: func(t *testing.T, captured capturedRequest) {
				t.Helper()
				if got := getString(t, captured.Args, "path"); got != "/tmp/file.txt" {
					t.Errorf("args.path: got %q, want %q", got, "/tmp/file.txt")
				}
			},
		},
		{
			name: "write maps content key to file_text",
			intent: parser.ToolIntent{
				Name: "write",
				Args: map[string]string{"path": "/tmp/out.txt", "content": "hello"},
			},
			checkTool: "write",
			checkArgs: func(t *testing.T, captured capturedRequest) {
				t.Helper()
				if got := getString(t, captured.Args, "path"); got != "/tmp/out.txt" {
					t.Errorf("args.path: got %q, want %q", got, "/tmp/out.txt")
				}
				if got := getString(t, captured.Args, "file_text"); got != "hello" {
					t.Errorf("args.file_text: got %q, want %q", got, "hello")
				}
				if hasKey(captured.Args, "content") {
					t.Errorf("args should not contain 'content' key; gateway expects 'file_text'")
				}
			},
		},
		{
			name: "write with file_text key passes through correctly",
			intent: parser.ToolIntent{
				Name: "write",
				Args: map[string]string{"path": "/tmp/out.txt", "file_text": "world"},
			},
			checkTool: "write",
			checkArgs: func(t *testing.T, captured capturedRequest) {
				t.Helper()
				if got := getString(t, captured.Args, "file_text"); got != "world" {
					t.Errorf("args.file_text: got %q, want %q", got, "world")
				}
			},
		},
		{
			name: "exec maps command and sets timeout int 60",
			intent: parser.ToolIntent{
				Name: "exec",
				Args: map[string]string{"command": "ls -la"},
			},
			checkTool: "exec",
			checkArgs: func(t *testing.T, captured capturedRequest) {
				t.Helper()
				if got := getString(t, captured.Args, "command"); got != "ls -la" {
					t.Errorf("args.command: got %q, want %q", got, "ls -la")
				}
				if got := getFloat64(t, captured.Args, "timeout"); got != 60 {
					t.Errorf("args.timeout: got %v, want 60", got)
				}
				if hasKey(captured.Args, "timeout_seconds") {
					t.Errorf("args should use 'timeout' not 'timeout_seconds'")
				}
			},
		},
		{
			name: "browser maps action and url",
			intent: parser.ToolIntent{
				Name: "browser",
				Args: map[string]string{"action": "navigate", "url": "https://example.com"},
			},
			checkTool: "browser",
			checkArgs: func(t *testing.T, captured capturedRequest) {
				t.Helper()
				if got := getString(t, captured.Args, "action"); got != "navigate" {
					t.Errorf("args.action: got %q, want %q", got, "navigate")
				}
				if got := getString(t, captured.Args, "url"); got != "https://example.com" {
					t.Errorf("args.url: got %q, want %q", got, "https://example.com")
				}
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv, captured := mockGatewayServer(t, successHandler(`"ok"`))
			te := newToolExecutor(t, srv.URL, nil, 1)

			_, err := te.Execute(context.Background(), tc.intent)
			if err != nil {
				t.Fatalf("Execute returned unexpected error: %v", err)
			}

			if tc.checkTool != "" && captured.Tool != tc.checkTool {
				t.Errorf("tool: got %q, want %q", captured.Tool, tc.checkTool)
			}
			if tc.checkSessKey != "" && captured.SessionKey != tc.checkSessKey {
				t.Errorf("sessionKey: got %q, want %q", captured.SessionKey, tc.checkSessKey)
			}
			if tc.checkArgs != nil {
				tc.checkArgs(t, *captured)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// truncateResult tests
// ---------------------------------------------------------------------------

func TestTruncateResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		toolName     string
		input        string
		resultLimits map[string]int
		wantResult   string
		wantTrunc    bool
	}{
		{
			name:         "result shorter than limit returned unchanged",
			toolName:     "web_search",
			input:        "short result",
			resultLimits: map[string]int{"web_search": 100},
			wantResult:   "short result",
			wantTrunc:    false,
		},
		{
			name:         "result longer than limit is truncated with suffix",
			toolName:     "web_search",
			input:        strings.Repeat("x", 50),
			resultLimits: map[string]int{"web_search": 20},
			wantTrunc:    true,
		},
		{
			name:         "no limit configured uses default 3000 chars",
			toolName:     "some_unknown_tool",
			input:        strings.Repeat("y", 2999),
			resultLimits: nil,
			wantResult:   strings.Repeat("y", 2999),
			wantTrunc:    false,
		},
		{
			name:         "result exactly at limit is not truncated",
			toolName:     "read",
			input:        strings.Repeat("z", 3000),
			resultLimits: nil,
			wantResult:   strings.Repeat("z", 3000),
			wantTrunc:    false,
		},
		{
			name:         "result one over default limit is truncated",
			toolName:     "read",
			input:        strings.Repeat("a", 3001),
			resultLimits: nil,
			wantTrunc:    true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			logger := slog.New(slog.NewTextHandler(noopWriter{}, nil))
			te := &ToolExecutor{
				ResultLimits: tc.resultLimits,
				Logger:       logger,
			}

			got := te.truncateResult(tc.toolName, tc.input)

			if tc.wantTrunc {
				if !strings.Contains(got, "[truncated:") {
					t.Errorf("expected truncation suffix in result, got: %q", got[:min(len(got), 80)])
				}
				// Verify the truncated portion starts with the right prefix.
				limit := 3000
				if tc.resultLimits != nil {
					if l, ok := tc.resultLimits[tc.toolName]; ok && l > 0 {
						limit = l
					}
				}
				if !strings.HasPrefix(got, tc.input[:limit]) {
					t.Errorf("truncated result should start with first %d chars of input", limit)
				}
				omitted := len(tc.input) - limit
				wantSuffix := fmt.Sprintf("\n... [truncated: %d chars omitted]", omitted)
				if !strings.HasSuffix(got, wantSuffix) {
					t.Errorf("truncated suffix: got %q, want suffix %q", got[len(got)-len(wantSuffix):], wantSuffix)
				}
			} else {
				if got != tc.wantResult {
					t.Errorf("result mismatch:\ngot  %q\nwant %q", got, tc.wantResult)
				}
			}
		})
	}
}

// min returns the smaller of a and b. Named to avoid collision with builtin min
// (added in Go 1.21); this is a local helper for Go 1.22 compat.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Retry tests
// ---------------------------------------------------------------------------

func TestToolExecutor_Retry(t *testing.T) {
	t.Parallel()

	t.Run("503 twice then success returns ok", func(t *testing.T) {
		t.Parallel()

		attempts := 0
		srv, _ := mockGatewayServer(t, func(_ capturedRequest) (int, gatewayResponse) {
			attempts++
			if attempts < 3 {
				return http.StatusServiceUnavailable, gatewayResponse{OK: false}
			}
			encoded, _ := json.Marshal("done")
			return http.StatusOK, gatewayResponse{OK: true, Result: json.RawMessage(encoded)}
		})

		te := newToolExecutor(t, srv.URL, nil, 3)
		intent := parser.ToolIntent{
			Name: "read",
			Args: map[string]string{"path": "/tmp/test.txt"},
		}

		result, err := te.Execute(context.Background(), intent)
		if err != nil {
			t.Fatalf("Execute returned error after eventual success: %v", err)
		}
		if result == "" {
			t.Error("expected non-empty result")
		}
		if attempts != 3 {
			t.Errorf("expected 3 gateway attempts, got %d", attempts)
		}
	})

	t.Run("HTTP 400 non-retryable returns error immediately", func(t *testing.T) {
		t.Parallel()

		attempts := 0
		srv, _ := mockGatewayServer(t, func(_ capturedRequest) (int, gatewayResponse) {
			attempts++
			return http.StatusBadRequest, gatewayResponse{OK: false}
		})

		te := newToolExecutor(t, srv.URL, nil, 3)
		intent := parser.ToolIntent{
			Name: "read",
			Args: map[string]string{"path": "/tmp/test.txt"},
		}

		_, err := te.Execute(context.Background(), intent)
		if err == nil {
			t.Fatal("expected error for HTTP 400, got nil")
		}
		if attempts != 1 {
			t.Errorf("expected 1 gateway attempt for non-retryable error, got %d", attempts)
		}
	})

	t.Run("context cancelled during retry backoff returns ctx error", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())

		// Cancel the context on the first gateway response so the backoff
		// select case fires during the second attempt.
		var cancelOnce sync.Once
		srv, _ := mockGatewayServer(t, func(_ capturedRequest) (int, gatewayResponse) {
			cancelOnce.Do(cancel)
			return http.StatusServiceUnavailable, gatewayResponse{OK: false}
		})

		te := newToolExecutor(t, srv.URL, nil, 10)
		intent := parser.ToolIntent{
			Name: "read",
			Args: map[string]string{"path": "/tmp/test.txt"},
		}

		_, err := te.Execute(ctx, intent)
		if err == nil {
			t.Fatal("expected error when context cancelled, got nil")
		}
		if !strings.Contains(err.Error(), "context") {
			t.Errorf("expected context-related error, got: %v", err)
		}
	})
}
