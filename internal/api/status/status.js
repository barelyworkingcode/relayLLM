'use strict';

// ---------------------------------------------------------------- state --

let skewMs = 0;                 // Date.parse(generatedAt) - Date.now(), refreshed each poll
let startTimeMs = null;         // wall-clock estimate of process start, set on first poll
let paused = false;
let pollTimer = null;
let pollDelay = 2000;
let consecutiveFailures = 0;
let lastPollAt = null;          // Date.now() of last completed poll (success or failure)
let lastPollOk = false;
let lastSnapshot = null;
const expandedKeys = new Set(); // connections rows currently showing detail
const attentionItems = [];      // rebuilt each poll, read by tick() for the header/title
const rateBuffers = new Map();  // row key -> [{v, stalled}] ring, 30 samples
const tileSparkBuffers = { in: [], out: [] };

// ------------------------------------------------------------ formatters --

function fmtDuration(totalSeconds) {
  const s = Math.max(0, Math.floor(totalSeconds));
  if (s < 60) return s + 's';
  const m = Math.floor(s / 60), rs = s % 60;
  if (m < 60) return m + 'm ' + String(rs).padStart(2, '0') + 's';
  const h = Math.floor(m / 60), rm = m % 60;
  if (h < 24) return h + 'h ' + rm + 'm';
  const d = Math.floor(h / 24), rh = h % 24;
  return d + 'd ' + rh + 'h';
}

function fmtBytes(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
  return (n / 1024 / 1024 / 1024).toFixed(1) + ' GB';
}

function fmtRate(bps) {
  if (!bps) return '0 B/s';
  return fmtBytes(bps) + '/s';
}

function fmtInt(n) {
  if (n === undefined || n === null) return undefined;
  return Number(n).toLocaleString();
}

function fmtAbs(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}

function fmtAbsShort(ms) {
  if (!ms) return '—';
  const d = new Date(ms);
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) + ' ' +
    d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
}

function setText(el, text) {
  if (el.textContent !== text) el.textContent = text;
}

function nowMs() { return Date.now() + skewMs; }

// ---------------------------------------------------------- badge widget --

function buildBadge() {
  const el = document.createElement('span');
  el.className = 'badge';
  const glyph = document.createElement('span');
  glyph.className = 'badge-glyph';
  glyph.setAttribute('aria-hidden', 'true');
  const word = document.createElement('span');
  word.className = 'badge-word';
  word.setAttribute('aria-hidden', 'true');
  el.append(glyph, word);
  return { el, glyph, word };
}

function setBadge(b, status, variant, glyphChar, wordText, ariaLabel) {
  if (b.el.dataset.status !== status) b.el.dataset.status = status;
  if (variant === 'solid') {
    if (b.el.dataset.variant !== 'solid') b.el.dataset.variant = 'solid';
  } else if (b.el.hasAttribute('data-variant')) {
    b.el.removeAttribute('data-variant');
  }
  setText(b.glyph, glyphChar);
  if (wordText !== null) setText(b.word, wordText);
  if (ariaLabel !== undefined && b.el.getAttribute('aria-label') !== ariaLabel) {
    b.el.setAttribute('aria-label', ariaLabel);
  }
}

// ------------------------------------------------------------ sparklines --

const SVGNS = 'http://www.w3.org/2000/svg';

function buildSparkSvg(w, h) {
  const el = document.createElementNS(SVGNS, 'svg');
  el.setAttribute('class', 'spark');
  el.setAttribute('width', w); el.setAttribute('height', h);
  el.setAttribute('viewBox', `0 0 ${w} ${h}`);
  const base = document.createElementNS(SVGNS, 'line');
  base.setAttribute('class', 'spark-baseline');
  base.setAttribute('x1', '0'); base.setAttribute('x2', String(w));
  base.setAttribute('y1', String(h - 1)); base.setAttribute('y2', String(h - 1));
  const p1 = document.createElementNS(SVGNS, 'polyline');
  const p2 = document.createElementNS(SVGNS, 'polyline');
  p2.setAttribute('class', 'spark-stalled');
  el.append(base, p1, p2);

  function setPoints(buf) {
    if (!buf.length) { p1.setAttribute('points', ''); p2.setAttribute('points', ''); return; }
    const max = Math.max(1, ...buf.map((b) => b.v));
    const n = buf.length;
    const stepX = n > 1 ? w / (n - 1) : w;
    const pts = buf.map((b, i) => [i * stepX, (h - 1) - (b.v / max) * (h - 3)]);
    const splitIdx = buf.findIndex((b) => b.stalled);
    p1.classList.toggle('spark-flat', buf.every((b) => b.v === 0));
    if (splitIdx === -1) {
      p1.setAttribute('points', pts.map((p) => p.join(',')).join(' '));
      p2.setAttribute('points', '');
    } else {
      p1.setAttribute('points', pts.slice(0, splitIdx + 1).map((p) => p.join(',')).join(' '));
      p2.setAttribute('points', pts.slice(splitIdx).map((p) => p.join(',')).join(' '));
    }
  }
  return { el, setPoints };
}

function pushRate(key, v, stalled) {
  let buf = rateBuffers.get(key);
  if (!buf) { buf = []; rateBuffers.set(key, buf); }
  buf.push({ v, stalled });
  if (buf.length > 30) buf.shift();
  return buf;
}

// -------------------------------------------------------- list reconciler --

