package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func validateWhatsAppSecret(w http.ResponseWriter, r *http.Request) bool {
	expected := strings.TrimSpace(os.Getenv("WHATSAPP_WEBHOOK_SECRET"))
	if expected == "" {
		writeError(w, http.StatusInternalServerError, "whatsapp secret is not configured")
		return false
	}

	provided := strings.TrimSpace(r.Header.Get("X-WhatsApp-Secret"))
	if provided == "" {
		writeError(w, http.StatusUnauthorized, "missing whatsapp secret")
		return false
	}

	if subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid whatsapp secret")
		return false
	}

	return true
}

func validateWhatsAppSignature(w http.ResponseWriter, r *http.Request, body []byte) bool {
	expected := strings.TrimSpace(os.Getenv("WHATSAPP_WEBHOOK_SECRET"))
	if expected == "" {
		writeError(w, http.StatusInternalServerError, "whatsapp secret is not configured")
		return false
	}

	timestamp := strings.TrimSpace(r.Header.Get("X-WhatsApp-Timestamp"))
	if timestamp == "" {
		writeError(w, http.StatusUnauthorized, "missing whatsapp timestamp")
		return false
	}

	signature := strings.TrimSpace(r.Header.Get("X-WhatsApp-Signature"))
	if signature == "" {
		writeError(w, http.StatusUnauthorized, "missing whatsapp signature")
		return false
	}

	tsValue, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid whatsapp timestamp")
		return false
	}

	now := time.Now().Unix()
	if now-tsValue > 300 || tsValue-now > 300 {
		writeError(w, http.StatusUnauthorized, "stale whatsapp timestamp")
		return false
	}

	mac := hmac.New(sha256.New, []byte(expected))
	mac.Write([]byte(fmt.Sprintf("%s.%s", timestamp, string(body))))
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(expectedSig), []byte(signature)) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid whatsapp signature")
		return false
	}

	return true
}

type createGroupRequest struct {
	Name         string   `json:"name"`
	Participants []string `json:"participants"`
}

type joinGroupRequest struct {
	InviteLink string `json:"invite_link"`
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if !validateWhatsAppSecret(w, r) {
		return
	}

	if !validateWhatsAppSignature(w, r, body) {
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(body))

	var req createGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(req.Participants) == 0 {
		writeError(w, http.StatusBadRequest, "participants are required")
		return
	}

	groupInfo, err := s.Client.CreateGroup(r.Context(), req.Name, req.Participants)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	participants := make([]string, 0, len(groupInfo.Participants))
	for _, participant := range groupInfo.Participants {
		participants = append(participants, participant.JID.String())
	}

	inviteLink, err := s.Client.GetGroupInviteLink(r.Context(), groupInfo.JID.String())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"jid":              groupInfo.JID.String(),
		"name":             groupInfo.Name,
		"participant_count": groupInfo.ParticipantCount,
		"participants":     participants,
		"invite_link":      inviteLink,
	})
}

func (s *Server) handleJoinGroup(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if !validateWhatsAppSecret(w, r) {
		return
	}

	if !validateWhatsAppSignature(w, r, body) {
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(body))

	var req joinGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.InviteLink) == "" {
		writeError(w, http.StatusBadRequest, "invite_link is required")
		return
	}

	groupJID, err := s.Client.JoinGroupWithLink(r.Context(), req.InviteLink)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"jid":         groupJID.String(),
		"invite_link": strings.TrimSpace(req.InviteLink),
	})
}