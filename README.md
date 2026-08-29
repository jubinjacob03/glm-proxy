# GLM Bridge — Z.AI Proxy API

An **OpenAI** and **Anthropic-compatible** API proxy for [chat.z.ai](https://chat.z.ai). Drop it in front of any OpenAI- or Anthropic-compatible tool and start using Z.AI's GLM models without browser automation or complex setup at runtime.

---

## Features

- **Dual protocol** — OpenAI `/v1/chat/completions` + Anthropic `/v1/messages` on the same server
- **No browser on the request path** — completions are pure HTTP; a headless Chromium runs only periodically, in the background, to replenish device tokens when the store runs low
- **Throwaway chat sessions (context-rot guard)** — Every stateless request runs on a chat session that is **deleted on Z.AI the moment its response is fully processed** (`DELETE /api/v1/chats/{chat_id}`), so no server-side history outlives a request and the account never accumulates dead sessions
- **Async session pool (default)** — A standing batch of pre-made sessions (`SESSION_POOL_SIZE`, default 5) is kept ready at all times; requests grab one instantly, and each consumed session is deleted upstream + replaced immediately, so the batch refills itself while the app runs. `--sync-mode` / `SYNC_MODE=true` restores the legacy per-request flow (still garbage-collected)
- **Graceful shutdown** — CTRL+C / SIGTERM drains in-flight requests (10 s deadline), then deletes every remaining pooled chat session on Z.AI before exiting (a second CTRL+C force-exits)
- **Background captcha cache** — When agent mode is enabled, captcha params are pre-generated asynchronously (2 cached, 75 s TTL, auto-pauses after 2 min idle so it stops spending device tokens on nobody)
- **Automatic token replenishment** — A background monitor watches the device token store and runs the collector when it drops below `TOKEN_MIN`, with exponential backoff on repeated failure
- **Observability** — Real counters behind `/metrics` (Prometheus text) and `/admin/stats`, plus a `/health` check that reports the device token level rather than just session state
- **Streaming + non-streaming** — Full SSE support with keep-alive pings every 5 s
- **Agent mode** — Translates OpenAI tools / function-calling into a text contract that Z.AI can understand, then intercepts `<<<TOOL_CALL>>>` blocks from the model's output and rewrites them back into native `tool_calls` deltas. Ships a **modern** XML-sectioned prompt shim (tolerant marker/fence/payload parsing, history summarization — the default) plus the original **legacy** `[ROLE: ...]` shim as an opt-in.
- **Per-model feature resolution** — Features resolved per-model from Z.AI server capabilities, with user overrides stored per-model. `image_generation` is **always forced to `false`**.
- **`reasoning_effort` support** — `high` / `max` values forwarded only when the model's capabilities explicitly allow it; `enable_thinking` is force-enabled when active
- **Token pool** — Device tokens harvested via the token collector (`cmd/token-collector`) and stored in `tokens.sqlite`. Claimed FIFO and atomically, and only ever spent by the call that actually consumes them, so an upstream hiccup no longer destroys a token (up to 5 attempts per captcha computation, with backoff)
- **Live model list** — Models fetched from Z.AI `/api/models` (cached 5 min, falls back to a static list)
- **Pure-Go SQLite** — Uses `modernc.org/sqlite` — no CGO required
- **HTTP/2 + pooled connections** — Optimised transport for both Aliyun and Z.AI endpoints

---

## Supported Models

Models are fetched live from Z.AI's `/api/models` (cached 5 min). The server keeps models from the newest down to `glm-4.7` (inclusive). The fallback list (used if Z.AI is unreachable) is:

| Model ID       | Notes                                                   |
| -------------- | ------------------------------------------------------- |
| `glm-5.3`      | Flagship model, excels at coding and long-horizon tasks |
| `glm-5.2`      | Previous flagship model                                 |
| `GLM-5-Turbo`  | New model for chat, coding, and agentic tasks           |
| `GLM-5v-Turbo` | Vision model with evolved intelligence                  |
| `glm-4.7`      | Classic high-performance model                          |

The live list (from `/v1/models`) also includes `glm-5.3-flash` (id `x-preview-l`) when Z.AI is reachable.

> **Note:**
>
> - If you don't pass `model` in `/v1/chat/completions` or `/v1/messages`, the server defaults to `glm-4.7` (works without a `ZAI_TOKEN`).
> - Z.AI's guest session (no `ZAI_TOKEN`) typically only allows `glm-5.3-flash` and `glm-4.7`. Use `glm-4.7` for fast tokenless testing.
> - `/models` (plural) returns `{ models: [...], currentModel: "glm-5.2" }` for clients that expect that shape.

---

## Getting `ZAI_TOKEN` (optional, but recommended)

`ZAI_TOKEN` is a Z.AI JWT. Setting it skips guest initialization and unlocks all models.

1. Go to **https://chat.z.ai** and log in.
2. Open browser **DevTools** (`F12` or `Ctrl+Shift+I`).
3. Navigate to **Application → Local Storage → https://chat.z.ai**.
4. Find the key named **`token`** and copy its value.
5. Export it before starting the server:

   ```bash
   # Linux / macOS
   export ZAI_TOKEN="paste-the-copied-jwt-here"

   # Windows PowerShell
   $env:ZAI_TOKEN="paste-the-copied-jwt-here"
   ```

   Or, in the DevTools **Console** tab, run:

   ```js
   localStorage.getItem("token");
   ```

   and copy the printed string.

---

## Getting Started

```bash
# 1. Clone the repo
git clone https://github.com/jubinjacob03/glm-proxy.git
cd glm-proxy

# 2. Resolve dependencies
go mod tidy

# 3. Configure — copy the sample and fill in your values
cp .env.example .env

# 4. Build the collector and fetch its browser (one time, ~700 MB on disk)
go build -trimpath -ldflags="-s -w" -o token-collector ./cmd/token-collector
./token-collector --install-browsers

# 5. Generate the token database
./token-collector

# 6. Start the server
go build -trimpath -ldflags="-s -w" -o zai-api . && ./zai-api
```

On Windows, `.\make.ps1 run` does steps 5 and 6 for you: it builds the collector
and proxy, then starts the proxy. The proxy loads `.env` itself and seeds the
token store on first run, so nothing else is needed.

On startup, you'll see a banner with your health URL, API endpoints, and auth token. The Z.AI session is initialised asynchronously — if guest init fails, the first chat request will retry it.

> Building from source is the developer path. For everyday use pick a
> [deployment method](#deployment) instead.

---

## Deployment

Two turnkey ways to run this without touching the source.

### Windows installer (background tray app)

`.\make.ps1 installer` produces a single `GLM-Proxy-Setup.exe` that bundles the
proxy, the token collector, and a tray supervisor. Share that one file.

```powershell
# Build the installer (needs Go + NSIS installed)
.\make.ps1 installer
# -> installer\out\GLM-Proxy-Setup.exe
```

Running the setup:

- Installs per-user to `%LOCALAPPDATA%\Programs\GLM-Proxy` (no administrator prompt).
- Requires your **Z.AI token** on a setup screen, with five steps for copying it out of your browser. Setup will not continue without one: image generation and the newer routes do not work on a guest session.
- Downloads the Playwright browser and collects the first device tokens during setup, so the first launch has nothing left to fetch.
- Registers autostart, so the proxy runs on every login and keeps running in the tray.
- Starts immediately when setup finishes.

Uninstalling asks whether to keep your data. Keeping it preserves `tokens.sqlite`,
`.env` and the ~700 MB browser in `%LOCALAPPDATA%\GLM-Proxy`, so a later reinstall
skips the download and pre-fills your token.

The tray icon (right-click) has three actions:

| Action           | What it does                                                              |
| ---------------- | ------------------------------------------------------------------------- |
| **Monitor Logs** | Opens a console tailing the live proxy log                                |
| **Change token** | Prompts for a new Z.AI token, writes it to `.env`, and restarts the proxy |
| **Exit proxy**   | Stops this instance (autostart brings it back at next login)              |

The tray supervises `zai-api.exe`: it restarts the proxy if it crashes, runs it
windowless, and is bound to a Windows job object so the proxy can never be left
orphaned if the tray dies. Setup already fetched the browser and the first tokens,
so a first run only starts serving. Should either setup step have failed, the
proxy's monitor finishes the job on demand and the first request waits for it
rather than failing.

### Docker (server, 24/7)

```bash
cp .env.example .env      # set ZAI_TOKEN and AUTH_TOKEN
docker compose up -d --build
```

The container (`glm-proxy`) restarts unless explicitly stopped, so it
survives reboots and Docker restarts. Configuration is baked in at build time
from `.env`; a named volume keeps the token store and logs across container
replacements. Rotate the token with `-e ZAI_TOKEN=...` or by editing `.env` and
rebuilding.

---

## Using it in an agentic IDE (TRAE, Cline, etc.)

The proxy is OpenAI-compatible, so point any tool that accepts a custom OpenAI
provider at it. For **TRAE**, add a custom model:

| Setting  | Value                                                                 |
| -------- | --------------------------------------------------------------------- |
| Base URL | `http://localhost:3007/v1`                                            |
| API key  | `Jubin` or `unlimited` (any value in `AUTH_TOKEN`)                    |
| Model    | `glm-5.3` (set a `ZAI_TOKEN` to unlock it); `glm-4.7` works tokenless |

Enable **agent mode** (the installer and Docker default already do) so tools work.

**What works**

- **Streaming chat** and normal completions.
- **Tool / function calling.** TRAE's tools are translated into a prompt
  contract and the model's calls are converted back to native `tool_calls`
  deltas (with `role`, `index`, `id`, `type`, and JSON-string arguments), closed
  by `finish_reason: tool_calls`. This is what lets TRAE edit files, run
  commands, and drive its agent loop.
- **Image attachments**, on a vision-capable model. Attached images are
  forwarded to Z.AI as standard `image_url` content blocks.

**Honest caveats**

- Tool calling is a **prompt-contract emulation**, not the upstream's native
  function API (Z.AI's unofficial endpoint has none). It is tolerant and well
  tested, but a model that ignores the contract can occasionally emit a tool
  call as plain text. `glm-5.3` and `glm-4.7` follow it reliably.
- **Images require a vision model** (`GLM-5v-Turbo`) **and** a `ZAI_TOKEN`.
  `glm-5.3` is a coding/text flagship, and guest sessions are usually limited to
  `glm-5.3-flash` / `glm-4.7` — attach images on `GLM-5v-Turbo`, or the upstream
  ignores them.
- Image passthrough targets Z.AI's documented `image_url` format; validate it
  against your account, since the unofficial web endpoint is not contractually
  guaranteed.

---

## Configuration

### CLI Flags

| Flag                   | Default         | Description                                                                                                                                                                        |
| ---------------------- | --------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--db-path`            | `tokens.sqlite` | Path to the SQLite token database                                                                                                                                                  |
| `--verbose`            | `false`         | Shorthand for `LOG_LEVEL=debug`                                                                                                                                                    |
| `--agent-mode`         | `false`         | Enable agent mode (translate tools & roles for Z.AI compatibility; also starts the background captcha cache)                                                                       |
| `--agent-mode-variant` | `modern`        | Agent-mode shim variant: `modern` (XML-sectioned prompt, tolerant parsing — recommended) or `legacy` (the original `[ROLE: ...]` rewrite shim)                                     |
| `--sync-mode`          | `false`         | Legacy synchronous session flow: create a fresh chat per request instead of drawing from the pre-warmed session pool (used sessions are still deleted on Z.AI after each response) |

### Environment Variables

| Variable                  | Default           | Description                                                                                                                                                   |
| ------------------------- | ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `PORT`                    | `3007`            | HTTP server port                                                                                                                                              |
| `HOST`                    | `127.0.0.1`       | Bind address. `127.0.0.1` accepts local clients only; set `0.0.0.0` to expose on the network                                                                  |
| `AUTH_TOKEN`              | `Jubin,unlimited` | Accepted API keys for Bearer / `x-api-key` auth; comma-separated to allow several                                                                             |
| `TIMEOUT`                 | `300000`          | Request timeout in milliseconds                                                                                                                               |
| `ZAI_TOKEN`               | _(empty)_         | Hardcoded Z.AI JWT — skips guest initialization                                                                                                               |
| `AGENT_MODE`              | `false`           | Enable agent mode (`1`/`true`/`yes`/`on`/`modern` to enable with the modern shim, `legacy` to enable with the legacy shim)                                    |
| `AGENT_MODE_VARIANT`      | `modern`          | Agent-mode shim variant override: `modern` or `legacy` (takes precedence over the implicit variant of `AGENT_MODE`)                                           |
| `LOG_LEVEL`               | `info`            | `debug`, `info`, `warn`, `error`, `off`. `debug` logs every Z.AI request body, response header set, and SSE line, which is expensive on the streaming path    |
| `LOG_FORMAT`              | `text`            | Log format                                                                                                                                                    |
| `STREAM_HOLDBACK`         | `24`              | Runes held back at the tail of a live stream so Z.AI `edit_content` tail-backtracks are absorbed before text reaches the client (`0` disables; see issue #23) |
| `SYNC_MODE`               | `false`           | Restore the legacy synchronous session flow (one fresh chat per request, no pre-warmed pool). Used sessions are still deleted after use                       |
| `SESSION_POOL_SIZE`       | `5`               | Standing batch of pre-made ready chat sessions kept by the async session pool                                                                                 |
| `SESSION_ACQUIRE_TIMEOUT` | `10`              | Seconds a request waits for a pooled session before creating one directly instead of stalling (`0` = wait indefinitely)                                       |

### Logging

Every log line is written to a size-rotated file in full and echoed to the
console truncated, so a streaming payload stays readable in the terminal without
losing anything on disk. The file writer is buffered and flushed on a ticker.

| Variable            | Default | Description                                                          |
| ------------------- | ------- | -------------------------------------------------------------------- |
| `LOG_DIR`           | `logs`  | Directory holding the rotated log files                              |
| `LOG_CONSOLE_WIDTH` | `150`   | Byte budget for a console line before truncation (rune-safe)         |
| `LOG_MAX_SIZE_MB`   | `10`    | Rotate once the active file passes this size (rotation is size-only) |
| `LOG_MAX_BACKUPS`   | `7`     | Rotated files to keep                                                |
| `LOG_MAX_AGE_DAYS`  | `7`     | Days to keep rotated files                                           |
| `LOG_FLUSH_MS`      | `500`   | How often the buffered file writer is drained                        |

### Limits and device tokens

Each captcha verification spends exactly one device token, so an empty store
fails every completion. The bridge watches the level and runs the collector for
you.

| Variable                         | Default    | Description                                                                                          |
| -------------------------------- | ---------- | ---------------------------------------------------------------------------------------------------- |
| `MAX_REQUEST_BYTES`              | `33554432` | Inbound request body cap (32 MiB)                                                                    |
| `UPSTREAM_MAX_CONNS`             | `128`      | Concurrent connections to Z.AI. The uTLS dialer speaks HTTP/1.1 only, so this caps in-flight streams |
| `CAPTCHA_CACHE_SIZE`             | `2`        | Captcha parameters kept warm. Each one costs a device token and expires unused                       |
| `CAPTCHA_TTL_SECONDS`            | `75`       | Lifetime of a cached parameter                                                                       |
| `CAPTCHA_IDLE_SECONDS`           | `120`      | Stop restocking after this much idle time, to stop burning tokens on nobody                          |
| `TOKEN_MONITOR`                  | `true`     | Watch the device token store and replenish it automatically                                          |
| `TOKEN_MIN`                      | `50`       | Run the collector once the store drops below this                                                    |
| `TOKEN_COLLECT_BATCH`            | `2`        | Batches per collector run                                                                            |
| `TOKEN_MONITOR_INTERVAL_SECONDS` | `60`       | Poll interval. Consecutive failures back this off exponentially                                      |
| `TOKEN_COLLECT_TIMEOUT_SECONDS`  | `900`      | Kill a collector run that exceeds this                                                               |
| `TOKEN_COLLECTOR_PATH`           | _(auto)_   | Explicit path to the collector binary; auto-detected next to the proxy or on `PATH`                  |

---

## Session Lifecycle — Throwaway Sessions & the Session Pool

OpenAI-compatible clients are **stateless**: they re-send the entire conversation (user + assistant turns) on every request. The bridge forwards that history to Z.AI inside a chat identified by `chat_id`, and every chat referenced by a completion materializes server-side under the bridge account (`ZAI_TOKEN`, or the guest identity) together with its full history.

If those chats were left behind, two things would rot the experience:

1. **Accumulation** — the account fills up with dead sessions, one per proxied request.
2. **Context rot** — if a server-side chat outlives a single request, Z.AI's stored history stacks on top of the history the client already re-sent; the model sees duplicated/stale context and conversation quality degrades.

So the bridge treats every stateless request as **throwaway**:

- Each request draws a chat session, streams its response, and **the moment the response is fully written (or definitively failed)** the chat is deleted on Z.AI via `DELETE /api/v1/chats/{chat_id}`. Deletion is idempotent — Z.AI's `{"detail":"We could not find what you're looking for :/"}` reply counts as success, so the GC never jams on an already-collected chat.
- Clients never see the `chat_id`; their conversation state lives entirely in their own `messages` array, so deleting the server-side chat is invisible to them.

### Async mode (default)

At startup the bridge pre-makes a standing batch of `SESSION_POOL_SIZE` (default **5**) sessions and keeps it warm:

- A completion request **acquires** a ready session instantly (no per-request creation cost).
- If a burst exhausts the batch, extra requests wait up to `SESSION_ACQUIRE_TIMEOUT` seconds (default **10**) and then create a session directly instead of stalling.
- Only after a response has been **fully written and processed** is the consumed session deleted upstream and a replacement created — the batch refills itself for as long as the app runs.

Z.AI chat IDs are client-generated UUIDs (a chat only materializes server-side when a completion first references it), so warmup is instant and local, and a session that is never consumed never touches the account at all.

```bash
# tune the async flow (optional)
SESSION_POOL_SIZE=5            # standing ready-session batch size
SESSION_ACQUIRE_TIMEOUT=10     # seconds to wait for a pooled session (0 = forever)
```

### Sync mode (legacy)

`--sync-mode` (or `SYNC_MODE=true`) restores the legacy flow: every request creates its own session first, then completes, then the session is garbage-collected — no pre-warming. Used sessions are still deleted after each response.

### Graceful shutdown

Pressing CTRL+C (or sending SIGTERM) stops the bridge respectfully: it stops accepting new connections, lets in-flight responses finish (10 s drain deadline), prints `clearing all remaining sessions...`, deletes every still-pooled chat session on Z.AI so nothing is left behind, and only then exits. A second CTRL+C force-exits immediately.

### Observability

`GET /status` reports the live session-lifecycle state under `sessionPool`:

```json
{
  "mode": "async",
  "throwaway": true,
  "gc_enabled": true,
  "size": 5,
  "ready": 5
}
```

---

## API Reference

> **Reminder:** the **OpenAI endpoint is recommended**. The Anthropic endpoint works but is considered primitive.

### OpenAI-Compatible

| Method | Path                   | Auth | Description                                  |
| ------ | ---------------------- | ---- | -------------------------------------------- |
| `POST` | `/v1/chat/completions` | ✅   | Chat completions (streaming + non-streaming) |
| `GET`  | `/v1/models`           | ✅   | OpenAI-style model list                      |
| `GET`  | `/models`              | ✅   | Compact `{ models, currentModel }` shape     |

### Anthropic-Compatible

| Method | Path           | Auth | Description                                                            |
| ------ | -------------- | ---- | ---------------------------------------------------------------------- |
| `POST` | `/v1/messages` | ✅   | Anthropic Messages API (streaming + non-streaming, tool use, thinking) |

Anthropic clients authenticate via the `x-api-key` header (same value as `AUTH_TOKEN`). The `anthropic-version` header is allowed through CORS.

#### `/v1/chat/completions` body

| Field                  | Type   | Default       | Notes                                                                                        |
| ---------------------- | ------ | ------------- | -------------------------------------------------------------------------------------------- |
| `model`                | string | `glm-4.7`     | Any model ID from `/v1/models`                                                               |
| `messages`             | array  | _(required)_  | OpenAI-style message array                                                                   |
| `stream`               | bool   | `true`        | SSE stream when true                                                                         |
| `reasoning`            | bool   | _(per-model)_ | Enables `enable_thinking` for this request                                                   |
| `thinking`             | object | _(per-model)_ | `{"type":"enabled"}` or `{"type":"disabled"}` → `enable_thinking`                            |
| `reasoning_effort`     | string | _(empty)_     | `"high"` or `"max"` — only forwarded if the model supports it; forces `enable_thinking=true` |
| `tools`                | array  | _(empty)_     | OpenAI-style tools (requires agent mode)                                                     |
| `webSearch` / `search` | bool   | _(per-model)_ | Toggle `auto_web_search` + `web_search`                                                      |

#### `/v1/messages` body

Standard Anthropic Messages API fields: `model`, `messages`, `system`, `max_tokens`, `temperature`, `top_p`, `stop_sequences`, `stream`, `thinking` (`{"type":"enabled"}`), `tools`, `reasoning_effort`.

Tool-use content blocks (`tool_use` / `tool_result`) are translated to/from OpenAI format internally. Requires agent mode for tool calls to function.

#### Request headers

| Header                               | Purpose                                                                    |
| ------------------------------------ | -------------------------------------------------------------------------- |
| `Authorization: Bearer <AUTH_TOKEN>` | OpenAI-style auth                                                          |
| `x-api-key: <AUTH_TOKEN>`            | Anthropic-style auth                                                       |
| `Include-All-Features: true`         | (Only for `POST /features`) Send all server capabilities to `/completions` |

### Management

| Method | Path             | Auth | Description                                                                                                                                                |
| ------ | ---------------- | ---- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GET`  | `/`              | ❌   | Redirects to `/health`                                                                                                                                     |
| `GET`  | `/health`        | ❌   | Health check. `200` when the session is up **and** device tokens remain, else `503`. Reports the token level, whether it is low, active streams and uptime |
| `GET`  | `/status`        | ❌   | Live session status (JSON: connected, userName, userId, feVersion, features, mode)                                                                         |
| `GET`  | `/metrics`       | ✅   | Prometheus text exposition: requests, upstream latency and errors, SSE volume, captcha cache, device tokens, collector runs, pool state                    |
| `GET`  | `/admin/stats`   | ✅   | Same counters as `/metrics`, as JSON                                                                                                                       |
| `GET`  | `/admin/health`  | ✅   | Same as `/health`                                                                                                                                          |
| `GET`  | `/admin/clients` | ✅   | Client list (`[]` or one idle entry if initialised)                                                                                                        |
| `POST` | `/features`      | ✅   | Per-model feature overrides (see below)                                                                                                                    |
| `GET`  | `/features`      | ✅   | Inspect resolved features / stored states                                                                                                                  |
| `POST` | `/stop`          | ✅   | Acknowledged no-op (returns `{ success: true }`)                                                                                                           |
| `GET`  | `/inject.js`     | ❌   | Returns `{"message":"Direct mode"}`                                                                                                                        |

> `/metrics` and `/admin/*` require the auth token, and the listener binds
> `127.0.0.1` by default so internal state is not exposed to the network.

---

## `/features` — Per-Model Feature Configuration

Features are resolved **per model** (not globally). The resolution logic is:

1. Start from the model's server capabilities (`getModelCapabilities`).
2. If `IncludeAll` is set for that model → include **all** capabilities except `reasoning_effort` (which is a support flag, not a feature value). Otherwise include nothing by default.
3. Apply stored user overrides (per-model).
4. `enable_thinking` defaults to `true` unless explicitly overridden.
5. `think` is **never** included — only `enable_thinking` reaches the request.
6. **`image_generation` is always forced to `false`** (overrides are ignored).
7. `reasoning_effort` is **never stored** — it is a per-request parameter validated against model capabilities.

### `GET /features`

- Without query: returns all per-model states:

  ```bash
  curl -X GET http://localhost:3007/features \
    -H "Authorization: Bearer Jubin"
  ```

  ```json
  {
    "states": {
      "glm-4.7": {
        "includeAll": false,
        "overrides": {
          "preview_mode": true,
          "enable_thinking": true
        }
      }
    }
  }
  ```

- With `?model=glm-4.7`: returns the resolved feature map, the stored `includeAll` flag, stored `overrides`, and the model's raw `capabilities`:

  ```bash
  curl -X GET "http://localhost:3007/features?model=glm-4.7" \
    -H "Authorization: Bearer Jubin"
  ```

  ```json
  {
    "model": "glm-4.7",
    "features": {
      "enable_thinking": true,
      "preview_mode": false,
      "image_generation": false
    },
    "includeAll": false,
    "overrides": {
      "enable_thinking": true
    },
    "capabilities": {
      "enable_thinking": true,
      "preview_mode": false,
      "image_generation": true
    }
  }
  ```

### `POST /features`

Body **must** contain `model`. Any other key is treated as a feature override (camelCase keys are converted to snake_case automatically).

**Special key handling:**

| Key                                       | Behaviour                                                   |
| ----------------------------------------- | ----------------------------------------------------------- |
| `reasoning` (bool)                        | → stored as `enable_thinking`                               |
| `thinking` (bool or `{"type":"enabled"}`) | → stored as `enable_thinking`                               |
| `image_generation`                        | Ignored — always `false`                                    |
| `think`                                   | Ignored — use `enable_thinking`, `reasoning`, or `thinking` |
| `reasoning_effort`                        | Not stored — per-request only                               |

**Toggle thinking for `glm-4.7`:**

```bash
curl -X POST http://localhost:3007/features \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer Jubin" \
  -d '{"model":"glm-4.7","enable_thinking":false}'
```

Response:

```json
{
  "success": true,
  "model": "glm-4.7",
  "includeAll": false,
  "overrides": {
    "enable_thinking": true
  },
  "features": {
    "enable_thinking": true,
    "preview_mode": false,
    "image_generation": false
  }
}
```

To enable **all** server capabilities for a model (e.g. for testing), send the header:

```bash
curl -X POST http://localhost:3007/features \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer Jubin" \
  -H "Include-All-Features: true" \
  -d '{"model":"glm-4.7"}'
```

> `image_generation` is **always** `false` — sending it in the body has no effect.

---

## Agent Mode

Z.AI's unofficial `/api/v2/chat/completions` endpoint only accepts messages with `role="user"`. System, assistant, and tool roles cause `INTERNAL_ERROR`. OpenAI-style `tools` / `tool_calls` are also rejected.

When `AGENT_MODE` is enabled (via `--agent-mode` flag or `AGENT_MODE=true` env), the server translates OpenAI roles & tools into a user-only prompt and converts the model's `<<<TOOL_CALL>>>` blocks back into native `tool_calls` deltas (OpenAI) / `tool_use` content blocks (Anthropic).

Two shim implementations are available (select with `--agent-mode-variant` or `AGENT_MODE_VARIANT`):

### Modern shim (default, recommended)

Implemented in `agent.go`. Instead of rewriting each message, it folds the entire conversation plus the tool contract into **one XML-sectioned prompt** sent as a single user message:

```
<system>          — compact output contract (pinned tool-call schema)
<tools>           — available tool definitions (name, description, JSON schema)
<history_summary> — older tool exchanges summarized (anti context-rot)
<recent>          — recent turns verbatim, tool call→result pairs grouped in <tool_exchange>
<current_task>    — the latest user message (recency anchor)
<output_rules>    — output contract repeated at the very end (recency bias)
```

The modern shim is also tolerant where the legacy one was strict:

- **Marker tolerance** — `<<<TOOL_CALL>>>` / `<<<END_TOOL_CALL>>>` are matched with 2–4 angle brackets per side, so models that miscount brackets (observed in the wild) don't leak the whole tool call as plain text.
- **Fence tolerance** — ` ```json ` fences the model wraps around the block are stripped.
- **Payload tolerance** — besides the canonical `{"name": ..., "arguments": {...}}`, flat payloads like `{"tool": "bash", "command": ...}` and alternate key spellings (`tool_name`, `function`, `parameters`, `args`, …) are accepted and normalized.
- **Stream-safe interception** — a trailing hold-back window guarantees a marker split across upstream SSE chunks never leaks to the client.
- **History summarization** — beyond the 6 most recent tool exchanges, older ones are compressed into a `<history_summary>` block, keeping long agent sessions focused on the current task.

### Legacy shim (opt-in)

The original implementation (select with `--agent-mode-variant=legacy` / `AGENT_MODE_VARIANT=legacy`), kept for backward compatibility:

1. **Mandatory system prefix** — A user message is prepended explaining the prompt architecture (roles, tools, execution contract) so the model can interpret the rewritten conversation.

2. **Role replacement** — Every non-user message is rewritten as a user message with a `[ROLE: <original_role>]` tag. e.g. a system message `"Do X"` becomes `"[ROLE: system] Do X"`.

3. **Tool translation & simulation** — OpenAI tools JSON is rendered into a user message with a strict contract: the model must emit any tool invocation as:

   ```
   <<<TOOL_CALL>>>
   {"name":"<tool_name>","arguments":{...}}
   <<<END_TOOL_CALL>>>
   ```

In both variants the stream interceptor detects the tool-call token sequence in the assistant's output, parses the JSON, and rewrites the chunk into an OpenAI-style `tool_calls` delta (or Anthropic `tool_use` content block) with `finish_reason="tool_calls"` / `stop_reason="tool_use"`.

Enabling agent mode also starts the **background captcha cache**, which pre-generates captcha verify params asynchronously (2 cached, 75 s TTL, auto-pauses after 3 minutes of inactivity) to reduce per-request latency.

---

## Examples

Every example uses `glm-4.7` so it works **without** `ZAI_TOKEN`. With a token
set you can use the newer models.

**Basic non-streaming request (OpenAI)**

```bash
curl -X POST http://localhost:3007/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer Jubin" \
  -d '{
    "model": "glm-4.7",
    "stream": false,
    "messages": [{"role": "user", "content": "Hello, who are you?"}]
  }'
```

**Streaming (SSE)**

```bash
curl -N -X POST http://localhost:3007/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer Jubin" \
  -d '{
    "model": "glm-4.7",
    "stream": true,
    "messages": [{"role": "user", "content": "Write a haiku about Go."}]
  }'
```

**Deep thinking**

```bash
curl -N -X POST http://localhost:3007/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer Jubin" \
  -d '{
    "model": "glm-4.7",
    "stream": true,
    "reasoning": true,
    "messages": [{"role": "user", "content": "Summarize today'\''s top AI news."}]
  }'
