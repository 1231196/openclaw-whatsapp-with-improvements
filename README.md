# openclaw-whatsapp

WhatsApp bridge for [OpenClaw](https://openclaw.ai) agents. Single Go binary — start, scan QR, done.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/0xs4m1337/openclaw-whatsapp/main/install.sh | bash
```

Or build from source:

```bash
make build
sudo make install
```

## Quick Start

```bash
# Start the bridge
openclaw-whatsapp start

# Open browser, scan QR
open http://localhost:8555/qr

# Send a message
openclaw-whatsapp send "+1234567890" "Hello from OpenClaw!"
```

## Run as Service (systemd)

```bash
make install-service
systemctl --user start openclaw-whatsapp
journalctl --user -u openclaw-whatsapp -f
```

## Features

- **Group creation** — create WhatsApp groups from the HTTP API
- **Always-on connection** — auto-reconnect with exponential backoff
- **REST API** — send text/files, read messages, search, list chats/contacts
- **QR Web UI** — scan from browser, auto-refreshes every 3s
- **Webhook delivery** — incoming messages POST to your endpoint
- **Full-text search** — SQLite FTS5 across all messages
- **Media handling** — auto-downloads images, videos, audio, documents
- **Agent mode** — trigger OpenClaw agents on incoming messages (command or HTTP)
- **Message deduplication** — no duplicate webhooks
- **Single binary** — pure Go, no CGO, cross-compiles everywhere

---

## Configuration

Create `config.yaml` in the working directory or use environment variables (`OC_WA_` prefix):

```yaml
port: 8555
data_dir: ~/.openclaw-whatsapp
webhook_url: http://localhost:1337/webhook/whatsapp
webhook_filters:
  dm_only: false
  ignore_groups: []
auto_reconnect: true
reconnect_interval: 30s
log_level: info
```

Environment variables: `OC_WA_PORT`, `OC_WA_WEBHOOK_URL`, `OC_WA_DATA_DIR`, etc.

---

## Agent Mode

Agent mode lets the bridge trigger an AI agent whenever a WhatsApp message arrives. The flow:

```
WhatsApp message → Bridge receives it → Triggers command or HTTP POST → Agent processes → Replies via POST /reply
```

### Configuration

```yaml
agent:
  enabled: true
  mode: "command"                              # "command" or "http"
  command: "./scripts/wa-notify.sh '{name}' '{message}' '{from}'"
  http_url: ""                                 # POST endpoint for "http" mode
  reply_endpoint: "http://localhost:8555/reply" # so agent knows where to reply
  ignore_from_me: true                         # don't trigger on own messages
  dm_only: true                                # only trigger on DMs, not groups
  timeout: 30s                                 # command/HTTP timeout
```

Environment variables: `OC_WA_AGENT_ENABLED`, `OC_WA_AGENT_MODE`, `OC_WA_AGENT_COMMAND`, `OC_WA_AGENT_HTTP_URL`, `OC_WA_AGENT_REPLY_ENDPOINT`, `OC_WA_AGENT_TIMEOUT`, `OC_WA_AGENT_SYSTEM_PROMPT`, `OC_WA_AGENT_ALLOWLIST`, `OC_WA_AGENT_BLOCKLIST`.

### System Prompt

The `system_prompt` field controls the agent's personality and behavior. It's passed to the agent command via the `OC_WA_SYSTEM_PROMPT` environment variable (not as a command argument — avoids shell escaping issues).

```yaml
agent:
  enabled: true
  mode: "command"
  command: "./scripts/wa-notify.sh '{name}' '{message}' '{from}'"
  system_prompt: |
    You are a helpful customer service agent for Acme Corp.
    Be friendly, professional, and concise.
    If asked about pricing, direct them to https://acme.com/pricing
```

The system prompt can include tool instructions — if your OpenClaw instance has Google Calendar, messaging, or other integrations, the agent can use them directly. See `examples/setupclawd-agent.yaml` for a real-world example that books calendar meetings and sends Telegram notifications.

### Allowlist / Blocklist

Restrict which phone numbers the agent responds to:

```yaml
agent:
  allowlist: ["971586971337", "1234567890"]  # only respond to these numbers
  blocklist: ["spammer123"]                   # never respond to these
```

- **Allowlist** — if non-empty, only these numbers get responses (empty = respond to all)
- **Blocklist** — these numbers are always ignored
- Numbers can be with or without the `@s.whatsapp.net` suffix

Environment variables (comma-separated): `OC_WA_AGENT_ALLOWLIST=971586971337,1234567890`, `OC_WA_AGENT_BLOCKLIST=spammer123`.

### Command Mode

Runs a shell command with template variables substituted:

| Variable | Description |
|----------|-------------|
| `{from}` | Sender JID (e.g. `971558762351@s.whatsapp.net`) |
| `{name}` | Sender push name |
| `{message}` | Message text |
| `{chat_jid}` | Chat JID |
| `{type}` | Message type (`text`, `image`, etc.) |
| `{is_group}` | `"true"` or `"false"` |
| `{group_name}` | Group name (empty for DMs) |
| `{message_id}` | WhatsApp message ID |

When the command runs, a **typing indicator** is shown in the chat until the command completes.

### HTTP Mode

POSTs a JSON payload to `http_url`:

```json
{
  "from": "971558762351@s.whatsapp.net",
  "name": "Sam",
  "message": "Hey!",
  "chat_jid": "971558762351@s.whatsapp.net",
  "type": "text",
  "is_group": false,
  "group_name": "",
  "message_id": "ABC123",
  "timestamp": 1708387200,
  "reply_endpoint": "http://localhost:8555/reply"
}
```

The agent can use the included `reply_endpoint` to send a response.

### Reply Endpoint

Agents reply via `POST /reply`:

```bash
curl -X POST http://localhost:8555/reply \
  -H "Content-Type: application/json" \
  -d '{"to": "971558762351@s.whatsapp.net", "message": "Hello!", "quote_message_id": "ABC123"}'
```

| Field | Required | Description |
|-------|----------|-------------|
| `to` | Yes | Recipient JID |
| `message` | Yes | Reply text |
| `quote_message_id` | No | Message ID to quote-reply |

---

## Auto-Reply with OpenClaw

The most powerful setup: every incoming WhatsApp DM triggers an isolated OpenClaw agent that reads conversation history and replies automatically.

### Architecture

```
WhatsApp DM
  → Bridge (agent mode, command)
  → wa-notify.sh (enqueue + dedupe)
  → wa-notify-worker.sh (background, single-instance)
  → Fetches last 10 messages from bridge API for context
  → openclaw agent (processes message)
  → openclaw-whatsapp send <JID> <reply>
  → WhatsApp reply sent
```

Key design decisions:
- **Queue-based** — fast enqueue, async processing, no blocked bridge
- **Deduplication** — message IDs tracked to prevent double-replies
- **Single worker** — file-locked, sequential processing, no race conditions
- **Conversation context** — fetches last 10 messages so the agent has history

### Step-by-Step Setup

#### 1. Install openclaw-whatsapp

```bash
curl -fsSL https://raw.githubusercontent.com/0xs4m1337/openclaw-whatsapp/main/install.sh | bash
```

#### 2. Configure agent mode

Create `config.yaml`:

```yaml
port: 8555
data_dir: ~/.openclaw-whatsapp
auto_reconnect: true
reconnect_interval: 30s
log_level: info

agent:
  enabled: true
  mode: "command"
  command: "/path/to/wa-notify.sh '{name}' '{message}' '{from}'"
  ignore_from_me: true
  dm_only: true
  timeout: 30s
```

#### 3. Copy the relay scripts

```bash
cp scripts/wa-notify.sh /usr/local/bin/wa-notify.sh
cp scripts/wa-notify-worker.sh /usr/local/bin/wa-notify-worker.sh
chmod +x /usr/local/bin/wa-notify.sh /usr/local/bin/wa-notify-worker.sh
```

Update the paths in the scripts and config:
- In `wa-notify.sh`: update the path to `wa-notify-worker.sh`
- In `wa-notify-worker.sh`: update the `PATH` export to include your `openclaw` binary
- In `config.yaml`: update the `command` path

#### 4. Start the bridge

```bash
openclaw-whatsapp start -c config.yaml
```

#### 5. Scan QR

Open http://localhost:8555/qr in your browser and scan with WhatsApp on your phone.

#### 6. Test it

Send a WhatsApp message to your linked number from another phone. You should see:
- Bridge logs: `agent trigger: command mode`
- A reply appearing within a few seconds

### The Relay Scripts

The auto-reply system uses a **queue-based architecture** with two scripts:

#### wa-notify.sh (Enqueuer)

Fast, non-blocking script called by the bridge:

1. Receives `name`, `message`, `JID`, and `message_id` from the bridge
2. Deduplicates by message ID (prevents double-processing)
3. Appends message to a JSONL queue file
4. Spawns the worker in background (if not already running)
5. Exits immediately — bridge doesn't wait

#### wa-notify-worker.sh (Processor)

Single-instance worker that processes the queue:

1. Acquires a file lock (only one worker runs globally)
2. Reads messages from queue one at a time
3. Fetches last 10 messages from `GET /chats/{jid}/messages?limit=10` for context
4. Calls `openclaw agent` to process and reply
5. Loops until queue is empty

This design ensures:
- **Fast bridge response** — enqueue returns instantly
- **No duplicate replies** — message ID deduplication
- **Sequential processing** — one reply at a time, no race conditions
- **Crash resilience** — queue persists across restarts

### Customization

Edit `wa-notify-worker.sh` to customize:
- **PATH** — set to include your `openclaw` binary location
- **Timeout** — default 45s hard timeout per message
- **Agent** — uses `--agent main` by default

Data files are stored in `/tmp/openclaw-wa-agent/` by default (override with `OC_WA_AGENT_DATA_DIR`).

---

## Example Configs

See the `examples/` directory for ready-to-use configurations:

- **`examples/setupclawd-agent.yaml`** — A real-world customer-facing agent for SetupClawd (OpenClaw deployment service). Demonstrates system prompt with pricing info, Google Calendar booking via Composio, Telegram lead notifications, and bilingual (Arabic/English) support.

---

## Security Guide for Public-Facing WhatsApp Agents

Running an AI agent that auto-replies on WhatsApp requires careful security configuration. Here's what to consider:

### 1. Rate Limiting

WhatsApp spammers can trigger expensive API calls. Implement rate limiting in your relay script:

```bash
# Simple rate limit: max 10 messages per minute per sender
RATE_FILE="/tmp/wa-rate-${JID}"
COUNT=$(cat "$RATE_FILE" 2>/dev/null || echo 0)
if [ "$COUNT" -gt 10 ]; then
  exit 0  # silently drop
fi
echo $((COUNT + 1)) > "$RATE_FILE"
```

Or implement rate limiting at the HTTP endpoint level if using HTTP mode.

### 2. Allowlist / Blocklist

Restrict which phone numbers the agent responds to:

```bash
# In your relay script
ALLOWED="+971558762351 +1234567890"
if ! echo "$ALLOWED" | grep -q "${JID%%@*}"; then
  exit 0
fi
```

### 3. DM Only

Set `dm_only: true` in your config to ignore group messages entirely. Groups can generate massive volumes and expose your agent to unknown users.

```yaml
agent:
  dm_only: true
```

### 4. Ignore Own Messages

Always set `ignore_from_me: true` to prevent infinite loops where the agent triggers on its own replies:

```yaml
agent:
  ignore_from_me: true
```

### 5. Message Content Filtering

Don't blindly pass message content to shell commands. The relay script should sanitize inputs:

- Never use message content in `eval` or unquoted shell expansions
- JSON-escape all user inputs (wa-notify.sh does this with Python)
- Consider filtering out messages that look like prompt injection attempts

### 6. Timeout Limits

Set aggressive timeouts to prevent runaway processes:

```yaml
agent:
  timeout: 30s  # kill the command after 30 seconds
```

The agentTurn in OpenClaw also has `timeoutSeconds: 20` as a backstop.

### 7. Cost Control

Auto-replies can get expensive fast. Mitigate costs:

- Use cheaper models (`anthropic/claude-sonnet-4-5` instead of Opus)
- Set `dm_only: true` to avoid group spam
- Implement rate limiting (see above)
- Monitor usage through your OpenClaw dashboard

### 8. Webhook Authentication

If using HTTP mode, add authentication to your endpoint:

```yaml
agent:
  mode: "http"
  http_url: "https://your-server.com/whatsapp-webhook"
```

Your HTTP endpoint should validate requests with a shared secret header or IP allowlist.

### 9. Data Privacy

- All messages are stored in a local SQLite database at `~/.openclaw-whatsapp/`
- The bridge runs locally — no data leaves your machine unless you configure webhooks
- Conversation history is passed to the AI model via the relay script
- Consider data retention policies and GDPR compliance if serving EU users

### 10. Network Security

- The bridge REST API has **no authentication** by default
- Bind to `localhost` only (default) — don't expose port 8555 to the internet
- If you need remote access, put it behind a reverse proxy with auth

---

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/status` | Connection status, uptime, version |
| `GET` | `/qr` | QR code web page for device linking |
| `GET` | `/qr/data` | QR code as base64 PNG (JSON) |
| `POST` | `/logout` | Unlink device |
| `POST` | `/send/text` | Send text message `{"to": "+...", "message": "..."}` |
| `POST` | `/send/file` | Send file (multipart: `file`, `to`, `caption`) |
| `POST` | `/reply` | Agent reply `{"to": "jid", "message": "...", "quote_message_id": "..."}` |
| `GET` | `/messages?chat=JID&limit=50` | Get messages for a chat |
| `GET` | `/messages/search?q=keyword` | Full-text search |
| `GET` | `/chats` | List all chats with last message |
| `GET` | `/chats/{jid}/messages` | Messages for specific chat |
| `GET` | `/contacts` | List contacts |

## Webhook Payload

Incoming messages are POSTed to your `webhook_url`:

```json
{
  "from": "971558762351@s.whatsapp.net",
  "name": "Sam",
  "message": "Hey!",
  "timestamp": 1708387200,
  "type": "text",
  "media_url": "",
  "chat_type": "dm",
  "group_name": "",
  "message_id": "ABC123"
}
```

## CLI

```bash
openclaw-whatsapp start [-c config.yaml]  # Start the bridge
openclaw-whatsapp status [--addr URL]      # Check connection status
openclaw-whatsapp send NUMBER MESSAGE      # Send a message
openclaw-whatsapp stop                     # Stop the bridge
openclaw-whatsapp version                  # Print version
```

## Build

```bash
make build              # Build for current platform
make release            # Cross-compile all platforms
make clean              # Remove build artifacts
```

## Docker

```bash
docker build -t openclaw-whatsapp .
docker run -p 8555:8555 -v wa-data:/app/data openclaw-whatsapp
```

## License

MIT
