# gpt-oss-executor

An OpenAI-compatible HTTP gateway that wraps a local 120B reasoning model (gpt-oss) with an agentic loop, routing tool calls through the OpenClaw gateway.

## Problem

OpenClaw provides a rich set of tools (web search, file read/write, shell exec, browser control) accessible through a gateway API, but the built-in model driving those tools has limited reasoning depth. A locally-hosted 120B reasoning model (gpt-oss, served via vLLM) has substantially better planning and multi-step reasoning capabilities — but it has two problems:

1. **It speaks text, not tool calls.** vLLM serves gpt-oss as a raw completion endpoint. The model produces reasoning output in natural language or structured text formats, not OpenClaw's native tool protocol.
2. **It has no execution environment.** Even when the model correctly identifies what tools to call, it has no mechanism to actually invoke them, receive results, and continue reasoning.

gpt-oss-executor bridges this gap. It sits between callers and the model, parsing the model's output for tool intents, executing those tools through OpenClaw's existing gateway, and feeding results back so the model can reason further. The result is a locally-hosted reasoning agent that can use all of OpenClaw's tooling without modifications to either gpt-oss or the OpenClaw gateway.

## Architecture

The executor supports two execution modes, selected by `executor.mode` in the config.

### ReAct mode (default)

Incoming `POST /v1/chat/completions` requests are forwarded to gpt-oss. The model's response is parsed for tool call intents using one of four configurable strategies; matched intents are dispatched to the OpenClaw gateway at `POST /tools/invoke`. Results are injected back into the conversation as `tool` role messages and the loop repeats until the model produces a final answer, the iteration limit is hit, or the run timeout elapses.

```
client
  └─► POST /v1/chat/completions (port 8001)
        └─► gpt-oss vLLM (port 8000)   ← model decides what tools to call
              └─► intent parser
                    └─► OpenClaw gateway POST /tools/invoke (port 18789)
                          └─► tool result injected → repeat
```

### RAG mode

The executor classifies the user's message directly — without asking gpt-oss — executes the relevant tools, then calls gpt-oss once with the retrieved context to synthesise a final answer. gpt-oss never needs to emit tool intent; it acts purely as a synthesis engine.

```
client
  └─► POST /v1/chat/completions (port 8001)
        └─► fuzzy intent classifier (user message)
              └─► OpenClaw gateway POST /tools/invoke (port 18789)
                    └─► synthesis prompt (question + tool results)
                          └─► gpt-oss vLLM (port 8000)   ← one synthesis call
                                └─► answer returned
```

RAG mode is more predictable than ReAct because it does not rely on the model deciding when and how to call tools. It is the recommended mode when the model has a hardcoded system prompt (e.g. gpt-oss ships with a "You are ChatGPT / cannot browse" prompt baked into its vLLM serving config) that conflicts with tool-calling instructions.

## Prerequisites

- Go 1.22 or later
- A running vLLM instance serving gpt-oss (default: `http://spark:8000`)
- A running OpenClaw gateway (default: `http://localhost:18789`)
- `jq` for the smoke test (optional)

## Quickstart

```bash
# 1. Clone the repository
git clone https://github.com/jgavinray/gpt-oss-executor.git
cd gpt-oss-executor

# 2. Copy and edit the config
cp config/executor.yaml.example config/executor.yaml
$EDITOR config/executor.yaml   # set gpt_oss_url and openclaw_gateway_url

# 3. Set the required gateway token
export GPTOSS_EXECUTOR_GATEWAY_TOKEN=<your-openclaw-token>

# 4. Build the binary
make build

# 5. Run the executor
make run

# 6. Verify with a smoke test (requires jq)
make smoke
```

The executor listens on `http://127.0.0.1:8001` by default. The `make health` target hits `GET /health` for a quick liveness check.

## Configuration reference

Copy `config/executor.yaml.example` to `config/executor.yaml`. All fields can be overridden by environment variables. The naming pattern is `GPTOSS_EXECUTOR_<UPPER_KEY>` — only the keys with a defined env var are listed in the table; the rest must be set in the YAML file.

### `executor`

