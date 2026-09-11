package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// Keep the original sender namespace and a shallow quote, not a recursively
// nested reply chain. Older text rows can be quoted using verified identities.
func quotePayload(msg *waProto.Message) (*waProto.Message, error) {
	if msg == nil {
		return nil, fmt.Errorf("message payload is unavailable")
	}
	q := &waProto.Message{}
	switch {
	case msg.GetConversation() != "":
		q.Conversation = proto.String(msg.GetConversation())
	case msg.ExtendedTextMessage != nil:
		q.Conversation = proto.String(msg.ExtendedTextMessage.GetText())
	case msg.ImageMessage != nil:
		q.ImageMessage = proto.Clone(msg.ImageMessage).(*waProto.ImageMessage)
		q.ImageMessage.ContextInfo = nil
	case msg.VideoMessage != nil:
		q.VideoMessage = proto.Clone(msg.VideoMessage).(*waProto.VideoMessage)
		q.VideoMessage.ContextInfo = nil
	case msg.DocumentMessage != nil:
		q.DocumentMessage = proto.Clone(msg.DocumentMessage).(*waProto.DocumentMessage)
		q.DocumentMessage.ContextInfo = nil
	case msg.AudioMessage != nil:
		q.AudioMessage = proto.Clone(msg.AudioMessage).(*waProto.AudioMessage)
		q.AudioMessage.ContextInfo = nil
	case msg.StickerMessage != nil:
		q.StickerMessage = proto.Clone(msg.StickerMessage).(*waProto.StickerMessage)
		q.StickerMessage.ContextInfo = nil
	default:
		return nil, fmt.Errorf("this message type cannot be quoted")
	}
	return q, nil
}

func (s *MessageStore) storeQuote(id, chat, sender string, msg *waProto.Message) error {
	q, err := quotePayload(msg)
	if err != nil {
		return err
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(q)
	if err != nil {
		return err
	}
	contextInfo := msg.GetExtendedTextMessage().GetContextInfo()
	_, err = s.db.Exec(`INSERT OR REPLACE INTO message_quotes (id,chat_jid,sender_jid,payload,reply_to_message_id,reply_to_sender_jid) VALUES (?,?,?,?,?,?)`, id, chat, sender, data, contextInfo.GetStanzaID(), contextInfo.GetParticipant())
	return err
}

type replyTarget struct {
	ChatJID     string `json:"chat_jid"`
	MessageID   string `json:"message_id"`
	SenderJID   string `json:"sender_jid"`
	Text        string `json:"text"`
	MediaType   string `json:"media_type,omitempty"`
	Fingerprint string `json:"fingerprint"`
	payload     *waProto.Message
}

func replyChat(value string) (types.JID, error) {
	jid, err := parseChatJID(value)
	if err != nil {
		return types.EmptyJID, err
	}
	if jid.User == "" || (jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer && jid.Server != types.GroupServer) {
		return types.EmptyJID, fmt.Errorf("reply requires a direct or group chat JID")
	}
	return jid, nil
}

func (s *MessageStore) resolveReply(ctx context.Context, client *whatsmeow.Client, chat, id string) (*replyTarget, error) {
	jid, err := replyChat(chat)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("message_id is required")
	}
	t := &replyTarget{ChatJID: jid.String(), MessageID: id}
	var sender string
	var fromMe bool
	err = s.db.QueryRow(`SELECT COALESCE(sender,''),COALESCE(content,''),COALESCE(media_type,''),is_from_me FROM messages WHERE chat_jid=? AND id=?`, t.ChatJID, id).Scan(&sender, &t.Text, &t.MediaType, &fromMe)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("message not found in this exact chat; use its stored chat JID")
	}
	if err != nil {
		return nil, err
	}
	var raw []byte
	err = s.db.QueryRow(`SELECT sender_jid,payload FROM message_quotes WHERE chat_jid=? AND id=?`, t.ChatJID, id).Scan(&t.SenderJID, &raw)
	if err == nil {
		t.payload = &waProto.Message{}
		if err = proto.Unmarshal(raw, t.payload); err != nil {
			return nil, err
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		if t.MediaType != "" || t.Text == "" {
			return nil, fmt.Errorf("original media payload is unavailable; only newly captured media can be quoted")
		}
		switch {
		case fromMe:
			if client.Store.ID == nil {
				return nil, fmt.Errorf("own identity unavailable")
			}
			t.SenderJID = client.Store.ID.ToNonAD().String()
		case strings.Contains(sender, "@"):
			sj, e := replyChat(sender)
			if e != nil || sj.Server == types.GroupServer {
				return nil, fmt.Errorf("invalid original sender")
			}
			t.SenderJID = sj.String()
		case jid.Server != types.GroupServer:
			t.SenderJID = jid.String()
		default:
			// Legacy live group rows lost the sender namespace. Require a LID mapping
			// instead of guessing that a bare numeric identifier is a phone number.
			if client.Store.LIDs == nil {
				return nil, fmt.Errorf("original sender namespace unavailable")
			}
			lid := types.NewJID(sender, types.HiddenUserServer)
			pn, e := client.Store.LIDs.GetPNForLID(ctx, lid)
			if e != nil || pn.IsEmpty() {
				return nil, fmt.Errorf("original sender namespace unavailable; resync the message")
			}
			t.SenderJID = lid.String()
		}
		t.payload = &waProto.Message{Conversation: proto.String(t.Text)}
		raw, _ = proto.MarshalOptions{Deterministic: true}.Marshal(t.payload)
	} else {
		return nil, err
	}
	// Bind confirmation to both the selected message and its original contents.
	h := sha256.New()
	for _, v := range []string{t.ChatJID, t.MessageID, t.SenderJID} {
		h.Write([]byte(v))
		h.Write([]byte{0})
	}
	h.Write(raw)
	t.Fingerprint = hex.EncodeToString(h.Sum(nil))
	return t, nil
}

