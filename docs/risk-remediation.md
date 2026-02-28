# Risk Remediation Plan

**Date:** 2026-02-28
**Context:** Research-backed remediation strategies for the critical risks identified in the [architecture review](architecture-review.md) and [executor spec](gpt-oss-executor-spec.md).

---

## Risk 1: Model Compliance — gpt-oss Won't Produce Structured Markers

**Severity:** Critical
**Status:** Confirmed — PoC showed empty reasoning/content fields

The entire architecture depends on extracting tool intents from gpt-oss output. If the model doesn't produce parseable markers, nothing works.

### Remediation: vLLM Guided Decoding (Primary)

vLLM natively supports constrained decoding via `guided_json`, `guided_regex`, and `guided_grammar` parameters. This forces the model to produce valid structured output at the token level — no post-hoc parsing needed.

**Implementation:** Add `guided_json` to the vLLM API call:

```json
{
  "model": "gpt-oss",
  "messages": [...],
  "extra_body": {
    "guided_json": {
      "type": "object",
      "properties": {
        "reasoning": {"type": "string"},
        "tool_calls": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "name": {"type": "string", "enum": ["web_search", "web_fetch", "read", "write", "exec"]},
              "arguments": {"type": "object"}
            },
            "required": ["name", "arguments"]
          }
        },
        "done": {"type": "boolean"}
      },
      "required": ["reasoning", "tool_calls", "done"]
    }
  }
}
```

**Backends:** vLLM supports `outlines` and `xgrammar` (default in v0.6+) for guided decoding. Configure via `--guided-decoding-backend` server flag.

**Trade-off:** Constrained decoding may degrade reasoning quality on complex tasks. Test empirically.

### Remediation: vLLM Native Tool Calling (Alternative)

vLLM supports `--tool-call-parser` for many model families:

| Parser | Models |
|--------|--------|
| `llama3_json` | Llama 3.1, 3.2, 3.3 |
| `hermes` | Nous Hermes 2 Pro, Hermes 3 |
| `mistral` | Mistral-7B-Instruct v0.3+, Mixtral |
| `internlm` | InternLM2 |

If gpt-oss is based on one of these model families, enable native tool calling:

```bash
vllm serve gpt-oss \
    --tool-call-parser hermes \
    --chat-template path/to/template.jinja
```

This would eliminate the need for custom intent parsing entirely.

### Remediation: Adopt ReAct Format (Fallback)

The `[TOOL:name|arg=val]` format is novel and not in training data. The ReAct format (`Thought/Action/Action Input`) is widely represented in LLM training corpora and models follow it far more reliably:

```
Thought: I need to search for Rust async patterns.
Action: web_search
Action Input: {"query": "Rust async patterns best practices 2026"}
```

Parsing is simple line-oriented regex:
```go
var (
    actionRe = regexp.MustCompile(`(?m)^Action:\s*(\w+)\s*$`)
    inputRe  = regexp.MustCompile(`(?m)^Action Input:\s*(.+)$`)
)
```

### Remediation: Two-Phase Generation

Let the model reason freely (unconstrained) in phase 1, then apply guided decoding for structured tool output in phase 2:

1. Generate with no constraints — model produces `<think>...</think>` reasoning
2. After `</think>`, apply `guided_json` to force structured tool calls

Can be implemented with a custom EBNF grammar:
```ebnf
root ::= thinking_block (tool_call_block | text_response)
thinking_block ::= "<think>" free_text "</think>\n"
tool_call_block ::= "<tool_call>\n" json_object "\n</tool_call>"
```

### Validation Steps Before Building

1. Test `guided_json` with gpt-oss to confirm it produces valid structured output
2. Test if vLLM's `--enable-reasoning` + `--reasoning-parser` works with this model
3. Test ReAct format in system prompt with few-shot examples
4. Increase `max_tokens` to 1000+ (600 may be too low for reasoning + markers)
5. Try temperature 0.1-0.3 for more deterministic marker production

---

## Risk 2: Fuzzy Parsing Reliability

**Severity:** High
**Status:** Mitigated if Risk 1 remediation works; critical if it doesn't

When guided decoding isn't used or the model produces unexpected output, the parser must handle free-form text.

### Remediation: 4-Tier Parser Cascade

Implement a cascade that tries increasingly permissive extraction:

