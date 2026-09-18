package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func resetObserved(t *testing.T) *observedRegistry {
	t.Helper()
	old := observed
	reg := newObservedRegistry(time.Unix(1000, 0))
	observed = reg
	t.Cleanup(func() { observed = old })
	return reg
}

func TestObservedSanitizesSourceAndBaseURL(t *testing.T) {
	reg := resetObserved(t)
	observeCandidates([]pluginapi.SchedulerAuthCandidate{{
		ID: "compat-acct", Provider: "openai-compatible-deepseek", Priority: 2, Status: "active",
		Attributes: map[string]string{
			"source":   "config:deepseek[SECRETTOKEN]",
			"base_url": "https://user:pass@api.example.com:8443/v1/path?api_key=xyz&b=1#frag",
			"api_key":  "SHOULD-NOT-APPEAR",
			"custom":   "raw-content",
		},
	}})
	reg.mu.Lock()
	entry, ok := reg.byID["compat-acct"]
	reg.mu.Unlock()
	if !ok {
		t.Fatal("candidate was not observed")
	}
	if entry.Provider != "openai-compatible-deepseek" || entry.Priority != 2 || entry.Status != "active" {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.Source != "config:openai-compatible-deepseek" {
		t.Fatalf("source = %q", entry.Source)
	}
	if entry.BaseURL != "https://api.example.com:8443" {
		t.Fatalf("base_url = %q", entry.BaseURL)
	}
	raw := string(mustJSON(reg.snapshotResponse()))
	for _, secret := range []string{"SECRETTOKEN", "user:pass", "api_key=xyz", "/v1/path", "SHOULD-NOT-APPEAR", "raw-content", "frag"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("snapshot leaks %q: %s", secret, raw)
		}
	}
}

func TestObservedDropsUnsafeSourceAndBaseURL(t *testing.T) {
	reg := resetObserved(t)
	observeCandidates([]pluginapi.SchedulerAuthCandidate{
		{ID: "file-acct", Provider: "codex", Attributes: map[string]string{"source": "file:/private/auths/codex.json", "base_url": "ftp://example.com/x"}},
		{ID: "malformed", Provider: "claude", Attributes: map[string]string{"source": "config2:thing[abc]", "base_url": "not a url"}},
		{ID: "relative", Provider: "gemini", Attributes: map[string]string{"base_url": "api.example.com/v1"}},
	})
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, id := range []string{"file-acct", "malformed", "relative"} {
		entry, ok := reg.byID[id]
		if !ok {
			t.Fatalf("%s was not observed", id)
		}
		if entry.Source != "" {
			t.Fatalf("%s source = %q", id, entry.Source)
		}
		if entry.BaseURL != "" {
			t.Fatalf("%s base_url = %q", id, entry.BaseURL)
		}
	}
}

func TestObservedSkipsBlankIDsAndSorts(t *testing.T) {
	reg := resetObserved(t)
	observeCandidates([]pluginapi.SchedulerAuthCandidate{
		{ID: "   ", Provider: "codex"},
		candidate("zeta", "claude"),
		candidate("alpha", "codex"),
	})
	snap := reg.snapshotResponse()
	if len(snap.Candidates) != 2 || snap.Candidates[0].ID != "alpha" || snap.Candidates[1].ID != "zeta" {
		t.Fatalf("snapshot = %+v", snap.Candidates)
	}
	if snap.ObservedSince.IsZero() {
		t.Fatal("observed_since must be set")
	}
}

func TestObservedEvictsOldestWithIDTiebreak(t *testing.T) {
	reg := resetObserved(t)
	base := time.Unix(1000, 0)
	reg.mu.Lock()
	for i := 0; i < observedMaxCandidates; i++ {
		id := fmt.Sprintf("auth-%04d", i)
		seen := base.Add(time.Duration(i) * time.Second)
		if i == 1 {
			seen = base
		}
		reg.byID[id] = observedCandidate{ID: id, Provider: "codex", LastSeen: seen}
	}
	reg.mu.Unlock()
	observeCandidates([]pluginapi.SchedulerAuthCandidate{candidate("auth-new", "codex")})
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if len(reg.byID) != observedMaxCandidates {
		t.Fatalf("registry size = %d, want %d", len(reg.byID), observedMaxCandidates)
	}
	if _, ok := reg.byID["auth-0000"]; ok {
		t.Fatal("oldest (tie-broken by lowest ID) must be evicted")
	}
	if _, ok := reg.byID["auth-0001"]; !ok {
		t.Fatal("same-timestamp higher ID must survive a single eviction")
	}
	if _, ok := reg.byID["auth-new"]; !ok {
		t.Fatal("newest observation missing")
	}
}

func TestObservedPreservesExactID(t *testing.T) {
	reg := resetObserved(t)
	observeCandidates([]pluginapi.SchedulerAuthCandidate{{ID: "  auth-1  ", Provider: "codex"}})
	reg.mu.Lock()
	entry, ok := reg.byID["  auth-1  "]
	reg.mu.Unlock()
	if !ok || entry.ID != "  auth-1  " {
		t.Fatalf("exact ID not preserved: %+v", entry)
	}
}

func TestObservedProviderKeys(t *testing.T) {
	reg := resetObserved(t)
	observeCandidates([]pluginapi.SchedulerAuthCandidate{{ID: "compat-acct", Provider: "openai-compatibility", Attributes: map[string]string{"provider_key": "deepseek"}}})
	reg.mu.Lock()
	entry, ok := reg.byID["compat-acct"]
	reg.mu.Unlock()
	if !ok {
		t.Fatal("candidate was not observed")
	}
	want := []string{"deepseek", "openai-compatibility"}
	if !slices.Equal(entry.ProviderKeys, want) {
		t.Fatalf("provider_keys = %v, want %v", entry.ProviderKeys, want)
	}
}

func TestCandidatesManagementRoute(t *testing.T) {
	resetObserved(t)
	observeCandidates([]pluginapi.SchedulerAuthCandidate{candidate("route-acct", "codex")})
	raw := testJSON(managementRequest{Method: "GET", Path: "/v0/management/plugins/provider-rate-limiter/candidates"})
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeManagementResponse(t, out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET candidates status = %d", resp.StatusCode)
	}
	text := string(resp.Body)
	for _, want := range []string{`"candidates"`, `"observed_since"`, `"route-acct"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("candidates body missing %q: %s", want, text)
		}
	}
	raw = testJSON(managementRequest{Method: "POST", Path: "/v0/management/plugins/provider-rate-limiter/candidates"})
	out, err = handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp = decodeManagementResponse(t, out)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST candidates status = %d", resp.StatusCode)
	}
}
