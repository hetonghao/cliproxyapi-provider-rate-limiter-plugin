const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const goSource = fs.readFileSync(path.join(__dirname, 'main.go'), 'utf8');
const scriptMatch = goSource.match(/<script>([\s\S]*?)<\/script>/);
assert.ok(scriptMatch, 'inline <script> not found in menuHTML');
const scriptSource = scriptMatch[1];

function createHarness() {
  const elements = new Map();
  function el(id) {
    if (!elements.has(id)) {
      elements.set(id, {
        id, attrs: {}, listeners: {}, value: '', checked: false, disabled: false,
        innerHTML: '', textContent: '', className: '',
        addEventListener(type, fn) { (this.listeners[type] ??= []).push(fn); },
        getAttribute(name) { return this.attrs[name] ?? null; },
        setAttribute(name, value) { this.attrs[name] = value; },
      });
    }
    return elements.get(id);
  }
  ['key', 'status', 'default', 'queueEnabled', 'queueMaxWaitMS', 'queueMaxWaiters',
    'providerRows', 'accountRows', 'accountFilter', 'providerFilter', 'observedStatus', 'saveButton']
    .forEach(el);
  const effectiveStubs = [];
  const document = {
    getElementById: (id) => elements.get(id) ?? null,
    querySelectorAll: (selector) => {
      if (selector === 'input,select,button') return Array.from(elements.values());
      const m = selector.match(/^\[data-effective-(provider|auth)\]$/);
      if (!m) return [];
      const attr = 'data-effective-' + m[1];
      const out = [];
      for (const e of elements.values()) {
        for (const mm of String(e.innerHTML).matchAll(new RegExp(attr + '="([^"]*)"', 'g'))) {
          const stub = { attrName: attr, attrValue: mm[1], textContent: '', getAttribute: (n) => (n === attr ? mm[1] : null) };
          effectiveStubs.push(stub);
          out.push(stub);
        }
      }
      return out;
    },
  };
  const calls = [];
  const responses = [];
  const fetch = (url, options = {}) => {
    calls.push({ url, method: options.method || 'GET', body: options.body, headers: options.headers });
    const next = responses.shift();
    if (next === undefined) return Promise.reject(new Error('unexpected fetch ' + url));
    if (next === 'pending') return new Promise(() => {});
    if (next instanceof Error) return Promise.reject(next);
    return Promise.resolve({
      ok: next.ok ?? true,
      status: next.status ?? 200,
      statusText: next.statusText ?? 'OK',
      text: () => Promise.resolve(next.text ?? ''),
    });
  };
  const context = vm.createContext({ document, fetch });
  vm.runInContext(scriptSource, context);
  return {
    elements, document, calls, responses, effectiveStubs,
    run: (code) => vm.runInContext(code, context),
    state: () => vm.runInContext('state', context),
    fn: (name) => vm.runInContext(name, context),
  };
}

function jsonResponse(body, status = 200) {
  return { ok: status >= 200 && status < 300, status, text: JSON.stringify(body) };
}

function plain(value) {
  return JSON.parse(JSON.stringify(value));
}

function settingsBody(overrides = {}) {
  return { default_rpm: 3, providers: {}, auths: {}, queue_enabled: true, queue_max_wait_ms: 15000, queue_max_waiters: 256, ...overrides };
}

async function load(h, { settings = settingsBody(), files = [], candidates = [], observed_since = '2026-09-18T00:00:00Z' } = {}) {
  h.elements.get('key').value = 'k';
  h.responses.push(jsonResponse(settings), jsonResponse({ files }), jsonResponse({ candidates, observed_since }));
  await h.fn('loadAll')();
}

function fireInput(h, containerId, cls, attrName, key, value) {
  const target = { classList: { contains: (c) => c === cls }, getAttribute: (n) => (n === attrName ? key : null), value };
  for (const fn of h.elements.get(containerId).listeners.input ?? []) fn({ target });
}

function latestStub(h, attrName, attrValue) {
  return h.effectiveStubs.filter((s) => s.attrName === attrName && s.attrValue === attrValue).at(-1);
}

test('mergeAccounts merges file and observed rows by exact ID', (t) => {
  const h = createHarness();
  const merge = h.fn('mergeAccounts');
  const rows = merge(
    [{ id: 'a1', label: 'File Label', provider: 'codex', status: 'ready' }],
    [{ id: 'a1', provider: 'openai-compatible-deepseek', source: 'config:openai-compatible-deepseek', status: 'active', provider_keys: ['openai-compatible-deepseek', 'deepseek', 'openai-compatibility'] }],
    {},
  );
  assert.equal(rows.length, 1);
  assert.equal(rows[0].id, 'a1');
  assert.equal(rows[0].label, 'File Label');
  assert.equal(rows[0].provider, 'openai-compatible-deepseek');
  assert.equal(rows[0].observed, true);
  assert.equal(rows[0].source, 'config:openai-compatible-deepseek');
  assert.equal(rows[0].status, 'ready');
  assert.deepEqual(plain(rows[0].providerKeys), ['openai-compatible-deepseek', 'deepseek', 'openai-compatibility']);
});