```

**Anthropic Messages API** _(primitive — OpenAI recommended)_

```bash
curl -X POST http://localhost:3007/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: Jubin" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "glm-4.7",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello, who are you?"}]
  }'
```

**Anthropic streaming with thinking**

```bash
curl -N -X POST http://localhost:3007/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: Jubin" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "glm-4.7",
    "max_tokens": 4096,
    "stream": true,
    "thinking": {"type": "enabled"},
    "messages": [{"role": "user", "content": "Explain quantum entanglement."}]
  }'
```

**Python (OpenAI SDK)**

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:3007/v1",
    api_key="Jubin",
)

resp = client.chat.completions.create(
    model="glm-4.7",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(resp.choices[0].message.content)
```

**Python (Anthropic SDK)** _(primitive — OpenAI recommended)_

```python
from anthropic import Anthropic

client = Anthropic(
    base_url="http://localhost:3007",
    api_key="Jubin",
)

resp = client.messages.create(
    model="glm-4.7",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello!"}],
)
print(resp.content[0].text)
```

---

## How It Works

1. **Guest token** — On startup, the server calls Z.AI's `/api/v1/auths/guest` (and `/api/v1/auths/`) for a session JWT, or uses `ZAI_TOKEN` if provided. The frontend version (`prod-fe-x.y.z`) is scraped from the Z.AI homepage.

