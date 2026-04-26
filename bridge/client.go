package bridge

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	_ "modernc.org/sqlite"
)

// Status represents the current connection state of the WhatsApp client.
type Status string

const (
	StatusDisconnected Status = "disconnected"
	StatusConnecting   Status = "connecting"
	StatusConnected    Status = "connected"
)

// Client wraps a single-device whatsmeow client, managing session storage,
// QR pairing, connection lifecycle, and message sending.
type Client struct {
	client    *whatsmeow.Client
	container *sqlstore.Container
	status    Status
	latestQR  string
	qrChan    <-chan whatsmeow.QRChannelItem
	createdGroups     map[string]struct{}
	createdGroupsPath string
	mu        sync.RWMutex
	log       *slog.Logger
	startTime time.Time
	dataDir   string

	// Set externally before Connect.
	eventHandler func(evt interface{})
}

// NewClient creates a new bridge Client backed by an SQLite session store
// in dataDir/sessions. The store is opened immediately so that session
// presence can be checked before connecting.
func NewClient(dataDir string, log *slog.Logger) (*Client, error) {
	storeDir := filepath.Join(dataDir, "sessions")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return nil, fmt.Errorf("create sessions dir: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)",
		filepath.Join(storeDir, "whatsapp.db"))

	container, err := sqlstore.New(context.Background(), "sqlite", dsn, waLog.Noop)
	if err != nil {
		return nil, fmt.Errorf("open sqlstore: %w", err)
	}

	return &Client{
		container: container,
		status:    StatusDisconnected,
		createdGroups: loadCreatedGroups(filepath.Join(dataDir, "created_groups.txt")),
		createdGroupsPath: filepath.Join(dataDir, "created_groups.txt"),
		log:       log,
		startTime: time.Now(),
		dataDir:   dataDir,
	}, nil
}

// SetEventHandler sets the handler function that will receive all whatsmeow
// events. Must be called before Connect.
func (c *Client) SetEventHandler(handler func(evt interface{})) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eventHandler = handler
}

// Connect establishes the WhatsApp connection. If the device has no stored
// session, it initiates QR code pairing; otherwise it reconnects using the
// existing session. Connect is safe to call multiple times.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	if c.status == StatusConnected && c.client != nil && c.client.IsConnected() {
		c.mu.Unlock()
		return nil
	}
	c.status = StatusConnecting
	c.mu.Unlock()

	// Get or create device store.
	deviceStore, err := c.container.GetFirstDevice(ctx)
	if err != nil {
		c.setStatus(StatusDisconnected)
		return fmt.Errorf("get device store: %w", err)
	}

	cli := whatsmeow.NewClient(deviceStore, waLog.Noop)

	c.mu.Lock()
	if c.eventHandler != nil {
		cli.AddEventHandler(c.eventHandler)
	}
	c.client = cli
	c.mu.Unlock()

	if cli.Store.ID == nil {
		// No existing session — need QR pairing.
		qrChan, err := cli.GetQRChannel(ctx)
		if err != nil {
			c.setStatus(StatusDisconnected)
			return fmt.Errorf("get QR channel: %w", err)
		}

		if err := cli.Connect(); err != nil {
			c.setStatus(StatusDisconnected)
			return fmt.Errorf("connect for QR: %w", err)
		}

		c.mu.Lock()
		c.qrChan = qrChan
		c.mu.Unlock()

		go c.processQRCodes()

		c.log.Info("QR pairing started, waiting for scan")
	} else {
		// Existing session — just reconnect.
		if err := cli.Connect(); err != nil {
			c.setStatus(StatusDisconnected)
			return fmt.Errorf("connect: %w", err)
		}
		c.setStatus(StatusConnected)
		c.log.Info("reconnected with existing session",
			"jid", cli.Store.ID.String())
	}

	return nil
}

