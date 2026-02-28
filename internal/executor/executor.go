// Package executor implements the core agentic loop for gpt-oss-executor.
// It calls the vLLM-served gpt-oss model, parses tool intents from the
// response, routes them through the OpenClaw gateway, and iterates until
// the model signals completion, an error occurs, or limits are reached.
package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jgavinray/gpt-oss-executor/internal/config"
	execerrors "github.com/jgavinray/gpt-oss-executor/internal/errors"
	"github.com/jgavinray/gpt-oss-executor/internal/logging"
	"github.com/jgavinray/gpt-oss-executor/internal/parser"
	"github.com/jgavinray/gpt-oss-executor/internal/tools"
)

// Message is an OpenAI-compatible chat message used throughout the agentic loop.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// RunResult holds the outcome of a completed agentic run.
type RunResult struct {
	RunID      string    `json:"run_id"`
	Answer     string    `json:"answer"`
	Iterations int       `json:"iterations"`
	Messages   []Message `json:"messages"`
}

// gptOSSRawResponse is the response shape returned by the vLLM
// OpenAI-compatible /v1/chat/completions endpoint. The optional
// ReasoningContent field is populated when vLLM is started with
// --enable-reasoning --reasoning-parser deepseek_r1.
type gptOSSRawResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning,omitempty"` // gpt-oss uses "reasoning" not "reasoning_content"
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// gptOSSRequest is the body sent to POST /v1/chat/completions on the vLLM
// endpoint. ExtraBody carries vLLM-specific extensions such as guided_json.
type gptOSSRequest struct {
	Model       string                 `json:"model"`
	Messages    []Message              `json:"messages"`
	MaxTokens   int                    `json:"max_tokens"`
	Temperature float32                `json:"temperature"`
	Stream      bool                   `json:"stream"`
	ExtraBody   map[string]interface{} `json:"extra_body,omitempty"`
}

// Executor orchestrates the agentic loop: it calls gpt-oss, parses tool
// intents, routes them through the OpenClaw gateway, injects results, and
// repeats until the model signals completion or a limit is hit.
type Executor struct {
	Config           *config.Config
	Parser           *parser.IntentParser
	ToolExecutor     *tools.ToolExecutor
	Logger           *slog.Logger
	ErrorLogger      *logging.ErrorLogger
	SystemPrompt     string
	GuidedJSONSchema map[string]interface{}
	httpClient       *http.Client
}

// New constructs an Executor wired to the provided Config. It loads the system
// prompt and optional guided-JSON schema from disk, initialises the intent
// parser, and builds the HTTP clients for the vLLM endpoint and OpenClaw
// gateway.
func New(cfg *config.Config, logger *slog.Logger, errLogger *logging.ErrorLogger) (*Executor, error) {
	sysPrompt, err := cfg.SystemPrompt()
	if err != nil {
		return nil, fmt.Errorf("executor: loading system prompt: %w", err)
	}

	guidedSchema, err := cfg.GuidedJSONSchema()
	if err != nil {
		return nil, fmt.Errorf("executor: loading guided JSON schema: %w", err)
	}

	p := parser.New(cfg.Parser.Strategy, cfg.Parser.FallbackStrategy)

	gatewayTimeout := time.Duration(cfg.Tools.DefaultTimeoutSeconds) * time.Second
	if gatewayTimeout <= 0 {
		gatewayTimeout = 30 * time.Second
	}

	gatewayClient := &tools.GatewayClient{
		BaseURL:    cfg.Executor.OpenClawGatewayURL,
		Token:      cfg.Executor.OpenClawGatewayToken,
		SessionKey: cfg.Executor.OpenClawSessionKey,
		Client:     &http.Client{Timeout: gatewayTimeout},
	}

	toolExec := &tools.ToolExecutor{
		Gateway:      gatewayClient,
		ResultLimits: cfg.Tools.ResultLimits,
		MaxRetries:   cfg.Executor.MaxRetries,
		Logger:       logger,
	}

	gptCallTimeout := time.Duration(cfg.Executor.GptOSSCallTimeoutSeconds) * time.Second
	if gptCallTimeout <= 0 {
		gptCallTimeout = 60 * time.Second
	}

	return &Executor{
		Config:           cfg,
		Parser:           p,
		ToolExecutor:     toolExec,
		Logger:           logger,
		ErrorLogger:      errLogger,
		SystemPrompt:     sysPrompt,
		GuidedJSONSchema: guidedSchema,
		httpClient:       &http.Client{Timeout: gptCallTimeout},
	}, nil
}