```go
func (p *IntentParser) Parse(text string) ([]ToolIntent, error) {
    // Tier 1: Structured JSON extraction
    if intents := p.parseJSON(text); len(intents) > 0 {
        return intents, nil
    }
    // Tier 2: ReAct format (Action/Action Input lines)
    if intents := p.parseReAct(text); len(intents) > 0 {
        return intents, nil
    }
    // Tier 3: Custom markers [TOOL:name|arg=val]
    if intents := p.parseMarkers(text); len(intents) > 0 {
        return intents, nil
    }
    // Tier 4: Fuzzy keyword matching (natural language)
    return p.parseFuzzy(text), nil
}
```

This is the pattern used by LangChain, CrewAI, and AutoGPT.

### Remediation: Retry with Format Correction

If all parsing tiers fail, inject a format-correction message and re-prompt:

```go
if len(intents) == 0 && state.Iteration < e.MaxIterations {
    state.Messages = append(state.Messages, Message{
        Role:    "user",
        Content: "I couldn't parse your tool usage. Please restate using exactly this format:\nAction: tool_name\nAction Input: {\"arg\": \"value\"}\n\nAvailable tools: web_search, web_fetch, read, write, exec",
    })
    // Re-prompt gpt-oss — consumes one iteration
}
```

LangChain implements this via `OutputFixingParser` and `RetryWithErrorOutputParser`.

### Remediation: Fuzzy Tool Name Matching

Handle model variation in tool names (e.g., "web-search" vs "web_search"):

```go
var toolAliases = map[string]string{
    "web_search": "web_search", "websearch": "web_search", "search": "web_search",
    "web_fetch":  "web_fetch",  "webfetch":  "web_fetch",  "fetch":  "web_fetch",
    "read_file":  "read",       "readfile":  "read",       "open":   "read",
    "write_file": "write",      "writefile": "write",      "save":   "write",
    "execute":    "exec",       "run":       "exec",       "shell":  "exec",
}
```

### Remediation: JSON Repair

When the model produces malformed JSON (missing quotes, trailing commas, truncated output), apply a repair step before `json.Unmarshal`:

- Strip markdown code fences (` ```json ... ``` `)
- Add missing closing brackets/braces
- Strip trailing commas
- Handle partial output from token limit truncation (extract and execute the first N complete tool calls)

### Go Libraries

| Library | Purpose |
|---------|---------|
| `regexp` (stdlib) | Primary regex parsing |
| `github.com/dlclark/regexp2` | .NET-compatible regex with lookahead/lookbehind |
| `github.com/jdkato/prose` | NLP tokenization and NER for argument extraction |

---

## Risk 3: Context Window Exhaustion

**Severity:** High
**Status:** Manageable with proper budgeting

Tool results accumulate across iterations. With 32K context and ~1000 tokens per tool result, you exhaust the window in 5-6 iterations.

### Remediation: Tiered Context Management

Implement three tiers triggered by context usage percentage:

```go
func (e *Executor) manageContext(state *RunState) {
    usage := float64(state.TokensUsed) / float64(e.ContextLimit)

    switch {
    case usage > 0.8:
        // Aggressive: summarize all old iterations into one block
        state.Messages = e.compactMessages(state.Messages, state.Iteration)
    case usage > 0.6:
        // Moderate: truncate old tool results to 500 chars each
        state.Messages = e.truncateOldResults(state.Messages, 500)
    default:
        // Normal: truncate individual results at injection time (3000 chars max)
    }
}
```

### Remediation: Tool Result Truncation at Injection

Hard cap all tool results before injecting into messages. This is the universal first defense across all agentic frameworks:

| Tool | Max Result Size | Strategy |
|------|----------------|----------|
| web_search | Top 3-5 results, 200 chars each | Keep titles + URLs + snippets |
| web_fetch | 3000 chars | First N chars with truncation marker |
| read | First 100 lines + last 20 lines | Head/tail pattern |
| exec | First 1000 chars + last 500 chars | Head/tail pattern |

### Remediation: Hybrid Token Counting

No external tokenizer dependency needed:

1. **Pre-call estimation:** Character heuristic (~3.5 chars/token) to predict whether the next call will fit
2. **Post-call accounting:** Use `usage.prompt_tokens` and `usage.completion_tokens` from vLLM's response for exact tracking

```go
type TokenCounter struct {
    ActualUsed   int     // from vLLM usage field
    ContextLimit int     // e.g., 32768
    BufferTokens int     // e.g., 2000
}

func (tc *TokenCounter) EstimateTokens(text string) int {
    return int(math.Ceil(float64(len(text)) / 3.5))
}

func (tc *TokenCounter) CanFit(additionalText string) bool {
    return tc.ActualUsed + tc.EstimateTokens(additionalText) + tc.BufferTokens <= tc.ContextLimit
}
```

