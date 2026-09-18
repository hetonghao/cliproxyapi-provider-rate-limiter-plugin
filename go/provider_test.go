package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestProviderKeyFor(t *testing.T) {
	tests := []struct {
		name      string
		candidate pluginapi.SchedulerAuthCandidate
		want      string
	}{
		{"plain provider", pluginapi.SchedulerAuthCandidate{Provider: "claude"}, "claude"},
		{"normalizes case and space", pluginapi.SchedulerAuthCandidate{Provider: "  Codex "}, "codex"},
		{"non-compat ignores attributes", pluginapi.SchedulerAuthCandidate{Provider: "gemini", Attributes: map[string]string{"provider_key": "other", "compat_name": "other"}}, "gemini"},
		{"compat provider_key attribute wins", pluginapi.SchedulerAuthCandidate{Provider: "openai-compatibility", Attributes: map[string]string{"provider_key": "openai-compatible-deepseek"}}, "openai-compatible-deepseek"},
		{"compat provider_key kept unprefixed", pluginapi.SchedulerAuthCandidate{Provider: "openai-compatible-deepseek", Attributes: map[string]string{"provider_key": "deepseek"}}, "deepseek"},
		{"prefixed provider without attributes", pluginapi.SchedulerAuthCandidate{Provider: "openai-compatible-deepseek"}, "openai-compatible-deepseek"},
		{"compat_name adds prefix", pluginapi.SchedulerAuthCandidate{Provider: "openai-compatibility", Attributes: map[string]string{"compat_name": "DeepSeek"}}, "openai-compatible-deepseek"},
		{"compat_name already prefixed", pluginapi.SchedulerAuthCandidate{Provider: "openai-compatibility", Attributes: map[string]string{"compat_name": "openai-compatible-deepseek"}}, "openai-compatible-deepseek"},
		{"compat_name bare compat stays unchanged", pluginapi.SchedulerAuthCandidate{Provider: "openai-compatibility", Attributes: map[string]string{"compat_name": "openai-compatibility"}}, "openai-compatibility"},
		{"bare compat without attributes", pluginapi.SchedulerAuthCandidate{Provider: "openai-compatibility"}, "openai-compatibility"},
		{"future provider stays opaque", pluginapi.SchedulerAuthCandidate{Provider: "future-provider", Attributes: map[string]string{"provider_key": "ignored"}}, "future-provider"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerKeyFor(tc.candidate); got != tc.want {
				t.Fatalf("providerKeyFor = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLimitForProviderAliases(t *testing.T) {
	tests := []struct {
		name      string
		candidate pluginapi.SchedulerAuthCandidate
		providers map[string]int
		auths     map[string]int
		want      int
	}{
		{"auth exact wins over provider", candidate("acct-1", "codex"), map[string]int{"codex": 10}, map[string]int{"acct-1": 99}, 99},
		{"codex direct", candidate("a", "codex"), map[string]int{"codex": 10}, nil, 10},
		{"claude direct", candidate("a", "claude"), map[string]int{"claude": 11}, nil, 11},
		{"gemini direct", candidate("a", "gemini"), map[string]int{"gemini": 12}, nil, 12},
		{"antigravity direct", candidate("a", "antigravity"), map[string]int{"antigravity": 13}, nil, 13},
		{"future provider direct", candidate("a", "future-provider"), map[string]int{"future-provider": 14}, nil, 14},
		{"compat full key beats short and fallback", candidate("a", "openai-compatible-deepseek"), map[string]int{"openai-compatible-deepseek": 20, "deepseek": 21, "openai-compatibility": 22}, nil, 20},
		{"compat short beats fallback", candidate("a", "openai-compatible-deepseek"), map[string]int{"deepseek": 21, "openai-compatibility": 22}, nil, 21},
		{"compat fallback only", candidate("a", "openai-compatible-deepseek"), map[string]int{"openai-compatibility": 22}, nil, 22},
		{"compat default", candidate("a", "openai-compatible-deepseek"), map[string]int{"codex": 10}, nil, 5},
		{"provider_key attribute resolves full key", pluginapi.SchedulerAuthCandidate{ID: "a", Provider: "openai-compatibility", Attributes: map[string]string{"provider_key": "openai-compatible-deepseek"}}, map[string]int{"openai-compatible-deepseek": 20, "openai-compatibility": 22}, nil, 20},
		{"provider_key attribute resolves short key", pluginapi.SchedulerAuthCandidate{ID: "a", Provider: "openai-compatibility", Attributes: map[string]string{"provider_key": "deepseek"}}, map[string]int{"deepseek": 21, "openai-compatibility": 22}, nil, 21},
		{"compat_name attribute resolves", pluginapi.SchedulerAuthCandidate{ID: "a", Provider: "openai-compatibility", Attributes: map[string]string{"compat_name": "deepseek"}}, map[string]int{"openai-compatible-deepseek": 20}, nil, 20},
		{"explicit zero provider disables", candidate("a", "openai-compatible-deepseek"), map[string]int{"deepseek": 0}, nil, 0},
		{"explicit zero auth disables", candidate("z", "codex"), map[string]int{"codex": 10}, map[string]int{"z": 0}, 0},
		{"claude ignores compat fallback", candidate("a", "claude"), map[string]int{"openai-compatibility": 22}, nil, 5},
		{"gemini ignores compat fallback", candidate("a", "gemini"), map[string]int{"openai-compatibility": 22}, nil, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := pluginConfig{DefaultRPM: 5, Providers: tc.providers, Auths: tc.auths}
			if got := limitFor(cfg, tc.candidate); got != tc.want {
				t.Fatalf("limitFor = %d, want %d", got, tc.want)
			}
		})
	}
}
