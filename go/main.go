package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef struct { uint32_t abi_version; void* host_ctx; void* call; void* free_buffer; } cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct { uint32_t abi_version; cliproxy_plugin_call_fn call; cliproxy_plugin_free_fn free_buffer; cliproxy_plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type pluginConfig struct {
	DefaultRPM      int            `yaml:"default_rpm" json:"default_rpm"`
	Providers       map[string]int `yaml:"providers" json:"providers"`
	Auths           map[string]int `yaml:"auths" json:"auths"`
	QueueEnabled    bool           `yaml:"queue_enabled" json:"queue_enabled"`
	QueueMaxWaitMS  int            `yaml:"queue_max_wait_ms" json:"queue_max_wait_ms"`
	QueueMaxWaiters int            `yaml:"queue_max_waiters" json:"queue_max_waiters"`
}

func defaultConfig() pluginConfig {
	return pluginConfig{Providers: map[string]int{}, Auths: map[string]int{}, QueueEnabled: true, QueueMaxWaitMS: 15000, QueueMaxWaiters: 256}
}

type window struct {
	mu   sync.Mutex
	hits []time.Time
}

func (w *window) allow(now time.Time, rpm int) bool {
	return w.allowFor(now, rpm, time.Minute)
}

func (w *window) allowFor(now time.Time, rpm int, duration time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked(now, duration)
	if rpm <= 0 {
		return true
	}
	if len(w.hits) >= rpm {
		return false
	}
	w.hits = append(w.hits, now)
	return true
}

func (w *window) nextFree(now time.Time, rpm int, duration time.Duration) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked(now, duration)
	if rpm <= 0 || len(w.hits) < rpm {
		return now
	}
	return w.hits[len(w.hits)-rpm].Add(duration)
}

func (w *window) pruneLocked(now time.Time, duration time.Duration) {
	cutoff := now.Add(-duration)
	keep := w.hits[:0]
	for _, t := range w.hits {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	w.hits = keep
}

var currentConfig atomic.Value

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}
type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
	StopRetry  bool   `json:"stop_retry,omitempty"`
}
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}
type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}
type registrationCapability struct {
	Scheduler     bool `json:"scheduler"`
	ManagementAPI bool `json:"management_api"`
}

type managementRegistration struct {
	Resources []resourceRoute   `json:"resources,omitempty"`
	Routes    []managementRoute `json:"routes,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu"`
	Description string `json:"description"`
}

type managementRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

type managementRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   []byte `json:"body"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, p *C.cliproxy_plugin_api) C.int {
	if p == nil {
		return 1
	}
	queue.reopen()
	p.abi_version = C.uint32_t(pluginabi.ABIVersion)
	p.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	p.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	p.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, n C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var b []byte
	if request != nil && n > 0 {
		b = C.GoBytes(unsafe.Pointer(request), C.int(n))
	}
	out, err := handleMethod(C.GoString(method), b)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, out)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { queue.shutdown() }
func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(raw); err != nil {
			return nil, err
		}
		return okEnvelope(registrationData())
	case pluginabi.MethodSchedulerPick:
		return pick(raw)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration{
			Resources: []resourceRoute{{Path: "/menu", Menu: "Provider Rate Limiter", Description: "View and manage Provider/AuthID rate limits."}},
			Routes:    []managementRoute{{Method: "GET", Path: "/plugins/provider-rate-limiter/settings"}, {Method: "PUT", Path: "/plugins/provider-rate-limiter/settings"}, {Method: "GET", Path: "/plugins/provider-rate-limiter/candidates"}},
		})
	case pluginabi.MethodManagementHandle:
		return handleManagement(raw)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}
func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			return err
		}
	}
	return setConfig(cfg)
}

func normalizeLimits(input map[string]int, lowerKeys bool) (map[string]int, error) {
	result := make(map[string]int, len(input))
	for rawKey, limit := range input {
		key := strings.TrimSpace(rawKey)
		if lowerKeys {
			key = strings.ToLower(key)
		}
		if key == "" {
			return nil, fmt.Errorf("empty key")
		}
		if limit < 0 {
			return nil, fmt.Errorf("%q has negative limit %d", key, limit)
		}
		if previous, exists := result[key]; exists && previous != limit {
			return nil, fmt.Errorf("duplicate key %q with conflicting limits", key)
		}
		result[key] = limit
	}
	return result, nil
}
func loaded() pluginConfig {
	if v := currentConfig.Load(); v != nil {
		return v.(pluginConfig)
	}
	return defaultConfig()
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	if strings.HasSuffix(path, "/menu") {
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: []byte(menuHTML())})
	}
	if strings.HasSuffix(path, "/candidates") {
		if strings.ToUpper(strings.TrimSpace(req.Method)) != http.MethodGet {
			return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusMethodNotAllowed, Headers: jsonHeaders(), Body: []byte(`{"error":"method_not_allowed"}`)})
		}
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: jsonHeaders(), Body: mustJSON(observed.snapshotResponse())})
	}
	if !strings.HasSuffix(path, "/settings") {
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"error":"not_found"}`)})
	}
	switch strings.ToUpper(strings.TrimSpace(req.Method)) {
	case http.MethodGet:
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: jsonHeaders(), Body: mustJSON(loaded())})
	case http.MethodPut:
		cfg := defaultConfig()
		if err := json.Unmarshal(req.Body, &cfg); err != nil {
			return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusBadRequest, Headers: jsonHeaders(), Body: []byte(`{"error":"invalid_json"}`)})
		}
		if err := setConfig(cfg); err != nil {
			return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusBadRequest, Headers: jsonHeaders(), Body: mustJSON(map[string]string{"error": err.Error()})})
		}
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: jsonHeaders(), Body: mustJSON(loaded())})
	default:
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusMethodNotAllowed, Headers: jsonHeaders(), Body: []byte(`{"error":"method_not_allowed"}`)})
	}
}

func jsonHeaders() http.Header {
	return http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}}
}