// Keeps a container (tbody or ul) in sync with an ordered `items` array by
// key, touching the DOM only where the incoming order actually differs from
// the current one — see frontend.md §7. `row-detail` siblings (connections'
// expanded panels) are not part of `items`; the anchor walk skips over them
// so they ride along with their owning row instead of being mistaken for
// the next item's slot.
function reconcileList(container, items, keyFn, createFn, updateFn) {
  const seen = new Set();
  let anchor = container.firstChild;
  const skipDetail = () => { while (anchor && anchor.classList && anchor.classList.contains('row-detail')) anchor = anchor.nextSibling; };
  skipDetail();
  for (const item of items) {
    const key = keyFn(item);
    seen.add(key);
    let node = container.querySelector(':scope > [data-key="' + cssEscape(key) + '"]');
    if (node) {
      updateFn(node, item);
    } else {
      node = createFn(item);
      node.dataset.key = key;
    }
    if (anchor !== node) {
      container.insertBefore(node, anchor);
    } else {
      anchor = anchor.nextSibling;
      skipDetail();
    }
  }
  for (const node of Array.from(container.children)) {
    if (node.classList.contains('row-detail')) continue;
    const key = node.dataset.key;
    if (key && !seen.has(key)) {
      const detail = node.nextSibling;
      if (detail && detail.classList && detail.classList.contains('row-detail')) detail.remove();
      node.classList.add('row-leaving');
      setTimeout(() => node.remove(), 150);
    }
  }
}

function cssEscape(s) {
  return window.CSS && CSS.escape ? CSS.escape(s) : String(s).replace(/[^a-zA-Z0-9_-]/g, '\\$&');
}

function setEmpty(prefix, isEmpty, tableId, message) {
  const empty = document.getElementById(prefix + '-empty');
  empty.hidden = !isEmpty;
  if (isEmpty && message) setText(empty, message);
  const table = tableId ? document.getElementById(tableId) : null;
  if (table) table.hidden = isEmpty;
}

// ------------------------------------------------------------ connections --

function connBadgeSpec(kind, state) {
  switch (state) {
    case 'active': return { status: 'good', glyph: '●', word: 'ACTIVE', variant: 'outline' };
    case 'idle': return { status: 'neutral', glyph: '○', word: 'IDLE', variant: 'outline' };
    case 'quiet': return { status: 'warn', glyph: '◐', word: 'QUIET', variant: 'outline' };
    case 'stalled': return { status: 'critical', glyph: '■', word: 'STALLED', variant: 'solid' };
    default: return { status: 'neutral', glyph: '○', word: String(state || 'unknown').toUpperCase(), variant: 'outline' };
  }
}

function connGroup(state) {
  // stalled -> quiet -> active -> idle, per frontend.md §4.5
  return { stalled: 0, quiet: 1, active: 2, idle: 3 }[state] ?? 4;
}

function connKey(item) { return item.kind + ':' + item.id; }

function createConnRow(item) {
  const tr = document.createElement('tr');
  tr.className = 'row-clickable';
  tr.tabIndex = 0;
  tr.setAttribute('role', 'button');
  tr.setAttribute('aria-expanded', 'false');

  const tdState = document.createElement('td');
  const badge = buildBadge();
  tdState.appendChild(badge.el);

  const tdKind = document.createElement('td');

  const tdTarget = document.createElement('td');
  const primary = document.createElement('span'); primary.className = 'target-primary';
  const secondary = document.createElement('span'); secondary.className = 'target-secondary';
  tdTarget.append(primary, secondary);

  const tdClient = document.createElement('td'); tdClient.className = 'mono';

  const tdOpen = document.createElement('td'); tdOpen.className = 'num';
  const openSpan = document.createElement('span'); openSpan.dataset.kind = 'dur';
  tdOpen.appendChild(openSpan);

  const tdBytes = document.createElement('td'); tdBytes.className = 'num bytes-cell';
  const bIn = document.createElement('span'); const bOut = document.createElement('span'); bOut.className = 'out';
  tdBytes.append(bIn, bOut);

  const tdRate = document.createElement('td'); tdRate.className = 'num';
  const rateWrap = document.createElement('div'); rateWrap.className = 'rate-cell';
  const spark = buildSparkSvg(60, 16);
  const rateText = document.createElement('span');
  rateWrap.append(spark.el, rateText);
  tdRate.appendChild(rateWrap);

  const tdLast = document.createElement('td'); tdLast.className = 'num';
  const lastSpan = document.createElement('span'); lastSpan.dataset.kind = 'dur';
  tdLast.appendChild(lastSpan);

  const tdChevron = document.createElement('td');
  const chev = document.createElement('span'); chev.className = 'chevron'; chev.textContent = '›';
  chev.setAttribute('aria-hidden', 'true');
  tdChevron.appendChild(chev);

  tr.append(tdState, tdKind, tdTarget, tdClient, tdOpen, tdBytes, tdRate, tdLast, tdChevron);
  tr.__refs = { badge, kindCell: tdKind, primary, secondary, clientCell: tdClient, openSpan, bIn, bOut, spark, rateText, lastSpan, tdLast };

  const toggle = () => toggleConnRow(tr);
  tr.addEventListener('click', toggle);
  tr.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); } });

  updateConnRow(tr, item);
  return tr;
}

