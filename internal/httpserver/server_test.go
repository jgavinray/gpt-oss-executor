package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jgavinray/gpt-oss-executor/internal/config"
	execerrors "github.com/jgavinray/gpt-oss-executor/internal/errors"
	"github.com/jgavinray/gpt-oss-executor/internal/executor"

	"log/slog"
)

// stubRunner implements Runner for unit tests. It returns a pre-configured
// result or error without touching the real executor.
type stubRunner struct {
	result *executor.RunResult
	err    error
}

func (s *stubRunner) Run(ctx context.Context, msgs []executor.Message) (*executor.RunResult, error) {
	return s.result, s.err
}

// minimalConfig returns a *config.Config that satisfies the Server constructor
// without requiring a real file on disk.
func minimalConfig() *config.Config {
	return &config.Config{
		Executor: config.ExecutorConfig{
			GptOSSURL:            "http://localhost:9999",
			GptOSSModel:          "gpt-oss-test",
			OpenClawGatewayURL:   "http://localhost:9998",
			OpenClawGatewayToken: "test-token",
			MaxIterations:        5,
			RunTimeoutSeconds:    30,
		},
		HTTPServer: config.HTTPServerConfig{
			Bind:                   "127.0.0.1",
			Port:                   0,
			ReadTimeoutSeconds:     5,
			WriteTimeoutSeconds:    5,
			IdleTimeoutSeconds:     30,
			ShutdownTimeoutSeconds: 5,
		},
	}
}

// newTestServer builds a Server with the given stub and returns its internal
// http.Handler so tests can drive it directly with httptest.NewRecorder.
func newTestServer(t *testing.T, runner Runner) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil))
	return New(minimalConfig(), runner, logger)
}

// doRequest fires req against srv's mux via an httptest.ResponseRecorder and
// returns the recorder.
func doRequest(t *testing.T, srv *Server, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rr, req)
	return rr
}

