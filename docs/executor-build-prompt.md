# GPT-OSS Executor for OpenClaw - Complete Build Specification

## Executive Summary

Build a Go-based OpenClaw executor that wraps gpt-oss (local 120B reasoning model) and executes tools using OpenClaw's native tooling. The executor exposes an OpenAI-compatible `/v1/chat/completions` endpoint, parses gpt-oss reasoning for tool intents, and routes execution through OpenClaw's native tools (web_search, web_fetch, read, write, exec, etc.).

**Key constraint:** No Anthropic models in the tool execution path. gpt-oss does reasoning; OpenClaw does execution.

---

## 1. Problem Statement

- gpt-oss (120B) doesn't natively support tool calling but excels at reasoning
- Users want to leverage local reasoning without calling external APIs for tool execution
- Current architecture: gpt-oss reasoning → parse → **OpenClaw native tools** (web_search, web_fetch, read, write, exec, browser, canvas, nodes, etc.)
- **Goal:** Build an executor that implements this loop, exposes it as an OpenClaw-compatible service, integrates into the gateway config, and can be spawned by subagents

---

## 2. Architecture Overview

### Data Flow

```
User Request
    ↓
Executor Receives /v1/chat/completions
    ↓
Call gpt-oss with user messages (temp: 0.2-0.3)
    ↓
Extract Tool Intents from reasoning field
    (Fuzzy matching: look for tool names like "search", "fetch", "read", "write")
    ↓
Execute OpenClaw Tools
    (Call web_search, web_fetch, read, write, exec directly)
    ↓
Inject Results Back into Messages
    ("[TOOL_RESULT: tool_name] result_text")
    ↓
Re-prompt gpt-oss with results (max 5 iterations)
    ↓
Return final response to caller
```

### Design Decisions

1. **Batch mode, not streaming** — Parse reasoning field completely before extracting tools
2. **Fuzzy intent matching, not strict markers** — gpt-oss won't produce `[TOOL:...]` markers reliably; use NLP-style pattern matching instead
3. **Sequential tool execution** — Execute tools one at a time (data dependencies, simpler error handling)
4. **Max 5 iterations** — Prevent runaway loops and token explosion
5. **Hard timeout: 300s** — Global timeout on the entire run
6. **Token accounting** — Log token usage per iteration (context window management)
7. **Comprehensive error logging** — All failures go to `logs/YYYY-MM-DD-errors.md` for debugging

---

## 3. Go Implementation Requirements

### 3.1 Project Structure

```
gpt-oss-executor/
├── cmd/
│   └── main.go                 # Entry point, HTTP server
├── internal/
│   ├── executor/
│   │   └── executor.go         # Core loop (prompt → parse → execute)
│   ├── parser/
│   │   └── intent_parser.go    # Fuzzy tool intent extraction
│   ├── tools/
│   │   └── tool_executor.go    # Route to OpenClaw tools
│   └── logging/
│       └── logger.go           # Structured logging (JSON)
├── Makefile
├── README.md
├── go.mod / go.sum
└── config/
    └── executor.yaml           # Configuration
```

### 3.2 Core Interfaces

```go
// Executor.go
type Executor struct {
    GptOSSURL      string
    OpenClawGateway string
    MaxIterations  int
    RunTimeout     time.Duration
    Logger         *slog.Logger
}

type RunState struct {
    RunID       string
    UserPrompt  string
    Messages    []Message
    Iteration   int
    ToolIntents []ToolIntent
    TokensUsed  int
    StartTime   time.Time
}

type ToolIntent struct {
    Name string            // "web_search", "web_fetch", "read", "write", etc.
    Args map[string]string // {"query": "...", "url": "...", "path": "...", etc.}
}

type Message struct {
    Role    string // "system", "user", "assistant"
    Content string
}

// Call gpt-oss and execute the loop
func (e *Executor) Run(ctx context.Context, userPrompt string) (string, error)
```

### 3.3 Intent Parser (Fuzzy Matching)

The parser looks for tool names and arguments in gpt-oss reasoning:

```go
// Example reasoning from gpt-oss:
// "We would search for Claude AI using web_search with query 'Claude AI 2026', 
//  then fetch the top result using web_fetch, 
//  then summarize and write to a file using write_file"

type IntentParser struct{}

func (p *IntentParser) Parse(reasoning string) []ToolIntent {
    intents := []ToolIntent{}
    
    // Fuzzy patterns:
    // - "search for X" / "web_search" → {name: "web_search", args: {query: "X"}}
    // - "fetch from Y" / "web_fetch" / "get the page" → {name: "web_fetch", args: {url: "Y"}}
    // - "read file X" / "read_file" → {name: "read", args: {path: "X"}}
    // - "write to X" / "save as X" / "write_file" → {name: "write", args: {path: "X", content: "..."}}
    // - "execute command" / "run" / "exec" → {name: "exec", args: {command: "..."}}
    
    // Implementation: regex + NLP-style extraction
    // Don't be too strict; favor false positives over false negatives
    
    return intents
}
```

