# GPT-OSS Executor Architecture Review

**Date:** 2026-02-28  
**Reviewer:** Opus (architecture review subagent)  
**Context:** Reviewing the bash PoC (`parser.sh`, `poc-working.sh`) and evaluating a Go-based executor implementation.

---

## 1. Is the Approach Sound?

**Verdict: Yes, with significant caveats.**

The core idea — use gpt-oss's `reasoning` field to extract structured tool intents, then execute them in a loop — is sound and creative. You're essentially building an agentic loop on top of a model that wasn't designed for tool calling, by imposing a structured marker format (`[TOOL:name|arg=val]`) via system prompt.

**What works:**
- The `reasoning` field is a natural place to capture "thinking about tools" before the model commits to a response
- The `[TOOL:...]` marker format is simple and parseable
- The iterative loop with max iterations prevents runaway execution

**Critical problems to solve in Go:**
- **The PoC doesn't actually work yet** — execution log shows empty reasoning/content fields. The model isn't reliably producing the `[TOOL:...]` markers. This is the fundamental risk: gpt-oss (Spark 72B) may not reliably follow the structured output format, especially under low token budgets
- **No feedback loop** — the current design calls gpt-oss once, extracts tools, executes them, but never feeds tool results back into the next iteration. A real agentic loop needs: prompt → reason → extract tools → execute → inject results → re-prompt
- **Prompt engineering is load-bearing** — the entire system depends on the model producing exact `[TOOL:...]` syntax. This is fragile with smaller models

**Recommendation for Go implementation:**
- Build the feedback loop as the primary architecture (not just extraction)
- Add a fallback parser for when the model uses natural language instead of markers
- Consider few-shot examples in the system prompt to improve marker reliability

---

## 2. Implementation Strategy

### Batching

**Don't batch tool calls within a single iteration.** Execute them sequentially for these reasons:
- Tool calls often have data dependencies (search → fetch URL from results → write)
- Sequential execution simplifies error recovery
- The model's reasoning typically implies an order

**Do batch iterations as a logical unit:**
- One user request = one "run" with N iterations
- Track the full run as a single unit for logging/accounting

### Error Recovery

Implement a **3-tier recovery strategy:**

```
Tier 1: Retry (transient errors)
  - HTTP timeouts to gpt-oss → retry with backoff (max 3)
  - Tool execution failures → retry once, then inject error into context

Tier 2: Skip and continue (non-fatal)
  - web_fetch returns 404 → inject "[TOOL_ERROR: URL not found]" as assistant context
  - File not found → inject error, let model adapt

Tier 3: Abort (fatal)
  - gpt-oss unreachable after retries → abort run, save state
  - Max iterations exceeded → abort with partial results
  - Malformed response (no JSON) → abort
```

### Timeouts

| Component | Timeout | Rationale |
|-----------|---------|-----------|
| gpt-oss API call | 60s | Spark can be slow on long reasoning |
| Individual tool execution | 30s | web_fetch can hang |
| Full run (all iterations) | 300s | Hard cap to prevent runaways |
| Idle between iterations | 1s | Rate limiting / backpressure |

### Go Structure

```go
type Executor struct {
    gptossURL     string
    maxIterations int
    runTimeout    time.Duration
    toolRegistry  map[string]ToolHandler
}

type ToolHandler interface {
    Execute(ctx context.Context, args map[string]string) (string, error)
}

type RunState struct {
    Messages   []Message
    Iteration  int
    ToolCalls  []ToolCall
    TokensUsed int
    StartTime  time.Time
}
```

---

## 3. Tool Protocol: Marshaling GPT-OSS Intents to OpenClaw Format

This is the most architecturally interesting part. You have two options:

### Option A: Direct Execution (Recommended for PoC)

The Go executor calls OpenClaw tools directly via the gateway API or CLI:

```
gpt-oss reasoning: [TOOL:web_search|query=Claude AI]
     ↓ parse
Go executor: openclaw.WebSearch(ctx, "Claude AI")
     ↓ result
Inject into next message: "[TOOL_RESULT:web_search] ..."
```

