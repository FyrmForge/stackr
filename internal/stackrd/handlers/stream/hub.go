// Package stream is an in-process pub/sub hub used to fan out log lines and
// status updates to SSE subscribers.
package stream

import "sync"

type Hub struct {
	mu     sync.Mutex
	topics map[string]map[chan string]struct{}
}

func NewHub() *Hub {
	return &Hub{topics: map[string]map[chan string]struct{}{}}
}

// Publish sends msg to all subscribers of topic. Slow subscribers drop messages.
func (h *Hub) Publish(topic, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.topics[topic] {
		select {
		case ch <- msg:
		default: // drop on full buffer; log viewers tolerate gaps
		}
	}
}

// Subscribe returns a channel of messages for topic and an unsubscribe func.
func (h *Hub) Subscribe(topic string) (<-chan string, func()) {
	ch := make(chan string, 256)
	h.mu.Lock()
	if h.topics[topic] == nil {
		h.topics[topic] = map[chan string]struct{}{}
	}
	h.topics[topic][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.topics[topic], ch)
		h.mu.Unlock()
		close(ch)
	}
}