function updateConnRow(tr, item) {
  tr.__item = item;
  const r = tr.__refs;
  const state = item.state || 'active';
  const startedMs = item.startedAt ? Date.parse(item.startedAt) : null;
  const lastByteMs = item.lastByteAt ? Date.parse(item.lastByteAt) : null;

  const bd = connBadgeSpec(item.kind, state);
  const ticking = state === 'quiet' || state === 'stalled';
  const sinceMs = lastByteMs || startedMs;
  setBadge(r.badge, bd.status, bd.variant, bd.glyph, ticking ? null : bd.word,
    ticking ? bd.word.toLowerCase() + ', ' + Math.max(0, Math.floor((nowMs() - sinceMs) / 1000)) + ' seconds' : bd.word);
  if (ticking && sinceMs) {
    r.badge.word.dataset.since = String(sinceMs);
    r.badge.word.dataset.kind = 'badge';
    r.badge.word.dataset.prefix = bd.word;
    setText(r.badge.word, bd.word + ' ' + fmtDuration((nowMs() - sinceMs) / 1000));
  } else {
    delete r.badge.word.dataset.since;
  }
  r.badge.glyph.classList.toggle('is-pulse', state === 'stalled');

  setText(r.kindCell, item.kind.toUpperCase());

  const primaryText = item.model || item.target || item.path || '—';
  const secondaryText = (item.target && item.target !== item.model) ? item.target : (item.path || '');
  setText(r.primary, primaryText);
  setText(r.secondary, secondaryText);

  setText(r.clientCell, item.remoteAddr || '—');

  if (startedMs) {
    r.openSpan.dataset.since = String(startedMs);
    r.openSpan.title = fmtAbs(item.startedAt);
    setText(r.openSpan, fmtDuration((nowMs() - startedMs) / 1000));
  } else {
    delete r.openSpan.dataset.since;
    setText(r.openSpan, '—');
  }

  setText(r.bIn, '↓ ' + fmtBytes(item.bytesIn || 0));
  setText(r.bOut, '↑ ' + fmtBytes(item.bytesOut || 0));

  if (typeof item.bytesOutPerSec === 'number') {
    const buf = pushRate(connKey(item), item.bytesOutPerSec, state === 'stalled');
    r.spark.setPoints(buf);
    r.spark.el.style.visibility = 'visible';
    setText(r.rateText, fmtRate(item.bytesOutPerSec));
    const peak = Math.max(0, ...buf.map((b) => b.v));
    const avg = buf.reduce((a, b) => a + b.v, 0) / buf.length;
    r.rateText.title = 'peak ' + fmtRate(peak) + ' · avg ' + fmtRate(avg) + ' · last 60s';
  } else {
    r.spark.el.style.visibility = 'hidden';
    setText(r.rateText, '—');
  }

  if (lastByteMs) {
    r.tdLast.hidden = false;
    r.lastSpan.dataset.since = String(lastByteMs);
    r.lastSpan.title = fmtAbs(item.lastByteAt);
    setText(r.lastSpan, fmtDuration((nowMs() - lastByteMs) / 1000));
  } else {
    delete r.lastSpan.dataset.since;
    setText(r.lastSpan, '—');
  }

  tr.classList.toggle('row-idle', state === 'idle');
  tr.classList.toggle('row-stalled', state === 'stalled');

  if (expandedKeys.has(connKey(item))) refreshDetailRow(tr);
}

function connDetailFields(item) {
  const f = [];
  const add = (label, value) => { if (value !== undefined && value !== null && value !== '') f.push([label, value]); };
  add('REQUEST ID', item.id);
  add('OPENED', fmtAbs(item.startedAt));
  add('LAST BYTE', item.lastByteAt ? fmtAbs(item.lastByteAt) : undefined);
  add('PATH', [item.method, item.path].filter(Boolean).join(' '));
  add('MODEL (ASKED)', item.model);
  add('MODEL (SERVED)', item.target && item.target !== item.model ? item.target : undefined);
  add('TARGET KIND', item.targetKind);
  add('ATTEMPTS', item.attempts);
  add('STATUS', item.status || undefined);
  add('BYTES IN', fmtInt(item.bytesIn));
  add('BYTES OUT', fmtInt(item.bytesOut));
  add('VIA ANTHROPIC', item.viaAnthropic ? 'yes' : undefined);
  return f;
}

function toggleConnRow(tr) {
  const key = connKey(tr.__item);
  if (expandedKeys.has(key)) {
    expandedKeys.delete(key);
    const detail = tr.nextSibling;
    if (detail && detail.classList && detail.classList.contains('row-detail')) detail.remove();
    tr.setAttribute('aria-expanded', 'false');
    tr.classList.remove('is-expanded');
  } else {
    expandedKeys.add(key);
    tr.setAttribute('aria-expanded', 'true');
    tr.classList.add('is-expanded');
    refreshDetailRow(tr);
  }
}

function refreshDetailRow(tr) {
  let detail = tr.nextSibling;
  if (!detail || !detail.classList || !detail.classList.contains('row-detail')) {
    detail = document.createElement('tr');
    detail.className = 'row-detail';
    const td = document.createElement('td');
    td.colSpan = 9;
    const grid = document.createElement('div');
    grid.className = 'detail-grid';
    td.appendChild(grid);
    detail.appendChild(td);
    tr.after(detail);
  }
  const grid = detail.querySelector('.detail-grid');
  grid.innerHTML = '';
  for (const [label, value] of connDetailFields(tr.__item)) {
    const dl = document.createElement('span'); dl.className = 'detail-label'; dl.textContent = label;
    const dv = document.createElement('span'); dv.className = 'detail-value'; dv.textContent = value;
    grid.append(dl, dv);
  }
}

function renderConnections(json) {
  const items = (json.connections || []).slice().sort((a, b) => {
    const ga = connGroup(a.state), gb = connGroup(b.state);
    if (ga !== gb) return ga - gb;
    return Date.parse(b.startedAt || 0) - Date.parse(a.startedAt || 0);
  });
  const tbody = document.getElementById('connections-tbody');
  reconcileList(tbody, items, connKey, createConnRow, updateConnRow);
  setEmpty('connections', items.length === 0, 'connections-table', 'No open connections');
  document.getElementById('connections-count').textContent = items.length + ' open';
  return items;
}

// ----------------------------------------------------------------- models --

function modelKey(kind, alias) { return 'model:' + kind + ':' + alias; }