// Run executes the agentic loop for the given input messages. It enforces
// RunTimeoutSeconds as an overall deadline and MaxIterations as a cycle cap.
// Returns a RunResult on success, or an error when the loop cannot complete.
func (e *Executor) Run(ctx context.Context, inputMessages []Message) (*RunResult, error) {
	runID := generateRunID()
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(e.Config.Executor.RunTimeoutSeconds)*time.Second)
	defer cancel()

	e.Logger.Info("run started",
		slog.String("run_id", runID),
		slog.Int("max_iterations", e.Config.Executor.MaxIterations),
	)

	messages := e.buildInitialMessages(inputMessages)

	var (
		answer      string
		lastContent string // tracks last non-empty content from gpt-oss
		iterations  int
	)

	for iterations = 0; iterations < e.Config.Executor.MaxIterations; iterations++ {
		// Bail immediately if the overall deadline has passed.
		select {
		case <-runCtx.Done():
			return nil, execerrors.Wrap(execerrors.ErrRunTimeout, runCtx.Err())
		default:
		}

		var err error
		messages, err = e.manageContext(messages)
		if err != nil {
			return nil, fmt.Errorf("executor: managing context at iteration %d: %w", iterations+1, err)
		}

		e.Logger.Debug("calling gpt-oss",
			slog.String("run_id", runID),
			slog.Int("iteration", iterations+1),
			slog.Int("message_count", len(messages)),
		)

		resp, callErr := e.callGptOss(runCtx, messages)
		if callErr != nil {
			if isContextWindowExceeded(callErr) {
				return nil, execerrors.Wrap(execerrors.ErrContextWindow, callErr)
			}
			if execerrors.IsTransientError(callErr) {
				e.Logger.Warn("transient error from gpt-oss, retrying next iteration",
					slog.String("run_id", runID),
					slog.Int("iteration", iterations+1),
					slog.String("error", callErr.Error()),
				)
				continue
			}
			return nil, fmt.Errorf("executor: calling gpt-oss at iteration %d: %w", iterations+1, callErr)
		}

		if len(resp.Choices) == 0 {
			// gpt-oss non-deterministically returns 0 choices on certain prompts.
			// Treat as a transient error and retry the iteration instead of aborting.
			e.Logger.Warn("gpt-oss returned 0 choices, retrying iteration",
				slog.String("run_id", runID),
				slog.Int("iteration", iterations+1),
			)
			// Increment iteration counter will happen at the end of the loop.
			// Sleep briefly to avoid hammering the endpoint.
			select {
			case <-time.After(500 * time.Millisecond):
			case <-runCtx.Done():
				return nil, execerrors.Wrap(execerrors.ErrRunTimeout, runCtx.Err())
			}
			continue
		}

		choice := resp.Choices[0]
		content := choice.Message.Content
		reasoningContent := choice.Message.ReasoningContent

		e.Logger.Debug("gpt-oss response received",
			slog.String("run_id", runID),
			slog.Int("iteration", iterations+1),
			slog.Int("prompt_tokens", resp.Usage.PromptTokens),
			slog.Int("completion_tokens", resp.Usage.CompletionTokens),
			slog.Bool("has_reasoning", reasoningContent != ""),
		)

		// Track last non-empty content for use as fallback answer.
		if strings.TrimSpace(content) != "" {
			lastContent = content
		}

		// Append assistant message to conversation history.
		messages = append(messages, Message{Role: "assistant", Content: content})

		// Select which field to parse for tool intents.
		parseSource := e.selectParseSource(reasoningContent, content)
		if strings.TrimSpace(parseSource) == "" {
			if strings.TrimSpace(content) == "" {
				// Both reasoning and content are empty — gpt-oss produced nothing.
				// Retry this iteration (non-deterministic model behavior).
				e.Logger.Warn("gpt-oss returned empty reasoning and content, retrying",
					slog.String("run_id", runID),
					slog.Int("iteration", iterations+1),
				)
				select {
				case <-time.After(500 * time.Millisecond):
				case <-runCtx.Done():
					return nil, execerrors.Wrap(execerrors.ErrRunTimeout, runCtx.Err())
				}
				continue
			}
			// Non-empty content with no tool markers — final answer reached.
			answer = content
			e.Logger.Info("empty parse source, treating content as final answer",
				slog.String("run_id", runID),
				slog.Int("iteration", iterations+1),
			)
			break
		}

		// Temporary debug: log parse source
		parsePreview := parseSource
		if len(parsePreview) > 300 {
			parsePreview = parsePreview[:300]
		}
		e.Logger.Debug("parse source",
			slog.String("run_id", runID),
			slog.Int("iteration", iterations+1),
			slog.String("source_preview", parsePreview),
		)

		intents := e.Parser.Parse(parseSource)

		e.Logger.Debug("intents parsed",
			slog.String("run_id", runID),
			slog.Int("iteration", iterations+1),
			slog.Int("intent_count", len(intents)),
		)

		// No tool intents → model has produced its final answer.
		if len(intents) == 0 {
			answer = content
			e.Logger.Info("no tool intents found, final answer reached",
				slog.String("run_id", runID),
				slog.Int("iteration", iterations+1),
			)
			break
		}

		// Execute each tool intent sequentially and inject results.
		for _, intent := range intents {
			select {
			case <-runCtx.Done():
				return nil, execerrors.Wrap(execerrors.ErrRunTimeout, runCtx.Err())
			default:
			}

			toolResult, toolErr := e.ToolExecutor.Execute(runCtx, intent)
			if toolErr != nil {
				e.Logger.Warn("tool execution failed",
					slog.String("run_id", runID),
					slog.Int("iteration", iterations+1),
					slog.String("tool", intent.Name),
					slog.String("error", toolErr.Error()),
				)
				if e.ErrorLogger != nil {
					_ = e.ErrorLogger.Log(
						runID,
						strconv.Itoa(iterations+1),
						intent.Name,
						toolErr,
						"injecting error into context for model recovery",
					)
				}
				// Inject the error as a tool message so the model can adapt.
				messages = append(messages, Message{
					Role:    "tool",
					Content: fmt.Sprintf("Tool %q failed: %s", intent.Name, toolErr.Error()),
				})
				continue
			}

			messages = append(messages, Message{
				Role:    "tool",
				Content: fmt.Sprintf("Tool %q result:\n%s", intent.Name, toolResult),
			})

			e.Logger.Debug("tool result injected",
				slog.String("run_id", runID),
				slog.Int("iteration", iterations+1),
				slog.String("tool", intent.Name),
				slog.Int("result_len", len(toolResult)),
			)
		}
	}

	// Exhausted iteration budget without a clean break.
	// If gpt-oss produced content along the way, return it as the best answer.
	if answer == "" {
		if lastContent != "" {
			e.Logger.Warn("max iterations reached, returning last content as answer",
				slog.String("run_id", runID),
				slog.Int("iterations", iterations),
			)
			answer = lastContent
		} else {
			return nil, execerrors.ErrMaxIterations
		}
	}

	e.Logger.Info("run complete",
		slog.String("run_id", runID),
		slog.Int("iterations", iterations+1),
		slog.Int("answer_len", len(answer)),
	)

	return &RunResult{
		RunID:      runID,
		Answer:     answer,
		Iterations: iterations + 1,
		Messages:   messages,
	}, nil
}