// processQRCodes reads from the QR channel in a goroutine, updating the
// client's latestQR on new codes and status on success or timeout.
func (c *Client) processQRCodes() {
	c.mu.RLock()
	ch := c.qrChan
	c.mu.RUnlock()

	if ch == nil {
		return
	}

	for evt := range ch {
		switch evt.Event {
		case "code":
			c.mu.Lock()
			c.latestQR = evt.Code
			c.mu.Unlock()
			c.log.Info("new QR code available")

		case "success":
			c.mu.Lock()
			c.status = StatusConnected
			c.latestQR = ""
			c.qrChan = nil
			c.mu.Unlock()

			jid := ""
			if c.client != nil && c.client.Store.ID != nil {
				jid = c.client.Store.ID.String()
			}
			c.log.Info("QR pairing successful", "jid", jid)

		case "timeout":
			c.mu.Lock()
			c.latestQR = ""
			c.qrChan = nil
			c.status = StatusDisconnected
			c.mu.Unlock()
			c.log.Warn("QR code timed out")
		}
	}
}

// Disconnect cleanly disconnects the WhatsApp client.
func (c *Client) Disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.client != nil {
		c.client.Disconnect()
	}
	c.status = StatusDisconnected
	c.latestQR = ""
}

// Logout logs out the current session and disconnects. The stored session
// is removed so the next Connect will require a fresh QR scan.
func (c *Client) Logout() error {
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()

	if cli == nil {
		return nil
	}

	if err := cli.Logout(context.Background()); err != nil {
		return fmt.Errorf("logout: %w", err)
	}

	c.Disconnect()
	return nil
}

// IsConnected returns true if the client is currently connected to WhatsApp.
// Thread-safe. Implements the Reconnectable interface.
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.client == nil {
		return false
	}
	return c.client.IsConnected()
}

// HasSession returns true if there is a stored WhatsApp session (i.e. the
// device has been paired before). Thread-safe. Implements the Reconnectable
// interface.
func (c *Client) HasSession() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.client == nil {
		return false
	}
	return c.client.Store.ID != nil
}

// GetStatus returns the current connection status. It cross-checks the actual
// whatsmeow connection state against the stored status for accuracy.
// A device must be paired (Store.ID != nil) AND the websocket connected to
// count as StatusConnected. An open websocket without a paired device means
// we're waiting for QR scan.
func (c *Client) GetStatus() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.client != nil && c.client.IsConnected() && c.client.Store.ID != nil {
		return StatusConnected
	}
	if c.client != nil && c.client.IsConnected() && c.client.Store.ID == nil {
		return StatusConnecting // websocket open but waiting for QR scan
	}
	if c.status == StatusConnecting {
		return StatusConnecting
	}
	return StatusDisconnected
}

// GetLatestQR returns the most recent QR code string for pairing, or an
// empty string if no QR is currently available. Thread-safe.
func (c *Client) GetLatestQR() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latestQR
}

// GetClient returns the underlying whatsmeow.Client for direct access.
func (c *Client) GetClient() *whatsmeow.Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.client
}

// GetJID returns the JID (WhatsApp ID) of the connected device as a string,
// or an empty string if not connected or no session exists.
func (c *Client) GetJID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.client == nil || c.client.Store.ID == nil {
		return ""
	}
	return c.client.Store.ID.String()
}

// GetStartTime returns the time when the client was created.
func (c *Client) GetStartTime() time.Time {
	return c.startTime
}

// SendText sends a plain text message to the specified JID or phone number.
func (c *Client) SendText(ctx context.Context, to string, message string) error {
	if c.client == nil || !c.client.IsConnected() {
		return fmt.Errorf("client is not connected")
	}

	jid, err := parseJID(to)
	if err != nil {
		return fmt.Errorf("parse recipient JID: %w", err)
	}

	msg := &waProto.Message{
		Conversation: proto.String(message),
	}

	_, err = c.client.SendMessage(ctx, jid, msg)
	if err != nil {
		return fmt.Errorf("send text message: %w", err)
	}

	return nil
}