function mergeModelRows(json) {
  const catalog = (json.models && json.models.catalog) || [];
  const instances = (json.models && json.models.instances) || [];
  const byInst = new Map();
  for (const inst of instances) byInst.set(inst.kind + ':' + inst.alias, inst);
  const rows = catalog.map((c) => {
    const inst = byInst.get(c.kind + ':' + c.alias);
    let status;
    if (c.failed) status = 'failed';
    else if (inst && !inst.exited && inst.healthy) status = 'loaded';
    else if (inst && !inst.exited && !inst.healthy) status = 'loading';
    else if (inst && inst.exited) status = 'exited';
    else status = 'unloaded';
    return {
      key: modelKey(c.kind, c.alias), alias: c.alias, kind: c.kind, status,
      error: c.error, contextSize: c.contextSize, trainedContext: c.trainedContext,
      pid: inst ? inst.pid : null, port: inst ? inst.port : null,
      leases: inst ? inst.leases : 0, idleSeconds: inst ? inst.idleSeconds : null,
      estimatedGB: inst ? inst.estimatedGB : null, activeRequests: inst ? inst.activeRequests : 0,
    };
  });
  const group = { failed: 0, loading: 1, loaded: 2, exited: 3, unloaded: 4 };
  rows.sort((a, b) => {
    if (group[a.status] !== group[b.status]) return group[a.status] - group[b.status];
    if (a.status === 'loaded') {
      if (b.leases !== a.leases) return b.leases - a.leases;
      return (a.idleSeconds ?? 0) - (b.idleSeconds ?? 0);
    }
    return a.alias.localeCompare(b.alias);
  });
  return rows;
}

const modelBadge = {
  loaded: { status: 'good', glyph: '●', word: 'LOADED' },
  loading: { status: 'progress', glyph: '◌', word: 'LOADING' },
  unloaded: { status: 'neutral', glyph: '○', word: 'UNLOADED' },
  exited: { status: 'serious', glyph: '○', word: 'EXITED' },
  failed: { status: 'critical', glyph: '✕', word: 'FAILED' },
};

function createModelRow(item) {
  const tr = document.createElement('tr');
  const tdAlias = document.createElement('td'); tdAlias.className = 'mono';
  const aliasLine = document.createElement('span');
  const errLine = document.createElement('span'); errLine.className = 'target-secondary truncate'; errLine.hidden = true;
  tdAlias.append(aliasLine, errLine);
  const tdBackend = document.createElement('td');
  const chip = document.createElement('span'); chip.className = 'backend-chip'; tdBackend.appendChild(chip);
  const tdStatus = document.createElement('td');
  const badge = buildBadge(); tdStatus.appendChild(badge.el);
  const tdPidPort = document.createElement('td'); tdPidPort.className = 'mono num';
  const tdMem = document.createElement('td'); tdMem.className = 'num';
  const tdLeases = document.createElement('td'); tdLeases.className = 'num';
  const leaseDot = document.createElement('span'); leaseDot.className = 'lease-dot';
  const leaseText = document.createElement('span');
  tdLeases.append(leaseDot, leaseText);
  const tdIdle = document.createElement('td'); tdIdle.className = 'num';
  const idleSpan = document.createElement('span'); idleSpan.dataset.kind = 'dur'; tdIdle.appendChild(idleSpan);
  tr.append(tdAlias, tdBackend, tdStatus, tdPidPort, tdMem, tdLeases, tdIdle);
  tr.__refs = { aliasLine, errLine, chip, badge, tdPidPort, tdMem, tdLeases, leaseDot, leaseText, tdIdle, idleSpan };
  updateModelRow(tr, item);
  return tr;
}

function updateModelRow(tr, item) {
  const r = tr.__refs;
  setText(r.aliasLine, item.alias);
  if (item.status === 'failed' && item.error) {
    r.errLine.hidden = false;
    r.errLine.title = item.error;
    setText(r.errLine, item.error);
  } else {
    r.errLine.hidden = true;
  }
  setText(r.chip, item.kind);
  const bd = modelBadge[item.status] || modelBadge.unloaded;
  setBadge(r.badge, bd.status, null, bd.glyph, bd.word, bd.word);
  r.badge.glyph.classList.toggle('is-progress', item.status === 'loading');
  setText(r.tdPidPort, item.pid ? (item.pid + ' : ' + item.port) : '—');
  setText(r.tdMem, item.estimatedGB ? item.estimatedGB + ' GB' : '—');
  if (item.estimatedGB) r.tdMem.title = 'estimated from GGUF header';
  if (item.leases > 0) {
    r.leaseDot.hidden = false;
    setText(r.leaseText, String(item.leases));
    r.tdLeases.style.color = '';
  } else {
    r.leaseDot.hidden = true;
    setText(r.leaseText, '0');
    r.tdLeases.style.color = 'var(--ink-3)';
  }
  if (item.leases > 0 || item.idleSeconds === null) {
    delete r.idleSpan.dataset.since;
    setText(r.idleSpan, '—');
  } else {
    r.idleSpan.dataset.since = String(nowMs() - item.idleSeconds * 1000);
    setText(r.idleSpan, fmtDuration(item.idleSeconds));
  }
  tr.classList.toggle('row-idle', item.status === 'unloaded');
  tr.classList.toggle('row-critical', item.status === 'failed');
}

function renderModels(json) {
  const rows = mergeModelRows(json);
  const tbody = document.getElementById('models-tbody');
  reconcileList(tbody, rows, (r) => r.key, createModelRow, updateModelRow);
  setEmpty('models', rows.length === 0, 'models-table', 'No managed models configured');
  const loaded = rows.filter((r) => r.status === 'loaded').length;
  document.getElementById('models-count').textContent = rows.length === 0
    ? 'No managed models configured' : rows.length + ' configured · ' + loaded + ' loaded';
  return rows;
}

// ---------------------------------------------------------------- budgets --