// buildInitialMessages prepends the system prompt (if configured) to the
// caller-supplied messages.
//
// NOTE: gpt-oss (vLLM) returns 0 choices when a "system" role message is
// present — the model has a hardcoded OpenAI system prompt that conflicts.
// Workaround: inject the system prompt at the top of the first user message
// instead of using a dedicated system role entry.
func (e *Executor) buildInitialMessages(input []Message) []Message {
	if e.SystemPrompt == "" {
		return input
	}

	result := make([]Message, 0, len(input))

	injected := false
	for _, msg := range input {
		if !injected && msg.Role == "user" {
			result = append(result, Message{
				Role:    "user",
				Content: e.SystemPrompt + "\n\n" + msg.Content,
			})
			injected = true
		} else {
			result = append(result, msg)
		}
	}

	// No user message found — fall back to prepending a user message.
	if !injected {
		result = append([]Message{{Role: "user", Content: e.SystemPrompt}}, result...)
	}

	return result
}

// selectParseSource returns the text the parser should analyse. It prefers
// reasoningContent when the configured source field is "reasoning" and the
// value is non-empty; otherwise it falls back as configured.
func (e *Executor) selectParseSource(reasoningContent, content string) string {
	switch e.Config.Parser.SourceField {
	case "reasoning":
		if strings.TrimSpace(reasoningContent) != "" {
			return reasoningContent
		}
		// Primary source empty; try the configured fallback field.
		if e.Config.Parser.FallbackField == "content" {
			return content
		}
		return ""
	case "content":
		return content
	default:
		return content
	}
}

