(() => {
  'use strict';

  const $ = (id) => document.getElementById(id);
  const qsa = (sel, root) => Array.from((root || document).querySelectorAll(sel));
  const SUPPORTED = ['zh', 'en', 'vi'];
  const RANGES = ['LIVE', '1H', '6H', '24H', '7D', '30D', 'CUSTOM'];
  const METRICS = [
    'requests_attempted', 'requests_quota_consumed', 'input_tokens', 'output_tokens',
    'total_tokens', 'credits', 'success_rate', 'latency', 'ttfb'
  ];
  const ERROR_CODES = [
    'validation_error', 'authentication_failed', 'api_key_disabled', 'api_key_expired',
    'request_quota_exceeded', 'token_quota_exceeded', 'credit_quota_exceeded',
    'no_available_accounts', 'provider_rate_limited', 'provider_error', 'provider_timeout',
    'client_cancelled', 'server_timeout', 'internal_error', 'unsettled_reservation'
  ];
  const ENDPOINTS = ['openai', 'claude', 'responses'];
  const STATUSES = ['success', 'failed', 'cancelled', 'rejected'];
  const MAX_RANGE_SEC = 365 * 24 * 3600;
  const MAX_DEDUPE = 2048;
  const STALE_MS = 45000;
  const RECONCILE_MS = 45000;
  const SSE = {
    CONNECTING: 'CONNECTING',
    LIVE: 'LIVE',
    RECONNECTING: 'RECONNECTING',
    STALE: 'STALE',
    OFFLINE: 'OFFLINE',
    SESSION_EXPIRED: 'SESSION_EXPIRED'
  };

  const dict = {};
  let lang = localStorage.getItem('kiro_lang') || (navigator.language || 'en').slice(0, 2);
  if (!SUPPORTED.includes(lang)) lang = 'en';

  function t(key, ...args) {
    let text = dict[key] || key;
    args.forEach((arg, idx) => { text = String(text).replace('{' + idx + '}', arg); });
    return text;
  }

  function applyI18n() {
    qsa('[data-i18n]').forEach((el) => { el.textContent = t(el.dataset.i18n); });
    qsa('[data-i18n-placeholder]').forEach((el) => { el.placeholder = t(el.dataset.i18nPlaceholder); });
    qsa('[data-i18n-aria-label]').forEach((el) => { el.setAttribute('aria-label', t(el.dataset.i18nAriaLabel)); });
    document.title = t('portal.title');
    document.documentElement.lang = lang;
  }

  async function loadI18n() {
    try {
      const res = await fetch('/locales/' + lang + '.json?v=' + Date.now(), { cache: 'no-store' });
      if (res.ok) Object.assign(dict, await res.json());
    } catch (e) { /* keep keys */ }
    applyI18n();
    fillLangSelects();
  }

  function applyTheme() {
    const pref = localStorage.getItem('kiro_theme') || 'system';
    const dark = pref === 'dark' || (pref === 'system' && window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
    document.documentElement.classList.toggle('dark', dark);
  }

  function fillLangSelects() {
    ['gateLang', 'dashLang'].forEach((id) => {
      const el = $(id);
      if (!el) return;
      el.innerHTML = SUPPORTED.map((l) => '<option value="' + l + '"' + (l === lang ? ' selected' : '') + '>' + t('lang.' + l) + '</option>').join('');
    });
  }

  function escapeHtml(s) {
    const d = document.createElement('div');
    d.textContent = s == null ? '' : String(s);
    return d.innerHTML;
  }

  function show(el, on) {
    if (el) el.classList.toggle('hidden', !on);
  }

  function banner(id, msg) {
    const el = $(id);
    if (!el) return;
    if (!msg) { show(el, false); return; }
    el.textContent = msg;
    show(el, true);
  }

  function notice(msg) {
    const el = $('notice');
    if (!msg) { show(el, false); return; }
    el.textContent = msg;
    show(el, true);
  }

  async function api(path, opts) {
    opts = opts || {};
    opts.credentials = 'same-origin';
    opts.headers = Object.assign({ 'Accept': 'application/json' }, opts.headers || {});
    if (opts.body && !opts.headers['Content-Type']) opts.headers['Content-Type'] = 'application/json';
    return fetch('/portal/api' + path, opts);
  }

  function readQuery() {
    const u = new URLSearchParams(location.search);
    const q = {
      range: (u.get('range') || '24H').toUpperCase(),
      from: u.get('from') || '',
      to: u.get('to') || '',
      model: u.get('model') || '',
      endpoint: u.get('endpoint') || '',
      status: u.get('status') || '',
      streaming: u.get('streaming') || '',
      error_code: u.get('error_code') || '',
      metric: u.get('metric') || 'requests_attempted'
    };
    if (RANGES.indexOf(q.range) < 0) q.range = '24H';
    if (METRICS.indexOf(q.metric) < 0) q.metric = 'requests_attempted';
    return q;
  }

  function writeQuery(q, replace) {
    const u = new URLSearchParams();
    Object.keys(q).forEach((k) => {
      if (q[k]) u.set(k, q[k]);
    });
    const next = location.pathname + (u.toString() ? '?' + u.toString() : '');
    if (replace) history.replaceState(null, '', next);
    else history.pushState(null, '', next);
  }

  function filterQuery(includeRange) {
    const q = state.query;
    const u = new URLSearchParams();
    if (includeRange) {
      u.set('range', q.range);
      if (q.range === 'CUSTOM') {
        u.set('from', q.from);
        u.set('to', q.to);
      }
    }
    if (q.model) u.set('model', q.model);
    if (q.endpoint) u.set('endpoint', q.endpoint);
    if (q.status) u.set('status', q.status);
    if (q.streaming) u.set('streaming', q.streaming);
    if (q.error_code) u.set('error_code', q.error_code);
    return u.toString();
  }

  function localToUnix(v) {
    if (!v) return 0;
    const d = new Date(v);
    const n = d.getTime();
    if (Number.isNaN(n)) return 0;
    return Math.floor(n / 1000);
  }

  function unixToLocal(sec) {
    if (!sec) return '';
    const d = new Date(Number(sec) * 1000);
    const p = (n) => String(n).padStart(2, '0');
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' + p(d.getHours()) + ':' + p(d.getMinutes());
  }

  function validateCustom(fromS, toS) {
    const from = localToUnix(fromS);
    const to = localToUnix(toS);
    if (!from || !to) return t('portal.err.invalid_date');
    if (from >= to) return t('portal.err.fromTo');
    if (to - from > MAX_RANGE_SEC) return t('portal.err.rangeTooLarge');
    return '';
  }

  function fmtNum(n) {
    if (n == null) return t('portal.na');
    return Number(n).toLocaleString(lang);
  }

  function fmtMs(v) {
    if (v == null) return t('portal.na');
    return Math.round(Number(v)) + ' ms';
  }

  function fmtTtfb(v) {
    if (v == null) return t('portal.na');
    return fmtMs(v);
  }

  function fmtRate(v) {
    if (v == null) return t('portal.na');
    return Math.round(Number(v) * 1000) / 10 + '%';
  }

  function fmtTime(v) {
    if (v == null || v === '') return t('portal.na');
    const d = typeof v === 'number' ? new Date(v * 1000) : new Date(v);
    if (Number.isNaN(d.getTime())) return t('portal.na');
    return d.toLocaleString();
  }

  function ago(ts) {
    if (!ts) return '';
    const s = Math.max(0, Math.round((Date.now() - ts) / 1000));
    if (s < 5) return t('portal.justNow');
    if (s < 60) return t('portal.secondsAgo', s);
    return t('portal.minutesAgo', Math.floor(s / 60));
  }

  function usedLimit(used, limit, remain) {
    const u = fmtNum(used || 0);
    if (!limit) return u;
    const r = remain == null ? '' : t('portal.remaining', fmtNum(remain));
    return u + ' / ' + fmtNum(limit) + (r ? ' · ' + r : '');
  }

  const state = {
    query: readQuery(),
    summary: null,
    series: null,
    events: [],
    nextCursor: '',
    hasMore: false,
    sse: SSE.CONNECTING,
    lastEventAt: 0,
    lastConnectionAt: 0,
    lastEventId: 0,
    seen: new Map(),
    source: null,
    clock: null,
    reconcile: null
  };

  function remember(id) {
    if (!id) return true;
    const key = String(id);
    if (state.seen.has(key)) return false;
    state.seen.set(key, true);
    if (state.seen.size > MAX_DEDUPE) {
      const first = state.seen.keys().next().value;
      state.seen.delete(first);
    }
    return true;
  }

  function eventInRange(ev) {
    const q = state.query;
    if (q.range === 'CUSTOM' && q.from && q.to) {
      const ts = Date.parse(ev.timestamp);
      if (Number.isNaN(ts)) return false;
      const sec = Math.floor(ts / 1000);
      return sec >= Number(q.from) && sec <= Number(q.to);
    }
    return true;
  }

  function fillControls() {
    $('rangeBar').innerHTML = RANGES.map((r) => {
      const pressed = state.query.range === r ? 'true' : 'false';
      return '<button type="button" class="range-btn" data-range="' + r + '" aria-pressed="' + pressed + '">' +
        escapeHtml(t('portal.range.' + r.toLowerCase())) + '</button>';
    }).join('');
    show($('customRange'), state.query.range === 'CUSTOM');
    if (state.query.range === 'CUSTOM') {
      $('customFrom').value = unixToLocal(state.query.from);
      $('customTo').value = unixToLocal(state.query.to);
    }
    $('filterModel').value = state.query.model;
    const opt = (value, label) => '<option value="' + escapeHtml(value) + '">' + escapeHtml(label) + '</option>';
    $('filterEndpoint').innerHTML = opt('', t('portal.all')) + ENDPOINTS.map((e) => opt(e, e)).join('');
    $('filterEndpoint').value = state.query.endpoint;
    $('filterStatus').innerHTML = opt('', t('portal.all')) + STATUSES.map((s) => opt(s, t('portal.' + s))).join('');
    $('filterStatus').value = state.query.status;
    $('filterStreaming').innerHTML = opt('', t('portal.all')) + opt('true', t('portal.streaming.yes')) + opt('false', t('portal.streaming.no'));
    $('filterStreaming').value = state.query.streaming;
    $('filterError').innerHTML = opt('', t('portal.all')) + ERROR_CODES.map((c) => opt(c, t('portal.error.' + c))).join('');
    $('filterError').value = state.query.error_code;
    $('metricSel').innerHTML = METRICS.map((m) => opt(m, t('portal.metric.' + m))).join('');
    $('metricSel').value = state.query.metric;
  }

  function renderLive() {
    const dot = $('liveDot');
    const label = $('liveLabel');
    dot.className = 'live-dot';
    let text = t('portal.sse.' + state.sse.toLowerCase());
    if (state.sse === SSE.LIVE) {
      dot.classList.add('ok');
      if (state.lastEventAt) text = t('portal.sse.liveAgo', ago(state.lastEventAt));
    } else if (state.sse === SSE.RECONNECTING || state.sse === SSE.CONNECTING) {
      dot.classList.add('warn');
      if (state.lastEventAt) text += ' · ' + t('portal.lastUpdate', ago(state.lastEventAt));
    } else {
      dot.classList.add('err');
    }
    label.textContent = text;
  }

  function renderHeader() {
    const s = state.summary || {};
    $('dashName').textContent = s.name || '—';
    $('dashMasked').textContent = s.keyMasked || '';
    const st = s.status || '';
    const pill = $('dashStatus');
    pill.textContent = t('portal.status.' + st) || st;
    pill.className = 'status-pill ' + st;
    $('dashExpiry').textContent = s.expiresAt ? t('portal.expires', fmtTime(s.expiresAt)) : '';
    $('dashReset').textContent = s.nextReset ? t('portal.nextReset', fmtTime(s.nextReset)) : '';
  }

  function card(label, value, sub) {
    return '<div class="card"><div class="label">' + escapeHtml(label) + '</div><div class="value">' +
      escapeHtml(value) + '</div>' + (sub ? '<div class="muted">' + escapeHtml(sub) + '</div>' : '') + '</div>';
  }

  function renderSummary() {
    const s = state.summary;
    if (!s) return;
    const html = [
      card(t('portal.credits'), usedLimit(s.creditsUsed, s.creditLimit, s.creditsRemaining)),
      card(t('portal.tokens'), usedLimit(s.tokensUsed, s.tokenLimit, s.tokensRemaining)),
      card(t('portal.requestQuota'), usedLimit(s.requestsQuotaConsumed != null ? s.requestsQuotaConsumed : s.requestsUsed, s.requestLimit, s.requestsRemaining), t('portal.requestsConsumedHint')),
      card(t('portal.requestsAttempted'), fmtNum(s.requestsAttempted || 0), t('portal.requestsAttemptedHint')),
      card(t('portal.success'), fmtNum(s.requestsSuccess || 0)),
      card(t('portal.failed'), fmtNum(s.requestsFailed || 0)),
      card(t('portal.cancelled'), fmtNum(s.requestsCancelled || 0)),
      card(t('portal.rejected'), fmtNum(s.requestsRejected || 0)),
      card(t('portal.successRate'), fmtRate(s.successRate)),
      card(t('portal.avgLatency'), fmtMs(s.avgLatencyMs)),
      card(t('portal.avgTtfb'), fmtTtfb(s.avgTtfbMs)),
      card(t('portal.lastUsed'), fmtTime(s.lastUsedAt))
    ];
    $('summaryCards').innerHTML = html.join('');
  }

  function pointValue(p, metric) {
    switch (metric) {
      case 'requests_attempted': return p.requestsAttempted;
      case 'requests_quota_consumed': return p.requestsQuotaConsumed;
      case 'input_tokens': return p.inputTokens;
      case 'output_tokens': return p.outputTokens;
      case 'total_tokens': return p.totalTokens;
      case 'credits': return p.credits;
      case 'success_rate': return p.successRate;
      case 'latency': return p.avgLatencyMs;
      case 'ttfb': return p.avgTtfbMs;
      default: return null;
    }
  }

  function renderChart() {
    const svg = $('chart');
    const series = state.series;
    const metric = state.query.metric;
    if (!series || !series.points || !series.points.length) {
      svg.innerHTML = '<text x="20" y="90" fill="currentColor" font-size="14">' + escapeHtml(t('portal.empty')) + '</text>';
      $('chartMeta').textContent = '';
      return;
    }
    const nullable = metric === 'ttfb' || metric === 'latency' || metric === 'success_rate';
    const pts = series.points.map((p) => ({ t: p.t, v: pointValue(p, metric) }))
      .filter((p) => !nullable || p.v != null);
    if (!pts.length) {
      svg.innerHTML = '<text x="20" y="90" fill="currentColor" font-size="14">' + escapeHtml(t('portal.empty')) + '</text>';
      return;
    }
    const w = 800, h = 180, pad = 18;
    const ys = pts.map((p) => Number(p.v) || 0);
    const max = Math.max(1e-9, ...ys);
    const step = (w - pad * 2) / Math.max(1, pts.length - 1);
    let d = '';
    pts.forEach((p, i) => {
      const x = pad + i * step;
      const y = h - pad - (Number(p.v) / max) * (h - pad * 2);
      d += (i ? ' L ' : 'M ') + x + ' ' + y;
    });
    svg.innerHTML = '<path d="' + d + '" fill="none" stroke="currentColor" stroke-width="2" />';
    const bits = [
      t('portal.resolution', series.resolution || ''),
      series.source || ''
    ];
    $('chartMeta').textContent = bits.filter(Boolean).join(' · ');
    renderRetention(series);
  }

  function renderRetention(meta) {
    if (!meta) return;
    const days = meta.dataRetention && meta.dataRetention.rawEventsDays;
    const parts = [];
    if (days) parts.push(t('portal.retentionHistory', days));
    if (meta.truncated) parts.push(t('portal.truncated'));
    notice(parts.join(' '));
  }

  function statusClass(st) {
    return st === 'success' ? 'status-ok' : 'status-bad';
  }

  function renderEvents() {
    const rows = $('eventRows');
    if (!state.events.length) {
      rows.innerHTML = '<tr><td colspan="13" class="empty">' + escapeHtml(t('portal.empty')) + '</td></tr>';
      show($('loadMore'), false);
      return;
    }
    rows.innerHTML = state.events.map((ev) => {
      return '<tr>' +
        '<td>' + escapeHtml(fmtTime(ev.timestamp)) + '</td>' +
        '<td class="mono muted">' + escapeHtml(ev.requestId || '') + '</td>' +
        '<td>' + escapeHtml(ev.endpoint || '') + '</td>' +
        '<td>' + escapeHtml(ev.model || ev.effectiveModel || ev.clientModel || '') + '</td>' +
        '<td class="' + statusClass(ev.status) + '">' + escapeHtml(t('portal.' + (ev.status || '')) || ev.status || '') + '</td>' +
        '<td>' + fmtNum(ev.inputTokens) + '</td>' +
        '<td>' + fmtNum(ev.outputTokens) + '</td>' +
        '<td>' + fmtNum(ev.totalTokens) + '</td>' +
        '<td>' + fmtNum(ev.credits) + '</td>' +
        '<td>' + fmtMs(ev.latencyMs) + '</td>' +
        '<td>' + fmtTtfb(ev.ttfbMs) + '</td>' +
        '<td>' + escapeHtml(ev.streaming ? t('portal.yes') : t('portal.no')) + '</td>' +
        '<td>' + escapeHtml(ev.errorCode ? t('portal.error.' + ev.errorCode) : '') + '</td>' +
        '</tr>';
    }).join('');
    show($('loadMore'), !!state.hasMore);
  }

  function applyEventToSeries(ev) {
    const series = state.series;
    if (!series || !series.bucketSeconds || !eventInRange(ev)) return;
    if (state.query.range !== 'LIVE' && state.query.range !== '1H' && state.query.range !== '6H') return;
    const ts = Date.parse(ev.timestamp);
    if (Number.isNaN(ts)) return;
    const bucket = Math.floor(Math.floor(ts / 1000) / series.bucketSeconds) * series.bucketSeconds;
    let p = series.points.find((x) => x.t === bucket);
    if (!p) {
      p = {
        t: bucket, requestsAttempted: 0, requestsQuotaConsumed: 0, requestsSuccess: 0,
        requestsFailed: 0, requestsCancelled: 0, requestsRejected: 0, inputTokens: 0,
        outputTokens: 0, totalTokens: 0, credits: 0, successRate: null, avgLatencyMs: null, avgTtfbMs: null
      };
      series.points.push(p);
      series.points.sort((a, b) => a.t - b.t);
    }
    p.requestsAttempted += 1;
    if (ev.status !== 'rejected') p.requestsQuotaConsumed += 1;
    if (ev.status === 'success') p.requestsSuccess += 1;
    if (ev.status === 'failed') p.requestsFailed += 1;
    if (ev.status === 'cancelled') p.requestsCancelled += 1;
    if (ev.status === 'rejected') p.requestsRejected += 1;
    p.inputTokens += ev.inputTokens || 0;
    p.outputTokens += ev.outputTokens || 0;
    p.totalTokens += ev.totalTokens || 0;
    p.credits += ev.credits || 0;
    renderChart();
  }

  function applyEventToSummary(ev) {
    const s = state.summary;
    if (!s) return;
    s.requestsAttempted = (s.requestsAttempted || 0) + 1;
    if (ev.status !== 'rejected') {
      s.requestsQuotaConsumed = (s.requestsQuotaConsumed || 0) + 1;
      s.requestsUsed = s.requestsQuotaConsumed;
    }
    if (ev.status === 'success') s.requestsSuccess = (s.requestsSuccess || 0) + 1;
    if (ev.status === 'failed') s.requestsFailed = (s.requestsFailed || 0) + 1;
    if (ev.status === 'cancelled') s.requestsCancelled = (s.requestsCancelled || 0) + 1;
    if (ev.status === 'rejected') s.requestsRejected = (s.requestsRejected || 0) + 1;
    s.tokensUsed = (s.tokensUsed || 0) + (ev.totalTokens || 0);
    s.creditsUsed = (s.creditsUsed || 0) + (ev.credits || 0);
    s.lastUsedAt = Math.floor(Date.now() / 1000);
    const consumed = (s.requestsSuccess || 0) + (s.requestsFailed || 0) + (s.requestsCancelled || 0);
    s.successRate = consumed ? (s.requestsSuccess || 0) / consumed : 0;
    renderSummary();
    renderHeader();
  }

  function ingestEvent(ev, fromLive) {
    if (!ev || !remember(ev.eventId)) return;
    if (ev.eventId) state.lastEventId = ev.eventId;
    state.lastEventAt = Date.now();
    if (!eventInRange(ev)) {
      renderLive();
      return;
    }
    const idx = state.events.findIndex((e) => e.eventId && e.eventId === ev.eventId);
    if (idx < 0) state.events.unshift(ev);
    if (fromLive) {
      applyEventToSeries(ev);
      applyEventToSummary(ev);
    }
    renderEvents();
    renderLive();
  }

  async function readJSON(res) {
    try { return await res.json(); } catch (e) { return {}; }
  }

  function errMessage(d, fallback) {
    if (!d) return fallback;
    const key = 'portal.err.' + (d.code || '');
    if (dict[key]) return t(key);
    return d.error || fallback;
  }

  async function refreshSummary() {
    const res = await api('/summary');
    if (res.status === 401) return expireSession();
    if (!res.ok) {
      banner('dashError', errMessage(await readJSON(res), t('portal.err.network')));
      return;
    }
    state.summary = await res.json();
    renderHeader();
    renderSummary();
  }

  async function refreshUsage() {
    const qs = filterQuery(true);
    const extra = state.query.metric ? '&metric=' + encodeURIComponent(state.query.metric) : '';
    const res = await api('/usage?' + qs + extra);
    if (res.status === 401) return expireSession();
    const d = await readJSON(res);
    if (!res.ok) {
      banner('dashError', errMessage(d, t('portal.err.network')));
      state.series = null;
      renderChart();
      return;
    }
    banner('dashError', '');
    state.series = d;
    renderChart();
  }

  async function refreshEvents(reset) {
    if (reset) {
      state.events = [];
      state.nextCursor = '';
      state.hasMore = false;
      state.seen.clear();
    }
    const qs = filterQuery(true);
    const cur = state.nextCursor && !reset ? '&cursor=' + encodeURIComponent(state.nextCursor) : '';
    const res = await api('/events?' + qs + '&limit=50' + cur);
    if (res.status === 401) return expireSession();
    const d = await readJSON(res);
    if (!res.ok) {
      banner('dashError', errMessage(d, t('portal.err.network')));
      renderEvents();
      return;
    }
    (d.items || []).forEach((ev) => {
      if (!remember(ev.eventId)) return;
      if (ev.eventId && ev.eventId > state.lastEventId) state.lastEventId = ev.eventId;
      state.events.push(ev);
    });
    state.nextCursor = d.nextCursor || '';
    state.hasMore = !!d.hasMore;
    if (d.truncated || (d.dataRetention && d.dataRetention.rawEventsDays)) renderRetention(d);
    renderEvents();
  }

  async function loadMore() {
    if (!state.hasMore || !state.nextCursor) return;
    await refreshEvents(false);
  }

  async function refreshAll() {
    await refreshSummary();
    await refreshUsage();
    await refreshEvents(true);
  }

  function expireSession() {
    closeStream();
    state.sse = SSE.SESSION_EXPIRED;
    renderLive();
    show($('dash'), false);
    show($('gate'), true);
    banner('gateError', t('portal.sse.expired'));
  }

  function closeStream() {
    if (state.source) {
      state.source.close();
      state.source = null;
    }
  }

  function openStream() {
    closeStream();
    if (state.sse === SSE.SESSION_EXPIRED) return;
    state.sse = SSE.CONNECTING;
    renderLive();
    const parts = [];
    const qs = filterQuery(false);
    if (qs) parts.push(qs);
    if (state.lastEventId) parts.push('after=' + encodeURIComponent(state.lastEventId));
    const url = '/portal/api/events/stream' + (parts.length ? '?' + parts.join('&') : '');
    const es = new EventSource(url);
    state.source = es;
    es.addEventListener('request', (e) => {
      let ev;
      try { ev = JSON.parse(e.data); } catch (err) { return; }
      ingestEvent(ev, true);
    });
    es.addEventListener('sync_required', (e) => {
      let body = {};
      try { body = JSON.parse(e.data); } catch (err) { body = {}; }
      if (body.highWater) state.lastEventId = body.highWater;
      notice(t('portal.syncRequired'));
      refreshAll();
    });
    es.addEventListener('session', () => expireSession());
    es.onopen = () => {
      state.sse = SSE.LIVE;
      state.lastConnectionAt = Date.now();
      renderLive();
    };
    es.onerror = () => {
      if (state.sse === SSE.SESSION_EXPIRED) return;
      state.sse = navigator.onLine === false ? SSE.OFFLINE : SSE.RECONNECTING;
      renderLive();
    };
  }

  function applyFiltersFromControls() {
    state.query.model = $('filterModel').value.trim();
    state.query.endpoint = $('filterEndpoint').value;
    state.query.status = $('filterStatus').value;
    state.query.streaming = $('filterStreaming').value;
    state.query.error_code = $('filterError').value;
    state.query.metric = $('metricSel').value;
    writeQuery(state.query, true);
    state.seen.clear();
    refreshUsage();
    refreshEvents(true);
    openStream();
  }

  function setRange(range) {
    state.query.range = range;
    if (range !== 'CUSTOM') {
      state.query.from = '';
      state.query.to = '';
    }
    writeQuery(state.query, true);
    fillControls();
    if (range === 'CUSTOM') return;
    state.seen.clear();
    refreshUsage();
    refreshEvents(true);
    openStream();
  }

  function applyCustom() {
    const err = validateCustom($('customFrom').value, $('customTo').value);
    if (err) { banner('dashError', err); return; }
    state.query.range = 'CUSTOM';
    state.query.from = String(localToUnix($('customFrom').value));
    state.query.to = String(localToUnix($('customTo').value));
    writeQuery(state.query, true);
    banner('dashError', '');
    state.seen.clear();
    refreshUsage();
    refreshEvents(true);
    openStream();
  }

  async function enterDash() {
    show($('gate'), false);
    show($('dash'), true);
    fillControls();
    const me = await api('/me');
    if (!me.ok) {
      show($('gate'), true);
      show($('dash'), false);
      return false;
    }
    await refreshAll();
    openStream();
    if (!state.clock) {
      state.clock = setInterval(() => {
        if (state.sse === SSE.LIVE && state.lastEventAt && Date.now() - state.lastEventAt > STALE_MS) {
          state.sse = SSE.STALE;
        }
        renderLive();
      }, 1000);
    }
    if (!state.reconcile) {
      state.reconcile = setInterval(() => {
        if (state.sse !== SSE.SESSION_EXPIRED) refreshSummary();
      }, RECONCILE_MS);
    }
    return true;
  }

  function wire() {
    $('gateBtn').onclick = async () => {
      banner('gateError', '');
      const key = $('gateKey').value.trim();
      if (!key) { banner('gateError', t('portal.keyRequired')); return; }
      const res = await api('/session', { method: 'POST', body: JSON.stringify({ key }) });
      const d = await readJSON(res);
      $('gateKey').value = '';
      if (!res.ok) { banner('gateError', errMessage(d, t('portal.err.invalid_api_key'))); return; }
      enterDash();
    };
    $('gateKey').addEventListener('keydown', (e) => {
      if (e.key === 'Enter') $('gateBtn').click();
    });
    $('signOutBtn').onclick = async () => {
      await api('/session', { method: 'DELETE' });
      closeStream();
      state.sse = SSE.OFFLINE;
      show($('dash'), false);
      show($('gate'), true);
    };
    $('rangeBar').addEventListener('click', (e) => {
      const btn = e.target.closest('[data-range]');
      if (btn) setRange(btn.getAttribute('data-range'));
    });
    $('customApply').onclick = applyCustom;
    $('filterModel').onchange = applyFiltersFromControls;
    $('filterEndpoint').onchange = applyFiltersFromControls;
    $('filterStatus').onchange = applyFiltersFromControls;
    $('filterStreaming').onchange = applyFiltersFromControls;
    $('filterError').onchange = applyFiltersFromControls;
    $('metricSel').onchange = () => {
      state.query.metric = $('metricSel').value;
      writeQuery(state.query, true);
      renderChart();
    };
    $('clearFilters').onclick = () => {
      state.query.model = '';
      state.query.endpoint = '';
      state.query.status = '';
      state.query.streaming = '';
      state.query.error_code = '';
      writeQuery(state.query, true);
      fillControls();
      state.seen.clear();
      refreshUsage();
      refreshEvents(true);
      openStream();
    };
    $('loadMore').onclick = loadMore;
    const onLang = async (e) => {
      lang = e.target.value;
      localStorage.setItem('kiro_lang', lang);
      await loadI18n();
      fillControls();
      renderHeader();
      renderSummary();
      renderChart();
      renderEvents();
      renderLive();
    };
    $('gateLang').onchange = onLang;
    $('dashLang').onchange = onLang;
    window.addEventListener('popstate', () => {
      state.query = readQuery();
      fillControls();
      state.seen.clear();
      if (!$('dash').classList.contains('hidden')) {
        refreshUsage();
        refreshEvents(true);
        openStream();
      }
    });
    window.addEventListener('online', () => { if (state.sse !== SSE.SESSION_EXPIRED) openStream(); });
    window.addEventListener('offline', () => { state.sse = SSE.OFFLINE; renderLive(); });
  }

  async function boot() {
    applyTheme();
    await loadI18n();
    fillControls();
    wire();
    if (new URLSearchParams(location.search).get('error') === 'invalid') {
      banner('gateError', t('portal.invalid'));
    }
    const me = await api('/me');
    if (me.ok) enterDash();
    else {
      show($('gate'), true);
      show($('dash'), false);
    }
  }

  boot();
})();
