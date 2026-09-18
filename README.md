# CLIProxyAPI Provider Rate Limiter

English | [简体中文](README.zh-CN.md)

CLIProxyAPI dynamic plugin implementing the official `scheduler` capability. It limits each candidate independently before CPA selects an auth record; no `X-Provider` header, extra port, or path is required.

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    provider-rate-limiter:
      enabled: true
      priority: 100
      queue_enabled: true
      queue_max_wait_ms: 15000
      queue_max_waiters: 256
      default_rpm: 60
      providers:
        codex: 60
        claude: 30
        gemini: 60
        openai-compatible-deepseek: 30
      auths:
        codex-account-01: 120
        codex-account-02: 90
```

Limits apply to every auth candidate exposed by CPA's scheduler, including Codex, Claude, Gemini, Antigravity, configured API keys, OpenAI-compatible upstreams and future provider plugins. Each auth ID has an independent sliding one-minute window; provider settings are per-auth defaults, not a shared provider-wide quota. An admission is counted when the plugin returns an AuthID, including requests whose upstream attempt subsequently fails.

Precedence is auth override, resolved provider key, compatible short alias, compatible fallback, then global default. For `openai-compatible-deepseek`, the provider keys are `openai-compatible-deepseek`, `deepseek`, then `openai-compatibility`. Compatibility attributes (`provider_key`, then `compat_name` for generic records) identify the logical upstream; ordinary provider types remain opaque. An explicitly configured `0` disables limiting at that level; an omitted override inherits. Negative limits are rejected.

## Queuing and limits

Queuing is enabled by default. `queue_max_wait_ms` defaults to 15000 per scheduler pick, and `queue_max_waiters` defaults to 256 pending picks per process. `queue_enabled: false` or `queue_max_wait_ms: 0` restores immediate rejection. `queue_max_waiters: 0` permits an unbounded queue and is not recommended in production.

Waiters are checked in arrival order. A waiter without a free candidate is skipped so unrelated provider pools are not blocked; waiters competing for the same candidates retain arrival order. This is candidate-aware FIFO, not strict global FIFO. Only successful admissions consume slots. Timeout returns retryable HTTP 429 `provider_rate_limit_exceeded` with `waited_ms` and `next_free_in_ms`; a full queue returns HTTP 429 `provider_rate_limit_queue_full`. Queuing absorbs bursts, not sustained traffic above the configured capacity, and does not guarantee elimination of 429s.

Reconfiguration preserves window hits and wakes pending picks. Lowering the wait limit can shorten an existing deadline but raising it does not extend existing waits. Disabling queuing rejects pending picks; lowering queue capacity affects subsequent enqueues. CPA can retry scheduler errors, so total waiting can approach `(request-retry + 1) * queue_max_wait_ms`, plus other request work. Tune this against caller and proxy timeouts.

The current C ABI does not deliver request cancellation to the plugin. CPA can return promptly after the client disconnects, but the plugin's bounded wait continues and may consume a slot later. This is not a fully cancellation-aware queue. Go shared libraries may remain mapped during reload, so in-memory history can survive re-enabling; a process restart clears it. Separate CPA processes do not share quotas or queues, even if they load the same plugin file. Requests routed directly through a model-router executor without auth selection bypass scheduler plugins.

## Management menu

The plugin registers a CPA management menu named **Provider Rate Limiter**. Open the sidebar menu, enter the CPA management key, and load the merged account list. The page combines `/v0/management/auth-files` with the plugin's `/v0/management/plugins/provider-rate-limiter/candidates` endpoint: config-defined auths that have no auth-files entry appear only after they have been seen in scheduler picks. The observed candidate list is a bounded process-local cache (up to 2000 entries keyed by exact auth ID), not an authoritative complete inventory; the page shows the observation start timestamp and warns when the list is incomplete or could not be loaded. Rows merge by exact ID, so observed candidates can introduce provider names unknown to auth-files, and saved overrides whose accounts no longer exist are shown as unmatched and remain editable or removable. Each row shows its effective limit and whether it comes from an auth override, a provider key or the global default, and the global card edits `queue_enabled`, `queue_max_wait_ms` and `queue_max_waiters`. The management key stays in the page only. Saving issues a PATCH to `/v0/management/plugins/provider-rate-limiter/config` followed by a PUT to the runtime settings endpoint; if the config persisted but the runtime save fails, the page reports that distinction instead of a generic success. Candidate output is a whitelist: the `config:` source category and resolved provider, the upstream URL reduced to its origin, and scheduler identity fields — never tokens or raw attributes. Every management request still requires the CPA management key; the public menu shell alone exposes no account data.

## Build

```bash
cd go
go test -race ./...
node --test menu_test.js
go vet ./...
go build -buildmode=c-shared -o /tmp/provider-rate-limiter.so .
```

Copy the `.so` into the CPA plugin directory for the target architecture (use `.dylib` on macOS), then restart or reload plugins according to the CPA deployment. On the current macOS development machine, cgo builds required `SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk` because the selected newer SDK is incompatible with the installed linker; this is a local toolchain workaround, not a portable required default. Windows and the admission queue live in a single process's memory; separate CPA processes do not share quotas or queues.

## Local CPA source development

For unreleased CPA SDK changes, add a temporary local `replace` in `go/go.mod`, then remove it before publishing:

```text
replace github.com/router-for-me/CLIProxyAPI/v7 => ../cliproxyapi-fork
```

## GitHub Actions

The repository CI runs race tests, static checks, and dynamic-library builds on Linux and macOS.

## Custom CPA plugin-store source

This repository is a maintained fork of the [lsmallice repository](https://github.com/lsmallice/cliproxyapi-provider-rate-limiter-plugin) and includes `registry.json` for use as a third-party CPA plugin-store source. Add the raw URL below to the CPA configuration under `plugins.store-sources`:

**Production installation must use the tagged GitHub Release archive and verify its SHA-256 entry in `checksums.txt`; do not install directly from an unverified branch build.**

```yaml
plugins:
  store-sources:
    - https://raw.githubusercontent.com/hetonghao/cliproxyapi-provider-rate-limiter-plugin/ai-cove/main/registry.json
```