// callGptOss sends a chat completion request to the vLLM endpoint and
// returns the parsed response. It injects the guided_json schema into
// extra_body when the parser strategy is "guided_json".
func (e *Executor) callGptOss(ctx context.Context, messages []Message) (*gptOSSRawResponse, error) {
	reqBody := gptOSSRequest{
		Model:       e.Config.Executor.GptOSSModel,
		Messages:    messages,
		MaxTokens:   e.Config.Executor.GptOSSMaxTokens,
		Temperature: e.Config.Executor.GptOSSTemperature,
		Stream:      false,
	}

	if e.Config.Parser.Strategy == "guided_json" && e.GuidedJSONSchema != nil {
		reqBody.ExtraBody = map[string]interface{}{
			"guided_json": e.GuidedJSONSchema,
		}
	}

	encoded, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("executor: marshalling gpt-oss request: %w", err)
	}

	url := strings.TrimRight(e.Config.Executor.GptOSSURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("executor: building gpt-oss request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, execerrors.Wrap(execerrors.ErrGptOssUnreachable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("executor: reading gpt-oss response body: %w", err)
	}

	// vLLM returns HTTP 400 for context_length_exceeded — surface it distinctly.
	if resp.StatusCode == http.StatusBadRequest {
		bodyStr := string(body)
		if strings.Contains(bodyStr, "context_length_exceeded") ||
			strings.Contains(bodyStr, "maximum context length") {
			return nil, execerrors.Wrap(execerrors.ErrContextWindow,
				fmt.Errorf("vLLM HTTP 400: %s", strings.TrimSpace(bodyStr)))
		}
		return nil, fmt.Errorf("executor: gpt-oss returned HTTP 400: %s", strings.TrimSpace(bodyStr))
	}

	if resp.StatusCode != http.StatusOK {
		return nil, execerrors.Wrap(execerrors.ErrGptOssUnreachable,
			fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}

	var raw gptOSSRawResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("executor: unmarshalling gpt-oss response: %w", err)
	}
	return &raw, nil
}

// manageContext applies tiered context window management before each gpt-oss
// call. If the estimated token count exceeds ContextTruncThreshold, tool
// result messages are shortened. If it then still exceeds
// ContextCompactThreshold, the oldest non-system messages are dropped.
func (e *Executor) manageContext(messages []Message) ([]Message, error) {
	limit := e.Config.Executor.ContextWindowLimit
	compactAt := float64(limit) * e.Config.Executor.ContextCompactThreshold
	truncAt := float64(limit) * e.Config.Executor.ContextTruncThreshold

	estimated := e.estimateTokens(messages)
	if float64(estimated) < truncAt {
		return messages, nil
	}

	e.Logger.Warn("context near trunc threshold, shortening tool results",
		slog.Int("estimated_tokens", estimated),
		slog.Float64("trunc_threshold", truncAt),
	)

	messages = truncateToolResults(messages)
	estimated = e.estimateTokens(messages)

	if float64(estimated) < compactAt {
		return messages, nil
	}

	e.Logger.Warn("context above compact threshold, dropping oldest messages",
		slog.Int("estimated_tokens", estimated),
		slog.Float64("compact_threshold", compactAt),
	)

	messages = compactMessages(messages)
	return messages, nil
}

// estimateTokens uses a heuristic of 3.5 characters per token plus per-message
// overhead to estimate the total token count for a slice of messages. Exact
// counts are returned by vLLM in the Usage field and are used for logging, but
// the heuristic is sufficient for pre-call threshold checks.
func (e *Executor) estimateTokens(messages []Message) int {
	total := 0
	for _, m := range messages {
		total += len(m.Role) + len(m.Content)
	}
	// 3.5 chars ≈ 1 token; add 4 tokens per message for role/separator overhead.
	return int(float64(total)/3.5) + len(messages)*4
}

// truncateToolResults shortens tool-role messages that exceed 500 characters.
// This is the Tier 1 compaction strategy: preserve structure but cut bulk.
func truncateToolResults(messages []Message) []Message {
	const maxToolResult = 500
	result := make([]Message, len(messages))
	copy(result, messages)
	for i, m := range result {
		if m.Role == "tool" && len(m.Content) > maxToolResult {
			result[i].Content = m.Content[:maxToolResult] + "\n... [compacted]"
		}
	}
	return result
}

// compactMessages retains the system message (if present), the first user
// message, and the most recent half of the remaining messages. This is the
// Tier 2 compaction strategy for severe context pressure.
func compactMessages(messages []Message) []Message {
	if len(messages) <= 4 {
		return messages
	}

	start := 0
	var result []Message

	if messages[0].Role == "system" {
		result = append(result, messages[0])
		start = 1
	}

	rest := messages[start:]
	if len(rest) == 0 {
		return result
	}

	// Always keep the first user message.
	result = append(result, rest[0])

	// Keep the most recent half of subsequent messages.
	tail := rest[1:]
	keepFrom := len(tail) / 2
	result = append(result, tail[keepFrom:]...)

	return result
}

// isContextWindowExceeded reports whether err represents a context length
// overflow returned by vLLM (HTTP 400 with a context_length_exceeded body).
func isContextWindowExceeded(err error) bool {
	if err == nil {
		return false
	}
	if execerrors.IsContextWindowError(err) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "context_length_exceeded") ||
		strings.Contains(s, "maximum context length")
}

// generateRunID returns a 16-character lowercase hex string derived from 8
// random bytes. Errors from crypto/rand are silently ignored; an all-zero ID
// is still unique enough for within-process logging.
func generateRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
