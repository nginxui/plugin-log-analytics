// Package hub fans plugin events out to the /events websocket clients. The
// event names and payloads are the ones the host event bus carried before the
// log analytics became a plugin, so the page keeps working unchanged.
package hub

import (
	"sync"
	"sync/atomic"
)

// Event types pushed to websocket clients.
const (
	TypeProcessingStatus      = "processing_status"
	TypeNginxLogIndexReady    = "nginx_log_index_ready"
	TypeNginxLogIndexProgress = "nginx_log_index_progress"
	TypeNginxLogIndexComplete = "nginx_log_index_complete"
)

// Event is one message of the /events stream.
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// ProcessingStatusData is the payload of processing_status events.
type ProcessingStatusData struct {
	NginxLogIndexing bool `json:"nginx_log_indexing"`
}

// NginxLogIndexReadyData is the payload of nginx_log_index_ready events.
type NginxLogIndexReadyData struct {
	LogPath     string `json:"log_path"`
	StartTime   int64  `json:"start_time"`
	EndTime     int64  `json:"end_time"`
	Available   bool   `json:"available"`
	IndexStatus string `json:"index_status"`
}

// NginxLogIndexProgressData is the payload of nginx_log_index_progress events.
type NginxLogIndexProgressData struct {
	LogPath         string  `json:"log_path"`
	Progress        float64 `json:"progress"`         // 0-100 percentage
	Stage           string  `json:"stage"`            // "scanning", "indexing", "stats"
	Status          string  `json:"status"`           // "running", "completed", "error"
	ElapsedTime     int64   `json:"elapsed_time"`     // milliseconds
	EstimatedRemain int64   `json:"estimated_remain"` // milliseconds
}

// NginxLogIndexCompleteData is the payload of nginx_log_index_complete events.
type NginxLogIndexCompleteData struct {
	LogPath     string `json:"log_path"`
	Success     bool   `json:"success"`
	Duration    int64  `json:"duration"` // milliseconds
	TotalLines  int64  `json:"total_lines"`
	IndexedSize int64  `json:"indexed_size"` // bytes
	Error       string `json:"error,omitempty"`
}

// subscriberBuffer is how many events a slow client may lag behind before its
// oldest undelivered event is dropped.
const subscriberBuffer = 64

// Hub delivers events to subscribers without ever blocking the publisher.
type Hub struct {
	mu   sync.RWMutex
	subs map[uint64]chan Event
	next atomic.Uint64
}

// New returns an empty hub.
func New() *Hub {
	return &Hub{subs: make(map[uint64]chan Event)}
}

// Subscribe registers a subscriber. The returned function removes it and
// closes the channel.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	id := h.next.Add(1)
	ch := make(chan Event, subscriberBuffer)

	h.mu.Lock()
	h.subs[id] = ch
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, id)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// Publish sends an event to every subscriber. A subscriber whose buffer is
// full loses its oldest event instead of holding the publisher up.
func (h *Hub) Publish(event Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, ch := range h.subs {
		for {
			select {
			case ch <- event:
			default:
				select {
				case <-ch:
					continue
				default:
				}
			}
			break
		}
	}
}

// Subscribers returns the number of connected subscribers.
func (h *Hub) Subscribers() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// Status tracks whether an indexing run is active and reports every change.
type Status struct {
	hub *Hub

	mu       sync.Mutex
	indexing bool
	onChange func(indexing bool)
}

// NewStatus returns a status that publishes processing_status events on hub.
// onChange, when not nil, is called after every change with the new value, so
// the host activity entry can follow it.
func NewStatus(h *Hub, onChange func(indexing bool)) *Status {
	return &Status{hub: h, onChange: onChange}
}

// Indexing reports whether an indexing run is active.
func (s *Status) Indexing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.indexing
}

// SetIndexing records the state. Setting the current value again does nothing.
func (s *Status) SetIndexing(indexing bool) {
	s.mu.Lock()
	if s.indexing == indexing {
		s.mu.Unlock()
		return
	}
	s.indexing = indexing
	onChange := s.onChange
	s.mu.Unlock()

	s.Broadcast()
	if onChange != nil {
		onChange(indexing)
	}
}

// Broadcast publishes the current state, for a client that just connected.
func (s *Status) Broadcast() {
	s.hub.Publish(Event{
		Type: TypeProcessingStatus,
		Data: ProcessingStatusData{NginxLogIndexing: s.Indexing()},
	})
}
