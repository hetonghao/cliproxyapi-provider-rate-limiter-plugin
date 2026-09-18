package main

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func providerKeyFor(c pluginapi.SchedulerAuthCandidate) string {
	p := strings.ToLower(strings.TrimSpace(c.Provider))
	if p != "openai-compatibility" && !strings.HasPrefix(p, "openai-compatible-") {
		return p
	}
	if k := strings.ToLower(strings.TrimSpace(c.Attributes["provider_key"])); k != "" {
		return k
	}
	if strings.HasPrefix(p, "openai-compatible-") {
		return p
	}
	if n := strings.ToLower(strings.TrimSpace(c.Attributes["compat_name"])); n != "" {
		if n == "openai-compatibility" || strings.HasPrefix(n, "openai-compatible-") {
			return n
		}
		return "openai-compatible-" + n
	}
	return p
}

func providerLimitKeys(c pluginapi.SchedulerAuthCandidate) []string {
	key := providerKeyFor(c)
	keys := []string{key}
	if short, ok := strings.CutPrefix(key, "openai-compatible-"); ok {
		keys = append(keys, short)
	}
	raw := strings.ToLower(strings.TrimSpace(c.Provider))
	if raw == "openai-compatibility" || strings.HasPrefix(raw, "openai-compatible-") {
		keys = append(keys, "openai-compatibility")
	}
	return keys
}