// SendFile uploads and sends a media file (image, video, audio, or document)
// to the specified JID or phone number. The media type is inferred from the
// provided MIME type.
func (c *Client) SendFile(ctx context.Context, to string, data []byte, mimetype, filename, caption string) error {
	if c.client == nil || !c.client.IsConnected() {
		return fmt.Errorf("client is not connected")
	}

	jid, err := parseJID(to)
	if err != nil {
		return fmt.Errorf("parse recipient JID: %w", err)
	}

	var msg *waProto.Message

	switch {
	case isImage(mimetype):
		resp, err := c.client.Upload(ctx, data, whatsmeow.MediaImage)
		if err != nil {
			return fmt.Errorf("upload image: %w", err)
		}
		msg = &waProto.Message{
			ImageMessage: &waProto.ImageMessage{
				URL:           proto.String(resp.URL),
				Mimetype:      proto.String(mimetype),
				Caption:       proto.String(caption),
				FileLength:    proto.Uint64(uint64(len(data))),
				FileSHA256:    resp.FileSHA256,
				FileEncSHA256: resp.FileEncSHA256,
				MediaKey:      resp.MediaKey,
				DirectPath:    proto.String(resp.DirectPath),
			},
		}

	case isVideo(mimetype):
		resp, err := c.client.Upload(ctx, data, whatsmeow.MediaVideo)
		if err != nil {
			return fmt.Errorf("upload video: %w", err)
		}
		msg = &waProto.Message{
			VideoMessage: &waProto.VideoMessage{
				URL:           proto.String(resp.URL),
				Mimetype:      proto.String(mimetype),
				Caption:       proto.String(caption),
				FileLength:    proto.Uint64(uint64(len(data))),
				FileSHA256:    resp.FileSHA256,
				FileEncSHA256: resp.FileEncSHA256,
				MediaKey:      resp.MediaKey,
				DirectPath:    proto.String(resp.DirectPath),
			},
		}

	case isAudio(mimetype):
		resp, err := c.client.Upload(ctx, data, whatsmeow.MediaAudio)
		if err != nil {
			return fmt.Errorf("upload audio: %w", err)
		}
		msg = &waProto.Message{
			AudioMessage: &waProto.AudioMessage{
				URL:           proto.String(resp.URL),
				Mimetype:      proto.String(mimetype),
				FileLength:    proto.Uint64(uint64(len(data))),
				FileSHA256:    resp.FileSHA256,
				FileEncSHA256: resp.FileEncSHA256,
				MediaKey:      resp.MediaKey,
				DirectPath:    proto.String(resp.DirectPath),
			},
		}

	default:
		// Treat everything else as a document.
		resp, err := c.client.Upload(ctx, data, whatsmeow.MediaDocument)
		if err != nil {
			return fmt.Errorf("upload document: %w", err)
		}
		msg = &waProto.Message{
			DocumentMessage: &waProto.DocumentMessage{
				URL:           proto.String(resp.URL),
				Mimetype:      proto.String(mimetype),
				Title:         proto.String(caption),
				FileName:      proto.String(filename),
				FileLength:    proto.Uint64(uint64(len(data))),
				FileSHA256:    resp.FileSHA256,
				FileEncSHA256: resp.FileEncSHA256,
				MediaKey:      resp.MediaKey,
				DirectPath:    proto.String(resp.DirectPath),
			},
		}
	}

	_, err = c.client.SendMessage(ctx, jid, msg)
	if err != nil {
		return fmt.Errorf("send file message: %w", err)
	}

	return nil
}

// CreateGroup creates a new WhatsApp group with the given name and participants.
func (c *Client) CreateGroup(ctx context.Context, name string, participantIDs []string) (*types.GroupInfo, error) {
	if c.client == nil || !c.client.IsConnected() {
		return nil, fmt.Errorf("client is not connected")
	}
	if name == "" {
		return nil, fmt.Errorf("group name is required")
	}
	if len(participantIDs) == 0 {
		return nil, fmt.Errorf("at least one participant is required")
	}

	participants := make([]types.JID, 0, len(participantIDs))
	for _, participantID := range participantIDs {
		jid, err := parseJID(participantID)
		if err != nil {
			return nil, fmt.Errorf("parse participant %q: %w", participantID, err)
		}
		participants = append(participants, jid)
	}

	groupInfo, err := c.client.CreateGroup(ctx, whatsmeow.ReqCreateGroup{
		Name:         name,
		Participants: participants,
	})
	if err != nil {
		return nil, fmt.Errorf("create group: %w", err)
	}

	c.rememberCreatedGroup(groupInfo.JID.String())

	return groupInfo, nil
}

