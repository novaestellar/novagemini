# novagemini

> **Turn the Gemini web app into an OpenAI-compatible API.** Text · reasoning chains · web search · image generation · image editing · music · canvas · video. Single binary, with rate limiting and a multi-account pool behind an admin panel.

<p align="center">
  <img src="docs/logo.png" width="132" alt="novagemini">
</p>

<p align="center">
  <a href="https://github.com/novaestellar/novagemini/releases/latest"><img src="https://img.shields.io/badge/release-4.22.0-4285F4" alt="release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-9B72CB" alt="license"></a>
  <img src="https://img.shields.io/badge/go-1.26%2B-D96570" alt="go">
  <img src="https://img.shields.io/badge/docker-distroless-6E7B8B" alt="docker">
</p>

[中文](README.md) | **English**

---

## 📋 What this is

novagemini is a reverse proxy. It does **not** wrap Google's official API (`generativelanguage.googleapis.com`) — it speaks the `gemini.google.com` web protocol directly. Therefore:

- **No Google API Key**
- **No paid quota**
- Anonymous (no cookie) works for the text models

```
[OpenAI SDK / Cherry Studio / Cursor / Dify / new-api / ...]
    ↓ http://localhost:8083/v1/chat/completions
[novagemini]
    ↓ replicates the gemini.google.com web protocol
[Google Gemini web app]
```

The trade-off: capability follows the web app. What the web app has (web search, image generation, music, canvas, video) works here; what it lacks (deep research) does not.

---

## ✨ Features

| Feature | Description |
|---------|-------------|
| 🔌 **OpenAI-compatible** | `/v1/chat/completions` · `/v1/models` · `/v1/responses`, true incremental streaming |
| 🎬 **Video endpoint** | `/v1/videos` async jobs: create → poll → download MP4 (needs a Pro account) |
| 🖼️ **Multimodal** | Read images/video, text-to-image, **image-to-image**, music, canvas — artifacts returned inline as base64 |
| 🧠 **Reasoning chains** | `reasoning_content` field, rendered as a collapsible "thinking" block by clients |
| 🌐 **Web search** | Built into the base models; also exposed as a `web_search` tool on `/mcp` |
| 🎭 **TLS fingerprint** | `utls` replicates a real Chrome 146 handshake, not the default SDK fingerprint |
| 🍪 **Account pool** | Multiple Google accounts, least-recently-used rotation, automatic `__Secure-1PSIDTS` renewal + keep-alive |
| 🛡️ **Proxy pool** | Runtime CRUD, failure circuit-breaker, one independent rate-limit slot per proxy; each account sticks to its own exit |
| ⏱️ **Three-tier limits** | Per-exit: concurrency / RPM / RPH |
| 🖥️ **Admin panel** | Overview · requests · proxies · accounts · settings. Changes apply immediately |
| 💾 **Storage** | SQLite by default; MySQL / PostgreSQL via `SQL_DSN` |
| 🔒 **Privacy** | Prompts and responses are **never** stored — only metadata (length, latency, model, status) |

---

## 🚀 Quick install

### Option 1: Download the binary

