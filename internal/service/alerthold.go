package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Alerts the webhook did not take, or the full queue refused, wait here and
// go out again on a timer: an outage of the receiver must not lose the one
// node.offline the debounce row already claimed was sent.
const (
	alertHoldMax = 256 // beyond it the oldest is dropped, counted and shown
	alertHoldAge = 6 * time.Hour
)

// alertRetryEvery is how often held alerts are tried again; a var for tests.
var alertRetryEvery = time.Minute

const alertDropKey = "alert.dropped"

type heldAlert struct {
	a  Alert
	at time.Time
}

type alertHold struct {
	mu      sync.Mutex
	held    []heldAlert
	dropped int
}

func (h *alertHold) add(a Alert) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.held) >= alertHoldMax {
		h.held = h.held[1:]
		h.dropped++
	}
	h.held = append(h.held, heldAlert{a, time.Now()})
}

func (h *alertHold) take() []heldAlert {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.held
	h.held = nil
	return out
}

// retryHeld tries every held alert once. A failure puts the rest back in
// order; no webhook set, or an alert older than alertHoldAge, drops it.
func (o *Orchestrator) retryHeld(ctx context.Context) {
	h := &o.alertHold
	batch := h.take()
	for i, x := range batch {
		if time.Since(x.at) > alertHoldAge {
			h.mu.Lock()
			h.dropped++
			h.mu.Unlock()
			continue
		}
		err := o.sendAlert(ctx, x.a, 1)
		if errors.Is(err, errAlertsOff) {
			break // nothing to send to: the rest would only be dropped too
		}
		if err != nil {
			h.mu.Lock()
			h.held = append(append([]heldAlert{}, batch[i:]...), h.held...)
			h.mu.Unlock()
			break
		}
	}
	h.mu.Lock()
	dropped, empty := h.dropped, len(h.held) == 0
	if empty {
		h.dropped = 0
	}
	h.mu.Unlock()
	switch {
	case dropped > 0:
		o.openProblem(ctx, alertDropKey, SevWarning,
			fmt.Sprintf("%d alerts could not be delivered and were dropped. Check the alert webhook.", dropped))
	case empty:
		o.clearProblem(ctx, alertDropKey)
	}
}
