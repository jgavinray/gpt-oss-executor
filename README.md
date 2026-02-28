# gpt-oss-executor

An OpenAI-compatible HTTP gateway that wraps a local 120B reasoning model (gpt-oss) with an agentic loop, routing tool calls through the OpenClaw gateway.

## Architecture

Incoming `POST /v1/chat/completions` requests are received by the executor, which forwards the conversation to a vLLM-served gpt-oss instance. The response is parsed for tool call intents using one of four configurable strategies; matched intents are dispatched to the OpenClaw gateway at `POST /tools/invoke`. Results are injected back into the conversation as `tool` role messages and the loop repeats until the model produces a final answer, the iteration limit is hit, or the run timeout elapses.

```
client
  └─► POST /v1/chat/completions (port 8001)
        └─► gpt-oss vLLM (port 8000)
              └─► intent parser
                    └─► OpenClaw gateway POST /tools/invoke (port 18789)
                          └─► tool result injected → repeat
```

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
