package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/openclaw/whatsapp/store"
)

// MakeEventHandler returns an event handler function suitable for use with
// whatsmeow's AddEventHandler. It processes incoming WhatsApp events, persists
// messages to msgStore, forwards them to the webhook, and triggers the agent.
func MakeEventHandler(client *Client, msgStore *store.MessageStore, webhook *WebhookSender, agent *AgentTrigger, log *slog.Logger) func(evt interface{}) {
	return func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			handleMessage(client, v, msgStore, webhook, agent, log)

		case *events.Connected:
			client.mu.Lock()
			client.status = StatusConnected
			if client.client != nil {
				jid := client.client.Store.ID
				if jid != nil {
					log.Info("connected to WhatsApp", "jid", jid.String())
				}
			}
			client.mu.Unlock()

		case *events.Disconnected:
			client.mu.Lock()
			client.status = StatusDisconnected
			client.mu.Unlock()
			log.Info("disconnected from WhatsApp")

		case *events.LoggedOut:
			client.mu.Lock()
			client.status = StatusDisconnected
			client.latestQR = ""
			client.mu.Unlock()
			log.Warn("logged out from WhatsApp")

		case *events.StreamReplaced:
			client.mu.Lock()
			client.status = StatusDisconnected
			client.mu.Unlock()
			log.Warn("stream replaced — another device connected with this session")
		}
	}
}

// handleMessage processes a single incoming WhatsApp message event. It skips
// messages sent by the current user and status broadcasts, extracts content
// based on message type, persists to the message store, and sends a webhook.
func handleMessage(client *Client, msg *events.Message, msgStore *store.MessageStore, webhook *WebhookSender, agent *AgentTrigger, log *slog.Logger) {
	// Skip messages from ourselves.
	if msg.Info.IsFromMe {
		return
	}

	// Skip status broadcast messages.
	if msg.Info.Chat.String() == "status@broadcast" {
		return
	}

	// Determine message type and extract content / media path.
	var (
		msgType   string
		content   string
		mediaPath string
	)

	m := msg.Message
	switch {
	case m.GetConversation() != "":
		msgType = "text"
		content = m.GetConversation()

	case m.GetExtendedTextMessage() != nil:
		msgType = "text"
		content = m.GetExtendedTextMessage().GetText()

	case m.GetImageMessage() != nil:
		msgType = "image"
		img := m.GetImageMessage()
		content = img.GetCaption()
		ext := getExtension(img.GetMimetype())
		mediaPath = downloadMedia(client, img, msg.Info.ID, ext, log)

	case m.GetVideoMessage() != nil:
		msgType = "video"
		vid := m.GetVideoMessage()
		content = vid.GetCaption()
		ext := getExtension(vid.GetMimetype())
		mediaPath = downloadMedia(client, vid, msg.Info.ID, ext, log)

	case m.GetAudioMessage() != nil:
		msgType = "audio"
		aud := m.GetAudioMessage()
		ext := getExtension(aud.GetMimetype())
		mediaPath = downloadMedia(client, aud, msg.Info.ID, ext, log)

	case m.GetDocumentMessage() != nil:
		msgType = "document"
		doc := m.GetDocumentMessage()
		content = doc.GetTitle()
		ext := getExtension(doc.GetMimetype())
		mediaPath = downloadMedia(client, doc, msg.Info.ID, ext, log)

	case m.GetStickerMessage() != nil:
		msgType = "sticker"
		stk := m.GetStickerMessage()
		ext := getExtension(stk.GetMimetype())
		mediaPath = downloadMedia(client, stk, msg.Info.ID, ext, log)

	case m.GetContactMessage() != nil:
		msgType = "contact"
		content = m.GetContactMessage().GetDisplayName()

	case m.GetLocationMessage() != nil:
		msgType = "location"
		loc := m.GetLocationMessage()
		content = fmt.Sprintf("%.6f,%.6f", loc.GetDegreesLatitude(), loc.GetDegreesLongitude())

	default:
		msgType = "unknown"
		log.Debug("received unhandled message type", "message_id", msg.Info.ID)
	}

	// Determine chat context.
	isGroup := msg.Info.Chat.Server == "g.us"
	senderJID := msg.Info.Sender.String()
	chatJID := msg.Info.Chat.String()
	senderName := msg.Info.PushName

	var groupName string
	if isGroup {
		// Try to get group info for the name.
		if client.GetClient() != nil {
			gi, err := client.GetClient().GetGroupInfo(context.Background(), msg.Info.Chat)
			if err == nil && gi != nil {
				groupName = gi.Name
			}
		}
	}

	// Build the store message.
	storeMsg := &store.Message{
		ID:         msg.Info.ID,
		ChatJID:    chatJID,
		SenderJID:  senderJID,
		SenderName: senderName,
		Content:    content,
		MsgType:    msgType,
		MediaPath:  mediaPath,
		Timestamp:  msg.Info.Timestamp.Unix(),
		IsFromMe:   false,
		IsGroup:    isGroup,
		GroupName:  groupName,
	}

	// Persist the message.
	if err := msgStore.SaveMessage(storeMsg); err != nil {
		log.Error("failed to save message", "error", err, "message_id", msg.Info.ID)
	}

	// Build and send webhook payload.
	chatType := "dm"
	groupID := ""
	if isGroup {
		chatType = "group"
		groupID = chatJID
	}

	payload := &WebhookPayload{
		From:      chatJID,
		Name:      senderName,
		Message:   content,
		GroupID:   groupID,
		Timestamp: msg.Info.Timestamp.Unix(),
		Type:      msgType,
		MediaURL:  mediaPath,
		ChatType:  chatType,
		GroupName: groupName,
		MessageID: msg.Info.ID,
	}

	if isGroup && !client.IsCreatedGroup(chatJID) {
		log.Debug("skipping webhook for unmanaged group", "group_jid", chatJID, "message_id", msg.Info.ID)
	} else if err := webhook.Send(payload); err != nil {
		log.Error("failed to send webhook", "error", err, "message_id", msg.Info.ID)
	}

	// Trigger agent (async — does not block).
	if agent != nil {
		agent.Trigger(client, payload)
	}

	log.Info("message processed",
		"message_id", msg.Info.ID,
		"type", msgType,
		"from", senderJID,
		"chat", chatJID,
		"is_group", isGroup,
	)
}

