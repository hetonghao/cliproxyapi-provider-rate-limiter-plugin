package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type pickOutcome struct {
	authID  string
	code    string
	message string
	status  int
}

// schedulerPreferredAuthMetadataKey is the host-provided preferred auth in
// SchedulerPickRequest.Options.Metadata. The host sets it to the auth its own
// selector (round-robin, session affinity, pinned, priority) already chose, so
// the plugin only vetoes it when that credential is over the rate limit.
const schedulerPreferredAuthMetadataKey = "scheduler_preferred_auth_id"

type waiter struct {
	candidates        []pluginapi.SchedulerAuthCandidate
	provider          string
	preferredID       string
	started, deadline time.Time
	result            chan pickOutcome
}

type admissionQueue struct {
	mu              sync.Mutex
	waiters         []*waiter
	windows         map[string]*window
	lastPicked      map[string]string
	wake            chan struct{}
	running, closed bool
	now             func() time.Time
	duration        time.Duration
}

func newAdmissionQueue(now func() time.Time, duration time.Duration) *admissionQueue {
	return &admissionQueue{windows: map[string]*window{}, lastPicked: map[string]string{}, wake: make(chan struct{}, 1), now: now, duration: duration}
}

var queue = newAdmissionQueue(time.Now, time.Minute)

func (q *admissionQueue) windowFor(id string) *window {
	w, ok := q.windows[id]
	if !ok {
		w = &window{}
		q.windows[id] = w
	}
	return w
}

func rateLimitOutcome(provider string) pickOutcome {
	return pickOutcome{code: "provider_rate_limit_exceeded", message: fmt.Sprintf("all candidates for provider %q are over the configured rate limit", provider), status: http.StatusTooManyRequests}
}

func queueFullOutcome(provider string) pickOutcome {
	return pickOutcome{code: "provider_rate_limit_queue_full", message: fmt.Sprintf("admission queue for provider %q is full", provider), status: http.StatusTooManyRequests}
}

func stoppedOutcome() pickOutcome {
	return pickOutcome{code: "provider_rate_limiter_stopped", message: "provider rate limiter is stopped", status: http.StatusServiceUnavailable}
}

func (q *admissionQueue) signalWake() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func requestPreferredAuthID(req pluginapi.SchedulerPickRequest) string {
	raw, ok := req.Options.Metadata[schedulerPreferredAuthMetadataKey].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(raw)
}

// admitLocked selects one candidate that is under its rate limit. The host's
// preferred auth wins whenever it has capacity; otherwise candidates are tried
// in round-robin order starting after the last admitted auth, so spillover
// spreads across accounts instead of always landing on the lowest-sorted ID.
func (q *admissionQueue) admitLocked(now time.Time, cfg pluginConfig, candidates []pluginapi.SchedulerAuthCandidate, scope, preferredID string) (string, bool) {
	if len(candidates) == 0 {
		return "", false
	}
	if preferredID != "" {
		for _, c := range candidates {
			if c.ID != preferredID {
				continue
			}
			if q.windowFor(c.ID).allowFor(now, limitFor(cfg, c), q.duration) {
				q.lastPicked[scope] = c.ID
				return c.ID, true
			}
			break
		}
	}
	start := 0
	if last := q.lastPicked[scope]; last != "" {
		for i, c := range candidates {
			if c.ID == last {
				start = i + 1
				break
			}
		}
		if start >= len(candidates) {
			start = 0
		}
	}
	for k := 0; k < len(candidates); k++ {
		c := candidates[(start+k)%len(candidates)]
		if c.ID == preferredID {
			continue
		}
		if q.windowFor(c.ID).allowFor(now, limitFor(cfg, c), q.duration) {
			q.lastPicked[scope] = c.ID
			return c.ID, true
		}
	}
	return "", false
}

