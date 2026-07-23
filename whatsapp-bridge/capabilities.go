package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type capabilityResponse struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Message string `json:"message,omitempty"`
}

type identityResponse struct {
	JID          string `json:"jid"`
	PhoneNumber  string `json:"phone_number"`
	LID          string `json:"lid,omitempty"`
	PushName     string `json:"push_name,omitempty"`
	BusinessName string `json:"business_name,omitempty"`
	Platform     string `json:"platform,omitempty"`
}

type groupParticipantResponse struct {
	JID          string `json:"jid"`
	PhoneNumber  string `json:"phone_number,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	IsAdmin      bool   `json:"is_admin"`
	IsSuperAdmin bool   `json:"is_super_admin"`
	ErrorCode    int    `json:"error_code,omitempty"`
}

type groupResponse struct {
	JID                    string                     `json:"jid"`
	Name                   string                     `json:"name"`
	Topic                  string                     `json:"topic,omitempty"`
	OwnerJID               string                     `json:"owner_jid,omitempty"`
	CreatedAt              string                     `json:"created_at,omitempty"`
	ParticipantCount       int                        `json:"participant_count"`
	Participants           []groupParticipantResponse `json:"participants,omitempty"`
	IsAnnounce             bool                       `json:"is_announce"`
	IsLocked               bool                       `json:"is_locked"`
	IsEphemeral            bool                       `json:"is_ephemeral"`
	DisappearingTimer      uint32                     `json:"disappearing_timer_seconds,omitempty"`
	JoinApprovalRequired   bool                       `json:"join_approval_required"`
	IsCommunity            bool                       `json:"is_community"`
	LinkedParentJID        string                     `json:"linked_parent_jid,omitempty"`
	IsDefaultCommunityChat bool                       `json:"is_default_community_chat"`
}

type createGroupRequest struct {
	Name         string   `json:"name"`
	Participants []string `json:"participants"`
}

type updateGroupParticipantsRequest struct {
	GroupJID     string   `json:"group_jid"`
	Participants []string `json:"participants"`
	Action       string   `json:"action"`
}

type chatReadStateRequest struct {
	ChatJID string `json:"chat_jid"`
	Read    bool   `json:"read"`
}

type whatsappLookupRequest struct {
	PhoneNumbers []string `json:"phone_numbers"`
}

type muteChatRequest struct {
	ChatJID         string `json:"chat_jid"`
	Muted           bool   `json:"muted"`
	DurationSeconds int64  `json:"duration_seconds,omitempty"`
}

type archiveChatRequest struct {
	ChatJID  string `json:"chat_jid"`
	Archived bool   `json:"archived"`
}

type blockContactRequest struct {
	JID     string `json:"jid"`
	Blocked bool   `json:"blocked"`
}

type chatPresenceRequest struct {
	ChatJID string `json:"chat_jid"`
	State   string `json:"state"`
	Media   string `json:"media,omitempty"`
}

type chatListItem struct {
	JID                    string `json:"jid"`
	PhoneNumber            string `json:"phone_number,omitempty"`
	Name                   string `json:"name,omitempty"`
	Type                   string `json:"type"`
	LastMessageTime        string `json:"last_message_time,omitempty"`
	LastMessage            string `json:"last_message,omitempty"`
	LastMediaType          string `json:"last_media_type,omitempty"`
	UnreadCount            uint32 `json:"unread_count"`
	MarkedAsUnread         bool   `json:"marked_as_unread"`
	Archived               bool   `json:"archived"`
	IsCommunity            bool   `json:"is_community"`
	CommunityJID           string `json:"community_jid,omitempty"`
	IsDefaultCommunityChat bool   `json:"is_default_community_chat"`
}

type messageListItem struct {
	ID        string `json:"id"`
	ChatJID   string `json:"chat_jid"`
	Sender    string `json:"sender"`
	Content   string `json:"content,omitempty"`
	Timestamp string `json:"timestamp"`
	IsFromMe  bool   `json:"is_from_me"`
	MediaType string `json:"media_type,omitempty"`
	Filename  string `json:"filename,omitempty"`
	Status    string `json:"status,omitempty"`
}

func writeCapabilityJSON(w http.ResponseWriter, status int, response capabilityResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func writeCapabilityError(w http.ResponseWriter, status int, err error) {
	writeCapabilityJSON(w, status, capabilityResponse{Success: false, Message: err.Error()})
}

func decodeCapabilityRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeCapabilityError(w, http.StatusBadRequest, fmt.Errorf("invalid request: %w", err))
		return false
	}
	return true
}

func normalizePhoneNumber(value string) string {
	var digits strings.Builder
	for _, character := range value {
		if unicode.IsDigit(character) {
			digits.WriteRune(character)
		}
	}
	normalized := digits.String()
	return strings.TrimPrefix(normalized, "00")
}

func parseUserJID(value string) (types.JID, error) {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "@") {
		jid, err := types.ParseJID(value)
		if err != nil {
			return types.EmptyJID, fmt.Errorf("invalid user JID: %w", err)
		}
		if jid.Server == types.GroupServer {
			return types.EmptyJID, fmt.Errorf("expected a user JID, got a group JID")
		}
		return jid.ToNonAD(), nil
	}

	phoneNumber := normalizePhoneNumber(value)
	if phoneNumber == "" {
		return types.EmptyJID, fmt.Errorf("phone number is required")
	}
	return types.NewJID(phoneNumber, types.DefaultUserServer), nil
}

func parseChatJID(value string) (types.JID, error) {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "@") {
		return parseUserJID(value)
	}
	jid, err := types.ParseJID(value)
	if err != nil {
		return types.EmptyJID, fmt.Errorf("invalid chat JID: %w", err)
	}
	return jid.ToNonAD(), nil
}

func parseGroupJID(value string) (types.JID, error) {
	jid, err := types.ParseJID(strings.TrimSpace(value))
	if err != nil {
		return types.EmptyJID, fmt.Errorf("invalid group JID: %w", err)
	}
	if jid.Server != types.GroupServer {
		return types.EmptyJID, fmt.Errorf("group JID must end in @g.us")
	}
	return jid, nil
}

func parseUserJIDs(values []string) ([]types.JID, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("at least one participant is required")
	}
	jids := make([]types.JID, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		jid, err := parseUserJID(value)
		if err != nil {
			return nil, err
		}
		key := jid.String()
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		jids = append(jids, jid)
	}
	return jids, nil
}

func groupToResponse(group *types.GroupInfo) groupResponse {
	participants := make([]groupParticipantResponse, 0, len(group.Participants))
	for _, participant := range group.Participants {
		participants = append(participants, groupParticipantResponse{
			JID:          participant.JID.String(),
			PhoneNumber:  participant.PhoneNumber.String(),
			DisplayName:  participant.DisplayName,
			IsAdmin:      participant.IsAdmin,
			IsSuperAdmin: participant.IsSuperAdmin,
			ErrorCode:    participant.Error,
		})
	}
	participantCount := group.ParticipantCount
	if participantCount == 0 {
		participantCount = len(participants)
	}
	createdAt := ""
	if !group.GroupCreated.IsZero() {
		createdAt = group.GroupCreated.Format(time.RFC3339)
	}
	return groupResponse{
		JID:                    group.JID.String(),
		Name:                   group.Name,
		Topic:                  group.Topic,
		OwnerJID:               group.OwnerJID.String(),
		CreatedAt:              createdAt,
		ParticipantCount:       participantCount,
		Participants:           participants,
		IsAnnounce:             group.IsAnnounce,
		IsLocked:               group.IsLocked,
		IsEphemeral:            group.IsEphemeral,
		DisappearingTimer:      group.DisappearingTimer,
		JoinApprovalRequired:   group.IsJoinApprovalRequired,
		IsCommunity:            group.IsParent,
		LinkedParentJID:        group.LinkedParentJID.String(),
		IsDefaultCommunityChat: group.IsDefaultSubGroup,
	}
}

func lastChatMessageTime(store *MessageStore, chatJID string) time.Time {
	var timestamp time.Time
	if err := store.db.QueryRow("SELECT last_message_time FROM chats WHERE jid = ?", chatJID).Scan(&timestamp); err != nil && err != sql.ErrNoRows {
		return time.Time{}
	}
	return timestamp
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeCapabilityError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		return false
	}
	return true
}

func registerCapabilityHandlers(mux *http.ServeMux, client *whatsmeow.Client, messageStore *MessageStore) {
	mux.HandleFunc("/api/history-sync", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request struct {
			ChatJID    string `json:"chat_jid"`
			FromLatest bool   `json:"from_latest"`
		}
		if !decodeCapabilityRequest(w, r, &request) || strings.TrimSpace(request.ChatJID) == "" {
			return
		}
		if err := requestHistorySync(client, messageStore, strings.TrimSpace(request.ChatJID), request.FromLatest); err != nil {
			writeCapabilityError(w, http.StatusBadGateway, err)
			return
		}
		writeCapabilityJSON(w, http.StatusAccepted, capabilityResponse{Success: true, Message: "History sync requested"})
	})

	mux.HandleFunc("/api/chats", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		limit := 30
		if value := r.URL.Query().Get("limit"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > 100 {
				writeCapabilityError(w, http.StatusBadRequest, fmt.Errorf("limit must be between 1 and 100"))
				return
			}
			limit = parsed
		}
		offset := 0
		if value := r.URL.Query().Get("offset"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 0 {
				writeCapabilityError(w, http.StatusBadRequest, fmt.Errorf("offset must be a non-negative integer"))
				return
			}
			offset = parsed
		}
		query := strings.TrimSpace(r.URL.Query().Get("query"))
		searchPattern := "%" + query + "%"
		rows, err := messageStore.db.Query(`
			SELECT c.jid, COALESCE(c.name, ''), COALESCE(CAST(c.last_message_time AS TEXT), ''),
				COALESCE((SELECT content FROM messages m WHERE m.chat_jid = c.jid ORDER BY m.timestamp DESC LIMIT 1), ''),
				COALESCE((SELECT media_type FROM messages m WHERE m.chat_jid = c.jid ORDER BY m.timestamp DESC LIMIT 1), ''),
				COALESCE(c.unread_count, 0), COALESCE(c.marked_as_unread, 0), COALESCE(c.archived, 0),
				COALESCE(c.is_community, 0), COALESCE(c.community_jid, ''), COALESCE(c.is_default_community_chat, 0)
			FROM chats c
			WHERE ? = '' OR LOWER(c.name) LIKE LOWER(?) OR LOWER(c.jid) LIKE LOWER(?)
			ORDER BY c.last_message_time DESC, c.jid DESC
			LIMIT ? OFFSET ?`, query, searchPattern, searchPattern, limit, offset)
		if err != nil {
			writeCapabilityError(w, http.StatusInternalServerError, fmt.Errorf("failed to list chats: %w", err))
			return
		}
		defer rows.Close()
		chats := make([]chatListItem, 0)
		for rows.Next() {
			var item chatListItem
			if err = rows.Scan(
				&item.JID, &item.Name, &item.LastMessageTime, &item.LastMessage, &item.LastMediaType,
				&item.UnreadCount, &item.MarkedAsUnread, &item.Archived, &item.IsCommunity,
				&item.CommunityJID, &item.IsDefaultCommunityChat,
			); err != nil {
				writeCapabilityError(w, http.StatusInternalServerError, fmt.Errorf("failed to read chat: %w", err))
				return
			}
			if client.Store.ID != nil && item.JID == client.Store.ID.ToNonAD().String() && isNumericChatName(item.Name) {
				if jid, parseErr := types.ParseJID(item.JID); parseErr == nil {
					if contact, contactErr := client.Store.Contacts.GetContact(r.Context(), jid); contactErr == nil && contact.FullName != "" {
						item.Name = contact.FullName
					}
				}
			}
			if jid, parseErr := types.ParseJID(item.JID); parseErr == nil {
				if jid.Server == types.DefaultUserServer {
					item.PhoneNumber = jid.User
				} else if jid.Server == types.HiddenUserServer {
					if phoneJID, mapErr := client.Store.LIDs.GetPNForLID(r.Context(), jid); mapErr == nil {
						item.PhoneNumber = phoneJID.User
					}
				}
			}
			item.Type = "direct"
			if strings.HasSuffix(item.JID, "@g.us") {
				item.Type = "group"
			} else if item.JID == "status@broadcast" {
				item.Type = "status"
			}
			chats = append(chats, item)
		}
		writeCapabilityJSON(w, http.StatusOK, capabilityResponse{Success: true, Data: chats})
	})

	mux.HandleFunc("/api/messages", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		chatJID := strings.TrimSpace(r.URL.Query().Get("chat_jid"))
		if chatJID == "" {
			writeCapabilityError(w, http.StatusBadRequest, fmt.Errorf("chat_jid is required"))
			return
		}
		limit := 50
		if value := r.URL.Query().Get("limit"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > 200 {
				writeCapabilityError(w, http.StatusBadRequest, fmt.Errorf("limit must be between 1 and 200"))
				return
			}
			limit = parsed
		}
		offset := 0
		if value := r.URL.Query().Get("offset"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 0 {
				writeCapabilityError(w, http.StatusBadRequest, fmt.Errorf("offset must be a non-negative integer"))
				return
			}
			offset = parsed
		}
		chatJIDs := []string{chatJID}
		if jid, err := types.ParseJID(chatJID); err == nil {
			if jid.Server == types.HiddenUserServer {
				if phoneJID, mapErr := client.Store.LIDs.GetPNForLID(r.Context(), jid); mapErr == nil {
					chatJIDs = append(chatJIDs, phoneJID.String())
				}
			} else if jid.Server == types.DefaultUserServer {
				if lidJID, mapErr := client.Store.LIDs.GetLIDForPN(r.Context(), jid); mapErr == nil {
					chatJIDs = append(chatJIDs, lidJID.String())
				}
			}
		}
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chatJIDs)), ",")
		queryArgs := make([]any, 0, len(chatJIDs)+2)
		for _, jid := range chatJIDs {
			queryArgs = append(queryArgs, jid)
		}
		queryArgs = append(queryArgs, limit, offset)
		rows, err := messageStore.db.Query(`
			SELECT id, chat_jid, sender, COALESCE(content, ''), CAST(timestamp AS TEXT),
				is_from_me, COALESCE(media_type, ''), COALESCE(filename, ''), COALESCE(status, '')
			FROM messages WHERE chat_jid IN (`+placeholders+`) ORDER BY timestamp DESC, id DESC LIMIT ? OFFSET ?`, queryArgs...)
		if err != nil {
			writeCapabilityError(w, http.StatusInternalServerError, fmt.Errorf("failed to list messages: %w", err))
			return
		}
		defer rows.Close()
		messages := make([]messageListItem, 0)
		for rows.Next() {
			var item messageListItem
			if err = rows.Scan(&item.ID, &item.ChatJID, &item.Sender, &item.Content, &item.Timestamp, &item.IsFromMe, &item.MediaType, &item.Filename, &item.Status); err != nil {
				writeCapabilityError(w, http.StatusInternalServerError, fmt.Errorf("failed to read message: %w", err))
				return
			}
			messages = append(messages, item)
		}
		writeCapabilityJSON(w, http.StatusOK, capabilityResponse{Success: true, Data: messages})
	})

	mux.HandleFunc("/api/identity", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if client.Store.ID == nil {
			writeCapabilityError(w, http.StatusServiceUnavailable, fmt.Errorf("WhatsApp identity is not available"))
			return
		}
		identity := client.Store.ID.ToNonAD()
		lid := ""
		if !client.Store.LID.IsEmpty() {
			lid = client.Store.LID.ToNonAD().String()
		}
		writeCapabilityJSON(w, http.StatusOK, capabilityResponse{Success: true, Data: identityResponse{
			JID:          identity.String(),
			PhoneNumber:  identity.User,
			LID:          lid,
			PushName:     client.Store.PushName,
			BusinessName: client.Store.BusinessName,
			Platform:     client.Store.Platform,
		}})
	})

	mux.HandleFunc("/api/groups", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		groups, err := client.GetJoinedGroups(r.Context())
		if err != nil {
			writeCapabilityError(w, http.StatusBadGateway, fmt.Errorf("failed to list joined groups: %w", err))
			return
		}
		response := make([]groupResponse, 0, len(groups))
		for _, group := range groups {
			response = append(response, groupToResponse(group))
		}
		writeCapabilityJSON(w, http.StatusOK, capabilityResponse{Success: true, Data: response})
	})

	mux.HandleFunc("/api/groups/info", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		jid, err := parseGroupJID(r.URL.Query().Get("jid"))
		if err != nil {
			writeCapabilityError(w, http.StatusBadRequest, err)
			return
		}
		group, err := client.GetGroupInfo(r.Context(), jid)
		if err != nil {
			writeCapabilityError(w, http.StatusBadGateway, fmt.Errorf("failed to get group info: %w", err))
			return
		}
		writeCapabilityJSON(w, http.StatusOK, capabilityResponse{Success: true, Data: groupToResponse(group)})
	})

	mux.HandleFunc("/api/groups/create", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request createGroupRequest
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		request.Name = strings.TrimSpace(request.Name)
		if request.Name == "" || len([]rune(request.Name)) > 25 {
			writeCapabilityError(w, http.StatusBadRequest, fmt.Errorf("group name must contain 1 to 25 characters"))
			return
		}
		participants, err := parseUserJIDs(request.Participants)
		if err != nil {
			writeCapabilityError(w, http.StatusBadRequest, err)
			return
		}
		group, err := client.CreateGroup(r.Context(), whatsmeow.ReqCreateGroup{Name: request.Name, Participants: participants})
		if err != nil {
			writeCapabilityError(w, http.StatusBadGateway, fmt.Errorf("failed to create group: %w", err))
			return
		}
		writeCapabilityJSON(w, http.StatusOK, capabilityResponse{Success: true, Data: groupToResponse(group), Message: "Group created"})
	})

	mux.HandleFunc("/api/groups/participants", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request updateGroupParticipantsRequest
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		groupJID, err := parseGroupJID(request.GroupJID)
		if err != nil {
			writeCapabilityError(w, http.StatusBadRequest, err)
			return
		}
		participants, err := parseUserJIDs(request.Participants)
		if err != nil {
			writeCapabilityError(w, http.StatusBadRequest, err)
			return
		}
		actions := map[string]whatsmeow.ParticipantChange{
			"add": whatsmeow.ParticipantChangeAdd, "remove": whatsmeow.ParticipantChangeRemove,
			"promote": whatsmeow.ParticipantChangePromote, "demote": whatsmeow.ParticipantChangeDemote,
		}
		action, valid := actions[strings.ToLower(request.Action)]
		if !valid {
			writeCapabilityError(w, http.StatusBadRequest, fmt.Errorf("action must be add, remove, promote, or demote"))
			return
		}
		updated, err := client.UpdateGroupParticipants(r.Context(), groupJID, participants, action)
		if err != nil {
			writeCapabilityError(w, http.StatusBadGateway, fmt.Errorf("failed to update group participants: %w", err))
			return
		}
		response := make([]groupParticipantResponse, 0, len(updated))
		for _, participant := range updated {
			response = append(response, groupParticipantResponse{
				JID: participant.JID.String(), PhoneNumber: participant.PhoneNumber.String(),
				IsAdmin: participant.IsAdmin, IsSuperAdmin: participant.IsSuperAdmin, ErrorCode: participant.Error,
			})
		}
		writeCapabilityJSON(w, http.StatusOK, capabilityResponse{Success: true, Data: response, Message: "Group participants updated"})
	})

	mux.HandleFunc("/api/chat/read-state", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request chatReadStateRequest
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		jid, err := parseChatJID(request.ChatJID)
		if err != nil {
			writeCapabilityError(w, http.StatusBadRequest, err)
			return
		}
		patch := appstate.BuildMarkChatAsRead(jid, request.Read, lastChatMessageTime(messageStore, jid.String()), nil)
		if err = client.SendAppState(r.Context(), patch); err != nil {
			writeCapabilityError(w, http.StatusBadGateway, fmt.Errorf("failed to update chat read state: %w", err))
			return
		}
		if err = messageStore.SetChatReadState(jid.String(), request.Read); err != nil {
			writeCapabilityError(w, http.StatusInternalServerError, fmt.Errorf("failed to store chat read state: %w", err))
			return
		}
		writeCapabilityJSON(w, http.StatusOK, capabilityResponse{Success: true, Message: "Chat read state updated"})
	})

	mux.HandleFunc("/api/contacts/is-on-whatsapp", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request whatsappLookupRequest
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		if len(request.PhoneNumbers) == 0 || len(request.PhoneNumbers) > 50 {
			writeCapabilityError(w, http.StatusBadRequest, fmt.Errorf("provide between 1 and 50 phone numbers"))
			return
		}
		phones := make([]string, 0, len(request.PhoneNumbers))
		for _, value := range request.PhoneNumbers {
			normalized := normalizePhoneNumber(value)
			if normalized == "" {
				writeCapabilityError(w, http.StatusBadRequest, fmt.Errorf("invalid phone number %q", value))
				return
			}
			phones = append(phones, "+"+normalized)
		}
		results, err := client.IsOnWhatsApp(r.Context(), phones)
		if err != nil {
			writeCapabilityError(w, http.StatusBadGateway, fmt.Errorf("WhatsApp lookup failed: %w", err))
			return
		}
		response := make([]map[string]any, 0, len(results))
		for _, result := range results {
			verifiedName := ""
			if result.VerifiedName != nil && result.VerifiedName.Details != nil {
				verifiedName = result.VerifiedName.Details.GetVerifiedName()
			}
			response = append(response, map[string]any{
				"query": result.Query, "is_on_whatsapp": result.IsIn, "jid": result.JID.String(),
				"phone_number": result.PhoneNumber.User, "verified_name": verifiedName,
			})
		}
		writeCapabilityJSON(w, http.StatusOK, capabilityResponse{Success: true, Data: response})
	})

	mux.HandleFunc("/api/chat/mute", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request muteChatRequest
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		jid, err := parseChatJID(request.ChatJID)
		if err != nil || request.DurationSeconds < 0 {
			if err == nil {
				err = fmt.Errorf("duration_seconds cannot be negative")
			}
			writeCapabilityError(w, http.StatusBadRequest, err)
			return
		}
		if err = client.SendAppState(r.Context(), appstate.BuildMute(jid, request.Muted, time.Duration(request.DurationSeconds)*time.Second)); err != nil {
			writeCapabilityError(w, http.StatusBadGateway, fmt.Errorf("failed to update mute state: %w", err))
			return
		}
		writeCapabilityJSON(w, http.StatusOK, capabilityResponse{Success: true, Message: "Chat mute state updated"})
	})

	mux.HandleFunc("/api/chat/archive", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request archiveChatRequest
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		jid, err := parseChatJID(request.ChatJID)
		if err != nil {
			writeCapabilityError(w, http.StatusBadRequest, err)
			return
		}
		patch := appstate.BuildArchive(jid, request.Archived, lastChatMessageTime(messageStore, jid.String()), nil)
		if err = client.SendAppState(r.Context(), patch); err != nil {
			writeCapabilityError(w, http.StatusBadGateway, fmt.Errorf("failed to update archive state: %w", err))
			return
		}
		if err = messageStore.SetChatArchived(jid.String(), request.Archived); err != nil {
			writeCapabilityError(w, http.StatusInternalServerError, fmt.Errorf("failed to store chat archive state: %w", err))
			return
		}
		writeCapabilityJSON(w, http.StatusOK, capabilityResponse{Success: true, Message: "Chat archive state updated"})
	})

	mux.HandleFunc("/api/contact/block", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request blockContactRequest
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		jid, err := parseUserJID(request.JID)
		if err != nil {
			writeCapabilityError(w, http.StatusBadRequest, err)
			return
		}
		action := events.BlocklistChangeActionUnblock
		if request.Blocked {
			action = events.BlocklistChangeActionBlock
		}
		blocklist, err := client.UpdateBlocklist(r.Context(), jid, action)
		if err != nil {
			writeCapabilityError(w, http.StatusBadGateway, fmt.Errorf("failed to update block state: %w", err))
			return
		}
		writeCapabilityJSON(w, http.StatusOK, capabilityResponse{Success: true, Data: map[string]any{"blocked_count": len(blocklist.JIDs)}, Message: "Contact block state updated"})
	})

	mux.HandleFunc("/api/chat/presence", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request chatPresenceRequest
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		jid, err := parseChatJID(request.ChatJID)
		if err != nil {
			writeCapabilityError(w, http.StatusBadRequest, err)
			return
		}
		states := map[string]types.ChatPresence{"composing": types.ChatPresenceComposing, "paused": types.ChatPresencePaused}
		state, valid := states[strings.ToLower(request.State)]
		if !valid {
			writeCapabilityError(w, http.StatusBadRequest, fmt.Errorf("state must be composing or paused"))
			return
		}
		media := types.ChatPresenceMediaText
		if request.Media != "" {
			if strings.ToLower(request.Media) != "audio" {
				writeCapabilityError(w, http.StatusBadRequest, fmt.Errorf("media must be empty or audio"))
				return
			}
			media = types.ChatPresenceMediaAudio
		}
		presenceContext, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err = client.SendChatPresence(presenceContext, jid, state, media); err != nil {
			writeCapabilityError(w, http.StatusBadGateway, fmt.Errorf("failed to send typing state: %w", err))
			return
		}
		writeCapabilityJSON(w, http.StatusOK, capabilityResponse{Success: true, Message: "Chat presence updated"})
	})
}