// downloadMedia downloads media from a WhatsApp message and saves it to disk.
// It returns the file path on success, or an empty string on error.
func downloadMedia(client *Client, downloadable whatsmeow.DownloadableMessage, msgID, ext string, log *slog.Logger) string {
	wc := client.GetClient()
	if wc == nil {
		log.Error("cannot download media: whatsmeow client is nil", "message_id", msgID)
		return ""
	}

	data, err := wc.Download(context.Background(), downloadable)
	if err != nil {
		log.Error("failed to download media", "error", err, "message_id", msgID)
		return ""
	}

	// Ensure the media directory exists.
	mediaDir := filepath.Join(client.dataDir, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		log.Error("failed to create media directory", "error", err, "message_id", msgID)
		return ""
	}

	filePath := filepath.Join(mediaDir, msgID+ext)
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		log.Error("failed to write media file", "error", err, "path", filePath, "message_id", msgID)
		return ""
	}

	log.Debug("media saved", "path", filePath, "size", len(data), "message_id", msgID)
	return filePath
}

// getExtension maps a MIME type to a file extension (with leading dot).
func getExtension(mimeType string) string {
	// Normalise: strip any parameters (e.g. "audio/ogg; codecs=opus").
	mime := strings.ToLower(strings.TrimSpace(mimeType))
	if idx := strings.Index(mime, ";"); idx != -1 {
		mime = strings.TrimSpace(mime[:idx])
	}

	switch mime {
	// Images
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"

	// Video
	case "video/mp4":
		return ".mp4"
	case "video/3gpp":
		return ".3gp"

	// Audio
	case "audio/ogg":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4":
		return ".m4a"
	case "audio/aac":
		return ".aac"

	// Documents
	case "application/pdf":
		return ".pdf"
	case "application/msword":
		return ".doc"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "application/vnd.ms-excel":
		return ".xls"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	case "application/vnd.ms-powerpoint":
		return ".ppt"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return ".pptx"
	case "application/zip":
		return ".zip"
	case "text/plain":
		return ".txt"

	default:
		return ".bin"
	}
}