test('mergeAccounts surfaces unknown and new providers and orphan overrides', (t) => {
  const h = createHarness();
  const merge = h.fn('mergeAccounts');
  const rows = merge([], [{ id: 'x', provider: 'brand-new-provider' }], { 'ghost-1': 7 });
  assert.equal(rows.length, 2);
  const seen = rows.find((r) => r.id === 'x');
  assert.equal(seen.provider, 'brand-new-provider');
  assert.deepEqual(plain(seen.providerKeys), ['brand-new-provider']);
  const orphan = rows.find((r) => r.id === 'ghost-1');
  assert.equal(orphan.unmatched, true);
  assert.equal(orphan.provider, 'unknown');
});

test('providerNames unions account providers, settings keys and the compat fallback', (t) => {
  const h = createHarness();
  const state = h.state();
  state.accounts = h.fn('mergeAccounts')([{ id: 'a', provider: 'codex' }], [], {});
  state.providers = { deepseek: 5 };
  const names = h.fn('providerNames')();
  assert.ok(names.includes('codex'));
  assert.ok(names.includes('deepseek'));
  assert.ok(names.includes('openai-compatibility'));
});

test('effectiveLimit resolves auth, full key, alias, compat fallback then default incl. explicit 0', (t) => {
  const h = createHarness();
  const state = h.state();
  const effective = h.fn('effectiveLimit');
  const item = { id: 'a', providerKeys: ['openai-compatible-deepseek', 'deepseek', 'openai-compatibility'] };
  h.elements.get('default').value = '3';
  state.auths = { a: 99 };
  state.providers = { 'openai-compatible-deepseek': 20, deepseek: 21, 'openai-compatibility': 22 };
  assert.deepEqual(plain(effective(item)), { value: 99, source: 'AuthID' });
  state.auths = {};
  assert.deepEqual(plain(effective(item)), { value: 20, source: 'Provider: openai-compatible-deepseek' });
  state.providers = { deepseek: 21, 'openai-compatibility': 22 };
  assert.deepEqual(plain(effective(item)), { value: 21, source: 'Provider: deepseek' });
  state.providers = { 'openai-compatibility': 22 };
  assert.deepEqual(plain(effective(item)), { value: 22, source: 'Provider: openai-compatibility' });
  state.providers = { deepseek: 0 };
  assert.deepEqual(plain(effective(item)), { value: 0, source: 'Provider: deepseek' });
  assert.match(h.fn('formatEffective')(effective(item)), /不限|Unlimited/);
  state.providers = {};
  assert.deepEqual(plain(effective(item)), { value: '3', source: '全局 / Default' });
});

test('__proto__ auth and provider keys work through null-prototype maps', (t) => {
  const h = createHarness();
  const state = h.state();
  const auths = Object.create(null);
  auths.__proto__ = 7;
  state.auths = auths;
  assert.deepEqual(plain(h.fn('effectiveLimit')({ id: '__proto__', providerKeys: [] })), { value: 7, source: 'AuthID' });
  const providers = Object.create(null);
  providers.__proto__ = 3;
  state.auths = Object.create(null);
  state.providers = providers;
  assert.deepEqual(plain(h.fn('effectiveLimit')({ id: 'x', providerKeys: ['__proto__'] })), { value: 3, source: 'Provider: __proto__' });
  const rows = h.fn('mergeAccounts')([], [], authsOf('__proto__', 7));
  assert.equal(rows[0].id, '__proto__');
  assert.equal(rows[0].unmatched, true);
});

function authsOf(key, value) {
  const map = Object.create(null);
  map[key] = value;
  return map;
}

test('renderAccounts escapes malicious labels and ids', (t) => {
  const h = createHarness();
  const state = h.state();
  state.accounts = h.fn('mergeAccounts')([{ id: 'a<b>"\'', label: '<img src=x onerror=alert(1)>', provider: 'codex' }], [], {});
  state.loaded = true;
  h.fn('renderAccounts')();
  const html = h.elements.get('accountRows').innerHTML;
  assert.ok(!html.includes('<img'), 'label must be escaped: ' + html);
  assert.ok(html.includes('&lt;img src=x onerror=alert(1)&gt;'), 'label must render as escaped text: ' + html);
  assert.ok(html.includes('&lt;img'));
});