func (q *admissionQueue) pick(req pluginapi.SchedulerPickRequest) pickOutcome {
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, c := range req.Candidates {
		if strings.TrimSpace(c.ID) != "" {
			candidates = append(candidates, c)
		}
	}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return stoppedOutcome()
	}
	now := q.now()
	cfg := loaded()
	q.dispatchLocked(now, cfg)
	if authID, ok := q.admitLocked(now, cfg, candidates, req.Provider, requestPreferredAuthID(req)); ok {
		q.mu.Unlock()
		return pickOutcome{authID: authID}
	}
	if len(candidates) == 0 || !cfg.QueueEnabled || cfg.QueueMaxWaitMS <= 0 {
		q.mu.Unlock()
		return rateLimitOutcome(req.Provider)
	}
	if cfg.QueueMaxWaiters > 0 && len(q.waiters) >= cfg.QueueMaxWaiters {
		q.mu.Unlock()
		return queueFullOutcome(req.Provider)
	}
	w := &waiter{
		candidates:  candidates,
		provider:    req.Provider,
		preferredID: requestPreferredAuthID(req),
		started:     now,
		deadline:    now.Add(time.Duration(cfg.QueueMaxWaitMS) * time.Millisecond),
		result:      make(chan pickOutcome, 1),
	}
	q.waiters = append(q.waiters, w)
	if !q.running {
		q.running = true
		go q.run()
	}
	q.mu.Unlock()
	q.signalWake()
	return <-w.result
}

func (q *admissionQueue) dispatchLocked(now time.Time, cfg pluginConfig) time.Time {
	var next time.Time
	disabled := !cfg.QueueEnabled || cfg.QueueMaxWaitMS <= 0
	kept := q.waiters[:0]
	for _, w := range q.waiters {
		if cfg.QueueMaxWaitMS > 0 {
			if shortened := w.started.Add(time.Duration(cfg.QueueMaxWaitMS) * time.Millisecond); shortened.Before(w.deadline) {
				w.deadline = shortened
			}
		}
		if !w.deadline.After(now) {
			outcome := rateLimitOutcome(w.provider)
			outcome.message = fmt.Sprintf("%s (waited_ms=%d, next_free_in_ms=%d)", outcome.message, now.Sub(w.started).Milliseconds(), q.nextFreeInLocked(now, cfg, w).Milliseconds())
			w.result <- outcome
			continue
		}
		if disabled {
			w.result <- rateLimitOutcome(w.provider)
			continue
		}
		if authID, admitted := q.admitLocked(now, cfg, w.candidates, w.provider, w.preferredID); admitted {
			w.result <- pickOutcome{authID: authID}
			continue
		}
		if next.IsZero() || w.deadline.Before(next) {
			next = w.deadline
		}
		for _, c := range w.candidates {
			if free := q.windowFor(c.ID).nextFree(now, limitFor(cfg, c), q.duration); free.After(now) && free.Before(next) {
				next = free
			}
		}
		kept = append(kept, w)
	}
	for i := len(kept); i < len(q.waiters); i++ {
		q.waiters[i] = nil
	}
	q.waiters = kept
	return next
}

func (q *admissionQueue) nextFreeInLocked(now time.Time, cfg pluginConfig, w *waiter) time.Duration {
	var best time.Duration
	found := false
	for _, c := range w.candidates {
		d := q.windowFor(c.ID).nextFree(now, limitFor(cfg, c), q.duration).Sub(now)
		if d < 0 {
			d = 0
		}
		if !found || d < best {
			best, found = d, true
		}
	}
	return best
}

func (q *admissionQueue) run() {
	for {
		q.mu.Lock()
		if q.closed {
			q.running = false
			q.mu.Unlock()
			return
		}
		next := q.dispatchLocked(q.now(), loaded())
		if len(q.waiters) == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		q.mu.Unlock()
		var timer *time.Timer
		var timeout <-chan time.Time
		if !next.IsZero() {
			delay := next.Sub(q.now())
			if delay < 0 {
				delay = 0
			}
			timer = time.NewTimer(delay)
			timeout = timer.C
		}
		select {
		case <-q.wake:
		case <-timeout:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (q *admissionQueue) reopen() {
	q.mu.Lock()
	q.closed = false
	q.mu.Unlock()
}

func (q *admissionQueue) shutdown() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	for i, w := range q.waiters {
		w.result <- stoppedOutcome()
		q.waiters[i] = nil
	}
	q.waiters = q.waiters[:0]
	q.mu.Unlock()
	q.signalWake()
}
