package main

import (
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestQueue() (*admissionQueue, *testClock) {
	clock := &testClock{now: time.Unix(1000, 0)}
	return newAdmissionQueue(clock.Now, time.Minute), clock
}

func candidate(id, provider string) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{ID: id, Provider: provider}
}

func testWaiter(candidates []pluginapi.SchedulerAuthCandidate, provider string, started, deadline time.Time) *waiter {
	return &waiter{candidates: candidates, provider: provider, started: started, deadline: deadline, result: make(chan pickOutcome, 1)}
}

func waitForWaiters(t *testing.T, q *admissionQueue, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		q.mu.Lock()
		got := len(q.waiters)
		q.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiter count %d, want >= %d", got, n)
		}
		runtime.Gosched()
	}
}

func drainResult(t *testing.T, w *waiter) pickOutcome {
	t.Helper()
	select {
	case outcome := <-w.result:
		return outcome
	case <-time.After(5 * time.Second):
		t.Fatal("waiter result was not produced")
		return pickOutcome{}
	}
}

func expectNoResult(t *testing.T, w *waiter) {
	t.Helper()
	select {
	case outcome := <-w.result:
		t.Fatalf("unexpected waiter result: %+v", outcome)
	default:
	}
}

func TestQueuePickFastPathAdmitsImmediately(t *testing.T) {
	if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte("default_rpm: 10\n")})); err != nil {
		t.Fatal(err)
	}
	q, _ := newTestQueue()
	defer q.shutdown()
	outcome := q.pick(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{candidate("acct-fast", "codex")}})
	if outcome.authID != "acct-fast" {
		t.Fatalf("fast path outcome = %+v", outcome)
	}
	if len(q.waiters) != 0 {
		t.Fatalf("fast path must not enqueue: %d waiters", len(q.waiters))
	}
}

func TestQueuePickNoCandidatesReturns429(t *testing.T) {
	if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte("default_rpm: 10\n")})); err != nil {
		t.Fatal(err)
	}
	q, _ := newTestQueue()
	defer q.shutdown()
	outcome := q.pick(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "  ", Provider: "codex"}}})
	if outcome.status != http.StatusTooManyRequests || outcome.code != "provider_rate_limit_exceeded" {
		t.Fatalf("empty candidates outcome = %+v", outcome)
	}
	if len(q.waiters) != 0 {
		t.Fatalf("blank candidates must not enqueue: %d waiters", len(q.waiters))
	}
}

func TestQueueDispatchFIFOSameCandidates(t *testing.T) {
	q, clock := newTestQueue()
	defer q.shutdown()
	cfg := defaultConfig()
	cfg.DefaultRPM = 1
	cfg.QueueMaxWaitMS = 300000
	base := clock.Now()
	q.mu.Lock()
	if !q.windowFor("fifo-auth").allowFor(base, 1, q.duration) {
		q.mu.Unlock()
		t.Fatal("seeding the window should pass")
	}
	first := testWaiter([]pluginapi.SchedulerAuthCandidate{candidate("fifo-auth", "codex")}, "codex", base, base.Add(300*time.Second))
	second := testWaiter([]pluginapi.SchedulerAuthCandidate{candidate("fifo-auth", "codex")}, "codex", base, base.Add(300*time.Second))
	q.waiters = append(q.waiters, first, second)
	next := q.dispatchLocked(base, cfg)
	q.mu.Unlock()
	expectNoResult(t, first)
	expectNoResult(t, second)
	if !next.Equal(base.Add(time.Minute)) {
		t.Fatalf("next wake = %v, want next free slot %v", next, base.Add(time.Minute))
	}
	q.mu.Lock()
	q.dispatchLocked(base.Add(61*time.Second), cfg)
	q.mu.Unlock()
	if outcome := drainResult(t, first); outcome.authID != "fifo-auth" {
		t.Fatalf("first waiter outcome = %+v", outcome)
	}
	expectNoResult(t, second)
	q.mu.Lock()
	q.dispatchLocked(base.Add(122*time.Second), cfg)
	q.mu.Unlock()
	if outcome := drainResult(t, second); outcome.authID != "fifo-auth" {
		t.Fatalf("second waiter outcome = %+v", outcome)
	}
}

