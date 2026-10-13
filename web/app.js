'use strict';

// All dynamic content is inserted as text nodes via h(); webhook payloads are
// untrusted, so nothing in this file assigns innerHTML.

const state = { types: [], channels: [], integrations: [], settings: {}, update: {}, containerUpdates: [], version: '' };
const app = document.getElementById('app');
const modalRoot = document.getElementById('modal-root');

// ---------- helpers ----------

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'value') el.value = v;
    else if (k === 'checked' || k === 'disabled' || k === 'selected' || k === 'readOnly') el[k] = !!v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

async function api(method, path, body) {
  const res = await fetch('/api' + path, {
    method,
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-Notified': '1' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  let data = null;
  try { data = await res.json(); } catch (e) { /* empty body */ }
  if (res.status === 401 && !path.startsWith('/login')) {
    boot();
    throw new Error('Signed out');
  }
  if (!res.ok) throw new Error((data && data.error) || ('Request failed (' + res.status + ')'));
  return data;
}

function toast(msg, isErr) {
  const t = h('div', { class: 'toast' + (isErr ? ' err' : '') }, msg);
  document.getElementById('toasts').append(t);
  setTimeout(() => t.remove(), isErr ? 7000 : 3500);
}
const fail = (e) => toast(e.message || String(e), true);

function ago(unix) {
  if (!unix) return 'never';
  const s = Math.max(0, Math.floor(Date.now() / 1000 - unix));
  if (s < 60) return 'just now';
  if (s < 3600) return Math.floor(s / 60) + 'm ago';
  if (s < 86400) return Math.floor(s / 3600) + 'h ago';
  return Math.floor(s / 86400) + 'd ago';
}
const when = (unix) => new Date(unix * 1000).toLocaleString();
const typeDef = (t) => state.types.find((x) => x.type === t);
const channelName = (id) => (state.channels.find((c) => c.id === id) || {}).name;
const baseURL = () => (state.settings.base_url || location.origin).replace(/\/+$/, '');
const hookURL = (token) => baseURL() + '/api/v1/hook/' + token;

// tile shows the integration's icon from web/icons/<type>.svg, falling back
// to its initials if a type has no icon file.
function tile(type) {
  const def = typeDef(type);
  // Artwork with black lettering needs a light plate to stay readable in the dark theme.
  const t = h('div', { class: 'tile' + (type === 'nzbhydra2' ? ' plate' : '') });
  const img = h('img', { src: 'icons/' + encodeURIComponent(type) + '.svg', alt: '', loading: 'lazy' });
  img.addEventListener('error', () => {
    t.classList.add('letters');
    t.replaceChildren((def ? def.label : type).slice(0, 2));
  });
  t.append(img);
  return t;
}

function field(label, input, help) {
  return h('label', { class: 'field' }, h('span', {}, label), input, help ? h('small', {}, help) : null);
}

function modal(title, body, footer, wide) {
  const close = () => {
    overlay.remove();
    document.removeEventListener('keydown', onKey);
    document.body.classList.toggle('modal-open', modalRoot.children.length > 0);
  };
  const onKey = (e) => { if (e.key === 'Escape') close(); };
  const overlay = h('div', { class: 'overlay', onmousedown: (e) => { if (e.target === overlay) close(); } },
    h('div', { class: 'modal' + (wide ? ' wide' : '') },
      h('header', {}, h('h2', {}, title), h('button', { class: 'x', onclick: close, 'aria-label': 'Close' }, '×')),
      h('div', { class: 'body' }, body),
      footer ? h('footer', {}, footer(close)) : null));
  document.addEventListener('keydown', onKey);
  modalRoot.append(overlay);
  document.body.classList.add('modal-open');
  return close;
}

async function busy(btn, fn) {
  btn.disabled = true;
  try { await fn(); } catch (e) { fail(e); } finally { btn.disabled = false; }
}

// ---------- boot / auth ----------

async function boot() {
  modalRoot.replaceChildren();
  document.body.classList.remove('modal-open');
  let st;
  try {
    st = await (await fetch('/api/status', { credentials: 'same-origin' })).json();
  } catch (e) {
    app.replaceChildren(h('div', { class: 'auth' }, h('div', { class: 'card' }, 'Cannot reach the Notified server.')));
    return;
  }
  state.version = st.version;
  document.title = st.instance_name || 'Notified';
  if (st.setup_required) return authScreen(true);
  if (!st.authenticated) return authScreen(false);
  try {
    await loadAll();
  } catch (e) { return fail(e); }
  renderShell();
  route();
}

function authScreen(setup) {
  const user = h('input', { type: 'text', autocomplete: 'username', required: true });
  const pass = h('input', { type: 'password', autocomplete: setup ? 'new-password' : 'current-password', required: true });
  const err = h('div', { class: 'error' });
  const btn = h('button', { class: 'btn primary', type: 'submit' }, setup ? 'Create account' : 'Sign in');
  const form = h('form', {
    onsubmit: async (e) => {
      e.preventDefault();
      err.textContent = '';
      btn.disabled = true;
      try {
        await api('POST', setup ? '/setup' : '/login', { username: user.value, password: pass.value });
        boot();
      } catch (ex) {
        err.textContent = ex.message;
        btn.disabled = false;
      }
    },
  },
    field('Username', user),
    field('Password', pass, setup ? 'At least 8 characters.' : null),
    err, btn);
  app.replaceChildren(h('div', { class: 'auth' }, h('div', { class: 'card' },
    h('div', { class: 'brand' }, h('img', { src: 'favicon.svg', alt: '' }), 'Notified'),
    h('p', {}, setup ? 'Welcome. Create the admin account to get started.' : 'Sign in to continue.'),
    form)));
  user.focus();
}

async function loadAll() {
  const [types, channels, integrations, settings, update, containerUpdates] = await Promise.all([
    api('GET', '/types'), api('GET', '/channels'), api('GET', '/integrations'), api('GET', '/settings'), api('GET', '/update'),
    api('GET', '/container-updates'),
  ]);
  Object.assign(state, { types, channels, integrations, settings, update, containerUpdates });
}

// ---------- shell / routing ----------

const pages = {
  dashboard: { label: 'Dashboard', render: dashboardPage },
  integrations: { label: 'Integrations', render: integrationsPage },
  channels: { label: 'Channels', render: channelsPage },
  updates: { label: 'Container updates', render: containerUpdatesPage },
  history: { label: 'History', render: historyPage },
  settings: { label: 'Settings', render: settingsPage },
};
let mainEl, navEl, refreshTimer;

function setMain(...kids) {
  mainEl.replaceChildren(...kids.flat(Infinity).filter((k) => k != null && k !== false));
}

function renderShell() {
  navEl = h('nav', { class: 'nav' }, Object.entries(pages).map(([k, p]) => h('a', { href: '#' + k, 'data-page': k }, p.label)));
  refreshNavCount();
  mainEl = h('main', { class: 'main' });
  app.replaceChildren(h('div', { class: 'shell' },
    h('aside', { class: 'sidebar' },
      h('div', { class: 'brand' }, h('img', { src: 'favicon.svg', alt: '' }), state.settings.instance_name || 'Notified'),
      navEl,
      h('div', { class: 'spacer' }),
      state.update.update_available ? h('a', {
        class: 'update-pill', href: state.update.url || '#settings', target: state.update.url ? '_blank' : null, rel: 'noopener noreferrer',
        title: 'Installed: ' + verLabel(state.update.current),
      }, 'Update available: ' + verLabel(state.update.latest)) : null,
      h('div', { class: 'foot' }, h('span', {}, verLabel(state.version)), '·', themeButton(), '·',
        h('button', { class: 'linkbtn', onclick: async () => { try { await api('POST', '/logout'); } catch (e) { /* already signed out */ } boot(); } }, 'Sign out'))),
    mainEl));
}

// Shows how many container updates are waiting next to the nav item.
function refreshNavCount() {
  if (!navEl) return;
  const link = [...navEl.children].find((a) => a.dataset.page === 'updates');
  const n = state.containerUpdates.reduce((sum, g) => sum + g.items.length, 0);
  link.replaceChildren(pages.updates.label, n ? h('span', { class: 'count' }, n) : null);
}

function route() {
  if (!mainEl) return;
  clearInterval(refreshTimer);
  const name = pages[location.hash.slice(1)] ? location.hash.slice(1) : 'dashboard';
  for (const a of navEl.children) a.classList.toggle('active', a.dataset.page === name);
  mainEl.replaceChildren();
  pages[name].render().catch(fail);
}
window.addEventListener('hashchange', route);

// Release versions get a "v" prefix; development builds are shown as they are.
function verLabel(v) {
  return /^\d/.test(v || '') ? 'v' + v : (v || 'unknown');
}

const themeNames = { system: 'Auto', dark: 'Dark', light: 'Light' };

function themeButton() {
  const order = ['system', 'dark', 'light'];
  const btn = h('button', {
    class: 'linkbtn', title: 'Switch between automatic, dark and light theme',
    onclick: () => {
      const next = order[(order.indexOf(NotifiedTheme.get()) + 1) % order.length];
      NotifiedTheme.set(next);
      btn.textContent = themeNames[next] + ' theme';
    },
  }, themeNames[NotifiedTheme.get()] + ' theme');
  return btn;
}

function pageHead(title, sub, ...actions) {
  return h('div', { class: 'page-head' },
    h('div', {}, h('h1', {}, title), sub ? h('p', {}, sub) : null),
    h('div', { class: 'row' }, actions));
}

// ---------- dashboard ----------

async function dashboardPage() {
  const draw = async () => {
    const [d, recent, integrations] = await Promise.all([
      api('GET', '/dashboard'), api('GET', '/notifications?limit=8'), api('GET', '/integrations'),
    ]);
    state.integrations = integrations;
    const stat = (num, lbl, bad) => h('div', { class: 'card stat' + (bad ? ' bad' : '') }, h('div', { class: 'num' }, num), h('div', { class: 'lbl' }, lbl));
    const max = Math.max(1, ...d.days.map((x) => x.sent + x.failed));
    const chart = h('div', { class: 'chart' }, d.days.map((x) => {
      const sent = h('div', { class: 'bar' });
      const failed = h('div', { class: 'bar fail' });
      sent.style.height = (x.sent / max * 100) + '%';
      failed.style.height = (x.failed / max * 100) + '%';
      const date = new Date(x.date + 'T00:00:00');
      return h('div', { class: 'col', title: x.sent + ' sent, ' + x.failed + ' failed, ' + x.skipped + ' skipped' },
        h('div', { class: 'n' }, x.sent + x.failed),
        h('div', { class: 'bars' }, sent, failed),
        h('div', { class: 'd' }, date.toLocaleDateString(undefined, { weekday: 'short' })));
    }));
    const problems = integrations.filter((i) => i.enabled && i.status && i.status.last_error);

    const view = [
      pageHead('Dashboard', 'What has been happening across your apps.'),
      (state.channels.length === 0 || integrations.length === 0) ? h('div', { class: 'card', }, h('h2', {}, 'Getting started'),
        h('div', {}, '1. Add a Discord channel under ', h('a', { href: '#channels' }, 'Channels'), ' (paste a channel webhook URL).'),
        h('div', {}, '2. Add your apps under ', h('a', { href: '#integrations' }, 'Integrations'), ' and copy each webhook URL into the app.')) : null,
      h('div', { class: 'grid stats' },
        stat(d.sent_24h, 'Sent in the last 24h'),
        stat(d.failed_24h, 'Failed in the last 24h', d.failed_24h > 0),
        stat(d.integrations_active + ' / ' + d.integrations, 'Integrations enabled'),
        stat(d.channels, 'Discord channels')),
      h('div', { class: 'grid two' },
        h('div', { class: 'card' }, h('h2', {}, 'Last 7 days'), chart),
        h('div', { class: 'card' }, h('h2', {}, 'Integration health'),
          problems.length === 0
            ? h('div', { class: 'dim' }, integrations.length ? 'All polling integrations are reporting normally.' : 'No integrations yet.')
            : problems.map((i) => h('div', { class: 'row' }, h('span', { class: 'badge err' }, i.name), h('span', { class: 'grow dim' }, i.status.last_error))))),
      h('h3', {}, 'Recent notifications'),
      h('div', { class: 'card table-wrap' }, notificationTable(recent.items, draw)),
    ];
    setMain(view);
  };
  await draw();
  refreshTimer = setInterval(() => { if (!modalRoot.children.length) draw().catch(() => {}); }, 15000);
}

// ---------- notifications ----------

function notificationTable(items, onChange) {
  if (!items.length) return h('div', { class: 'empty' }, 'No notifications yet.');
  return h('table', {},
    h('thead', {}, h('tr', {}, ['Time', 'Integration', 'Event', 'Title', 'Status'].map((c) => h('th', {}, c)))),
    h('tbody', {}, items.map((n) => h('tr', { class: 'click', onclick: () => notificationModal(n.id, onChange).catch(fail) },
      h('td', { class: 'dim nowrap', title: when(n.created_at) }, ago(n.created_at)),
      h('td', {}, n.integration_name),
      h('td', { class: 'dim' }, n.event),
      h('td', {}, n.title || '—'),
      h('td', {}, h('span', { class: 'badge ' + n.status, title: n.error || '' }, n.status))))));
}

function embedPreview(e) {
  const box = h('div', { class: 'embed' });
  if (e.color) box.style.borderLeftColor = '#' + e.color.toString(16).padStart(6, '0');
  const strip = (s) => (s || '').replace(/\*\*/g, '');
  box.append(h('div', { class: 'grow' },
    e.author && e.author.name ? h('div', { class: 'e-author' }, e.author.name) : null,
    e.title ? h('div', { class: 'e-title' }, e.title) : null,
    e.description ? h('div', { class: 'e-desc' }, strip(e.description)) : null,
    e.fields && e.fields.length ? h('div', { class: 'e-fields' }, e.fields.map((f) =>
      h('div', { class: 'e-field' + (f.inline ? '' : ' full') }, h('b', {}, f.name), h('span', {}, strip(f.value))))) : null,
    e.footer ? h('div', { class: 'e-foot' }, e.footer.text) : null));
  if (e.thumbnail && /^https:\/\//.test(e.thumbnail.url)) {
    box.append(h('img', { src: e.thumbnail.url, alt: '', referrerpolicy: 'no-referrer', loading: 'lazy' }));
  }
  return box;
}

async function notificationModal(id, onChange) {
  const n = await api('GET', '/notifications/' + id);
  let embed = null;
  try { embed = n.embed ? JSON.parse(n.embed) : null; } catch (e) { /* ignore */ }
  let payload = n.payload;
  try { payload = JSON.stringify(JSON.parse(n.payload), null, 2); } catch (e) { /* not JSON */ }
  const body = [
    h('div', { class: 'row' },
      h('span', { class: 'badge ' + n.status }, n.status),
      h('span', { class: 'dim' }, n.integration_name + ' · ' + n.event + ' · ' + when(n.created_at)),
      n.channel_id ? h('span', { class: 'dim' }, '→ ' + (channelName(n.channel_id) || 'deleted channel')) : null),
    n.error ? h('div', { class: 'error' }, n.error) : h('div', { class: 'help' }, ' '),
    embed ? embedPreview(embed) : null,
    payload ? h('h3', {}, 'Received payload') : null,
    payload ? h('pre', {}, payload) : null,
  ];
  modal('Notification #' + n.id, body, (close) => [
    embed ? h('button', {
      class: 'btn', onclick: (e) => busy(e.target, async () => {
        await api('POST', '/notifications/' + id + '/resend');
        toast('Queued again');
        close();
        if (onChange) onChange();
      }),
    }, 'Send again') : null,
    h('button', { class: 'btn primary', onclick: close }, 'Close'),
  ], true);
}

async function historyPage() {
  const filters = { q: '', status: '', integration_id: '', offset: 0 };
  const limit = 50;
  const tableBox = h('div', { class: 'card table-wrap' });
  const pager = h('div', { class: 'row end' });
  pager.style.marginTop = '12px';

  const draw = async () => {
    const qs = new URLSearchParams({ limit, offset: filters.offset });
    for (const k of ['q', 'status', 'integration_id']) if (filters[k]) qs.set(k, filters[k]);
    const res = await api('GET', '/notifications?' + qs);
    tableBox.replaceChildren(notificationTable(res.items, draw));
    const from = res.total ? filters.offset + 1 : 0;
    pager.replaceChildren(
      h('span', { class: 'dim' }, from + '–' + (filters.offset + res.items.length) + ' of ' + res.total),
      h('button', { class: 'btn small', disabled: filters.offset === 0, onclick: () => { filters.offset = Math.max(0, filters.offset - limit); draw().catch(fail); } }, 'Newer'),
      h('button', { class: 'btn small', disabled: filters.offset + limit >= res.total, onclick: () => { filters.offset += limit; draw().catch(fail); } }, 'Older'));
  };
  const set = (k) => (e) => { filters[k] = e.target.value; filters.offset = 0; draw().catch(fail); };

  const search = h('input', { type: 'search', placeholder: 'Search title or event…', oninput: set('q') });
  const status = h('select', { onchange: set('status') },
    h('option', { value: '' }, 'Any status'), ['sent', 'failed', 'skipped', 'queued'].map((s) => h('option', { value: s }, s)));
  const integ = h('select', { onchange: set('integration_id') },
    h('option', { value: '' }, 'All integrations'), state.integrations.map((i) => h('option', { value: i.id }, i.name)));
  const filterRow = h('div', { class: 'row' }, h('div', { class: 'grow' }, search), h('div', {}, status), h('div', {}, integ));
  filterRow.style.marginBottom = '14px';

  setMain(
    pageHead('History', 'Every notification received, sent, skipped or failed.',
      h('button', { class: 'btn', onclick: () => draw().catch(fail) }, 'Refresh'),
      h('button', {
        class: 'btn danger', onclick: async () => {
          if (!confirm('Delete the entire notification history?')) return;
          try { await api('DELETE', '/notifications'); filters.offset = 0; await draw(); } catch (e) { fail(e); }
        },
      }, 'Clear history')),
    filterRow, tableBox, pager);
  await draw();
}

// ---------- channels ----------

async function channelsPage() {
  state.channels = await api('GET', '/channels');
  const rows = state.channels.map((c) => h('tr', {},
    h('td', {}, h('b', {}, c.name)),
    h('td', { class: 'dim mono' }, c.webhook_url.replace(/(\/webhooks\/\d+\/).+$/, '$1••••••••')),
    h('td', { class: 'nowrap' }, h('div', { class: 'row end' },
      h('button', {
        class: 'btn small', onclick: (e) => busy(e.target, async () => {
          await api('POST', '/channels/' + c.id + '/test');
          toast('Test message sent to ' + c.name);
        }),
      }, 'Test'),
      h('button', { class: 'btn small', onclick: () => channelModal(c) }, 'Edit')))));
  setMain(
    pageHead('Channels', 'Discord channels that notifications can be delivered to.',
      h('button', { class: 'btn primary', onclick: () => channelModal(null) }, 'Add channel')),
    h('div', { class: 'card table-wrap' }, rows.length
      ? h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Name'), h('th', {}, 'Webhook'), h('th', {}))), h('tbody', {}, rows))
      : h('div', { class: 'empty' }, 'No channels yet. In Discord: channel settings → Integrations → Webhooks → New Webhook → Copy Webhook URL.')));
}

function channelModal(c) {
  const name = h('input', { type: 'text', value: c ? c.name : '', placeholder: 'e.g. #media-alerts' });
  const url = h('input', { type: 'text', value: c ? c.webhook_url : '', placeholder: 'https://discord.com/api/webhooks/…' });
  modal(c ? 'Edit channel' : 'Add channel', [
    field('Name', name, 'A label for you; it does not need to match Discord.'),
    field('Discord webhook URL', url, 'Discord: channel settings → Integrations → Webhooks → New Webhook → Copy Webhook URL.'),
  ], (close) => [
    c ? h('button', {
      class: 'btn danger left', onclick: async () => {
        if (!confirm('Delete channel "' + c.name + '"? Integrations using it will stop sending until reassigned.')) return;
        try { await api('DELETE', '/channels/' + c.id); close(); channelsPage(); } catch (e) { fail(e); }
      },
    }, 'Delete') : null,
    h('button', { class: 'btn', onclick: close }, 'Cancel'),
    h('button', {
      class: 'btn primary', onclick: (e) => busy(e.target, async () => {
        const body = { name: name.value, webhook_url: url.value };
        if (c) await api('PUT', '/channels/' + c.id, body); else await api('POST', '/channels', body);
        close();
        toast('Channel saved');
        await channelsPage();
      }),
    }, 'Save'),
  ]);
  name.focus();
}

// ---------- integrations ----------

async function integrationsPage() {
  [state.integrations, state.channels] = await Promise.all([api('GET', '/integrations'), api('GET', '/channels')]);
  const groups = [...new Set(state.types.map((t) => t.group))];
  const sections = [];
  for (const g of groups) {
    const items = state.integrations.filter((i) => (typeDef(i.type) || {}).group === g);
    if (!items.length) continue;
    sections.push(h('h3', {}, g), h('div', { class: 'grid cards' }, items.map(integrationCard)));
  }
  setMain(
    pageHead('Integrations', 'Apps that send events to Notified, and things Notified watches for you.',
      h('button', { class: 'btn primary', onclick: typePicker }, 'Add integration')),
    sections.length ? sections : h('div', { class: 'card empty' }, 'No integrations yet. Add one to get a webhook URL for your app.'));
}

function integrationCard(i) {
  const def = typeDef(i.type) || { label: i.type };
  let badge = h('span', { class: 'badge ok' }, 'enabled');
  if (!i.enabled) badge = h('span', { class: 'badge off' }, 'disabled');
  else if (i.status && i.status.last_error) badge = h('span', { class: 'badge err' }, 'error');
  else if (!i.channel_id) badge = h('span', { class: 'badge warn' }, 'no channel');
  const meta = [];
  meta.push(i.channel_id ? '→ ' + (channelName(i.channel_id) || 'deleted channel') : 'No channel assigned');
  meta.push('last event ' + ago(i.last_event));
  if (i.status) meta.push('checked ' + ago(i.status.last_run));
  return h('div', { class: 'card icard', onclick: () => integrationModal(i) },
    h('div', { class: 'top' }, tile(i.type),
      h('div', { class: 'grow' }, h('div', { class: 'name' }, i.name), h('div', { class: 'meta' }, def.label)),
      badge),
    h('div', { class: 'meta' }, meta.join(' · ')),
    i.enabled && i.status && i.status.last_error ? h('div', { class: 'err' }, i.status.last_error) : null);
}

function typePicker() {
  let close;
  const groups = [...new Set(state.types.map((t) => t.group))];
  const body = groups.map((g) => [
    h('h3', {}, g),
    h('div', { class: 'typegrid' }, state.types.filter((t) => t.group === g).map((t) =>
      h('button', { class: 'btn', onclick: () => { close(); integrationModal(null, t.type); } }, tile(t.type), t.label))),
  ]);
  close = modal('Add integration', body, null, true);
}

function integrationModal(existing, newType) {
  const def = typeDef(existing ? existing.type : newType);
  if (!def) return fail(new Error('Unknown integration type'));
  const i = existing || { name: def.label, enabled: true, channel_id: state.channels.length === 1 ? state.channels[0].id : 0, config: {}, events: {} };

  const channelSelect = (value, emptyLabel) => h('select', {},
    h('option', { value: 0 }, emptyLabel),
    state.channels.map((c) => h('option', { value: c.id, selected: c.id === value }, c.name)));

  const name = h('input', { type: 'text', value: i.name });
  const enabled = h('input', { type: 'checkbox', checked: i.enabled });
  const channel = channelSelect(i.channel_id, '— none —');

  const inputs = {};
  const fields = def.fields.map((f) => {
    const val = i.config[f.key] !== undefined ? i.config[f.key] : (f.default || '');
    if (f.kind === 'bool') {
      inputs[f.key] = h('input', { type: 'checkbox', checked: val === 'true' });
      return h('label', { class: 'check' }, inputs[f.key], f.label);
    }
    inputs[f.key] = f.kind === 'textarea'
      ? h('textarea', { value: val })
      : h('input', { type: f.kind === 'password' ? 'password' : f.kind === 'number' ? 'number' : 'text', value: val, autocomplete: 'off' });
    return field(f.label, inputs[f.key], f.help);
  });

  const eventInputs = {};
  const eventRows = def.events.map((ev) => {
    const rule = i.events[ev.key] || { enabled: ev.default, channel_id: 0 };
    const on = h('input', { type: 'checkbox', checked: rule.enabled });
    const ch = channelSelect(rule.channel_id, 'Default channel');
    eventInputs[ev.key] = { on, ch };
    return h('tr', {}, h('td', {}, h('label', { class: 'check' }, on, ev.label)), h('td', {}, ch));
  });
  for (const r of eventRows) r.querySelector('label').style.marginBottom = '0';

  const collect = () => {
    const config = {};
    for (const f of def.fields) config[f.key] = f.kind === 'bool' ? String(inputs[f.key].checked) : inputs[f.key].value;
    const events = {};
    for (const ev of def.events) events[ev.key] = { enabled: eventInputs[ev.key].on.checked, channel_id: Number(eventInputs[ev.key].ch.value) };
    return { type: def.type, name: name.value, enabled: enabled.checked, channel_id: Number(channel.value), config, events };
  };

  let hookBox = null;
  if (def.webhook) {
    if (existing) {
      const urlInput = h('input', { type: 'text', readOnly: true, value: hookURL(existing.token), onfocus: (e) => e.target.select() });
      hookBox = field('Webhook URL', h('div', { class: 'copybox' }, urlInput,
        h('button', {
          class: 'btn', type: 'button', onclick: async () => {
            try { await navigator.clipboard.writeText(urlInput.value); toast('Copied'); } catch (e) { urlInput.select(); document.execCommand('copy'); toast('Copied'); }
          },
        }, 'Copy'),
        h('button', {
          class: 'btn', type: 'button', title: 'Generate a new URL; the old one stops working', onclick: async () => {
            if (!confirm('Generate a new webhook URL? The current one will stop working.')) return;
            try {
              const upd = await api('POST', '/integrations/' + existing.id + '/token');
              existing.token = upd.token;
              urlInput.value = hookURL(upd.token);
              toast('New URL generated');
            } catch (e) { fail(e); }
          },
        }, 'Regenerate')),
        'Anyone with this URL can send notifications, so treat it like a password. If the app runs in Docker, use an address it can reach (set the base URL in Settings).');
    } else {
      hookBox = h('div', { class: 'help' }, 'The webhook URL appears after you save.');
      hookBox.style.marginBottom = '14px';
    }
  }
  const setupText = existing ? def.setup.replaceAll('{URL}', hookURL(existing.token)) : def.setup.replaceAll('{URL}', 'the webhook URL');

  const body = [
    h('div', { class: 'setup' }, setupText),
    hookBox,
    h('div', { class: 'grid two' }, field('Name', name), field('Default Discord channel', channel)),
    h('label', { class: 'check' }, enabled, 'Enabled'),
    fields.length ? h('h3', {}, 'Settings') : null,
    fields,
    h('h3', {}, 'Events'),
    h('table', { class: 'events' }, h('tbody', {}, eventRows)),
  ];

  const save = async (close, keepOpen) => {
    const saved = existing
      ? await api('PUT', '/integrations/' + existing.id, collect())
      : await api('POST', '/integrations', collect());
    toast('Integration saved');
    close();
    await integrationsPage();
    if (keepOpen) integrationModal(state.integrations.find((x) => x.id === saved.id));
  };

  modal((existing ? 'Edit ' : 'Add ') + def.label, body, (close) => [
    existing ? h('button', {
      class: 'btn danger left', onclick: async () => {
        if (!confirm('Delete integration "' + existing.name + '"?')) return;
        try { await api('DELETE', '/integrations/' + existing.id); close(); integrationsPage(); } catch (e) { fail(e); }
      },
    }, 'Delete') : null,
    existing ? h('button', {
      class: 'btn', onclick: (e) => busy(e.target, async () => {
        await api('PUT', '/integrations/' + existing.id, collect());
        const r = await api('POST', '/integrations/' + existing.id + '/test');
        if (r.send_error) toast('Could not send: ' + r.send_error, true);
        else toast('Test notification queued');
        if (r.poll_error) toast('Check failed: ' + r.poll_error, true);
        else if (def.poller && !r.send_error) toast('Connection check passed');
      }),
    }, 'Save & test') : null,
    h('button', { class: 'btn', onclick: close }, 'Cancel'),
    h('button', { class: 'btn primary', onclick: (e) => busy(e.target, () => save(close, !existing && def.webhook)) }, existing ? 'Save' : 'Create'),
  ], true);
}

// ---------- container updates (Portainer) ----------

async function containerUpdatesPage() {
  const draw = () => {
    refreshNavCount();
    const groups = state.containerUpdates;
    const total = groups.reduce((sum, g) => sum + g.items.length, 0);
    let working = false;

    // Recreates one container via Portainer and removes its row on success.
    const apply = async (g, it, btn, row) => {
      btn.disabled = true;
      btn.textContent = 'Updating…';
      try {
        await api('POST', '/container-updates/apply', { integration_id: g.integration_id, env_id: it.env_id, container_id: it.container_id });
        g.items = g.items.filter((x) => x !== it);
        row.remove();
        refreshNavCount();
        toast(it.name + ' updated');
        return true;
      } catch (e) {
        btn.disabled = false;
        btn.textContent = 'Retry';
        toast(it.name + ': ' + e.message, true);
        return false;
      }
    };

    const sections = groups.map((g) => {
      const rows = g.items.map((it) => {
        const btn = h('button', { class: 'btn small primary' }, 'Update');
        const row = h('tr', {},
          h('td', {}, h('b', {}, it.name), it.stack ? h('div', { class: 'dim' }, 'stack: ' + it.stack) : null),
          h('td', { class: 'mono' }, it.image),
          h('td', { class: 'dim' }, it.env_name),
          h('td', { class: 'nowrap' }, h('div', { class: 'row end' }, btn)));
        btn.addEventListener('click', async () => {
          if (working) return toast('Another update is still running', true);
          working = true;
          await apply(g, it, btn, row);
          working = false;
          if (!g.items.length) draw();
        });
        row.update = () => apply(g, it, btn, row);
        return row;
      });
      const skipped = [];
      if (g.limited) skipped.push(g.limited + ' in stacks with limited control');
      if (g.ignored) skipped.push(g.ignored + ' ignored');
      return [
        h('h3', {}, g.integration_name),
        h('div', { class: 'card table-wrap' },
          rows.length
            ? h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Container'), h('th', {}, 'Image'), h('th', {}, 'Environment'), h('th', {}))), h('tbody', {}, rows))
            : h('div', { class: 'empty' }, g.checked_at ? 'Everything checked is up to date.' : 'Not checked yet. Press "Check now".')),
        h('div', { class: 'help' }, g.checked_at
          ? 'Checked ' + ago(g.checked_at) + ': ' + g.checked + ' containers compared with their registry' + (skipped.length ? '; skipped ' + skipped.join(', ') : '') + '.'
          : ''),
        g.errors.map((e) => h('div', { class: 'error' }, e)),
      ];
    });

    setMain(
      pageHead('Container updates', 'Containers whose image tag has a newer build in the registry. Updating pulls the image and recreates the container through Portainer.',
        total > 1 ? h('button', {
          class: 'btn', onclick: async (e) => {
            if (working) return toast('Another update is still running', true);
            if (!confirm('Update all ' + total + ' containers now, one after another?')) return;
            working = true;
            e.target.disabled = true;
            for (const row of mainEl.querySelectorAll('tbody tr')) await row.update();
            working = false;
            draw();
          },
        }, 'Update all') : null,
        groups.length ? h('button', {
          class: 'btn primary', onclick: (e) => busy(e.target, async () => {
            state.containerUpdates = await api('POST', '/container-updates/check');
            draw();
            toast('Check finished');
          }),
        }, 'Check now') : null),
      groups.length ? sections : h('div', { class: 'card empty' },
        'Add a Portainer integration under ', h('a', { href: '#integrations' }, 'Integrations'), ' to see container updates here.'));
  };
  state.containerUpdates = await api('GET', '/container-updates');
  draw();
}

// ---------- settings ----------

async function settingsPage() {
  const s = state.settings = await api('GET', '/settings');
  const mk = (key, type) => h('input', { type: type || 'text', value: s[key] || '' });
  const inputs = { instance_name: mk('instance_name'), bot_name: mk('bot_name'), avatar_url: mk('avatar_url'), base_url: mk('base_url'), retention: mk('retention', 'number') };
  const cur = h('input', { type: 'password', autocomplete: 'current-password' });
  const next = h('input', { type: 'password', autocomplete: 'new-password' });

  setMain(
    pageHead('Settings'),
    h('div', { class: 'grid two' },
      h('div', { class: 'card' }, h('h2', {}, 'General'),
        field('Instance name', inputs.instance_name, 'Shown in the sidebar and in each notification footer.'),
        field('Bot name', inputs.bot_name, 'The sender name shown in Discord.'),
        field('Bot avatar URL', inputs.avatar_url, 'Optional image URL for the sender avatar.'),
        field('Base URL', inputs.base_url, 'How your apps reach Notified, e.g. http://notified:8080. Used to build webhook URLs. Blank uses this browser address.'),
        field('History size', inputs.retention, 'Number of notifications to keep.'),
        h('button', {
          class: 'btn primary', onclick: (e) => busy(e.target, async () => {
            const body = {};
            for (const [k, el] of Object.entries(inputs)) body[k] = el.value;
            state.settings = await api('PUT', '/settings', body);
            toast('Settings saved');
            renderShell();
            route();
          }),
        }, 'Save settings')),
      h('div', { class: 'card' }, h('h2', {}, 'Password'),
        h('div', { class: 'help' }, 'Signed in as ' + (s.admin_user || 'admin') + '. Changing the password signs out other browsers.'),
        h('div', {}, ' '),
        field('Current password', cur),
        field('New password', next, 'At least 8 characters.'),
        h('button', {
          class: 'btn', onclick: (e) => busy(e.target, async () => {
            await api('POST', '/settings/password', { current: cur.value, new: next.value });
            cur.value = next.value = '';
            toast('Password changed');
          }),
        }, 'Change password')),
      updatesCard(),
      appearanceCard()));
}

function updatesCard() {
  const u = state.update;
  const s = state.settings;
  const enabled = h('input', { type: 'checkbox', checked: s.update_check !== 'false' });
  const status = u.update_available
    ? h('span', { class: 'badge warn' }, 'update available')
    : (u.latest ? h('span', { class: 'badge ok' }, 'up to date') : h('span', { class: 'badge' }, 'unknown'));
  return h('div', { class: 'card' }, h('h2', {}, 'Updates'),
    h('dl', { class: 'kv' },
      h('dt', {}, 'Installed'), h('dd', {}, verLabel(u.current), ' ', status),
      h('dt', {}, 'Latest release'), h('dd', {}, u.latest
        ? (u.url ? h('a', { href: u.url, target: '_blank', rel: 'noopener noreferrer' }, verLabel(u.latest)) : verLabel(u.latest))
        : 'none found yet'),
      h('dt', {}, 'Last checked'), h('dd', {}, u.checked_at ? ago(u.checked_at) : 'never')),
    u.error ? h('div', { class: 'error' }, u.error) : null,
    u.update_available ? h('div', { class: 'setup' }, 'To update, run on your Docker host:\ndocker compose pull && docker compose up -d') : null,
    h('label', { class: 'check' }, enabled, 'Check GitHub for new releases every 6 hours'),
    h('div', { class: 'row' },
      h('button', {
        class: 'btn primary', onclick: (e) => busy(e.target, async () => {
          state.settings = await api('PUT', '/settings', { update_check: String(enabled.checked) });
          toast('Update settings saved');
        }),
      }, 'Save'),
      h('button', {
        class: 'btn', onclick: (e) => busy(e.target, async () => {
          state.update = await api('POST', '/update/check');
          if (state.update.error) toast(state.update.error, true);
          else toast(state.update.update_available ? 'Update available: ' + verLabel(state.update.latest) : 'You are up to date');
          renderShell();
          route();
        }),
      }, 'Check now')));
}

function appearanceCard() {
  const sel = h('select', {
    onchange: (e) => { NotifiedTheme.set(e.target.value); renderShell(); route(); },
  }, Object.entries(themeNames).map(([k, label]) =>
    h('option', { value: k, selected: NotifiedTheme.get() === k }, k === 'system' ? 'Auto (follow this device)' : label)));
  return h('div', { class: 'card' }, h('h2', {}, 'Appearance'),
    field('Theme', sel, 'Saved in this browser.'));
}

boot();
