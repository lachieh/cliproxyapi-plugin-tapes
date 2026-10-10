// Routing UI for lachieh-router. Served as an unauthenticated plugin resource; every read and
// write goes through management routes that require the management key.
'use strict';

const API = '/v0/management/plugins/lachieh-router';
const CONFIG = '/v8/management/config/plugins/configs/lachieh-router';
const KEY_STORAGE = 'lachieh-router.management-key';

const S = {
  key: sessionStorage.getItem(KEY_STORAGE) || '',
  config: null,
  saved: '',
  state: null,
  usage: [],
  errors: [],
  tab: 'tree',
  sim: { input: { model: '', provider: '', client: '', session: '', at: '' }, result: null },
  highlight: null, // { path: Step[], label } from a simulation or a recorded decision
  message: '',
};

// ---------- DOM ----------

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'value') el.value = v;
    else if (k === 'checked') el.checked = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : String(c));
  }
  return el;
}

function render() {
  const app = document.getElementById('app');
  const scroll = window.scrollY;
  app.replaceChildren(S.key && S.config ? renderApp() : renderLogin());
  window.scrollTo(0, scroll);
}

// ---------- API ----------

async function api(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    headers: { Authorization: 'Bearer ' + S.key, 'Content-Type': 'application/json', ...(opts.headers || {}) },
  });
  if (res.status === 401 || res.status === 403) {
    S.key = '';
    sessionStorage.removeItem(KEY_STORAGE);
    S.message = 'The management key was rejected.';
    render();
    throw new Error('unauthorized');
  }
  const text = await res.text();
  const body = text ? JSON.parse(text) : null;
  if (!res.ok) throw new Error((body && (body.error || body.message)) || res.statusText);
  return body;
}

async function load() {
  const [config, st] = await Promise.all([api(CONFIG), api(API + '/state')]);
  S.config = config || {};
  S.saved = JSON.stringify(S.config);
  applyState(st);
  render();
  validate();
}

async function refreshState() {
  applyState(await api(API + '/state'));
  render();
}

function applyState(st) {
  S.state = st.state;
  S.usage = st.usage || [];
}

let validateTimer;
function changed() {
  renderStatus();
  clearTimeout(validateTimer);
  validateTimer = setTimeout(validate, 300);
}

async function validate() {
  const res = await api(API + '/validate', { method: 'POST', body: JSON.stringify(S.config) });
  S.errors = res.errors;
  renderStatus();
  if (S.sim.result) simulate();
}

async function save() {
  await validate();
  if (S.errors.length) return;
  await api(CONFIG, { method: 'PUT', body: JSON.stringify(S.config) });
  S.saved = JSON.stringify(S.config);
  S.message = 'Saved. The plugin reloads with the new rules.';
  render();
  setTimeout(refreshState, 1000);
}

async function simulate() {
  const input = { ...S.sim.input };
  input.at = input.at ? new Date(input.at).toISOString() : undefined;
  const res = await api(API + '/simulate', { method: 'POST', body: JSON.stringify({ config: S.config, input }) });
  if (res.errors) {
    S.sim.result = { result: 'invalid config', path: [], candidates: [] };
  } else {
    S.sim.result = res;
    S.highlight = { path: res.path, label: 'Simulation' };
  }
  render();
}

async function refreshUsage() {
  S.usage = await api(API + '/usage/refresh', { method: 'POST' });
  render();
}

// ---------- shell ----------

function renderLogin() {
  const input = h('input', { type: 'password', placeholder: 'Management key', autocomplete: 'current-password' });
  const submit = (e) => {
    e.preventDefault();
    S.key = input.value.trim();
    if (!S.key) return;
    sessionStorage.setItem(KEY_STORAGE, S.key);
    S.message = '';
    load().catch((err) => { S.message = err.message; render(); });
  };
  return h('form', { class: 'login', onsubmit: submit },
    h('h1', {}, 'Routing'),
    h('p', { class: 'muted' }, 'Enter the management key. It is kept for this browser tab only.'),
    input,
    h('button', { class: 'primary' }, 'Continue'),
    S.message && h('p', { class: 'error' }, S.message));
}

const TABS = [
  ['tree', 'Decision tree'],
  ['accounts', 'Accounts'],
  ['clients', 'Clients'],
  ['activity', 'Activity'],
  ['settings', 'Settings'],
  ['raw', 'Raw'],
];

