package main

import (
	"sync"
	"time"
)

// Operational evidence is deliberately separate from historical completeness.
// A quiet account is not necessarily stalled; reconnecting does not prove backfill.
type ingestionHealth struct {
	mu              sync.Mutex
	lastWrite       time.Time
	offlineSync     time.Time
	failures        uint64
	keepaliveFailed bool
	lastReconnect   time.Time
	lastInbound     time.Time
	inbound         uint64
	undecryptable   uint64
}

func (h *ingestionHealth) received(undecryptable bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastInbound = time.Now()
	h.inbound++
	if undecryptable {
		h.undecryptable++
	}
}

func (h *ingestionHealth) recordWrite(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.failures++
	} else {
		h.lastWrite = time.Now()
	}
}
func (h *ingestionHealth) offlineSyncCompleted() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.offlineSync = time.Now()
}
func (h *ingestionHealth) disconnected() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.offlineSync = time.Time{}
}
func (h *ingestionHealth) keepaliveRestored() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.keepaliveFailed = false
}
func (h *ingestionHealth) connected() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.keepaliveFailed = false
	h.offlineSync = time.Time{}
}
func (h *ingestionHealth) shouldReconnect(count int, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.keepaliveFailed = true
	if count < 2 || (!h.lastReconnect.IsZero() && now.Sub(h.lastReconnect) < 5*time.Minute) {
		return false
	}
	h.lastReconnect = now
	h.offlineSync = time.Time{}
	return true
}
func (h *ingestionHealth) snapshot() map[string]interface{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return map[string]interface{}{"last_successful_write": h.lastWrite, "offline_sync_completed_at": h.offlineSync, "storage_failures": h.failures, "keepalive_failed": h.keepaliveFailed, "last_inbound_event": h.lastInbound, "inbound_events": h.inbound, "undecryptable_events": h.undecryptable}
}