**Pros:** Simple, fast, no protocol translation  
**Cons:** Tight coupling to OpenClaw internals

### Option B: OpenClaw Tool Call Format (Recommended for Production)

Marshal gpt-oss intents into OpenClaw's native tool call format, then submit through the standard tool execution pipeline:

```go
type OpenClawToolCall struct {
    Name       string            `json:"name"`
    Parameters map[string]string `json:"parameters"`
}

func marshalIntent(marker string) (*OpenClawToolCall, error) {
    // [TOOL:web_search|query=hello] → OpenClawToolCall{Name: "web_search", Parameters: {"query": "hello"}}
}
```

**Pros:** Uses existing OpenClaw tool infrastructure, security policies, rate limiting  
**Cons:** More complex, requires understanding OpenClaw's internal tool dispatch

### Recommended Approach

Start with **Option A** for the PoC. The parser is straightforward:

```go
var toolRegex = regexp.MustCompile(`\[TOOL:(\w+)\|([^\]]+)\]`)

func ParseToolMarkers(reasoning string) []ToolIntent {
    matches := toolRegex.FindAllStringSubmatch(reasoning, -1)
    var intents []ToolIntent
    for _, m := range matches {
        args := parseArgs(m[2]) // split on |, then key=value
        intents = append(intents, ToolIntent{Name: m[1], Args: args})
    }
    return intents
}
```

**Critical edge case:** The model may produce malformed markers like:
- `[TOOL: web_search | query = hello]` (spaces)
- `[TOOL:web_search|query=hello world|another=thing]` (values with spaces/pipes)
- Nested brackets or partial markers

Make the regex tolerant and add a normalization step.

---

## 4. Token Accounting

### What to Track

| Metric | Source | Purpose |
|--------|--------|---------|
| Prompt tokens per iteration | gpt-oss `usage.prompt_tokens` | Cost tracking |
| Completion tokens per iteration | gpt-oss `usage.completion_tokens` | Cost tracking |
| Reasoning tokens | gpt-oss `usage.reasoning_tokens` (if available) | Understanding overhead |
| Total tokens per run | Sum of all iterations | Per-request cost |
| Tool result injection size | Measured before injection | Context window management |

### Billing Model

**Treat each user request as one logical request, but track iterations internally:**

```go
type TokenAccounting struct {
    RunID        string
    UserRequest  string
    Iterations   []IterationTokens
    TotalPrompt  int
    TotalCompletion int
    TotalCost    float64  // computed from per-token pricing
}
```

Since Spark is self-hosted (FREE), token accounting is primarily for:
1. **Context window management** — don't exceed model limits by accumulating tool results
2. **Performance monitoring** — identify prompts that cause excessive iterations
3. **Comparison benchmarking** — compare cost if this were running on a paid API

### Context Window Budget

Spark 72B likely has 8K-32K context. Budget it:
- System prompt: ~500 tokens (fixed)
- User prompt: ~500 tokens (variable)
- Per-iteration tool results: ~1000 tokens each
- Leave 2000 tokens for model output

This gives you roughly **5-6 iterations** before context exhaustion. Implement a sliding window or summarization strategy if you need more.

---

## 5. Error Handling

### Malformed Reasoning

The most common failure mode. Handle these cases:

```go
func HandleReasoningErrors(reasoning string) ([]ToolIntent, error) {
    // Case 1: Empty reasoning
    if reasoning == "" {
        return nil, ErrEmptyReasoning // abort or retry with stronger prompt
    }
    
    // Case 2: Reasoning but no markers
    intents := ParseToolMarkers(reasoning)
    if len(intents) == 0 {
        // Try fuzzy matching: "I would search for X" → web_search
        intents = FuzzyParseIntents(reasoning)
        if len(intents) == 0 {
            return nil, ErrNoToolIntents // model didn't produce actionable output
        }
    }
    
    // Case 3: Invalid tool names
    for _, intent := range intents {
        if !IsValidTool(intent.Name) {
            log.Warn("Unknown tool in reasoning", "tool", intent.Name)
            // Skip invalid tools, continue with valid ones
        }
    }
    
    return intents, nil
}
```