function renderApp() {
  return h('div', { class: 'shell' },
    // The panel floats its own toolbar over the top-right corner, so controls stay left.
    h('header', { class: 'top' },
      h('div', { class: 'title' }, h('h1', {}, 'Routing'), h('div', { id: 'status', class: 'status' }, statusChildren())),
      h('nav', { class: 'tabs' }, TABS.map(([id, label]) =>
        h('button', { class: S.tab === id ? 'tab on' : 'tab', onclick: () => { S.tab = id; render(); } }, label)))),
    S.message && h('div', { class: 'banner' }, S.message,
      h('button', { class: 'link', onclick: () => { S.message = ''; render(); } }, 'Dismiss')),
    h('main', {}, {
      tree: renderTreeTab, accounts: renderAccounts, clients: renderClients,
      activity: renderActivity, settings: renderSettings, raw: renderRaw,
    }[S.tab]()));
}

function dirty() { return JSON.stringify(S.config) !== S.saved; }

function statusChildren() {
  const errs = S.errors.length;
  return [
    errs ? h('details', { class: 'errors' },
      h('summary', {}, `${errs} problem${errs > 1 ? 's' : ''}`),
      h('ul', {}, S.errors.map((e) => h('li', {}, e))))
      : h('span', { class: 'muted' }, dirty() ? 'Unsaved changes' : 'Saved'),
    dirty() && h('button', { onclick: () => { S.config = JSON.parse(S.saved); render(); validate(); } }, 'Revert'),
    h('button', { class: 'primary', disabled: !dirty() || errs > 0, onclick: save }, 'Save'),
  ];
}

function renderStatus() {
  const el = document.getElementById('status');
  if (el) el.replaceChildren(...statusChildren().filter(Boolean));
}

// ---------- shared field helpers ----------

function text(obj, key, placeholder, attrs = {}) {
  return h('input', {
    value: obj[key] ?? '', placeholder, ...attrs,
    oninput: (e) => { const v = e.target.value; if (v === '') delete obj[key]; else obj[key] = v; changed(); },
  });
}

function number(obj, key, attrs = {}) {
  return h('input', {
    type: 'number', value: obj[key] ?? '', ...attrs,
    oninput: (e) => { const v = e.target.value; if (v === '') delete obj[key]; else obj[key] = Number(v); changed(); },
  });
}

function select(obj, key, options, fallback) {
  return h('select', {
    onchange: (e) => { const v = e.target.value; if (v === fallback) delete obj[key]; else obj[key] = v; changed(); render(); },
  }, options.map(([v, label]) => h('option', { value: v, selected: (obj[key] ?? fallback) === v }, label)));
}

function seenAccountIds() {
  const ids = new Set(S.usage.map((a) => a.id));
  for (const a of (S.state?.accounts || [])) ids.add(a.id);
  return [...ids].sort();
}

function seenProviders() {
  const p = new Set(S.usage.map((a) => a.provider));
  for (const a of (S.state?.accounts || [])) p.add(a.provider);
  return [...p].sort();
}

function clientNames() { return Object.keys(S.config.clients || {}).sort(); }

function datalists() {
  return [
    h('datalist', { id: 'dl-accounts' }, seenAccountIds().map((id) => h('option', { value: id }))),
    h('datalist', { id: 'dl-providers' }, seenProviders().map((p) => h('option', { value: p }))),
    h('datalist', { id: 'dl-clients' }, clientNames().map((n) => h('option', { value: n }))),
  ];
}

// ---------- conditions ----------

const COND_FIELDS = {
  model: { label: 'model', hint: 'claude-opus-*' },
  provider: { label: 'provider', hint: 'claude', list: 'dl-providers' },
  client: { label: 'client', hint: 'work-laptop', list: 'dl-clients' },
  session: { label: 'session', hint: 'header:*' },
  hours: { label: 'between', hint: '09:00-17:30' },
  days: { label: 'on days' },
  headers: { label: 'header' },
};
const DAYS = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'];