Grab the archive for your platform from [Releases](https://github.com/novaestellar/novagemini/releases). No Go, no Docker — a single file is everything.

```bash
chmod +x novagemini_*
./novagemini_* --port 8083 --admin-token your-admin-token
```

Data lands in `./data/gemini.db` by default; add `--db /your/path.db` to move it.

For MySQL / PostgreSQL, set `SQL_DSN` (leave it unset for SQLite — no code changes needed):

```bash
# MySQL (the native go-sql-driver form user:pass@tcp(host:3306)/dbname also works)
SQL_DSN="mysql://user:pass@host:3306/dbname"

# PostgreSQL
SQL_DSN="postgres://user:pass@host:5432/dbname?sslmode=disable"
```

Tables are created automatically; all three backends share one schema.

### Option 2: Docker

```bash
docker run -d --name novagemini \
  -p 127.0.0.1:8083:8083 \
  -v "$PWD/data:/data" \
  -e ADMIN_TOKEN=your-admin-token \
  ghcr.io/novaestellar/novagemini:latest
```

Or use compose without cloning:

```bash
curl -O https://raw.githubusercontent.com/novaestellar/novagemini/main/docker-compose.yml
ADMIN_TOKEN=your-admin-token docker compose up -d
```

The image is distroless and runs as nonroot (uid 65532) — with a bind mount the host directory must be writable, otherwise you get `unable to open database file (14)`.

### Option 3: From source

```bash
git clone https://github.com/novaestellar/novagemini
cd novagemini
go build -o novagemini .
./novagemini --port 8083 --admin-token your-admin-token
```

### What you should see

```
novagemini v4.22.0
  Listening:   http://0.0.0.0:8083
  Base URL:    http://localhost:8083/v1
  API key:     sk-gemini-XX...XXXX  (mutable in admin UI)
  Admin UI:    http://localhost:8083/admin  (token auth)
  DB:          ./data/gemini.db
  Models:      [gemini-3.5-flash-lite gemini-3.6-flash]
  Cookie:      none (anonymous)
  Proxy:       none
  Impersonate: chrome_146
  Tokenizer:   tiktoken cl100k_base
  Rate limits: concurrency=5 / RPM=30 / RPH=80
  Retry:       3x / 2s
```

The API key is generated on first boot (masked in the banner — the full value is on the admin panel's Settings page).

### Environment variables (optional)

Only three variables are read at startup; changing them requires a restart.

```bash
cp .env.example .env
```

| Variable | Description |
|----------|-------------|
| `ADMIN_TOKEN` | Admin panel login token. Empty = no auth (only acceptable when bound to `127.0.0.1`) |
| `API_KEY` | Locks the `/v1/*` key (cannot be changed from the panel) |
| `SQL_DSN` | MySQL / PostgreSQL connection string; unset means SQLite |

Port, bind address and DB path are **not** environment variables — use `config.json` or CLI flags.

---

## 💻 Usage

### curl

```bash
curl http://localhost:8083/v1/chat/completions \
  -H "Authorization: Bearer sk-gemini-..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.6-flash",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

> **Windows PowerShell:** `curl` is an alias for `Invoke-WebRequest` and will re-interpret the JSON quotes. Use `curl.exe` with `--%`.

### OpenAI Python SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8083/v1",
    api_key="sk-gemini-...",  # see the admin panel's Settings page
)
resp = client.chat.completions.create(
    model="gemini-3.6-flash",
    messages=[{"role": "user", "content": "Explain quantum entanglement"}],
)
print(resp.choices[0].message.content)
```

### Client setup

Any OpenAI-compatible client (Cherry Studio / ChatBox / Open WebUI / Dify / Cursor / …) uses the same values:

| Field | Value |
|-------|-------|
| Base URL | `http://localhost:8083/v1` (some clients want just `http://localhost:8083` and append `/v1` themselves) |
| API Key | the `sk-gemini-...` from the admin panel |
| Model | `gemini-3.6-flash` |

**Codex CLI** uses `/v1/responses` — point its base URL at `http://localhost:8083/v1`.

**Gemini CLI is not supported**: it wants Google's native `/v1beta/models/{model}:generateContent`, and this project only exposes OpenAI-shaped endpoints.

There is also an unauthenticated health check at `GET /` returning `{"status":"ok","version":...,"models":[...]}` for liveness probes.

### Image generation / image editing / music / canvas

These all go through `/v1/chat/completions` like a normal chat — just describe what you want. Artifact bytes come back as base64 data URLs in `content` (`![image](data:image/png;base64,...)` for images, `[audio](data:audio/mpeg;base64,...)` for audio); markdown-capable clients render them directly.

```bash
# text-to-image
curl http://127.0.0.1:8083/v1/chat/completions \
  -H "Authorization: Bearer sk-gemini-..." -H "Content-Type: application/json" \
  -d '{"model":"gemini-image","messages":[{"role":"user","content":"an orange cat wearing an astronaut helmet"}]}'
```

**Image-to-image** (one image + one edit instruction):

```json
{
  "model": "gemini-image",
  "messages": [{
    "role": "user",
    "content": [
      {"type": "text", "text": "Change the object's color to blue"},
      {"type": "image_url", "image_url": {"url": "data:image/png;base64,<...>"}}
    ]
  }]
}
```

External links are deliberately not returned — Gemini's artifact URLs require the cookie to download, so handing the link to a client would just 403. The server fetches the bytes and converts them to base64. Artifact base64 does **not** count toward `completion_tokens` (one image would be over a million tokens; billing downstreams on it would be absurd).

### MCP (web_search tool)

The same process and the same port also host an MCP server at `/mcp`, exposing the web app's search as a `web_search` tool. No separate process; anonymous works.

```json
{
  "mcpServers": {
    "gemini-search": {
      "url": "http://localhost:8083/mcp",
      "headers": { "Authorization": "Bearer sk-gemini-your-key" }
    }
  }
}
```

```bash
# Claude Code
claude mcp add --transport http gemini-search http://localhost:8083/mcp --header "Authorization: Bearer sk-gemini-your-key"
```

`web_search(query)` returns a search-verified answer followed by a `Sources:` list.

---

## 🧩 Models

| Model | Description | Requires |
|-------|-------------|----------|
| `gemini-3.6-flash` | All-round, default | anonymous |
| `gemini-3.5-flash-lite` | Fastest, lightweight | anonymous |
| `gemini-3.1-pro` | Most capable; emits a reasoning chain | cookie |
| `gemini-3.8-flash` | Newer Flash | **paid** account cookie (free accounts get downgraded to 3.5 Flash-Lite) |
| `gemini-4-argon` | 4 Argon (complex tasks) | **paid / Ultra** account cookie |
| `gemini-4-argon-low` | 4 Argon, reasoning depth low / fast | paid / Ultra |
| `gemini-4-argon-medium` | 4 Argon, reasoning depth medium / balanced | paid / Ultra |
| `gemini-4-argon-high` | 4 Argon, reasoning depth high / deep | paid / Ultra |
| `gemini-3.6-flash-thinking` | 3.6 Flash, extended thinking | cookie |
| `gemini-3.5-flash-lite-thinking` | 3.5 Flash-Lite, extended thinking | cookie |
| `gemini-3.1-pro-thinking` | 3.1 Pro, extended thinking | cookie |
| `gemini-3.8-flash-thinking` | 3.8 Flash, extended thinking | paid cookie |
| `gemini-4-argon-thinking` | 4 Argon, extended thinking (upstream calls this the High tier) | paid / Ultra |
| `gemini-image` | Image generation / editing (Nano Banana), base64 | cookie |
| `gemini-music` | Music (Lyria, ~30s), base64 | cookie |
| `gemini-canvas` | Canvas: interactive HTML document, returned inline | cookie |
| `gemini-video` | Video (async, tens of seconds to minutes), base64 MP4 | Pro / paid |

`gemini-3.7-flash` (and `-thinking`) are aliases of `gemini-3.8-flash` — Google bumped that hex ID in place.

Without a cookie, `/v1/models` returns only the first two. Selecting `gemini-3.1-pro` fails immediately with an explanation, because an anonymous request is always silently downgraded to 3.5 Flash-Lite — better to fail at selection time than to hand a client a "successful" reply that isn't actually Pro.

`-thinking` is the web UI's "extended thinking" toggle, orthogonal to the model — any of the base models can enable it. It only takes effect when signed in; anonymous requests have the flag silently ignored, so it isn't exposed without a cookie.

---

## 🧠 Reasoning chains (`reasoning_content`)

`gemini-3.1-pro` emits a reasoning block before every answer. It is surfaced on the de-facto `reasoning_content` field, which new-api, Cherry Studio and ChatBox render as a collapsible "thinking" block.

```json
{"choices":[{"message":{
  "role":"assistant",
  "content":"The core rule of this classic puzzle is...",
  "reasoning_content":"**Defining the Constraints**\n\nI've successfully defined..."
}}],
 "usage":{"prompt_tokens":26,"completion_tokens":337,"reasoning_tokens":77,"total_tokens":363}}
```

Streaming pushes `delta.reasoning_content`, and the whole reasoning block finishes before the main text starts (that is the upstream order), so it feels the same as the web app.

`reasoning_tokens` is counted **separately** and is **not** included in `completion_tokens`: clients collapse the reasoning block by default, so counting it would bill users for invisible output. Add the two yourself if you need to bill for it.

---

## 🍪 Cookies (optional — unlocks more)

With a Google account cookie, requests run signed in and additionally unlock `gemini-3.1-pro`, the extended-thinking variants, image/video reading, image generation / editing / music / canvas / video, and longer context.

### How to export

1. Sign in to `gemini.google.com` in a browser
2. DevTools (F12) → Application → Cookies → `https://gemini.google.com`
3. Copy: `SID` / `HSID` / `SSID` / `APISID` / `SAPISID` / `__Secure-1PSID` / `__Secure-1PSIDTS`
4. Paste into the panel's **Accounts → Add account**, or write a `cookie.txt` and pass `--cookie-file cookie.txt`

```
SID=...; HSID=...; SSID=...; APISID=...; SAPISID=...; __Secure-1PSID=...; __Secure-1PSIDTS=...
```

JSON form `{"cookie": "SID=...; ...", "sapisid": "..."}` also works. Requests carrying `SAPISID` get an auto-computed `SAPISIDHASH` authorization header, so that field is required.

> **Export cookies with Firefox.** Recent Chrome enables Device Bound Session Credentials, which binds the session to that machine — `*PSIDTS` cannot be renewed and dies within half an hour to a few hours. Firefox has no such binding.

### How the account pool works

- Each request picks an `enabled` account **least-recently-used first**, advancing rotation
- **Automatic renewal**: nearly every upstream response refreshes `SIDCC` / `__Secure-1PSIDCC` / `__Secure-3PSIDCC`, which are written back; every 10 minutes it also calls `RotateCookies` to re-mint `__Secure-1PSIDTS` (the ~30-minute ticket)
- **Accounts stick to their exit**: once an account has used a proxy it stays bound to it until that exit becomes unusable — one account hitting from dozens of IPs is a textbook account-sharing signal
- **A dead account is switched out automatically**, without taking the whole request down
- Only **401/403** count as a cookie failure; network errors, proxy failures and Google's 302 blocks are excluded

> "Last success" only means a request involving this cookie succeeded — it does **not** prove the cookie is still valid. When a cookie expires Gemini does not error; it just treats you as anonymous. Use the panel's "Check" button to verify.

---

## 🛡️ Proxy pool

**Why you need it:** a single IP gets redirected to `google.com/sorry/index` after enough burst traffic. Measured, that number is **80–180**, and it depends on connection strategy, exit quality and request pacing.

| Exit | Connection strategy | Concurrency | Pacing | Successful requests before block |
|------|--------------------|-------------|--------|----------------------------------|
| Residential | Pooled connections | 10 | none | 151 / 172 / 177 |
| Residential | Pooled connections | 3 | none | 103 / 111 |
| Residential | New connection each | 10 | none | 106 / 109 |
| Residential | New connection each | 1 | 24s apart | 81 / 166 |
| Static | Pooled connections | 10 | none | 188 |
| Static | Pooled connections | 1 | 10/min | **800 with no block** |

**Gentle pacing beats everything else**: on the same static IP, bursting got blocked at 188, while 10/min ran 800 requests over 110 minutes with zero blocks. That is why the default `per_ip_rph=80` is a conservative floor.

**Once blocked it is a hard block, recovering after about two hours.** The proxy pool's default 120-minute circuit-breaker cooldown comes from that measurement.

**The fix**: add proxies on the panel's Proxies page. Each proxy is an independent IP slot with its own concurrency / RPM / RPH quota. N proxies = N times the capacity.

Supported protocols:

```
http://user:pass@host:port
https://user:pass@host:port
socks5://user:pass@host:port
socks5h://user:pass@host:port     # remote DNS, bypasses local DNS poisoning
```

Scheduling rules:

- With proxies configured it **never falls back to direct** (so a full pool can't burn the host IP)
- 5 failures trips the circuit-breaker (resettable from the panel)
- All proxies full → HTTP 429 (no upstream quota consumed)
- **Does not read `HTTPS_PROXY` / `ALL_PROXY`** — proxies come only from the pool, otherwise a stray exported variable on the host would silently change the exit IP while the panel still shows "direct"

---

## 🎭 Fingerprinting

- **Direct (no proxy)** uses `bogdanfinn/tls-client`: TLS handshake, HTTP/2 SETTINGS frames and ALPS all match real Chrome 146
- **Through a proxy** switches to stdlib `net/http` + `http.ProxyURL` (best compatibility), but application-layer headers (`Sec-CH-UA` / `Sec-Fetch-*` / `User-Agent`) are still spoofed as Chrome 146

> Through a proxy the TLS fingerprint is ours, not the exit node's — HTTP CONNECT only builds a tunnel; TLS is negotiated end-to-end with Google. This does not affect the block threshold: in a head-to-head run tls-client reached 111 and Go stdlib 103, an 8-request gap, while exit-to-exit variance under identical config reached 36%.

**A measured surprise**: a naive SDK call (e.g. Python `urllib`) gets HTTP 429 when rate-limited; a Chrome-spoofed client gets HTTP 302 to `google.com/sorry/index`. Both are the same IP blacklist, but the 302 proves Google really sees us as a browser.

---

## 🖥️ Admin panel

`http://localhost:8083/admin`, authenticated with `--admin-token`.

| Page | Contents |
|------|----------|
| **Overview** | 24h KPIs + dual-axis request-volume/P50-latency chart + model/proxy breakdown + per-IP limit usage + one-click connectivity diagnostic |
| **Requests** | Detail list (metadata only, no prompt/response content), status/model filters + pagination |
| **Proxies** | Runtime CRUD + enable/disable + failure circuit-breaker (each proxy is an independent IP slot) |
| **Accounts** | Import multiple Google signed-in accounts + one-click "Check" + automatic renewal. Only redacted summaries shown |
| **Settings** | Runtime config form (applies on save) + API key rotation + read-only deployment config |

The panel frontend is a single HTML file with Chart.js embedded in the binary — **no CDN**, so it works on air-gapped networks.

Reverse-proxying under a subpath needs no extra config: every URL in the panel is relative. Point `https://example.com/gemini/` at this service's `/` and open `https://example.com/gemini/admin`.

---

## ⚙️ Configuration

Two places, split by "does changing it need a restart".

### Runtime config → panel Settings page (applies immediately)

Stored in the database; **higher priority than `config.json` and CLI flags**.

| Item | Description |
|------|-------------|
| Default model | Used when a client sends no `model` |
| Per-slot concurrency / RPM / RPH | Rate limits, 0 = unlimited |
| Prompt byte limit | Default 128000. Over: with a cookie it becomes a text attachment; without one it returns 400 `context_length_exceeded`. Never silently truncated |
| `multi_turn` | Off by default. Uses native `conversation_id` server-side continuation so long chats stop hitting the single-request byte wall |
| `auto_delete_conversation` | Off by default. Deletes the gemini.google.com conversation after each result |
| `temporary_chat` | Off by default. The conversation never enters the account's list and isn't trained on — cleaner than auto-delete |
| `anon_first` | Off by default. Requests that don't need signed-in capability go anonymous, saving account quota |
| Retry count / retry delay / upstream timeout | — |
| Detail retention days | Only details expire; aggregates are kept forever |
| TLS fingerprint | `chrome_146` (default) / `chrome_144` / `chrome_133` / `firefox_147` / `safari_16_0` / `safari_ios_17_0` |
| Gemini `bl` version | Upstream frontend version, update when it expires |
| Log requests | — |

All values are range-validated **server-side** (e.g. `retry_attempts` only accepts 1-10, timeout 5-600 seconds); invalid values are rejected with an explanation.

### Deployment config → requires a restart

| Item | Where |
|------|-------|
| Listen port | `ports` + `command: --port` |
| Database path | `volumes` + `command: --db` |
| `ADMIN_TOKEN` | environment |
| `API_KEY` | environment; setting it locks the key |

`--proxy` and `--cookie-file` are **seeding parameters**, not a second config system: they import values into the proxy/account pools at startup (deduplicated by URL and cookie content), managed from the panel afterward.

### CLI flags

| Flag | Description |
|------|-------------|
| `--port` | Listen port, default 8083 |
| `--config` | Path to `config.json` |
| `--db` | SQLite path, default `./data/gemini.db` |
| `--admin-token` | Panel login token; empty = no auth (only acceptable on `127.0.0.1`) |
| `--api-key` | Locks the `/v1/*` key, equivalent to `API_KEY` |
| `--cookie-file` | Import cookies from a file into the account pool at startup |
| `--proxy` | Import this proxy into the proxy pool at startup |
| `--impersonate` | TLS fingerprint profile |
| `--version` | Print version and exit |

**Priority**: panel changes > CLI flag / `config.json` > built-in defaults.

Without `--config` it looks for `config.json` in the current directory, then `$HOME/.config/novagemini/config.json`. Copy `config.example.json` to `config.json` as a starting template.

---

## 📡 OpenAI protocol coverage

| Path | Status | Notes |
|------|--------|-------|
| `POST /v1/chat/completions` | ✅ | True streaming: each upstream frame is forwarded incrementally, plus a `: keepalive` SSE comment every 15 s while upstream is silent (clients ignore comments per the SSE spec). Degrades to buffer-then-send when `tools` are present |
| `POST /v1/responses` | ✅ | OpenAI Responses API (used by Codex CLI). Not truly streaming |
| `GET /v1/models` | ✅ | 2 models anonymous, 3 with a cookie |
| `GET /` | ✅ | Unauthenticated health check |
| `/v1/videos` | ✅ | Async video: POST to create, GET to poll, GET to download MP4 |
| `/mcp` | ✅ | MCP server (Streamable HTTP) exposing `web_search` |
| Function calling | ⚠️ | **Prompt-level** implementation (the model emits a `tool_call` block that is parsed), not a real protocol layer |
| `tool_choice` | ⚠️ | `none` omits tool definitions entirely; `required` strengthens the wording but cannot truly force |
| Vision / image input | ⚠️ | Works with a cookie; 400 when anonymous |
| `stream_options.include_usage` | ✅ | Appends a usage-only chunk after `finish_reason` |
| usage token counts | ✅ | tiktoken cl100k_base, same accounting as the panel's requests table |
| `n > 1` | ❌ | Returns 400. Upstream only yields one candidate |
| Sampling params | ➖ | `temperature` / `top_p` / `max_tokens` / `stop` / `seed` etc. accepted and ignored |
| `response_format` / `logprobs` | ➖ | Not implemented; accepted and ignored |
| Audio input | ❌ | Sending `input_audio` returns 400 |
| `/v1/embeddings`, `/v1/images/*` | ❌ | Not implemented; returns 404 |
| `/v1beta/models/...` (Gemini native) | ❌ | Not implemented; only OpenAI-shaped endpoints are exposed |

**Token counts** use tiktoken cl100k_base (accurate for Chinese and English alike), not a `len/4` estimate. The Gemini web app returns no real token counts, so expect deviation within ±20%.

---

## 📁 Project structure

```
main.go                    one line: app.Run(); entry stays at the repo root
internal/app/              all implementation
  app.go                   flag parsing + route registration + startup
  config.go                config loading + defaults
  client.go                dual client: tls-client (chrome146) vs stdlib (proxied)
  gemini.go                model table + 80-slot payload + model headers + StreamGenerate + wrb.fr parsing
  xsrf.go                  XSRF token required when signed in: fetch + cache per cookie + self-healing
  messages.go              OpenAI messages -> prompt, tool_call parsing
  server.go                /v1/* + rate-limit entry + param validation + metrics
  sse.go                   SSE writer (lazy headers; still returns 502 JSON on failure)
  ratelimit.go             per-IP-slot concurrency / RPM / RPH limiting
  tokenizer.go             tiktoken cl100k_base singleton
  apikey.go                API key (flag-locked / panel-rotatable dual track)
  db.go                    schema: sessions / requests / accounts / kv
  dbdialect.go             SQLite / MySQL / PostgreSQL dialects
  proxy.go                 proxy pool CRUD + capacity scheduling + circuit-breaker
  cookie_pool.go           account pool data layer (CRUD + LRU pick + health write-back)
  rotate.go                session keep-alive (RotateCookies: 1PSIDTS sentinel + SIDCC iframe)
  upload.go                attachment upload (content-push two-step resumable)
  context_file.go          long conversation -> text attachment
  vision.go                image input: data: URL / http(s) -> pending upload
  media.go                 media artifact retrieval (image CDN 302 following + music/video polling)
  media_store.go           artifact persistence
  videos.go                /v1/videos async video jobs
  mcp.go                   /mcp MCP server (web_search tool)
  search.go                grounding source parsing
  bl.go                    upstream frontend version (bl) auto-follow
  scheduler.go             hourly/daily aggregation + expired-detail cleanup
  runtime.go               runtime config snapshot (panel changes take effect immediately)
  admin.go                 /admin/api/* auth + REST
  admin_cookies.go         account pool admin REST (redacted before returning)
  admin_ui.go              embeds admin_ui/
  admin_ui/                panel frontend (single page + Chart.js, shipped in the binary)
Dockerfile                 multi-stage (alpine builder -> distroless runtime)
docker-compose.yml         single container, ghcr image by default, sqlite volume
.github/workflows/         docker.yml pushes the image / release.yml tags and ships binaries
```

---

## 🚧 Known limits

- **Per-IP ceiling**: a burst gets redirected to the sorry page after 80–180 requests. Gentle pacing barely reaches it. Scale capacity with a proxy pool
- **Video / deep research**: video is gated to Pro accounts; deep research is a multi-step async flow and is not implemented
- **Function calling**: prompt-level; the model does not always follow the format
- **Long context has two walls**: ~130k bytes for the request body, ~160k bytes for attachments. A cookie only raises usable length from ~130k to ~160k
- **The account pool does not auto-retire bad accounts**: failures are written back, but an account is never auto-disabled — stop it manually from the panel
- **Chrome-exported cookies may not renew**: see above; export from Firefox
- **Half of the streaming is fake**: `/v1/responses` and chat requests carrying `tools` are buffer-then-send

---

## 🔧 Troubleshooting

| Symptom | Likely cause | What to do |
|---------|--------------|------------|
| We return 429 | All IP slots are at their concurrency/RPM/RPH limit — **not** a Google block, no upstream quota consumed | Add proxies, or raise limits on the Settings page |
| Panel diagnostic shows 302 -> `google.com/sorry/index` | This exit IP is blocked (after 80–180 requests) | Change or add exits. It is a hard block, not probabilistic — retrying in place is pointless |
| Occasional empty response, panel logs an upstream rejection | Transient upstream rejection (1155); no predictable threshold | Resend; it usually works. Lowering RPM does not help |
| All requests time out | The host cannot reach `gemini.google.com` | Configure a proxy. Note it does **not** read `HTTPS_PROXY` |
| Exits immediately with `unable to open database file (14)` | Container runs as nonroot (uid 65532) while the bind-mounted host dir is root-owned | Use a named volume, or `sudo chown -R 65532:65532 ./data` |
| Selecting `gemini-3.1-pro` errors immediately | Without a cookie it is not exposed — deliberate | Add a cookie |
| All requests 502 after adding a cookie | Cookie is invalid, XSRF cannot be fetched | Re-export the cookie. Tell-tale: requesting `gemini-3.1-pro` returns 3.5 Flash-Lite |
| Cookie dies after ~30 minutes, keep-alive does not help | Chrome device-bound session; `__Secure-1PSIDTS` renewal returns 401 | Re-login in Firefox and export again |
| Panel unreachable / 401 | `--admin-token` (or `ADMIN_TOKEN`) mismatch | An empty token disables auth, acceptable only on `127.0.0.1` |
| Image generation occasionally 502 `artifact retrieval failed` | On very long generations the CDN rejects the just-finished download link | One retry is built in; resend if it still fails. Faster generations are more reliable |
| Docker on the default bridge network returns empty content | Google rejects some NAT ranges | Try `network_mode: host` to confirm |

---

## ❓ FAQ

**How is this different from Google's official Gemini API? Do I need an API key?**
No. This project does not wrap `generativelanguage.googleapis.com`; it replicates the `gemini.google.com` web protocol directly, so there is no API key and no paid quota. The trade-off is that capability follows the web app.

**Will I get banned? Is banning per account or per IP?**
Per **exit IP + TLS fingerprint**, not per account. On the same exit a burst gets redirected to the sorry page after 80–180 requests and recovers after about two hours; gentle pacing (say 10/min) ran 800 requests without a block. A paid (Google AI Pro) plan does not raise that threshold.

**What is the difference between anonymous, free and paid accounts?**

| | Anonymous | Free cookie | Paid / Ultra cookie |
|---|---|---|---|
| Text models | 3.6 Flash, 3.5 Flash-Lite | +3.1 Pro, extended thinking | +3.8 Flash, 4 Argon |
| Image / video reading | ❌ | ✅ | ✅ |
| Image gen / editing / music / canvas | ❌ | ✅ | ✅ |
| Video generation | ❌ | usually no | ✅ |

**Why does every message add a conversation on `gemini.google.com`?**
By default every request is an independent conversation. Three options: enable **temporary chat** (recommended — no conversation is created at all), enable **auto delete conversation**, or leave it and clean up manually in the web UI.

**Can I use this as the backend for Cursor / Dify-style agent tools?**
Yes (OpenAI-compatible), and plain chat or coding works fine. But the web app caps a single request at ~130k bytes (~160k with a cookie), so agent tools that push tens of thousands of characters of system prompt and history tend to answer off-target — an inherent web-app limitation. Function calling is prompt-level, so the model does not always follow the format.

**Which model does image generation use? Can it do 2K?**
`gemini-image` goes through the web app's image tool slot and does not pin a model: whatever backend the web app currently uses (e.g. Nano Banana), this follows automatically. Resolution is locked by the web app at about one megapixel; there is no 2K.

**Why does streaming become a single output behind Dify / a reverse proxy?**
Plain chat is truly streaming; requests with `tools` and `/v1/responses` are buffer-then-send. Responses already carry `X-Accel-Buffering: no` — if it is still buffered behind Nginx, check the proxy's own buffering settings.

**Does it support MySQL / PostgreSQL?**
Yes. Set `SQL_DSN`; unset means SQLite. Tables are created automatically and all three backends share one schema.

---

## 🔒 Privacy

- Prompts and responses are **never** stored — only metadata: model, proxy ID, latency, token counts, status code, error type
- Legacy `prompt_preview` / `response_preview` columns are dropped automatically when migrating from older versions
- Token counts use the tiktoken-go cl100k_base BPE tokenizer, not a `len/4` estimate

---

## 📄 License

MIT — see [LICENSE](LICENSE).

## 🙏 Credits

- [bogdanfinn/tls-client](https://github.com/bogdanfinn/tls-client) — Chrome-fingerprint TLS library
- [pkoukk/tiktoken-go](https://github.com/pkoukk/tiktoken-go) — BPE tokenizer
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) — pure-Go SQLite (CGO-free)

