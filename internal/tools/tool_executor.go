// Package tools provides a GatewayClient for calling the OpenClaw /tools/invoke
// endpoint and a ToolExecutor that maps parser.ToolIntent values to the exact
// argument shapes required by the gateway.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jgavinray/gpt-oss-executor/internal/parser"
)

// GatewayClient handles all /tools/invoke calls against the OpenClaw gateway.
// The zero value is not usable; construct with a non-nil Client.
type GatewayClient struct {
	BaseURL    string
	Token      string
	SessionKey string
	Client     *http.Client
}

// invokeRequest is the JSON body sent to POST /tools/invoke.
type invokeRequest struct {
	Tool       string                 `json:"tool"`
	Action     string                 `json:"action,omitempty"`
	Args       map[string]interface{} `json:"args,omitempty"`
	SessionKey string                 `json:"sessionKey,omitempty"`
}

// invokeResponse is the JSON body returned by /tools/invoke.
type invokeResponse struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *invokeError    `json:"error,omitempty"`
}

type invokeError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Invoke calls POST /tools/invoke on the gateway and returns the raw JSON
// result as a string. It does not retry; retry logic lives in ToolExecutor.
func (g *GatewayClient) Invoke(ctx context.Context, toolName string, args map[string]interface{}) (string, error) {
	reqBody := invokeRequest{
		Tool:       toolName,
		Args:       args,
		SessionKey: g.SessionKey,
	}

	encoded, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("tools: marshalling invoke request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+"/tools/invoke", bytes.NewReader(encoded))
	if err != nil {
		return "", fmt.Errorf("tools: building invoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.Token)

	resp, err := g.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tools: HTTP request to gateway: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("tools: reading gateway response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tools: gateway returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var invokeResp invokeResponse
	if err := json.Unmarshal(body, &invokeResp); err != nil {
		return "", fmt.Errorf("tools: unmarshalling gateway response: %w", err)
	}

	if !invokeResp.OK {
		if invokeResp.Error != nil {
			return "", fmt.Errorf("tools: gateway error [%s]: %s", invokeResp.Error.Type, invokeResp.Error.Message)
		}
		return "", fmt.Errorf("tools: gateway returned ok=false with no error detail")
	}

	return string(invokeResp.Result), nil
}

// ToolExecutor routes ToolIntents to the OpenClaw gateway with per-tool
// argument mapping, retry logic, and result truncation.
type ToolExecutor struct {
	Gateway      *GatewayClient
	ResultLimits map[string]int // max chars per tool result; 0/missing → 3000
	MaxRetries   int
	Logger       *slog.Logger
}

// Execute maps intent.Args to the exact argument names expected by the
// OpenClaw gateway, invokes the tool with retry, and truncates the result.
func (te *ToolExecutor) Execute(ctx context.Context, intent parser.ToolIntent) (string, error) {
	args := make(map[string]interface{}, len(intent.Args))

	switch intent.Name {
	case "web_search":
		args["query"] = intent.Args["query"]
		if c, ok := intent.Args["count"]; ok {
			args["count"] = mustParseInt(c, 10)
		}
		if country, ok := intent.Args["country"]; ok && country != "" {
			args["country"] = country
		}
		if freshness, ok := intent.Args["freshness"]; ok && freshness != "" {
			args["freshness"] = freshness
		}

	case "web_fetch":
		args["url"] = intent.Args["url"]
		args["extractMode"] = "markdown" // camelCase; default to markdown
		if mc, ok := intent.Args["max_chars"]; ok {
			args["maxChars"] = mustParseInt(mc, 50000)
		}

	case "read":
		args["path"] = intent.Args["path"]

	case "write":
		args["path"] = intent.Args["path"]
		// OpenClaw write tool uses "file_text", not "content".
		content := intent.Args["content"]
		if content == "" {
			content = intent.Args["file_text"]
		}
		args["file_text"] = content

	case "exec":
		args["command"] = intent.Args["command"]
		if wd, ok := intent.Args["workdir"]; ok && wd != "" {
			args["workdir"] = wd
		}
		// OpenClaw exec uses "timeout" (int, seconds) not "timeout_seconds".
		args["timeout"] = 60

	case "browser":
		args["action"] = intent.Args["action"]
		if u, ok := intent.Args["url"]; ok && u != "" {
			args["url"] = u
		}
		if t, ok := intent.Args["target"]; ok && t != "" {
			args["target"] = t
		}

	default:
		// Pass args through as-is for unknown tools so the gateway can decide.
		for k, v := range intent.Args {
			args[k] = v
		}
	}

	te.Logger.Debug("executing tool", slog.String("tool", intent.Name), slog.Any("args", args))

	result, err := te.executeWithRetry(ctx, intent.Name, args)
	if err != nil {
		return "", err
	}

	return te.truncateResult(intent.Name, result), nil
}

// executeWithRetry calls Gateway.Invoke up to MaxRetries times, backing off
// exponentially for transient errors (HTTP 5xx, connection refused, timeout).
// Non-transient errors (4xx, bad request) are returned immediately.
func (te *ToolExecutor) executeWithRetry(ctx context.Context, toolName string, args map[string]interface{}) (string, error) {
	maxAttempts := te.MaxRetries
	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	var lastErr error
	backoff := time.Second

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			te.Logger.Warn("retrying tool invocation",
				slog.String("tool", toolName),
				slog.Int("attempt", attempt+1),
				slog.Duration("backoff", backoff),
				slog.String("last_error", lastErr.Error()),
			)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return "", fmt.Errorf("tools: context cancelled during retry backoff: %w", ctx.Err())
			}
			backoff *= 2
		}

		result, err := te.Gateway.Invoke(ctx, toolName, args)
		if err == nil {
			return result, nil
		}

		lastErr = err

		if !isRetryable(err) {
			te.Logger.Debug("non-retryable error from gateway",
				slog.String("tool", toolName),
				slog.String("error", err.Error()),
			)
			return "", fmt.Errorf("tools: invoking %s: %w", toolName, err)
		}
	}

	return "", fmt.Errorf("tools: invoking %s after %d attempts: %w", toolName, maxAttempts, lastErr)
}

// isRetryable reports whether err represents a transient condition that is
// safe to retry: HTTP 5xx responses, connection-level failures, or timeouts.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "HTTP 5") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "timeout")
}

// truncateResult caps result at the configured limit for toolName.
// If no limit is configured for the tool, 3000 characters is used.
// Truncated results include a suffix describing how many characters were omitted.
func (te *ToolExecutor) truncateResult(toolName, result string) string {
	limit := 3000
	if te.ResultLimits != nil {
		if l, ok := te.ResultLimits[toolName]; ok && l > 0 {
			limit = l
		}
	}

	if len(result) <= limit {
		return result
	}

	omitted := len(result) - limit
	return result[:limit] + fmt.Sprintf("\n... [truncated: %d chars omitted]", omitted)
}

// mustParseInt parses s as a base-10 integer. If s is empty or not a valid
// integer, def is returned instead. It never panics.
func mustParseInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