function renderCond(cond) {
  const rows = [];
  for (const [key, meta] of Object.entries(COND_FIELDS)) {
    if (cond[key] === undefined) continue;
    const remove = h('button', { class: 'x', title: 'Remove', onclick: () => { delete cond[key]; changed(); render(); } }, '×');
    if (key === 'days') {
      rows.push(h('div', { class: 'pill' }, h('span', { class: 'pk' }, meta.label),
        h('span', { class: 'days' }, DAYS.map((d) => h('button', {
          class: cond.days.includes(d) ? 'day on' : 'day',
          onclick: () => {
            cond.days = cond.days.includes(d) ? cond.days.filter((x) => x !== d) : DAYS.filter((x) => x === d || cond.days.includes(x));
            changed(); render();
          },
        }, d))), remove));
    } else if (key === 'headers') {
      for (const name of Object.keys(cond.headers)) {
        rows.push(h('div', { class: 'pill' }, h('span', { class: 'pk' }, 'header'),
          h('input', { class: 'short', value: name, onchange: (e) => {
            const v = cond.headers[name]; delete cond.headers[name]; cond.headers[e.target.value] = v; changed(); render();
          } }),
          h('span', { class: 'pk' }, 'matches'),
          text(cond.headers, name, 'value glob'),
          h('button', { class: 'x', onclick: () => {
            delete cond.headers[name]; if (!Object.keys(cond.headers).length) delete cond.headers; changed(); render();
          } }, '×')));
      }
    } else {
      rows.push(h('div', { class: 'pill' }, h('span', { class: 'pk' }, meta.label),
        text(cond, key, meta.hint, { list: meta.list }), remove));
    }
  }
  const add = h('select', {
    class: 'add',
    onchange: (e) => {
      const k = e.target.value;
      if (k === 'days') cond.days = ['mon', 'tue', 'wed', 'thu', 'fri'];
      else if (k === 'headers') cond.headers = { ...(cond.headers || {}), 'User-Agent': '' };
      else cond[k] = '';
      changed(); render();
    },
  }, h('option', { value: '' }, '+ condition'),
  Object.entries(COND_FIELDS).filter(([k]) => k === 'headers' || cond[k] === undefined)
    .map(([k, m]) => h('option', { value: k }, m.label)));
  return h('div', { class: 'conds' }, rows.length ? rows : h('span', { class: 'muted' }, 'always'), add);
}

// ---------- tree ----------

const KINDS = {
  if: { label: 'If', make: (name) => ({ name, if: {} }) },
  pick: { label: 'Pick account', make: (name) => ({ name, pick: { accounts: [] } }) },
  delegate: { label: 'Delegate', make: (name) => ({ name, delegate: 'round-robin' }) },
  reject: { label: 'Reject', make: (name) => ({ name, reject: 'No account is available for this request.' }) },
  builtin: { label: 'Host default', make: (name) => ({ name, builtin: true }) },
};

function kindOf(n) {
  if (n.if) return 'if';
  if (n.pick) return 'pick';
  if (n.delegate) return 'delegate';
  if (n.reject) return 'reject';
  return 'builtin';
}

function stepFor(id) { return S.highlight?.path.find((s) => s.node === id); }

function renderTreeTab() {
  return h('div', {},
    datalists(),
    renderSimulator(),
    h('section', { class: 'tree' },
      h('p', { class: 'muted intro' },
        'Runs for every request after model routing. The host only offers accounts that are currently usable; ',
        'pick nodes then filter them by name and cached usage, and fall through to "else" when none qualify.'),
      renderSlot(S.config, 'tree', 'tree')));
}

// A slot is a place a node can live: the root, an if's then/else, or a pick's else.
function renderSlot(parent, key, id) {
  const n = parent[key];
  if (!n) {
    const step = stepFor(id);
    return h('div', { class: step ? 'empty on-path' : 'empty' },
      h('span', { class: 'muted' }, key === 'tree' ? 'No tree: the host picks accounts.' : 'Host default'),
      step && h('span', { class: 'outcome' }, step.outcome),
      h('select', {
        class: 'add', onchange: (e) => { parent[key] = KINDS[e.target.value].make(''); changed(); render(); },
      }, h('option', { value: '' }, '+ add node'),
      Object.entries(KINDS).map(([k, v]) => h('option', { value: k }, v.label))));
  }
  return renderNode(parent, key, id);
}

