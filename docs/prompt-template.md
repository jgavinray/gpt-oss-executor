# GPT-OSS System Prompts — Reference

This file contains the system prompts sent to gpt-oss for each parser strategy.
The active strategy is set via `parser.strategy` in `config/executor.yaml`.

> **Important:** Tool names in prompts MUST match the canonical names used in the executor:
> `web_search`, `web_fetch`, `read`, `write`, `exec`, `browser`

---

## Strategy 1: Guided JSON (Recommended — requires vLLM `guided_json`)

When `parser.strategy: "guided_json"`, the executor sends a JSON schema via vLLM's
`guided_json` parameter to constrain the model's output at decode time. The system prompt
is minimal because the schema enforces structure.

### System Prompt

```
You are a tool-using assistant. Analyze the user's request and determine what tools to call.

Available tools:
- web_search(query): Search the web for information
- web_fetch(url): Fetch and extract content from a URL
- read(path): Read a file from disk
- write(path, content): Write content to a file
- exec(command): Execute a shell command
- browser(action, url): Control a browser (navigate, screenshot, snapshot)

Respond with your reasoning and the tool calls needed. Set "done" to true when no more tools are needed.
```

### Guided JSON Schema

Passed as `extra_body.guided_json` in the vLLM API call:

```json
{
  "type": "object",
  "properties": {
    "reasoning": {
      "type": "string",
      "description": "Your step-by-step reasoning about what to do"
    },
    "tool_calls": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "name": {
            "type": "string",
            "enum": ["web_search", "web_fetch", "read", "write", "exec", "browser"]
          },
          "arguments": {
            "type": "object"
          }
        },
        "required": ["name", "arguments"]
      }
    },
    "done": {
      "type": "boolean",
      "description": "true if the task is complete and no more tools are needed"
    }
  },
  "required": ["reasoning", "tool_calls", "done"]
}
```

### Expected Output

```json
{
  "reasoning": "The user wants to find news about Rust async. I should search for it first, then fetch the top result, then save a summary.",
  "tool_calls": [
    {"name": "web_search", "arguments": {"query": "Rust async patterns best practices 2026"}}
  ],
  "done": false
}
```

---

## Strategy 2: ReAct Format (Recommended fallback — no vLLM features required)

When `parser.strategy: "react"`, the executor parses the model's output for
`Thought/Action/Action Input/Observation` blocks. This format is widely present in
LLM training data and models follow it reliably without constrained decoding.

### System Prompt

```
You are a tool-using assistant. You solve tasks by reasoning step-by-step and calling tools.

Available tools:
- web_search: Search the web. Arguments: {"query": "search terms"}
- web_fetch: Fetch a web page. Arguments: {"url": "https://..."}
- read: Read a file. Arguments: {"path": "/path/to/file"}
- write: Write a file. Arguments: {"path": "/path/to/file", "content": "..."}
- exec: Run a shell command. Arguments: {"command": "..."}
- browser: Control browser. Arguments: {"action": "navigate|screenshot|snapshot", "url": "..."}

You MUST use this exact format for every step:

Thought: <your reasoning about what to do next>
Action: <tool_name>
Action Input: <JSON arguments>

After you receive tool results, continue reasoning. When the task is complete:

Thought: <summary of what was accomplished>
Action: done
Action Input: {}

IMPORTANT: Always use this exact format. One action per step.
```

### Few-Shot Example (include in system prompt for best results)

```
Example:

User: Find information about Rust async patterns and save a summary.

Thought: I need to search for information about Rust async patterns first.
Action: web_search
Action Input: {"query": "Rust async patterns best practices 2026"}

[Tool results will be injected here]

Thought: I found several results. Let me fetch the most relevant article.
Action: web_fetch
Action Input: {"url": "https://blog.rust-lang.org/async-patterns"}

[Tool results will be injected here]

Thought: I have the article content. Now I'll save a summary to a file.
Action: write
Action Input: {"path": "rust-async-summary.md", "content": "# Rust Async Patterns\n\n..."}

[Tool results will be injected here]

Thought: The task is complete. I searched for Rust async patterns, fetched the top article, and saved a summary.
Action: done
Action Input: {}
```

### Parser Regex

```go
var (
    actionRe = regexp.MustCompile(`(?m)^Action:\s*(\w+)\s*$`)
    inputRe  = regexp.MustCompile(`(?m)^Action Input:\s*(.+)$`)
)
```

