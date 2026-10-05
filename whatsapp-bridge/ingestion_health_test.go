package main

import (
	"errors"
	"testing"
	"time"
)

func TestKeepaliveRecoveryIsBounded(t *testing.T) {
	h := &ingestionHealth{}
	now := time.Now()
	if h.shouldReconnect(1, now) {
		t.Fatal("single timeout must not trigger reconnect")
	}
	if !h.shouldReconnect(2, now) {
		t.Fatal("repeated timeout must trigger recovery")
	}
	if h.shouldReconnect(3, now.Add(time.Minute)) {
		t.Fatal("reconnect storm")
	}
	if !h.shouldReconnect(2, now.Add(6*time.Minute)) {
		t.Fatal("later recovery suppressed")
	}
	h.keepaliveRestored()
	if h.snapshot()["keepalive_failed"].(bool) {
		t.Fatal("restoration not recorded")
	}
}

func TestStorageFailureCannotBeHiddenByLaterSuccess(t *testing.T) {
	h := &ingestionHealth{}
	h.recordWrite(errors.New("locked"))
	h.recordWrite(nil)
	if h.snapshot()["storage_failures"].(uint64) != 1 {
		t.Fatal("lost failure evidence")
	}
	if h.snapshot()["last_successful_write"].(time.Time).IsZero() {
		t.Fatal("missing write evidence")
	}
}

func TestDisconnectInvalidatesOfflineSync(t *testing.T) {
	h := &ingestionHealth{}
	h.offlineSyncCompleted()
	h.disconnected()
	if !h.snapshot()["offline_sync_completed_at"].(time.Time).IsZero() {
		t.Fatal("stale sync evidence")
	}
}

func TestBackfillCannotRegressChatTime(t *testing.T) {
	s, err := NewMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().Truncate(time.Second)
	if err := s.StoreChat("test@s.whatsapp.net", "test", now); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreChat("test@s.whatsapp.net", "test", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	var actual time.Time
	if err := s.db.QueryRow("SELECT last_message_time FROM chats WHERE jid = ?", "test@s.whatsapp.net").Scan(&actual); err != nil {
		t.Fatal(err)
	}
	if !actual.Equal(now) {
		t.Fatalf("backfill regressed chat timestamp to %v", actual)
	}
	var mode string
	var timeout int
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" || timeout != 5000 {
		t.Fatalf("unsafe database settings: %s %d", mode, timeout)
	}
}