Key tool mappings:

| Tool Intent | OpenClaw Tool | Required Args | Optional Args |
|-------------|---------------|---------------|---------------|
| web_search | web_search | query | count, country, freshness |
| web_fetch | web_fetch | url | extractMode, maxChars |
| read | read | file_path | offset, limit |
| write | write | file_path, content | (none) |
| exec | exec | command | workdir, env, timeout |
| browser | browser | action (snapshot, screenshot, navigate, etc.) | profile, target, etc. |

### 3.4 Tool Executor (Route to OpenClaw)

```go
type ToolExecutor struct {
    GatewayURL   string
    GatewayToken string
    Logger       *slog.Logger
}

func (te *ToolExecutor) Execute(ctx context.Context, intent ToolIntent) (string, error) {
    // Call OpenClaw tools via HTTP or CLI
    // Options:
    // 1. Call OpenClaw gateway HTTP endpoints directly
    // 2. Call OpenClaw CLI (e.g., `openclaw exec ...`)
    // 3. Call via Go library if available
    
    // For now: Use gateway HTTP endpoints
    // Example: POST /api/tools/web_search with {query: "..."}
    
    switch intent.Name {
    case "web_search":
        return te.WebSearch(ctx, intent.Args)
    case "web_fetch":
        return te.WebFetch(ctx, intent.Args)
    case "read":
        return te.Read(ctx, intent.Args)
    case "write":
        return te.Write(ctx, intent.Args)
    case "exec":
        return te.Exec(ctx, intent.Args)
    // ... more tools
    default:
        return "", fmt.Errorf("unknown tool: %s", intent.Name)
    }
}

func (te *ToolExecutor) WebSearch(ctx context.Context, args map[string]string) (string, error) {
    // POST to gateway: /api/tools/web_search?query=...&count=...
    // Return results as JSON or formatted text
}
```

### 3.5 Main Loop (Executor.Run)

```go
func (e *Executor) Run(ctx context.Context, userPrompt string) (string, error) {
    state := &RunState{
        RunID:      uuid.New().String(),
        UserPrompt: userPrompt,
        Messages:   []Message{{Role: "user", Content: userPrompt}},
        StartTime:  time.Now(),
    }
    
    contextWithTimeout, cancel := context.WithTimeout(ctx, e.RunTimeout)
    defer cancel()
    
    for state.Iteration = 0; state.Iteration < e.MaxIterations; state.Iteration++ {
        // Check timeout
        if time.Since(state.StartTime) > e.RunTimeout {
            return "", ErrRunTimeout
        }
        
        // 1. Call gpt-oss
        gptResponse, err := e.callGptOss(contextWithTimeout, state.Messages)
        if err != nil {
            e.Logger.Error("gpt-oss call failed", "error", err, "iteration", state.Iteration)
            return "", err
        }
        
        // Add assistant response to messages
        state.Messages = append(state.Messages, Message{
            Role:    "assistant",
            Content: gptResponse.Content,
        })
        
        // Update token count
        state.TokensUsed += gptResponse.Tokens
        
        // 2. Parse tool intents from reasoning
        intents := e.parser.Parse(gptResponse.Reasoning)
        
        if len(intents) == 0 {
            // No tools requested, assume done
            e.Logger.Info("no tools requested", "iteration", state.Iteration)
            return gptResponse.Content, nil
        }
        
        // 3. Execute tools
        var toolResults []string
        for _, intent := range intents {
            result, err := e.toolExecutor.Execute(contextWithTimeout, intent)
            if err != nil {
                e.Logger.Warn("tool execution failed", 
                    "tool", intent.Name, 
                    "error", err, 
                    "iteration", state.Iteration)
                // Don't abort; inject error as context
                toolResults = append(toolResults, fmt.Sprintf("[ERROR: %s failed: %v]", intent.Name, err))
            } else {
                toolResults = append(toolResults, fmt.Sprintf("[TOOL_RESULT: %s] %s", intent.Name, result))
            }
        }
        
        // 4. Inject results back into messages
        resultText := strings.Join(toolResults, "\n")
        state.Messages = append(state.Messages, Message{
            Role:    "user",
            Content: fmt.Sprintf("Tool results:\n%s\n\nContinue with next step or provide final answer.", resultText),
        })
        
        // 5. Check for completion signal (would be in gptResponse reasoning)
        if strings.Contains(gptResponse.Reasoning, "[DONE]") || 
           strings.Contains(gptResponse.Content, "complete") {
            e.Logger.Info("run complete", "iteration", state.Iteration)
            return gptResponse.Content, nil
        }
    }
    
    return "", ErrMaxIterationsExceeded
}

func (e *Executor) callGptOss(ctx context.Context, messages []Message) (*GptOssResponse, error) {
    payload := map[string]interface{}{
        "model":       "gpt-oss",
        "messages":    messages,
        "temperature": 0.25,
        "max_tokens":  1000,
    }
    
    req, _ := http.NewRequestWithContext(ctx, "POST", e.GptOSSURL + "/v1/chat/completions", ...)
    // ... make request, parse response, return reasoning + content + tokens
}

type GptOssResponse struct {
    Content   string
    Reasoning string
    Tokens    int
}
```