---

## Strategy 3: Tool Markers (Original PoC format)

When `parser.strategy: "markers"`, the executor parses `[TOOL:name|arg=val]` markers.
This is the least reliable strategy — use only if the model has been specifically
fine-tuned or prompted to produce this format.

### System Prompt

```
You are a reasoning-driven agent. Your task is to break down requests into steps.

For each step, use this EXACT format in your response:
[TOOL:tool_name|arg1=value1|arg2=value2]

Available tools:
- [TOOL:web_search|query=search terms here]
- [TOOL:web_fetch|url=https://example.com]
- [TOOL:read|path=/path/to/file]
- [TOOL:write|path=/path/to/file|content=file content here]
- [TOOL:exec|command=shell command here]
- [TOOL:browser|action=navigate|url=https://example.com]

After listing all tool calls needed for the current step, end with: [DONE]

Example:
User: Find news about AI and save it.
Response:
I'll search for AI news, fetch the top result, and save a summary.
[TOOL:web_search|query=AI news 2026]
[TOOL:web_fetch|url=https://example.com/ai-news]
[TOOL:write|path=ai-summary.md|content=Summary of AI news...]
[DONE]
```

### Parser Regex

```go
var toolRegex = regexp.MustCompile(`(?i)\[\s*TOOL\s*:\s*(\w+)\s*\|([^\]]+)\]`)
```

---

## Strategy 4: Fuzzy / NLP (Last resort — highest tolerance, lowest precision)

When `parser.strategy: "fuzzy"`, the executor scans the model's output for natural
language patterns that imply tool usage. No specific prompt format is required.

### System Prompt

```
You are a helpful assistant with access to the following tools:
- web_search: Search the web for information
- web_fetch: Fetch content from a URL
- read: Read a file from the filesystem
- write: Write content to a file
- exec: Execute a shell command
- browser: Control a web browser

When you need to use a tool, describe what you would do clearly. For example:
- "I would search for 'Rust async patterns'"
- "I would fetch the page at https://example.com"
- "I would read the file at /path/to/file"
- "I would write the results to /tmp/output.md"
- "I would run the command 'ls -la'"

Be explicit about tool names, search queries, URLs, and file paths.
```

### Parser Patterns

```go
var fuzzyPatterns = map[string]*regexp.Regexp{
    "web_search": regexp.MustCompile(`(?i)(?:search|look up|query|find)\s+(?:for\s+)?["']?(.+?)["']?(?:\s+(?:on|using|via)|[.\n]|$)`),
    "web_fetch":  regexp.MustCompile(`(?i)(?:fetch|retrieve|get|download|open)\s+(?:the\s+)?(?:page|url|site|content)?\s*(?:at|from)?\s*(https?://\S+)`),
    "read":       regexp.MustCompile(`(?i)(?:read|open|view|check)\s+(?:the\s+)?(?:file|contents?\s+of)\s+["'\x60]?([/\w.\-]+)["'\x60]?`),
    "write":      regexp.MustCompile(`(?i)(?:write|save|create|output)\s+(?:to|as|the file)\s+["'\x60]?([/\w.\-]+)["'\x60]?`),
    "exec":       regexp.MustCompile(`(?i)(?:run|execute|exec)\s+(?:the\s+)?(?:command|shell|bash)?\s*["'\x60](.+?)["'\x60]`),
}
```

---

## Choosing a Strategy

| Strategy | Reliability | Requirements | Best For |
|----------|-------------|-------------|----------|
| `guided_json` | Very High | vLLM with `guided_json` support | Production (preferred) |
| `react` | High | None (prompt only) | General purpose fallback |
| `markers` | Low-Medium | Model must follow exact format | Fine-tuned models only |
| `fuzzy` | Medium | None | Last resort / any model |

**Recommended configuration:**

```yaml
parser:
  strategy: "react"           # Primary strategy
  fallback_strategy: "fuzzy"  # Used when primary fails to extract intents
  source_field: "reasoning"   # Parse from reasoning field first
  fallback_field: "content"   # Fall back to content field if reasoning is empty
```

If vLLM guided decoding is available and tested:

```yaml
parser:
  strategy: "guided_json"
  guided_json_schema_path: "config/guided-schema.json"
  source_field: "content"     # Guided decoding puts structured output in content
```