function renderNode(parent, key, id) {
  const n = parent[key];
  const kind = kindOf(n);
  const step = stepFor(id);
  const cls = ['node', 'k-' + kind, step ? 'on-path' : (S.highlight ? 'off-path' : '')].join(' ');
  return h('div', { class: cls },
    h('div', { class: 'node-head' },
      h('select', {
        class: 'kind', onchange: (e) => { parent[key] = KINDS[e.target.value].make(n.name || ''); changed(); render(); },
      }, Object.entries(KINDS).map(([k, v]) => h('option', { value: k, selected: k === kind }, v.label))),
      h('input', { class: 'name', value: n.name || '', placeholder: 'name this step', oninput: (e) => {
        if (e.target.value) n.name = e.target.value; else delete n.name; changed();
      } }),
      step && h('span', { class: 'outcome' }, step.outcome),
      h('button', { class: 'x', title: 'Remove this node and everything under it', onclick: () => { delete parent[key]; changed(); render(); } }, '×')),
    h('div', { class: 'node-body' }, {
      if: () => renderCond(n.if),
      pick: () => renderPick(n),
      delegate: () => h('div', { class: 'conds' }, h('span', { class: 'pk' }, 'use the host’s'),
        select(n, 'delegate', [['round-robin', 'round-robin'], ['fill-first', 'fill-first']], null), h('span', { class: 'pk' }, 'scheduler')),
      reject: () => h('div', { class: 'conds' }, h('span', { class: 'pk' }, 'fail with'), text(n, 'reject', 'message', { class: 'wide' })),
      builtin: () => h('p', { class: 'muted' }, 'Leave the choice to the host’s configured routing strategy.'),
    }[kind]()),
    kind === 'if' && h('div', { class: 'branches' },
      branch(n, 'then', id + '.then', 'match', step?.outcome === 'true'),
      branch(n, 'else', id + '.else', 'otherwise', step?.outcome === 'false')),
    kind === 'pick' && h('div', { class: 'branches' },
      branch(n, 'else', id + '.else', 'no account qualified', step?.outcome === 'no account qualified')));
}

function branch(n, key, id, label, taken) {
  return h('div', { class: taken ? 'branch taken' : 'branch' },
    h('div', { class: 'branch-label' }, label),
    renderSlot(n, key, id));
}

const WHERE_FIELDS = [
  ['weekly', 'used_below', 'weekly used below', '%'],
  ['weekly', 'used_at_least', 'weekly used at least', '%'],
  ['weekly', 'resets_within', 'weekly resets within', '24h'],
  ['five_hour', 'used_below', '5-hour used below', '%'],
  ['five_hour', 'used_at_least', '5-hour used at least', '%'],
  ['five_hour', 'resets_within', '5-hour resets within', '2h'],
];

