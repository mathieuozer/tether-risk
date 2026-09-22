/*
 * Risk screening Mini App.
 *
 * Plain JS, no build step. Talks to /app/api/* (docs/APP_API.md) with
 * "Authorization: tma <initData>". Outside Telegram, or with ?mock=1, every
 * endpoint is answered in memory (see mock section at the bottom) so the UI
 * can be previewed in a normal browser.
 *
 * Rendering is string templates into #view, with one delegated click handler
 * dispatching on data-act and one submit handler dispatching on data-form.
 * Everything user- or server-provided goes through esc().
 */
(function () {
  'use strict';

  var tg = (window.Telegram && window.Telegram.WebApp) || null;
  var qs = new URLSearchParams(location.search);
  var INIT_DATA = (tg && tg.initData) || '';
  var MOCK = !INIT_DATA || qs.get('mock') === '1';
  var IN_TG = !!(tg && tg.platform && tg.platform !== 'unknown');
  var API_BASE = '/app/api';
  var REQUEST_TIMEOUT = 30000;
  var SCREEN_TIMEOUT = 120000;

  // Same as categoryOrder / riskChecks in internal/report/connections.go.
  var CATEGORY_ORDER = ['sanctions', 'terrorist_financing', 'darknet', 'stolen_funds', 'frozen_funds', 'mixer', 'scam',
    'high_risk_exchange', 'gambling', 'named_service', 'unnamed_service', 'dust', 'dex', 'exchange'];
  var RISK_CHECKS = ['sanctions', 'terrorist_financing', 'darknet', 'stolen_funds', 'frozen_funds',
    'mixer', 'scam', 'high_risk_exchange', 'gambling'];
  var SEVERE = ['sanctions', 'terrorist_financing', 'darknet', 'stolen_funds', 'frozen_funds', 'mixer', 'scam'];
  var ELEVATED = ['high_risk_exchange', 'gambling'];
  var MIN_LISTED = 0.1;
  var ENTRIES_SHOWN = 5;
  var UPGRADE_CODES = ['no_plan', 'feature_locked', 'limit_reached'];
  var NO_RETRY_CODES = ['bad_address', 'chain_unavailable', 'unauthorized', 'not_found'];

  var S = {
    lang: 'en',
    me: null,
    boot: 'loading', // loading | ready | error
    bootError: null,
    tab: 'screen',
    view: 'screen', // screen | result | history | watches | account
    input: '',
    chain: '',
    screening: false,
    screenStarted: 0,
    screenError: null,
    result: null,
    dir: 'inbound',
    entriesAll: {},
    breakdown: false,
    minorOpen: false,
    res: { history: blank(), watches: blank(), keys: blank() },
    busy: {},
    watchAddr: '',
    watchLabel: '',
    watchError: null,
    batchText: '',
    keyName: '',
    newKey: null,
    sheet: null
  };

  function blank() { return { data: null, loading: false, error: null }; }

  /* ------------------------------------------------------------------ */
  /* Utilities                                                           */
  /* ------------------------------------------------------------------ */

  function $(sel) { return document.querySelector(sel); }

  function esc(v) {
    return String(v == null ? '' : v)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  function has(list, v) { return list.indexOf(v) !== -1; }

  function hasKey(key) {
    var d = window.I18N[S.lang] || window.I18N.en;
    return d[key] != null || window.I18N.en[key] != null;
  }

  function t(key, vars) {
    var d = window.I18N[S.lang] || window.I18N.en;
    var s = d[key] != null ? d[key] : window.I18N.en[key];
    if (s == null) return key;
    if (vars) {
      s = s.replace(/\{(\w+)\}/g, function (m, k) { return vars[k] != null ? vars[k] : m; });
    }
    return s;
  }
  function tt(key, vars) { return esc(t(key, vars)); }
  // Plural: key_one / key_other, with {n} set to the formatted count.
  // Russian adds key_few for 2-4 (not 12-14); its _other is the form after 5.
  function tn(key, n, vars) {
    var v = Object.assign({ n: fmtInt(n) }, vars || {});
    var form = n === 1 ? '_one' : '_other';
    if (S.lang === 'ru') {
      var n10 = n % 10, n100 = n % 100;
      if (n10 === 1 && n100 !== 11) form = '_one';
      else if (n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14)) form = '_few';
    }
    return t(key + form, v);
  }

  function loc() { return { tr: 'tr-TR', ru: 'ru-RU' }[S.lang] || 'en-US'; }

  function fmtNum(v, digits) {
    return new Intl.NumberFormat(loc(), { minimumFractionDigits: digits, maximumFractionDigits: digits }).format(v);
  }
  function fmtInt(n) { return new Intl.NumberFormat(loc(), { maximumFractionDigits: 0 }).format(n || 0); }
  function fmtPct(v) {
    var s = fmtNum(v || 0, 1);
    return S.lang === 'tr' ? '%' + s : s + '%';
  }
  // A share, with trace amounts written as "under 0.1%" as the text report does.
  function fmtShare(v) {
    if (v > 0 && v < MIN_LISTED) return t('under_pct', { pct: fmtPct(MIN_LISTED) });
    return fmtPct(v);
  }
  function fmtUSD(v) {
    v = v || 0;
    if (v >= 1e9) return '$' + fmtNum(v / 1e9, 2) + 'B';
    if (v >= 1e6) return '$' + fmtNum(v / 1e6, 2) + 'M';
    if (v >= 1e3) return '$' + fmtNum(v / 1e3, 1) + 'k';
    return '$' + fmtNum(v, 0);
  }
  function fmtLimit(n) { return n < 0 ? t('unlimited') : fmtInt(n); }
  function parseDate(s) {
    if (!s) return null;
    var d = /^\d{4}-\d{2}-\d{2}$/.test(s) ? new Date(s + 'T00:00:00Z') : new Date(s);
    return isNaN(d.getTime()) ? null : d;
  }
  function fmtDate(s) {
    var d = parseDate(s);
    if (!d) return '';
    var o = { day: 'numeric', month: 'short', year: 'numeric' };
    if (/^\d{4}-\d{2}-\d{2}$/.test(s)) o.timeZone = 'UTC';
    return new Intl.DateTimeFormat(loc(), o).format(d);
  }
  function fmtDateTime(s) {
    var d = parseDate(s);
    if (!d) return '';
    return new Intl.DateTimeFormat(loc(), { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' }).format(d);
  }
  function fmtTime(s) {
    var d = parseDate(s);
    if (!d) return '';
    return new Intl.DateTimeFormat(loc(), { hour: '2-digit', minute: '2-digit' }).format(d);
  }
  function fmtRange(a, b) {
    if (!a) return '';
    return fmtDate(a) + ' → ' + fmtDate(b || a);
  }
  function fmtRel(s) {
    var d = parseDate(s);
    if (!d) return '';
    var diff = (d.getTime() - Date.now()) / 1000;
    var abs = Math.abs(diff);
    if (abs < 45) return t('just_now');
    var units = [['minute', 60], ['hour', 3600], ['day', 86400], ['week', 604800], ['month', 2592000], ['year', 31536000]];
    var unit = 'minute', size = 60;
    for (var i = 0; i < units.length; i++) {
      if (abs >= units[i][1]) { unit = units[i][0]; size = units[i][1]; }
    }
    var rtf = new Intl.RelativeTimeFormat(loc(), { numeric: 'auto' });
    return rtf.format(Math.round(diff / size), unit);
  }
  function fmtList(items) {
    if (window.Intl && Intl.ListFormat) {
      return new Intl.ListFormat(loc(), { style: 'long', type: 'conjunction' }).format(items);
    }
    return items.join(', ');
  }
  function short(a) {
    a = a || '';
    return a.length <= 12 ? a : a.slice(0, 6) + '…' + a.slice(-4);
  }
  function titleCase(s) { return s ? s.charAt(0).toUpperCase() + s.slice(1).toLowerCase() : s; }

  function catName(c) {
    if (hasKey('cat_' + c)) return t('cat_' + c);
    return titleCase(String(c || '').replace(/_/g, ' '));
  }
  function catTier(c) {
    if (has(SEVERE, c)) return 'severe';
    if (has(ELEVATED, c)) return 'elevated';
    return 'neutral';
  }
  function chainName(id) {
    if (S.me && S.me.chains) {
      for (var i = 0; i < S.me.chains.length; i++) if (S.me.chains[i].id === id) return S.me.chains[i].name;
    }
    return hasKey('chain_' + id) ? t('chain_' + id) : (id || '');
  }
  function reasonText(r) {
    return hasKey('reason_' + r) ? t('reason_' + r) : String(r || '').replace(/_/g, ' ');
  }
  // "Unidentified high-volume service" reads as "High-volume service": the
  // category line already says the operator is unnamed (entryName in Go).
  function entryName(e) {
    if (!e.entity) return catName(e.category);
    var m = /^Unidentified (.+)$/.exec(e.entity);
    if (m) {
      var k = 'ent_' + m[1].toLowerCase().replace(/[^a-z]+/g, '_');
      if (hasKey(k)) return t(k);
      return m[1].charAt(0).toUpperCase() + m[1].slice(1);
    }
    return e.entity;
  }
  function hopsText(n) { return n <= 1 ? t('hops_direct') : t('hops_away', { n: fmtInt(n) }); }

  function icon(name, cls) {
    return '<svg class="i' + (cls ? ' ' + cls : '') + '" viewBox="0 0 24 24" aria-hidden="true" focusable="false">' +
      (ICONS[name] || '') + '</svg>';
  }
  var ICONS = {
    search: '<circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/>',
    clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
    eye: '<path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>',
    user: '<circle cx="12" cy="8" r="4"/><path d="M4 21a8 8 0 0 1 16 0"/>',
    copy: '<rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/>',
    paste: '<rect x="8" y="3" width="8" height="4" rx="1"/><path d="M16 5h2a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2h2"/>',
    x: '<path d="M18 6 6 18M6 6l12 12"/>',
    lock: '<rect x="4.5" y="11" width="15" height="10" rx="2"/><path d="M8 11V7.5a4 4 0 0 1 8 0V11"/>',
    ok: '<circle cx="12" cy="12" r="9"/><path d="m8 12.2 2.8 2.8L16 9.5"/>',
    found: '<circle cx="12" cy="12" r="9"/><path d="M12 7.5v5.5"/><path d="M12 16.5h.01"/>',
    neutral: '<circle cx="12" cy="12" r="9"/><path d="M8.5 12h7"/>',
    info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v5"/><path d="M12 7.5h.01"/>',
    warn: '<path d="M10.3 3.9 2.4 17.5a2 2 0 0 0 1.7 3h15.8a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"/><path d="M12 9v4.5"/><path d="M12 17h.01"/>',
    ban: '<circle cx="12" cy="12" r="9"/><path d="m5.6 5.6 12.8 12.8"/>',
    progress: '<path d="M21 12a9 9 0 1 1-2.6-6.4L21 8"/><path d="M21 3v5h-5"/>',
    left: '<path d="m15 18-6-6 6-6"/>',
    right: '<path d="m9 18 6-6-6-6"/>',
    down: '<path d="m6 9 6 6 6-6"/>',
    file: '<path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/><path d="M9 13h6M9 17h4"/>',
    bell: '<path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9"/><path d="M10.3 21a1.94 1.94 0 0 0 3.4 0"/>',
    layers: '<path d="m12 3 9 5-9 5-9-5 9-5z"/><path d="m3 13 9 5 9-5"/>',
    trash: '<path d="M4 7h16"/><path d="M10 11v6M14 11v6"/><path d="M6 7l1 13a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-13"/><path d="M9 7V4h6v3"/>',
    plus: '<path d="M12 5v14M5 12h14"/>',
    star: '<path class="fill" d="M12 2.8l2.8 5.8 6.3.9-4.6 4.4 1.1 6.3L12 17.2l-5.6 3 1.1-6.3L2.9 9.5l6.3-.9z"/>',
    inbound: '<path d="M17 7 7 17"/><path d="M16 17H7V8"/>',
    outbound: '<path d="M7 17 17 7"/><path d="M8 7h9v9"/>',
    key: '<circle cx="7.5" cy="15.5" r="4.5"/><path d="m10.7 12.3 9.3-9.3M17 6l3 3M14 9l2 2"/>',
    globe: '<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3a14 14 0 0 1 0 18M12 3a14 14 0 0 0 0 18"/>',
    help: '<circle cx="12" cy="12" r="9"/><path d="M9.5 9.2a2.5 2.5 0 0 1 5 .3c0 1.6-2.5 2-2.5 3.5"/><path d="M12 17h.01"/>',
    list: '<path d="M9 6h12M9 12h12M9 18h12"/><path d="M4 6h.01M4 12h.01M4 18h.01"/>',
    shield: '<path d="M12 3 4.5 6v5.5c0 4.8 3.2 8.2 7.5 9.5 4.3-1.3 7.5-4.7 7.5-9.5V6z"/>',
    doc: '<path d="M7 3h10a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2z"/><path d="M9 8h6M9 12h6M9 16h3"/>'
  };

  function spinner(cls) { return '<span class="spinner' + (cls ? ' ' + cls : '') + '" aria-hidden="true"></span>'; }

  /* ------------------------------------------------------------------ */
  /* Telegram glue                                                       */
  /* ------------------------------------------------------------------ */

  function tgv(v) {
    try { return !!(tg && IN_TG && tg.isVersionAtLeast && tg.isVersionAtLeast(v)); } catch (_) { return false; }
  }

  function haptic(kind) {
    if (!tgv('6.1') || !tg.HapticFeedback) return;
    try {
      if (kind === 'select') tg.HapticFeedback.selectionChanged();
      else if (kind === 'success' || kind === 'error' || kind === 'warning') tg.HapticFeedback.notificationOccurred(kind);
      else tg.HapticFeedback.impactOccurred('light');
    } catch (_) { /* optional */ }
  }

  function confirmDialog(text) {
    return new Promise(function (resolve) {
      if (tgv('6.2') && tg.showConfirm) {
        try { tg.showConfirm(text, function (ok) { resolve(!!ok); }); return; } catch (_) { /* fall through */ }
      }
      resolve(window.confirm(text));
    });
  }

  function applyTheme() {
    var root = document.documentElement;
    var forced = MOCK ? qs.get('theme') : null;
    if (forced === 'dark' || forced === 'light') root.setAttribute('data-theme', forced);
    else if (IN_TG && tg.colorScheme) root.setAttribute('data-theme', tg.colorScheme);
    else root.removeAttribute('data-theme');
    if (tgv('6.1')) {
      try {
        tg.setHeaderColor('secondary_bg_color');
        tg.setBackgroundColor('secondary_bg_color');
        if (tgv('7.10') && tg.setBottomBarColor) tg.setBottomBarColor('secondary_bg_color');
      } catch (_) { /* optional */ }
    }
  }

  function syncTelegramButtons() {
    if (!IN_TG) return;
    if (tgv('6.1') && tg.BackButton) {
      if (S.sheet || S.view === 'result') tg.BackButton.show(); else tg.BackButton.hide();
    }
    var mb = tg.MainButton;
    if (!mb) return;
    var show = S.boot === 'ready' && S.view === 'screen' && !S.sheet && !S.screening && S.input.trim() !== '';
    if (show) {
      mb.setText(t('screen_btn'));
      mb.enable();
      mb.show();
    } else {
      mb.hide();
    }
  }

  function openLink(url) {
    if (/^https:\/\/t\.me\//.test(url) && tgv('6.1') && tg.openTelegramLink) { tg.openTelegramLink(url); return; }
    if (IN_TG && tg.openLink) { tg.openLink(url); return; }
    window.open(url, '_blank', 'noopener');
  }

  /* ------------------------------------------------------------------ */
  /* API                                                                 */
  /* ------------------------------------------------------------------ */

  function ApiError(code, message, status) {
    this.code = code;
    this.message = message || '';
    this.status = status || 0;
  }

  function api(method, path, body, timeout) {
    if (MOCK) return mockApi(method, path, body);
    var ctrl = new AbortController();
    var timer = setTimeout(function () { ctrl.abort(); }, timeout || REQUEST_TIMEOUT);
    var headers = { 'Authorization': 'tma ' + INIT_DATA };
    if (body) headers['Content-Type'] = 'application/json';
    return fetch(API_BASE + path, {
      method: method,
      headers: headers,
      body: body ? JSON.stringify(body) : undefined,
      signal: ctrl.signal,
      cache: 'no-store'
    }).then(function (res) {
      return res.text().then(function (text) {
        clearTimeout(timer);
        var data = null;
        try { data = text ? JSON.parse(text) : null; } catch (_) { /* non-JSON */ }
        if (!res.ok) {
          throw new ApiError((data && data.error) || ('http_' + res.status), data && data.message, res.status);
        }
        return data;
      });
    }, function (e) {
      clearTimeout(timer);
      throw new ApiError(e && e.name === 'AbortError' ? 'timeout' : 'network');
    });
  }

  function errText(e, ctx) {
    var code = (e && e.code) || 'unknown';
    if (ctx && hasKey('err_' + code + '_' + ctx)) return t('err_' + code + '_' + ctx);
    if (hasKey('err_' + code)) return t('err_' + code);
    if (/^http_5/.test(code)) return t('err_server');
    return (e && e.message) || t('err_unknown');
  }

  function isUpgrade(e) { return !!e && has(UPGRADE_CODES, e.code); }

  // retryAct is a data-act name; retryRes, when set, is passed as data-res.
  function errorNote(e, ctx, retryAct, retryRes) {
    var up = isUpgrade(e);
    var retry = retryAct && !up && !has(NO_RETRY_CODES, e.code);
    var actions = '';
    if (up) actions += '<button type="button" class="btn sm primary" data-act="upgrade">' + tt('upgrade') + '</button>';
    if (retry) {
      actions += '<button type="button" class="btn sm" data-act="' + retryAct + '"' + (retryRes ? ' data-res="' + retryRes + '"' : '') + '>' +
        icon('progress') + tt('retry') + '</button>';
    }
    return '<div class="note danger" role="alert">' + icon('warn') + '<div class="note-body"><p>' + esc(errText(e, ctx)) + '</p>' +
      (actions ? '<div class="note-actions">' + actions + '</div>' : '') + '</div></div>';
  }

  function toastError(e, ctx) {
    haptic('error');
    toast(errText(e, ctx), { kind: 'error', action: isUpgrade(e) ? { label: t('upgrade'), fn: goUpgrade } : null });
  }

  /* ------------------------------------------------------------------ */
  /* Toast and sheet                                                     */
  /* ------------------------------------------------------------------ */

  var toastTimer = null;
  function toast(msg, opts) {
    opts = opts || {};
    var el = $('#toast');
    var action = opts.action;
    el.className = 'toast show' + (opts.kind ? ' ' + opts.kind : '');
    el.innerHTML = '<span>' + esc(msg) + '</span>' +
      (action ? '<button type="button" class="toast-act">' + esc(action.label) + '</button>' : '');
    if (action) {
      el.querySelector('.toast-act').onclick = function () { hideToast(); action.fn(); };
    }
    clearTimeout(toastTimer);
    toastTimer = setTimeout(hideToast, action ? 6000 : 3200);
  }
  function hideToast() { var el = $('#toast'); el.className = 'toast'; }

  var sheetReturnFocus = null;
  function openSheet(sheet) {
    if (S.sheet && S.sheet.timer) clearInterval(S.sheet.timer);
    else sheetReturnFocus = document.activeElement;
    S.sheet = sheet;
    renderSheet();
    syncTelegramButtons();
    var h = document.querySelector('#sheet-root [data-autofocus]');
    if (h) h.focus();
  }
  function closeSheet() {
    if (S.sheet && S.sheet.timer) clearInterval(S.sheet.timer);
    var refresh = S.sheet && S.sheet.refreshOnClose;
    S.sheet = null;
    renderSheet();
    syncTelegramButtons();
    if (sheetReturnFocus && document.body.contains(sheetReturnFocus)) sheetReturnFocus.focus();
    if (refresh) refreshMe();
  }

  /* ------------------------------------------------------------------ */
  /* Navigation                                                          */
  /* ------------------------------------------------------------------ */

  function navigate(view) {
    S.view = view;
    if (view !== 'result') S.tab = view;
    render();
    window.scrollTo(0, 0);
    var main = $('#view');
    if (main) main.focus({ preventScroll: true });
    onEnter(view);
  }

  function onEnter(view) {
    if (view === 'history') loadRes('history');
    if (view === 'watches' && S.me && S.me.limits.watches !== 0) loadRes('watches');
    if (view === 'account') {
      refreshMe();
      if (S.me && S.me.limits.api) loadRes('keys');
    }
  }

  function goTab(tab) {
    if (tab === S.tab && S.view === tab) { window.scrollTo({ top: 0, behavior: 'smooth' }); return; }
    haptic('select');
    navigate(tab);
  }

  function goBack() {
    if (S.sheet) { closeSheet(); return; }
    if (S.view === 'result') navigate(S.tab || 'screen');
  }

  function goUpgrade() {
    if (S.sheet) closeSheet();
    navigate('account');
    var plans = document.getElementById('plans');
    if (plans) plans.scrollIntoView({ block: 'start' });
  }

  /* ------------------------------------------------------------------ */
  /* Data                                                                */
  /* ------------------------------------------------------------------ */

  function loadRes(name, force) {
    var r = S.res[name];
    if (r.loading) return;
    if (r.data && !force && r.fresh && Date.now() - r.fresh < 5000) return;
    r.loading = true;
    r.error = null;
    render();
    var path = name === 'history' ? '/history?limit=50' : name === 'watches' ? '/watches' : '/keys';
    api('GET', path).then(function (data) {
      r.data = data;
      r.fresh = Date.now();
    }, function (e) {
      r.error = e;
    }).then(function () {
      r.loading = false;
      render();
    });
  }

  function refreshMe() {
    return api('GET', '/me').then(function (me) {
      setMe(me);
      render();
    }, function () { /* keep what we have */ });
  }

  function setMe(me) {
    S.me = me;
    var l = me.user && me.user.lang;
    if (LANGS.indexOf(l) !== -1) S.lang = l;
    document.documentElement.lang = S.lang;
  }

  var LANGS = ['en', 'tr', 'ru'];
  // Telegram language codes answered in Russian, as the bot does.
  var RU_CLIENTS = ['ru', 'uk', 'be', 'kk', 'uz', 'ky', 'tg'];

  function initialLang() {
    var u = tg && tg.initDataUnsafe && tg.initDataUnsafe.user;
    var code = (u && u.language_code) || (MOCK ? (qs.get('lang') || navigator.language || '') : '');
    code = code.toLowerCase().split(/[-_]/)[0];
    if (code === 'tr') return 'tr';
    return RU_CLIENTS.indexOf(code) !== -1 ? 'ru' : 'en';
  }

  function boot() {
    S.boot = 'loading';
    S.bootError = null;
    render();
    api('GET', '/me').then(function (me) {
      setMe(me);
      S.boot = 'ready';
      render();
      loadRes('history');
      mockAutoOpen();
    }, function (e) {
      S.boot = 'error';
      S.bootError = e;
      render();
    });
  }

  /* ------------------------------------------------------------------ */
  /* Actions                                                             */
  /* ------------------------------------------------------------------ */

  var progressTimer = null;
  var PROGRESS_STEPS = [[0, 'prog_1'], [4, 'prog_2'], [12, 'prog_3'], [25, 'prog_4'], [45, 'prog_5']];

  function tickProgress() {
    var secs = Math.floor((Date.now() - S.screenStarted) / 1000);
    var key = PROGRESS_STEPS[0][1];
    for (var i = 0; i < PROGRESS_STEPS.length; i++) if (secs >= PROGRESS_STEPS[i][0]) key = PROGRESS_STEPS[i][1];
    var a = $('#progress-text'), b = $('#progress-time');
    if (a && a.textContent !== t(key)) a.textContent = t(key);
    if (b) b.textContent = t('elapsed', { n: fmtInt(secs) });
  }

  function runScreen(address, chain) {
    if (S.screening) return;
    address = (address || '').trim();
    if (!address) return;
    S.input = address;
    S.screening = true;
    S.screenError = null;
    S.screenStarted = Date.now();
    S.view = 'screen';
    S.tab = 'screen';
    haptic('impact');
    render();
    window.scrollTo(0, 0);
    tickProgress();
    progressTimer = setInterval(tickProgress, 1000);
    var body = { address: address };
    if (chain) body.chain = chain;
    api('POST', '/screen', body, SCREEN_TIMEOUT).then(function (data) {
      S.result = data.result;
      // The bot keeps tracing an unfinished screen and sends the final result to the chat.
      S.followUp = !!data.follow_up;
      if (data.usage && S.me) S.me.usage.screens_today = data.usage.screens_today;
      S.dir = hasEntries(data.result.inbound) || !hasEntries(data.result.outbound) ? 'inbound' : 'outbound';
      S.entriesAll = {};
      S.breakdown = false;
      S.minorOpen = false;
      S.res.history.fresh = 0;
      haptic('success');
      stopProgress();
      navigate('result');
      loadRes('history', true);
      if (S.me && S.me.limits.watches !== 0 && !S.res.watches.data) loadRes('watches');
    }, function (e) {
      S.screenError = e;
      haptic('error');
      stopProgress();
      render();
    });
  }

  function stopProgress() {
    clearInterval(progressTimer);
    S.screening = false;
  }

  function submitScreen() {
    if (!S.input.trim()) { var el = $('#addr'); if (el) el.focus(); return; }
    runScreen(S.input, S.chain);
  }

  function rescreen(address, chain, ask) {
    if (!ask) { S.chain = ''; runScreen(address, chain); return; }
    confirmDialog(t('confirm_rescreen', { addr: short(address) })).then(function (ok) {
      if (ok) { S.chain = ''; runScreen(address, chain); }
    });
  }

  function pasteAddress() {
    var done = function (text) {
      text = (text || '').trim();
      if (text) {
        S.input = text;
        var el = $('#addr');
        if (el) el.value = text;
        updateScreenForm();
        haptic('select');
      } else {
        toast(t('paste_blocked'));
        var inp = $('#addr');
        if (inp) inp.focus();
      }
    };
    var viaTelegram = function () {
      if (!(tgv('6.4') && tg.readTextFromClipboard)) { done(''); return; }
      var settled = false;
      var timer = setTimeout(function () { if (!settled) { settled = true; done(''); } }, 1500);
      try {
        tg.readTextFromClipboard(function (v) { if (!settled) { settled = true; clearTimeout(timer); done(v); } });
      } catch (_) { settled = true; clearTimeout(timer); done(''); }
    };
    if (navigator.clipboard && navigator.clipboard.readText) {
      navigator.clipboard.readText().then(function (v) { if (v && v.trim()) done(v); else viaTelegram(); }, viaTelegram);
    } else {
      viaTelegram();
    }
  }

  function copyText(text) {
    var ok = function () { haptic('select'); toast(t('copied')); };
    var fallback = function () {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      var done = false;
      try { done = document.execCommand('copy'); } catch (_) { /* ignore */ }
      ta.remove();
      if (done) ok(); else toast(t('copy_failed'), { kind: 'error' });
    };
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(ok, fallback);
    else fallback();
  }

  function withBusy(key, promise) {
    S.busy[key] = true;
    render();
    return promise.then(function (v) { S.busy[key] = false; render(); return v; },
      function (e) { S.busy[key] = false; render(); throw e; });
  }

  function sendPdf() {
    var r = S.result;
    if (!r || S.busy.pdf) return;
    withBusy('pdf', api('POST', '/report', { address: r.address, chain: r.chain }, SCREEN_TIMEOUT)).then(function () {
      haptic('success');
      toast(t('pdf_sent'), { kind: 'ok' });
    }, function (e) { toastError(e, 'pdf'); });
  }

  function isWatched(address) {
    var w = S.res.watches.data;
    if (!w || !w.items) return false;
    for (var i = 0; i < w.items.length; i++) if (w.items[i].address === address) return true;
    return false;
  }

  function watchResult() {
    var r = S.result;
    if (!r || S.busy.watch || isWatched(r.address)) return;
    withBusy('watch', api('POST', '/watches', { address: r.address, chain: r.chain })).then(function (w) {
      addWatchLocal(w);
      haptic('success');
      toast(t('watch_added'), { kind: 'ok' });
    }, function (e) { toastError(e, 'watch'); });
  }

  function addWatchLocal(w) {
    var d = S.res.watches.data;
    if (d && d.items) d.items.unshift(w);
    if (S.me) S.me.usage.watches = (S.me.usage.watches || 0) + 1;
  }

  function addWatch() {
    var address = S.watchAddr.trim();
    if (!address || S.busy.addWatch) return;
    S.watchError = null;
    var body = { address: address };
    var label = S.watchLabel.trim();
    if (label) body.label = label;
    withBusy('addWatch', api('POST', '/watches', body)).then(function (w) {
      S.watchAddr = '';
      S.watchLabel = '';
      addWatchLocal(w);
      haptic('success');
      toast(t('watch_added'), { kind: 'ok' });
      render();
    }, function (e) {
      S.watchError = e;
      haptic('error');
      render();
    });
  }

  function removeWatch(id) {
    var d = S.res.watches.data;
    var w = null;
    if (d) for (var i = 0; i < d.items.length; i++) if (String(d.items[i].id) === String(id)) w = d.items[i];
    var name = w ? (w.label || short(w.address)) : '';
    confirmDialog(t('confirm_unwatch', { name: name })).then(function (ok) {
      if (!ok) return;
      withBusy('rm' + id, api('DELETE', '/watches/' + encodeURIComponent(id))).then(function () {
        if (d) d.items = d.items.filter(function (x) { return String(x.id) !== String(id); });
        if (S.me) S.me.usage.watches = Math.max(0, (S.me.usage.watches || 1) - 1);
        haptic('success');
        toast(t('watch_removed'));
      }, function (e) { toastError(e, 'watch'); });
    });
  }

  function payStars(planId) {
    var key = 'stars:' + planId;
    if (S.busy[key]) return;
    withBusy(key, api('POST', '/invoice/stars', { plan: planId })).then(function (data) {
      var onStatus = function (status) {
        if (status === 'paid') {
          haptic('success');
          toast(t('pay_success'), { kind: 'ok' });
          refreshMe();
        } else if (status === 'failed') {
          haptic('error');
          toast(t('pay_failed'), { kind: 'error' });
        } else if (status === 'pending') {
          toast(t('pay_pending'));
        }
      };
      if (MOCK) {
        confirmDialog(t('mock_pay')).then(function (ok) {
          if (ok) mockGrant(planId, 'stars');
          onStatus(ok ? 'paid' : 'cancelled');
        });
      } else if (tgv('6.1') && tg.openInvoice) {
        tg.openInvoice(data.link, onStatus);
      } else {
        openLink(data.link);
      }
    }, function (e) { toastError(e, 'pay'); });
  }

  function payUsdt(planId) {
    var key = 'usdt:' + planId;
    if (S.busy[key]) return;
    withBusy(key, api('POST', '/invoice/usdt', { plan: planId })).then(function (inv) {
      var plan = findPlan(planId);
      openSheet({ type: 'usdt', inv: inv, plan: plan, refreshOnClose: true });
      S.sheet.timer = setInterval(tickCountdown, 1000);
      tickCountdown();
    }, function (e) { toastError(e, 'pay'); });
  }

  function tickCountdown() {
    if (!S.sheet || S.sheet.type !== 'usdt') return;
    var end = parseDate(S.sheet.inv.expires_at);
    var left = end ? Math.max(0, Math.floor((end.getTime() - Date.now()) / 1000)) : 0;
    var el = $('#usdt-countdown');
    if (left <= 0) {
      if (!S.sheet.expired) { S.sheet.expired = true; clearInterval(S.sheet.timer); renderSheet(); }
      return;
    }
    var m = Math.floor(left / 60), s = left % 60;
    var txt = (m >= 60 ? Math.floor(m / 60) + ':' + pad(m % 60) : m) + ':' + pad(s);
    if (el) el.textContent = txt;
  }
  function pad(n) { return n < 10 ? '0' + n : String(n); }

  function parseBatch(text) {
    var seen = {};
    var out = [];
    String(text || '').split(/[\s,;]+/).forEach(function (a) {
      a = a.trim();
      if (a && !seen[a]) { seen[a] = true; out.push(a); }
    });
    return out;
  }

  function sendBatch() {
    var list = parseBatch(S.batchText);
    var max = S.me.limits.batch;
    if (!list.length || S.busy.batch) return;
    if (max >= 0 && list.length > max) { toast(t('batch_too_many', { n: fmtInt(max) }), { kind: 'error' }); return; }
    withBusy('batch', api('POST', '/batch', { addresses: list })).then(function (data) {
      S.batchText = '';
      haptic('success');
      toast(tn('batch_sent', (data && data.accepted) || list.length), { kind: 'ok' });
      render();
      refreshMe();
    }, function (e) { toastError(e, 'batch'); });
  }

  function createKey() {
    var name = S.keyName.trim();
    if (!name || S.busy.key) return;
    withBusy('key', api('POST', '/keys', { name: name })).then(function (k) {
      S.newKey = k;
      S.keyName = '';
      haptic('success');
      loadRes('keys', true);
    }, function (e) { toastError(e, 'api'); });
  }

  function revokeKey(id) {
    var d = S.res.keys.data;
    var k = null;
    if (d) for (var i = 0; i < d.items.length; i++) if (String(d.items[i].id) === String(id)) k = d.items[i];
    confirmDialog(t('confirm_revoke', { name: k ? k.name : '' })).then(function (ok) {
      if (!ok) return;
      withBusy('rk' + id, api('DELETE', '/keys/' + encodeURIComponent(id))).then(function () {
        if (d) d.items = d.items.filter(function (x) { return String(x.id) !== String(id); });
        haptic('success');
        toast(t('key_revoked'));
      }, function (e) { toastError(e, 'api'); });
    });
  }

  function setLang(lang) {
    if (lang === S.lang || S.busy.lang) return;
    var prev = S.lang;
    S.lang = lang;
    document.documentElement.lang = lang;
    haptic('select');
    withBusy('lang', api('POST', '/lang', { lang: lang })).then(function () {
      if (S.me && S.me.user) S.me.user.lang = lang;
    }, function (e) {
      S.lang = prev;
      document.documentElement.lang = prev;
      render();
      toastError(e);
    });
  }

  function openSupport() {
    var s = (S.me && S.me.support) || '';
    if (!s) return;
    if (/^https?:\/\//.test(s)) openLink(s);
    else if (s.charAt(0) === '@') openLink('https://t.me/' + s.slice(1));
    else if (/^[\w.+-]+@[\w-]+\.[\w.]+$/.test(s)) location.href = 'mailto:' + s;
    else openLink('https://t.me/' + s);
  }

  function findPlan(id) {
    var ps = (S.me && S.me.plans) || [];
    for (var i = 0; i < ps.length; i++) if (ps[i].id === id) return ps[i];
    return null;
  }

  function lockedToast(feature) {
    haptic('warning');
    toast(t('locked_' + feature), { action: { label: t('upgrade'), fn: goUpgrade } });
  }

  var ACTIONS = {
    tab: function (el) { goTab(el.getAttribute('data-tab')); },
    back: goBack,
    paste: pasteAddress,
    clear: function () {
      S.input = '';
      S.screenError = null;
      var el = $('#addr');
      if (el) { el.value = ''; el.focus(); }
      updateScreenForm();
    },
    chain: function (el) { S.chain = el.getAttribute('data-chain'); haptic('select'); render(); },
    rescreen: function (el) { rescreen(el.getAttribute('data-address'), el.getAttribute('data-chain'), el.getAttribute('data-confirm') === '1'); },
    retryScreen: submitScreen,
    retryBoot: boot,
    upgrade: goUpgrade,
    copy: function (el) { copyText(el.getAttribute('data-copy')); },
    dir: function (el) { S.dir = el.getAttribute('data-dir'); haptic('select'); render(); },
    moreEntries: function (el) { S.entriesAll[el.getAttribute('data-dir')] = true; render(); },
    pdf: sendPdf,
    watchResult: watchResult,
    breakdown: function () { S.breakdown = !S.breakdown; haptic('select'); render(); },
    locked: function (el) { lockedToast(el.getAttribute('data-feature')); },
    showResult: function () { if (S.result) navigate('result'); },
    goHistory: function () { navigate('history'); },
    reload: function (el) { loadRes(el.getAttribute('data-res'), true); },
    removeWatch: function (el) { removeWatch(el.getAttribute('data-id')); },
    payStars: function (el) { payStars(el.getAttribute('data-plan')); },
    payUsdt: function (el) { payUsdt(el.getAttribute('data-plan')); },
    closeSheet: closeSheet,
    revokeKey: function (el) { revokeKey(el.getAttribute('data-id')); },
    dismissKey: function () { S.newKey = null; render(); },
    lang: function (el) { setLang(el.getAttribute('data-lang')); },
    support: openSupport,
    closeApp: function () { if (IN_TG) tg.close(); }
  };

  var FORMS = {
    screen: submitScreen,
    watch: addWatch,
    batch: sendBatch,
    key: createKey
  };

  /* ------------------------------------------------------------------ */
  /* Result math (mirrors internal/report/connections.go)                */
  /* ------------------------------------------------------------------ */

  function traced(d) { return d && d.total_traced > 0 ? d.total_traced : 0; }
  function hasEntries(d) { return !!(d && d.connections && d.connections.length); }
  function truncated(d) { return !!(d && d.traversal && (d.traversal.fanout_capped || d.traversal.hop_limit_reached)); }

  // combine(): both directions weighted by total_traced, sorted largest first,
  // ties by name, so the combined unattributed share equals 100% - coverage.
  function combine(dirs) {
    var total = 0;
    dirs.forEach(function (d) { total += traced(d); });
    if (total <= 0) return { shares: [], unattributed: 0, total: 0 };
    var by = {};
    var un = 0;
    dirs.forEach(function (d) {
      if (!traced(d)) return;
      var w = d.total_traced / total;
      (d.categories || []).forEach(function (c) { by[c.category] = (by[c.category] || 0) + c.pct * w; });
      un += (d.unattributed_pct || 0) * w;
    });
    var shares = Object.keys(by).map(function (c) { return { category: c, pct: by[c] }; });
    shares.sort(function (a, b) {
      if (a.pct !== b.pct) return b.pct - a.pct;
      return a.category < b.category ? -1 : a.category > b.category ? 1 : 0;
    });
    return { shares: shares, unattributed: un, total: total };
  }

  function combineReasons(dirs) {
    var total = 0;
    dirs.forEach(function (d) { total += traced(d); });
    if (total <= 0) return [];
    var by = {};
    var order = [];
    dirs.forEach(function (d) {
      if (!traced(d)) return;
      var w = d.total_traced / total;
      (d.unattributed_reasons || []).forEach(function (r) {
        if (!(r.reason in by)) { by[r.reason] = 0; order.push(r.reason); }
        by[r.reason] += r.pct * w;
      });
    });
    var out = order.map(function (r, i) { return { reason: r, pct: by[r], i: i }; });
    out.sort(function (a, b) { return b.pct - a.pct || a.i - b.i; });
    return out;
  }

  function notFound(shares) {
    var seen = {};
    shares.forEach(function (s) { if (s.pct > 0) seen[s.category] = true; });
    return CATEGORY_ORDER.filter(function (c) { return !seen[c]; });
  }

  /* ------------------------------------------------------------------ */
  /* Shared pieces                                                       */
  /* ------------------------------------------------------------------ */

  function bandPill(band, compact) {
    if (!band) return '<span class="pill band-none">' + tt('band_pdf') + '</span>';
    var known = has(['low', 'medium', 'high'], band);
    var cls = known ? band : 'none';
    var label = known ? t((compact ? 'band_short_' : 'band_') + band) : titleCase(band);
    return '<span class="pill band-' + cls + '"><span class="dot" aria-hidden="true"></span>' + esc(label) + '</span>';
  }

  function copyBtn(text, label) {
    return '<button type="button" class="icon-btn" data-act="copy" data-copy="' + esc(text) + '" aria-label="' +
      esc(label || t('copy')) + '" title="' + esc(label || t('copy')) + '">' + icon('copy') + '</button>';
  }

  function addrChip(address) {
    return '<span class="addr"><span class="mono" title="' + esc(address) + '">' + esc(short(address)) + '</span>' +
      copyBtn(address, t('copy_address')) + '</span>';
  }

  function meter(value, max, cls) {
    var pct = max > 0 ? Math.min(100, Math.max(0, value / max * 100)) : 0;
    return '<div class="meter' + (cls ? ' ' + cls : '') + '" aria-hidden="true"><span style="width:' + pct.toFixed(2) + '%"></span></div>';
  }

  function sectionHead(title, sub, extra) {
    return '<div class="card-head"><div><h2>' + esc(title) + '</h2>' + (sub ? '<p class="hint">' + esc(sub) + '</p>' : '') +
      '</div>' + (extra || '') + '</div>';
  }

  function skeletonRows(n) {
    var out = '';
    for (var i = 0; i < n; i++) out += '<div class="sk-row"><span class="sk sk-pill"></span><span class="sk sk-line"></span><span class="sk sk-num"></span></div>';
    return '<div class="card" aria-busy="true" aria-label="' + tt('loading') + '">' + out + '</div>';
  }

  function usageBlock() {
    var me = S.me;
    var used = me.usage.screens_today || 0;
    var max = me.limits.daily_screens;
    var resets = me.usage.resets_at ? t('resets_at', { time: fmtTime(me.usage.resets_at) }) : '';
    if (max < 0) {
      return '<div class="usage"><div class="usage-row"><span>' + esc(tn('screens_today_unl', used)) + '</span>' +
        '<span class="hint">' + tt('unlimited') + '</span></div></div>';
    }
    var cls = max > 0 && used >= max ? 'full' : max > 0 && used / max >= 0.8 ? 'near' : '';
    return '<div class="usage"><div class="usage-row"><span>' + tt('screens_of', { n: fmtInt(used), m: fmtInt(max) }) + '</span>' +
      '<span class="hint">' + esc(resets) + '</span></div>' + meter(used, max, cls) + '</div>';
  }

  function mockBadge() {
    return MOCK ? '<div class="mock-badge" role="note">' + tt('mock_badge') + '</div>' : '';
  }

  function pageHead(title, sub, extra) {
    return '<header class="page-head">' + mockBadge() + '<div class="page-title"><h1>' + esc(title) + '</h1>' + (extra || '') + '</div>' +
      (sub ? '<p class="hint">' + esc(sub) + '</p>' : '') + '</header>';
  }

  function noPlanNote() {
    return '<div class="note warn">' + icon('info') + '<div class="note-body"><p><strong>' + tt('no_plan_title') + '</strong></p>' +
      '<p>' + tt('no_plan_body') + '</p><div class="note-actions"><button type="button" class="btn sm primary" data-act="upgrade">' +
      tt('see_plans') + '</button></div></div></div>';
  }

  /* ------------------------------------------------------------------ */
  /* Views                                                               */
  /* ------------------------------------------------------------------ */

  function viewBoot() {
    if (S.boot === 'error') {
      var e = S.bootError;
      var unauthorized = e && e.code === 'unauthorized';
      return '<div class="boot">' + icon('shield', 'boot-mark') + '<h1>' + tt('app_name') + '</h1>' +
        '<p class="hint">' + esc(errText(e)) + '</p>' +
        (unauthorized && IN_TG
          ? '<button type="button" class="btn primary" data-act="closeApp">' + tt('close') + '</button>'
          : '<button type="button" class="btn primary" data-act="retryBoot">' + icon('progress') + tt('retry') + '</button>') +
        '</div>';
    }
    return '<div class="boot" aria-busy="true">' + icon('shield', 'boot-mark') + '<p class="hint">' + tt('loading') + '</p>' + spinner() + '</div>';
  }

  function viewScreen() {
    var me = S.me;
    var chains = me.chains || [];
    var enabled = chains.filter(function (c) { return c.enabled; });
    var soon = chains.filter(function (c) { return !c.enabled; });
    var chainUI = '';
    if (enabled.length > 1) {
      var opts = [{ id: '', name: t('chain_auto') }].concat(enabled);
      chainUI = '<div class="field"><span class="label" id="chain-label">' + tt('chain_label') + '</span>' +
        '<div class="chip-row" role="radiogroup" aria-labelledby="chain-label">' +
        opts.map(function (o) {
          var on = S.chain === o.id;
          return '<button type="button" class="chip" role="radio" aria-checked="' + on + '" data-act="chain" data-chain="' +
            esc(o.id) + '" id="chain-' + esc(o.id || 'auto') + '"' + (S.screening ? ' disabled' : '') + '>' + esc(o.name) + '</button>';
        }).join('') +
        soon.map(function (c) {
          return '<span class="chip disabled" aria-disabled="true">' + esc(c.name) + ' <small>' + tt('soon') + '</small></span>';
        }).join('') + '</div></div>';
    } else if (chains.length) {
      var line = t('supports', { chains: fmtList(enabled.map(function (c) { return c.name; })) });
      if (soon.length) line += ' ' + t('coming_soon_list', { chains: fmtList(soon.map(function (c) { return c.name; })) });
      chainUI = '<p class="hint small">' + esc(line) + '</p>';
    }

    var filled = S.input.trim() !== '';
    var canPaste = !!((navigator.clipboard && navigator.clipboard.readText) || tgv('6.4'));
    var inputBtn = filled
      ? '<button type="button" class="input-btn" data-act="clear" id="addr-btn" aria-label="' + tt('clear') + '"' + (S.screening ? ' disabled' : '') + '>' + icon('x') + '</button>'
      : canPaste ? '<button type="button" class="input-btn" data-act="paste" id="addr-btn" aria-label="' + tt('paste') + '">' + icon('paste') + '<span>' + tt('paste') + '</span></button>' : '';

    var html = pageHead(t('screen_title'), t('screen_sub'));
    if (!me.access) html += noPlanNote();
    html += '<form class="card form" data-form="screen" novalidate>' +
      '<label class="label" for="addr">' + tt('address_label') + '</label>' +
      '<div class="input-wrap">' +
      '<input id="addr" class="input mono" data-bind="input" value="' + esc(S.input) + '" placeholder="' + tt('address_ph') +
      '" autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false" enterkeyhint="go"' + (S.screening ? ' disabled' : '') + '>' +
      inputBtn + '</div>' + chainUI +
      '<button type="submit" class="btn primary block" id="screen-btn"' + (!filled || S.screening ? ' disabled' : '') + '>' +
      (S.screening ? spinner('on-accent') + tt('screening') : icon('search') + tt('screen_btn')) + '</button>' +
      usageBlock() + '</form>';

    if (S.screening) html += progressCard();
    if (S.screenError) html += errorNote(S.screenError, 'screen', 'retryScreen');

    if (S.result && !S.screening) {
      var r = S.result;
      html += '<button type="button" class="card row-card" data-act="showResult">' + bandPill(r.band, true) +
        '<span class="row-main"><span class="row-title">' + tt('last_result') + '</span><span class="hint mono">' + esc(short(r.address)) + '</span></span>' +
        '<span class="row-end"><span class="score-sm">' + esc(fmtNum(r.score, 1)) + '</span>' + icon('right') + '</span></button>';
    }

    var h = S.res.history;
    var items = (h.data && h.data.items) || [];
    if (items.length) {
      html += '<section class="section"><div class="section-head"><h2>' + tt('recent') + '</h2>' +
        '<button type="button" class="link-btn" data-act="goHistory">' + tt('see_all') + '</button></div>' +
        '<ul class="card list">' + items.slice(0, 3).map(function (it) { return historyRow(it, false); }).join('') + '</ul>' +
        '<p class="hint small">' + tt('recent_hint') + '</p></section>';
    }
    return html;
  }

  function progressCard() {
    return '<section class="card progress" role="status" aria-live="polite">' +
      '<div class="progress-top">' + spinner('lg') + '<div><p class="progress-text" id="progress-text">' + tt('prog_1') + '</p>' +
      '<p class="hint small"><span id="progress-time"></span> · ' + tt('prog_hint') + '</p></div></div>' +
      '<div class="sk-block"><span class="sk sk-line w60"></span><span class="sk sk-line w90"></span><span class="sk sk-line w75"></span></div></section>';
  }

  function historyRow(it, confirm) {
    var sub = chainName(it.chain) + ' · ' + fmtRel(it.created_at);
    return '<li><button type="button" class="row" data-act="rescreen" data-address="' + esc(it.address) + '" data-chain="' +
      esc(it.chain || '') + '"' + (confirm ? ' data-confirm="1"' : '') + ' aria-label="' + tt('rescreen_aria', { addr: short(it.address) }) + '">' +
      bandPill(it.band, true) +
      '<span class="row-main"><span class="mono row-title">' + esc(short(it.address)) + '</span><span class="hint small">' + esc(sub) + '</span></span>' +
      '<span class="row-end">' + (it.score != null ? '<span class="score-sm">' + esc(fmtNum(it.score, 1)) + '</span>' : '') + icon('right') + '</span>' +
      '</button></li>';
  }

  function viewResult() {
    var r = S.result;
    var dirs = [r.inbound, r.outbound];
    var c = combine(dirs);
    var html = '';
    if (!IN_TG) {
      html += '<button type="button" class="back-link" data-act="back">' + icon('left') + tt('back') + '</button>';
    }
    html += mockBadge();
    html += resultBanners(r);
    html += verdictCard(r);
    html += summaryCard(r);
    html += actionsRow(r);
    html += depthNotes(r);
    html += connectionsCard(r, c, dirs);
    if (c.total > 0) html += checksCard(r, c.shares);
    html += flagsCard(r);
    html += entriesCard(r);
    html += activityCard(r);
    html += breakdownBlock(r);
    // The API's disclaimer is English; show its meaning in the reader's language.
    if (r.disclaimer) html += '<p class="disclaimer">' + esc(tt('disclaimer')) + '</p>';
    return html;
  }

  function resultBanners(r) {
    var out = '';
    if (r.own_label) {
      var name = r.own_label.imitates ? tt('poisoning_entity', { addr: short(r.own_label.imitates) })
        : (r.own_label.entity || catName(r.own_label.category));
      out += '<div class="note danger strong" role="alert">' + icon('ban') + '<div class="note-body"><p><strong>' +
        tt('listed_title') + '</strong></p><p>' + tt('listed_body', { name: name, category: catName(r.own_label.category) }) + '</p></div></div>';
    }
    if (r.sanctions_override) {
      out += '<div class="note danger strong" role="alert">' + icon('ban') + '<div class="note-body"><p><strong>' +
        tt('sanctions_title') + '</strong></p><p>' + tt('sanctions_body') + '</p></div></div>';
    }
    if (r.low_confidence) {
      out += '<div class="note warn" role="note">' + icon('warn') + '<div class="note-body"><p><strong>' + tt('lowconf_title') +
        '</strong></p><p>' + tt('lowconf_body', { pct: fmtPct(r.coverage * 100) }) + '</p></div></div>';
    }
    if (r.band_capped_by_abuse_rule) {
      out += '<div class="note info" role="note">' + icon('info') + '<div class="note-body"><p>' + tt('band_capped') + '</p></div></div>';
    }
    return out;
  }

  function summaryCard(r) {
    var band = has(['low', 'medium', 'high'], r.band) ? r.band : 'none';
    var cov = (r.coverage || 0) * 100;
    return '<section class="card summary" aria-labelledby="sum-title">' +
      '<div class="summary-top"><div><h1 class="eyebrow" id="sum-title">' + tt('risk_level') + '</h1>' + bandPill(r.band) + '</div>' +
      '<span class="chain-tag">' + esc(chainName(r.chain)) + '</span></div>' +
      '<div class="score" aria-label="' + tt('score_aria', { score: fmtNum(r.score, 1) }) + '"><span class="score-num">' + esc(fmtNum(r.score, 1)) +
      '</span><span class="score-den">/ 100</span></div>' +
      '<div class="scorebar band-' + band + '" aria-hidden="true"><span style="width:' + Math.min(100, Math.max(0, r.score)).toFixed(1) + '%"></span></div>' +
      '<div class="summary-addr"><span class="label">' + tt('address') + '</span>' + addrChip(r.address) + '</div>' +
      '<div class="coverage"><div class="kv-line"><span class="label" id="cov-label">' + tt('coverage') + '</span><strong>' + esc(fmtPct(cov)) + '</strong></div>' +
      '<div class="meter cov" role="meter" aria-labelledby="cov-label" aria-valuemin="0" aria-valuemax="100" aria-valuenow="' + cov.toFixed(1) + '">' +
      '<span style="width:' + cov.toFixed(2) + '%"></span></div>' +
      '<p class="hint small">' + tt('coverage_hint') + '</p></div></section>';
  }

  function actionsRow(r) {
    var lim = S.me.limits;
    var pdfBtn, watchBtn;
    if (!lim.pdf) {
      pdfBtn = '<button type="button" class="btn action locked" data-act="locked" data-feature="pdf">' + icon('lock') + '<span>' + tt('pdf_report') + '</span></button>';
    } else {
      pdfBtn = '<button type="button" class="btn action" data-act="pdf" id="act-pdf"' + (S.busy.pdf ? ' disabled aria-busy="true"' : '') + '>' +
        (S.busy.pdf ? spinner() : icon('file')) + '<span>' + tt('pdf_report') + '</span></button>';
    }
    if (lim.watches === 0) {
      watchBtn = '<button type="button" class="btn action locked" data-act="locked" data-feature="watches">' + icon('lock') + '<span>' + tt('watch_this') + '</span></button>';
    } else if (isWatched(r.address)) {
      watchBtn = '<button type="button" class="btn action done" disabled>' + icon('ok') + '<span>' + tt('watching') + '</span></button>';
    } else {
      watchBtn = '<button type="button" class="btn action" data-act="watchResult" id="act-watch"' + (S.busy.watch ? ' disabled aria-busy="true"' : '') + '>' +
        (S.busy.watch ? spinner() : icon('bell')) + '<span>' + tt('watch_this') + '</span></button>';
    }
    return '<div class="actions">' + pdfBtn + watchBtn + '</div>';
  }

  function depthNotes(r) {
    var d = r.depth;
    var notes = [];
    var fu = S.followUp ? '_fu' : '';
    if (d) {
      if (d.fetch_error) notes.push(['warn', t('depth_fetch_error', { err: d.fetch_error })]);
      if (d.still_fetching) notes.push(['progress', t('depth_still_fetching' + fu)]);
      if (d.history_truncated) notes.push(['info', t('depth_history_truncated', { n: fmtInt(10000) })]);
      if (d.frontier_pending > 0) {
        var q = d.frontier_queued < d.frontier_pending
          ? t('n_of_m', { n: fmtInt(d.frontier_queued), m: fmtInt(d.frontier_pending) })
          : fmtInt(d.frontier_queued);
        notes.push(['progress', t('depth_frontier' + fu, { n: q })]);
      }
      if (d.counterparties > 0 && d.traced < d.counterparties) {
        notes.push(['progress', t('depth_tracing' + fu, { n: fmtInt(d.traced), m: fmtInt(d.counterparties) })]);
      }
    }
    if (truncated(r.inbound) || truncated(r.outbound)) notes.push(['info', t('traversal_truncated')]);
    if (!notes.length) return '';
    return '<div class="notes">' + notes.map(function (n) {
      var cls = n[0] === 'warn' ? 'warn' : 'info';
      return '<div class="note ' + cls + ' compact">' + icon(n[0] === 'progress' ? 'progress' : n[0]) +
        '<div class="note-body"><p>' + esc(n[1]) + '</p></div></div>';
    }).join('') + '</div>';
  }

  function barRow(label, pct, tier, extra) {
    var w = pct > 0 ? Math.max(0.8, Math.min(100, pct)) : 0;
    return '<li class="bar-row"><div class="bar-label"><span>' + esc(label) + '</span><span class="num">' + esc(fmtShare(pct)) + '</span></div>' +
      '<div class="bar" aria-hidden="true"><span class="fill ' + tier + '" style="width:' + w.toFixed(2) + '%"></span></div>' + (extra || '') + '</li>';
  }

  function unattributedRow(pct, reasons) {
    var sub = reasons.filter(function (r) { return r.pct >= MIN_LISTED; }).map(function (r) {
      return '<li><span>' + esc(reasonText(r.reason)) + '</span><span class="num">' + esc(fmtShare(r.pct)) + '</span></li>';
    }).join('');
    var w = Math.max(0.8, Math.min(100, pct));
    return '<li class="bar-row unattr"><div class="bar-label"><span>' + tt('unattributed') + ' <span class="hint">· ' + tt('unknown_not_clean') +
      '</span></span><span class="num">' + esc(fmtShare(pct)) + '</span></div>' +
      '<div class="bar" aria-hidden="true"><span class="fill hatch" style="width:' + w.toFixed(2) + '%"></span></div>' +
      (sub ? '<ul class="reasons">' + sub + '</ul>' : '') + '</li>';
  }

  function connectionsCard(r, c, dirs) {
    var html = '<section class="card" aria-labelledby="conn-title">' +
      '<div class="card-head"><div><h2 id="conn-title">' + tt('connections') + '</h2><p class="hint">' + tt('connections_sub') + '</p></div></div>';
    if (c.total <= 0) {
      return html + '<p class="empty-line">' + tt('no_traced') + '</p></section>';
    }
    var listed = [], minor = [];
    c.shares.forEach(function (s) {
      if (s.pct >= MIN_LISTED) listed.push(s);
      else if (s.pct > 0) minor.push(s.category);
    });
    html += '<ul class="bars">' + listed.map(function (s) { return barRow(catName(s.category), s.pct, catTier(s.category)); }).join('');
    if (c.unattributed > 0) html += unattributedRow(c.unattributed, combineReasons(dirs));
    html += '</ul>';
    var small = minor.concat(notFound(c.shares));
    if (small.length) {
      html += '<details class="minor" data-details="minorOpen"' + (S.minorOpen ? ' open' : '') + '><summary>' +
        '<span>' + tt('less_than', { pct: fmtPct(MIN_LISTED) }) + '</span><span class="hint">' + esc(tn('n_categories', small.length)) + '</span>' +
        icon('down', 'chev') + '</summary><ul class="tag-list">' +
        small.map(function (cat) { return '<li class="tag tier-' + catTier(cat) + '">' + esc(catName(cat)) + '</li>'; }).join('') +
        '</ul></details>';
    }
    return html + '</section>';
  }

  function checksCard(r, shares) {
    var found = {};
    shares.forEach(function (s) { found[s.category] = s.pct; });
    var unseen = 0;
    ((r.verdict && r.verdict.reasons) || []).forEach(function (x) { if (x.code === 'unidentified') unseen = x.pct || 0; });
    var neutral = r.low_confidence || unseen > 0;
    var clearIcon = neutral ? 'neutral' : 'ok';
    var rows = RISK_CHECKS.map(function (cat) {
      var pct = found[cat];
      if (pct > 0) {
        return '<li class="check found">' + icon('found') + '<span class="check-name">' + esc(catName(cat)) + '</span>' +
          '<span class="check-status">' + tt('found_share', { pct: fmtShare(pct) }) + '</span></li>';
      }
      return '<li class="check ' + (neutral ? 'neutral' : 'clear') + '">' + icon(clearIcon) + '<span class="check-name">' + esc(catName(cat)) + '</span>' +
        '<span class="check-status">' + tt('not_found') + '</span></li>';
    }).join('');
    return '<section class="card" aria-labelledby="checks-title">' + '<div class="card-head"><div><h2 id="checks-title">' + tt('risk_checks') + '</h2></div></div>' +
      '<ul class="checks">' + rows + '</ul>' +
      '<p class="hint small card-foot">' + tt('checks_cover', { pct: fmtPct((r.coverage || 0) * 100) }) +
      (r.low_confidence ? ' ' + tt('checks_lowconf') : '') +
      (unseen > 0 ? ' ' + tt('checks_unseen', { pct: fmtPct(unseen) }) : '') + '</p></section>';
  }

  // The answer first: clean, caution or high risk, how far to trust it, and why.
  function verdictCard(r) {
    var v = r.verdict;
    if (!v || !v.level) return '';
    var reasons = (v.reasons || []).slice(0, 2).map(function (x) {
      var p = { pct: fmtPct(x.pct || 0), cat: x.category ? catName(x.category) : '', flag: x.flag ? tt('flag_' + x.flag + '_title') : '',
        addr: x.address ? short(x.address) : '' };
      return '<li>' + esc(tt('vr_' + x.code, p)) + '</li>';
    }).join('');
    return '<section class="card verdict verdict-' + esc(v.level) + '" aria-labelledby="verdict-title">' +
      '<div class="verdict-head"><span class="verdict-dot" aria-hidden="true"></span>' +
      '<h2 id="verdict-title">' + tt('v_' + v.level) + '</h2>' +
      '<span class="verdict-conf">' + tt('conf_label', { pct: String(v.confidence_pct || 0) }) +
      (v.insufficient_data ? ' · ' + tt('v_insufficient') : '') + '</span></div>' +
      '<ul class="verdict-reasons">' + reasons + '</ul></section>';
  }

  // Behaviour notes: what the address did, shown beside the score but never part of it.
  function flagsCard(r) {
    var flags = (r.flags || []).filter(function (f) { return FLAG_TEXT[f.code]; });
    if (!flags.length) return '';
    var rows = flags.map(function (f) {
      return '<li class="flag"><span class="flag-name">' + tt(FLAG_TEXT[f.code] + '_title') + '</span>' +
        '<span class="flag-text">' + tt(FLAG_TEXT[f.code], {
          inn: fmtUSD(f.in_usd || 0), out: fmtUSD(f.out_usd || 0), vol: fmtUSD(f.volume_usd || 0),
          days: fmtInt(f.days || 0), age: fmtInt(f.age_days || 0),
          n: fmtInt(f.count || 0), amt: fmtUSD(f.amount_usd || 0), min: fmtInt(f.minutes || 0),
          addr: f.address ? short(f.address) : ''
        }) + '</span></li>';
    }).join('');
    return '<section class="card" aria-labelledby="flags-title"><div class="card-head"><div><h2 id="flags-title">' +
      tt('flags_title') + '</h2><p class="hint small">' + tt('flags_hint') + '</p></div></div>' +
      '<ul class="flags">' + rows + '</ul></section>';
  }

  var FLAG_TEXT = { pass_through: 'flag_pass_through', high_volume_new: 'flag_high_volume_new', new_address: 'flag_new_address',
    round_split: 'flag_round_split', parked_funds: 'flag_parked_funds', poisoning_target: 'flag_poisoning_target',
    frozen_contact: 'flag_frozen_contact' };

  function entriesCard(r) {
    var inN = hasEntries(r.inbound) ? r.inbound.connections.length : 0;
    var outN = hasEntries(r.outbound) ? r.outbound.connections.length : 0;
    if (!inN && !outN) return '';
    var act = r.activity || {};
    var dir = S.dir === 'outbound' ? 'outbound' : 'inbound';
    var d = r[dir];
    var vol = dir === 'inbound' ? act.in_usd : act.out_usd;
    // Risk connections first, so a small but decisive one is never hidden
    // behind larger services (same order as the chat report).
    var list = ((d && d.connections) || []).slice().sort(function (a, b) {
      var ra = RISK_CHECKS.indexOf(a.category) >= 0 ? 0 : 1, rb = RISK_CHECKS.indexOf(b.category) >= 0 ? 0 : 1;
      return ra !== rb ? ra - rb : (b.pct || 0) - (a.pct || 0);
    });
    var all = S.entriesAll[dir];
    var shown = all ? list : list.slice(0, ENTRIES_SHOWN);

    var tab = function (id, n) {
      var on = dir === id;
      return '<button type="button" role="tab" class="seg-btn" id="dir-' + id + '" aria-selected="' + on + '" aria-controls="dir-panel" data-act="dir" data-dir="' + id + '"' +
        (n ? '' : ' disabled') + '>' + icon(id) + '<span>' + tt('dir_' + id) + '</span><span class="count">' + fmtInt(n) + '</span></button>';
    };

    var html = '<section class="card" aria-labelledby="ent-title"><div class="card-head"><div><h2 id="ent-title">' + tt('identified') + '</h2></div></div>' +
      '<div class="seg" role="tablist" aria-label="' + tt('direction') + '">' + tab('inbound', inN) + tab('outbound', outN) + '</div>' +
      '<div id="dir-panel" role="tabpanel" aria-labelledby="dir-' + dir + '"><p class="hint small dir-desc">' + tt('dir_' + dir + '_desc') + '</p>';
    if (!list.length) {
      html += '<p class="empty-line">' + tt('no_identified') + '</p>';
    } else {
      html += '<ol class="entries">' + shown.map(function (e) { return entryItem(e, vol); }).join('') + '</ol>';
      if (!all && list.length > ENTRIES_SHOWN) {
        html += '<button type="button" class="btn ghost block" data-act="moreEntries" data-dir="' + dir + '">' +
          tt('show_all', { n: fmtInt(list.length) }) + '</button>';
      }
    }
    return html + '</div></section>';
  }

  function entryItem(e, vol) {
    var usd = vol > 0 ? '<span class="entry-usd">≈ ' + esc(fmtUSD(e.pct / 100 * vol)) + '</span>' : '';
    var profile = e.profile ? '<p class="entry-profile">' + esc(profileText(e.profile)) + '</p>' : '';
    return '<li class="entry"><div class="entry-top"><span class="entry-name">' + esc(entryName(e)) + '</span>' +
      '<span class="entry-pct">' + esc(fmtShare(e.pct)) + '</span></div>' +
      '<div class="entry-meta"><span class="cat-dot ' + catTier(e.category) + '" aria-hidden="true"></span><span>' + esc(catName(e.category)) +
      ' · ' + esc(hopsText(e.min_hops)) + '</span>' + usd + '</div>' +
      '<div class="entry-addr">' + addrChip(e.address) + '</div>' + profile + '</li>';
  }

  function profileText(p) {
    var out = t('profile_main', {
      vol: fmtUSD(p.volume_usd),
      addrs: tn('n_addresses', p.counterparties || 0),
      transfers: tn('n_stored_transfers', p.transfers || 0)
    });
    if (p.partial) out += ' ' + t('partial_history');
    if (p.first_seen) out += ' · ' + fmtRange(p.first_seen, p.last_seen);
    if (p.assets && p.assets.length) out += ' · ' + p.assets.join(', ');
    return out;
  }

  function activityCard(r) {
    var a = r.activity;
    if (!a || (a.in_transfers || 0) + (a.out_transfers || 0) + (a.unpriced_transfers || 0) === 0) return '';
    var stat = function (key, usd, transfers, cps, dirKey) {
      return '<div class="stat"><span class="label">' + icon(dirKey) + tt(key) + '</span><span class="stat-num">' + esc(fmtUSD(usd)) + '</span>' +
        '<span class="hint small">' + esc(tn('n_transfers', transfers || 0)) + ' · ' + esc(tn('n_addresses', cps || 0)) + '</span></div>';
    };
    var rows = '';
    if (a.first_seen) rows += kv(t('active'), fmtRange(a.first_seen, a.last_seen));
    if (a.assets && a.assets.length) {
      rows += kv(t('assets'), a.assets.map(function (as) { return as.asset + ' ' + fmtUSD((as.in_usd || 0) + (as.out_usd || 0)); }).join(' · '));
    }
    if (a.unpriced_transfers > 0) {
      rows += kv(t('unpriced'), t('unpriced_val', {
        transfers: tn('n_transfers', a.unpriced_transfers), tokens: tn('n_tokens', a.unpriced_tokens || 0)
      }));
    }
    var d = r.depth;
    if (d && d.counterparties > 0) {
      var v = t('n_of_m', { n: fmtInt(d.traced), m: fmtInt(d.counterparties) });
      if (d.total_counterparties > d.counterparties) v += ' · ' + t('most_active', { n: fmtInt(d.counterparties), m: fmtInt(d.total_counterparties) });
      rows += kv(t('cps_traced'), v);
    }
    return '<section class="card" aria-labelledby="act-title"><div class="card-head"><div><h2 id="act-title">' + tt('activity') + '</h2></div></div>' +
      '<div class="stats">' + stat('received', a.in_usd, a.in_transfers, a.in_counterparties, 'inbound') +
      stat('sent', a.out_usd, a.out_transfers, a.out_counterparties, 'outbound') + '</div>' +
      (rows ? '<dl class="kv-list">' + rows + '</dl>' : '') + '</section>';
  }

  function kv(k, v) { return '<div class="kv"><dt>' + esc(k) + '</dt><dd>' + esc(v) + '</dd></div>'; }

  function breakdownBlock(r) {
    var ok = !!S.me.limits.details;
    if (!ok) {
      return '<button type="button" class="btn block outline locked" data-act="locked" data-feature="details">' + icon('lock') +
        tt('full_breakdown') + '</button>';
    }
    var html = '<button type="button" class="btn block outline" data-act="breakdown" id="bd-toggle" aria-expanded="' + S.breakdown + '" aria-controls="bd">' +
      icon('layers') + (S.breakdown ? tt('hide_breakdown') : tt('full_breakdown')) + '</button>';
    if (!S.breakdown) return html;
    html += '<div id="bd">';
    ['inbound', 'outbound'].forEach(function (id) {
      var d = r[id];
      if (!d) return;
      var sub = traced(d) ? t('dir_coverage', { pct: fmtPct((d.coverage != null ? d.coverage : 1 - (d.unattributed_pct || 0) / 100) * 100) }) : t('no_traced');
      html += '<section class="card"><div class="card-head"><div><h2>' + icon(id) + tt('dir_' + id) + '</h2><p class="hint">' + esc(sub) + '</p></div></div>';
      if (traced(d)) {
        var cats = (d.categories || []).slice().sort(function (a, b) { return b.pct - a.pct; });
        html += '<ul class="bars">' + cats.map(function (c) { return barRow(catName(c.category), c.pct, catTier(c.category)); }).join('');
        if (d.unattributed_pct > 0) html += unattributedRow(d.unattributed_pct, (d.unattributed_reasons || []).slice().sort(function (a, b) { return b.pct - a.pct; }));
        html += '</ul>';
        var paths = d.top_paths || [];
        if (paths.length) {
          html += '<h3 class="sub-h">' + tt('top_paths') + '</h3><ol class="paths">' +
            paths.map(function (p) { return '<li>' + esc(p.explanation) + '</li>'; }).join('') + '</ol>';
        }
        var tv = d.traversal || {};
        if (tv.fanout_capped) html += '<p class="hint small">' + tt('fanout_capped') + '</p>';
        if (tv.hop_limit_reached) html += '<p class="hint small">' + tt('hop_limit_reached') + '</p>';
      }
      html += '</section>';
    });
    return html + '</div>';
  }

  function viewHistory() {
    var h = S.res.history;
    var html = pageHead(t('history_title'), t('history_sub'));
    if (h.error && !h.data) return html + errorNote(h.error, 'history', 'reload', 'history');
    if (!h.data) return html + skeletonRows(5);
    var items = h.data.items || [];
    if (!items.length) {
      return html + '<div class="card empty">' + icon('clock', 'empty-icon') + '<p>' + tt('history_empty') + '</p>' +
        '<button type="button" class="btn primary" data-act="tab" data-tab="screen">' + tt('screen_first') + '</button></div>';
    }
    return html + '<ul class="card list">' + items.map(function (it) { return historyRow(it, true); }).join('') + '</ul>';
  }

  function viewWatches() {
    var me = S.me;
    var max = me.limits.watches;
    var html = pageHead(t('watches_title'), t('watches_sub'));
    if (max === 0) {
      return html + '<div class="card empty">' + icon('bell', 'empty-icon') + '<p><strong>' + tt('watches_locked_title') + '</strong></p>' +
        '<p class="hint">' + tt('watches_locked_body') + '</p><button type="button" class="btn primary" data-act="upgrade">' + tt('upgrade') + '</button></div>';
    }
    var w = S.res.watches;
    var items = (w.data && w.data.items) || [];
    var limit = w.data && w.data.limit != null ? w.data.limit : max;
    var count = w.data ? items.length : (me.usage.watches || 0);
    var full = limit >= 0 && count >= limit;
    var countText = limit < 0 ? tn('watches_count_unl', count) : t('watches_of', { n: fmtInt(count), m: fmtInt(limit) });

    html += '<form class="card form" data-form="watch" novalidate>' +
      '<div class="card-head"><div><h2>' + tt('add_watch') + '</h2></div><span class="count-tag">' + esc(countText) + '</span></div>' +
      (limit >= 0 ? meter(count, limit, full ? 'full' : '') : '') +
      '<label class="label" for="w-addr">' + tt('address_label') + '</label>' +
      '<input id="w-addr" class="input mono" data-bind="watchAddr" value="' + esc(S.watchAddr) + '" placeholder="' + tt('address_ph') +
      '" autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false"' + (full ? ' disabled' : '') + '>' +
      '<label class="label" for="w-label">' + tt('label_optional') + '</label>' +
      '<input id="w-label" class="input" data-bind="watchLabel" maxlength="64" value="' + esc(S.watchLabel) + '" placeholder="' + tt('label_ph') + '"' + (full ? ' disabled' : '') + '>' +
      (full
        ? '<div class="note warn compact">' + icon('info') + '<div class="note-body"><p>' + tt('watches_full') + '</p><div class="note-actions"><button type="button" class="btn sm primary" data-act="upgrade">' + tt('upgrade') + '</button></div></div></div>'
        : '<button type="submit" class="btn primary block" id="w-submit"' + (!S.watchAddr.trim() || S.busy.addWatch ? ' disabled' : '') + '>' +
          (S.busy.addWatch ? spinner('on-accent') : icon('plus')) + tt('add_watch_btn') + '</button>') +
      (S.watchError ? errorNote(S.watchError, 'watch') : '') +
      '<p class="hint small">' + icon('bell', 'inline') + tt('alerts_hint') + '</p></form>';

    if (w.error && !w.data) return html + errorNote(w.error, 'watch', 'reload', 'watches');
    if (!w.data) return html + skeletonRows(2);
    if (!items.length) return html + '<div class="card empty"><p class="hint">' + tt('watches_empty') + '</p></div>';
    return html + '<ul class="watch-list">' + items.map(watchItem).join('') + '</ul>';
  }

  function watchItem(w) {
    var last = w.last;
    var status;
    if (!last) {
      status = '<span class="hint small">' + spinner('xs') + ' ' + tt('first_check') + '</span>';
    } else {
      status = bandPill(last.band, true) + '<span class="score-sm">' + esc(fmtNum(last.score || 0, 1)) + '</span>' +
        '<span class="hint small">' + tt('cov_short', { pct: fmtPct((last.coverage || 0) * 100) }) + '</span>';
    }
    var chips = '';
    if (last) {
      var cats = last.risk_categories || [];
      if (last.listed) chips += '<li class="tag tier-severe strong">' + tt('directly_listed') + '</li>';
      chips += cats.map(function (c) { return '<li class="tag tier-' + catTier(c) + '">' + esc(catName(c)) + '</li>'; }).join('');
      if (!chips) chips = '<li class="tag tier-clear">' + tt('no_risk_cats') + '</li>';
    }
    return '<li class="card watch"><div class="watch-top"><div class="watch-id"><span class="watch-name">' + esc(w.label || short(w.address)) + '</span>' +
      '<span class="hint small">' + esc(chainName(w.chain)) + '</span></div>' +
      '<button type="button" class="icon-btn danger" data-act="removeWatch" data-id="' + esc(w.id) + '" aria-label="' + tt('remove_watch', { name: w.label || short(w.address) }) + '"' +
      (S.busy['rm' + w.id] ? ' disabled' : '') + '>' + (S.busy['rm' + w.id] ? spinner('xs') : icon('trash')) + '</button></div>' +
      addrChip(w.address) +
      '<div class="watch-status">' + status + '</div>' +
      (chips ? '<ul class="tag-list">' + chips + '</ul>' : '') +
      '<div class="watch-foot"><span class="hint small">' + (w.checked_at ? tt('checked', { when: fmtRel(w.checked_at) }) : tt('not_checked')) + '</span>' +
      '<button type="button" class="link-btn" data-act="rescreen" data-confirm="1" data-address="' + esc(w.address) + '" data-chain="' + esc(w.chain || '') + '">' + tt('screen_now') + '</button></div></li>';
  }

  function viewAccount() {
    var me = S.me;
    var html = pageHead(t('account_title'), null, me.user && me.user.admin ? '<span class="badge admin">' + tt('admin') + '</span>' : '');
    html += currentPlanCard();
    html += '<section class="section" id="plans" aria-labelledby="plans-title"><div class="section-head"><h2 id="plans-title">' + tt('plans') + '</h2></div>' +
      (me.plans || []).map(planCard).join('') + '<p class="hint small">' + tt('plans_hint') + '</p></section>';
    html += batchSection();
    html += apiSection();
    html += settingsSection();
    return html;
  }

  function currentPlanCard() {
    var me = S.me, a = me.access, u = me.user || {};
    var who = u.first_name ? (u.username ? t('signed_in_as_user', { name: u.first_name, username: u.username }) : t('signed_in_as', { name: u.first_name })) : '';
    var html = '<section class="card plan-current" aria-labelledby="cur-title">';
    if (who) html += '<p class="hint small">' + esc(who) + '</p>';
    if (!a) {
      html += '<h2 id="cur-title" class="plan-name">' + tt('no_plan_title') + '</h2><p class="hint">' + tt('no_plan_body') + '</p>';
    } else {
      var until = a.until ? fmtDate(a.until) : '';
      var status = '';
      if (until) status = a.renews ? t('renews_on', { date: until }) : t('ends_on', { date: until }) + ' · ' + t('no_renew');
      var daysLeft = a.until ? Math.max(0, Math.ceil((parseDate(a.until).getTime() - Date.now()) / 86400000)) : null;
      html += '<div class="plan-current-top"><div><span class="eyebrow">' + tt('current_plan') + '</span>' +
        '<h2 id="cur-title" class="plan-name">' + esc(a.plan.name) + '</h2></div>' +
        (daysLeft != null ? '<span class="count-tag">' + esc(tn('days_left', daysLeft)) + '</span>' : '') + '</div>' +
        '<p class="plan-source">' + tt('source_' + (hasKey('source_' + a.source) ? a.source : 'other')) + (status ? ' · ' + esc(status) : '') + '</p>';
    }
    html += '<div class="plan-usage">' + usageBlock();
    var wmax = me.limits.watches;
    if (wmax !== 0) {
      var wn = me.usage.watches || 0;
      html += '<div class="usage"><div class="usage-row"><span>' +
        (wmax < 0 ? esc(tn('watches_count_unl', wn)) : tt('watches_of', { n: fmtInt(wn), m: fmtInt(wmax) })) +
        '</span></div>' + (wmax > 0 ? meter(wn, wmax) : '') + '</div>';
    }
    return html + '</div></section>';
  }

  function planFeatures(p) {
    var f = function (ok, text) {
      return '<li class="' + (ok ? 'yes' : 'no') + '">' + icon(ok ? 'ok' : 'neutral') + '<span>' + esc(text) + '</span>' +
        (ok ? '' : '<span class="sr-only">' + tt('not_included') + '</span>') + '</li>';
    };
    return '<ul class="features">' +
      f(true, tn('f_screens', p.daily_screens)) +
      f(p.watches !== 0, p.watches !== 0 ? tn('f_watches', p.watches) : t('f_watches_none')) +
      f(!!p.details, t('f_details')) +
      f(!!p.pdf, t('f_pdf')) +
      f(p.batch !== 0, p.batch !== 0 ? t('f_batch', { n: fmtLimit(p.batch) }) : t('f_batch_none')) +
      f(!!p.api, t('f_api')) + '</ul>';
  }

  function planCard(p) {
    var me = S.me, a = me.access;
    var current = !!(a && a.plan && a.plan.id === p.id);
    var autoRenew = current && a.source === 'stars' && a.renews;
    var busyS = S.busy['stars:' + p.id], busyU = S.busy['usdt:' + p.id];
    var buttons = '';
    if (autoRenew) {
      buttons += '<p class="hint small renew-note">' + icon('ok', 'inline') + tt('auto_renews') + '</p>';
    } else if (p.price_stars) {
      buttons += '<button type="button" class="btn primary" data-act="payStars" data-plan="' + esc(p.id) + '"' + (busyS ? ' disabled aria-busy="true"' : '') + '>' +
        (busyS ? spinner('on-accent') : icon('star', 'star')) + tt(current ? 'extend_stars' : 'pay_stars') + '</button>';
    }
    if (me.usdt_enabled && p.price_usdt) {
      buttons += '<button type="button" class="btn secondary" data-act="payUsdt" data-plan="' + esc(p.id) + '"' + (busyU ? ' disabled aria-busy="true"' : '') + '>' +
        (busyU ? spinner() : '') + tt(current ? 'extend_usdt' : 'pay_usdt') + '</button>';
    }
    var price = '';
    if (p.price_usdt) price += '<span class="price-main">' + esc(fmtUsdt(p.price_usdt)) + ' <small>USDT</small></span>';
    if (p.price_stars) price += '<span class="price-alt">' + icon('star', 'star') + esc(fmtInt(p.price_stars)) + ' ' + tt('stars') + '</span>';
    return '<article class="card plan' + (current ? ' current' : '') + '" aria-labelledby="plan-' + esc(p.id) + '">' +
      '<div class="plan-head"><h3 id="plan-' + esc(p.id) + '">' + esc(p.name) + '</h3>' + (current ? '<span class="badge">' + tt('current') + '</span>' : '') + '</div>' +
      '<div class="plan-price">' + price + '<span class="hint small">' + tt('per_period') + '</span></div>' +
      planFeatures(p) + (buttons ? '<div class="plan-actions">' + buttons + '</div>' : '') + '</article>';
  }

  function fmtUsdt(s) {
    var n = parseFloat(s);
    if (isNaN(n)) return s;
    return fmtNum(n, n % 1 === 0 ? 0 : 2);
  }

  function lockedSection(iconName, title, body) {
    return '<section class="card locked-card">' + '<div class="card-head"><div><h2>' + icon(iconName) + esc(title) + '</h2><p class="hint">' + esc(body) + '</p></div>' +
      '<span class="badge muted">' + icon('lock', 'inline') + tt('locked') + '</span></div>' +
      '<button type="button" class="btn ghost block" data-act="upgrade">' + tt('see_plans') + '</button></section>';
  }

  function batchSection() {
    var max = S.me.limits.batch;
    if (max === 0) return lockedSection('list', t('batch_title'), t('batch_locked'));
    var list = parseBatch(S.batchText);
    return '<form class="card form" data-form="batch" aria-labelledby="batch-title" novalidate>' +
      '<div class="card-head"><div><h2 id="batch-title">' + icon('list') + tt('batch_title') + '</h2><p class="hint">' + tt('batch_sub') + '</p></div></div>' +
      '<label class="label" for="batch-ta">' + tt('batch_label') + '</label>' +
      '<textarea id="batch-ta" class="input mono" rows="5" data-bind="batchText" placeholder="' + tt('batch_ph') + '" spellcheck="false" autocapitalize="off" autocorrect="off">' +
      esc(S.batchText) + '</textarea>' +
      '<p class="hint small" id="batch-count" aria-live="polite">' + esc(batchCountText(list, max)) + '</p>' +
      '<button type="submit" class="btn primary block" id="batch-btn"' + (batchDisabled(list, max) ? ' disabled' : '') + '>' +
      (S.busy.batch ? spinner('on-accent') : '') + tt('batch_btn') + '</button>' +
      '<p class="hint small">' + tt('batch_note') + '</p></form>';
  }

  function batchCountText(list, max) {
    var n = list.length;
    var s = tn('n_addresses_batch', n);
    if (max >= 0) s += ' · ' + t('batch_max', { n: fmtInt(max) });
    if (max >= 0 && n > max) s += ' · ' + t('batch_too_many', { n: fmtInt(max) });
    var dmax = S.me.limits.daily_screens;
    if (dmax >= 0) {
      var left = Math.max(0, dmax - (S.me.usage.screens_today || 0));
      if (n > left) s += ' · ' + t('batch_screens_left', { n: fmtInt(left) });
    }
    return s;
  }
  function batchDisabled(list, max) { return !list.length || (max >= 0 && list.length > max) || !!S.busy.batch; }

  function apiSection() {
    if (!S.me.limits.api) return lockedSection('key', t('api_title'), t('api_locked'));
    var k = S.res.keys;
    var html = '<section class="card" aria-labelledby="api-title"><div class="card-head"><div><h2 id="api-title">' + icon('key') + tt('api_title') + '</h2>' +
      '<p class="hint">' + tt('api_sub') + '</p></div></div>';
    if (S.newKey) {
      html += '<div class="secret" role="alert"><p class="label">' + tt('key_new', { name: S.newKey.name || '' }) + '</p>' +
        '<div class="secret-box"><code class="mono">' + esc(S.newKey.key) + '</code>' + copyBtn(S.newKey.key, t('copy_key')) + '</div>' +
        '<p class="warn-text">' + icon('warn', 'inline') + tt('key_once') + '</p>' +
        '<button type="button" class="btn sm" data-act="dismissKey">' + tt('key_saved') + '</button></div>';
    }
    if (k.error && !k.data) html += errorNote(k.error, 'api', 'reload', 'keys');
    else if (!k.data) html += '<div class="sk-block"><span class="sk sk-line w75"></span><span class="sk sk-line w60"></span></div>';
    else if (!k.data.items || !k.data.items.length) html += '<p class="empty-line">' + tt('keys_empty') + '</p>';
    else {
      html += '<ul class="keys">' + k.data.items.map(function (x) {
        var busy = S.busy['rk' + x.id];
        return '<li class="key-row"><div class="key-main"><span class="row-title">' + esc(x.name) + '</span>' +
          '<span class="mono small">' + esc(x.prefix) + '…</span>' +
          '<span class="hint small">' + tt('key_created', { date: fmtDate(x.created_at) }) + ' · ' +
          (x.last_used_at ? tt('key_used', { when: fmtRel(x.last_used_at) }) : tt('key_never')) + '</span></div>' +
          '<button type="button" class="btn sm danger-outline" data-act="revokeKey" data-id="' + esc(x.id) + '"' + (busy ? ' disabled' : '') + '>' +
          (busy ? spinner('xs') : '') + tt('revoke') + '</button></li>';
      }).join('') + '</ul>';
    }
    html += '<form class="inline-form" data-form="key" novalidate><label class="sr-only" for="key-name">' + tt('key_name') + '</label>' +
      '<input id="key-name" class="input" data-bind="keyName" maxlength="48" value="' + esc(S.keyName) + '" placeholder="' + tt('key_name_ph') + '">' +
      '<button type="submit" class="btn primary" id="key-btn"' + (!S.keyName.trim() || S.busy.key ? ' disabled' : '') + '>' +
      (S.busy.key ? spinner('on-accent') : icon('plus')) + tt('key_create') + '</button></form>' +
      '<p class="hint small api-doc">' + tt('api_doc_pre') + ' <code>POST /api/v1/screen</code> ' + tt('api_doc_mid') +
      ' <code>Authorization: Bearer &lt;key&gt;</code>' + tt('api_doc_post') + '</p>';
    return html + '</section>';
  }

  function settingsSection() {
    var me = S.me;
    var langBtn = function (id, label) {
      return '<button type="button" class="seg-btn" data-act="lang" data-lang="' + id + '" id="lang-' + id + '" aria-pressed="' + (S.lang === id) + '" lang="' + id + '">' + esc(label) + '</button>';
    };
    return '<section class="section" aria-labelledby="set-title"><div class="section-head"><h2 id="set-title">' + tt('settings') + '</h2></div>' +
      '<div class="card settings">' +
      '<div class="setting"><span class="setting-label">' + icon('globe') + '<span id="lang-label">' + tt('language') + '</span></span>' +
      '<div class="seg small" role="group" aria-labelledby="lang-label">' + langBtn('en', 'English') + langBtn('tr', 'Türkçe') + langBtn('ru', 'Русский') + '</div></div>' +
      (me.support ? '<button type="button" class="setting link" data-act="support"><span class="setting-label">' + icon('help') + tt('support') + '</span>' +
        '<span class="hint">' + esc(me.support) + icon('right') + '</span></button>' : '') +
      '<div class="setting"><span class="setting-label">' + icon('doc') + '<span>' + tt('terms') + '</span></span><span class="hint small">' + tt('terms_hint') + '</span></div>' +
      '</div></section>';
  }

  function renderSheetUsdt(sh) {
    var inv = sh.inv;
    var planName = sh.plan ? sh.plan.name : '';
    if (sh.expired) {
      return '<h2 id="sheet-title" tabindex="-1" data-autofocus>' + tt('usdt_title') + '</h2>' +
        '<div class="note warn">' + icon('clock') + '<div class="note-body"><p>' + tt('usdt_expired') + '</p></div></div>' +
        '<div class="sheet-actions"><button type="button" class="btn primary block" data-act="payUsdt" data-plan="' + esc(sh.plan ? sh.plan.id : '') + '">' + tt('usdt_new') + '</button>' +
        '<button type="button" class="btn ghost block" data-act="closeSheet">' + tt('close') + '</button></div>';
    }
    return '<h2 id="sheet-title" tabindex="-1" data-autofocus>' + tt('usdt_title') + '</h2>' +
      '<p class="hint">' + tt('usdt_for', { plan: planName }) + '</p>' +
      '<div class="amount-box"><span class="label">' + tt('usdt_send_exactly') + '</span>' +
      '<div class="amount"><span class="mono">' + esc(inv.amount) + '</span> <small>USDT</small></div>' +
      '<button type="button" class="btn sm" data-act="copy" data-copy="' + esc(inv.amount) + '">' + icon('copy') + tt('copy_amount') + '</button></div>' +
      '<dl class="kv-list"><div class="kv"><dt>' + tt('network') + '</dt><dd>' + esc(inv.network) + '</dd></div>' +
      '<div class="kv"><dt>' + tt('usdt_expires') + '</dt><dd><span class="mono" id="usdt-countdown">—</span></dd></div></dl>' +
      '<span class="label">' + tt('usdt_address') + '</span>' +
      '<div class="secret-box"><code class="mono">' + esc(inv.address) + '</code>' + copyBtn(inv.address, t('copy_address')) + '</div>' +
      '<div class="note warn">' + icon('warn') + '<div class="note-body"><p><strong>' + tt('usdt_exact_title') + '</strong></p><p>' +
      tt('usdt_exact_body', { amount: inv.amount }) + '</p></div></div>' +
      '<p class="hint small">' + tt('usdt_detect') + '</p>' +
      '<div class="sheet-actions"><button type="button" class="btn primary block" data-act="closeSheet">' + tt('usdt_done') + '</button></div>';
  }

  /* ------------------------------------------------------------------ */
  /* Render                                                              */
  /* ------------------------------------------------------------------ */

  function renderTabs() {
    var nav = $('#tabs');
    if (S.boot !== 'ready') { nav.hidden = true; return; }
    nav.hidden = false;
    var tabs = [['screen', 'search'], ['history', 'clock'], ['watches', 'eye'], ['account', 'user']];
    nav.setAttribute('aria-label', t('nav'));
    nav.innerHTML = '<div class="tabs-inner">' + tabs.map(function (x) {
      var on = S.tab === x[0];
      return '<button type="button" class="tab" data-act="tab" data-tab="' + x[0] + '"' + (on ? ' aria-current="page"' : '') + '>' +
        icon(x[1]) + '<span>' + tt('tab_' + x[0]) + '</span></button>';
    }).join('') + '</div>';
  }

  function render() {
    var main = $('#view');
    // Keep focus and caret across the rebuild.
    var a = document.activeElement;
    var focusId = a && a.id && main.contains(a) ? a.id : null;
    var sel = focusId && (a.tagName === 'INPUT' || a.tagName === 'TEXTAREA') ? [a.selectionStart, a.selectionEnd] : null;

    var html;
    if (S.boot !== 'ready') html = viewBoot();
    else if (S.view === 'result' && S.result) html = viewResult();
    else if (S.view === 'history') html = viewHistory();
    else if (S.view === 'watches') html = viewWatches();
    else if (S.view === 'account') html = viewAccount();
    else html = viewScreen();
    main.innerHTML = '<div class="view view-' + S.view + '">' + html + '</div>';
    main.setAttribute('data-view', S.view);

    if (focusId) {
      var el = document.getElementById(focusId);
      if (el && !el.disabled) {
        el.focus({ preventScroll: true });
        if (sel && el.setSelectionRange) { try { el.setSelectionRange(sel[0], sel[1]); } catch (_) { /* ignore */ } }
      }
    }
    renderTabs();
    syncTelegramButtons();
    if (S.screening) tickProgress();
    document.title = t('app_name');
  }

  function renderSheet() {
    var root = $('#sheet-root');
    if (!S.sheet) {
      root.innerHTML = '';
      document.body.classList.remove('sheet-open');
      return;
    }
    var body = S.sheet.type === 'usdt' ? renderSheetUsdt(S.sheet) : '';
    root.innerHTML = '<div class="backdrop" data-act="closeSheet"></div>' +
      '<div class="sheet" role="dialog" aria-modal="true" aria-labelledby="sheet-title">' +
      '<button type="button" class="icon-btn sheet-close" data-act="closeSheet" aria-label="' + tt('close') + '">' + icon('x') + '</button>' +
      body + '</div>';
    document.body.classList.add('sheet-open');
    tickCountdown();
  }

  function updateScreenForm() {
    var btn = $('#screen-btn');
    var filled = S.input.trim() !== '';
    if (btn) btn.disabled = !filled || S.screening;
    // Swap paste <-> clear when emptiness changes.
    var ib = $('#addr-btn');
    var wantClear = filled;
    var isClear = ib && ib.getAttribute('data-act') === 'clear';
    if (ib && wantClear !== isClear) {
      if (wantClear) {
        ib.setAttribute('data-act', 'clear');
        ib.setAttribute('aria-label', t('clear'));
        ib.innerHTML = icon('x');
      } else {
        ib.setAttribute('data-act', 'paste');
        ib.setAttribute('aria-label', t('paste'));
        ib.innerHTML = icon('paste') + '<span>' + tt('paste') + '</span>';
      }
    }
    syncTelegramButtons();
  }

  function onInput(e) {
    var el = e.target;
    var bind = el.getAttribute && el.getAttribute('data-bind');
    if (!bind) return;
    S[bind] = el.value;
    if (bind === 'input') {
      if (S.screenError) { S.screenError = null; var n = document.querySelector('.view-screen .note.danger'); if (n) n.remove(); }
      updateScreenForm();
    } else if (bind === 'batchText') {
      var list = parseBatch(S.batchText);
      var c = $('#batch-count');
      if (c) c.textContent = batchCountText(list, S.me.limits.batch);
      var b = $('#batch-btn');
      if (b) b.disabled = batchDisabled(list, S.me.limits.batch);
    } else if (bind === 'watchAddr') {
      var wb = $('#w-submit');
      if (wb) wb.disabled = !S.watchAddr.trim() || !!S.busy.addWatch;
    } else if (bind === 'keyName') {
      var kb = $('#key-btn');
      if (kb) kb.disabled = !S.keyName.trim() || !!S.busy.key;
    }
  }

  document.addEventListener('click', function (e) {
    var el = e.target.closest && e.target.closest('[data-act]');
    if (!el || el.disabled) return;
    var fn = ACTIONS[el.getAttribute('data-act')];
    if (fn) { e.preventDefault(); fn(el); }
  });
  document.addEventListener('submit', function (e) {
    var f = e.target.getAttribute('data-form');
    if (!f || !FORMS[f]) return;
    e.preventDefault();
    FORMS[f]();
  });
  document.addEventListener('input', onInput);
  document.addEventListener('toggle', function (e) {
    var key = e.target.getAttribute && e.target.getAttribute('data-details');
    if (key) S[key] = e.target.open;
  }, true);
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && S.sheet) closeSheet();
  });

  /* ------------------------------------------------------------------ */
  /* Mock mode                                                           */
  /* ------------------------------------------------------------------ */

  var M = null;
  var sampleCache = null;

  var MOCK_PLANS = [
    { id: 'basic', name: 'Basic', daily_screens: 20, details: false, pdf: false, watches: 3, batch: 0, api: false, price_stars: 800, price_usdt: '10.00' },
    { id: 'pro', name: 'Pro', daily_screens: 200, details: true, pdf: true, watches: 25, batch: 100, api: false, price_stars: 3800, price_usdt: '49.00' },
    { id: 'business', name: 'Business', daily_screens: 2000, details: true, pdf: true, watches: 200, batch: 1000, api: true, price_stars: 10000, price_usdt: '199.00' }
  ];
  var MOCK_ADDR = {
    sample: 'TNwf8VBNCkg7Y1pgyzbHdWdekkamoqcrmL',
    medium: 'TQ8vX3pLmR2kYh7sWdC4nFb9JtE6aGuZoK',
    high: 'TYh4sK9wPq2RmVx7cNd3LbF6gTjE8aHuZr',
    watch2: 'TMq5bR7xWn3KpY2hVd8sLc4FgJt6eAzUoP',
    pay: 'TPayK3mX8vQ2wR7nYh4sLd9cFbGt6jEuAz'
  };

  function mockInit() {
    var planId = qs.get('plan') || 'pro';
    var plan = null;
    MOCK_PLANS.forEach(function (p) { if (p.id === planId) plan = p; });
    var admin = qs.get('admin') === '1';
    var now = Date.now();
    var iso = function (ms) { return new Date(ms).toISOString(); };
    var midnight = new Date(); midnight.setUTCHours(24, 0, 0, 0);
    M = {
      admin: admin,
      plan: plan,
      access: plan ? { plan: { id: plan.id, name: plan.name }, until: iso(now + 30 * 86400000), source: 'stars', renews: true } : null,
      lang: LANGS.indexOf(qs.get('lang')) !== -1 ? qs.get('lang') : null,
      screens: plan ? 3 : 0,
      resets: midnight.toISOString(),
      history: [
        { chain: 'tron', address: MOCK_ADDR.sample, band: 'low', score: 14.9904, coverage: 0.999364, channel: 'app', created_at: iso(now - 25 * 60000) },
        { chain: 'tron', address: MOCK_ADDR.high, band: 'high', score: 71.3, coverage: 0.842, channel: 'bot', created_at: iso(now - 5 * 3600000) },
        { chain: 'tron', address: MOCK_ADDR.medium, band: 'medium', score: 38.2, coverage: 0.41, channel: 'app', created_at: iso(now - 26 * 3600000) },
        { chain: 'tron', address: MOCK_ADDR.watch2, band: null, score: null, coverage: null, channel: 'bot', created_at: iso(now - 4 * 86400000) }
      ],
      watches: plan && plan.watches !== 0 ? [
        { id: 7, chain: 'tron', address: MOCK_ADDR.sample, label: 'Supplier A', created_at: iso(now - 9 * 86400000), checked_at: iso(now - 2 * 3600000),
          last: { band: 'low', score: 14.99, coverage: 0.999, risk_categories: [], listed: false } },
        { id: 9, chain: 'tron', address: MOCK_ADDR.medium, label: '', created_at: iso(now - 3 * 86400000), checked_at: iso(now - 40 * 60000),
          last: { band: 'medium', score: 38.2, coverage: 0.41, risk_categories: ['gambling', 'high_risk_exchange'], listed: false } }
      ] : [],
      keys: plan && plan.api ? [
        { id: 3, prefix: 'trk_4f9a', name: 'Production', created_at: iso(now - 20 * 86400000), last_used_at: iso(now - 3600000) }
      ] : [],
      nextId: 20
    };
  }

  function mockLimits() {
    if (M.admin) return { daily_screens: -1, details: true, pdf: true, watches: -1, batch: -1, api: true };
    if (!M.plan) return { daily_screens: 0, details: false, pdf: false, watches: 0, batch: 0, api: false };
    var p = M.plan;
    return { daily_screens: p.daily_screens, details: p.details, pdf: p.pdf, watches: p.watches, batch: p.batch, api: p.api };
  }

  function mockMe() {
    var u = tg && tg.initDataUnsafe && tg.initDataUnsafe.user;
    var multi = qs.get('chains') === '2';
    return {
      user: { id: 42, first_name: (u && u.first_name) || 'Ada', username: (u && u.username) || 'ada', lang: M.lang, admin: M.admin },
      access: M.access,
      limits: mockLimits(),
      usage: { screens_today: M.screens, watches: M.watches.length, resets_at: M.resets },
      plans: MOCK_PLANS,
      usdt_enabled: qs.get('usdt') !== '0',
      chains: [
        { id: 'tron', name: 'Tron', enabled: true },
        { id: 'ethereum', name: 'Ethereum', enabled: multi },
        { id: 'bsc', name: 'BNB Smart Chain', enabled: false }
      ],
      support: '@support'
    };
  }

  function mockErr(code, status, message) { return new ApiError(code, message || '', status); }

  function mockGrant(planId, source) {
    MOCK_PLANS.forEach(function (p) { if (p.id === planId) M.plan = p; });
    M.access = { plan: { id: M.plan.id, name: M.plan.name }, until: new Date(Date.now() + 30 * 86400000).toISOString(), source: source, renews: source === 'stars' };
  }

  function mockDetect(address) {
    if (/^T[1-9A-HJ-NP-Za-km-z]{33}$/.test(address)) return 'tron';
    if (/^0x[0-9a-fA-F]{40}$/.test(address)) return 'ethereum';
    return null;
  }

  function mockSample() {
    if (sampleCache) return Promise.resolve(JSON.parse(sampleCache));
    return fetch('./sample-screen.json', { cache: 'no-store' }).then(function (r) { return r.text(); }).then(function (text) {
      sampleCache = text;
      return JSON.parse(text);
    });
  }

  // Variants of the sample so every banner and colour can be previewed.
  function mockVariant(r, kind) {
    if (kind === 'high') {
      r.band = 'high'; r.score = 71.3; r.coverage = 0.842;
      r.own_label = { entity: 'Example OTC Desk', category: 'high_risk_exchange' };
      r.inbound.categories = [
        { category: 'unnamed_service', pct: 38.4 }, { category: 'scam', pct: 21.7 }, { category: 'mixer', pct: 9.2 },
        { category: 'high_risk_exchange', pct: 8.1 }, { category: 'exchange', pct: 6.3 }, { category: 'gambling', pct: 0.04 }
      ];
      r.inbound.unattributed_pct = 16.26;
      r.inbound.unattributed_reasons = [{ reason: 'dead_end', pct: 12.1 }, { reason: 'hop_limit', pct: 4.16 }];
      r.inbound.connections.splice(1, 0,
        { address: 'TKsN2yZ8pQ4vW7mRcX3hLd9bFg6jEt5uAa', entity: 'Pig-butchering cluster #14', category: 'scam', pct: 21.7, min_hops: 2 },
        { address: 'TDx7Lq3wPz9mKv2NcR8hYd4sFb6gJt5uEa', entity: 'Tornado-style mixer', category: 'mixer', pct: 9.2, min_hops: 3 });
      r.outbound.total_traced = 0.2;
      r.outbound.categories = [{ category: 'exchange', pct: 62.5 }, { category: 'dex', pct: 12.0 }];
      r.outbound.unattributed_pct = 25.5;
      r.outbound.unattributed_reasons = [{ reason: 'fanout_cap', pct: 25.5 }];
      r.outbound.connections = [{ address: 'TAUN6FwrnwwmaEqYcckffC7wYmbaS6cBiX', entity: 'Binance', category: 'exchange', pct: 62.5, min_hops: 1 }];
      r.outbound.top_paths = [{ explanation: '1 hop(s) to Binance, categorised exchange, contributing 0.1250 of traced value' }];
      r.outbound.traversal = { fanout_capped: true, hop_limit_reached: false };
      r.activity.out_usd = 18200; r.activity.out_transfers = 12; r.activity.out_counterparties = 3;
      r.depth.frontier_pending = 14; r.depth.frontier_queued = 9;
    } else if (kind === 'medium') {
      r.band = 'medium'; r.score = 38.2; r.coverage = 0.41; r.low_confidence = true;
      r.inbound.categories = [{ category: 'gambling', pct: 22.5 }, { category: 'high_risk_exchange', pct: 11.0 }, { category: 'unnamed_service', pct: 7.5 }];
      r.inbound.unattributed_pct = 59.0;
      r.inbound.unattributed_reasons = [{ reason: 'dead_end', pct: 51.2 }, { reason: 'hop_limit', pct: 7.8 }];
      r.depth.still_fetching = true; r.depth.traced = 12;
    }
    return r;
  }

  function mockApi(method, path, body) {
    if (!M) mockInit();
    body = body || {};
    var delay = path === '/screen' ? 1800 : path === '/report' ? 1200 : 350;
    return new Promise(function (resolve) { setTimeout(resolve, delay); }).then(function () {
      var lim = mockLimits();
      var route = method + ' ' + path.split('?')[0];
      var m;
      if (route === 'GET /me') return mockMe();
      if (route === 'POST /lang') { M.lang = body.lang; return { lang: body.lang }; }
      if (route === 'GET /history') return { items: M.history.slice() };

      if (route === 'POST /screen' || route === 'POST /report') {
        if (!M.access && !M.admin) throw mockErr('no_plan', 402);
        if (route === 'POST /report' && !lim.pdf) throw mockErr('feature_locked', 403);
        var address = String(body.address || '').trim();
        var chain = mockDetect(address);
        if (!chain) throw mockErr('bad_address', 400);
        if (chain === 'ethereum' && qs.get('chains') !== '2') throw mockErr('chain_unavailable', 400);
        if (/fail/i.test(address)) throw mockErr('screen_failed', 502);
        if (lim.daily_screens >= 0 && M.screens >= lim.daily_screens) throw mockErr('limit_reached', 429);
        M.screens++;
        if (route === 'POST /report') return { sent: true };
        return mockSample().then(function (r) {
          r.address = address;
          r.chain = chain;
          var kind = qs.get('variant') || (address === MOCK_ADDR.high ? 'high' : address === MOCK_ADDR.medium ? 'medium' : '');
          mockVariant(r, kind);
          M.history.unshift({ chain: chain, address: address, band: r.band, score: r.score, coverage: r.coverage, channel: 'app', created_at: new Date().toISOString() });
          return { result: r, usage: { screens_today: M.screens, daily_screens: lim.daily_screens } };
        });
      }

      if (route === 'GET /watches') {
        if (lim.watches === 0) throw mockErr('feature_locked', 403);
        return { items: M.watches.slice(), limit: lim.watches };
      }
      if (route === 'POST /watches') {
        if (lim.watches === 0) throw mockErr('feature_locked', 403);
        if (lim.watches >= 0 && M.watches.length >= lim.watches) throw mockErr('limit_reached', 429);
        var wc = mockDetect(String(body.address || '').trim());
        if (!wc) throw mockErr('bad_address', 400);
        var w = { id: M.nextId++, chain: wc, address: body.address.trim(), label: body.label || '', created_at: new Date().toISOString(), checked_at: null, last: null };
        M.watches.unshift(w);
        return w;
      }
      if ((m = /^DELETE \/watches\/(.+)$/.exec(route))) {
        var before = M.watches.length;
        M.watches = M.watches.filter(function (x) { return String(x.id) !== m[1]; });
        if (M.watches.length === before) throw mockErr('not_found', 404);
        return { removed: true };
      }

      if (route === 'POST /invoice/stars') return { link: 'https://t.me/$mock-invoice-' + body.plan };
      if (route === 'POST /invoice/usdt') {
        var plan = null;
        MOCK_PLANS.forEach(function (p) { if (p.id === body.plan) plan = p; });
        var cents = (Math.floor(Math.random() * 90) + 10) / 100;
        return {
          amount: (parseFloat(plan ? plan.price_usdt : '10') + cents).toFixed(2),
          address: MOCK_ADDR.pay,
          network: 'TRON (TRC-20)',
          expires_at: new Date(Date.now() + 3600000).toISOString()
        };
      }

      if (route === 'POST /batch') {
        if (lim.batch === 0) throw mockErr('feature_locked', 403);
        var n = (body.addresses || []).length;
        if (lim.batch >= 0 && n > lim.batch) throw mockErr('limit_reached', 429);
        M.screens += n;
        return { accepted: n };
      }

      if (route === 'GET /keys') {
        if (!lim.api) throw mockErr('feature_locked', 403);
        return { items: M.keys.slice() };
      }
      if (route === 'POST /keys') {
        if (!lim.api) throw mockErr('feature_locked', 403);
        var hex = '';
        for (var i = 0; i < 40; i++) hex += '0123456789abcdef'.charAt(Math.floor(Math.random() * 16));
        var key = 'trk_' + hex;
        var k = { id: M.nextId++, prefix: key.slice(0, 8), name: body.name, created_at: new Date().toISOString(), last_used_at: null };
        M.keys.unshift(k);
        return { id: k.id, key: key, prefix: k.prefix, name: body.name };
      }
      if ((m = /^DELETE \/keys\/(.+)$/.exec(route))) {
        var kb = M.keys.length;
        M.keys = M.keys.filter(function (x) { return String(x.id) !== m[1]; });
        if (M.keys.length === kb) throw mockErr('not_found', 404);
        return { revoked: true };
      }
      throw mockErr('not_found', 404);
    });
  }

  // ?mock=1&open=result|history|watches|account jumps straight to a screen,
  // which is handy for screenshots.
  function mockAutoOpen() {
    if (!MOCK) return;
    var open = qs.get('open');
    if (open === 'result') runScreen(qs.get('address') || MOCK_ADDR.sample, '');
    else if (open === 'history' || open === 'watches' || open === 'account') navigate(open);
  }

  /* ------------------------------------------------------------------ */
  /* Start                                                               */
  /* ------------------------------------------------------------------ */

  S.lang = initialLang();
  document.documentElement.lang = S.lang;
  if (tg) {
    try { tg.ready(); tg.expand(); } catch (_) { /* outside Telegram */ }
    if (tgv('7.7') && tg.disableVerticalSwipes) { try { tg.disableVerticalSwipes(); } catch (_) { /* optional */ } }
    if (IN_TG) {
      tg.onEvent('themeChanged', applyTheme);
      if (tgv('6.1') && tg.BackButton) tg.BackButton.onClick(goBack);
      if (tg.MainButton) tg.MainButton.onClick(submitScreen);
    }
  }
  applyTheme();
  boot();
})();
