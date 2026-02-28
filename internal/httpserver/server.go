// Package httpserver provides an OpenAI-compatible HTTP server for the
// gpt-oss-executor. It exposes POST /v1/chat/completions, which drives the
// agentic loop, and GET /health for readiness checks.
package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jgavinray/gpt-oss-executor/internal/config"
	execerrors "github.com/jgavinray/gpt-oss-executor/internal/errors"
	"github.com/jgavinray/gpt-oss-executor/internal/executor"
)

// Runner executes an agentic loop for the given messages and returns the result.
type Runner interface {
	Run(ctx context.Context, messages []executor.Message) (*executor.RunResult, error)
}

// Server wraps an *http.Server and holds references to the dependencies
// needed by the request handlers.
type Server struct {
	httpSrv *http.Server
	exec    Runner
	cfg     *config.Config
	logger  *slog.Logger
}

// New constructs a Server configured from cfg, wired to exec. The underlying
// http.Server is created but not started; call ListenAndServe to begin
// accepting connections.
func New(cfg *config.Config, exec Runner, logger *slog.Logger) *Server {
	s := &Server{
		exec:   exec,
		cfg:    cfg,
		logger: logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("GET /health", s.handleHealth)

	addr := fmt.Sprintf("%s:%d", cfg.HTTPServer.Bind, cfg.HTTPServer.Port)

	s.httpSrv = &http.Server{
		Addr:         addr,
		Handler:      loggingMiddleware(logger, mux),
		ReadTimeout:  time.Duration(cfg.HTTPServer.ReadTimeoutSeconds) * time.Second,
		WriteTimeout: time.Duration(cfg.HTTPServer.WriteTimeoutSeconds) * time.Second,
		IdleTimeout:  time.Duration(cfg.HTTPServer.IdleTimeoutSeconds) * time.Second,
	}

	return s
}

// ListenAndServe starts the HTTP server. It blocks until the server is shut
// down. The caller should call Shutdown in a separate goroutine (e.g. on
// signal receipt) to unblock this method.
func (s *Server) ListenAndServe() error {
	s.logger.Info("HTTP server starting",
		slog.String("addr", s.httpSrv.Addr),
	)
	if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("httpserver: listen: %w", err)
	}
	return nil
}

// Shutdown gracefully stops the server, waiting up to the configured
// shutdown timeout for in-flight requests to complete.
func (s *Server) Shutdown(ctx context.Context) error {
	timeout := time.Duration(s.cfg.HTTPServer.ShutdownTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	s.logger.Info("HTTP server shutting down")
	if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("httpserver: shutdown: %w", err)
	}
	return nil
}

// Addr returns the address the server is configured to listen on.
func (s *Server) Addr() string {
	return s.httpSrv.Addr
}

// ---------------------------------------------------------------------------
// Request / response types
// ---------------------------------------------------------------------------

// chatRequest is the subset of the OpenAI chat completions request body that
// this executor consumes.
type chatRequest struct {
	Model    string             `json:"model"`
	Messages []chatMessage      `json:"messages"`
	MaxTokens int               `json:"max_tokens,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the OpenAI-compatible response returned by
// POST /v1/chat/completions.
type chatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []chatChoice   `json:"choices"`
	Usage   chatUsage      `json:"usage"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// errorResponse is the OpenAI-compatible error body.
type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// handleChatCompletions implements POST /v1/chat/completions.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("invalid JSON body: %s", err.Error()), "")
		return
	}

	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"messages array must not be empty", "")
		return
	}

	// Convert to executor messages.
	execMessages := make([]executor.Message, len(req.Messages))
	for i, m := range req.Messages {
		execMessages[i] = executor.Message{Role: m.Role, Content: m.Content}
	}

	result, err := s.exec.Run(r.Context(), execMessages)
	if err != nil {
		s.logger.Error("run failed", slog.String("error", err.Error()))
		statusCode, errType, code := classifyRunError(err)
		writeError(w, statusCode, errType, err.Error(), code)
		return
	}

	resp := chatResponse{
		ID:      "chatcmpl-" + result.RunID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   s.cfg.Executor.GptOSSModel,
		Choices: []chatChoice{
			{
				Index: 0,
				Message: chatMessage{
					Role:    "assistant",
					Content: result.Answer,
				},
				FinishReason: "stop",
			},
		},
		Usage: chatUsage{},
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleHealth implements GET /health with a simple liveness check.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"model":  s.cfg.Executor.GptOSSModel,
	})
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// loggingMiddleware logs each request's method, path, and latency.
func loggingMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(lrw, r)
		logger.Info("http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", lrw.statusCode),
			slog.String("remote_addr", remoteAddr(r)),
			slog.Duration("latency", time.Since(start)),
		)
	})
}

// loggingResponseWriter captures the status code written by a handler.
type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

// remoteAddr returns the client IP, preferring X-Forwarded-For when behind a
// proxy. Falls back to r.RemoteAddr.
func remoteAddr(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// writeJSON serialises v as JSON and writes it to w.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes an OpenAI-compatible JSON error response.
func writeError(w http.ResponseWriter, status int, errType, message, code string) {
	writeJSON(w, status, errorResponse{
		Error: errorDetail{
			Message: message,
			Type:    errType,
			Code:    code,
		},
	})
}

// classifyRunError maps executor errors to HTTP status codes and OpenAI error
// types. Unknown errors become HTTP 500 server_error.
func classifyRunError(err error) (statusCode int, errType, code string) {
	switch {
	case execerrors.IsContextWindowError(err):
		return http.StatusBadRequest, "invalid_request_error", "context_length_exceeded"
	case isErr(err, execerrors.ErrMaxIterations):
		return http.StatusInternalServerError, "server_error", "max_iterations_exceeded"
	case isErr(err, execerrors.ErrRunTimeout):
		return http.StatusGatewayTimeout, "server_error", "timeout_exceeded"
	case isErr(err, execerrors.ErrGptOssUnreachable):
		return http.StatusBadGateway, "server_error", "upstream_unavailable"
	default:
		return http.StatusInternalServerError, "server_error", ""
	}
}

// isErr is a convenience wrapper around errors.Is for sentinel matching.
func isErr(err error, target error) bool {
	// Use the standard errors.Is which traverses chains and calls our Is() method.
	return execerrors.IsTransientError(err) && execerrors.IsTransientError(target) ||
		errorCode(err) == errorCode(target)
}

// errorCode extracts the Code field from an *execerrors.ExecutorError, or "".
func errorCode(err error) string {
	if err == nil {
		return ""
	}
	type coder interface{ Error() string }
	// Walk the chain looking for an ExecutorError.
	type unwrapper interface{ Unwrap() error }
	for e := err; e != nil; {
		if ee, ok := e.(*execerrors.ExecutorError); ok {
			return ee.Code
		}
		if u, ok := e.(unwrapper); ok {
			e = u.Unwrap()
		} else {
			break
		}
	}
	return ""
}
