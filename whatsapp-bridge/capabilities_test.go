package main

import (
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestNormalizePhoneNumber(t *testing.T) {
	if got := normalizePhoneNumber("+27 (82) 345-6789"); got != "27823456789" {
		t.Fatalf("normalizePhoneNumber() = %q", got)
	}
	if got := normalizePhoneNumber("0027 82 345 6789"); got != "27823456789" {
		t.Fatalf("normalizePhoneNumber() with international prefix = %q", got)
	}
}

func TestParseUserJID(t *testing.T) {
	jid, err := parseUserJID("+27823456789")
	if err != nil {
		t.Fatal(err)
	}
	if jid.User != "27823456789" || jid.Server != types.DefaultUserServer {
		t.Fatalf("unexpected JID: %s", jid)
	}
	if _, err = parseUserJID("120363000000000000@g.us"); err == nil {
		t.Fatal("expected group JID to be rejected as a user")
	}
}

func TestParseGroupJID(t *testing.T) {
	jid, err := parseGroupJID("120363000000000000@g.us")
	if err != nil {
		t.Fatal(err)
	}
	if jid.Server != types.GroupServer {
		t.Fatalf("unexpected group server: %s", jid.Server)
	}
	if _, err = parseGroupJID("27823456789@s.whatsapp.net"); err == nil {
		t.Fatal("expected direct-chat JID to be rejected as a group")
	}
}

func TestParseUserJIDsDeduplicates(t *testing.T) {
	jids, err := parseUserJIDs([]string{"+27823456789", "27823456789"})
	if err != nil {
		t.Fatal(err)
	}
	if len(jids) != 1 {
		t.Fatalf("got %d JIDs, want 1", len(jids))
	}
}
