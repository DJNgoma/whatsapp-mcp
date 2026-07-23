package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const eventStreamHeartbeatInterval = 15 * time.Second

type bridgeEvent struct {
	Type        string   `json:"type"`
	ID          string   `json:"id,omitempty"`
	ChatJID     string   `json:"chat_jid,omitempty"`
	ChatName    string   `json:"chat_name,omitempty"`
	PhoneNumber string   `json:"phone_number,omitempty"`
	Sender      string   `json:"sender,omitempty"`
	Content     string   `json:"content,omitempty"`
	Timestamp   string   `json:"timestamp,omitempty"`
	IsFromMe    bool     `json:"is_from_me"`
	MediaType   string   `json:"media_type,omitempty"`
	Filename    string   `json:"filename,omitempty"`
	Status      string   `json:"status,omitempty"`
	MessageIDs  []string `json:"message_ids,omitempty"`
}

type sequencedBridgeEvent struct {
	ID   string
	Data bridgeEvent
}

type eventBroker struct {
	mu           sync.Mutex
	instanceID   string
	nextSequence uint64
	historyLimit int
	history      []sequencedBridgeEvent
	subscribers  map[chan sequencedBridgeEvent]struct{}
}

func newEventBroker(historyLimit int) *eventBroker {
	return newEventBrokerWithInstanceID(historyLimit, strconv.FormatInt(time.Now().UnixNano(), 36))
}

func newEventBrokerWithInstanceID(historyLimit int, instanceID string) *eventBroker {
	if historyLimit < 1 {
		historyLimit = 1
	}
	return &eventBroker{
		instanceID:   instanceID,
		historyLimit: historyLimit,
		subscribers:  make(map[chan sequencedBridgeEvent]struct{}),
	}
}

func (broker *eventBroker) publish(data bridgeEvent) sequencedBridgeEvent {
	broker.mu.Lock()
	defer broker.mu.Unlock()

	broker.nextSequence++
	event := sequencedBridgeEvent{
		ID:   fmt.Sprintf("%s:%d", broker.instanceID, broker.nextSequence),
		Data: data,
	}
	broker.history = append(broker.history, event)
	if len(broker.history) > broker.historyLimit {
		broker.history = broker.history[len(broker.history)-broker.historyLimit:]
	}

	for subscriber := range broker.subscribers {
		select {
		case subscriber <- event:
		default:
			delete(broker.subscribers, subscriber)
			close(subscriber)
		}
	}
	return event
}

func (broker *eventBroker) subscribe(lastEventID string) (<-chan sequencedBridgeEvent, func(), bool) {
	broker.mu.Lock()
	defer broker.mu.Unlock()

	bufferSize := broker.historyLimit + 16
	subscriber := make(chan sequencedBridgeEvent, bufferSize)
	replayAvailable := lastEventID == ""
	if lastEventID != "" {
		for index, event := range broker.history {
			if event.ID != lastEventID {
				continue
			}
			replayAvailable = true
			for _, replay := range broker.history[index+1:] {
				subscriber <- replay
			}
			break
		}
	}
	broker.subscribers[subscriber] = struct{}{}

	cancel := func() {
		broker.mu.Lock()
		defer broker.mu.Unlock()
		if _, exists := broker.subscribers[subscriber]; exists {
			delete(broker.subscribers, subscriber)
			close(subscriber)
		}
	}
	return subscriber, cancel, !replayAvailable
}

func (broker *eventBroker) subscriberCount() int {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return len(broker.subscribers)
}

func eventStreamHandler(broker *eventBroker, heartbeatInterval time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming is not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		events, cancel, reset := broker.subscribe(r.Header.Get("Last-Event-ID"))
		defer cancel()
		_, _ = fmt.Fprint(w, "retry: 3000\n: connected\n\n")
		if reset {
			_, _ = fmt.Fprint(w, "event: reset\ndata: {\"reason\":\"replay_unavailable\"}\n\n")
		}
		flusher.Flush()

		heartbeat := time.NewTicker(heartbeatInterval)
		defer heartbeat.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case event, open := <-events:
				if !open {
					return
				}
				payload, err := json.Marshal(event.Data)
				if err != nil {
					continue
				}
				_, _ = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", event.ID, event.Data.Type, payload)
				flusher.Flush()
			case <-heartbeat.C:
				_, _ = fmt.Fprintf(w, ": heartbeat %d\n\n", time.Now().Unix())
				flusher.Flush()
			}
		}
	}
}

func registerEventStreamHandler(mux *http.ServeMux, broker *eventBroker) {
	mux.HandleFunc("/api/events", eventStreamHandler(broker, eventStreamHeartbeatInterval))
}