---

## 4. Error Handling & Recovery

### 3-Tier Strategy

```
Tier 1: Retry (transient errors)
  ├─ gpt-oss timeout → retry with backoff (max 3 attempts)
  ├─ Tool HTTP error (5xx) → retry once
  └─ Network error → retry once

Tier 2: Skip and continue (non-fatal)
  ├─ Tool returns 404 → inject error, let model adapt
  ├─ Tool timeout → inject error, continue
  └─ Malformed tool intent → log and skip

Tier 3: Abort (fatal)
  ├─ gpt-oss unreachable after retries
  ├─ Max iterations exceeded
  ├─ Global timeout exceeded
  └─ Context window overflow (tokens > limit)
```

### Implementation

```go
func (e *Executor) executeWithRetry(ctx context.Context, intent ToolIntent) (string, error) {
    var lastErr error
    
    for attempt := 0; attempt < 3; attempt++ {
        result, err := e.toolExecutor.Execute(ctx, intent)
        if err == nil {
            return result, nil
        }
        
        lastErr = err
        
        // Decide if we should retry
        if isTransientError(err) {
            backoff := time.Duration(math.Pow(2, float64(attempt))) * 100 * time.Millisecond
            select {
            case <-time.After(backoff):
                continue
            case <-ctx.Done():
                return "", ctx.Err()
            }
        } else {
            // Non-transient, don't retry
            break
        }
    }
    
    return "", lastErr
}

func isTransientError(err error) bool {
    // Check error type
    // HTTP 5xx, timeouts, network errors → true
    // HTTP 4xx, not found → false
    return true // Simplified
}
```

---

## 5. Token Accounting & Context Management

### Tracking

```go
type TokenAccounting struct {
    PerIteration map[int]int // iteration -> token count
    Total        int
    ContextLimit int // e.g., 32768 for Spark
    StartedAt    time.Time
}

func (ta *TokenAccounting) AddIteration(iteration int, tokens int) {
    ta.PerIteration[iteration] = tokens
    ta.Total += tokens
    
    // Log
    slog.Info("iteration_tokens", "iteration", iteration, "tokens", tokens, "total", ta.Total)
    
    // Check if approaching limit (leave 2000 token buffer)
    if ta.Total + 2000 > ta.ContextLimit {
        slog.Warn("context_window_approaching_limit", "used", ta.Total, "limit", ta.ContextLimit)
        // Consider summarizing old messages or returning early
    }
}
```

### Logging

All logging must be structured (JSON) and persisted:

```go
// Stdout: JSON logging for CloudWatch / ELK
slog.Info("executor_start", 
    "run_id", state.RunID,
    "user_prompt", userPrompt[:100],
    "timestamp", time.Now(),
)

slog.Info("tool_execution",
    "run_id", state.RunID,
    "iteration", state.Iteration,
    "tool", intent.Name,
    "duration_ms", elapsed.Milliseconds(),
    "status", "success", // or "failed"
    "tokens_used", gptResponse.Tokens,
)

// Also: Append errors to logs/YYYY-MM-DD-errors.md
if err != nil {
    appendToErrorLog(fmt.Sprintf(
        "[%s] Iteration %d | Tool: %s | Error: %v | Attempted fix: %s\n",
        time.Now().Format("15:04:05"), state.Iteration, intent.Name, err, "retry"))
}
```

---

## 6. Deployment & Integration

### 6.1 Build & Run

```makefile
# Makefile
.PHONY: build run test clean

build:
	@mkdir -p bin
	@go build -o bin/gpt-oss-executor cmd/main.go

run: build
	@bin/gpt-oss-executor --config config/executor.yaml

test:
	@go test ./... -v

clean:
	@rm -rf bin/ dist/
```

### 6.2 OpenClaw Gateway Integration

The executor exposes `/v1/chat/completions` (OpenAI-compatible). Register it in `~/.openclaw/openclaw.json`:

```json
{
  "models": {
    "providers": {
      "gpt-oss-executor": {
        "baseUrl": "http://localhost:8001/v1",
        "apiKey": "local",
        "api": "openai-completions",
        "models": [
          {
            "id": "executor",
            "name": "GPT-OSS Executor (Reasoning + OpenClaw Tools)",
            "reasoning": true,
            "input": ["text"],
            "cost": { "input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0 },
            "contextWindow": 32768,
            "maxTokens": 16384
          }
        ]
      }
    }
  },
  "agents": {
    "list": [
      {
        "id": "gpt-oss-executor",
        "model": "gpt-oss-executor/executor",
        "workspace": "/Users/jgavinray/Obsidian/personal/zoidberg"
      }
    ]
  }
}
```

Then use it:

```bash
# Spawn a subagent with gpt-oss executor
openclaw session spawn --task "Research X" --agent gpt-oss-executor

# Or in Go (sessions_spawn):
sessions_spawn(task="...", agentId="gpt-oss-executor", model="gpt-oss-executor/executor")
```

### 6.3 Configuration File (executor.yaml)

```yaml
executor:
  gpt_oss_url: "http://spark:8000"
  openclaw_gateway_url: "http://localhost:18789"
  openclaw_gateway_token: "REDACTED"
  max_iterations: 5
  run_timeout_seconds: 300
  gpt_oss_temperature: 0.25
  gpt_oss_max_tokens: 1000

http_server:
  port: 8001
  bind: "127.0.0.1"

logging:
  level: "info"  # debug, info, warn, error
  format: "json"
  output: "stdout"
  error_log_path: "/Users/jgavinray/Obsidian/personal/zoidberg/logs"
```

---

## 7. Implementation Phases

### Phase 1: Working PoC (1-2 days)
- [ ] Basic HTTP server + `/v1/chat/completions` endpoint
- [ ] Call gpt-oss, extract reasoning field
- [ ] Implement fuzzy parser (detect "search", "fetch", "read", "write")
- [ ] Execute 3 tools: web_search, web_fetch, read
- [ ] Simple loop (max 3 iterations)
- [ ] Batch mode only (no streaming)
- [ ] Structured JSON logging
- [ ] Hard timeout: 300s

### Phase 2: Robustness (2-3 days)
- [ ] All 8+ OpenClaw tools supported
- [ ] Error recovery tiers (retry, skip, abort)
- [ ] Token accounting + context window management
- [ ] Tool result summarization (truncate long outputs)
- [ ] Checkpoint/resume for interrupted runs
- [ ] Fuzzy parser improvements (handle variations in tool names)

### Phase 3: Integration (3-5 days)
- [ ] OpenClaw gateway config + executor registration
- [ ] Subagent spawning support
- [ ] Streaming mode (if needed for latency)
- [ ] Multi-model support (swap gpt-oss for other models)
- [ ] Cost comparison dashboard
- [ ] Production error alerting

---

## 8. Key Constraints & Gotchas

| Constraint | Why | Mitigation |
|-----------|-----|-----------|
| gpt-oss won't produce structured markers | Model training | Use fuzzy NLP-style intent extraction |
| Reasoning field can be empty/null | Model behavior | Gracefully handle null, don't crash |
| Token limits (context window) | Spark 32K context | Track tokens, summarize results, abort early if approaching limit |
| gpt-oss slow (60s+ per call) | Large model on CPU | Implement timeouts, allow retries |
| Tool execution failures | Network, tool errors | 3-tier recovery (retry → skip → abort) |
| Runaway loops | Infinite reasoning | Max 5 iterations hard limit |
| Intent parsing ambiguity | Natural language is ambiguous | Prefer false positives over false negatives; let model correct course |

---

## 9. Testing Strategy

```go
// Unit tests
func TestIntentParser(t *testing.T) {
    parser := &IntentParser{}
    
    reasoning := "We would search for Claude AI 2026 using web_search, then fetch the article"
    intents := parser.Parse(reasoning)
    
    // Assert: should find web_search and web_fetch
    if len(intents) != 2 { t.Fail() }
}

// Integration tests
func TestExecutorEndToEnd(t *testing.T) {
    // Mock gpt-oss response
    // Mock OpenClaw tools
    // Run executor loop
    // Verify results
}

// Load tests
func TestMaxTokens(t *testing.T) {
    // Ensure context window not exceeded
}
```

---

## 10. Success Criteria

- [ ] Executor receives user request, calls gpt-oss, parses reasoning
- [ ] Detects tool intents (web_search, web_fetch, read, write, etc.)
- [ ] Executes tools via OpenClaw API
- [ ] Loops: feeds results back, re-prompts, gets next tools
- [ ] Completes within 300s timeout
- [ ] Logs all iterations with token counts
- [ ] Handles errors gracefully (retry/skip/abort)
- [ ] Integrates into OpenClaw gateway config
- [ ] Can be spawned as a subagent


---

**Status:** Ready for implementation. Start with Phase 1. This specification is complete and self-contained.