| Field | Env var override | Default | Description |
|---|---|---|---|
| `mode` | — | `react` | Execution strategy: `react` (agentic loop) or `rag` (pre-classify → tools → synthesize) |
| `rag_auto_fetch` | — | `false` | RAG mode: automatically fetch top search result URL(s) to supplement snippets with full page content |
| `rag_fetch_top_n` | — | `1` | Maximum number of search result URLs to successfully fetch; tries next candidate if one fails |
| `gpt_oss_url` | `GPTOSS_EXECUTOR_GPT_OSS_URL` | — | Base URL of the vLLM OpenAI-compatible endpoint |
| `gpt_oss_model` | — | `gpt-oss` | Model name passed to vLLM in each request |
| `gpt_oss_temperature` | — | `0.25` | Sampling temperature |
| `gpt_oss_max_tokens` | — | `1000` | Max completion tokens per vLLM call |
| `gpt_oss_call_timeout_seconds` | — | `60` | Per-call HTTP timeout for vLLM requests |
| `max_iterations` | — | `5` | Maximum agentic loop iterations before giving up |
| `max_retries` | — | `3` | Retry attempts for transient tool / vLLM errors |
| `run_timeout_seconds` | — | `300` | Overall deadline for a single run |
| `context_window_limit` | — | `32768` | Token budget for the model context window |
| `context_buffer_tokens` | — | `2000` | Reserved tokens kept free for new completions |
| `context_compact_threshold` | — | `0.8` | Drop oldest messages above this fraction of the window |
| `context_trunc_threshold` | — | `0.6` | Shorten tool results above this fraction of the window |
| `openclaw_gateway_url` | `GPTOSS_EXECUTOR_GATEWAY_URL` | `http://localhost:18789` | Base URL of the OpenClaw gateway |
| `openclaw_gateway_token` | `GPTOSS_EXECUTOR_GATEWAY_TOKEN` | — | Bearer token for the OpenClaw gateway (required) |
| `openclaw_session_key` | — | `main` | Session key passed to every `/tools/invoke` call |

### `parser`

| Field | Default | Description |
|---|---|---|
| `strategy` | `react` | Primary parse strategy (`guided_json`, `react`, `markers`, `fuzzy`) |
| `fallback_strategy` | `fuzzy` | Strategy tried when the primary returns no intents |
| `source_field` | `reasoning` | Response field to parse (`reasoning` or `content`) |
| `fallback_field` | `content` | Field to parse when `source_field` is empty |
| `system_prompt_path` | `config/system-prompt-react.txt` | Path to the system prompt file loaded at startup |
| `guided_json_schema_path` | — | Path to a JSON schema file; required only for `guided_json` strategy |

### `http_server`

| Field | Env var override | Default | Description |
|---|---|---|---|
| `port` | `GPTOSS_EXECUTOR_PORT` | `8001` | Port the HTTP server listens on |
| `bind` | — | `127.0.0.1` | Bind address |
| `read_timeout_seconds` | — | `30` | HTTP read timeout |
| `write_timeout_seconds` | — | `600` | HTTP write timeout; must exceed `run_timeout_seconds` |
| `idle_timeout_seconds` | — | `120` | Keep-alive idle timeout |
| `shutdown_timeout_seconds` | — | `5` | Graceful shutdown window on SIGINT/SIGTERM |

### `logging`

