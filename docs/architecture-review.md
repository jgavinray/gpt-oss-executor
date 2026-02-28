# GPT-OSS Executor Architecture Review

**Date:** 2026-02-28
**Reviewer:** Opus (architecture review subagent)
**Context:** Reviewing the bash PoC (`parser.sh`, `poc-working.sh`) and evaluating a Go-based executor implementation.

> For full implementation details, see [gpt-oss-executor-spec.md](gpt-oss-executor-spec.md).

---

## 1. Is the Approach Sound?

**Verdict: Yes, with significant caveats.**

The core idea — use gpt-oss's `reasoning` field to extract structured tool intents, then execute them in a loop — is sound and creative. You're essentially building an agentic loop on top of a model that wasn't designed for tool calling, by imposing a structured marker format (`[TOOL:name|arg=val]`) via system prompt.

**What works:**
- The `reasoning` field is a natural place to capture "thinking about tools" before the model commits to a response
- The `[TOOL:...]` marker format is simple and parseable
- The iterative loop with max iterations prevents runaway execution

**Critical problems to solve in Go:**
- **The PoC doesn't actually work yet** — execution log shows empty reasoning/content fields. The model isn't reliably producing the `[TOOL:...]` markers. This is the fundamental risk: gpt-oss (120B) may not reliably follow the structured output format, especially under low token budgets
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

### Timeouts

| Component | Timeout | Rationale |
|-----------|---------|-----------|
| gpt-oss API call | 60s | Spark can be slow on long reasoning |
| Individual tool execution | 30s | web_fetch can hang |
| Full run (all iterations) | 300s | Hard cap to prevent runaways |
| Idle between iterations | 1s | Rate limiting / backpressure |

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

Start with **Option A** for the PoC, then migrate to **Option B** for production.

### Critical Edge Cases for Marker Parsing

The model may produce malformed markers like:
- `[TOOL: web_search | query = hello]` (spaces)
- `[TOOL:web_search|query=hello world|another=thing]` (values with spaces/pipes)
- Nested brackets or partial markers

Make the regex tolerant and add a normalization step.

---

## 4. Token Accounting: Billing Context

Since Spark is self-hosted (FREE), token accounting is primarily for:
1. **Context window management** — don't exceed model limits by accumulating tool results
2. **Performance monitoring** — identify prompts that cause excessive iterations
3. **Comparison benchmarking** — compare cost if this were running on a paid API

For implementation details (structs, budget breakdown, tracking), see the spec.

---

## 5. Error Handling: Malformed Reasoning

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

## 7. Key Risk: Model Compliance

**The #1 risk is model compliance.** The execution log shows the model returned empty reasoning/content. Before building the Go executor, validate that gpt-oss (120B) reliably produces `[TOOL:...]` markers with:
1. Temperature tuning (try 0.1-0.3)
2. More explicit few-shot examples in the system prompt
3. Longer max_tokens (600 may be too low for reasoning + markers + content)
4. Consider if the vLLM endpoint is correctly exposing the `reasoning` field (it may need specific model configuration)

If the model can't reliably produce structured markers, the entire architecture needs to pivot to either:
- A different model that supports native tool calling
- A more forgiving parsing approach (NLP-based intent extraction instead of regex)
- Using the `content` field instead of `reasoning` for tool markers