function renderBudgets(json) {
  const budgets = json.budgets || [];
  const body = document.getElementById('budgets-body');
  setEmpty('budgets', budgets.length === 0, null, 'No budgets configured');
  document.getElementById('budgets-count').textContent = budgets.length ? budgets.length + ' configured' : '0 configured';
  body.querySelectorAll('.budget-block').forEach((n) => n.remove());
  for (const b of budgets) {
    const block = document.createElement('div');
    block.className = 'budget-block';
    block.dataset.key = 'budget:' + b.kind;
    const title = document.createElement('div'); title.className = 'budget-title'; title.textContent = b.kind;
    block.appendChild(title);
    block.appendChild(meterRow('LOADED', b.loaded, b.maxLoaded, false));
    block.appendChild(meterRow('MEMORY', Number(b.usedMemoryGB.toFixed(1)), b.maxMemoryGB, true, ' GB'));
    if (b.idleTimeout) {
      const note = document.createElement('div'); note.className = 'meter-note';
      note.textContent = 'IDLE REAPER   ' + b.idleTimeout;
      block.appendChild(note);
    }
    body.appendChild(block);
  }
}

function meterRow(label, value, cap, decimal, unit) {
  const wrap = document.createElement('div');
  const row = document.createElement('div'); row.className = 'meter-row';
  const l = document.createElement('span'); l.textContent = label;
  const v = document.createElement('span'); v.className = 'meter-value';
  row.append(l, v);
  wrap.appendChild(row);
  if (!cap) {
    v.textContent = value + (unit || '') + ' · no cap';
    return wrap;
  }
  v.textContent = value + (unit || '') + ' / ' + cap + (unit || '');
  const track = document.createElement('div'); track.className = 'meter-track';
  const fill = document.createElement('div'); fill.className = 'meter-fill';
  const pct = Math.min(1, value / cap);
  fill.style.width = (pct * 100) + '%';
  const status = pct >= 1 ? 'critical' : pct >= 0.85 ? 'warn' : null;
  if (status) { fill.dataset.status = status; track.dataset.status = status; }
  track.appendChild(fill);
  wrap.appendChild(track);
  return wrap;
}

// ---------------------------------------------------------------- virtual --

function renderVirtual(json) {
  const virtual = (json.models && json.models.virtual) || [];
  const body = document.getElementById('virtual-body');
  setEmpty('virtual', virtual.length === 0, null, 'No virtual models configured');
  document.getElementById('virtual-count').textContent = virtual.length ? virtual.length + ' configured' : '0 configured';
  body.querySelectorAll('.virtual-block').forEach((n) => n.remove());
  for (const v of virtual) {
    const block = document.createElement('div');
    block.className = 'virtual-block';
    block.dataset.key = 'virtual:' + v.name;
    const noTarget = v.reachableCandidates === 0 && (v.candidates || []).length > 0;
    if (noTarget) block.classList.add('no-target');

    const nameRow = document.createElement('div'); nameRow.className = 'virtual-name-row';
    const name = document.createElement('span'); name.className = 'virtual-name'; name.textContent = v.name;
    nameRow.appendChild(name);
    if (noTarget) {
      const b = buildBadge();
      setBadge(b, 'critical', 'solid', '✕', 'NO TARGET', 'no target reachable');
      const tag = document.createElement('span'); tag.className = 'virtual-tags'; tag.appendChild(b.el);
      nameRow.appendChild(tag);
    }
    if (v.activeRequests) {
      const meta = document.createElement('span'); meta.className = 'virtual-meta';
      meta.textContent = v.activeRequests + ' active now';
      nameRow.appendChild(meta);
    }
    block.appendChild(nameRow);

    const wrap = document.createElement('div'); wrap.className = 'chip-wrap';
    const cands = v.candidates || [];
    const servingOrder = cands.findIndex((c) => c.reachable);
    cands.forEach((c, i) => {
      if (i > 0) { const arrow = document.createElement('span'); arrow.className = 'chip-arrow'; arrow.textContent = '─▸'; wrap.appendChild(arrow); }
      const col = document.createElement('span'); col.className = 'chip-col';
      const chip = document.createElement('span'); chip.className = 'chip';
      if (i === servingOrder) chip.classList.add('chip-serving');
      const idx = document.createElement('span'); idx.className = 'chip-index'; idx.textContent = '①②③④⑤⑥⑦⑧⑨'[i] || (i + 1) + '.';
      const label = document.createElement('span'); label.className = 'chip-label'; label.textContent = c.label;
      const reach = document.createElement('span'); reach.className = 'chip-reach';
      const rb = c.reachable ? { s: 'good', g: '●', w: 'reachable' } : { s: 'serious', g: '○', w: 'unreachable' };
      reach.dataset.status = rb.s;
      const rg = document.createElement('span'); rg.className = 'badge-glyph'; rg.textContent = rb.g;
      reach.append(rg, document.createTextNode(' ' + rb.w));
      chip.append(idx, label, reach);
      col.appendChild(chip);
      if (i === servingOrder) { const cap = document.createElement('span'); cap.className = 'chip-caption'; cap.textContent = 'currently serving'; col.appendChild(cap); }
      wrap.appendChild(col);
    });
    block.appendChild(wrap);
    body.appendChild(block);
  }
}

// -------------------------------------------------------------- endpoints --

function endpointGroup(status) { return { offline: 0, probing: 1, online: 2 }[status] ?? 3; }

function endpointStatusOf(e) {
  if (!e.online && e.error) return 'offline';
  if (!e.online) return 'probing';
  return (e.ageSeconds || 0) > 60 ? 'stale' : 'online';
}

