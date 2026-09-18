package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestWindowSlidingLimit(t *testing.T) {
	w := &window{}
	now := time.Unix(1000, 0)
	if !w.allow(now, 2) || !w.allow(now.Add(time.Second), 2) {
		t.Fatal("first two admissions should pass")
	}
	if w.allow(now.Add(2*time.Second), 2) {
		t.Fatal("third admission inside the window should be rejected")
	}
	if !w.allow(now.Add(time.Minute+time.Second), 2) {
		t.Fatal("expired admissions should leave the window")
	}
}

func TestLimitPrecedence(t *testing.T) {
	cfg := pluginConfig{DefaultRPM: 10, Providers: map[string]int{"codex": 20}, Auths: map[string]int{"a": 30}}
	if got := limitFor(cfg, pluginapi.SchedulerAuthCandidate{ID: "a", Provider: "codex"}); got != 30 {
		t.Fatalf("auth override=%d", got)
	}
	if got := limitFor(cfg, pluginapi.SchedulerAuthCandidate{ID: "b", Provider: "codex"}); got != 20 {
		t.Fatalf("provider override=%d", got)
	}
	if got := limitFor(cfg, pluginapi.SchedulerAuthCandidate{ID: "c", Provider: "openai"}); got != 10 {
		t.Fatalf("default=%d", got)
	}
}

func TestConfigureRejectsNegativeLimits(t *testing.T) {
	raw, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("default_rpm: -1\n")})
	if err != nil {
		t.Fatal(err)
	}
	if err := configure(raw); err == nil {
		t.Fatal("negative default_rpm was accepted")
	}
}

func TestConfigureNormalizesProviderKeysWithoutMutatingWhileRanging(t *testing.T) {
	raw, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("providers:\n  Codex: 10\nauths:\n  account-1: 20\n")})
	if err != nil {
		t.Fatal(err)
	}
	if err := configure(raw); err != nil {
		t.Fatal(err)
	}
	cfg := loaded()
	if cfg.Providers["codex"] != 10 || len(cfg.Providers) != 1 {
		t.Fatalf("providers = %#v", cfg.Providers)
	}
}