// GetGroupInviteLink returns the invite link for a WhatsApp group JID.
func (c *Client) GetGroupInviteLink(ctx context.Context, groupJID string) (string, error) {
	if c.client == nil || !c.client.IsConnected() {
		return "", fmt.Errorf("client is not connected")
	}

	jid, err := parseJID(groupJID)
	if err != nil {
		return "", fmt.Errorf("parse group JID: %w", err)
	}

	link, err := c.client.GetGroupInviteLink(ctx, jid, false)
	if err != nil {
		return "", fmt.Errorf("get group invite link: %w", err)
	}

	return link, nil
}

// IsCreatedGroup reports whether a group JID was created via this service.
func (c *Client) IsCreatedGroup(groupJID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.createdGroups[groupJID]
	return ok
}

// rememberCreatedGroup stores the group JID in memory and on disk.
func (c *Client) rememberCreatedGroup(groupJID string) {
	if groupJID == "" {
		return
	}

	c.mu.Lock()
	if _, exists := c.createdGroups[groupJID]; exists {
		c.mu.Unlock()
		return
	}
	c.createdGroups[groupJID] = struct{}{}
	path := c.createdGroupsPath
	c.mu.Unlock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		c.log.Warn("failed to persist created group", "error", err, "group_jid", groupJID)
		return
	}
	defer f.Close()

	if _, err := f.WriteString(groupJID + "\n"); err != nil {
		c.log.Warn("failed to append created group", "error", err, "group_jid", groupJID)
	}
}

// --- helpers ----------------------------------------------------------------

// parseJID converts a string to a types.JID. If the string contains "@" it is
// parsed as a full JID; otherwise it is treated as a phone number (leading "+"
// or "00" stripped, non-digit characters removed) on the default user server.
func parseJID(s string) (types.JID, error) {
	if s == "" {
		return types.JID{}, fmt.Errorf("empty JID")
	}

	if strings.Contains(s, "@") {
		jid, err := types.ParseJID(s)
		if err != nil {
			return types.JID{}, fmt.Errorf("parse JID %q: %w", s, err)
		}
		return jid, nil
	}

	// Treat as phone number.
	cleaned := s
	cleaned = strings.TrimPrefix(cleaned, "+")
	cleaned = strings.TrimPrefix(cleaned, "00")

	// Strip any remaining non-digit characters.
	var digits strings.Builder
	for _, r := range cleaned {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}

	num := digits.String()
	if num == "" {
		return types.JID{}, fmt.Errorf("no digits in JID %q", s)
	}

	return types.NewJID(num, types.DefaultUserServer), nil
}

// isImage returns true if the MIME type represents an image.
func isImage(mimetype string) bool {
	return strings.HasPrefix(mimetype, "image/")
}

// isVideo returns true if the MIME type represents a video.
func isVideo(mimetype string) bool {
	return strings.HasPrefix(mimetype, "video/")
}

// isAudio returns true if the MIME type represents audio.
func isAudio(mimetype string) bool {
	return strings.HasPrefix(mimetype, "audio/")
}

// setStatus is a helper that sets the client status under the write lock.
func (c *Client) setStatus(s Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = s
}

func loadCreatedGroups(path string) map[string]struct{} {
	groups := make(map[string]struct{})

	f, err := os.Open(path)
	if err != nil {
		return groups
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		jid := strings.TrimSpace(scanner.Text())
		if jid == "" {
			continue
		}
		groups[jid] = struct{}{}
	}

	return groups
}