func buildReply(text string, t *replyTarget) *waProto.Message {
	return &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
		Text: proto.String(text), ContextInfo: &waProto.ContextInfo{
			StanzaID: proto.String(t.MessageID), Participant: proto.String(t.SenderJID), QuotedMessage: t.payload,
		},
	}}
}

type replyRequest struct {
	ChatJID     string `json:"chat_jid"`
	MessageID   string `json:"message_id"`
	Message     string `json:"message"`
	Fingerprint string `json:"fingerprint"`
}

type replySender func(context.Context, types.JID, *waProto.Message) (string, error)

func registerReplyHandlers(mux *http.ServeMux, client *whatsmeow.Client, s *MessageStore, broker *eventBroker) {
	registerReplyRoutes(mux, client, s, broker, func(ctx context.Context, jid types.JID, msg *waProto.Message) (string, error) {
		if !client.IsConnected() || !client.IsLoggedIn() {
			return "", fmt.Errorf("bridge is not connected and logged in")
		}
		r, e := client.SendMessage(ctx, jid, msg)
		return r.ID, e
	})
}

func registerReplyRoutes(mux *http.ServeMux, client *whatsmeow.Client, s *MessageStore, broker *eventBroker, send replySender) {
	respond := func(w http.ResponseWriter, status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	fail := func(w http.ResponseWriter, status int, err error) {
		respond(w, status, map[string]any{"success": false, "message": err.Error()})
	}
	mux.HandleFunc("/api/reply/preview", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			fail(w, 405, fmt.Errorf("method not allowed"))
			return
		}
		t, err := s.resolveReply(r.Context(), client, r.URL.Query().Get("chat_jid"), r.URL.Query().Get("message_id"))
		if err != nil {
			fail(w, 400, err)
			return
		}
		respond(w, 200, map[string]any{"success": true, "data": t})
	})
	mux.HandleFunc("/api/reply", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			fail(w, 405, fmt.Errorf("method not allowed"))
			return
		}
		var req replyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, 400, fmt.Errorf("invalid request"))
			return
		}
		if strings.TrimSpace(req.Message) == "" || req.Fingerprint == "" {
			fail(w, 400, fmt.Errorf("message and preview fingerprint are required"))
			return
		}
		t, err := s.resolveReply(r.Context(), client, req.ChatJID, req.MessageID)
		if err != nil {
			fail(w, 400, err)
			return
		}
		if t.Fingerprint != req.Fingerprint {
			fail(w, 409, fmt.Errorf("quoted message changed; preview and confirm again"))
			return
		}
		if client.Store.ID == nil {
			fail(w, 503, fmt.Errorf("own identity unavailable"))
			return
		}
		msg := buildReply(req.Message, t)
		id, err := send(r.Context(), mustParseJID(t.ChatJID), msg)
		if err != nil {
			fail(w, 502, err)
			return
		}
		now := time.Now()
		sender := client.Store.ID.ToNonAD().String()
		// A successful provider send must never be reported as a failure merely
		// because local persistence failed: that would encourage duplicate retries.
		warning := ""
		if _, err = s.db.Exec(`UPDATE chats SET last_message_time=? WHERE jid=?`, now, t.ChatJID); err != nil {
			warning = "Sent, but local chat timestamp update failed"
		}
		if err = s.StoreMessage(id, t.ChatJID, sender, req.Message, now, true, "", "", "", nil, nil, nil, 0); err != nil {
			warning = "Sent, but local message storage failed"
		} else {
			if err = s.storeQuote(id, t.ChatJID, sender, msg); err != nil {
				warning = "Sent, but quote storage failed"
			}
			if broker != nil {
				broker.publish(bridgeEvent{Type: "message", ID: id, ChatJID: t.ChatJID, Sender: sender, Content: req.Message, Timestamp: now.Format(time.RFC3339), IsFromMe: true})
			}
		}
		respond(w, 200, map[string]any{"success": true, "message_id": id, "chat_jid": t.ChatJID, "reply_to_message_id": t.MessageID, "warning": warning})
	})
}