| Field | Env var override | Default | Description |
|---|---|---|---|
| `level` | `GPTOSS_EXECUTOR_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `format` | — | `json` | Log format: `json` or `text` |
| `output` | — | `stdout` | Log destination (`stdout` or `stderr`) |
| `error_log_dir` | — | `logs` | Directory for the daily error markdown log |
| `error_log_filename` | — | `errors.md` | Base filename; `YYYY-MM-DD` is prepended at runtime |

### `tools`

| Field | Default | Description |
|---|---|---|
| `enabled` | `[web_search, web_fetch, read, write, exec, browser]` | Allowlist of tool names forwarded to the gateway |
| `default_timeout_seconds` | `30` | Gateway HTTP client timeout used when no per-tool value is set |
| `result_limits.<tool>` | varies | Maximum characters returned per tool before truncation |

Per-tool sub-sections (`web_search`, `web_fetch`, `read`, `write`, `exec`, `browser`) accept a `timeout_seconds` field. `web_search` also accepts `max_results`; `web_fetch` accepts `max_chars` and `extract_mode` (`markdown` or `text`); `exec` accepts a `blocked_commands` list.

## Parser strategies

| Strategy | Confidence | When to use |
|---|---|---|
| `guided_json` | 1.0 | vLLM is started with `--guided-decoding-backend` and a JSON schema is provided; the model emits a structured `{"tool_calls": [...], "done": bool}` payload. Most reliable. |
| `react` | 0.9 | Default. The model follows the ReAct format (`Action: <tool>` / `Action Input: <args>`). Works well with the bundled system prompt. |
| `markers` | 0.85 | The model uses inline `[TOOL:name\|key=val]` markers. Useful for fine-tuned models trained on this syntax. |
| `fuzzy` | 0.6 | Last-resort natural language pattern matching. Catches plain-English requests such as "search for X" or "fetch https://...". Always safe as a fallback. |

The `fallback_strategy` field names the strategy tried when the primary returns no intents. The default pair (`react` + `fuzzy`) covers the widest range of model outputs without schema constraints.

## vLLM / gpt-oss quirks

These are observed behaviours of the gpt-oss model served via vLLM that affect executor configuration.

### `max_tokens` safe range

vLLM returns an `Unexpected token NNNNN` special-token parse error when `max_tokens` is set outside the range **300–750**. Values below 300 or at 1000+ reliably trigger this error. Set `gpt_oss_max_tokens` to a value in the 350–700 range.

### System role messages return 0 choices

Sending a `{"role": "system", ...}` message causes vLLM to return `choices: []` with no error. The executor works around this by injecting any system prompt into the first user message instead. If you supply a `system_prompt_path`, it is prepended to the user message content, not sent as a separate system turn.

### Hardcoded "You are ChatGPT" system prompt

gpt-oss ships with a hardcoded OpenAI system prompt baked into its vLLM serving configuration. This prompt instructs the model that it is ChatGPT and cannot browse the internet. User-supplied system prompts are overridden by this baked-in prompt, which means the model will not spontaneously emit tool call intent in ReAct mode. **RAG mode is not affected** because it classifies the user's message directly and does not rely on the model deciding to use tools.

### Certain phrase patterns trigger tokenizer errors

Specific input phrasings — notably "Search the web for..." — trigger a `BadRequestError` (HTTP 400) from vLLM due to special-token conflicts in the chat template. If you observe 0-choice responses or 400 errors for prompts that work in other phrasings, the input may be hitting one of these patterns. In RAG mode the synthesis prompt is always wrapped in a neutral task frame ("Task: answer the following question...") which avoids the known bad patterns.

## Development

```bash
make test          # go test -race ./...
make test-verbose  # go test -race -v ./...
make lint          # go vet ./...
make tidy          # go mod tidy
make clean         # remove bin/
make build         # build bin/gpt-oss-executor (debug)
make release       # build with -ldflags="-s -w" (stripped)
```

## Project structure

```
.
├── cmd/
│   └── main.go                      # Entry point: config, wiring, signal handling
├── config/
│   ├── executor.yaml.example        # Annotated config template
│   └── system-prompt-react.txt      # Default ReAct system prompt
├── internal/
│   ├── config/
│   │   └── config.go                # YAML loader, env overrides, validation
│   ├── errors/
│   │   └── errors.go                # Sentinel errors and ExecutorError type
│   ├── executor/
│   │   └── executor.go              # Agentic loop, context management, vLLM calls
│   ├── httpserver/
│   │   └── server.go                # OpenAI-compatible HTTP server (POST /v1/chat/completions, GET /health)
│   ├── logging/
│   │   └── logger.go                # slog construction and daily error log writer
│   ├── parser/
│   │   └── intent_parser.go         # 4-strategy intent parser (guided_json, react, markers, fuzzy)
│   └── tools/
│       └── tool_executor.go         # GatewayClient, argument mapping, retry, truncation
└── tests/
    └── parser_test.go               # Table-driven parser tests
```