func TestRateLimitErrorCarriesStopRetryHTTPStatus(t *testing.T) {
	if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte("default_rpm: 1\nqueue_enabled: false\n")})); err != nil {
		t.Fatal(err)
	}
	request := testJSON(pluginapi.SchedulerPickRequest{
		Provider:   "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "limited-account", Provider: "codex"}},
	})
	if _, err := pick(request); err != nil {
		t.Fatal(err)
	}
	raw, err := pick(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"http_status":429`) || !strings.Contains(string(raw), `"stop_retry":true`) {
		t.Fatalf("rate-limit response = %s", raw)
	}
	if strings.Contains(string(raw), `"retryable"`) {
		t.Fatalf("rate-limit response must not carry retryable: %s", raw)
	}
}

func TestManagementRegistrationExposesMenuAndProtectedSettingsRoutes(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"/menu", "Provider Rate Limiter", "GET", "PUT", "/plugins/provider-rate-limiter/settings", "/plugins/provider-rate-limiter/candidates"} {
		if !strings.Contains(text, want) {
			t.Fatalf("management registration missing %q: %s", want, text)
		}
	}
}

func TestManagementMenuProvidesAccountSelectorUI(t *testing.T) {
	html := menuHTML()
	for _, want := range []string{"/v0/management/auth-files", "/v0/management/plugins/provider-rate-limiter/config", "/v0/management/plugins/provider-rate-limiter/candidates", "accountRows", "auth-limit", "providerFilter", "queueEnabled", "queueMaxWaitMS", "queueMaxWaiters", "Save runtime settings"} {
		if !strings.Contains(html, want) {
			t.Fatalf("management menu missing %q", want)
		}
	}
}

func TestManagementSettingsCanUpdateRuntimeConfig(t *testing.T) {
	body := testJSON(pluginConfig{DefaultRPM: 77, Providers: map[string]int{"Codex": 88}, Auths: map[string]int{"account-a": 99}})
	raw, err := testJSONBytes(managementRequest{Method: "PUT", Path: "/v0/management/plugins/provider-rate-limiter/settings", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleManagement(raw); err != nil {
		t.Fatal(err)
	}
	got := loaded()
	if got.DefaultRPM != 77 || got.Providers["codex"] != 88 || got.Auths["account-a"] != 99 {
		t.Fatalf("runtime config = %#v", got)
	}
}

func TestConfigureQueueDefaultsAndExplicitValues(t *testing.T) {
	if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte("default_rpm: 3\n")})); err != nil {
		t.Fatal(err)
	}
	cfg := loaded()
	if !cfg.QueueEnabled || cfg.QueueMaxWaitMS != 15000 || cfg.QueueMaxWaiters != 256 {
		t.Fatalf("queue defaults = %#v", cfg)
	}
	if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte("queue_enabled: false\n")})); err != nil {
		t.Fatal(err)
	}
	cfg = loaded()
	if cfg.QueueEnabled || cfg.QueueMaxWaitMS != 15000 || cfg.QueueMaxWaiters != 256 {
		t.Fatalf("explicit false must be honored while others default: %#v", cfg)
	}
	if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte("queue_max_wait_ms: 0\nqueue_max_waiters: 0\n")})); err != nil {
		t.Fatal(err)
	}
	cfg = loaded()
	if !cfg.QueueEnabled || cfg.QueueMaxWaitMS != 0 || cfg.QueueMaxWaiters != 0 {
		t.Fatalf("explicit zero must be honored: %#v", cfg)
	}
}

func TestConfigureRejectsInvalidQueueValues(t *testing.T) {
	for _, yamlDoc := range []string{
		"queue_max_wait_ms: -1\n",
		"queue_max_waiters: -5\n",
		"queue_max_wait_ms: 9223372036854775807\n",
	} {
		if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte(yamlDoc)})); err == nil {
			t.Fatalf("%q was accepted", yamlDoc)
		}
	}
	if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte("queue_max_wait_ms: 9223372036854\n")})); err != nil {
		t.Fatalf("boundary wait rejected: %v", err)
	}
}

func TestManagementSettingsQueueFieldsRoundTrip(t *testing.T) {
	body := []byte(`{"default_rpm":5,"providers":{},"auths":{},"queue_enabled":true,"queue_max_wait_ms":7000,"queue_max_waiters":42}`)
	raw := testJSON(managementRequest{Method: "PUT", Path: "/v0/management/plugins/provider-rate-limiter/settings", Body: body})
	if _, err := handleManagement(raw); err != nil {
		t.Fatal(err)
	}
	cfg := loaded()
	if !cfg.QueueEnabled || cfg.QueueMaxWaitMS != 7000 || cfg.QueueMaxWaiters != 42 {
		t.Fatalf("PUT roundtrip = %#v", cfg)
	}
	raw = testJSON(managementRequest{Method: "GET", Path: "/v0/management/plugins/provider-rate-limiter/settings"})
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeManagementResponse(t, out)
	text := string(resp.Body)
	for _, want := range []string{`"queue_enabled":true`, `"queue_max_wait_ms":7000`, `"queue_max_waiters":42`} {
		if !strings.Contains(text, want) {
			t.Fatalf("GET settings missing %q: %s", want, text)
		}
	}
	raw = testJSON(managementRequest{Method: "PUT", Path: "/v0/management/plugins/provider-rate-limiter/settings", Body: []byte(`{"default_rpm":6}`)})
	if _, err := handleManagement(raw); err != nil {
		t.Fatal(err)
	}
	cfg = loaded()
	if !cfg.QueueEnabled || cfg.QueueMaxWaitMS != 15000 || cfg.QueueMaxWaiters != 256 {
		t.Fatalf("omitted queue fields must default: %#v", cfg)
	}
}

func decodeManagementResponse(t *testing.T, raw []byte) pluginapi.ManagementResponse {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("management call failed: %s", raw)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func testJSON(v any) []byte {
	raw, err := testJSONBytes(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func testJSONBytes(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return raw, nil
}
