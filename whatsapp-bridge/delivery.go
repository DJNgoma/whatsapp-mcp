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

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// A separate receipt ledger keeps evidence even if a receipt arrives before
// message persistence, and prevents history replays from erasing that evidence.
func (s *MessageStore) initDeliveryStore() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS outbound_acceptances (
 chat_jid TEXT NOT NULL, message_id TEXT NOT NULL, accepted_at TEXT NOT NULL,
 PRIMARY KEY(chat_jid,message_id));
 CREATE TABLE IF NOT EXISTS recipient_receipts (
 chat_jid TEXT NOT NULL, message_id TEXT NOT NULL, sender_jid TEXT NOT NULL,
 receipt_type TEXT NOT NULL, state TEXT NOT NULL, rank INTEGER NOT NULL,
 receipt_at TEXT NOT NULL, observed_at TEXT NOT NULL,
 PRIMARY KEY(chat_jid,message_id,sender_jid,receipt_type));`)
	return err
}

func (s *MessageStore) RecordOutboundAcceptance(chat, id string) error {
	jid, err := deliveryJID(chat)
	if err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("message ID required")
	}
	_, err = s.db.Exec(`INSERT OR IGNORE INTO outbound_acceptances VALUES (?,?,?)`, jid.String(), id, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func deliveryReceiptState(r *events.Receipt) (string, int) {
	if r.IsFromMe || r.Sender.IsEmpty() {
		return "", 0
	}
	switch r.Type {
	case types.ReceiptTypeRead, types.ReceiptTypePlayed:
		return "read", 3
	case types.ReceiptTypeDelivered:
		return "delivered", 2
	case types.ReceiptTypeRetry:
		return "retrying", 1
	case types.ReceiptTypeServerError:
		return "failed", 1
	default:
		return "", 0 // Own-device and protocol receipts do not prove recipient delivery.
	}
}

func (s *MessageStore) RecordDeliveryReceipt(r *events.Receipt) error {
	state, rank := deliveryReceiptState(r)
	if rank == 0 {
		return nil
	}
	chats := deliveryReceiptChats(r)
	if len(chats) == 0 {
		return nil
	}
	sender := r.Sender.ToNonAD()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	receiptAt := ""
	if !r.Timestamp.IsZero() {
		receiptAt = r.Timestamp.UTC().Format(time.RFC3339Nano)
	}
	for _, target := range chats {
		for _, id := range r.MessageIDs {
			if id == "" {
				continue
			}
			if _, err = tx.Exec(`INSERT OR IGNORE INTO recipient_receipts VALUES (?,?,?,?,?,?,?,?)`, target, id, sender.String(), string(r.Type), state, rank, receiptAt, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func deliveryReceiptChats(r *events.Receipt) []string {
	if _, rank := deliveryReceiptState(r); rank == 0 {
		return nil
	}
	chat, sender := r.Chat.ToNonAD(), r.Sender.ToNonAD()
	direct := chat.Server == types.DefaultUserServer || chat.Server == types.HiddenUserServer
	if !direct && chat.Server != types.GroupServer {
		return nil
	}
	if direct && chat != sender {
		return nil
	}
	chats := []string{chat.String()}
	// SenderAlt is supplied by WhatsApp itself, so the exact same receipt can
	// safely match an outbound message addressed through the other namespace.
	if direct && !r.SenderAlt.IsEmpty() {
		alt := r.SenderAlt.ToNonAD()
		if (alt.Server == types.DefaultUserServer || alt.Server == types.HiddenUserServer) && alt.Server != chat.Server {
			chats = append(chats, alt.String())
		}
	}
	return chats
}

func (s *MessageStore) handleDeliveryReceipt(broker *eventBroker, r *events.Receipt) error {
	if err := s.RecordDeliveryReceipt(r); err != nil {
		return err
	}
	if broker == nil {
		return nil
	}
	for _, chat := range deliveryReceiptChats(r) {
		jid, _ := deliveryJID(chat)
		for _, id := range r.MessageIDs {
			d, err := s.deliveryStatus(context.Background(), jid, id, []string{chat})
			if err != nil {
				return err
			}
			if d.Success {
				broker.publish(bridgeEvent{Type: "receipt", ChatJID: chat, Status: d.DeliveryStatus, MessageIDs: []string{id}})
			}
		}
	}
	return nil
}

type deliveryStatus struct {
	Success           bool   `json:"success"`
	MessageID         string `json:"message_id"`
	ChatJID           string `json:"chat_jid"`
	SendStatus        string `json:"send_status"`
	DeliveryStatus    string `json:"delivery_status"`
	DeliveryConfirmed bool   `json:"delivery_confirmed"`
	ReceiptType       string `json:"receipt_type,omitempty"`
	ReceiptAt         string `json:"receipt_at,omitempty"`
	ObservedAt        string `json:"receipt_observed_at,omitempty"`
	Scope             string `json:"receipt_scope"`
	Message           string `json:"message"`
}

func deliveryJID(raw string) (types.JID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return types.EmptyJID, fmt.Errorf("chat_jid is required")
	}
	if !strings.Contains(raw, "@") {
		raw += "@" + types.DefaultUserServer
	}
	jid, err := types.ParseJID(raw)
	if err != nil || jid.User == "" {
		return types.EmptyJID, fmt.Errorf("invalid chat_jid")
	}
	if jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer && jid.Server != types.GroupServer {
		return types.EmptyJID, fmt.Errorf("unsupported chat namespace")
	}
	return jid.ToNonAD(), nil
}

func deliveryAliases(ctx context.Context, c *whatsmeow.Client, jid types.JID) []string {
	aliases := []string{jid.String()}
	if c == nil || c.Store == nil || c.Store.LIDs == nil {
		return aliases
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	var alt types.JID
	var err error
	if jid.Server == types.HiddenUserServer {
		alt, err = c.Store.LIDs.GetPNForLID(ctx, jid)
	} else if jid.Server == types.DefaultUserServer {
		alt, err = c.Store.LIDs.GetLIDForPN(ctx, jid)
	}
	if err == nil && !alt.IsEmpty() {
		aliases = append(aliases, alt.ToNonAD().String())
	}
	return aliases
}

func (s *MessageStore) deliveryStatus(ctx context.Context, jid types.JID, id string, aliases []string) (deliveryStatus, error) {
	d := deliveryStatus{MessageID: id, ChatJID: jid.String(), SendStatus: "unknown", DeliveryStatus: "unknown", Scope: "recipient", Message: "No verified outbound message found."}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(aliases)), ",")
	args := make([]any, 0, len(aliases)+1)
	for _, a := range aliases {
		args = append(args, a)
	}
	args = append(args, id)
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_acceptances WHERE chat_jid IN (`+placeholders+`) AND message_id=?`, args...).Scan(&count); err != nil {
		return d, err
	}
	if count > 0 {
		d.SendStatus = "accepted"
	} else {
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE chat_jid IN (`+placeholders+`) AND id=? AND is_from_me=TRUE`, args...).Scan(&count); err != nil {
			return d, err
		}
		if count == 0 {
			return d, nil
		}
		d.SendStatus = "recorded" // Historical rows alone do not prove server acceptance.
	}
	d.Success = true
	d.DeliveryStatus = "pending"
	d.Message = "Recipient delivery is not confirmed. Do not resend; check this message ID again."
	var state, kind, at, observed string
	var rank int
	err := s.db.QueryRowContext(ctx, `SELECT state,rank,receipt_type,receipt_at,observed_at FROM recipient_receipts WHERE chat_jid IN (`+placeholders+`) AND message_id=? ORDER BY rank DESC, observed_at DESC LIMIT 1`, args...).Scan(&state, &rank, &kind, &at, &observed)
	if err == sql.ErrNoRows {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	d.DeliveryStatus = state
	d.ReceiptType = kind
	d.ReceiptAt = at
	d.ObservedAt = observed
	d.DeliveryConfirmed = rank >= 2
	if jid.Server == types.GroupServer {
		d.Scope = "some_group_members"
		d.DeliveryConfirmed = false
		if rank >= 2 {
			d.DeliveryStatus = "partial_delivery"
		}
		d.Message = "Receipt from a group member only; delivery to the whole group is not confirmed. Do not resend."
	} else if d.DeliveryConfirmed {
		d.Message = "Recipient " + state + " receipt verified."
	}
	return d, nil
}

func registerDeliveryRoutes(mux *http.ServeMux, c *whatsmeow.Client, s *MessageStore) {
	mux.HandleFunc("/api/message-status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fail := func(code int, msg string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "delivery_confirmed": false, "message": msg})
		}
		if r.Method != http.MethodGet {
			fail(405, "method not allowed")
			return
		}
		jid, err := deliveryJID(r.URL.Query().Get("chat_jid"))
		id := strings.TrimSpace(r.URL.Query().Get("message_id"))
		if err != nil || id == "" {
			fail(400, "valid chat_jid and message_id are required")
			return
		}
		wait := 0
		if raw := r.URL.Query().Get("wait_seconds"); raw != "" {
			wait, err = strconv.Atoi(raw)
			if err != nil || wait < 0 || wait > 30 {
				fail(400, "wait_seconds must be an integer between 0 and 30")
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(wait)*time.Second+time.Second)
		defer cancel()
		aliases := deliveryAliases(ctx, c, jid)
		deadline := time.Now().Add(time.Duration(wait) * time.Second)
		for {
			d, e := s.deliveryStatus(ctx, jid, id, aliases)
			if e != nil {
				fail(503, "delivery evidence unavailable; do not resend")
				return
			}
			if wait == 0 || d.DeliveryConfirmed || d.DeliveryStatus == "failed" || !d.Success || time.Now().After(deadline) {
				_ = json.NewEncoder(w).Encode(d)
				return
			}
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-r.Context().Done():
				timer.Stop()
				return
			case <-ctx.Done():
				timer.Stop()
				fail(503, "delivery wait cancelled; do not resend")
				return
			case <-timer.C:
			}
		}
	})
}
