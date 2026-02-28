# GPT-OSS Executor for OpenClaw - Complete Production Specification

## Table of Contents
1. [Executive Summary](#executive-summary)
2. [Problem Statement](#problem-statement)
3. [Architecture & Design](#architecture--design)
4. [Complete API Specification](#complete-api-specification)
5. [Go Implementation Details](#go-implementation-details)
6. [Tool Integration Protocol](#tool-integration-protocol)
7. [Error Handling & Recovery](#error-handling--recovery)
8. [Token Management & Context Windows](#token-management--context-windows)
9. [Logging & Observability](#logging--observability)
10. [Configuration & Deployment](#configuration--deployment)
11. [Testing Strategy](#testing-strategy)
12. [Security Considerations](#security-considerations)
13. [Performance Tuning](#performance-tuning)
14. [Troubleshooting Guide](#troubleshooting-guide)
15. [Implementation Roadmap](#implementation-roadmap)

---

## Executive Summary

**What:** Build a production-grade Go executor that wraps gpt-oss (local 120B reasoning model) and routes tool execution through OpenClaw's native tooling.

**Why:** gpt-oss excels at reasoning but lacks native tool-calling support. This executor bridges that gap by parsing gpt-oss's reasoning for tool intents, executing those tools via OpenClaw's native APIs (web_search, web_fetch, read, write, exec, browser, canvas, nodes), and looping until completion.

**Key Constraint:** Zero Anthropic model tokens in the tool execution path. All reasoning is local (gpt-oss). All execution is via OpenClaw tools.

**Architecture Pattern:**
```
User Request → Executor HTTP Server → gpt-oss (reasoning) → Intent Parser (fuzzy) → 
OpenClaw Tools (execution) → Results Injection → Loop
```

**Success Metrics:**
- Supports 8+ OpenClaw tools natively
- Completes within 300s timeout
- Handles errors gracefully (3-tier recovery)
- Tracks token usage per iteration
- Logs structured JSON for debugging
- Integrates into OpenClaw gateway config
- Can be spawned as a subagent

---

## Problem Statement

### The Challenge

1. **gpt-oss Model Limitations**
    - 120B reasoning model running locally (free)
    - Excels at multi-step reasoning and complex analysis
    - Does NOT support tool-calling out of the box
    - Hardcoded OpenAI system prompt (can't be overridden easily)
    - Won't emit structured tool markers like `[TOOL:name|arg=val]`
    - But its reasoning field captures exactly what tools it would use

2. **Current Ecosystem Gap**
    - Frontier models (Claude, GPT-4) have native tool calling but cost $$
    - Local models have free inference but no tool support
    - OpenClaw has excellent tool ecosystem (web_search, browser, exec, etc.) but no executor for gpt-oss

3. **User Expectation**
    - Run reasoning locally (free)
    - Execute tools via OpenClaw (free OpenClaw tools)
    - Zero external API calls needed
    - Full agentic loop: think → act → observe → think

### Solution Approach

Parse gpt-oss reasoning for natural language tool intents ("we would search for X", "fetch the URL", "save results"). Use fuzzy NLP-style matching (not strict markers). Route to OpenClaw tools. Loop.

This executor makes it possible.

---

## Architecture & Design

### 2.1 System Diagram

```
┌─────────────┐
│ User Request│
└──────┬──────┘
       │
       v
┌──────────────────────────────────────────────────────┐
│         GPT-OSS Executor (HTTP Server :8001)         │
│  ┌────────────────────────────────────────────────┐  │
│  │  POST /v1/chat/completions                     │  │
│  │  ├─ Validate input (messages, model, etc)      │  │
│  │  ├─ Track run state (iteration, tokens)        │  │
│  │  └─ Return OpenAI-compatible response          │  │
│  └────────────────────────────────────────────────┘  │
│           │                                          │
│           v                                          │
│  ┌────────────────────────────────────────────────┐  │
│  │  Core Loop (Executor.Run)                      │  │
│  │  ├─ Call gpt-oss API                           │  │
│  │  ├─ Extract reasoning field                    │  │
│  │  ├─ Parse tool intents (fuzzy NLP)             │  │
│  │  ├─ Execute tools (sequential)                 │  │
│  │  ├─ Inject results back                        │  │
│  │  └─ Loop until complete or max iterations      │  │
│  └────────────────────────────────────────────────┘  │
│           │                                          │
│           ├─────────────────┬───────────────────┐    │
│           v                 v                   v    │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────┐   │
│  │ Intent Parser│  │ Tool Executor│  │  Logger  │   │
│  │ (NLP fuzzy)  │  │  (OpenClaw)  │  │ (slog)   │   │
│  └──────────────┘  └──────────────┘  └──────────┘   │
└──────────────────────────────────────────────────────┘
       │
       ├─────────────────┬──────────────────┬─────────────────┐
       v                 v                  v                 v
   ┌────────┐      ┌──────────────┐   ┌──────────┐    ┌──────────┐
   │gpt-oss │      │ OpenClaw     │   │Logs/     │    │OpenClaw  │
   │:8000   │      │Gateway       │   │Metrics   │    │Config    │
   │(reason)│      │(tools)       │   │          │    │(register)│
   └────────┘      └──────────────┘   └──────────┘    └──────────┘
```

### 2.2 Data Flow with Example

```
User: "Find latest Claude AI news and save a summary"
         │
         v
Executor receives request, creates RunState
         │
         v
ITERATION 1:
  ├─ Call gpt-oss with: "Find latest Claude AI news and save a summary"
  ├─ gpt-oss response:
  │   Content: "I'll help with that..."
  │   Reasoning: "We would search for Claude AI latest news using web_search,
  │              then fetch the top result using web_fetch to get full article,
  │              then save summary using write_file..."
  │   Tokens: 145
  ├─ Parse reasoning → finds intents:
  │   ├─ Intent{Name: "web_search", Args: {query: "Claude AI news 2026"}}
  │   ├─ Intent{Name: "web_fetch", Args: {url: "<will fill from results>"}}
  │   └─ Intent{Name: "write", Args: {path: "<output file>"}}
  ├─ Execute web_search("Claude AI news 2026")
  │   → Returns: "1. https://...article1... | 2. https://...article2..."
  ├─ Extract URL from results, execute web_fetch(url)
  │   → Returns: "Article text..."
  ├─ Execute write("summary.md", "Article summary...")
  │   → Returns: "Wrote 2.5KB to file"
  └─ Inject results back into messages:
     "Tool results:
      [TOOL_RESULT: web_search] 1. https://...article1...
      [TOOL_RESULT: web_fetch] Article text...
      [TOOL_RESULT: write] Wrote 2.5KB
      
      Continue with next step or provide final answer."
         │
         v
ITERATION 2:
  ├─ Call gpt-oss with full message history + tool results
  ├─ gpt-oss reasoning: "All tasks complete. Search done, article fetched, summary saved."
  ├─ Parse reasoning → no tool intents found
  └─ Return final response to user
         │
         v
User receives: "I've found the latest Claude AI news, fetched the full article,
and saved a summary to summary.md"
```

### 2.3 Design Decisions (Rationale)

| Decision | Rationale |
|----------|-----------|
| **Fuzzy intent parsing** | gpt-oss won't emit `[TOOL:...]` markers reliably. NLP-style parsing ("search for", "fetch", "read", "write") is more robust. |
| **Sequential tool execution** | Tools often have data dependencies (search→fetch→write). Sequential execution is simpler and safer. |
| **Max 5 iterations** | Prevent runaway loops. With max_tokens=1000, 5 iterations ≈ 5000 tokens, well within context window. |
| **300s hard timeout** | gpt-oss can be slow (60s+ per call). 300s = 5 calls max with margins. Prevents hung processes. |
| **Batch mode (not streaming)** | Reasoning field must be complete to parse reliably. Streaming complicates parsing logic. |
| **3-tier error recovery** | Transient errors (network, timeout) should retry. Non-fatal errors (404) should inject and continue. Fatal errors (no retries, max iterations) should abort. |
| **Token accounting** | Context window is finite (32K for Spark). Must track to avoid overflow. Leave 2K buffer. |
| **Structured JSON logging** | Enables CloudWatch, ELK, Datadog ingestion. Essential for production observability. |

---

## Complete API Specification

### 3.1 HTTP Interface

The executor exposes a standard OpenAI-compatible `/v1/chat/completions` endpoint.

#### Request

```http
POST /v1/chat/completions
Content-Type: application/json

{
  "model": "gpt-oss",
  "messages": [
    {
      "role": "system",
      "content": "You are a helpful assistant that can use tools to complete tasks."
    },
    {
      "role": "user",
      "content": "Find the latest news about Claude AI and summarize it."
    }
  ],
  "temperature": 0.25,
  "max_tokens": 1000,
  "timeout": 300
}
```

**Parameters:**

| Parameter | Type | Required | Default | Notes |
|-----------|------|----------|---------|-------|
| `model` | string | Yes | - | Must be "gpt-oss" |
| `messages` | array | Yes | - | Standard OpenAI format |
| `temperature` | float | No | 0.25 | Recommended: 0.2-0.3 for reasoning |
| `max_tokens` | int | No | 1000 | Per-call limit (not total) |
| `top_p` | float | No | 0.95 | Nucleus sampling |
| `timeout` | int | No | 300 | Timeout in seconds for entire run |

#### Response (Success)

```json
{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "created": 1709067200,
  "model": "gpt-oss",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "I found the latest news about Claude AI and saved it to a file.",
        "reasoning": "We would search for Claude AI latest news using web_search with query 'Claude AI news 2026', then fetch the top result using web_fetch, then save a summary using write_file to /tmp/claude_summary.md."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 145,
    "completion_tokens": 287,
    "total_tokens": 432
  },
  "executor_metadata": {
    "run_id": "run-xyz789",
    "iterations": 2,
    "tools_called": ["web_search", "web_fetch", "write"],
    "total_tool_execution_time_ms": 5432,
    "context_usage_percent": 1.3
  }
}
```

**Response Fields:**

| Field | Type | Notes |
|-------|------|-------|
| `id` | string | Unique request ID |
| `object` | string | Always "chat.completion" |
| `model` | string | Always "gpt-oss" |
| `choices[0].message.content` | string | Final response text |
| `choices[0].message.reasoning` | string | Full reasoning from last iteration (useful for debugging) |
| `choices[0].finish_reason` | string | "stop" (completed), "length" (hit token limit), "error" (aborted) |
| `usage.prompt_tokens` | int | Tokens consumed in final iteration |
| `usage.completion_tokens` | int | Tokens generated in final iteration |
| `usage.total_tokens` | int | Sum across all iterations |
| `executor_metadata` | object | Custom fields (run_id, iterations, tools_called, etc.) |

#### Response (Error)

```json
{
  "error": {
    "message": "max iterations exceeded after 5 attempts",
    "type": "ExecutionError",
    "code": "max_iterations_exceeded"
  }
}
```

**Error Codes:**

| Code | HTTP | Meaning | Recovery |
|------|------|---------|----------|
| `invalid_request` | 400 | Bad request (missing model, invalid messages) | Fix request, retry |
| `model_not_found` | 404 | Model != "gpt-oss" | Use correct model name |
| `gpt_oss_unreachable` | 503 | gpt-oss service down | Wait, retry |
| `max_iterations_exceeded` | 400 | Hit max iterations (5) without completion | Simplify task or increase max_iterations |
| `timeout_exceeded` | 408 | Hit global timeout (300s) | Increase timeout or simplify task |
| `context_window_exceeded` | 400 | Token usage would exceed limit | Reduce prompt size or output length |
| `tool_execution_failed` | 500 | Tool call failed (web_search down, file I/O error) | Check tool availability, retry |

### 3.2 Gateway Integration

Register in `~/.openclaw/openclaw.json`:

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
            "cost": { "input": 0, "output": 0 },
            "contextWindow": 32768,
            "maxTokens": 16384
          }
        ]
      }
    }
  }
}
```

### 3.3 OpenClaw Subagent Interface

Use the executor as a subagent:

```go
// In OpenClaw agent code
sessions_spawn(
    task="Research Claude AI and summarize latest developments",
    agentId="gpt-oss-executor",
    model="gpt-oss-executor/executor",
    runTimeoutSeconds=300,
    label="gpt-oss-research-task"
)
```

---

## Go Implementation Details

### 4.1 Complete Project Structure

```
gpt-oss-executor/
├── cmd/
│   ├── main.go                    # Entry point, HTTP server setup
│   └── config.go                  # Config loading
├── internal/
│   ├── executor/
│   │   ├── executor.go            # Core Executor type and Run() method
│   │   ├── run_state.go           # RunState and message handling
│   │   └── call_gpt_oss.go        # gpt-oss API interaction
│   ├── parser/
│   │   ├── intent_parser.go       # Fuzzy tool intent extraction
│   │   ├── patterns.go            # Regex patterns for tool detection
│   │   └── argument_extraction.go # Extract args from reasoning text
│   ├── tools/
│   │   ├── tool_executor.go       # Route to OpenClaw tools
│   │   ├── handlers.go            # Individual tool handlers
│   │   ├── web_search.go          # web_search implementation
│   │   ├── web_fetch.go           # web_fetch implementation
│   │   ├── read.go                # read implementation
│   │   ├── write.go               # write implementation
│   │   ├── exec.go                # exec implementation
│   │   └── browser.go             # browser implementation
│   ├── logging/
│   │   ├── logger.go              # slog setup and configuration
│   │   ├── error_logger.go        # Error log persistence
│   │   └── metrics.go             # Metrics (token usage, latency)
│   ├── http/
│   │   ├── server.go              # HTTP server setup
│   │   ├── handlers.go            # Request handlers
│   │   └── middleware.go          # Logging, error handling
│   └── errors/
│       └── errors.go              # Custom error types
├── Makefile
├── go.mod
├── go.sum
├── README.md
├── DEVELOPMENT.md
├── config/
│   ├── executor.yaml.example
│   └── default.yaml
├── tests/
│   ├── executor_test.go
│   ├── parser_test.go
│   ├── tool_executor_test.go
│   └── integration_test.go
└── scripts/
    ├── build.sh
    ├── run.sh
    └── test.sh
```

### 4.2 Core Type Definitions

```go
package executor

import (
    "context"
    "log/slog"
    "time"
)

// Executor is the main orchestrator
type Executor struct {
    // Configuration
    GptOSSURL          string
    GptOSSModel        string // Usually "gpt-oss"
    OpenClawGatewayURL string
    OpenClawGatewayToken string
    
    // Execution parameters
    MaxIterations       int
    RunTimeout          time.Duration
    GptOSSTemperature   float32
    GptOSSMaxTokens     int
    ContextWindowLimit  int
    ContextBufferTokens int // Usually 2000
    
    // Components
    Logger        *slog.Logger
    Parser        *parser.IntentParser
    ToolExecutor  *tools.ToolExecutor
    ErrorLogger   *logging.ErrorLogger
    
    // Metrics (optional)
    Metrics       *logging.Metrics
}

// RunState tracks the state of a single executor run
type RunState struct {
    RunID               string
    UserPrompt          string
    Messages            []Message
    Iteration           int
    ToolIntents         []parser.ToolIntent
    ToolResults         map[string]string // tool name → result
    TokensPerIteration  map[int]int       // iteration → token count
    TotalTokens         int
    StartTime           time.Time
    LastError           error
}

// Message matches OpenAI chat message format
type Message struct {
    Role    string `json:"role"`    // "system", "user", "assistant"
    Content string `json:"content"`
}

// GptOSSResponse is the parsed response from gpt-oss API
type GptOSSResponse struct {
    ID      string `json:"id"`
    Content string `json:"choices[0].message.content"`
    Reasoning string `json:"choices[0].message.reasoning"`
    Tokens  int    `json:"usage.total_tokens"`
    FinishReason string `json:"choices[0].finish_reason"`
}

// ToolIntent is a parsed intent to use a tool
type ToolIntent struct {
    Name      string            // "web_search", "web_fetch", etc.
    Args      map[string]string // Tool arguments
    Confidence float32           // 0.0-1.0, for ranking multiple intents
    Source    string            // Where it came from in the reasoning
}

// ExecutorRequest is the HTTP request body
type ExecutorRequest struct {
    Model       string    `json:"model"`
    Messages    []Message `json:"messages"`
    Temperature *float32  `json:"temperature,omitempty"`
    MaxTokens   *int      `json:"max_tokens,omitempty"`
    Timeout     *int      `json:"timeout,omitempty"`
}

// ExecutorResponse is the HTTP response body
type ExecutorResponse struct {
    ID      string `json:"id"`
    Object  string `json:"object"`
    Created int64  `json:"created"`
    Model   string `json:"model"`
    Choices []struct {
        Index        int `json:"index"`
        Message      struct {
            Role      string `json:"role"`
            Content   string `json:"content"`
            Reasoning string `json:"reasoning,omitempty"`
        } `json:"message"`
        FinishReason string `json:"finish_reason"`
    } `json:"choices"`
    Usage struct {
        PromptTokens     int `json:"prompt_tokens"`
        CompletionTokens int `json:"completion_tokens"`
        TotalTokens      int `json:"total_tokens"`
    } `json:"usage"`
    ExecutorMetadata struct {
        RunID                     string   `json:"run_id"`
        Iterations                int      `json:"iterations"`
        ToolsCalled               []string `json:"tools_called"`
        TotalToolExecutionTimeMs  int      `json:"total_tool_execution_time_ms"`
        ContextUsagePercent       float32  `json:"context_usage_percent"`
    } `json:"executor_metadata"`
}
```

### 4.3 Core Executor Implementation

```go
package executor

import (
    "context"
    "fmt"
    "log/slog"
    "strings"
    "time"
)

// Run executes the agentic loop
func (e *Executor) Run(ctx context.Context, userPrompt string) (string, error) {
    // Initialize run state
    state := &RunState{
        RunID:              generateRunID(),
        UserPrompt:         userPrompt,
        Messages:           []Message{{Role: "user", Content: userPrompt}},
        StartTime:          time.Now(),
        ToolResults:        make(map[string]string),
        TokensPerIteration: make(map[int]int),
    }
    
    // Create context with timeout
    runCtx, cancel := context.WithTimeout(ctx, e.RunTimeout)
    defer cancel()
    
    // Log run start
    e.Logger.Info("executor_run_start",
        "run_id", state.RunID,
        "user_prompt_length", len(userPrompt),
        "max_iterations", e.MaxIterations,
    )
    
    // Main loop
    for state.Iteration = 0; state.Iteration < e.MaxIterations; state.Iteration++ {
        // Check context timeout
        if err := runCtx.Err(); err != nil {
            e.Logger.Error("executor_timeout",
                "run_id", state.RunID,
                "iteration", state.Iteration,
                "elapsed_seconds", time.Since(state.StartTime).Seconds(),
            )
            return "", fmt.Errorf("executor timeout at iteration %d: %w", state.Iteration, err)
        }
        
        iterStart := time.Now()
        
        // 1. Call gpt-oss
        e.Logger.Info("gpt_oss_call_start",
            "run_id", state.RunID,
            "iteration", state.Iteration,
            "message_count", len(state.Messages),
        )
        
        gptResponse, err := e.callGptOss(runCtx, state.Messages)
        if err != nil {
            e.Logger.Error("gpt_oss_call_failed",
                "run_id", state.RunID,
                "iteration", state.Iteration,
                "error", err,
            )
            state.LastError = err
            
            // Check if transient, retry if so
            if isTransientError(err) && state.Iteration < e.MaxIterations-1 {
                e.Logger.Info("retrying_gpt_oss_call",
                    "run_id", state.RunID,
                    "iteration", state.Iteration,
                )
                time.Sleep(time.Second * time.Duration(1<<uint(state.Iteration))) // exponential backoff
                continue
            }
            return "", err
        }
        
        // Add assistant message
        state.Messages = append(state.Messages, Message{
            Role:    "assistant",
            Content: gptResponse.Content,
        })
        
        // Track tokens
        state.TokensPerIteration[state.Iteration] = gptResponse.Tokens
        state.TotalTokens += gptResponse.Tokens
        
        // Check context window
        if state.TotalTokens+e.ContextBufferTokens > e.ContextWindowLimit {
            e.Logger.Warn("context_window_approaching",
                "run_id", state.RunID,
                "iteration", state.Iteration,
                "total_tokens", state.TotalTokens,
                "limit", e.ContextWindowLimit,
            )
            // Could summarize old messages here
        }
        
        e.Logger.Info("gpt_oss_call_complete",
            "run_id", state.RunID,
            "iteration", state.Iteration,
            "tokens", gptResponse.Tokens,
            "finish_reason", gptResponse.FinishReason,
            "elapsed_ms", time.Since(iterStart).Milliseconds(),
        )
        
        // 2. Parse tool intents from reasoning
        intents := e.Parser.Parse(gptResponse.Reasoning)
        state.ToolIntents = intents
        
        if len(intents) == 0 {
            // No tools → assume task complete
            e.Logger.Info("no_tools_requested",
                "run_id", state.RunID,
                "iteration", state.Iteration,
            )
            return gptResponse.Content, nil
        }
        
        e.Logger.Info("tool_intents_parsed",
            "run_id", state.RunID,
            "iteration", state.Iteration,
            "intent_count", len(intents),
        )
        
        // 3. Execute tools
        var toolResults []string
        var toolNames []string
        toolStart := time.Now()
        
        for _, intent := range intents {
            toolNames = append(toolNames, intent.Name)
            
            e.Logger.Info("tool_execution_start",
                "run_id", state.RunID,
                "iteration", state.Iteration,
                "tool", intent.Name,
            )
            
            result, err := e.ToolExecutor.Execute(runCtx, intent)
            if err != nil {
                e.Logger.Warn("tool_execution_failed",
                    "run_id", state.RunID,
                    "iteration", state.Iteration,
                    "tool", intent.Name,
                    "error", err,
                )
                // Don't abort; inject error as context
                state.ToolResults[intent.Name] = fmt.Sprintf("ERROR: %v", err)
                toolResults = append(toolResults, fmt.Sprintf("[ERROR: %s] %v", intent.Name, err))
            } else {
                e.Logger.Info("tool_execution_success",
                    "run_id", state.RunID,
                    "iteration", state.Iteration,
                    "tool", intent.Name,
                    "result_length", len(result),
                )
                state.ToolResults[intent.Name] = result
                toolResults = append(toolResults, fmt.Sprintf("[TOOL_RESULT: %s]\n%s", intent.Name, result))
            }
        }
        
        e.Logger.Info("tool_execution_complete",
            "run_id", state.RunID,
            "iteration", state.Iteration,
            "tools", strings.Join(toolNames, ","),
            "total_time_ms", time.Since(toolStart).Milliseconds(),
        )
        
        // 4. Inject results back into messages
        resultText := strings.Join(toolResults, "\n\n")
        state.Messages = append(state.Messages, Message{
            Role:    "user",
            Content: fmt.Sprintf("Tool execution results:\n%s\n\nContinue with next step or provide the final answer if the task is complete.", resultText),
        })
        
        // 5. Check for completion signal
        reasoning := strings.ToLower(gptResponse.Reasoning)
        content := strings.ToLower(gptResponse.Content)
        if strings.Contains(reasoning, "done") || 
           strings.Contains(reasoning, "complete") ||
           strings.Contains(content, "complete") {
            e.Logger.Info("completion_signal_detected",
                "run_id", state.RunID,
                "iteration", state.Iteration,
            )
            return gptResponse.Content, nil
        }
    }
    
    e.Logger.Error("executor_max_iterations_exceeded",
        "run_id", state.RunID,
        "max_iterations", e.MaxIterations,
        "total_tokens", state.TotalTokens,
    )
    return "", fmt.Errorf("max iterations (%d) exceeded", e.MaxIterations)
}

// callGptOss makes an HTTP request to gpt-oss API
func (e *Executor) callGptOss(ctx context.Context, messages []Message) (*GptOSSResponse, error) {
    // Build request
    payload := map[string]interface{}{
        "model":       e.GptOSSModel,
        "messages":    messages,
        "temperature": e.GptOSSTemperature,
        "max_tokens":  e.GptOSSMaxTokens,
    }
    
    // Make HTTP call
    // ... implementation details ...
    
    // Parse response
    var resp GptOSSResponse
    // ... parsing logic ...
    
    return &resp, nil
}

func isTransientError(err error) bool {
    // Check if error is transient (timeout, 5xx)
    // Return true if should retry
    return false // Simplified
}
```

### 4.4 Intent Parser (Fuzzy Matching)

```go
package parser

import (
    "fmt"
    "regexp"
    "strings"
)

type IntentParser struct {
    patterns map[string]*regexp.Regexp
}

func NewIntentParser() *IntentParser {
    return &IntentParser{
        patterns: map[string]*regexp.Regexp{
            "web_search": regexp.MustCompile(`(?i)(search|web_search|query|find).*for\s+["']?([^"'.\n]+)["']?`),
            "web_fetch":  regexp.MustCompile(`(?i)(fetch|get|retrieve|read|download).*(?:from|the|page|article|url)\s+(?:https?://)?([^\s\n]+)`),
            "read":       regexp.MustCompile(`(?i)(read|load|open|check)\s+(?:file|document)?\s*(?:named|at|path)?\s+(?:["']?([^\s"'\n]+)["']?)`),
            "write":      regexp.MustCompile(`(?i)(write|save|store|create).*(?:to|file|named|as)\s+(?:["']?([^\s"'\n]+)["']?)`),
            "exec":       regexp.MustCompile(`(?i)(execute|run|command|shell).*(?:command:)?\s*["']?([^"'\n]+)["']?`),
        },
    }
}

func (p *IntentParser) Parse(reasoning string) []ToolIntent {
    var intents []ToolIntent
    
    if reasoning == "" {
        return intents
    }
    
    // Check for each tool pattern
    for toolName, pattern := range p.patterns {
        matches := pattern.FindAllStringSubmatch(reasoning, -1)
        for _, match := range matches {
            if len(match) > 2 {
                intent := ToolIntent{
                    Name: toolName,
                    Args: make(map[string]string),
                }
                
                // Extract arguments based on tool
                switch toolName {
                case "web_search":
                    intent.Args["query"] = strings.TrimSpace(match[2])
                case "web_fetch":
                    intent.Args["url"] = normalizeURL(match[2])
                case "read":
                    intent.Args["path"] = strings.TrimSpace(match[2])
                case "write":
                    intent.Args["path"] = strings.TrimSpace(match[2])
                case "exec":
                    intent.Args["command"] = strings.TrimSpace(match[2])
                }
                
                // Avoid duplicates
                if !intentExists(intents, intent) {
                    intent.Confidence = 0.85
                    intents = append(intents, intent)
                }
            }
        }
    }
    
    return intents
}

func intentExists(intents []ToolIntent, target ToolIntent) bool {
    for _, i := range intents {
        if i.Name == target.Name && i.Args["query"] == target.Args["query"] {
            return true
        }
    }
    return false
}

func normalizeURL(url string) string {
    url = strings.TrimSpace(url)
    if !strings.HasPrefix(url, "http") {
        url = "https://" + url
    }
    return url
}
```

---

## Tool Integration Protocol

### 5.1 Tool Routing (Tool Executor)

```go
package tools

import (
    "context"
    "fmt"
)

type ToolExecutor struct {
    GatewayURL   string
    GatewayToken string
    Handlers     map[string]ToolHandler
}

type ToolHandler interface {
    Execute(ctx context.Context, args map[string]string) (string, error)
}

func (te *ToolExecutor) Execute(ctx context.Context, intent parser.ToolIntent) (string, error) {
    handler, ok := te.Handlers[intent.Name]
    if !ok {
        return "", fmt.Errorf("unknown tool: %s", intent.Name)
    }
    
    // Execute with retry
    return te.executeWithRetry(ctx, handler, intent.Args)
}

func (te *ToolExecutor) executeWithRetry(ctx context.Context, handler ToolHandler, args map[string]string) (string, error) {
    for attempt := 0; attempt < 3; attempt++ {
        result, err := handler.Execute(ctx, args)
        if err == nil {
            return result, nil
        }
        
        if !isRetryableError(err) {
            return "", err
        }
        
        // Backoff
        select {
        case <-time.After(time.Duration(1<<uint(attempt)) * time.Second):
            continue
        case <-ctx.Done():
            return "", ctx.Err()
        }
    }
    
    return "", fmt.Errorf("max retries exceeded")
}

func isRetryableError(err error) bool {
    // Check if transient (5xx, timeout, etc.)
    return false // Simplified
}
```

### 5.2 Specific Tool Implementations

#### web_search

```go
func (te *ToolExecutor) webSearch(ctx context.Context, args map[string]string) (string, error) {
    query := args["query"]
    if query == "" {
        return "", fmt.Errorf("query required")
    }
    
    // Call OpenClaw web_search tool via gateway
    // POST /api/tools/web_search?query=...
    
    // Return formatted results
    return fmt.Sprintf("1. https://...title1...\n2. https://...title2..."), nil
}
```

#### web_fetch

```go
func (te *ToolExecutor) webFetch(ctx context.Context, args map[string]string) (string, error) {
    url := args["url"]
    if url == "" {
        return "", fmt.Errorf("url required")
    }
    
    // Call OpenClaw web_fetch tool via gateway
    // POST /api/tools/web_fetch?url=...&extractMode=markdown
    
    // Return page content
    return "Article text...", nil
}
```

#### read

```go
func (te *ToolExecutor) readFile(ctx context.Context, args map[string]string) (string, error) {
    path := args["path"]
    if path == "" {
        return "", fmt.Errorf("path required")
    }
    
    // Validate path (no directory traversal)
    if strings.Contains(path, "..") {
        return "", fmt.Errorf("invalid path")
    }
    
    // Read file via gateway
    // POST /api/tools/read?path=...
    
    return "File contents...", nil
}
```

---

## Error Handling & Recovery

### 6.1 Error Types

```go
package errors

import "fmt"

type ExecutorError struct {
    Code    string
    Message string
    Err     error
}

func (e *ExecutorError) Error() string {
    return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Custom error types
var (
    ErrGptOssUnreachable = &ExecutorError{
        Code: "gpt_oss_unreachable",
        Message: "gpt-oss service is not responding",
    }
    ErrMaxIterations = &ExecutorError{
        Code: "max_iterations_exceeded",
        Message: "executor reached max iterations without completion",
    }
    ErrTimeout = &ExecutorError{
        Code: "timeout_exceeded",
        Message: "executor run exceeded timeout",
    }
    ErrContextWindow = &ExecutorError{
        Code: "context_window_exceeded",
        Message: "token usage would exceed context window limit",
    }
    ErrToolNotFound = &ExecutorError{
        Code: "tool_not_found",
        Message: "requested tool is not available",
    }
)
```

### 6.2 Recovery Strategies

```
Tier 1: RETRY (Transient)
  Condition: HTTP 5xx, timeout, network error
  Action: Retry with exponential backoff (1s, 2s, 4s)
  Max attempts: 3
  Example: gpt-oss API temporarily down, comes back up

Tier 2: SKIP & CONTINUE (Non-Fatal)
  Condition: Tool returns 404, file not found, invalid argument
  Action: Log warning, inject error into context, continue
  Max attempts: 1
  Example: URL not found, but gpt-oss can suggest alternative

Tier 3: ABORT (Fatal)
  Condition: gpt-oss unreachable after retries, max iterations, timeout
  Action: Return error to caller, stop execution
  Max attempts: 0
  Example: Critical failure, can't recover
```

---

## Token Management & Context Windows

### 7.1 Token Tracking

```go
type TokenAccounting struct {
    PerIteration   map[int]int
    Total          int
    ContextLimit   int
    BufferTokens   int // 2000 by default
    TotalAvailable int // ContextLimit - BufferTokens
}

func (ta *TokenAccounting) CanAddTokens(additional int) bool {
    return ta.Total+additional <= ta.TotalAvailable
}

func (ta *TokenAccounting) GetUsagePercent() float32 {
    return float32(ta.Total) / float32(ta.TotalAvailable) * 100
}
```

### 7.2 Context Window Budget

For Spark 72B (32K context):

```
Context Budget Breakdown:
├─ System prompt: ~500 tokens (fixed)
├─ User prompt: 100-1000 tokens (variable)
├─ Per-iteration gpt-oss output: ~300-600 tokens
├─ Per-iteration tool results: ~500-2000 tokens (can be large)
├─ Message history overhead: ~100-200 tokens
├─ Safety buffer: 2000 tokens (reserved, never used)
└─ Total: 32768 - 2000 = 30768 available

Max Iterations: 5-6 (to stay safe)
```

---

## Logging & Observability

### 8.1 Structured Logging Schema

All logs as JSON to `stdout`:

```json
{
  "timestamp": "2026-02-28T09:15:30Z",
  "level": "INFO",
  "logger": "executor",
  "event": "executor_run_start",
  "run_id": "run-xyz789",
  "user_prompt_length": 87,
  "max_iterations": 5,
  "timeout_seconds": 300
}
```

### 8.2 Log Events

| Event | Level | Fields | When |
|-------|-------|--------|------|
| `executor_run_start` | INFO | run_id, user_prompt_length, max_iterations, timeout_seconds | Run starts |
| `gpt_oss_call_start` | INFO | run_id, iteration, message_count | About to call gpt-oss |
| `gpt_oss_call_complete` | INFO | run_id, iteration, tokens, finish_reason, elapsed_ms | gpt-oss returns |
| `gpt_oss_call_failed` | ERROR | run_id, iteration, error, elapsed_ms | gpt-oss error |
| `tool_intents_parsed` | INFO | run_id, iteration, intent_count, tool_names | Intents extracted |
| `tool_execution_start` | INFO | run_id, iteration, tool, args | Tool execution starts |
| `tool_execution_success` | INFO | run_id, iteration, tool, result_length | Tool succeeds |
| `tool_execution_failed` | WARN | run_id, iteration, tool, error | Tool fails |
| `tool_execution_complete` | INFO | run_id, iteration, tools, total_time_ms | All tools done |
| `context_window_approaching` | WARN | run_id, iteration, total_tokens, limit | Token usage high |
| `no_tools_requested` | INFO | run_id, iteration | No tools found, task done |
| `completion_signal_detected` | INFO | run_id, iteration | Task complete detected |
| `executor_timeout` | ERROR | run_id, iteration, elapsed_seconds | Timeout hit |
| `executor_max_iterations_exceeded` | ERROR | run_id, max_iterations, total_tokens | Max iterations hit |

### 8.3 Error Log Persistence

All errors also written to: `logs/YYYY-MM-DD-errors.md`

```markdown
## 2026-02-28

[09:15:30] web_search | Error: query parameter missing | Attempted fix: retried with default query | Status: failed after 3 attempts
[09:16:15] gpt_oss | Error: timeout after 60s | Attempted fix: increased timeout to 90s | Status: recovered on retry 2
[09:17:45] web_fetch | Error: 404 not found | Attempted fix: injected error into context, let model adapt | Status: model suggested alternative
```

---

## Configuration & Deployment

### 9.1 Configuration File (executor.yaml)

```yaml
# executor.yaml
executor:
  # gpt-oss settings
  gpt_oss_url: "http://spark:8000"
  gpt_oss_model: "gpt-oss"
  gpt_oss_temperature: 0.25
  gpt_oss_max_tokens: 1000
  
  # Execution parameters
  max_iterations: 5
  run_timeout_seconds: 300
  context_window_limit: 32768
  context_buffer_tokens: 2000
  
  # OpenClaw gateway
  openclaw_gateway_url: "http://localhost:18789"
  openclaw_gateway_token: "${OPENCLAW_GATEWAY_TOKEN}"

http_server:
  port: 8001
  bind: "127.0.0.1"
  read_timeout_seconds: 30
  write_timeout_seconds: 30
  shutdown_timeout_seconds: 5

logging:
  level: "info"  # debug, info, warn, error
  format: "json"
  output: "stdout"
  error_log_path: "/Users/jgavinray/Obsidian/personal/zoidberg/logs"
  error_log_filename: "gpt-oss-executor-errors.md"

tools:
  # Which OpenClaw tools are available
  enabled:
    - web_search
    - web_fetch
    - read
    - write
    - exec
    - browser
  
  # Tool-specific settings
  web_search:
    timeout_seconds: 30
    max_results: 10
  web_fetch:
    timeout_seconds: 30
    max_chars: 50000
  exec:
    timeout_seconds: 60
    allow_commands: true
```

### 9.2 Deployment Steps

#### 1. Build

```bash
cd gpt-oss-executor
go mod download
go build -o bin/gpt-oss-executor cmd/main.go
```

#### 2. Run

```bash
OPENCLAW_GATEWAY_TOKEN=<token> \
bin/gpt-oss-executor --config config/executor.yaml
```

Output:
```
2026-02-28T09:15:30Z  INFO  executor  HTTP server listening on :8001
2026-02-28T09:15:30Z  INFO  executor  gpt-oss connected at http://spark:8000
2026-02-28T09:15:30Z  INFO  executor  OpenClaw gateway at http://localhost:18789
```

#### 3. Register in OpenClaw

Add to `~/.openclaw/openclaw.json`:

```json
{
  "models": {
    "providers": {
      "gpt-oss-executor": {
        "baseUrl": "http://localhost:8001/v1",
        "apiKey": "local",
        "api": "openai-completions",
        "models": [{
          "id": "executor",
          "name": "GPT-OSS Executor",
          "reasoning": true,
          "cost": {"input": 0, "output": 0},
          "contextWindow": 32768
        }]
      }
    }
  }
}
```

#### 4. Verify

```bash
curl -X POST http://localhost:8001/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-oss",
    "messages": [{"role": "user", "content": "Hello"}],
    "max_tokens": 100
  }'
```

---

## Testing Strategy

### 10.1 Unit Tests

```go
// tests/parser_test.go
func TestIntentParser(t *testing.T) {
    parser := parser.NewIntentParser()
    
    testCases := []struct {
        name     string
        input    string
        expected []string // tool names
    }{
        {
            name: "web_search",
            input: "We would search for Claude AI latest news using web_search",
            expected: []string{"web_search"},
        },
        {
            name: "multiple_intents",
            input: "Search for Claude, fetch the URL, and save to file",
            expected: []string{"web_search", "web_fetch", "write"},
        },
    }
    
    for _, tc := range testCases {
        t.Run(tc.name, func(t *testing.T) {
            intents := parser.Parse(tc.input)
            // Assert
        })
    }
}
```

### 10.2 Integration Tests

```go
// tests/integration_test.go
func TestExecutorEndToEnd(t *testing.T) {
    // Mock gpt-oss server
    // Mock OpenClaw tools
    // Run executor with test prompt
    // Verify response and tool calls
}
```

### 10.3 Load Tests

```go
// tests/load_test.go
func BenchmarkExecutor(b *testing.B) {
    // Run 100 concurrent requests
    // Measure latency, throughput, memory
}
```

---

## Security Considerations

### 11.1 Input Validation

- Validate gpt-oss model name (only allow "gpt-oss")
- Sanitize file paths (no directory traversal like `../`)
- Sanitize shell commands (if exec tool enabled)
- Validate URLs (only allow http/https)

### 11.2 Rate Limiting

- Rate limit HTTP requests (e.g., 100 req/min per IP)
- Rate limit tool execution (e.g., max 10 web_search/min)
- Timeout tool execution (e.g., 30s max)

### 11.3 Token Budget

- Enforce max_tokens per request (never exceed 1000)
- Enforce max iterations (never exceed 5)
- Enforce context window limit (never exceed 30K available)

### 11.4 Gateway Authentication

- Require valid OpenClaw gateway token
- Use token to route tool calls securely
- Log all tool execution with token/user info

---

## Performance Tuning

### 12.1 Optimizations

1. **Batching Tool Results** — Combine multiple tool results into single message
2. **Result Summarization** — Truncate large tool outputs (>5000 chars)
3. **Message Pruning** — Remove old messages when context window fills
4. **Parallel Tool Execution** — Execute tools concurrently where no dependencies
5. **Caching** — Cache frequent web_search results

### 12.2 Profiling

```bash
# CPU profile
go run -cpuprofile=cpu.prof cmd/main.go

# Memory profile
go run -memprofile=mem.prof cmd/main.go

# Analyze
go tool pprof cpu.prof
```

---

## Troubleshooting Guide

### 13.1 Common Issues

| Issue | Symptoms | Debug | Fix |
|-------|----------|-------|-----|
| gpt-oss unreachable | "connection refused" | `curl http://spark:8000/v1/models` | Start gpt-oss service |
| Max iterations hit | Task incomplete after 5 iterations | Check logs for tool intents | Simplify task, increase max_iterations |
| Context window overflow | "context window exceeded" error | Check `context_usage_percent` in logs | Reduce result summarization size |
| Tool failures | Web_search returns 500 | Check OpenClaw gateway logs | Restart gateway or fix tool |
| Slow responses | Latency > 60s | Check gpt-oss CPU usage | Profile, add caching, parallelize |

### 13.2 Debug Mode

```bash
GPTOSS_EXECUTOR_LOG_LEVEL=debug bin/gpt-oss-executor --config config/executor.yaml
```

Outputs:
- Full gpt-oss request/response
- Full intent parsing details
- All tool arguments and results
- Token accounting per iteration

---

## Implementation Roadmap

### Phase 1: Core PoC (1-2 days)

**Goals:**
- HTTP server + `/v1/chat/completions` endpoint working
- Calls gpt-oss, parses reasoning, executes tools
- Supports 3 tools (web_search, web_fetch, read)
- Basic error handling (retry/skip/abort)
- Structured JSON logging

**Deliverables:**
- [ ] `cmd/main.go` (server + handlers)
- [ ] `internal/executor/executor.go` (core loop)
- [ ] `internal/parser/intent_parser.go` (fuzzy matching)
- [ ] `internal/tools/tool_executor.go` (web_search, web_fetch, read)
- [ ] `internal/logging/logger.go` (slog setup)
- [ ] `config/executor.yaml` (default config)
- [ ] `Makefile` (build, run, test)
- [ ] `README.md` (setup instructions)

**Testing:**
- [ ] Unit test for intent parser
- [ ] Integration test (mock gpt-oss, mock tools)
- [ ] Manual smoke test (real gpt-oss, real web_search)

---

### Phase 2: Robustness (2-3 days)

**Goals:**
- All 8+ OpenClaw tools supported
- Token accounting + context window management
- 3-tier error recovery fully implemented
- Tool result summarization
- Checkpoint/resume for interrupted runs

**Deliverables:**
- [ ] Implement remaining tools (write, exec, browser, etc.)
- [ ] `internal/logging/token_accounting.go`
- [ ] Retry logic with exponential backoff
- [ ] Error log persistence (`logs/YYYY-MM-DD-errors.md`)
- [ ] Tool result summarization/truncation
- [ ] Context window overflow detection & message pruning
- [ ] Checkpoint save/restore

**Testing:**
- [ ] Test each tool individually
- [ ] Test token accounting accuracy
- [ ] Test context window limits
- [ ] Test error recovery scenarios

---

### Phase 3: Production (3-5 days)

**Goals:**
- OpenClaw gateway integration
- Subagent spawning support
- Streaming mode (optional)
- Multi-model support
- Metrics dashboard
- Production observability

**Deliverables:**
- [ ] `~/.openclaw/openclaw.json` integration
- [ ] Subagent support (spawn/kill)
- [ ] Streaming `/v1/chat/completions` (SSE)
- [ ] Metrics server (Prometheus format)
- [ ] Health check endpoint
- [ ] Graceful shutdown
- [ ] Docker image & deployment guide
- [ ] Production docs (SLA, monitoring, alerting)

**Testing:**
- [ ] Load tests (concurrent requests)
- [ ] Stress tests (token limits, timeouts)
- [ ] Chaos tests (tool failures, network issues)
- [ ] End-to-end integration with OpenClaw

---

## Success Criteria Checklist

- [ ] HTTP server listens on :8001 with `/v1/chat/completions`
- [ ] Accepts OpenAI-compatible requests (model, messages, temperature, max_tokens)
- [ ] Calls gpt-oss and parses reasoning field correctly
- [ ] Detects tool intents with fuzzy NLP parsing (no strict markers)
- [ ] Executes tools sequentially via OpenClaw API
- [ ] Injects tool results back into conversation
- [ ] Loops until max iterations or completion signal
- [ ] Completes within 300s timeout
- [ ] Tracks tokens per iteration and enforces context window limit
- [ ] Logs all events as structured JSON
- [ ] Persists errors to `logs/YYYY-MM-DD-errors.md`
- [ ] Handles errors gracefully (retry/skip/abort)
- [ ] Integrates into `~/.openclaw/openclaw.json`
- [ ] Can be spawned as subagent via `sessions_spawn`
- [ ] Production-ready (observability, metrics, health checks)

---

## References & Resources

- **gpt-oss Model:** Local 120B reasoning model via vLLM
- **OpenClaw Tools:** Full list at `https://docs.openclaw.ai/tools`
- **OpenAI API:** Chat completions spec at `https://platform.openai.com/docs/api-reference/chat/create`
- **Go Stdlib:** `net/http`, `log/slog` (Go 1.21+), `context`, `sync`
- **Project Codebase:** `gpt-oss-executor/` repository
- **Architecture Review:** `/Users/jgavinray/Obsidian/personal/zoidberg/gpt-oss-poc/opus-architecture-review.md`

---

## Contact & Support

For questions or blockers:
1. Check logs with `GPTOSS_EXECUTOR_LOG_LEVEL=debug`
2. Review troubleshooting guide (Section 13.1)
3. Check gpt-oss health: `curl http://spark:8000/v1/models`
4. Check OpenClaw health: `curl -H "Authorization: Bearer $TOKEN" http://localhost:18789/health`

---

**Status:** Specification complete and ready for implementation. Start with Phase 1 (Core PoC). This document is self-contained and requires no additional context.

**Last Updated:** 2026-02-28 09:30:00 PST  
**Specification Version:** 1.0  
**Estimated Implementation Time:** 6-10 days total (1-2 + 2-3 + 3-5 days)