function createEndpointRow(item) {
  const tr = document.createElement('tr');
  const tdName = document.createElement('td'); tdName.className = 'mono';
  const tdStatus = document.createElement('td'); const badge = buildBadge(); tdStatus.appendChild(badge.el);
  const tdUrl = document.createElement('td'); tdUrl.className = 'mono truncate'; tdUrl.style.color = 'var(--ink-2)';
  const tdModels = document.createElement('td'); tdModels.className = 'num';
  const tdChecked = document.createElement('td'); tdChecked.className = 'num';
  const checkedSpan = document.createElement('span'); checkedSpan.dataset.kind = 'ago'; tdChecked.appendChild(checkedSpan);
  const tdErr = document.createElement('td'); tdErr.className = 'mono truncate'; tdErr.style.color = 'var(--ink-2)';
  tr.append(tdName, tdStatus, tdUrl, tdModels, tdChecked, tdErr);
  tr.__refs = { tdName, badge, tdUrl, tdModels, checkedSpan, tdErr };
  updateEndpointRow(tr, item);
  return tr;
}

function updateEndpointRow(tr, item) {
  const r = tr.__refs;
  setText(r.tdName, item.name);
  const st = endpointStatusOf(item);
  if (st === 'offline') setBadge(r.badge, 'serious', null, '○', 'OFFLINE', 'offline');
  else if (st === 'probing') setBadge(r.badge, 'progress', null, '◌', 'PROBING', 'probing');
  else if (st === 'stale') setBadge(r.badge, 'warn', null, '●', 'ONLINE ?', 'online, stale');
  else setBadge(r.badge, 'good', null, '●', 'ONLINE', 'online');
  r.badge.glyph.classList.toggle('is-progress', st === 'probing');
  if (st === 'stale') r.badge.el.title = 'not re-checked for ' + fmtDuration(item.ageSeconds);
  setText(r.tdUrl, item.baseURL || '—'); r.tdUrl.title = item.baseURL || '';
  setText(r.tdModels, item.online ? String(item.modelCount || 0) : '—');
  if (item.lastChecked) {
    r.checkedSpan.dataset.since = String(Date.parse(item.lastChecked));
    r.checkedSpan.title = fmtAbs(item.lastChecked);
    setText(r.checkedSpan, fmtDuration(item.ageSeconds || 0) + ' ago');
  }
  setText(r.tdErr, item.error || '');
  r.tdErr.title = item.error || '';
  tr.classList.toggle('row-serious', st === 'offline');
}

function renderEndpoints(json) {
  const rows = ((json.models && json.models.endpoints) || []).slice().sort((a, b) => {
    const ga = endpointGroup(endpointStatusOf(a)), gb = endpointGroup(endpointStatusOf(b));
    if (ga !== gb) return ga - gb;
    return a.name.localeCompare(b.name);
  });
  const tbody = document.getElementById('endpoints-tbody');
  reconcileList(tbody, rows, (r) => 'endpoint:' + r.name, createEndpointRow, updateEndpointRow);
  setEmpty('endpoints', rows.length === 0, 'endpoints-table', 'No OpenAI-compatible endpoints configured');
  document.getElementById('endpoints-count').textContent = rows.length
    ? rows.length + ' configured · ' + rows.filter((r) => r.online).length + ' online'
    : 'No OpenAI-compatible endpoints configured';
  return rows;
}

// --------------------------------------------------------------- overview --

const TILE_ORDER = ['uptime', 'requests', 'in', 'out', 'attention'];
let overviewBuilt = false;

function buildOverviewSkeleton() {
  const strip = document.getElementById('overview-strip');
  strip.innerHTML = '';
  const labels = { uptime: 'UPTIME', requests: 'REQUESTS', in: 'IN', out: 'OUT', attention: 'ATTENTION' };
  const refs = {};
  for (const key of TILE_ORDER) {
    const tile = document.createElement('div'); tile.className = 'tile'; tile.dataset.tile = key;
    if (key === 'attention') tile.classList.add('tile-attention');
    const label = document.createElement('div'); label.className = 'tile-label'; label.textContent = labels[key];
    const value = document.createElement('div'); value.className = 'tile-value'; value.textContent = '—';
    const sub = document.createElement('div'); sub.className = 'tile-sub';
    tile.append(label, value, sub);
    strip.appendChild(tile);
    refs[key] = { tile, value, sub };
  }
  overviewBuilt = true;
  return refs;
}

let overviewRefs = null;
let tileSparks = {};

function renderOverview(json, streamingCount) {
  if (!overviewBuilt) overviewRefs = buildOverviewSkeleton();
  const o = json.overview || {};
  const r = overviewRefs;

  if (startTimeMs === null) startTimeMs = nowMs() - (json.uptimeSeconds || 0) * 1000;
  setText(r.uptime.value, fmtDuration(json.uptimeSeconds || 0));
  setText(r.uptime.sub, 'since ' + fmtAbsShort(startTimeMs));

  setText(r.requests.value, String(o.proxyConnections ?? 0));
  setText(r.requests.sub, 'proxied · ' + streamingCount + ' streaming');

  const t = o.throughput || {};
  setText(r['in'].value, fmtRate(t.bytesInPerSec || 0));
  renderTileSpark('in', t.bytesInPerSec || 0, r['in'].sub);
  setText(r['out'].value, fmtRate(t.bytesOutPerSec || 0));
  renderTileSpark('out', t.bytesOutPerSec || 0, r['out'].sub);

  const critical = attentionItems.some((i) => i.status === 'critical');
  r.attention.value.innerHTML = '';
  r.attention.value.classList.toggle('is-zero', attentionItems.length === 0);
  if (attentionItems.length > 0) {
    const g = document.createElement('span'); g.className = 'attn-glyph';
    g.dataset.status = critical ? 'critical' : 'serious';
    g.textContent = critical ? '■' : '●';
    r.attention.value.append(g, document.createTextNode(String(attentionItems.length)));
  } else {
    r.attention.value.textContent = '0';
  }
  setText(r.attention.sub, attentionItems.length === 0 ? 'all quiet'
    : summarizeAttention());
}