2. **Captcha** — For each request, an Aliyun `captcha_verify_param` is generated **in-memory** (no FIFO, no named pipe):
   - `InitCaptchaV3` → obtain `certifyId`
   - Generate `arg` via RC4-like permutation cipher (KSA + PRGA over a 64-byte state)
   - Build a `Track` JSON, compute `ali_hash` (custom 16-byte-state hash), zlib-compress, base64-encode, then `encrypt` (second RC4-like pass with a different key)
   - `VerifyCaptchaV3` with a pooled device token → receive `securityToken`
   - Base64-encode the final `{ certifyId, isSign, sceneId, securityToken }` payload
   - Tokens are consumed FIFO from `tokens.sqlite` and deleted after use (up to **5 retries** per captcha computation)
   - When agent mode is on, a background cache pre-generates params (2 cached, 75 s TTL) to reduce latency

3. **Signature** — HMAC-SHA256 over `(sortedPayload | promptBase64 | timestamp)` with a salted bucket key derived from `SALT_KEY` and `timestamp / 300000`.

4. **Streaming** — POST to `/api/v2/chat/completions` with `stream: true`, parse SSE chunks (`edit_content` with `edit_index`, `delta_content`, `content`, or OpenAI-style `choices[0].delta.content`), and forward as OpenAI-formatted SSE or Anthropic SSE events. Inline errors (HTTP 200 with `data.error`) are detected and surfaced. On `401`, the session is re-initialised and the request retried once.

   Field semantics mirror the official Z.AI web frontend (`prod-fe` bundle): `edit_content` replaces the accumulated text from `edit_index` onward, where `edit_index` is a **UTF-16 code-unit offset** (JavaScript `String.substring` indexing — a missing `edit_index` defaults to `0`, i.e. full replacement); `content` is a **full replacement**; `delta_content` appends. Deltas forwarded to clients are always cut on rune boundaries and a small tail (`STREAM_HOLDBACK`) is kept pending, so trailing backtracks never surface as replacement-character garble (issue #23).

5. **Session garbage collection** — Once the response is fully written (or definitively failed), the request's throwaway chat is deleted on Z.AI (`DELETE /api/v1/chats/{chat_id}`) in the background, and in async mode the pool immediately stocks a replacement. On shutdown, every still-pooled session is cleared the same way. See [Session Lifecycle](#session-lifecycle--throwaway-sessions--the-session-pool).

---

## Token Collection (`cmd/token-collector`)

The token collector seeds `tokens.sqlite` with device tokens harvested from `chat.z.ai` using Playwright. It features a full TUI (Bubble Tea) with progress bar, live logs, and spinner. It is a standalone binary under `cmd/token-collector` (formerly the root-level `captcha.go`).

### Build & Run

```bash
# Portable fallback (any CPU / any OS)
go build -ldflags="-s -w" -trimpath -o token-collector ./cmd/token-collector
./token-collector

# Or, for modern CPUs (fully static, stripped)
CGO_ENABLED=0 GOAMD64=v3 go build -ldflags="-s -w" -trimpath -o token-collector ./cmd/token-collector
```

### Flags

| Flag                 | Default         | Description                                                                              |
| -------------------- | --------------- | ---------------------------------------------------------------------------------------- |
| `--unsafe`           | `false`         | Increase token limit to 1500, batch limit to 25, parallel limit to 5                     |
| `--tokens`           | `0`             | Tokens per batch (0 = prompt; default 850, max 1500)                                     |
| `--batch`            | `0`             | Number of batches (0 = prompt; default 5, max 9 / 25 with `--unsafe`)                    |
| `--parallel`         | `0`             | Parallel workers (pages) on a single browser; 0 = prompt y/N (max 3 / 5 with `--unsafe`) |
| `--headed`           | `false`         | Show browser window for debugging                                                        |
| `--block-trackers`   | `false`         | Enable URL allowlist filter to block tracker/analytics requests                          |
| `--no-tui`           | `false`         | Disable TUI, use plain text output                                                       |
| `--db-path`          | `tokens.sqlite` | Path to the SQLite token database                                                        |
| `--fresh`            | `false`         | Delete the existing database first instead of appending                                  |
| `--install-browsers` | `false`         | Download the Playwright driver and Chromium, then exit (the installer's setup step)      |
| `--skip-if-stocked`  | `0`             | Exit without collecting if the database already holds at least this many tokens          |
| `--deadline`         | `0`             | Give up after this many seconds (0 = no limit); used by setup to bound an unattended run |

Playwright's driver and browsers are kept in `%LOCALAPPDATA%\GLM-Proxy` on Windows,
or the equivalent user cache directory elsewhere, so an uninstall can preserve them.
Set `PLAYWRIGHT_BROWSERS_PATH` or `PLAYWRIGHT_DRIVER_PATH` to override.

### Usage Examples

```bash
# Interactive prompts (TUI)
./token-collector

# High volume
./token-collector --unsafe

# Specific configuration
./token-collector --tokens 750 --batch 3

# Parallel collection with a visible browser
./token-collector --parallel 3 --headed

# Plain text output (no TUI)
./token-collector --no-tui

# Block tracker domains during collection
./token-collector --block-trackers
```

### Implementation Details

- Uses pure-Go SQLite (`modernc.org/sqlite`) — no CGO needed.
- Opens a single DB connection with WAL mode + 64 MB page cache + 256 MB mmap.
- Keeps one page open per worker across all batches (force-reloads instead of open/close).
- Runs batches in parallel using multiple pages on a single browser instance.
- GC tuned to 200% to reduce pause times in allocation-heavy workloads.
- Uses lock-free atomics for the abort flag and running total.
- TUI captures stdout/stderr into a ring buffer (1000 lines) with scroll support (↑/↓, g/G).
- Optional URL allowlist blocks tracker/analytics domains — only allows `chat.z.ai`, z-cdn build assets, Aliyun captcha scripts, and `cloudauth-device` endpoints.

---

## Project Structure

The bridge core lives in `internal/zbridge`; `main.go` is a thin entry point and
the token collector is a separate binary under `cmd/`.

Tests are split by what they can reach. Blackbox tests drive the exported
surface and live in `tests/`. Whitebox tests reach unexported internals
(`streamSSEResponse`, `splitDetails`, `findAgentMarker`, `sseEmitter`, …), and Go
requires those to sit in the same directory as the package they test, so they
stay in `internal/zbridge`.

```
glm-proxy/
├── main.go                        # Thin entry point -> zbridge.Run()
├── make.ps1                       # One script: run | installer | icon
├── internal/zbridge/              # The bridge core (package zbridge)
│   ├── run.go                     # Run() entry, NewHandler(), graceful shutdown
│   ├── config.go                  # Config + env/flag/.env loading
│   ├── types.go                   # Shared types + global state
│   ├── features.go                # Per-model feature resolution
│   ├── util.go                    # HTTP clients, cookie jar, helpers
│   ├── logger.go                  # Levelled logger + truncating rotating writer
│   ├── access.go                  # One-line-per-request console summary
│   ├── metrics.go                 # Counters, /metrics exposition
│   ├── captcha.go                 # Device token store, Aliyun captcha, cache
│   ├── zai.go                     # Signature, session init, streaming, SSE parse
│   ├── sse.go                     # Pooled SSE writer + OpenAI wire types
│   ├── format.go                  # OpenAI response/error formatting
│   ├── anthropic.go               # Anthropic-compatible endpoint + translation
│   ├── middleware.go              # CORS, auth, JSON, misc handlers
│   ├── models.go                  # Live model list + capabilities
│   ├── handlers.go                # /v1/chat/completions + admin handlers
│   ├── agent.go                   # Modern agent-mode shim (XML-sectioned prompt)
│   ├── agent_legacy.go            # Legacy [ROLE: ...] agent shim
│   ├── session_pool.go            # Throwaway sessions + async pool + GC
│   ├── testhooks.go               # Exported seams for the tests/ package
│   └── *_test.go                  # Whitebox tests (agent/TRAE, SSE parser)
├── cmd/
│   ├── token-collector/           # Harvests device tokens into tokens.sqlite (TUI)
│   └── glm-tray/                  # Windows tray supervisor (Monitor Logs/Change token/Exit proxy)
├── installer/
│   ├── installer.nsi              # NSIS installer (token page, autostart, Jubin)
│   └── logo.svg                   # Official Z.AI logo, source for the icon
├── tests/                         # Blackbox tests (package tests)
├── image-gen/                     # Standalone image generation script (Node)
├── Dockerfile                     # 24/7 container (self-seeds, no entrypoint script)
├── docker-compose.yml             # `docker compose up -d`
├── .env.example                   # Copy to .env and fill in
├── go.mod
└── README.md
```

Run the whole suite:

```bash
go test ./...
```

---

## Notes

- **There is no public hosted instance.** Run it yourself — see [Deployment](#deployment).
- Device tokens are **consumed** after use, but the background monitor replenishes them automatically once the store drops below `TOKEN_MIN`. Each captcha computation makes up to **5 attempts**, and a token is only spent by the call that actually consumes it.
- The default auth keys (`Jubin`, `unlimited`) are placeholders — set your own `AUTH_TOKEN` (comma-separated for several) before exposing the proxy beyond `127.0.0.1`.
- `ZAI_TOKEN` bypasses guest initialization entirely. Without it, Z.AI's guest session typically only permits `glm-4.7`.
- `LOG_LEVEL=debug` dumps every Z.AI request body, response status/headers, and SSE lines — useful for troubleshooting.
- `image_generation` is **always `false`** and cannot be enabled via `/features` or per-request overrides.
- The captcha step has a hard 90-second timeout; if it fails, the request returns `500`.
- `--verbose` controls only the captcha subsystem's `logInfo` / `logError` output. Standard `log.*` calls (Z.AI bridge, SSE debug) are gated by `LOG_LEVEL=debug`.
- Each request gets a fresh `chat_id` — there is no server-side conversation persistence across requests. The chat is **deleted on Z.AI right after the response completes** (throwaway sessions), so the account never accumulates dead sessions; see [Session Lifecycle](#session-lifecycle--throwaway-sessions--the-session-pool).
- Agent mode is **required** for tool-calling / function-calling to work. Without it, `tools` in the request body are ignored.
- `reasoning_effort` (`"high"` / `"max"`) is only forwarded to Z.AI when the model's capabilities explicitly include `"reasoning_effort": true`. When active, `enable_thinking` is forced to `true`.

---

## License

Provided under MIT License. Use responsibly and in accordance with Z.AI's terms of service.