func TestQueueDispatchSkipsBlockedWaiter(t *testing.T) {
	q, clock := newTestQueue()
	defer q.shutdown()
	cfg := defaultConfig()
	cfg.DefaultRPM = 1
	base := clock.Now()
	q.mu.Lock()
	if !q.windowFor("codex-full").allowFor(base, 1, q.duration) {
		q.mu.Unlock()
		t.Fatal("seeding the window should pass")
	}
	blocked := testWaiter([]pluginapi.SchedulerAuthCandidate{candidate("codex-full", "codex")}, "codex", base, base.Add(15*time.Second))
	free := testWaiter([]pluginapi.SchedulerAuthCandidate{candidate("claude-free", "claude")}, "claude", base, base.Add(15*time.Second))
	q.waiters = append(q.waiters, blocked, free)
	q.dispatchLocked(base, cfg)
	q.mu.Unlock()
	expectNoResult(t, blocked)
	if outcome := drainResult(t, free); outcome.authID != "claude-free" {
		t.Fatalf("later waiter with free candidate = %+v", outcome)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.waiters) != 1 || q.waiters[0] != blocked {
		t.Fatalf("blocked waiter should be retained: %#v", q.waiters)
	}
}

func TestQueueDeadlineBoundaryRejectsBeforeAllow(t *testing.T) {
	q, clock := newTestQueue()
	defer q.shutdown()
	cfg := defaultConfig()
	cfg.DefaultRPM = 10
	base := clock.Now()
	w := testWaiter([]pluginapi.SchedulerAuthCandidate{candidate("edge-auth", "codex")}, "codex", base.Add(-time.Second), base)
	q.mu.Lock()
	q.waiters = append(q.waiters, w)
	q.dispatchLocked(base, cfg)
	q.mu.Unlock()
	outcome := drainResult(t, w)
	if outcome.status != http.StatusTooManyRequests || outcome.code != "provider_rate_limit_exceeded" {
		t.Fatalf("boundary outcome = %+v", outcome)
	}
	if !strings.Contains(outcome.message, "waited_ms=") || !strings.Contains(outcome.message, "next_free_in_ms=") {
		t.Fatalf("timeout message missing fields: %q", outcome.message)
	}
	q.mu.Lock()
	hits := len(q.windowFor("edge-auth").hits)
	q.mu.Unlock()
	if hits != 0 {
		t.Fatalf("expired waiter must not consume a hit, got %d", hits)
	}
}

func TestQueueTimeoutMessageFields(t *testing.T) {
	q, clock := newTestQueue()
	defer q.shutdown()
	cfg := defaultConfig()
	cfg.DefaultRPM = 1
	base := clock.Now()
	q.mu.Lock()
	if !q.windowFor("slow-auth").allowFor(base, 1, q.duration) {
		q.mu.Unlock()
		t.Fatal("seeding the window should pass")
	}
	w := testWaiter([]pluginapi.SchedulerAuthCandidate{candidate("slow-auth", "codex")}, "codex", base.Add(-5*time.Second), base.Add(30*time.Second))
	q.waiters = append(q.waiters, w)
	q.dispatchLocked(base.Add(30*time.Second), cfg)
	q.mu.Unlock()
	outcome := drainResult(t, w)
	if outcome.status != http.StatusTooManyRequests || outcome.code != "provider_rate_limit_exceeded" {
		t.Fatalf("timeout outcome = %+v", outcome)
	}
	if !strings.Contains(outcome.message, `all candidates for provider "codex" are over the configured rate limit`) {
		t.Fatalf("timeout message must preserve legacy text: %q", outcome.message)
	}
	if !strings.Contains(outcome.message, "waited_ms=35000") {
		t.Fatalf("waited_ms wrong: %q", outcome.message)
	}
	if !strings.Contains(outcome.message, "next_free_in_ms=30000") {
		t.Fatalf("next_free_in_ms wrong: %q", outcome.message)
	}
}

func TestQueuePickFullReturns429(t *testing.T) {
	if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte("default_rpm: 1\nqueue_max_wait_ms: 120000\nqueue_max_waiters: 1\n")})); err != nil {
		t.Fatal(err)
	}
	q, clock := newTestQueue()
	defer q.shutdown()
	base := clock.Now()
	q.mu.Lock()
	if !q.windowFor("full-auth").allowFor(base, 1, q.duration) {
		q.mu.Unlock()
		t.Fatal("seeding the window should pass")
	}
	q.mu.Unlock()
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{candidate("full-auth", "codex")}}
	first := make(chan pickOutcome, 1)
	go func() { first <- q.pick(req) }()
	waitForWaiters(t, q, 1)
	outcome := q.pick(req)
	if outcome.code != "provider_rate_limit_queue_full" || outcome.status != http.StatusTooManyRequests {
		t.Fatalf("capacity outcome = %+v", outcome)
	}
	clock.Set(base.Add(61 * time.Second))
	q.mu.Lock()
	q.dispatchLocked(clock.Now(), loaded())
	q.mu.Unlock()
	if outcome := <-first; outcome.authID != "full-auth" {
		t.Fatalf("queued pick = %+v", outcome)
	}
}