// postCompletions is a helper that builds a POST /v1/chat/completions request
// with the supplied body string.
func postCompletions(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// decodeJSON is a generic helper that unmarshals the recorder body into dst.
func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder, dst interface{}) {
	t.Helper()
	if err := json.NewDecoder(rr.Body).Decode(dst); err != nil {
		t.Fatalf("decoding response JSON: %v\nbody: %s", err, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// POST /v1/chat/completions tests
// ---------------------------------------------------------------------------

func TestHandleChatCompletions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		runner         Runner
		body           string
		wantStatus     int
		checkResponse  func(t *testing.T, rr *httptest.ResponseRecorder)
	}{
		{
			name: "success returns 200 with answer in choices",
			runner: &stubRunner{
				result: &executor.RunResult{
					RunID:      "abc",
					Answer:     "hello",
					Iterations: 1,
				},
			},
			body:       `{"model":"gpt-oss","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusOK,
			checkResponse: func(t *testing.T, rr *httptest.ResponseRecorder) {
				t.Helper()
				var resp chatResponse
				decodeJSON(t, rr, &resp)
				if resp.ID != "chatcmpl-abc" {
					t.Errorf("id: got %q, want %q", resp.ID, "chatcmpl-abc")
				}
				if len(resp.Choices) == 0 {
					t.Fatalf("choices is empty")
				}
				if got := resp.Choices[0].Message.Content; got != "hello" {
					t.Errorf("choices[0].message.content: got %q, want %q", got, "hello")
				}
			},
		},
		{
			name:       "empty messages returns 400 invalid_request_error",
			runner:     &stubRunner{},
			body:       `{"model":"gpt-oss","messages":[]}`,
			wantStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, rr *httptest.ResponseRecorder) {
				t.Helper()
				var resp errorResponse
				decodeJSON(t, rr, &resp)
				if resp.Error.Type != "invalid_request_error" {
					t.Errorf("error.type: got %q, want %q", resp.Error.Type, "invalid_request_error")
				}
			},
		},
		{
			name:       "invalid JSON returns 400",
			runner:     &stubRunner{},
			body:       `{bad json`,
			wantStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, rr *httptest.ResponseRecorder) {
				t.Helper()
				var resp errorResponse
				decodeJSON(t, rr, &resp)
				if resp.Error.Type != "invalid_request_error" {
					t.Errorf("error.type: got %q, want %q", resp.Error.Type, "invalid_request_error")
				}
			},
		},
		{
			name: "ErrMaxIterations returns 500 max_iterations_exceeded",
			runner: &stubRunner{
				err: fmt.Errorf("run failed: %w", execerrors.ErrMaxIterations),
			},
			body:       `{"model":"gpt-oss","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusInternalServerError,
			checkResponse: func(t *testing.T, rr *httptest.ResponseRecorder) {
				t.Helper()
				var resp errorResponse
				decodeJSON(t, rr, &resp)
				if resp.Error.Code != "max_iterations_exceeded" {
					t.Errorf("error.code: got %q, want %q", resp.Error.Code, "max_iterations_exceeded")
				}
			},
		},
		{
			name: "ErrRunTimeout returns 504",
			runner: &stubRunner{
				err: fmt.Errorf("run failed: %w", execerrors.ErrRunTimeout),
			},
			body:       `{"model":"gpt-oss","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusGatewayTimeout,
			checkResponse: func(t *testing.T, rr *httptest.ResponseRecorder) {
				t.Helper()
				var resp errorResponse
				decodeJSON(t, rr, &resp)
				if resp.Error.Type != "server_error" {
					t.Errorf("error.type: got %q, want %q", resp.Error.Type, "server_error")
				}
			},
		},
		{
			name: "ErrGptOssUnreachable returns 502",
			runner: &stubRunner{
				err: fmt.Errorf("run failed: %w", execerrors.ErrGptOssUnreachable),
			},
			body:       `{"model":"gpt-oss","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusBadGateway,
			checkResponse: func(t *testing.T, rr *httptest.ResponseRecorder) {
				t.Helper()
				var resp errorResponse
				decodeJSON(t, rr, &resp)
				if resp.Error.Type != "server_error" {
					t.Errorf("error.type: got %q, want %q", resp.Error.Type, "server_error")
				}
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newTestServer(t, tc.runner)
			req := postCompletions(t, tc.body)
			rr := doRequest(t, srv, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d\nbody: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.checkResponse != nil {
				tc.checkResponse(t, rr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GET /health tests
// ---------------------------------------------------------------------------

func TestHandleHealth(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubRunner{})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := doRequest(t, srv, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rr.Code, http.StatusOK)
	}

	var body map[string]string
	decodeJSON(t, rr, &body)

	if got := body["status"]; got != "ok" {
		t.Errorf("status field: got %q, want %q", got, "ok")
	}
}

// ---------------------------------------------------------------------------
// classifyRunError unit tests
// ---------------------------------------------------------------------------

func TestClassifyRunError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		err            error
		wantStatus     int
		wantType       string
		wantCode       string
	}{
		{
			name:       "ErrMaxIterations",
			err:        execerrors.ErrMaxIterations,
			wantStatus: http.StatusInternalServerError,
			wantType:   "server_error",
			wantCode:   "max_iterations_exceeded",
		},
		{
			name:       "ErrRunTimeout",
			err:        execerrors.ErrRunTimeout,
			wantStatus: http.StatusGatewayTimeout,
			wantType:   "server_error",
			wantCode:   "timeout_exceeded",
		},
		{
			name:       "ErrGptOssUnreachable",
			err:        execerrors.ErrGptOssUnreachable,
			wantStatus: http.StatusBadGateway,
			wantType:   "server_error",
			wantCode:   "upstream_unavailable",
		},
		{
			name:       "ErrContextWindow",
			err:        execerrors.ErrContextWindow,
			wantStatus: http.StatusBadRequest,
			wantType:   "invalid_request_error",
			wantCode:   "context_length_exceeded",
		},
		{
			name:       "wrapped ErrMaxIterations",
			err:        execerrors.Wrap(execerrors.ErrMaxIterations, fmt.Errorf("some cause")),
			wantStatus: http.StatusInternalServerError,
			wantType:   "server_error",
			wantCode:   "max_iterations_exceeded",
		},
		{
			name:       "unknown error",
			err:        fmt.Errorf("some unknown failure"),
			wantStatus: http.StatusInternalServerError,
			wantType:   "server_error",
			wantCode:   "",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotStatus, gotType, gotCode := classifyRunError(tc.err)
			if gotStatus != tc.wantStatus {
				t.Errorf("status: got %d, want %d", gotStatus, tc.wantStatus)
			}
			if gotType != tc.wantType {
				t.Errorf("errType: got %q, want %q", gotType, tc.wantType)
			}
			if gotCode != tc.wantCode {
				t.Errorf("code: got %q, want %q", gotCode, tc.wantCode)
			}
		})
	}
}
