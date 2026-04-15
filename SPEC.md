# openclaw-whatsapp — WhatsApp Bridge for OpenClaw

## Overview
A standalone WhatsApp bridge service for OpenClaw agents. Uses whatsmeow (Go) for WhatsApp Web multi-device protocol. Runs as a persistent background service with REST API + webhook delivery.

## Goals
- Dead simple setup: start service → scan QR in browser → done
- Always-on: auto-reconnect, systemd-friendly, runs forever
- REST API for OpenClaw to send/receive messages
- Webhook pushes incoming messages to a configurable URL
- SQLite storage for message history + full-text search
- Single binary distribution (no Go toolchain needed for users)

## Reference Code
The Siventa WhatsApp bridge at `/home/oussama/dev/siventa/backend/services/whatsapp-bridge/` is a proven whatsmeow implementation (~1600 lines Go). Use it as reference for:
- Device management (bridge/device.go)
- Event handling (bridge/events.go)  
- Connection management (bridge/manager.go)
- REST handlers (handlers/)
- SQLite store (store/store.go)

## Architecture

```
openclaw-whatsapp (Go binary)
├── main.go              — CLI entry, config loading, service start
├── bridge/
│   ├── client.go        — whatsmeow client wrapper, connect/reconnect
│   ├── events.go        — message received/sent events, webhook dispatch
│   └── qr.go            — QR code generation for web UI
├── api/
│   ├── router.go        — HTTP router setup
│   ├── auth.go          — QR web page + pairing endpoints
│   ├── messages.go      — send text, send file, get history
│   ├── contacts.go      — list contacts, search
│   └── status.go        — health, connection status
├── store/
│   └── db.go            — SQLite message store + FTS5 search
└── config/
    └── config.go        — YAML/env config
```

## REST API

### Auth & Status
- `GET /status` — connection state, phone number, uptime
- `GET /qr` — web page with QR code for linking (auto-refreshes)
- `POST /logout` — unlink device

### Messaging
- `POST /groups` — `{"name": "Team", "participants": ["+971...", "+972..."]}`
- `POST /send/text` — `{"to": "+971...", "message": "hello"}`
- `POST /send/file` — multipart: file + to + caption
- `GET /messages?chat=JID&limit=50` — get recent messages from a chat
- `GET /messages/search?q=keyword&limit=20` — full-text search across all chats

### Contacts & Chats
- `GET /chats` — list all chats (DMs + groups) with last message
- `GET /contacts` — list contacts with names
- `GET /chats/:jid/messages?limit=50` — messages for specific chat

### Webhooks
- Incoming messages POST to configured webhook URL
- Payload: `{"from": "+971...", "name": "Ali", "message": "...", "timestamp": "...", "type": "text|image|document|audio", "media_url": "...", "chat_type": "dm|group", "group_name": "..."}`
- Configurable filters: DM only, specific numbers, keywords

## Configuration (config.yaml or env vars)
```yaml
port: 8555
data_dir: ~/.openclaw-whatsapp
webhook_url: http://localhost:1337/webhook/whatsapp  # OpenClaw gateway
webhook_filters:
  dm_only: false
  ignore_groups: []
auto_reconnect: true
reconnect_interval: 30s
log_level: info
```

## CLI
```
openclaw-whatsapp start          # Start the service (foreground)
openclaw-whatsapp start -d       # Start as daemon
openclaw-whatsapp stop           # Stop daemon
openclaw-whatsapp status         # Show connection status
openclaw-whatsapp send "number" "message"  # Quick send (connects, sends, exits)
```

## QR Web UI
- `GET /qr` serves a clean HTML page
- Shows QR code as SVG (scannable from phone)
- Auto-refreshes QR every 30s (WhatsApp QR expiry)
- Shows "Connected ✓" once linked
- No dependencies, no JS frameworks, just clean HTML

## Key Requirements
1. **Auto-reconnect** — if WhatsApp drops, reconnect within 30s
2. **Graceful shutdown** — SIGTERM/SIGINT cleanup
3. **Message dedup** — don't webhook the same message twice
4. **Media download** — save incoming images/docs/audio to data_dir/media/
5. **Rate limiting** — respect WhatsApp's sending limits
6. **FTS5 search** — search messages by content across all chats
7. **Minimal deps** — just whatsmeow + sqlite + stdlib

## Build & Distribution
- `go build -o openclaw-whatsapp .`
- Single binary, no runtime deps except SQLite (statically linked)
- Dockerfile for containerized deployment
- Future: brew tap, npm wrapper, OpenClaw plugin