function summarizeAttention() {
  const counts = {};
  for (const i of attentionItems) counts[i.badge.word.toLowerCase()] = (counts[i.badge.word.toLowerCase()] || 0) + 1;
  return Object.entries(counts).map(([w, n]) => n + ' ' + w).join(', ');
}

function renderTileSpark(key, value, subEl) {
  if (!tileSparks[key]) {
    const spark = buildSparkSvg(100, 20);
    tileSparks[key] = spark;
    spark.el.classList.add('tile-sparkline');
    subEl.appendChild(spark.el);
  }
  if (!tileSparkBuffers[key]) tileSparkBuffers[key] = [];
  const buf = tileSparkBuffers[key];
  buf.push({ v: value, stalled: false });
  if (buf.length > 30) buf.shift();
  tileSparks[key].setPoints(buf);
  const peak = Math.max(0, ...buf.map((b) => b.v));
  const avg = buf.reduce((a, b) => a + b.v, 0) / buf.length;
  subEl.title = 'peak ' + fmtRate(peak) + ' · avg ' + fmtRate(avg) + ' · last 60s';
}

// -------------------------------------------------------------- attention --

function rankStatus(s) { return { critical: 0, serious: 1, warn: 2 }[s] ?? 3; }

function buildAttention(json, modelRows, endpointRows) {
  const items = [];
  for (const c of json.connections || []) {
    if (c.state !== 'stalled') continue;
    const since = c.lastByteAt ? Date.parse(c.lastByteAt) : Date.parse(c.startedAt);
    items.push({
      key: 'attn:' + connKey(c), status: 'critical', badge: { glyph: '■', word: 'STALLED' },
      ageSecs: (nowMs() - since) / 1000, rowKey: connKey(c),
      facts: [c.kind.toUpperCase(), c.model || c.target || '', c.remoteAddr || '', 'open ' + fmtDuration((nowMs() - Date.parse(c.startedAt)) / 1000)].filter(Boolean).join('  '),
    });
  }
  for (const m of modelRows) {
    if (m.status !== 'failed') continue;
    items.push({
      key: 'attn:' + m.key, status: 'critical', badge: { glyph: '✕', word: 'FAILED' },
      ageSecs: 0, rowKey: m.key, facts: 'model ' + m.alias + (m.error ? '  "' + m.error + '"' : ''),
    });
  }
  for (const e of endpointRows) {
    const st = endpointStatusOf(e);
    if (st === 'offline') {
      items.push({
        key: 'attn:endpoint:' + e.name, status: 'serious', badge: { glyph: '○', word: 'OFFLINE' },
        ageSecs: e.ageSeconds || 0, rowKey: 'endpoint:' + e.name,
        facts: 'endpoint ' + e.name + (e.error ? '  "' + e.error + '"' : '') + ' · checked ' + fmtDuration(e.ageSeconds || 0) + ' ago',
      });
    } else if (st === 'stale') {
      items.push({
        key: 'attn:endpoint-stale:' + e.name, status: 'warn', badge: { glyph: '●', word: 'STALE' },
        ageSecs: e.ageSeconds || 0, rowKey: 'endpoint:' + e.name,
        facts: 'endpoint ' + e.name + ' not re-checked for ' + fmtDuration(e.ageSeconds || 0),
      });
    }
  }
  for (const b of json.budgets || []) {
    if (!b.maxMemoryGB) continue;
    const pct = b.usedMemoryGB / b.maxMemoryGB;
    if (pct < 0.85) continue;
    items.push({
      key: 'attn:budget:' + b.kind, status: pct >= 1 ? 'critical' : 'warn',
      badge: { glyph: pct >= 1 ? '■' : '◐', word: pct >= 1 ? 'AT CAP' : 'BUDGET' },
      ageSecs: 0, rowKey: 'budget:' + b.kind,
      facts: b.kind + ' memory ' + b.usedMemoryGB.toFixed(1) + ' / ' + b.maxMemoryGB + ' GB',
    });
  }
  for (const v of (json.models && json.models.virtual) || []) {
    if (v.reachableCandidates > 0 || !(v.candidates || []).length) continue;
    items.push({
      key: 'attn:virtual:' + v.name, status: 'critical', badge: { glyph: '✕', word: 'NO TARGET' },
      ageSecs: 0, rowKey: 'virtual:' + v.name, facts: 'virtual ' + v.name + '  no candidate reachable',
    });
  }
  items.sort((a, b) => rankStatus(a.status) - rankStatus(b.status) || b.ageSecs - a.ageSecs);
  return items;
}

function createAttentionRow(item) {
  const li = document.createElement('li');
  li.className = 'attn-row';
  const badge = buildBadge();
  const facts = document.createElement('span'); facts.className = 'attn-facts';
  const jump = document.createElement('button'); jump.type = 'button'; jump.className = 'attn-jump'; jump.textContent = 'jump ↓';
  jump.addEventListener('click', () => jumpToRow(item.rowKey));
  li.append(badge.el, facts, jump);
  li.__refs = { badge, facts, jump };
  updateAttentionRow(li, item);
  return li;
}

function updateAttentionRow(li, item) {
  li.dataset.status = item.status;
  const r = li.__refs;
  setBadge(r.badge, item.status, item.status === 'critical' ? 'solid' : null, item.badge.glyph, item.badge.word, item.badge.word);
  setText(r.facts, item.facts);
  r.jump.onclick = () => jumpToRow(item.rowKey);
}

function jumpToRow(key) {
  const el = document.querySelector('[data-key="' + cssEscape(key) + '"]');
  if (!el) return;
  el.scrollIntoView({ block: 'center', behavior: 'smooth' });
  el.classList.add('row-flash');
  setTimeout(() => el.classList.remove('row-flash'), 1200);
}

