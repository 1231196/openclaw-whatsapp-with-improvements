package api

import (
	"encoding/json"
	"net/http"
)

type createGroupRequest struct {
	Name         string   `json:"name"`
	Participants []string `json:"participants"`
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
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