func TestQueuePickDisabledAndZeroWait(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{"disabled", "default_rpm: 1\nqueue_enabled: false\n"},
		{"zero wait", "default_rpm: 1\nqueue_max_wait_ms: 0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte(tc.yaml)})); err != nil {
				t.Fatal(err)
			}
			q, clock := newTestQueue()
			defer q.shutdown()
			q.mu.Lock()
			if !q.windowFor("blocked-auth").allowFor(clock.Now(), 1, q.duration) {
				q.mu.Unlock()
				t.Fatal("seeding the window should pass")
			}
			q.mu.Unlock()
			outcome := q.pick(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{candidate("blocked-auth", "codex")}})
			if outcome.status != http.StatusTooManyRequests || outcome.code != "provider_rate_limit_exceeded" {
				t.Fatalf("outcome = %+v", outcome)
			}
			if len(q.waiters) != 0 {
				t.Fatalf("disabled queue must not enqueue: %d", len(q.waiters))
			}
		})
	}
}

func TestQueueDispatchDisabledDrainsWaiters(t *testing.T) {
	q, clock := newTestQueue()
	defer q.shutdown()
	cfg := defaultConfig()
	cfg.DefaultRPM = 1
	base := clock.Now()
	q.mu.Lock()
	if !q.windowFor("drain-auth").allowFor(base, 1, q.duration) {
		q.mu.Unlock()
		t.Fatal("seeding the window should pass")
	}
	w := testWaiter([]pluginapi.SchedulerAuthCandidate{candidate("drain-auth", "codex")}, "codex", base, base.Add(15*time.Second))
	q.waiters = append(q.waiters, w)
	q.dispatchLocked(base, cfg)
	expectNoResult(t, w)
	disabled := cfg
	disabled.QueueEnabled = false
	q.dispatchLocked(base.Add(time.Second), disabled)
	q.mu.Unlock()
	outcome := drainResult(t, w)
	want := `all candidates for provider "codex" are over the configured rate limit`
	if outcome.status != http.StatusTooManyRequests || outcome.message != want {
		t.Fatalf("disabled drain outcome = %+v", outcome)
	}
}

func TestWindowNextFreeHonorsReducedRPM(t *testing.T) {
	w := &window{}
	base := time.Unix(2000, 0)
	for _, offset := range []time.Duration{0, time.Second, 2 * time.Second} {
		if !w.allowFor(base.Add(offset), 5, time.Minute) {
			t.Fatal("seeding hits should pass")
		}
	}
	now := base.Add(3 * time.Second)
	if got := w.nextFree(now, 3, time.Minute); !got.Equal(base.Add(time.Minute)) {
		t.Fatalf("nextFree rpm=3 = %v, want %v", got, base.Add(time.Minute))
	}
	if got := w.nextFree(now, 2, time.Minute); !got.Equal(base.Add(61 * time.Second)) {
		t.Fatalf("nextFree rpm=2 = %v, want %v", got, base.Add(61*time.Second))
	}
	if got := w.nextFree(now, 1, time.Minute); !got.Equal(base.Add(62 * time.Second)) {
		t.Fatalf("nextFree rpm=1 = %v, want %v", got, base.Add(62*time.Second))
	}
	if got := w.nextFree(now, 5, time.Minute); !got.Equal(now) {
		t.Fatalf("nextFree below capacity = %v, want %v", got, now)
	}
	if got := w.nextFree(now, 0, time.Minute); !got.Equal(now) {
		t.Fatalf("nextFree rpm=0 = %v, want %v", got, now)
	}
}

func TestWindowExactExpiryBoundary(t *testing.T) {
	w := &window{}
	base := time.Unix(3000, 0)
	if !w.allowFor(base, 1, time.Minute) {
		t.Fatal("seeding hit should pass")
	}
	if w.allowFor(base.Add(59*time.Second+999*time.Millisecond), 1, time.Minute) {
		t.Fatal("hit still live one millisecond before expiry")
	}
	if !w.allowFor(base.Add(time.Minute), 1, time.Minute) {
		t.Fatal("hit+duration <= now must expire at the exact boundary")
	}
}