function renderPick(n) {
  const p = n.pick;
  const accounts = p.accounts || [];
  const where = p.where || {};
  const accountRows = accounts.map((g, i) => h('span', { class: 'chip' },
    h('span', { class: 'rank' }, i + 1),
    h('input', { value: g, list: 'dl-accounts', oninput: (e) => { p.accounts[i] = e.target.value; changed(); } }),
    i > 0 && h('button', { class: 'x', title: 'Prefer this earlier', onclick: () => { [p.accounts[i - 1], p.accounts[i]] = [p.accounts[i], p.accounts[i - 1]]; changed(); render(); } }, '↑'),
    h('button', { class: 'x', onclick: () => { p.accounts.splice(i, 1); changed(); render(); } }, '×')));

  const whereRows = [];
  for (const [win, field, label, unit] of WHERE_FIELDS) {
    if (where[win]?.[field] === undefined) continue;
    const input = unit === '%' ? number(where[win], field, { min: 0, max: 100, class: 'num' }) : text(where[win], field, unit, { class: 'short' });
    whereRows.push(h('div', { class: 'pill' }, h('span', { class: 'pk' }, label), input, unit === '%' && h('span', { class: 'pk' }, '%'),
      h('button', { class: 'x', onclick: () => {
        delete where[win][field]; if (!Object.keys(where[win]).length) delete where[win]; tidyWhere(p); changed(); render();
      } }, '×')));
  }
  if (where.credits) {
    whereRows.push(h('div', { class: 'pill' }, h('span', { class: 'pk' }, 'paid credit available'),
      h('button', { class: 'x', onclick: () => { delete where.credits; tidyWhere(p); changed(); render(); } }, '×')));
  }
  const addWhere = h('select', {
    class: 'add',
    onchange: (e) => {
      const v = e.target.value;
      p.where = p.where || {};
      if (v === 'credits') p.where.credits = true;
      else {
        const [win, field] = v.split('.');
        p.where[win] = p.where[win] || {};
        p.where[win][field] = field === 'resets_within' ? '24h' : 80;
      }
      changed(); render();
    },
  }, h('option', { value: '' }, '+ usage condition'),
  WHERE_FIELDS.filter(([w, f]) => where[w]?.[f] === undefined).map(([w, f, label]) => h('option', { value: w + '.' + f }, label)),
  !where.credits && h('option', { value: 'credits' }, 'paid credit available'));

  return h('div', { class: 'pick' },
    h('div', { class: 'row' }, h('span', { class: 'pk' }, 'from'),
      accountRows.length ? accountRows : h('span', { class: 'muted' }, 'any offered account'),
      h('button', { class: 'add', onclick: () => { p.accounts = [...accounts, '']; changed(); render(); } }, '+ account')),
    h('div', { class: 'row' }, h('span', { class: 'pk' }, 'where'),
      whereRows.length ? whereRows : h('span', { class: 'muted' }, 'no usage condition'), addWhere),
    h('div', { class: 'row' }, h('span', { class: 'pk' }, 'prefer'),
      select(p, 'order', [['listed', 'the order listed'], ['soonest_reset', 'the soonest reset'], ['least_used', 'the least used']], 'listed'),
      p.order && p.order !== 'listed' && select(p, 'window', [['weekly', 'weekly window'], ['five_hour', '5-hour window']], 'weekly'),
      h('label', { class: 'toggle' }, h('input', { type: 'checkbox', checked: !!n.sticky, onchange: (e) => {
        if (e.target.checked) n.sticky = true; else delete n.sticky; changed();
      } }), 'keep a session on its account while it still qualifies')));
}

function tidyWhere(p) {
  if (p.where && !Object.keys(p.where).length) delete p.where;
}

// ---------- simulator ----------

function renderSimulator() {
  const inp = S.sim.input;
  const r = S.sim.result;
  return h('section', { class: 'sim' },
    h('div', { class: 'sim-row' },
      h('strong', {}, 'Try a request'),
      text(inp, 'model', 'model', { class: 'short' }),
      h('input', { class: 'short', placeholder: 'provider', list: 'dl-providers', value: inp.provider, oninput: (e) => { inp.provider = e.target.value; } }),
      h('input', { class: 'short', placeholder: 'client', list: 'dl-clients', value: inp.client, oninput: (e) => { inp.client = e.target.value; } }),
      text(inp, 'session', 'session', { class: 'short' }),
      h('input', { type: 'datetime-local', value: inp.at, title: 'Time (defaults to now)', oninput: (e) => { inp.at = e.target.value; } }),
      h('button', { class: 'primary', onclick: simulate }, 'Simulate'),
      S.highlight && h('button', { class: 'link', onclick: () => { S.highlight = null; S.sim.result = null; render(); } }, 'Clear')),
    S.highlight && h('div', { class: 'sim-result' },
      h('span', { class: 'muted' }, S.highlight.label + ' →'),
      h('strong', {}, S.highlight.result ?? r?.result),
      r && S.highlight.label === 'Simulation' && h('span', { class: 'muted' }, `from ${r.candidates.length} candidate${r.candidates.length === 1 ? '' : 's'}`)));
}

// ---------- accounts ----------

function relative(iso) {
  if (!iso) return '';
  const ms = new Date(iso) - Date.now();
  const abs = Math.abs(ms);
  if (abs < 6e4) return 'just now';
  const fmt = abs < 3.6e6 ? Math.round(abs / 6e4) + 'm' : abs < 8.64e7 ? (abs / 3.6e6).toFixed(1) + 'h' : (abs / 8.64e7).toFixed(1) + 'd';
  return ms >= 0 ? 'in ' + fmt : fmt + ' ago';
}

