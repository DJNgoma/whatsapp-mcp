package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func replyFixture(t *testing.T) (*MessageStore, *whatsmeow.Client, string) {
	t.Helper()
	s, e := NewMessageStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.db.Close() })
	own := types.NewJID("99900001", types.DefaultUserServer)
	c := whatsmeow.NewClient(&store.Device{ID: &own}, nil)
	chat := "99900002@s.whatsapp.net"
	if e = s.StoreChat(chat, "Fixture", time.Now()); e != nil {
		t.Fatal(e)
	}
	if e = s.StoreMessage("original", chat, "99900002", "Original text", time.Now(), false, "", "", "", nil, nil, nil, 0); e != nil {
		t.Fatal(e)
	}
	return s, c, chat
}

func TestReplyLegacyTextAndExactChat(t *testing.T) {
	s, c, chat := replyFixture(t)
	q, e := s.resolveReply(context.Background(), c, chat, "original")
	if e != nil {
		t.Fatal(e)
	}
	ctx := buildReply("Reply", q).GetExtendedTextMessage().GetContextInfo()
	if ctx.GetStanzaID() != "original" || ctx.GetParticipant() != chat || ctx.GetQuotedMessage().GetConversation() != "Original text" {
		t.Fatalf("bad quote: %v", ctx)
	}
	if _, e = s.resolveReply(context.Background(), c, "99900003@s.whatsapp.net", "original"); e == nil {
		t.Fatal("cross-chat quote accepted")
	}
	if _, e = s.resolveReply(context.Background(), c, "status@broadcast", "original"); e == nil {
		t.Fatal("broadcast accepted")
	}
	if _, e = s.resolveReply(context.Background(), c, chat, "missing"); e == nil {
		t.Fatal("missing target accepted")
	}
	// Raw phone-number form resolves to the same exact stored chat.
	phone, e := s.resolveReply(context.Background(), c, "99900002", "original")
	if e != nil || phone.Fingerprint != q.Fingerprint {
		t.Fatal("phone canonicalization failed", e)
	}
}

func TestReplyOwnAndAmbiguousGroupSender(t *testing.T) {
	s, c, chat := replyFixture(t)
	_, _ = s.db.Exec(`UPDATE messages SET is_from_me=1 WHERE id='original'`)
	q, e := s.resolveReply(context.Background(), c, chat, "original")
	if e != nil || q.SenderJID != c.Store.ID.String() {
		t.Fatal(q, e)
	}
	group := "123456@g.us"
	_ = s.StoreChat(group, "Group", time.Now())
	_ = s.StoreMessage("group", group, "12345", "Group text", time.Now(), false, "", "", "", nil, nil, nil, 0)
	if _, e = s.resolveReply(context.Background(), c, group, "group"); e == nil {
		t.Fatal("ambiguous sender accepted")
	}
	_ = s.storeQuote("group", group, "12345@lid", &waProto.Message{Conversation: proto.String("Group text")})
	q, e = s.resolveReply(context.Background(), c, group, "group")
	if e != nil || q.SenderJID != "12345@lid" {
		t.Fatal(q, e)
	}
}

func TestReplyMediaAndShallowQuotes(t *testing.T) {
	s, c, chat := replyFixture(t)
	_, _ = s.db.Exec(`UPDATE messages SET media_type='image' WHERE id='original'`)
	if _, e := s.resolveReply(context.Background(), c, chat, "original"); e == nil {
		t.Fatal("legacy media quoted without original payload")
	}
	msg := &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("Photo"), ContextInfo: &waProto.ContextInfo{StanzaID: proto.String("older")}}}
	if e := s.storeQuote("original", chat, chat, msg); e != nil {
		t.Fatal(e)
	}
	q, e := s.resolveReply(context.Background(), c, chat, "original")
	if e != nil {
		t.Fatal(e)
	}
	if q.payload.GetImageMessage().GetCaption() != "Photo" || q.payload.GetImageMessage().ContextInfo != nil {
		t.Fatal("media quote lost or nested context retained")
	}
	if msg.ImageMessage.ContextInfo == nil {
		t.Fatal("original payload mutated")
	}
}

func TestReplyRoutesRejectChangedTargetAndPersistSuccess(t *testing.T) {
	s, c, chat := replyFixture(t)
	mux := http.NewServeMux()
	calls := 0
	failSend := false
	registerReplyRoutes(mux, c, s, nil, func(_ context.Context, jid types.JID, msg *waProto.Message) (string, error) {
		calls++
		if failSend {
			return "", errors.New("offline")
		}
		if jid.String() != chat || msg.GetExtendedTextMessage().GetContextInfo().GetStanzaID() != "original" {
			t.Fatal("wrong provider payload")
		}
		return "reply-id", nil
	})
	preview := httptest.NewRecorder()
	mux.ServeHTTP(preview, httptest.NewRequest("GET", "/api/reply/preview?chat_jid="+chat+"&message_id=original", nil))
	if preview.Code != 200 || calls != 0 {
		t.Fatal("preview sent", preview.Code)
	}
	q, _ := s.resolveReply(context.Background(), c, chat, "original")
	post := func(fp string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(replyRequest{ChatJID: chat, MessageID: "original", Message: "Reply", Fingerprint: fp})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/reply", strings.NewReader(string(body))))
		return w
	}
	if w := post(""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := post("wrong"); w.Code != 409 || calls != 0 {
		t.Fatal("fingerprint bypass")
	}
	_, _ = s.db.Exec(`UPDATE messages SET content='Edited' WHERE id='original'`)
	if w := post(q.Fingerprint); w.Code != 409 || calls != 0 {
		t.Fatal("changed target accepted")
	}
	q, _ = s.resolveReply(context.Background(), c, chat, "original")
	failSend = true
	if w := post(q.Fingerprint); w.Code != 502 {
		t.Fatal("send failure hidden")
	}
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM messages WHERE id='reply-id'`).Scan(&n)
	if n != 0 {
		t.Fatal("failed send persisted")
	}
	failSend = false
	if w := post(q.Fingerprint); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var text string
	_ = s.db.QueryRow(`SELECT content FROM messages WHERE id='reply-id' AND chat_jid=?`, chat).Scan(&text)
	var replyTo string
	_ = s.db.QueryRow(`SELECT reply_to_message_id FROM message_quotes WHERE id='reply-id' AND chat_jid=?`, chat).Scan(&replyTo)
	if replyTo != "original" {
		t.Fatal("reply association not stored")
	}
	if text != "Reply" {
		t.Fatal("reply not stored")
	}
	for _, path := range []string{"/api/reply", "/api/reply/preview"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("DELETE", path, nil))
		if w.Code != 405 {
			t.Fatal("method accepted")
		}
	}
}