func TestQueueReconfigureShortensDeadlineNeverExtends(t *testing.T) {
	q, clock := newTestQueue()
	defer q.shutdown()
	base := clock.Now()
	cfg := defaultConfig()
	cfg.DefaultRPM = 1
	q.mu.Lock()
	if !q.windowFor("reconf-auth").allowFor(base, 1, q.duration) {
		q.mu.Unlock()
		t.Fatal("seeding the window should pass")
	}
	w := testWaiter([]pluginapi.SchedulerAuthCandidate{candidate("reconf-auth", "codex")}, "codex", base, base.Add(15*time.Second))
	q.waiters = append(q.waiters, w)
	extended := cfg
	extended.QueueMaxWaitMS = 30000
	q.dispatchLocked(base, extended)
	if !w.deadline.Equal(base.Add(15 * time.Second)) {
		q.mu.Unlock()
		t.Fatalf("deadline must never extend, got %v", w.deadline)
	}
	expectNoResult(t, w)
	shortened := cfg
	shortened.QueueMaxWaitMS = 5000
	q.dispatchLocked(base, shortened)
	if !w.deadline.Equal(base.Add(5 * time.Second)) {
		q.mu.Unlock()
		t.Fatalf("deadline not shortened, got %v", w.deadline)
	}
	q.dispatchLocked(base.Add(5*time.Second), shortened)
	q.mu.Unlock()
	outcome := drainResult(t, w)
	if outcome.status != http.StatusTooManyRequests || !strings.Contains(outcome.message, "waited_ms=5000") {
		t.Fatalf("shortened deadline outcome = %+v", outcome)
	}
}

func TestQueueReconfigureIncreasedRPMAdmits(t *testing.T) {
	q, clock := newTestQueue()
	defer q.shutdown()
	base := clock.Now()
	cfg := defaultConfig()
	cfg.DefaultRPM = 1
	q.mu.Lock()
	if !q.windowFor("grow-auth").allowFor(base, 1, q.duration) {
		q.mu.Unlock()
		t.Fatal("seeding the window should pass")
	}
	w := testWaiter([]pluginapi.SchedulerAuthCandidate{candidate("grow-auth", "codex")}, "codex", base, base.Add(15*time.Second))
	q.waiters = append(q.waiters, w)
	q.dispatchLocked(base, cfg)
	expectNoResult(t, w)
	grown := cfg
	grown.DefaultRPM = 2
	q.dispatchLocked(base, grown)
	q.mu.Unlock()
	if outcome := drainResult(t, w); outcome.authID != "grow-auth" {
		t.Fatalf("increased rpm outcome = %+v", outcome)
	}
}

func TestQueueShutdownDrainsWaiters(t *testing.T) {
	q, clock := newTestQueue()
	base := clock.Now()
	q.mu.Lock()
	if !q.windowFor("sd-auth").allowFor(base, 1, q.duration) {
		q.mu.Unlock()
		t.Fatal("seeding the window should pass")
	}
	w1 := testWaiter([]pluginapi.SchedulerAuthCandidate{candidate("sd-auth", "codex")}, "codex", base, base.Add(15*time.Second))
	w2 := testWaiter([]pluginapi.SchedulerAuthCandidate{candidate("sd-auth2", "claude")}, "claude", base, base.Add(15*time.Second))
	q.waiters = append(q.waiters, w1, w2)
	q.mu.Unlock()
	q.shutdown()
	for _, w := range []*waiter{w1, w2} {
		outcome := drainResult(t, w)
		if outcome.status != http.StatusServiceUnavailable || outcome.code != "provider_rate_limiter_stopped" {
			t.Fatalf("shutdown outcome = %+v", outcome)
		}
	}
	if outcome := q.pick(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{candidate("sd-auth", "codex")}}); outcome.status != http.StatusServiceUnavailable {
		t.Fatalf("pick after shutdown = %+v", outcome)
	}
	q.shutdown()
}

func TestQueueSamplesClockUnderAdmissionLock(t *testing.T) {
	if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte("default_rpm: 10\n")})); err != nil {
		t.Fatal(err)
	}
	var sampledOutside atomic.Int64
	var q *admissionQueue
	q = newAdmissionQueue(func() time.Time {
		if q.mu.TryLock() {
			sampledOutside.Add(1)
			q.mu.Unlock()
		}
		return time.Unix(4000, 0)
	}, time.Minute)
	defer q.shutdown()
	outcome := q.pick(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{candidate("clock-auth", "codex")}})
	if outcome.authID != "clock-auth" {
		t.Fatalf("pick outcome = %+v", outcome)
	}
	if got := sampledOutside.Load(); got != 0 {
		t.Fatalf("clock sampled outside the admission lock %d times", got)
	}
}