### Remediation: Handle vLLM Context Overflow

vLLM returns HTTP 400 with `context_length_exceeded` error code — it does NOT silently truncate. The executor must catch this, compact messages, and retry:

```go
if apiErr.Code == "context_length_exceeded" {
    state.Messages = e.contextManager.Compact(state.Messages)
    // Retry with compacted context
    resp, err = e.callGptOss(ctx, state.Messages)
}
```

### Remediation: vLLM Prefix Caching

Enable `--enable-prefix-caching` on the vLLM server. This caches KV blocks for the shared system prompt and early messages across iterations, improving performance without extending the context window.

---

## Risk 4: gpt-oss Latency (60s+ per call)

**Severity:** Medium
**Status:** Mitigated by existing design (timeouts + iteration caps)

### Remediation: Already in Spec

- 60s per-call timeout with retry + backoff (3 attempts)
- 300s global run timeout
- Max 5 iterations hard cap
- Batch mode (not streaming) is correct — need complete reasoning before parsing

### Additional Mitigations

- **vLLM chunked prefill** (`--enable-chunked-prefill`): Processes long prompts in chunks, better GPU memory utilization
- **vLLM prefix caching** (`--enable-prefix-caching`): Reuses KV cache for shared prefixes across iterations
- **Token budget awareness**: Don't waste iterations — if context is nearly full, return best partial answer instead of continuing

---

## Risk 5: Reasoning Field Availability

**Severity:** High
**Status:** Depends on model architecture and vLLM configuration

The `reasoning_content` field is model-specific, not an OpenAI API standard. It follows the DeepSeek API convention for models using `<think>...</think>` tags.

### Remediation: vLLM Reasoning Parser

If gpt-oss uses `<think>...</think>` tags (DeepSeek-R1, QwQ style), enable reasoning extraction:

```bash
vllm serve gpt-oss \
    --enable-reasoning \
    --reasoning-parser deepseek_r1   # or qwq
```

Response will include:
```json
{
  "choices": [{
    "message": {
      "reasoning_content": "Let me think step by step...",
      "content": "The answer is..."
    }
  }]
}
```

### Remediation: Combined Reasoning + Tool Parsers

vLLM supports running both `--reasoning-parser` and `--tool-call-parser` simultaneously:

```bash
vllm serve gpt-oss \
    --enable-reasoning \
    --reasoning-parser deepseek_r1 \
    --tool-call-parser hermes \
    --chat-template path/to/custom_template.jinja
```

The reasoning parser strips `<think>...</think>` into `reasoning_content`, then the tool parser processes the remaining content for tool calls.

### Remediation: Parse from Content Field (Fallback)

If the `reasoning_content` field is not available for this model, fall back to parsing tool intents from the `content` field directly. The 4-tier parser cascade (Risk 2) works regardless of which field the text comes from:

```go
func (e *Executor) extractIntents(resp *GptOssResponse) ([]ToolIntent, error) {
    // Try reasoning field first
    if resp.Reasoning != "" {
        if intents, err := e.parser.Parse(resp.Reasoning); err == nil && len(intents) > 0 {
            return intents, nil
        }
    }
    // Fall back to content field
    return e.parser.Parse(resp.Content)
}
```

---

## Implementation Priority

| Risk | Remediation | Phase | Effort |
|------|-------------|-------|--------|
| 1. Model compliance | Test vLLM guided decoding + ReAct format | **Pre-Phase 1** | 1 day |
| 2. Fuzzy parsing | 4-tier parser cascade | Phase 1 | Built into parser |
| 3. Context exhaustion | Tool result truncation + token counting | Phase 1 | Built into executor loop |
| 3. Context exhaustion | Tiered context management (60%/80%) | Phase 2 | 1 day |
| 5. Reasoning field | Test `--enable-reasoning` flag | **Pre-Phase 1** | 0.5 day |
| 4. Latency | vLLM prefix caching + chunked prefill | Phase 2 | Config-only |
| 2. Fuzzy parsing | Retry with format correction | Phase 2 | 0.5 day |

**Recommended pre-Phase 1 validation** (before writing any Go code):

1. Test if `--enable-reasoning` exposes the `reasoning_content` field for gpt-oss
2. Test if `guided_json` produces valid structured output without degrading reasoning
3. Test if `--tool-call-parser` works natively for this model family
4. Test ReAct format with few-shot examples at temperature 0.2, max_tokens 1000+

Results from these tests determine whether to use guided decoding (primary path) or the parser cascade (fallback path) as the core architecture.