function renderAttention(items) {
  const block = document.getElementById('attention-block');
  block.hidden = items.length === 0;
  reconcileList(document.getElementById('attention-list'), items, (i) => i.key, createAttentionRow, updateAttentionRow);

  const stalledCount = items.filter((i) => i.badge.word === 'STALLED').length;
  const failedCount = items.filter((i) => i.badge.word === 'FAILED').length;
  const parts = [];
  if (stalledCount) parts.push('■ ' + stalledCount + ' stalled');
  if (failedCount) parts.push('✕ ' + failedCount + ' failed');
  document.title = parts.length ? parts.join(' ') + ' — relayLLM' : 'relayLLM status';

  const favicon = document.getElementById('favicon');
  favicon.href = stalledCount > 0
    ? "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='16' height='16'%3E%3Crect width='16' height='16' fill='%23d03b3b'/%3E%3C/svg%3E"
    : "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='16' height='16'%3E%3Ccircle cx='8' cy='8' r='6' fill='%23898781'/%3E%3C/svg%3E";
}

// ------------------------------------------------------------------ apply --

function apply(json) {
  lastSnapshot = json;
  skewMs = Date.parse(json.generatedAt) - Date.now();

  const connItems = renderConnections(json);
  const modelRows = renderModels(json);
  renderBudgets(json);
  renderVirtual(json);
  const endpointRows = renderEndpoints(json);

  attentionItems.length = 0;
  attentionItems.push(...buildAttention(json, modelRows, endpointRows));
  renderAttention(attentionItems);

  const streamingCount = connItems.filter((c) => c.streaming).length;
  renderOverview(json, streamingCount);
}

// ----------------------------------------------------------------- ticking --

function tick() {
  const now = nowMs();
  document.querySelectorAll('[data-since]').forEach((el) => {
    const since = Number(el.dataset.since);
    if (!since) return;
    const secs = (now - since) / 1000;
    const kind = el.dataset.kind || 'dur';
    let text;
    if (kind === 'ago') text = fmtDuration(secs) + ' ago';
    else if (kind === 'badge') text = el.dataset.prefix + ' ' + fmtDuration(secs);
    else text = fmtDuration(secs);
    if (el.textContent !== text) el.textContent = text;
  });
  updateLiveIndicator();
}
setInterval(tick, 1000);

// ------------------------------------------------------------------- poll --

function updateLiveIndicator() {
  const dot = document.getElementById('live-dot');
  const text = document.getElementById('live-text');
  const secsAgo = lastPollAt ? Math.floor((Date.now() - lastPollAt) / 1000) : null;
  if (!lastPollAt) { dot.dataset.status = 'neutral'; text.textContent = 'connecting'; return; }
  if (!lastPollOk) {
    dot.dataset.status = 'serious';
    text.textContent = 'disconnected · ' + secsAgo + 's ago';
  } else if (paused) {
    dot.dataset.status = 'neutral';
    text.textContent = 'paused · ' + secsAgo + 's ago';
  } else {
    dot.dataset.status = 'good';
    text.textContent = 'live · ' + secsAgo + 's ago';
  }
}

function showBanner(show) {
  const banner = document.getElementById('poll-banner');
  banner.hidden = !show;
  document.querySelector('main.page').classList.toggle('is-stale', show);
  if (show && lastSnapshot) {
    banner.textContent = 'Lost contact with relayLLM — showing data from ' +
      new Date(Date.parse(lastSnapshot.generatedAt)).toLocaleTimeString() + '. Retrying every ' + (pollDelay / 1000) + 's.';
  }
}

async function pollOnce() {
  try {
    const res = await fetch('/api/status/detailed', { cache: 'no-store', credentials: 'same-origin' });
    if (!res.ok) throw new Error('http ' + res.status);
    const json = await res.json();
    apply(json);
    consecutiveFailures = 0;
    pollDelay = 2000;
    lastPollOk = true;
    showBanner(false);
  } catch (e) {
    consecutiveFailures++;
    lastPollOk = false;
    console.error('status poll failed', e);
    if (consecutiveFailures >= 3) { pollDelay = 5000; showBanner(true); }
  }
  lastPollAt = Date.now();
  updateLiveIndicator();
}

function schedule() {
  if (pollTimer) clearTimeout(pollTimer);
  if (paused || document.hidden) return;
  pollTimer = setTimeout(async () => { await pollOnce(); schedule(); }, pollDelay);
}

async function pollLoop() {
  await pollOnce();
  schedule();
}

document.addEventListener('visibilitychange', () => {
  if (!document.hidden && !paused) {
    if (pollTimer) clearTimeout(pollTimer);
    pollOnce().then(schedule);
  }
});

// -------------------------------------------------------------- controls --

document.getElementById('pause-btn').addEventListener('click', () => {
  paused = !paused;
  document.getElementById('pause-btn').textContent = paused ? '▶ resume' : '⏸ pause';
  if (paused) { if (pollTimer) clearTimeout(pollTimer); }
  else { pollOnce().then(schedule); }
  updateLiveIndicator();
});

const THEME_KEY = 'relayllm-status-theme';
function applyTheme(theme) {
  document.documentElement.dataset.theme = theme;
  document.getElementById('theme-btn').textContent = theme === 'light' ? '☀' : '☾';
}
applyTheme(localStorage.getItem(THEME_KEY) === 'light' ? 'light' : 'dark');
document.getElementById('theme-btn').addEventListener('click', () => {
  const next = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
  localStorage.setItem(THEME_KEY, next);
  applyTheme(next);
});

// -------------------------------------------------------------------- go --

overviewRefs = buildOverviewSkeleton();
pollLoop();
