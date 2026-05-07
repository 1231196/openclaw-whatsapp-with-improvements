package bridge

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// WebhookPayload is the JSON body sent to the configured webhook URL for each
// incoming WhatsApp message.
type WebhookPayload struct {
	From      string `json:"from"`
	Name      string `json:"name,omitempty"`
	Message   string `json:"message"`
	ChatJID   string `json:"chat_jid"`
	GroupID   string `json:"group_id,omitempty"`
	Timestamp int64  `json:"timestamp"`
	Type      string `json:"type"`
	MediaURL  string `json:"media_url,omitempty"`
	ChatType  string `json:"chat_type"`
	GroupName string `json:"group_name,omitempty"`
	MessageID string `json:"message_id"`
}

// WebhookFilters controls which messages are forwarded to the webhook endpoint.
type WebhookFilters struct {
	DMOnly       bool     // If true, only direct messages are forwarded (groups are dropped).
	IgnoreGroups []string // Group JIDs to silently ignore.
}

// WebhookSender delivers webhook payloads to an external HTTP endpoint with
// deduplication and filtering.
type WebhookSender struct {
	url     string
	token   string
	filters WebhookFilters
	seen    map[string]time.Time // message ID -> first seen time (dedup)
	mu      sync.Mutex
	client  *http.Client
	log     *slog.Logger
}

// seenTTL is the time-to-live for entries in the deduplication map.
const seenTTL = 5 * time.Minute

// NewWebhookSender creates a WebhookSender ready to POST payloads to the given
// url. If url is empty the sender is effectively a no-op (Send returns nil
// immediately).
func NewWebhookSender(url string, token string, filters WebhookFilters, log *slog.Logger) *WebhookSender {
	return &WebhookSender{
		url:     url,
		token:   token,
		filters: filters,
		seen:    make(map[string]time.Time),
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		log: log,
	}
}

// Send delivers a webhook payload to the configured endpoint. It silently
// returns nil when no webhook URL is configured, when the message has already
// been sent (dedup), or when filters exclude the message.
func (w *WebhookSender) Send(payload *WebhookPayload) error {
	if w.url == "" {
		return nil
	}

	w.mu.Lock()

	// Housekeeping: remove stale dedup entries before checking.
	w.cleanupSeenLocked()

	// Dedup: skip if we've already seen this message ID.
	if _, ok := w.seen[payload.MessageID]; ok {
		w.mu.Unlock()
		w.log.Debug("webhook skipping duplicate message", "message_id", payload.MessageID)
		return nil
	}

	// Record this message ID.
	w.seen[payload.MessageID] = time.Now()
	w.mu.Unlock()

	// Apply filters.
	if w.filters.DMOnly && payload.ChatType == "group" {
		w.log.Debug("webhook skipping group message (dm_only)", "message_id", payload.MessageID)
		return nil
	}
	for _, ignored := range w.filters.IgnoreGroups {
		if payload.From == ignored || payload.GroupName == ignored {
			w.log.Debug("webhook skipping ignored group", "group", ignored, "message_id", payload.MessageID)
			return nil
		}
	}

	// Marshal payload to JSON.
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("webhook marshal payload: %w", err)
	}

	// POST to the configured URL.
	req, err := http.NewRequest(http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}

	webhookSecret := os.Getenv("WHATSAPP_WEBHOOK_SECRET")
	if webhookSecret != "" {
		timestamp := fmt.Sprintf("%d", time.Now().Unix())
		mac := hmac.New(sha256.New, []byte(webhookSecret))
		mac.Write([]byte(timestamp))
		mac.Write([]byte("."))
		mac.Write(body)
		signature := hex.EncodeToString(mac.Sum(nil))
		req.Header.Set("X-WhatsApp-Timestamp", timestamp)
		req.Header.Set("X-WhatsApp-Signature", signature)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		w.log.Error("webhook delivery failed", "error", err, "message_id", payload.MessageID)
		return fmt.Errorf("webhook POST: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		w.log.Info("webhook delivered", "status", resp.StatusCode, "message_id", payload.MessageID)
	} else {
		w.log.Warn("webhook non-2xx response", "status", resp.StatusCode, "message_id", payload.MessageID)
	}

	return nil
}

// CleanupSeen removes deduplication entries older than seenTTL. It is safe for
// concurrent use. Send() already calls this internally, but it can also be
// called externally if desired.
func (w *WebhookSender) CleanupSeen() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cleanupSeenLocked()
}

// cleanupSeenLocked removes stale entries from the seen map. The caller MUST
// hold w.mu.
func (w *WebhookSender) cleanupSeenLocked() {
	cutoff := time.Now().Add(-seenTTL)
	for id, t := range w.seen {
		if t.Before(cutoff) {
			delete(w.seen, id)
		}
	}
}
