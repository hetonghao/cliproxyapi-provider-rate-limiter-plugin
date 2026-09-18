package main

import (
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const observedMaxCandidates = 2000

type observedCandidate struct {
	ID           string    `json:"id"`
	Provider     string    `json:"provider"`
	ProviderKeys []string  `json:"provider_keys"`
	Source       string    `json:"source,omitempty"`
	BaseURL      string    `json:"base_url,omitempty"`
	Priority     int       `json:"priority"`
	Status       string    `json:"status"`
	LastSeen     time.Time `json:"last_seen"`
}

type observedResponse struct {
	Candidates    []observedCandidate `json:"candidates"`
	ObservedSince time.Time           `json:"observed_since"`
}

type observedRegistry struct {
	mu    sync.Mutex
	since time.Time
	byID  map[string]observedCandidate
}

func newObservedRegistry(since time.Time) *observedRegistry {
	return &observedRegistry{since: since, byID: map[string]observedCandidate{}}
}

var observed = newObservedRegistry(time.Now())

func observeCandidates(candidates []pluginapi.SchedulerAuthCandidate) {
	reg := observed
	now := time.Now()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, c := range candidates {
		if strings.TrimSpace(c.ID) == "" {
			continue
		}
		reg.byID[c.ID] = observedCandidate{
			ID:           c.ID,
			Provider:     providerKeyFor(c),
			ProviderKeys: providerLimitKeys(c),
			Source:       observedSource(c),
			BaseURL:      observedBaseURL(c.Attributes["base_url"]),
			Priority:     c.Priority,
			Status:       c.Status,
			LastSeen:     now,
		}
	}
	for len(reg.byID) > observedMaxCandidates {
		reg.evictOldestLocked()
	}
}

func observedSource(c pluginapi.SchedulerAuthCandidate) string {
	if !strings.HasPrefix(strings.TrimSpace(c.Attributes["source"]), "config:") {
		return ""
	}
	return "config:" + providerKeyFor(c)
}

func observedBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	u.User = nil
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}

func (r *observedRegistry) evictOldestLocked() {
	var oldestID string
	var oldest time.Time
	found := false
	for id, c := range r.byID {
		if !found || c.LastSeen.Before(oldest) || (c.LastSeen.Equal(oldest) && id < oldestID) {
			oldest, oldestID, found = c.LastSeen, id, true
		}
	}
	if found {
		delete(r.byID, oldestID)
	}
}

func (r *observedRegistry) snapshotResponse() observedResponse {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]observedCandidate, 0, len(r.byID))
	for _, c := range r.byID {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return observedResponse{Candidates: out, ObservedSince: r.since}
}