func TestQueuePickSeesConfigPublishedUnderLock(t *testing.T) {
	if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte("default_rpm: 2\nqueue_enabled: false\n")})); err != nil {
		t.Fatal(err)
	}
	q, clock := newTestQueue()
	defer q.shutdown()
	base := clock.Now()
	q.mu.Lock()
	if !q.windowFor("lower-auth").allowFor(base, 2, q.duration) {
		q.mu.Unlock()
		t.Fatal("seeding the window should pass")
	}
	result := make(chan pickOutcome, 1)
	go func() {
		result <- q.pick(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{candidate("lower-auth", "codex")}})
	}()
	lowered := defaultConfig()
	lowered.DefaultRPM = 1
	lowered.QueueEnabled = false
	currentConfig.Store(lowered)
	q.mu.Unlock()
	outcome := <-result
	if outcome.status != http.StatusTooManyRequests {
		t.Fatalf("pick must observe the config published under the lock: %+v", outcome)
	}
}

func TestQueueReopenAdmitsAndRetainsWindows(t *testing.T) {
	if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte("default_rpm: 1\nqueue_enabled: false\n")})); err != nil {
		t.Fatal(err)
	}
	q, clock := newTestQueue()
	defer q.shutdown()
	base := clock.Now()
	q.mu.Lock()
	if !q.windowFor("kept-auth").allowFor(base, 1, q.duration) {
		q.mu.Unlock()
		t.Fatal("seeding the window should pass")
	}
	q.mu.Unlock()
	q.shutdown()
	if outcome := q.pick(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{candidate("new-auth", "codex")}}); outcome.status != http.StatusServiceUnavailable {
		t.Fatalf("pick while closed = %+v", outcome)
	}
	q.reopen()
	if outcome := q.pick(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{candidate("new-auth", "codex")}}); outcome.authID != "new-auth" {
		t.Fatalf("pick after reopen = %+v", outcome)
	}
	if outcome := q.pick(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{candidate("kept-auth", "codex")}}); outcome.status != http.StatusTooManyRequests {
		t.Fatalf("prior hits must still enforce the limit after reopen: %+v", outcome)
	}
}

func TestPickWaitsForWindowInsteadOfFailing(t *testing.T) {
	if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte("default_rpm: 1\n")})); err != nil {
		t.Fatal(err)
	}
	testQueue, clock := newTestQueue()
	oldQueue := queue
	queue = testQueue
	defer func() {
		queue = oldQueue
		testQueue.shutdown()
	}()
	request := testJSON(pluginapi.SchedulerPickRequest{
		Provider:   "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{candidate("queued-account", "codex")},
	})
	raw, err := pick(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"AuthID":"queued-account"`) {
		t.Fatalf("first pick = %s", raw)
	}
	clock.Add(59 * time.Second)
	blocked := make(chan []byte, 1)
	go func() {
		b, err := pick(request)
		if err != nil {
			b = []byte("err: " + err.Error())
		}
		blocked <- b
	}()
	deadline := time.Now().Add(5 * time.Second)
	testQueue.mu.Lock()
	for len(testQueue.waiters) == 0 {
		testQueue.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("second pick did not enqueue")
		}
		runtime.Gosched()
		testQueue.mu.Lock()
	}
	clock.Add(2 * time.Second)
	testQueue.dispatchLocked(clock.Now(), loaded())
	testQueue.mu.Unlock()
	select {
	case raw = <-blocked:
		if !strings.Contains(string(raw), `"AuthID":"queued-account"`) {
			t.Fatalf("queued pick = %s", raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued pick did not complete")
	}
}

func TestConcurrentPicksRespectRPM(t *testing.T) {
	if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte("default_rpm: 5\nqueue_enabled: false\n")})); err != nil {
		t.Fatal(err)
	}
	request := testJSON(pluginapi.SchedulerPickRequest{
		Provider:   "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{candidate("flood-auth", "codex")},
	})
	const calls = 50
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, err := pick(request)
			if err != nil {
				return
			}
			if strings.Contains(string(raw), `"AuthID":"flood-auth"`) {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := admitted.Load(); got != 5 {
		t.Fatalf("admitted %d picks, want 5", got)
	}
}
