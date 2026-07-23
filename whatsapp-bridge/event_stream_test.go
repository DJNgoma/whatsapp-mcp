package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func readSSEFrame(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var frame strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE frame: %v", err)
		}
		frame.WriteString(line)
		if line == "\n" {
			return frame.String()
		}
	}
}

func TestEventBrokerReplaysEventsAfterCursor(t *testing.T) {
	broker := newEventBrokerWithInstanceID(4, "test")
	first := broker.publish(bridgeEvent{Type: "message", ID: "one"})
	second := broker.publish(bridgeEvent{Type: "message", ID: "two"})

	events, cancel, reset := broker.subscribe(first.ID)
	defer cancel()
	if reset {
		t.Fatal("expected cursor to be replayable")
	}
	select {
	case replay := <-events:
		if replay.ID != second.ID || replay.Data.ID != "two" {
			t.Fatalf("unexpected replay: %#v", replay)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for replay")
	}
}

func TestEventBrokerRequestsResetForExpiredCursor(t *testing.T) {
	broker := newEventBrokerWithInstanceID(1, "test")
	first := broker.publish(bridgeEvent{Type: "message", ID: "one"})
	broker.publish(bridgeEvent{Type: "message", ID: "two"})

	_, cancel, reset := broker.subscribe(first.ID)
	defer cancel()
	if !reset {
		t.Fatal("expected expired cursor to request reconciliation")
	}
}

func TestEventStreamEmitsMessageAndCleansUpSubscriber(t *testing.T) {
	broker := newEventBrokerWithInstanceID(8, "test")
	server := httptest.NewServer(eventStreamHandler(broker, time.Hour))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	reader := bufio.NewReader(response.Body)
	if frame := readSSEFrame(t, reader); !strings.Contains(frame, ": connected") {
		t.Fatalf("missing connected frame: %q", frame)
	}

	published := broker.publish(bridgeEvent{Type: "message", ID: "message-1", ChatJID: "chat@g.us", Content: "hello"})
	frame := readSSEFrame(t, reader)
	for strings.HasPrefix(frame, ":") {
		frame = readSSEFrame(t, reader)
	}
	if !strings.Contains(frame, "id: "+published.ID) || !strings.Contains(frame, "event: message") || !strings.Contains(frame, `"content":"hello"`) || !strings.Contains(frame, `"is_from_me":false`) {
		t.Fatalf("unexpected message frame: %q", frame)
	}

	cancel()
	deadline := time.Now().Add(time.Second)
	for broker.subscriberCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if count := broker.subscriberCount(); count != 0 {
		t.Fatalf("subscriber count = %d, want 0", count)
	}
}

func TestEventStreamRejectsNonGET(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/events", nil)
	response := httptest.NewRecorder()
	eventStreamHandler(newEventBroker(2), time.Hour).ServeHTTP(response, request)
	result := response.Result()
	defer result.Body.Close()
	if result.StatusCode != http.StatusMethodNotAllowed {
		body, _ := io.ReadAll(result.Body)
		t.Fatalf("status = %d, body = %q", result.StatusCode, body)
	}
}