function bar(w) {
  if (!w) return h('span', { class: 'muted' }, '—');
  const fill = h('span', { class: 'fill' });
  fill.style.width = Math.min(100, w.used_percent) + '%';
  if (w.used_percent >= 90) fill.classList.add('hot');
  else if (w.used_percent >= 70) fill.classList.add('warm');
  return h('div', { class: 'usage' }, h('div', { class: 'bar' }, fill),
    h('span', {}, Math.round(w.used_percent) + '%'),
    w.resets_at && h('span', { class: 'muted' }, 'resets ' + relative(w.resets_at)));
}

function renderAccounts() {
  const polled = new Map(S.usage.map((a) => [a.id, a]));
  const rows = seenAccountIds().map((id) => {
    const a = polled.get(id);
    const seen = (S.state?.accounts || []).find((x) => x.id === id);
    return h('tr', {},
      h('td', {}, h('code', {}, id), a?.label && h('div', { class: 'muted' }, a.label)),
      h('td', {}, a?.provider || seen?.provider || ''),
      h('td', {}, a ? bar(a.usage?.five_hour) : h('span', { class: 'muted' }, 'not polled')),
      h('td', {}, a ? bar(a.usage?.weekly) : ''),
      h('td', {}, a?.usage?.credits ? h('span', { class: a.usage.credits.available ? 'ok' : 'muted' }, a.usage.credits.detail) : ''),
      h('td', { class: 'muted' }, a?.error ? h('span', { class: 'error' }, a.error) : a?.fetched_at ? relative(a.fetched_at) : ''));
  });
  return h('section', {},
    h('div', { class: 'section-head' },
      h('p', { class: 'muted' }, 'Claude and Codex OAuth accounts are polled for usage every ',
        S.config.usage?.interval || '5m', '. Other accounts appear once a request offers them and have no usage, so usage conditions skip them.'),
      h('button', { onclick: refreshUsage }, 'Refresh usage now')),
    rows.length ? h('table', {},
      h('thead', {}, h('tr', {}, ['Account', 'Provider', '5-hour', 'Weekly', 'Paid credit', 'Updated'].map((t) => h('th', {}, t)))),
      h('tbody', {}, rows)) : h('p', { class: 'muted' }, 'No accounts seen yet.'));
}

// ---------- clients ----------

function renderClients() {
  const byHash = new Map(Object.entries(S.config.clients || {}).map(([name, sha]) => [sha, name]));
  const seen = S.state?.clients || [];
  const seenHashes = new Set(seen.map((c) => c.sha256));
  const rename = (sha, name) => {
    S.config.clients = S.config.clients || {};
    for (const [n, s] of Object.entries(S.config.clients)) if (s === sha) delete S.config.clients[n];
    if (name) S.config.clients[name] = sha;
    if (!Object.keys(S.config.clients).length) delete S.config.clients;
    changed();
  };
  const row = (sha, hint, lastSeen) => h('tr', {},
    h('td', {}, h('code', {}, hint || sha.slice(0, 12) + '…')),
    h('td', {}, h('input', { value: byHash.get(sha) || '', placeholder: 'name, e.g. work-laptop', onchange: (e) => rename(sha, e.target.value.trim()) })),
    h('td', { class: 'muted' }, lastSeen ? relative(lastSeen) : 'not seen since restart'));
  return h('section', {},
    h('p', { class: 'muted' }, 'Client API keys that have made requests. Name one to use it in "client" conditions. ',
      'Only the SHA-256 of the key is saved in the config.'),
    h('table', {},
      h('thead', {}, h('tr', {}, h('th', {}, 'Key'), h('th', {}, 'Name'), h('th', {}, 'Last request'))),
      h('tbody', {},
        seen.map((c) => row(c.sha256, c.hint, c.last_seen)),
        [...byHash.keys()].filter((sha) => !seenHashes.has(sha)).map((sha) => row(sha, '', null)))),
    !seen.length && h('p', { class: 'muted' }, 'No client keys seen yet. Send a request through the proxy and refresh.'),
    h('button', { onclick: refreshState }, 'Refresh'));
}

// ---------- activity ----------