test('draft edits persist across filter re-renders', (t) => {
  const h = createHarness();
  const state = h.state();
  state.accounts = h.fn('mergeAccounts')([{ id: 'a1', provider: 'codex' }, { id: 'a2', provider: 'claude' }], [], {});
  state.loaded = true;
  h.fn('renderAccounts')();
  fireInput(h, 'accountRows', 'auth-limit', 'data-auth-id', 'a2', '9');
  assert.equal(state.auths.a2, '9');
  h.elements.get('providerFilter').value = 'codex';
  h.fn('renderAccounts')();
  const html = h.elements.get('accountRows').innerHTML;
  assert.ok(html.includes('a1'));
  assert.ok(!html.includes('a2'));
  assert.equal(state.auths.a2, '9', 'hidden draft must survive filtering');
  fireInput(h, 'accountRows', 'auth-limit', 'data-auth-id', 'a2', '');
  assert.ok(!Object.hasOwn(state.auths, 'a2'), 'empty input deletes the draft');
});

test('provider draft input writes to the full provider key and refreshes effective labels', (t) => {
  const h = createHarness();
  const state = h.state();
  state.accounts = h.fn('mergeAccounts')([{ id: 'a1', provider: 'openai-compatible-deepseek' }], [], {});
  state.loaded = true;
  h.fn('render')();
  fireInput(h, 'providerRows', 'provider-limit', 'data-provider', 'openai-compatible-deepseek', '8');
  assert.equal(state.providers['openai-compatible-deepseek'], '8');
  const stub = latestStub(h, 'data-effective-auth', 'a1');
  assert.equal(stub.textContent, '8 RPM · Provider: openai-compatible-deepseek');
});

test('loadAll retains explicit false and 0 queue settings', async (t) => {
  const h = createHarness();
  await load(h, { settings: settingsBody({ queue_enabled: false, queue_max_wait_ms: 0, queue_max_waiters: 0 }) });
  const state = h.state();
  assert.equal(state.queueEnabled, false);
  assert.equal(state.queueMaxWaitMS, 0);
  assert.equal(state.queueMaxWaiters, 0);
  assert.equal(state.loaded, true);
  assert.equal(h.elements.get('queueEnabled').checked, false);
});

test('saveAll sends identical bodies to PATCH config and PUT settings incl. queue fields', async (t) => {
  const h = createHarness();
  await load(h, { settings: settingsBody({ queue_max_wait_ms: 7000, queue_max_waiters: 42 }) });
  const before = h.calls.length;
  h.responses.push(jsonResponse({}), jsonResponse(settingsBody({ queue_max_wait_ms: 7000, queue_max_waiters: 42 })));
  await h.fn('saveAll')();
  const [patch, put] = h.calls.slice(before);
  assert.equal(patch.method, 'PATCH');
  assert.equal(put.method, 'PUT');
  assert.equal(patch.body, put.body);
  const body = JSON.parse(patch.body);
  assert.deepEqual(Object.keys(body).sort(), ['auths', 'default_rpm', 'providers', 'queue_enabled', 'queue_max_wait_ms', 'queue_max_waiters']);
  assert.equal(body.queue_enabled, true);
  assert.equal(body.queue_max_wait_ms, 7000);
  assert.equal(body.queue_max_waiters, 42);
});

test('getJSON rejects a malformed successful response', async (t) => {
  const h = createHarness();
  await load(h);
  h.responses.push(jsonResponse({}), { ok: true, status: 200, text: '<not json' });
  await h.fn('saveAll')();
  assert.match(h.elements.get('status').textContent, /not valid JSON|Save failed/);
  assert.ok(!h.elements.get('status').textContent.includes('survive restart'));
});

test('candidates endpoint failure degrades but keeps the load successful', async (t) => {
  const h = createHarness();
  h.elements.get('key').value = 'k';
  h.responses.push(jsonResponse(settingsBody()), jsonResponse({ files: [{ id: 'a1', provider: 'codex' }] }), new Error('candidates down'));
  await h.fn('loadAll')();
  const state = h.state();
  assert.equal(state.loaded, true);
  assert.equal(state.accounts.length, 1);
  assert.match(h.elements.get('observedStatus').textContent, /Failed to load observed candidates/);
});

test('mandatory load failure keeps prior state and blocks saving', async (t) => {
  const h = createHarness();
  h.elements.get('key').value = 'k';
  h.responses.push(new Error('denied'), jsonResponse({ files: [] }), jsonResponse({ candidates: [] }));
  await h.fn('loadAll')();
  assert.equal(h.state().loaded, false);
  const calls = h.calls.length;
  await h.fn('saveAll')();
  assert.equal(h.calls.length, calls, 'no requests may be sent when nothing was loaded');
  assert.equal(h.elements.get('saveButton').disabled, true);
});

test('saveAll does nothing before the first successful load', async (t) => {
  const h = createHarness();
  h.elements.get('key').value = 'k';
  await h.fn('saveAll')();
  assert.equal(h.calls.length, 0);
});