func setConfig(cfg pluginConfig) error {
	if cfg.DefaultRPM < 0 {
		return fmt.Errorf("default_rpm must be >= 0")
	}
	if cfg.QueueMaxWaitMS < 0 {
		return fmt.Errorf("queue_max_wait_ms must be >= 0")
	}
	if uint64(cfg.QueueMaxWaitMS) > uint64((1<<63-1)/int64(time.Millisecond)) {
		return fmt.Errorf("queue_max_wait_ms is too large")
	}
	if cfg.QueueMaxWaiters < 0 {
		return fmt.Errorf("queue_max_waiters must be >= 0")
	}
	providers, err := normalizeLimits(cfg.Providers, true)
	if err != nil {
		return fmt.Errorf("providers: %w", err)
	}
	auths, err := normalizeLimits(cfg.Auths, false)
	if err != nil {
		return fmt.Errorf("auths: %w", err)
	}
	cfg.Providers, cfg.Auths = providers, auths
	queue.mu.Lock()
	currentConfig.Store(cfg)
	queue.mu.Unlock()
	queue.signalWake()
	return nil
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func menuHTML() string {
	return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Provider Rate Limiter</title>
<style>
:root{color-scheme:light;--line:#d9e0e8;--muted:#64748b;--blue:#2563eb;--bg:#f8fafc}*{box-sizing:border-box}body{font:14px system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:0;padding:28px;max-width:1180px;color:#243447;background:var(--bg)}h1{margin:0 0 6px;font-size:24px}h2{font-size:17px;margin:0 0 14px}.hint,.subtle{color:var(--muted)}.toolbar,.card{background:#fff;border:1px solid var(--line);border-radius:10px;padding:18px;margin:16px 0}.toolbar{display:flex;gap:10px;align-items:end;flex-wrap:wrap}.toolbar .key{flex:1 1 320px}.toolbar label,.field label{display:block;color:#334155;font-weight:600;margin-bottom:6px}input,select{width:100%;padding:9px 10px;border:1px solid #cbd5e1;border-radius:7px;background:#fff;color:#243447}input[type=number]{max-width:180px}input[type=checkbox]{width:auto}input:focus-visible,select:focus-visible,button:focus-visible{outline:2px solid var(--blue);outline-offset:1px}button{padding:9px 15px;border:0;border-radius:7px;background:var(--blue);color:#fff;cursor:pointer;font-weight:600}button.secondary{background:#475569}button:disabled{opacity:.6;cursor:default}.actions{display:flex;gap:10px;align-items:center;flex-wrap:wrap}.status{margin-top:12px;padding:10px;border-radius:7px;background:#f1f5f9;white-space:pre-wrap}.status.error{background:#fef2f2;color:#991b1b}.status.ok{background:#ecfdf5;color:#166534}.field{margin-bottom:8px}.queue-fields{display:flex;gap:14px;flex-wrap:wrap}.queue-fields .field{flex:1 1 170px}.table-wrap{overflow:auto;border:1px solid var(--line);border-radius:8px}table{width:100%;border-collapse:collapse;min-width:650px}th,td{text-align:left;padding:10px 12px;border-bottom:1px solid #e5eaf0;vertical-align:middle}th{font-size:12px;text-transform:uppercase;letter-spacing:.02em;color:var(--muted);background:#f8fafc}tr:last-child td{border-bottom:0}.account-name{font-weight:600}.account-id{font:12px ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--muted);word-break:break-all}.badge{display:inline-block;padding:3px 7px;border-radius:999px;background:#e2e8f0;color:#475569;font-size:12px}.badge.disabled{background:#fee2e2;color:#991b1b}.limit-input{max-width:130px}.inherit{color:var(--muted);font-size:12px}.section-head{display:flex;justify-content:space-between;gap:12px;align-items:end;flex-wrap:wrap;margin-bottom:12px}.filters{display:flex;gap:8px;flex-wrap:wrap;margin-bottom:10px}.filters .field{flex:1 1 200px;margin-bottom:0}.filters input{min-width:220px}.empty{padding:22px;text-align:center;color:var(--muted)}footer{position:sticky;bottom:0;display:flex;justify-content:flex-end;align-items:center;gap:10px;padding:14px 0 0;background:linear-gradient(transparent,var(--bg) 28%)}@media (max-width:640px){input,select{font-size:16px}}
</style></head><body>
<h1>Provider Rate Limiter</h1><p class="hint">按账号限制请求速率。账号限制覆盖 Provider 限制，Provider 限制覆盖全局默认值。/ Per-auth sliding-window limits: AuthID overrides Provider, which overrides the default.</p>
<section class="toolbar"><div class="key"><label for="key">CPA 管理密钥 / Management key</label><input id="key" type="password" autocomplete="off" placeholder="仅保存在当前页面 / kept only in this page"></div><div class="actions"><button class="secondary" onclick="loadAll()">读取账号和配置 / Load</button></div></section>
<section class="card"><h2>全局默认 / Global default</h2><div class="queue-fields"><div class="field"><label for="default">默认 RPM / Default RPM</label><input id="default" type="number" min="0" step="1" value="0"><div class="subtle">0 表示该层级不限流。/ 0 disables the limit at this level.</div></div><div class="field"><label for="queueEnabled">超限排队 / Queue when limited</label><input id="queueEnabled" type="checkbox" checked><div class="subtle">开启后候选全部超限时排队等待，而不是立即 429。/ When on, picks wait instead of failing immediately.</div></div><div class="field"><label for="queueMaxWaitMS">单次最长等待 (ms) / Max wait per pick (ms)</label><input id="queueMaxWaitMS" type="number" min="0" max="9223372036854" step="1" value="15000"><div class="subtle">0 表示禁用排队；支持 stop_retry 的 CPA 在本地限流拒绝后结束本次请求，不重复排队；旧版 CPA 仍可能重复等待。/ 0 disables queuing; CPA with stop_retry support ends the request after local admission rejection instead of queuing again; older CPA may still repeat waits.</div></div><div class="field"><label for="queueMaxWaiters">最大排队数 / Max queued picks</label><input id="queueMaxWaiters" type="number" min="0" step="1" value="256"><div class="subtle">达到上限立即 429；0 为不限（生产不建议）。/ Full queue returns 429; 0 means unlimited (not recommended in production).</div></div></div><div class="subtle">排队满或等待超时仍返回 429。/ A full queue or a wait timeout still returns 429.</div></section>
<section class="card"><div class="section-head"><div><h2>Provider 默认限制 / Provider limits</h2><div class="subtle">限制应用到该 Provider 的每个账号，而不是所有账号合计。/ Applied independently to each account.</div></div></div><div class="table-wrap"><table><thead><tr><th>Provider</th><th>账号数 / Accounts</th><th>RPM 覆盖 / RPM override</th></tr></thead><tbody id="providerRows"><tr><td colspan="3" class="empty">请先读取账号 / Load accounts first</td></tr></tbody></table></div></section>
<section class="card"><div class="section-head"><div><h2>账号限制 / Account limits</h2><div class="subtle">文件授权、已观测候选及未匹配覆盖；留空继承。/ File auths, observed candidates and unmatched overrides; blank inherits.</div></div><div class="filters"><div class="field"><label for="accountFilter">搜索 / Search</label><input id="accountFilter" placeholder="搜索账号 / Search account" oninput="renderAccounts()"></div><div class="field"><label for="providerFilter">Provider 筛选 / Provider filter</label><select id="providerFilter" onchange="renderAccounts()"><option value="">全部 Provider / All providers</option></select></div></div></div><div class="table-wrap"><table><thead><tr><th>账号 / Account</th><th>Provider</th><th>状态 / Status</th><th>RPM 覆盖 / RPM override</th></tr></thead><tbody id="accountRows"><tr><td colspan="4" class="empty">请先读取账号 / Load accounts first</td></tr></tbody></table></div><div id="observedStatus" class="subtle"></div></section>
<div id="status" class="status" role="status" aria-live="polite">请输入管理密钥，然后点击“读取账号和配置”。/ Enter the management key, then click Load.</div><footer><div class="subtle" style="margin-right:auto">保存会同时写入运行中的插件和 CPA 配置文件。/ Save writes to both the running plugin and the CPA config file.</div><button class="secondary" onclick="loadAll()">重新读取 / Reload</button><button id="saveButton" onclick="saveAll()" disabled>保存运行时配置 / Save runtime settings</button></footer>
<script>
const endpoint='/v0/management/plugins/provider-rate-limiter/settings';
const persistEndpoint='/v0/management/plugins/provider-rate-limiter/config';
const accountsEndpoint='/v0/management/auth-files';
const candidatesEndpoint='/v0/management/plugins/provider-rate-limiter/candidates';
const statusEl=document.getElementById('status');
const state={accounts:[],providers:Object.create(null),auths:Object.create(null),defaultRPM:0,queueEnabled:true,queueMaxWaitMS:15000,queueMaxWaiters:256,loaded:false,busy:false,observedCount:0,observedSince:'',observationWarning:''};
function headers(key){return {'Content-Type':'application/json','X-Management-Key':key};}
function esc(value){return String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));}
function setStatus(message,kind){statusEl.className='status '+(kind||'');statusEl.textContent=message;}
function limitValue(value,required){const text=String(value??'').trim();if(!text){if(required)throw Error('数值不能为空 / Value is required');return null;}const n=Number(text);if(!Number.isSafeInteger(n)||n<0)throw Error('数值必须是大于等于 0 的整数 / Value must be a non-negative integer');return n;}
function draftValue(value){const text=String(value??'').trim();if(!text)return null;const n=Number(text);if(!Number.isSafeInteger(n)||n<0)return undefined;return n;}
async function getJSON(url,options,key){const r=await fetch(url,{...(options||{}),headers:{...headers(key),...((options&&options.headers)||{})}});const text=await r.text();if(!r.ok){let message='HTTP '+r.status;try{const data=JSON.parse(text);message=data.error||data.message||message;}catch(e){}throw Error(message);}try{return JSON.parse(text);}catch(e){throw Error('响应不是有效 JSON / Response is not valid JSON');}}
function validateSettings(settings){
 if(!settings||typeof settings!=='object'||Array.isArray(settings)||!Number.isSafeInteger(settings.default_rpm)||settings.default_rpm<0)throw Error('配置响应无效 / Invalid settings response');
 for(const name of ['providers','auths']){const map=settings[name];if(!map||typeof map!=='object'||Array.isArray(map)||Object.values(map).some(n=>!Number.isSafeInteger(n)||n<0))throw Error('配置响应无效 / Invalid settings response');}
 if(settings.queue_enabled!==undefined&&typeof settings.queue_enabled!=='boolean')throw Error('配置响应无效 / Invalid settings response');
 for(const name of ['queue_max_wait_ms','queue_max_waiters'])if(settings[name]!==undefined&&(!Number.isSafeInteger(settings[name])||settings[name]<0))throw Error('配置响应无效 / Invalid settings response');
 return settings;
}
function providerKeys(provider){const keys=[provider];if(provider.startsWith('openai-compatible-'))keys.push(provider.slice('openai-compatible-'.length),'openai-compatibility');return keys;}
function providerLabel(provider){return provider.startsWith('openai-compatible-')?provider.slice('openai-compatible-'.length):provider;}
function mergeAccounts(files,candidates,auths){
 const rows=new Map();
 for(const file of Array.isArray(files)?files:[]){const id=String(file.id||file.name||'');if(!id.trim())continue;const provider=String(file.provider||file.type||'unknown').trim().toLowerCase();rows.set(id,{id,label:String(file.label||file.email||file.account||id),provider,providerKeys:providerKeys(provider),source:'',disabled:Boolean(file.disabled),status:String(file.status||''),observed:false,unmatched:false});}
 for(const c of Array.isArray(candidates)?candidates:[]){const id=String(c.id||'');if(!id.trim())continue;const old=rows.get(id);const provider=String(c.provider||old?.provider||'unknown').trim().toLowerCase();rows.set(id,{...old,id,label:old?.label||String(c.source||id),provider,providerKeys:Array.isArray(c.provider_keys)&&c.provider_keys.length?c.provider_keys:providerKeys(provider),source:String(c.source||''),disabled:old?.disabled||false,status:old?.status||String(c.status||''),observed:true,unmatched:false});}
 for(const id of Object.keys(auths)){if(!rows.has(id))rows.set(id,{id,label:id,provider:'unknown',providerKeys:[],source:'',disabled:false,status:'',observed:false,unmatched:true});}
 return Array.from(rows.values()).sort((a,b)=>a.provider.localeCompare(b.provider)||a.label.localeCompare(b.label)||a.id.localeCompare(b.id));
}
function effectiveLimit(item){
 if(Object.hasOwn(state.auths,item.id))return {value:state.auths[item.id],source:'AuthID'};
 for(const key of item.providerKeys){if(Object.hasOwn(state.providers,key))return {value:state.providers[key],source:'Provider: '+key};}
 return {value:document.getElementById('default').value,source:'全局 / Default'};
}
function toMap(obj){const map=Object.create(null);if(obj&&typeof obj==='object'){for(const k of Object.keys(obj))map[k]=obj[k];}return map;}
function accountProviders(){return Array.from(new Set(state.accounts.map(item=>item.provider).filter(Boolean))).sort();}
function providerNames(){const names=new Set(state.accounts.map(item=>item.provider).filter(Boolean));Object.keys(state.providers).forEach(name=>names.add(name));names.add('openai-compatibility');return Array.from(names).sort();}
function providerEffective(provider){return effectiveLimit({id:'',providerKeys:state.accounts.find(item=>item.provider===provider)?.providerKeys??providerKeys(provider)});}
function formatEffective(eff){const n=draftValue(eff.value);if(n===null||n===undefined)return '待校验 / Pending validation';if(n===0)return '不限 / Unlimited · '+eff.source;return n+' RPM · '+eff.source;}
function refreshEffectiveLabels(){const items=new Map(state.accounts.map(item=>[item.id,item]));document.querySelectorAll('[data-effective-provider]').forEach(el=>{el.textContent=formatEffective(providerEffective(el.getAttribute('data-effective-provider')));});document.querySelectorAll('[data-effective-auth]').forEach(el=>{const item=items.get(el.getAttribute('data-effective-auth'));if(item)el.textContent=formatEffective(effectiveLimit(item));});}
function renderProviders(){const rows=document.getElementById('providerRows');const names=providerNames();if(!names.length){rows.innerHTML='<tr><td colspan="3" class="empty">没有发现 Provider / No providers found</td></tr>';return;}rows.innerHTML=names.map(provider=>{const count=state.accounts.filter(item=>item.provider===provider).length;const value=Object.hasOwn(state.providers,provider)?state.providers[provider]:'';const isCompat=provider.startsWith('openai-compatible-');const title=provider==='openai-compatibility'?'openai-compatibility':providerLabel(provider);const subtitle=provider==='openai-compatibility'?'全部兼容上游默认兜底 / Default for all compatible providers':(isCompat?provider:'');return '<tr><td><strong>'+esc(title)+'</strong>'+(subtitle?'<div class="subtle">'+esc(subtitle)+'</div>':'')+'</td><td>'+count+'</td><td><input class="limit-input provider-limit" data-provider="'+esc(provider)+'" type="number" min="0" step="1" value="'+esc(value)+'" placeholder="继承全局 / inherit" aria-label="'+esc(provider)+' RPM"><div class="inherit" data-effective-provider="'+esc(provider)+'"></div></td></tr>';}).join('');refreshEffectiveLabels();}
function renderProviderFilter(){const select=document.getElementById('providerFilter');const current=select.value;const names=accountProviders();select.innerHTML='<option value="">全部 Provider / All providers</option>'+names.map(name=>'<option value="'+esc(name)+'">'+esc(providerLabel(name))+(name.startsWith('openai-compatible-')?' ('+esc(name)+')':'')+'</option>').join('');if(names.includes(current))select.value=current;}
function renderAccounts(){const rows=document.getElementById('accountRows');const query=document.getElementById('accountFilter').value.trim().toLowerCase();const provider=document.getElementById('providerFilter').value;const accounts=state.accounts.filter(item=>(!query||[item.id,item.label,item.source,item.provider].join(' ').toLowerCase().includes(query))&&(!provider||item.provider===provider));if(!accounts.length){rows.innerHTML='<tr><td colspan="4" class="empty">没有匹配账号 / No matching accounts</td></tr>';return;}rows.innerHTML=accounts.map(item=>{const value=Object.hasOwn(state.auths,item.id)?state.auths[item.id]:'';const badges=[];if(item.disabled)badges.push('<span class="badge disabled">已禁用 / disabled</span>');if(item.status)badges.push('<span class="badge">'+esc(item.status)+'</span>');if(item.observed)badges.push('<span class="badge">已观测 / Observed</span>');if(item.source.startsWith('config:'))badges.push('<span class="badge">配置授权 / Config auth</span>');if(item.unmatched)badges.push('<span class="badge disabled">未匹配账号 / Unmatched</span>');return '<tr><td><div class="account-name">'+esc(item.label)+'</div><div class="account-id">'+esc(item.id)+'</div>'+(item.source?'<div class="account-id">'+esc(item.source)+'</div>':'')+'</td><td>'+esc(providerLabel(item.provider))+(item.provider.startsWith('openai-compatible-')?'<div class="subtle">'+esc(item.provider)+'</div>':'')+'</td><td>'+badges.join(' ')+'</td><td><input class="limit-input auth-limit" data-auth-id="'+esc(item.id)+'" type="number" min="0" step="1" value="'+esc(value)+'" placeholder="继承 / inherit" aria-label="'+esc(item.id)+' RPM"><div class="inherit" data-effective-auth="'+esc(item.id)+'"></div></td></tr>';}).join('');refreshEffectiveLabels();}
function renderObservedStatus(){const el=document.getElementById('observedStatus');if(state.observationWarning){el.textContent=state.observationWarning;return;}if(!state.loaded){el.textContent='';return;}el.textContent='已观测 '+state.observedCount+' 个候选（自 '+state.observedSince+' 起；仅本进程内记录，重启清空，配置授权需在产生流量后才会出现）。/ Observed '+state.observedCount+' candidates since '+state.observedSince+'; process-local and cleared on restart; config auths appear only after traffic.';}
function render(){document.getElementById('default').value=state.defaultRPM;document.getElementById('queueEnabled').checked=state.queueEnabled;document.getElementById('queueMaxWaitMS').value=state.queueMaxWaitMS;document.getElementById('queueMaxWaiters').value=state.queueMaxWaiters;renderProviderFilter();renderProviders();renderAccounts();renderObservedStatus();}
function updateControls(){const disableAll=state.busy;document.querySelectorAll('input,select,button').forEach(el=>{el.disabled=disableAll;});if(!disableAll)document.getElementById('saveButton').disabled=!state.loaded;}
function bindDraftInputs(){document.getElementById('providerRows').addEventListener('input',event=>{const input=event.target;if(!input||!input.classList||!input.classList.contains('provider-limit'))return;const key=input.getAttribute('data-provider');if(key===null)return;if(String(input.value).trim()==='')delete state.providers[key];else state.providers[key]=input.value;refreshEffectiveLabels();});document.getElementById('accountRows').addEventListener('input',event=>{const input=event.target;if(!input||!input.classList||!input.classList.contains('auth-limit'))return;const key=input.getAttribute('data-auth-id');if(key===null)return;if(String(input.value).trim()==='')delete state.auths[key];else state.auths[key]=input.value;refreshEffectiveLabels();});document.getElementById('default').addEventListener('input',refreshEffectiveLabels);}
async function loadAll(){const key=document.getElementById('key').value.trim();if(!key){setStatus('请输入 CPA 管理密钥 / Enter the CPA management key.','error');return;}if(state.busy)return;state.busy=true;updateControls();setStatus('正在读取账号和配置 / Loading accounts and settings...');try{const results=await Promise.all([getJSON(endpoint,{},key),getJSON(accountsEndpoint,{},key),getJSON(candidatesEndpoint,{},key).then(data=>({data}),error=>({error}))]);const settings=validateSettings(results[0]);const files=results[1];if(!files||!Array.isArray(files.files))throw Error('账号列表响应无效 / Invalid auth-files response');const cands=results[2];const providers=toMap(settings.providers);const auths=toMap(settings.auths);const candidates=Array.isArray(cands.data&&cands.data.candidates)?cands.data.candidates:[];state.defaultRPM=settings.default_rpm??0;state.providers=providers;state.auths=auths;state.queueEnabled=settings.queue_enabled??true;state.queueMaxWaitMS=settings.queue_max_wait_ms??15000;state.queueMaxWaiters=settings.queue_max_waiters??256;state.accounts=mergeAccounts(files.files,candidates,auths);if(cands.error){state.observationWarning='已观测候选读取失败，仅显示 auth-files 账号。/ Failed to load observed candidates; showing auth-files only. ('+cands.error.message+')';state.observedCount=0;state.observedSince='';}else{state.observationWarning='';state.observedCount=candidates.length;state.observedSince=String(cands.data&&cands.data.observed_since||'');}state.loaded=true;render();setStatus('已读取 '+state.accounts.length+' 个账号。/ Loaded '+state.accounts.length+' accounts.','ok');}catch(error){state.loaded=false;setStatus('读取失败 / Load failed: '+error.message,'error');}finally{state.busy=false;updateControls();}}
function collectValidated(map){const out=Object.create(null);for(const key of Object.keys(map)){const value=limitValue(map[key]);if(value!==null)out[key]=value;}return out;}
async function saveAll(){const key=document.getElementById('key').value.trim();if(!state.loaded||state.busy)return;if(!key){setStatus('请输入 CPA 管理密钥 / Enter the CPA management key.','error');return;}let body;try{const defaultRPM=limitValue(document.getElementById('default').value,true);const providers=collectValidated(state.providers);const auths=collectValidated(state.auths);const queueMaxWaitMS=limitValue(document.getElementById('queueMaxWaitMS').value,true);if(queueMaxWaitMS>9223372036854)throw Error('queue_max_wait_ms 超出上限 / queue_max_wait_ms exceeds the maximum');const queueMaxWaiters=limitValue(document.getElementById('queueMaxWaiters').value,true);body={default_rpm:defaultRPM,providers,auths,queue_enabled:document.getElementById('queueEnabled').checked,queue_max_wait_ms:queueMaxWaitMS,queue_max_waiters:queueMaxWaiters};}catch(error){setStatus('校验失败 / Validation failed: '+error.message,'error');return;}state.busy=true;updateControls();setStatus('正在保存 / Saving...');try{await getJSON(persistEndpoint,{method:'PATCH',body:JSON.stringify(body)},key);let saved;try{saved=validateSettings(await getJSON(endpoint,{method:'PUT',body:JSON.stringify(body)},key));}catch(error){setStatus('已持久化，但运行时确认失败，请重新读取 / Persisted, but runtime confirmation failed; please reload. '+error.message,'error');return;}state.defaultRPM=saved.default_rpm??0;state.providers=toMap(saved.providers);state.auths=toMap(saved.auths);state.queueEnabled=saved.queue_enabled??true;state.queueMaxWaitMS=saved.queue_max_wait_ms??15000;state.queueMaxWaiters=saved.queue_max_waiters??256;render();setStatus('已保存到运行中的插件和 CPA 配置，重启后仍会保留。/ Saved to the running plugin and CPA config; the settings survive restart.','ok');}catch(error){setStatus('保存失败 / Save failed: '+error.message,'error');}finally{state.busy=false;updateControls();}}
bindDraftInputs();
updateControls();
</script></body></html>`
}
func limitFor(cfg pluginConfig, c pluginapi.SchedulerAuthCandidate) int {
	if n, ok := cfg.Auths[c.ID]; ok {
		return n
	}
	for _, key := range providerLimitKeys(c) {
		if n, ok := cfg.Providers[key]; ok {
			return n
		}
	}
	return cfg.DefaultRPM
}
func pick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	observeCandidates(req.Candidates)
	outcome := queue.pick(req)
	if outcome.authID != "" {
		return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: outcome.authID, Handled: true})
	}
	return errorEnvelopeWithStatus(outcome.code, outcome.message, outcome.status), nil
}
func registrationData() registration {
	return registration{SchemaVersion: pluginabi.SchemaVersion, Metadata: pluginapi.Metadata{Name: "provider-rate-limiter", Version: "0.5.2-aicove.1", Author: "hetonghao", GitHubRepository: "https://github.com/hetonghao/cliproxyapi-provider-rate-limiter-plugin", ConfigFields: []pluginapi.ConfigField{{Name: "default_rpm", Type: pluginapi.ConfigFieldTypeInteger, Description: "Default RPM applied independently to every candidate."}, {Name: "providers", Type: pluginapi.ConfigFieldTypeObject, Description: "Provider name to RPM override map."}, {Name: "auths", Type: pluginapi.ConfigFieldTypeObject, Description: "AuthID to RPM override map; overrides provider and default."}, {Name: "queue_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Queue picks instead of failing when every candidate is over the limit. Default true."}, {Name: "queue_max_wait_ms", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum milliseconds a pick waits in the admission queue. Default 15000; 0 disables waiting."}, {Name: "queue_max_waiters", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum queued picks before new picks fail. Default 256; 0 means unlimited."}}}, Capabilities: registrationCapability{Scheduler: true, ManagementAPI: true}}
}
func okEnvelope(v any) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	return json.Marshal(envelope{OK: true, Result: b})
}
func errorEnvelope(code, msg string) []byte {
	return errorEnvelopeWithStatus(code, msg, 0)
}

func errorEnvelopeWithStatus(code, msg string, status int) []byte {
	b, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: msg, HTTPStatus: status, StopRetry: status == http.StatusTooManyRequests}})
	return b
}
func writeResponse(r *C.cliproxy_buffer, b []byte) {
	if r == nil || len(b) == 0 {
		return
	}
	p := C.CBytes(b)
	if p == nil {
		return
	}
	r.ptr = p
	r.len = C.size_t(len(b))
}
