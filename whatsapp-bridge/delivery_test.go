package main

import (
	"encoding/json"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestDeliveryHTTPReceiptLifecycle(t *testing.T) {
	s, c, chat := replyFixture(t)
	mux := http.NewServeMux()
	registerDeliveryRoutes(mux, c, s)
	get := func(target, id string) deliveryStatus {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/message-status?chat_jid="+url.QueryEscape(target)+"&message_id="+id, nil))
		if w.Code != 200 {
			t.Fatalf("%d: %s", w.Code, w.Body.String())
		}
		var d deliveryStatus
		if e := json.Unmarshal(w.Body.Bytes(), &d); e != nil {
			t.Fatal(e)
		}
		return d
	}
	receipt := func(kind types.ReceiptType, id string) {
		t.Helper()
		if e := s.RecordDeliveryReceipt(&events.Receipt{MessageSource: types.MessageSource{Chat: mustParseJID(chat), Sender: mustParseJID(chat)}, MessageIDs: []string{id}, Type: kind, Timestamp: time.Now()}); e != nil {
			t.Fatal(e)
		}
	}
	receipt(types.ReceiptTypeRead, "out")
	_ = s.RecordOutboundAcceptance(chat, "out")
	if d := get(chat, "out"); !d.DeliveryConfirmed || d.DeliveryStatus != "read" {
		t.Fatalf("receipt before storage lost: %+v", d)
	}
	receipt(types.ReceiptTypeSender, "out")
	receipt(types.ReceiptTypeRetry, "out")
	_ = s.StoreMessage("out", chat, "me", "history", time.Now(), true, "", "", "", nil, nil, nil, 0)
	if get(chat, "out").DeliveryStatus != "read" {
		t.Fatal("read receipt regressed")
	}
	_ = s.RecordOutboundAcceptance(chat, "pending")
	for _, kind := range []types.ReceiptType{types.ReceiptTypeSender, types.ReceiptTypeReadSelf, types.ReceiptTypeInactive, types.ReceiptType("future-kind")} {
		receipt(kind, "pending")
	}
	if d := get(chat, "pending"); d.DeliveryConfirmed || d.DeliveryStatus != "pending" {
		t.Fatalf("unrelated receipt counted: %+v", d)
	}
	receipt(types.ReceiptTypeDelivered, "pending")
	if !get(chat, "pending").DeliveryConfirmed {
		t.Fatal("delivery missed")
	}
	_ = s.SetMessageStatus(chat, "original", "read")
	if get(chat, "original").DeliveryConfirmed {
		t.Fatal("legacy status trusted")
	}
	other := "99900003@s.whatsapp.net"
	_ = s.RecordOutboundAcceptance(other, "out")
	if get(other, "out").DeliveryConfirmed {
		t.Fatal("wrong recipient matched")
	}
}
func TestDeliveryOwnGroupAndAliasReceipts(t *testing.T) {
	s, c, chat := replyFixture(t)
	mux := http.NewServeMux()
	registerDeliveryRoutes(mux, c, s)
	get := func(target, id string) deliveryStatus {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/message-status?chat_jid="+url.QueryEscape(target)+"&message_id="+id, nil))
		var d deliveryStatus
		_ = json.Unmarshal(w.Body.Bytes(), &d)
		return d
	}
	_ = s.RecordOutboundAcceptance(chat, "self")
	_ = s.RecordDeliveryReceipt(&events.Receipt{MessageSource: types.MessageSource{Chat: mustParseJID(chat), Sender: *c.Store.ID, IsFromMe: true}, MessageIDs: []string{"self"}, Type: types.ReceiptTypeRead, Timestamp: time.Now()})
	if get(chat, "self").DeliveryConfirmed {
		t.Fatal("own device counted")
	}
	group := "123456@g.us"
	_ = s.RecordOutboundAcceptance(group, "group")
	_ = s.RecordDeliveryReceipt(&events.Receipt{MessageSource: types.MessageSource{Chat: mustParseJID(group), Sender: mustParseJID(chat), IsGroup: true}, MessageIDs: []string{"group"}, Timestamp: time.Now()})
	if d := get(group, "group"); d.DeliveryConfirmed || d.DeliveryStatus != "partial_delivery" {
		t.Fatalf("group overclaimed: %+v", d)
	}
	lid := "12345@lid"
	_ = s.RecordOutboundAcceptance(lid, "alias")
	_ = s.RecordDeliveryReceipt(&events.Receipt{MessageSource: types.MessageSource{Chat: mustParseJID(chat), Sender: mustParseJID(chat), SenderAlt: mustParseJID(lid)}, MessageIDs: []string{"alias"}, Timestamp: time.Now()})
	if !get(lid, "alias").DeliveryConfirmed {
		t.Fatal("PN/LID alias missed")
	}
}
func TestDeliveryWaitBoundaries(t *testing.T) {
	s, c, chat := replyFixture(t)
	mux := http.NewServeMux()
	registerDeliveryRoutes(mux, c, s)
	_ = s.RecordOutboundAcceptance(chat, "waiting")
	for _, wait := range []string{"-1", "31", "NaN"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/message-status?chat_jid="+url.QueryEscape(chat)+"&message_id=waiting&wait_seconds="+wait, nil))
		if w.Code != 400 {
			t.Fatalf("accepted wait %s", wait)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(50 * time.Millisecond)
		_ = s.RecordDeliveryReceipt(&events.Receipt{MessageSource: types.MessageSource{Chat: mustParseJID(chat), Sender: mustParseJID(chat)}, MessageIDs: []string{"waiting"}, Timestamp: time.Now()})
	}()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/message-status?chat_jid="+url.QueryEscape(chat)+"&message_id=waiting&wait_seconds=1", nil))
	<-done
	var d deliveryStatus
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if !d.DeliveryConfirmed {
		t.Fatalf("wait missed receipt: %s", w.Body.String())
	}
	t.Logf("HTTP/SQLite evidence: %s", w.Body.String())
}

func TestDeliveryHistoryAndEventsUseVerifiedFold(t *testing.T) {
	s, c, chat := replyFixture(t)
	mux := http.NewServeMux()
	registerCapabilityHandlers(mux, c, s)
	broker := newEventBroker(10)
	_ = s.RecordOutboundAcceptance(chat, "out")
	_ = s.StoreMessage("out", chat, "me", "text", time.Now(), true, "", "", "", nil, nil, nil, 0)
	receipt := &events.Receipt{MessageSource: types.MessageSource{Chat: mustParseJID(chat), Sender: mustParseJID(chat)}, MessageIDs: []string{"out"}, Type: types.ReceiptTypeRead, Timestamp: time.Now()}
	if err := s.handleDeliveryReceipt(broker, receipt); err != nil {
		t.Fatal(err)
	}
	receipt.Type = types.ReceiptTypeRetry
	if err := s.handleDeliveryReceipt(broker, receipt); err != nil {
		t.Fatal(err)
	}
	for _, event := range broker.history {
		if event.Data.Status != "read" {
			t.Fatalf("SSE regression: %+v", event.Data)
		}
	}
	n := len(broker.history)
	receipt.Sender = mustParseJID("123@lid")
	receipt.Type = types.ReceiptTypeDelivered
	_ = s.handleDeliveryReceipt(broker, receipt)
	if len(broker.history) != n {
		t.Fatal("ignored receipt was published")
	}
	// Simulate the existing history-sync replacement behavior.
	_ = s.StoreMessage("out", chat, "me", "text", time.Now(), true, "", "", "", nil, nil, nil, 0)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/messages?chat_jid="+url.QueryEscape(chat), nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Data []messageListItem `json:"data"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &result); e != nil {
		t.Fatal(e)
	}
	for _, m := range result.Data {
		if m.ID == "out" && m.Status != "read" {
			t.Fatalf("history diverged: %s", w.Body.String())
		}
	}
}