test('persisted config but failed runtime save reports the partial state', async (t) => {
  const h = createHarness();
  await load(h);
  h.responses.push(jsonResponse({}), { ok: false, status: 500, text: '{"error":"boom"}' });
  await h.fn('saveAll')();
  const status = h.elements.get('status').textContent;
  assert.match(status, /Persisted|已持久化/);
  assert.ok(!status.includes('survive restart'));
});

test('duplicate saves are prevented while one is in flight', async (t) => {
  const h = createHarness();
  await load(h);
  h.responses.push('pending');
  const first = h.fn('saveAll')();
  await h.fn('saveAll')();
  assert.equal(h.calls.filter((c) => c.method === 'PATCH').length, 1);
  void first;
});

test('blank, fractional, negative and excessive values are rejected before any request', async (t) => {
  const h = createHarness();
  await load(h);
  for (const [id, value] of [['queueMaxWaitMS', 'abc'], ['queueMaxWaitMS', '1.5'], ['queueMaxWaitMS', '-1'], ['queueMaxWaitMS', '9223372036855'], ['default', ''], ['queueMaxWaiters', '-2']]) {
    const el = h.elements.get(id);
    const old = el.value;
    el.value = value;
    const calls = h.calls.length;
    await h.fn('saveAll')();
    assert.equal(h.calls.length, calls, id + '=' + value + ' must not issue requests');
    assert.match(h.elements.get('status').textContent, /Validation failed|校验失败/);
    el.value = old;
  }
});

test('empty, {} and array settings responses are rejected on load', async (t) => {
  for (const bad of [{ ok: true, status: 200, text: '' }, jsonResponse({}), jsonResponse([1, 2])]) {
    const h = createHarness();
    h.elements.get('key').value = 'k';
    h.responses.push(bad, jsonResponse({ files: [] }), jsonResponse({ candidates: [], observed_since: 't' }));
    await h.fn('loadAll')();
    assert.equal(h.state().loaded, false);
    assert.match(h.elements.get('status').textContent, /Load failed|读取失败/);
  }
});

test('failed reload blocks saving but preserves rows and drafts', async (t) => {
  const h = createHarness();
  await load(h, { settings: settingsBody({ auths: {} }), files: [{ id: 'a1', provider: 'codex' }] });
  const state = h.state();
  assert.equal(state.loaded, true);
  fireInput(h, 'accountRows', 'auth-limit', 'data-auth-id', 'a1', '9');
  assert.equal(state.auths.a1, '9');
  h.responses.push(new Error('denied'), jsonResponse({ files: [] }), jsonResponse({ candidates: [] }));
  await h.fn('loadAll')();
  assert.equal(state.loaded, false);
  assert.equal(state.auths.a1, '9', 'draft must survive the failed reload');
  assert.equal(state.accounts.length, 1, 'prior rows stay visible');
  const calls = h.calls.length;
  await h.fn('saveAll')();
  assert.equal(h.calls.length, calls, 'save must be blocked after failed reload');
});

test('unprefixed observed provider row resolves the compat fallback', async (t) => {
  const h = createHarness();
  const state = h.state();
  state.accounts = h.fn('mergeAccounts')(
    [],
    [{ id: 'a1', provider: 'deepseek', provider_keys: ['deepseek', 'openai-compatibility'] }],
    {},
  );
  state.providers = { 'openai-compatibility': 9 };
  state.loaded = true;
  h.elements.get('default').value = '3';
  h.fn('render')();
  const stub = latestStub(h, 'data-effective-provider', 'deepseek');
  assert.equal(stub.textContent, '9 RPM · Provider: openai-compatibility');
});

test('PUT returning an invalid settings object reports the partial save', async (t) => {
  const h = createHarness();
  await load(h);
  h.responses.push(jsonResponse({}), jsonResponse({}));
  await h.fn('saveAll')();
  const status = h.elements.get('status').textContent;
  assert.match(status, /Persisted|已持久化/);
  assert.ok(!status.includes('survive restart'));
});

test('orphan auth override stays editable and removable', async (t) => {
  const h = createHarness();
  await load(h, { settings: settingsBody({ auths: { 'ghost-1': 7 } }), files: [], candidates: [] });
  const state = h.state();
  const orphan = state.accounts.find((r) => r.id === 'ghost-1');
  assert.equal(orphan.unmatched, true);
  const html = h.elements.get('accountRows').innerHTML;
  assert.match(html, /Unmatched|未匹配账号/);
  fireInput(h, 'accountRows', 'auth-limit', 'data-auth-id', 'ghost-1', '');
  assert.ok(!Object.hasOwn(state.auths, 'ghost-1'), 'cleared orphan override must be deleted from state');
});