### Logging Strategy

Use structured logging (e.g., `slog` in Go 1.21+):

```go
logger.Info("iteration_complete",
    "run_id", runID,
    "iteration", i,
    "tools_found", len(intents),
    "tools_executed", executed,
    "tools_failed", failed,
    "tokens_used", tokens,
    "duration_ms", elapsed.Milliseconds(),
)
```

**Log levels:**
- `INFO`: Iteration start/end, tool executions, run completion
- `WARN`: Malformed markers, fuzzy parsing fallback, tool retries
- `ERROR`: gpt-oss unreachable, fatal tool failures, context overflow
- `DEBUG`: Full reasoning text, raw API responses, parsed arguments

**Persist logs to:**
1. Structured JSON log file (for programmatic analysis)
2. OpenClaw's error log (`logs/YYYY-MM-DD-errors.md`) per AGENTS.md convention

---

## 6. Streaming vs Batch

### Analysis

| Approach | Latency | Complexity | Reliability |
|----------|---------|------------|-------------|
| **Batch** (current) | Higher (wait for full response) | Low | High |
| **Streaming** | Lower (start parsing mid-response) | High | Medium |

### Recommendation: **Batch for v1, streaming as v2 optimization**

**Why batch first:**
- The `reasoning` field needs to be complete before you can reliably parse `[TOOL:...]` markers
- Streaming complicates error handling (partial markers, connection drops mid-stream)
- gpt-oss/vLLM streaming support may have quirks
- The current architecture is inherently batch (call → parse → execute → repeat)

**When to add streaming:**
- When latency becomes a user-facing problem
- When you want to show "thinking" indicators in real-time
- When the model reliably produces markers early in reasoning

**If you do add streaming:**
- Buffer the stream until you see a complete `[TOOL:...]` marker
- Execute tools as soon as each marker is complete (don't wait for full reasoning)
- Handle the case where a partial marker spans multiple chunks
- Use `context.Context` cancellation to abort if the stream produces garbage

---

## 7. Summary: Go Implementation Roadmap

### Phase 1: Working PoC (1-2 days)
- [ ] Port bash parser to Go with `regexp`
- [ ] Implement basic agentic loop (prompt → parse → execute → feedback → repeat)
- [ ] Support 3 tools: `web_search`, `web_fetch`, `read_file`
- [ ] Batch mode only
- [ ] JSON structured logging
- [ ] Hard timeout at 300s, max 5 iterations

### Phase 2: Robustness (2-3 days)
- [ ] Fuzzy intent parsing fallback
- [ ] Token accounting and context window management
- [ ] Error recovery tiers (retry, skip, abort)
- [ ] Tool result summarization for long outputs
- [ ] Checkpoint/resume for interrupted runs

### Phase 3: Integration (3-5 days)
- [ ] OpenClaw tool protocol marshaling (Option B)
- [ ] Streaming support
- [ ] Multi-model support (swap gpt-oss for other models)
- [ ] Cost comparison dashboard
- [ ] Production error alerting

### Key Risk

**The #1 risk is model compliance.** The execution log shows the model returned empty reasoning/content. Before building the Go executor, validate that Spark 72B reliably produces `[TOOL:...]` markers with:
1. Temperature tuning (try 0.1-0.3)
2. More explicit few-shot examples in the system prompt
3. Longer max_tokens (600 may be too low for reasoning + markers + content)
4. Consider if the vLLM endpoint is correctly exposing the `reasoning` field (it may need specific model configuration)

If the model can't reliably produce structured markers, the entire architecture needs to pivot to either:
- A different model that supports native tool calling
- A more forgiving parsing approach (NLP-based intent extraction instead of regex)
- Using the `content` field instead of `reasoning` for tool markers