function renderActivity() {
  const recent = S.state?.recent || [];
  return h('section', {},
    h('div', { class: 'section-head' },
      h('p', { class: 'muted' }, 'The last 50 decisions since the host started. Pick a decision to see its path in the tree.'),
      h('button', { onclick: refreshState }, 'Refresh')),
    recent.length ? h('table', {},
      h('thead', {}, h('tr', {}, ['When', 'Request', 'Result', 'Path'].map((t) => h('th', {}, t)))),
      h('tbody', {}, recent.map((d) => h('tr', {},
        h('td', { class: 'muted' }, relative(d.at)),
        h('td', {}, h('code', {}, d.model), d.client && h('div', {}, 'client ', d.client), d.session && h('div', { class: 'muted' }, d.session)),
        h('td', {}, d.hook === 'route' ? 'route → ' : '', h('code', {}, d.result)),
        h('td', {}, d.path ? h('button', { class: 'link', onclick: () => {
          S.highlight = { path: d.path, label: 'Recorded decision', result: d.result };
          S.tab = 'tree'; render();
        } }, d.path.map((s) => s.name).join(' → ')) : d.rule)))))
      : h('p', { class: 'muted' }, 'No decisions yet.'));
}

// ---------- settings ----------

function renderSettings() {
  const c = S.config;
  const routes = c.routes || [];
  const field = (label, input, help) => h('label', { class: 'field' }, h('span', {}, label), input, help && h('small', { class: 'muted' }, help));
  return h('section', { class: 'settings' },
    datalists(),
    h('h2', {}, 'General'),
    field('Timezone', text(c, 'timezone', Intl.DateTimeFormat().resolvedOptions().timeZone), 'For "between" and "on days" conditions. Defaults to the host’s zone.'),
    field('Session pin lifetime', text(c, 'sticky_ttl', '6h'), 'How long an idle session stays on its account.'),
    field('Session headers', h('input', {
      value: (c.session_headers || []).join(', '), placeholder: 'X-Claude-Code-Session-Id',
      oninput: (e) => { const v = e.target.value.split(',').map((s) => s.trim()).filter(Boolean); if (v.length) c.session_headers = v; else delete c.session_headers; changed(); },
    }), 'Checked after the host’s own session ID.'),
    field('Usage poll interval', h('input', {
      value: c.usage?.interval || '', placeholder: '5m',
      oninput: (e) => {
        const v = e.target.value.trim();
        if (v) c.usage = { ...(c.usage || {}), interval: v };
        else if (c.usage) { delete c.usage.interval; if (!Object.keys(c.usage).length) delete c.usage; }
        changed();
      },
    }), 'At least 1m.'),
    h('h2', {}, 'Model routes'),
    h('p', { class: 'muted' }, 'Run before the tree: send matching requests to another provider and optionally rename the model. First match wins; a rule is skipped when its provider has no account.'),
    routes.map((r, i) => h('div', { class: 'node k-route' },
      h('div', { class: 'node-head' },
        h('input', { class: 'name', value: r.name || '', placeholder: 'rule name', oninput: (e) => { r.name = e.target.value; changed(); } }),
        h('button', { class: 'x', onclick: () => { routes.splice(i, 1); if (!routes.length) delete c.routes; changed(); render(); } }, '×')),
      h('div', { class: 'node-body' },
        renderCond(r.match = r.match || {}),
        h('div', { class: 'conds' }, h('span', { class: 'pk' }, 'send to'),
          text(r, 'provider', 'provider', { list: 'dl-providers', class: 'short' }),
          h('span', { class: 'pk' }, 'as model'), text(r, 'model', 'keep requested model'))))),
    h('button', { class: 'add', onclick: () => { c.routes = routes; routes.push({ name: '', match: {}, provider: '' }); changed(); render(); } }, '+ model route'));
}

// ---------- raw ----------

function renderRaw() {
  const area = h('textarea', { spellcheck: 'false', value: JSON.stringify(S.config, null, 2) });
  const err = h('p', { class: 'error' });
  return h('section', {},
    h('p', { class: 'muted' }, 'The plugin config as saved under plugins.configs.lachieh-router.'),
    area, err,
    h('button', { onclick: () => {
      try { S.config = JSON.parse(area.value); err.textContent = ''; changed(); S.tab = 'tree'; render(); }
      catch (e) { err.textContent = e.message; }
    } }, 'Apply'));
}

// ---------- boot ----------

if (S.key) load().catch((err) => { S.message = err.message; render(); });
else render();
