/*
 * Kiro-Go admin UI logic.
 */
(() => {
  'use strict';

  // State
  const baseUrl = location.origin;
  if (localStorage.getItem('kiro_remember') !== '1') {
    localStorage.removeItem('admin_password');
    localStorage.removeItem('admin_login_time');
  }
  let password = sessionStorage.getItem('admin_password') || localStorage.getItem('admin_password') || '';
  const supportedLangs = ['zh', 'en', 'vi'];
  let currentLang = localStorage.getItem('kiro_lang') || 'zh';
  if (!supportedLangs.includes(currentLang)) currentLang = 'zh';
  const dict = { en: null, zh: null, vi: null };
  let accountsData = [];
  const selectedAccounts = new Set();
  let filterKeyword = '';
  let filterStatus = 'all';
  let privacyModeEnabled = true;
  let promptRules = [];
  let builderIdSession = '';
  let builderIdPollTimer = null;
  let iamSession = '';
  let microsoftSession = '';
  let microsoftSelectionId = '';
  let microsoftStage = 'kiro';
  let microsoftAuthorizeUrl = '';
  let microsoftProfiles = [];
  let microsoftSelectedProfileArn = '';
  let microsoftBusy = false;
  let microsoftGeneration = 0;
  let exportSelectedIds = new Set();
  let currentVersion = '';
  let testLogs = [];
  let testModalAccountId = '';
  let testModalModels = [];
  let testModalLoadingModels = false;
  let testModalModelError = false;
  let testModalRunning = false;
  let customSelectUid = 0;
  let customSelectObserver = null;
  let customSelectRefreshQueued = false;
  // A dropdown this long is faster to type into than to scroll, so selects at or
  // above this many options grow a filter box. data-search="true"/"false" on the
  // <select> overrides the guess either way.
  const CUSTOM_SELECT_SEARCH_MIN = 8;
  let netInterfacesData = null;
  let customApiAddr = localStorage.getItem('kiro_api_custom_addr') || '';
  let securityWarnings = [];
  let injectFeatureAvailable = false;

  // DOM helpers
  const $ = (id) => document.getElementById(id);
  const qsa = (sel, root) => Array.from((root || document).querySelectorAll(sel));
  function escapeHtml(s) {
    const d = document.createElement('div');
    d.textContent = s == null ? '' : String(s);
    return d.innerHTML;
  }
  function escapeAttr(s) {
    return escapeHtml(s).replace(/"/g, '&quot;');
  }
  // Trigger a browser download of `data` serialized as pretty JSON.
  function downloadJson(filename, data) {
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = filename;
    a.click();
    URL.revokeObjectURL(url);
  }
  // Today as YYYY-MM-DD, for download filenames.
  function todayStamp() {
    return new Date().toISOString().slice(0, 10);
  }
  async function copyText(input) {
    const isPromise = input && typeof input.then === 'function';
    if (isPromise && typeof ClipboardItem !== 'undefined' && navigator.clipboard && navigator.clipboard.write) {
      const blobPromise = Promise.resolve(input).then(t => new Blob([String(t == null ? '' : t)], { type: 'text/plain' }));
      await navigator.clipboard.write([new ClipboardItem({ 'text/plain': blobPromise })]);
      return;
    }
    const text = isPromise ? await input : input;
    const str = String(text == null ? '' : text);
    if (navigator.clipboard && navigator.clipboard.writeText) {
      try {
        await navigator.clipboard.writeText(str);
        return;
      } catch (e) { }
    }
    const ta = document.createElement('textarea');
    ta.value = str;
    ta.readOnly = true;
    ta.className = 'clipboard-proxy';
    document.body.appendChild(ta);
    const range = document.createRange();
    range.selectNodeContents(ta);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
    ta.setSelectionRange(0, str.length);
    document.execCommand('copy');
    sel.removeAllRanges();
    document.body.removeChild(ta);
  }
  function renderEndpointCode(id, value) {
    const el = $(id);
    if (!el) return;
    const raw = String(value || '');
    el.dataset.rawValue = raw;
    try {
      const url = new URL(raw);
      const path = url.pathname + url.search + url.hash;
      el.innerHTML =
        '<span class="api-code-protocol">' + escapeHtml(url.protocol + '//') + '</span>' +
        '<span class="api-code-host">' + escapeHtml(url.host) + '</span>' +
        '<span class="api-code-path">' + escapeHtml(path) + '</span>';
    } catch (e) {
      el.textContent = raw;
    }
  }

  // i18n
  async function loadLocale(lang) {
    if (dict[lang]) return dict[lang];
    try {
      const res = await fetch('/admin/locales/' + lang + '.json?v=' + Date.now(), { cache: 'no-store' });
      dict[lang] = await res.json();
    } catch (e) {
      dict[lang] = {};
    }
    return dict[lang];
  }
  function t(key, ...args) {
    const active = dict[currentLang] || {};
    const fallback = dict.zh || {};
    let text = active[key] || fallback[key] || key;
    args.forEach((arg, idx) => { text = text.replace('{' + idx + '}', arg); });
    return text;
  }
  function applyTranslations() {
    qsa('[data-i18n]').forEach(el => { el.textContent = t(el.dataset.i18n); });
    qsa('[data-i18n-placeholder]').forEach(el => { el.placeholder = t(el.dataset.i18nPlaceholder); });
    qsa('[data-i18n-title]').forEach(el => { el.title = t(el.dataset.i18nTitle); });
    qsa('[data-i18n-aria-label]').forEach(el => { el.setAttribute('aria-label', t(el.dataset.i18nAriaLabel)); });
    document.title = t('app.title');
    document.documentElement.lang = currentLang;
    updateLangSelects();
    applyTheme(getThemePref());
    refreshCustomSelects();
  }
  async function setLang(lang) {
    if (!supportedLangs.includes(lang)) lang = 'zh';
    currentLang = lang;
    localStorage.setItem('kiro_lang', lang);
    await loadLocale(lang);
    applyTranslations();
    renderVersionBadge();
    renderAccounts();
    renderPromptRules();
    renderUpstreams();
    renderModelRoutes();
    buildApiAddrOptions();
    renderSecurityWarnings();
    renderLogs(logsCache);
    renderStatsTable();
    renderStatsCompare();
    renderStatsTrend(statsTrendLast.buckets, statsTrendLast.unit);
    renderApiKeys();
    // Inline stats and any expanded detail panels are innerHTML-built, so they
    // need an explicit re-render to pick up the new locale.
    renderProviderInlineStats();
    Object.keys(openProviderDetails).forEach(pid => {
      if (!providerDetailCache[pid]) return;
      qsa('[data-provider-detail="' + cssEscape(pid) + '"]').forEach(el => {
        el.innerHTML = renderProviderDetailHTML(providerDetailCache[pid]);
      });
    });
  }
  function updateLangSelects() {
    qsa('.lang-select').forEach(sel => {
      if (sel.value !== currentLang) sel.value = currentLang;
      // The custom-select overlay caches the trigger label, so it needs a nudge
      // whenever the value or the option text changes underneath it.
      syncCustomSelect(sel);
    });
  }

  // Custom select
  function getCustomSelectLabel(select) {
    const option = select.selectedOptions && select.selectedOptions[0];
    return ((option && option.textContent) || select.value || '').trim();
  }
  function syncCustomSelect(select) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const value = wrap.querySelector('.custom-select-value');
    const trigger = wrap.querySelector('.custom-select-trigger');
    if (value) value.textContent = getCustomSelectLabel(select);
    if (trigger) trigger.disabled = select.disabled;
    wrap.classList.toggle('is-disabled', select.disabled);
    qsa('.custom-select-option', wrap).forEach(option => {
      const selected = option.dataset.index === String(select.selectedIndex);
      option.classList.toggle('is-selected', selected);
      option.setAttribute('aria-selected', String(selected));
    });
  }
  // A select is searchable when it opts in explicitly, or when it simply has too
  // many options to scan by eye. Deciding per-select (rather than globally) keeps
  // two-item toggles free of a pointless filter box.
  function customSelectSearchable(select) {
    const flag = select.dataset.search;
    if (flag === 'true') return true;
    if (flag === 'false') return false;
    return select.options.length >= CUSTOM_SELECT_SEARCH_MIN;
  }
  function renderCustomSelectOptions(select) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const content = wrap.querySelector('.custom-select-content');
    const trigger = wrap.querySelector('.custom-select-trigger');
    if (!content) return;
    if (trigger) labelCustomSelect(select, trigger, content, select.id);
    // Options live in their own box so re-rendering the list never destroys the
    // search input the user is typing into.
    const box = wrap.querySelector('.custom-select-options') || content;
    const searchWrap = wrap.querySelector('.custom-select-search');
    const input = wrap.querySelector('.custom-select-search-input');
    const searchable = customSelectSearchable(select);
    if (searchWrap) searchWrap.hidden = !searchable;
    if (input) {
      input.placeholder = t('common.searchPlaceholder');
      input.setAttribute('aria-label', t('common.searchPlaceholder'));
      if (!searchable) input.value = '';
    }
    const kw = (searchable && input ? input.value : '').trim().toLowerCase();
    box.innerHTML = '';
    let shown = 0;
    Array.from(select.options).forEach((option, index) => {
      const label = (option.textContent || option.value || '').trim();
      if (kw && !label.toLowerCase().includes(kw)) return;
      const item = document.createElement('button');
      item.type = 'button';
      item.className = 'custom-select-option';
      item.setAttribute('role', 'option');
      item.dataset.index = String(index);
      item.disabled = option.disabled;
      item.textContent = label;
      box.appendChild(item);
      shown++;
    });
    if (!shown) {
      const empty = document.createElement('div');
      empty.className = 'custom-select-empty muted-text text-xs';
      empty.textContent = t('common.noMatch');
      box.appendChild(empty);
    }
    syncCustomSelect(select);
  }
  function placeCustomSelectContent(select) {
    const wrap = select && select.__customSelect;
    if (!wrap || !wrap.classList.contains('is-open')) return;
    const trigger = wrap.querySelector('.custom-select-trigger');
    const content = wrap.querySelector('.custom-select-content');
    if (!trigger || !content) return;
    const rect = trigger.getBoundingClientRect();
    const gap = 4;
    const below = window.innerHeight - rect.bottom - gap;
    const above = rect.top - gap;
    const openUp = below < 180 && above > below;
    const available = Math.max(96, Math.min(224, (openUp ? above : below) - 4));
    content.style.left = Math.round(rect.left) + 'px';
    content.style.width = Math.round(rect.width) + 'px';
    content.style.maxHeight = Math.round(available) + 'px';
    content.style.top = openUp ? 'auto' : Math.round(rect.bottom + gap) + 'px';
    content.style.bottom = openUp ? Math.round(window.innerHeight - rect.top + gap) + 'px' : 'auto';
    content.dataset.side = openUp ? 'top' : 'bottom';
  }
  function setCustomSelectOpen(select, open) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const trigger = wrap.querySelector('.custom-select-trigger');
    const content = wrap.querySelector('.custom-select-content');
    if (!trigger || !content) return;
    if (open && !select.disabled) {
      closeAllCustomSelects(select);
      renderCustomSelectOptions(select);
      wrap.classList.add('is-open');
      trigger.setAttribute('aria-expanded', 'true');
      content.hidden = false;
      placeCustomSelectContent(select);
      requestAnimationFrame(() => placeCustomSelectContent(select));
      // On a searchable select the point of opening is usually to type, so focus
      // goes to the filter box; otherwise it lands on the current option so the
      // arrow keys work straight away.
      const input = wrap.querySelector('.custom-select-search-input');
      if (customSelectSearchable(select) && input) {
        input.value = '';
        renderCustomSelectOptions(select);
        input.focus({ preventScroll: true });
      } else {
        const selected = content.querySelector('.custom-select-option.is-selected:not(:disabled)') || content.querySelector('.custom-select-option:not(:disabled)');
        if (selected) selected.focus({ preventScroll: true });
      }
    } else {
      wrap.classList.remove('is-open');
      trigger.setAttribute('aria-expanded', 'false');
      content.hidden = true;
    }
  }
  function closeAllCustomSelects(except) {
    qsa('select.custom-select-native').forEach(select => {
      if (select !== except) setCustomSelectOpen(select, false);
    });
  }
  function chooseCustomSelectOption(select, index) {
    const option = select.options[index];
    if (!option || option.disabled) return;
    select.value = option.value;
    select.dispatchEvent(new Event('input', { bubbles: true }));
    select.dispatchEvent(new Event('change', { bubbles: true }));
    syncCustomSelect(select);
    setCustomSelectOpen(select, false);
    const trigger = select.__customSelect && select.__customSelect.querySelector('.custom-select-trigger');
    if (trigger && trigger.isConnected) trigger.focus({ preventScroll: true });
  }
  function focusSiblingCustomOption(current, dir) {
    const options = qsa('.custom-select-option:not(:disabled)', current.parentElement);
    const index = options.indexOf(current);
    const next = options[(index + dir + options.length) % options.length];
    if (next) next.focus({ preventScroll: true });
  }
  function getCustomSelectLabelElement(select) {
    const explicit = qsa('label').find(label => label.htmlFor === select.id);
    if (explicit) return explicit;
    const group = select.closest('.form-group');
    return group ? group.querySelector('label') : null;
  }
  function labelCustomSelect(select, trigger, content, id) {
    trigger.id = id + '-trigger';
    const valueId = id + '-value';
    const value = trigger.querySelector('.custom-select-value');
    if (value) value.id = valueId;
    const label = getCustomSelectLabelElement(select);
    if (label) {
      if (!label.id) label.id = id + '-label';
      trigger.removeAttribute('aria-label');
      trigger.setAttribute('aria-labelledby', label.id + ' ' + valueId);
    } else {
      trigger.removeAttribute('aria-labelledby');
      trigger.setAttribute('aria-label', select.getAttribute('aria-label') || getCustomSelectLabel(select));
    }
    content.setAttribute('aria-labelledby', trigger.id);
  }
  function enhanceCustomSelect(select) {
    if (!select || select.__customSelect || select.dataset.nativeSelect === 'true') return;

    const id = select.id || 'custom-select-' + (++customSelectUid);
    if (!select.id) select.id = id;

    const wrap = document.createElement('div');
    wrap.className = 'custom-select';
    wrap.dataset.customSelect = 'true';
    if (select.id === 'filterStatusSelect') wrap.classList.add('custom-select-filter');

    const trigger = document.createElement('button');
    trigger.type = 'button';
    trigger.className = 'custom-select-trigger';
    trigger.setAttribute('aria-haspopup', 'listbox');
    trigger.setAttribute('aria-expanded', 'false');
    trigger.setAttribute('aria-controls', id + '-menu');
    trigger.innerHTML =
      '<span class="custom-select-value"></span>' +
      '<i class="fa-solid fa-chevron-down custom-select-icon" aria-hidden="true"></i>';

    const content = document.createElement('div');
    content.id = id + '-menu';
    content.className = 'custom-select-content';
    content.setAttribute('role', 'listbox');
    content.hidden = true;

    // The filter box is built for every select but stays hidden unless the
    // select is searchable, so option-count changes only have to flip `hidden`
    // rather than rebuild the popover.
    const searchWrap = document.createElement('div');
    searchWrap.className = 'custom-select-search';
    const searchInput = document.createElement('input');
    searchInput.type = 'text';
    searchInput.className = 'custom-select-search-input';
    searchInput.autocomplete = 'off';
    searchWrap.appendChild(searchInput);
    const optionsBox = document.createElement('div');
    optionsBox.className = 'custom-select-options';
    content.appendChild(searchWrap);
    content.appendChild(optionsBox);
    labelCustomSelect(select, trigger, content, id);

    wrap.appendChild(trigger);
    wrap.appendChild(content);
    select.insertAdjacentElement('afterend', wrap);
    select.classList.add('custom-select-native');
    select.setAttribute('aria-hidden', 'true');
    select.tabIndex = -1;
    select.__customSelect = wrap;
    wrap.__nativeSelect = select;

    trigger.addEventListener('click', () => setCustomSelectOpen(select, !wrap.classList.contains('is-open')));
    trigger.addEventListener('keydown', e => {
      if (['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(e.key)) {
        e.preventDefault();
        setCustomSelectOpen(select, true);
      }
    });
    // Typing refilters in place. Re-rendering only touches the options box, so
    // the input keeps both its value and focus.
    searchInput.addEventListener('input', () => {
      renderCustomSelectOptions(select);
      placeCustomSelectContent(select);
    });
    searchInput.addEventListener('keydown', e => {
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        // Step into the list; from the box itself, Down starts at the top and Up
        // wraps to the bottom.
        const options = qsa('.custom-select-option:not(:disabled)', optionsBox);
        if (!options.length) return;
        e.preventDefault();
        (e.key === 'ArrowDown' ? options[0] : options[options.length - 1]).focus({ preventScroll: true });
      } else if (e.key === 'Enter') {
        // Enter on a filtered-to-one list is the fast path: pick it without
        // making the user arrow down first.
        const first = optionsBox.querySelector('.custom-select-option:not(:disabled)');
        if (!first) return;
        e.preventDefault();
        chooseCustomSelectOption(select, parseInt(first.dataset.index, 10));
      } else if (e.key === 'Escape') {
        e.preventDefault();
        setCustomSelectOpen(select, false);
        trigger.focus({ preventScroll: true });
      }
    });
    // Clicks inside the filter box must not reach the document-level
    // outside-click handler that closes every open select.
    searchWrap.addEventListener('click', e => e.stopPropagation());
    content.addEventListener('click', e => {
      const option = e.target.closest('.custom-select-option');
      if (!option) return;
      chooseCustomSelectOption(select, parseInt(option.dataset.index, 10));
    });
    content.addEventListener('keydown', e => {
      const option = e.target.closest('.custom-select-option');
      if (!option) return;
      if (e.key === 'ArrowDown') { e.preventDefault(); focusSiblingCustomOption(option, 1); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); focusSiblingCustomOption(option, -1); }
      else if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); chooseCustomSelectOption(select, parseInt(option.dataset.index, 10)); }
      else if (e.key === 'Escape') { e.preventDefault(); setCustomSelectOpen(select, false); trigger.focus({ preventScroll: true }); }
    });
    select.addEventListener('change', () => syncCustomSelect(select));
    renderCustomSelectOptions(select);
  }
  function enhanceCustomSelects(root) {
    qsa('select:not(.custom-select-native)', root || document).forEach(enhanceCustomSelect);
  }
  function refreshCustomSelects(root) {
    enhanceCustomSelects(root);
    qsa('select.custom-select-native', root || document).forEach(renderCustomSelectOptions);
  }
  function positionOpenCustomSelects() {
    qsa('select.custom-select-native').forEach(placeCustomSelectContent);
  }
  function queueCustomSelectRefresh() {
    if (customSelectRefreshQueued) return;
    customSelectRefreshQueued = true;
    requestAnimationFrame(() => {
      customSelectRefreshQueued = false;
      refreshCustomSelects();
      positionOpenCustomSelects();
    });
  }
  function initCustomSelectObserver() {
    if (customSelectObserver || !document.body || typeof MutationObserver === 'undefined') return;
    customSelectObserver = new MutationObserver(mutations => {
      let shouldRefresh = false;
      for (const mutation of mutations) {
        const target = mutation.target;
        if (target && target.closest && target.closest('.custom-select')) continue;
        if (target && target.matches && target.matches('select')) {
          shouldRefresh = true;
          break;
        }
        for (const node of mutation.addedNodes || []) {
          if (node.nodeType !== 1) continue;
          if ((node.matches && node.matches('select')) || (node.querySelector && node.querySelector('select'))) {
            shouldRefresh = true;
            break;
          }
        }
        if (shouldRefresh) break;
      }
      if (shouldRefresh) queueCustomSelectRefresh();
    });
    customSelectObserver.observe(document.body, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ['disabled', 'class', 'id', 'data-native-select']
    });
  }

  // Theme
  const THEME_ORDER = ['system', 'light', 'dark'];
  const themeMQ = window.matchMedia('(prefers-color-scheme: dark)');
  function resolveTheme(pref) {
    if (pref === 'dark') return 'dark';
    if (pref === 'light') return 'light';
    return themeMQ.matches ? 'dark' : 'light';
  }
  function applyTheme(pref) {
    const resolved = resolveTheme(pref);
    const root = document.documentElement;
    root.classList.toggle('dark', resolved === 'dark');
    root.dataset.themePref = pref;
    qsa('.theme-toggle').forEach(btn => {
      btn.dataset.theme = pref;
      const themeLabel = t('theme.status', t('theme.' + pref));
      btn.setAttribute('aria-label', themeLabel);
      btn.setAttribute('title', themeLabel);
    });
  }
  function getThemePref() {
    const saved = localStorage.getItem('kiro_theme');
    return THEME_ORDER.includes(saved) ? saved : 'system';
  }
  function initTheme() {
    applyTheme(getThemePref());
    themeMQ.addEventListener('change', () => {
      if (getThemePref() === 'system') applyTheme('system');
    });
  }
  function toggleTheme() {
    const cur = getThemePref();
    const next = THEME_ORDER[(THEME_ORDER.indexOf(cur) + 1) % THEME_ORDER.length];
    localStorage.setItem('kiro_theme', next);
    applyTheme(next);
  }

  // Privacy and email mask
  function initPrivacyMode() {
    const saved = localStorage.getItem('privacyMode');
    privacyModeEnabled = saved === null ? true : saved === 'true';
    const toggle = $('privacyModeToggle');
    if (toggle) toggle.checked = privacyModeEnabled;
  }
  function maskEmail(email) {
    if (!privacyModeEnabled || !email || email.indexOf('@') === -1) return email;
    const [local, domain] = email.split('@');
    const maskedLocal = local.length <= 2 ? local : local.substring(0, 2) + '***';
    const parts = domain.split('.');
    if (parts.length >= 2) {
      const tld = parts[parts.length - 1];
      const sld = parts[parts.length - 2];
      const maskedSld = sld.length <= 2 ? sld : sld.substring(0, 2) + '***';
      const subs = parts.slice(0, -2).map(s => s.length <= 2 ? s : s.substring(0, 2) + '***');
      return maskedLocal + '@' + [...subs, maskedSld, tld].join('.');
    }
    return maskedLocal + '@' + domain;
  }
  function getDisplayEmail(email, id) {
    const raw = email || (id ? id.substring(0, 12) + '...' : '-');
    return maskEmail(raw);
  }

  // Toast bridge
  const toast = function (msg, variant, opts) {
    if (typeof window.toast === 'function') return window.toast(msg, variant, opts);
    try { console.warn('[toast missing]', variant, msg); } catch (_) { }
    return function () {};
  };
  const toastPrimary = (msg, opts) => toast(msg, 'primary', opts);
  const toastWarning = (msg, opts) => toast(msg, 'warning', opts);
  const toastError = (msg, opts) => toast(msg, 'error', opts);

  // Modal helpers
  let modalScrollY = 0;
  let confirmResolve = null;
  const modalFocusStack = [];
  function lockModalScroll() {
    if (document.body.classList.contains('modal-open')) return;
    modalScrollY = window.scrollY || document.documentElement.scrollTop || 0;
    document.body.style.top = '-' + modalScrollY + 'px';
    document.body.classList.add('modal-open');
  }
  function unlockModalScrollIfIdle() {
    if (qsa('.modal.active').length > 0) return;
    if (!document.body.classList.contains('modal-open')) return;
    document.body.classList.remove('modal-open');
    document.body.style.top = '';
    window.scrollTo(0, modalScrollY);
  }
  function getModalFocusable(modal) {
    return qsa('a[href], button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])', modal)
      .filter(el => !el.closest('[hidden]'));
  }
  function prepareDialog(modal) {
    modal.setAttribute('role', 'dialog');
    modal.setAttribute('aria-modal', 'true');
    modal.setAttribute('aria-hidden', 'false');
    if (!modal.hasAttribute('tabindex')) modal.tabIndex = -1;
    const title = modal.querySelector('.modal-title');
    if (title) {
      if (!title.id) title.id = modal.id + 'Title';
      modal.setAttribute('aria-labelledby', title.id);
    }
  }
  function focusDialog(modal) {
    if (modal.contains(document.activeElement) && document.activeElement !== modal) return;
    const focusable = getModalFocusable(modal);
    const target = focusable[0] || modal;
    if (target && target.focus) target.focus({ preventScroll: true });
  }
  function trapDialogFocus(e) {
    const modal = e.currentTarget;
    if (e.key !== 'Tab' || !modal.classList.contains('active')) return;
    const focusable = getModalFocusable(modal);
    if (!focusable.length) {
      e.preventDefault();
      modal.focus({ preventScroll: true });
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus({ preventScroll: true });
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus({ preventScroll: true });
    }
  }
  function isDialogOpen(id) {
    const modal = $(id);
    return !!modal && modal.classList.contains('active');
  }
  function openDialog(id) {
    const modal = $(id);
    if (!modal) return;
    prepareDialog(modal);
    modalFocusStack.push({ id, el: document.activeElement });
    modal.removeEventListener('keydown', trapDialogFocus);
    modal.addEventListener('keydown', trapDialogFocus);
    modal.classList.add('active');
    lockModalScroll();
    focusDialog(modal);
    setTimeout(() => focusDialog(modal), 0);
  }
  function closeDialog(id) {
    const modal = $(id);
    if (!modal) return;
    modal.classList.remove('active');
    modal.setAttribute('aria-hidden', 'true');
    const stackIndex = modalFocusStack.map(item => item.id).lastIndexOf(id);
    const previous = stackIndex >= 0 ? modalFocusStack.splice(stackIndex, 1)[0].el : null;
    unlockModalScrollIfIdle();
    if (previous && previous.isConnected && previous.focus) {
      requestAnimationFrame(() => previous.focus({ preventScroll: true }));
    }
  }
  function bindDialogBackdropClose(id, closeFn) {
    const modal = $(id);
    if (!modal) return;
    let startedOnBackdrop = false;
    modal.addEventListener('pointerdown', e => {
      startedOnBackdrop = e.target === modal;
    });
    modal.addEventListener('click', e => {
      if (startedOnBackdrop && e.target === modal) closeFn();
      startedOnBackdrop = false;
    });
  }
  function closeConfirm(value) {
    if (!confirmResolve) return;
    const resolve = confirmResolve;
    confirmResolve = null;
    closeDialog('confirmModal');
    resolve(!!value);
  }
  function confirmAction(message, opts) {
    opts = opts || {};
    if (confirmResolve) closeConfirm(false);
    const modal = $('confirmModal');
    const title = $('confirmTitle');
    const msg = $('confirmMessage');
    const ok = $('confirmOk');
    const cancel = $('confirmCancel');
    const close = $('confirmClose');
    if (!modal || !title || !msg || !ok || !cancel || !close) {
      return Promise.resolve(false);
    }
    title.textContent = opts.title || t('common.confirm');
    msg.textContent = message || '';
    ok.textContent = opts.confirmText || t('common.confirm');
    cancel.textContent = opts.cancelText || t('common.cancel');
    ok.className = 'btn ' + (opts.variant === 'danger' ? 'btn-danger' : 'btn-primary');
    cancel.className = 'btn btn-secondary';
    ok.onclick = () => closeConfirm(true);
    cancel.onclick = () => closeConfirm(false);
    close.onclick = () => closeConfirm(false);
    const pending = new Promise(resolve => { confirmResolve = resolve; });
    openDialog('confirmModal');
    ok.focus({ preventScroll: true });
    return pending;
  }

  // Fetch wrapper
  function api(path, opts) {
    opts = opts || {};
    opts.headers = Object.assign({ 'X-Admin-Password': password }, opts.headers || {});
    if (opts.body && !opts.headers['Content-Type']) opts.headers['Content-Type'] = 'application/json';
    return fetch('/admin/api' + path, opts);
  }

  // Login
  // The SSE log stream (EventSource) cannot send custom headers, so the admin
  // password is mirrored into a cookie that the backend also accepts.
  function setAdminCookie(value) {
    document.cookie = 'admin_password=' + encodeURIComponent(value) + '; path=/; SameSite=Strict';
  }
  function clearAdminCookie() {
    document.cookie = 'admin_password=; path=/; SameSite=Strict; expires=Thu, 01 Jan 1970 00:00:00 GMT';
  }
  function clearActivePassword() {
    sessionStorage.removeItem('admin_password');
    sessionStorage.removeItem('admin_login_time');
    localStorage.removeItem('admin_password');
    localStorage.removeItem('admin_login_time');
    clearAdminCookie();
    password = '';
  }
  function getActiveLoginTime() {
    const storage = sessionStorage.getItem('admin_password') ? sessionStorage : localStorage;
    return parseInt(storage.getItem('admin_login_time') || '0', 10);
  }
  function setActivePassword(nextPassword, remember) {
    const now = Date.now().toString();
    password = nextPassword;
    setAdminCookie(nextPassword);
    sessionStorage.setItem('admin_password', nextPassword);
    sessionStorage.setItem('admin_login_time', now);
    if (remember) {
      localStorage.setItem('admin_password', nextPassword);
      localStorage.setItem('admin_login_time', now);
      localStorage.setItem('kiro_remember', '1');
      localStorage.setItem('kiro_remembered_pwd', nextPassword);
    } else {
      localStorage.removeItem('admin_password');
      localStorage.removeItem('admin_login_time');
      localStorage.removeItem('kiro_remember');
      localStorage.removeItem('kiro_remembered_pwd');
    }
  }
  async function tryAutoLogin() {
    if (!password) return;
    setAdminCookie(password);
    const loginTime = getActiveLoginTime();
    if (loginTime && Date.now() - loginTime > 72 * 3600 * 1000) {
      clearActivePassword();
      return;
    }
    try {
      const res = await api('/status');
      if (res.ok) { showMain(); loadData(); }
    } catch (e) { }
  }
  async function login() {
    password = $('pwdField').value;
    try {
      const res = await api('/status');
      if (res.ok) {
        const remember = $('rememberPwd');
        setActivePassword(password, !!(remember && remember.checked));
        showMain(); loadData();
      } else {
        toast(t('login.error'), 'error');
      }
    } catch (e) {
      toast(t('login.connectError'), 'error');
    }
  }
  function initRememberMe() {
    const remember = $('rememberPwd');
    const field = $('pwdField');
    if (!remember || !field) return;
    if (localStorage.getItem('kiro_remember') === '1') {
      remember.checked = true;
      const saved = localStorage.getItem('kiro_remembered_pwd');
      if (saved) field.value = saved;
    }
  }
  function logout() {
    clearActivePassword();
    location.reload();
  }
  function showMain() {
    $('loginPage').classList.add('hidden');
    $('mainPage').classList.remove('hidden');
  }

  // Data loaders
  async function loadData() {
    await Promise.all([loadStats(), loadAccounts(), loadSettings(), loadVersion(), loadInjectStatus()]);
    // renderApiEndpoints() supersedes the per-endpoint renderEndpointCode calls:
    // it renders the same set from the selected base address.
    renderApiEndpoints();
    loadNetInterfaces();
    setTimeout(checkUpdate, 2000);
  }
  // Base URL currently selected for the API endpoint display. Defaults to how
  // the admin page itself was reached (location.origin).
  function apiEndpointBase() {
    const sel = $('apiAddrSelect');
    if (sel && sel.value) return sel.value;
    return baseUrl;
  }
  function renderApiEndpoints() {
    const base = apiEndpointBase();
    renderEndpointCode('claudeEndpoint', base + '/v1/messages');
    renderEndpointCode('openaiEndpoint', base + '/v1/chat/completions');
    renderEndpointCode('openaiResponsesEndpoint', base + '/v1/responses');
    renderEndpointCode('modelsEndpoint', base + '/v1/models');
    renderEndpointCode('statsEndpoint', base + '/v1/stats');
  }
  // Build a base URL for a given hostname, preserving the protocol and port the
  // browser is currently using (robust when reached via a TLS terminator on a
  // non-default port).
  function baseUrlForHost(host) {
    const port = location.port ? ':' + location.port : '';
    return location.protocol + '//' + host + port;
  }
  async function loadNetInterfaces() {
    const sel = $('apiAddrSelect');
    if (!sel) return;
    try {
      const res = await api('/net-interfaces');
      if (!res.ok) return;
      netInterfacesData = await res.json();
    } catch (e) { return; }
    buildApiAddrOptions();
  }
  // Rebuild the address dropdown from cached interface data. Split from the fetch
  // so it can re-run on language change to re-translate the option labels.
  function buildApiAddrOptions() {
    const sel = $('apiAddrSelect');
    if (!sel || !netInterfacesData) return;
    const data = netInterfacesData;
    const addrs = Array.isArray(data.addrs) ? data.addrs : [];
    const options = [];
    // Localhost always first; value mirrors exactly how the page was reached.
    options.push({ value: baseUrl, label: t('api.addrLocalhost') });
    addrs.forEach(a => {
      if (!a || !a.ip || a.isLoopback) return;
      const ifaceSuffix = a.iface ? ' · ' + a.iface : '';
      options.push({ value: baseUrlForHost(a.ip), label: t('api.addrLan') + ' · ' + a.ip + ifaceSuffix });
    });
    // Manually-entered address (public IP, domain, or tunnel URL) always offered last.
    if (customApiAddr) {
      options.push({ value: customApiAddr, label: t('api.addrCustom') + ' · ' + customApiAddr });
    }

    // Preserve the current selection across a language-triggered rebuild.
    const saved = sel.value || localStorage.getItem('kiro_api_addr');
    sel.innerHTML = '';
    let matched = false;
    options.forEach(opt => {
      const o = document.createElement('option');
      o.value = opt.value;
      o.textContent = opt.label;
      if (opt.value === saved) { o.selected = true; matched = true; }
      sel.appendChild(o);
    });
    if (!matched) sel.value = baseUrl;

    // Warn when a LAN address is offered but the server is not bound to all
    // interfaces, so those addresses are not actually reachable.
    const warn = $('apiAddrWarning');
    if (warn) {
      const hasLan = options.length > 1;
      if (hasLan && data.listensAll === false) {
        warn.querySelector('span').textContent = t('api.addrLanUnreachable', data.host || '127.0.0.1');
        warn.classList.remove('hidden');
      } else {
        warn.classList.add('hidden');
      }
    }

    refreshCustomSelects(sel.parentElement || document);
    renderApiEndpoints();
  }
  function initApiAddrSelect() {
    const sel = $('apiAddrSelect');
    if (!sel) return;
    sel.addEventListener('change', () => {
      localStorage.setItem('kiro_api_addr', sel.value);
      renderApiEndpoints();
    });

    const detectBtn = $('detectPublicIpBtn');
    if (detectBtn) {
      detectBtn.addEventListener('click', async () => {
        const out = $('publicIpResult');
        detectBtn.disabled = true;
        try {
          const res = await api('/public-ip');
          const d = await res.json();
          if (d && d.ok && d.ip) {
            const addr = location.protocol + '//' + d.ip + (d.port ? ':' + d.port : '');
            if (out) {
              out.textContent = t('api.publicIpFound', addr);
              out.classList.remove('hidden');
            }
            // Prefill the manual-address box so the operator can apply it in one click.
            const input = $('customAddrInput');
            if (input && !input.value) input.value = addr;
          } else {
            if (out) {
              out.textContent = t('api.publicIpFailed');
              out.classList.remove('hidden');
            }
          }
        } catch (e) {
          if (out) { out.textContent = t('api.publicIpFailed'); out.classList.remove('hidden'); }
        } finally {
          detectBtn.disabled = false;
        }
      });
    }

    const applyBtn = $('applyCustomAddrBtn');
    if (applyBtn) {
      applyBtn.addEventListener('click', () => {
        const input = $('customAddrInput');
        if (!input) return;
        let v = (input.value || '').trim();
        if (!v) { toast(t('api.customAddrEmpty'), 'error'); return; }
        // Accept a bare host/IP or a full URL; normalize to an origin.
        if (!/^https?:\/\//i.test(v)) {
          const port = location.port ? ':' + location.port : '';
          v = location.protocol + '//' + v + (/:\d+$/.test(v) ? '' : port);
        }
        v = v.replace(/\/+$/, '');
        customApiAddr = v;
        localStorage.setItem('kiro_api_custom_addr', v);
        localStorage.setItem('kiro_api_addr', v);
        buildApiAddrOptions();
        const sel2 = $('apiAddrSelect');
        if (sel2) { sel2.value = v; refreshCustomSelects(sel2.parentElement || document); }
        renderApiEndpoints();
        toast(t('api.customAddrApplied'), 'success');
      });
    }
  }
  async function loadStats() {
    const res = await api('/status');
    const d = await res.json();
    $('statAccounts').textContent = d.accounts || 0;
    $('statRequests').textContent = d.totalRequests || 0;
    $('statSuccess').textContent = d.successRequests || 0;
    $('statFailed').textContent = d.failedRequests || 0;
    $('statTokens').textContent = formatNum(d.totalTokens || 0);
    $('statCredits').textContent = (d.totalCredits || 0).toFixed(1);
  }

  // ===== Logs =====
  let logsFilter = 'all';
  let logsAutoTimer = null;
  let logsCache = [];

  function errorTypeLabel(type) {
    if (!type) return '';
    const key = 'errors.type' + type.charAt(0).toUpperCase() + type.slice(1);
    return t(key) || type;
  }

  function formatLogTime(ts) {
    const d = new Date(ts * 1000);
    const pad = n => String(n).padStart(2, '0');
    return pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' +
      pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
  }

  function accountLabel(id) {
    if (!id) return '-';
    const acc = accountsData.find(a => a.id === id);
    if (acc && acc.email) {
      return privacyModeEnabled ? maskEmail(acc.email) : acc.email;
    }
    return id.slice(0, 8);
  }

  async function loadLogs() {
    try {
      const res = await api('/logs');
      const d = await res.json();
      const logs = d.logs || [];
      renderLogs(logs);
    } catch (e) {
      // silent
    }
  }

  function renderLogs(logs) {
    logsCache = logs;
    const list = $('logsList');
    const summary = $('logsSummary');
    if (!list) return;

    const total = logs.length;
    const okCount = logs.filter(l => l.status === 'success').length;
    const errCount = total - okCount;
    summary.innerHTML =
      '<span>' + escapeHtml(t('logs.total')) + ': <strong>' + total + '</strong></span>' +
      '<span>' + escapeHtml(t('logs.success')) + ': <strong>' + okCount + '</strong></span>' +
      '<span>' + escapeHtml(t('logs.errors')) + ': <strong>' + errCount + '</strong></span>';

    const filtered = logs.filter(l => logsFilter === 'all' || l.status === logsFilter);

    if (!filtered.length) {
      list.innerHTML = '<p class="text-muted">' + escapeHtml(t('logs.empty')) + '</p>';
      return;
    }

    let html = '<table class="logs-table"><thead><tr>' +
      '<th>' + escapeHtml(t('logs.time')) + '</th>' +
      '<th>' + escapeHtml(t('logs.status')) + '</th>' +
      '<th>' + escapeHtml(t('logs.endpoint')) + '</th>' +
      '<th>' + escapeHtml(t('logs.model')) + '</th>' +
      '<th>' + escapeHtml(t('logs.account')) + '</th>' +
      '<th>' + escapeHtml(t('logs.tokens')) + '</th>' +
      '<th>' + escapeHtml(t('logs.duration')) + '</th>' +
      '<th>' + escapeHtml(t('logs.detail')) + '</th>' +
      '</tr></thead><tbody>';
    for (const l of filtered) {
      const isErr = l.status === 'error';
      const statusCell = '<span class="log-status log-status--' + escapeAttr(l.status) + '">' +
        escapeHtml(isErr ? t('logs.statusError') : t('logs.statusSuccess')) + '</span>';
      let detailCell;
      if (isErr) {
        detailCell = '<span class="err-badge err-badge--' + escapeAttr(l.errorType || 'unknown') + '">' +
          escapeHtml(errorTypeLabel(l.errorType || 'unknown')) + '</span> ' +
          '<span class="log-msg" title="' + escapeAttr(l.error) + '">' + escapeHtml(l.error) + '</span>';
      } else {
        detailCell = '<span class="text-muted">' + (l.credits ? (l.credits.toFixed(3) + ' cr') : '-') + '</span>';
      }
      html += '<tr>' +
        '<td>' + escapeHtml(formatLogTime(l.time)) + '</td>' +
        '<td>' + statusCell + '</td>' +
        '<td>' + escapeHtml(l.endpoint) + '</td>' +
        '<td>' + escapeHtml(l.model || '-') + '</td>' +
        '<td>' + escapeHtml(accountLabel(l.accountId)) + '</td>' +
        '<td>' + (l.tokens ? formatNum(l.tokens) : '-') + '</td>' +
        '<td>' + (l.duration ? (l.duration + 'ms') : '-') + '</td>' +
        '<td>' + detailCell + '</td>' +
        '</tr>';
    }
    html += '</tbody></table>';
    list.innerHTML = html;
  }

  async function clearLogs() {
    if (!confirm(t('logs.clearConfirm'))) return;
    await api('/logs', { method: 'DELETE' });
    renderLogs([]);
    toast(t('logs.cleared'), 'success');
  }

  function toggleLogsAutoRefresh() {
    const on = $('logsAutoRefresh').checked;
    if (logsAutoTimer) { clearInterval(logsAutoTimer); logsAutoTimer = null; }
    if (on) {
      logsAutoTimer = setInterval(() => {
        if (!$('tabLogs').classList.contains('hidden')) loadLogs();
      }, 5000);
    }
  }

  async function loadAccounts() {
    const res = await api('/accounts');
    accountsData = await res.json();
    renderAccounts();
  }
  async function loadInjectStatus() {
    const prev = injectFeatureAvailable;
    try {
      const res = await api('/inject/status', { method: 'GET' });
      const d = await res.json();
      injectFeatureAvailable = !!(d && d.available);
    } catch (e) {
      injectFeatureAvailable = false;
    }
    // loadData() runs this in parallel with loadAccounts(); if the account list
    // rendered before this flag resolved, the Inject buttons are missing. Re-render
    // once the flag becomes known so the buttons appear regardless of request order.
    if (injectFeatureAvailable !== prev && accountsData.length) renderAccounts();
  }

  // Account list
  function getFilteredAccounts() {
    return accountsData.filter(a => {
      if (filterStatus === 'enabled' && !a.enabled) return false;
      if (filterStatus === 'disabled' && (a.enabled || (a.banStatus && a.banStatus !== 'ACTIVE'))) return false;
      if (filterStatus === 'banned' && (!a.banStatus || a.banStatus === 'ACTIVE')) return false;
      if (filterKeyword) {
        const kw = filterKeyword.toLowerCase();
        if (!(a.email || '').toLowerCase().includes(kw)) return false;
      }
      return true;
    });
  }
  function onFilterChange() {
    filterKeyword = $('filterSearch').value;
    filterStatus = $('filterStatusSelect').value;
    renderAccounts();
  }
  function toggleSelectAll(checked) {
    const filtered = getFilteredAccounts();
    if (checked) filtered.forEach(a => selectedAccounts.add(a.id));
    else selectedAccounts.clear();
    renderAccounts();
    updateBatchBar();
  }
  function toggleSelectAccount(id) {
    if (selectedAccounts.has(id)) selectedAccounts.delete(id);
    else selectedAccounts.add(id);
    updateBatchBar();
  }
  function updateBatchBar() {
    const bar = $('batchBar');
    const count = selectedAccounts.size;
    const cb = $('selectAllCheckbox');
    if (cb) {
      const filtered = getFilteredAccounts();
      const selectedFiltered = filtered.filter(a => selectedAccounts.has(a.id)).length;
      cb.checked = filtered.length > 0 && selectedFiltered === filtered.length;
      cb.indeterminate = selectedFiltered > 0 && selectedFiltered < filtered.length;
    }
    if (count > 0) {
      bar.classList.remove('hidden');
      $('batchCount').textContent = String(count);
    } else {
      bar.classList.add('hidden');
    }
  }

  function formatSubscriptionLabel(type) {
    const s = (type || '').toUpperCase();
    if (s.includes('POWER')) return t('subscription.power');
    if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return t('subscription.proPlus');
    if (s.includes('PRO')) return t('subscription.pro');
    if (s.includes('FREE')) return t('subscription.free');
    return type || t('subscription.free');
  }
  function getSubBadge(type) {
    const s = (type || '').toUpperCase();
    if (s.includes('POWER')) return '<span class="badge badge-power">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return '<span class="badge badge-proplus">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    if (s.includes('PRO')) return '<span class="badge badge-pro">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    return '<span class="badge badge-free">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
  }
  function getTrialBadge(a) {
    if (a.trialStatus === 'ACTIVE' && a.trialUsageLimit > 0) {
      return '<span class="badge badge-trial">' + escapeHtml(t('accounts.trial')) + '</span>';
    }
    return '';
  }
  function formatTrialExpiry(ts) {
    if (!ts) return '';
    const date = new Date(ts * 1000);
    const diffDays = Math.ceil((date - new Date()) / (1000 * 60 * 60 * 24));
    if (diffDays < 0) return '(' + t('accounts.trialExpired') + ')';
    if (diffDays === 0) return '(' + t('accounts.trialToday') + ')';
    if (diffDays <= 7) return '(' + diffDays + t('accounts.trialDays') + ')';
    return '';
  }
  function formatAuthMethod(method) {
    if (!method) return '-';
    const normalized = String(method).toLowerCase();
    if (normalized === 'external_idp' || normalized === 'azuread') return t('auth.microsoft');
    if (normalized === 'idc') return t('auth.enterprise');
    if (normalized === 'social') return t('auth.social');
    if (normalized === 'api_key' || normalized === 'apikey') return t('auth.apiKey');
    if (normalized === 'builderid') return 'BuilderID';
    if (normalized === 'github') return t('local.providerGithub');
    if (normalized === 'google') return t('local.providerGoogle');
    return method;
  }
  function getStatusBadge(a) {
    const out = [];
    const isBanned = a.banStatus && a.banStatus !== 'ACTIVE';
    if (isBanned) {
      if (a.banStatus === 'BANNED') out.push('<span class="badge badge-banned">' + escapeHtml(t('accounts.banned')) + '</span>');
      else if (a.banStatus === 'SUSPENDED') out.push('<span class="badge badge-suspended">' + escapeHtml(t('accounts.suspended')) + '</span>');
      out.push('<span class="badge badge-warning">' + escapeHtml(t('accounts.disabled')) + '</span>');
    } else {
      if (!a.hasToken)
        out.push('<span class="badge badge-error">' + escapeHtml(t('accounts.noToken')) + '</span>');
      else if (a.expiresAt && a.expiresAt < Date.now() / 1000)
        out.push('<span class="badge badge-warning">' + escapeHtml(t('accounts.expired')) + '</span>');
      else
        out.push('<span class="badge badge-success">' + escapeHtml(t('accounts.normal')) + '</span>');
      out.push(a.enabled
        ? '<span class="badge badge-info">' + escapeHtml(t('accounts.enabled')) + '</span>'
        : '<span class="badge badge-warning">' + escapeHtml(t('accounts.disabled')) + '</span>');
    }
    return out.join('');
  }
  function formatTokenExpiry(ts) {
    if (!ts) return '-';
    const diff = ts - Date.now() / 1000;
    if (diff <= 0) return t('time.expired');
    if (diff < 3600) return Math.floor(diff / 60) + t('time.minutes');
    if (diff < 86400) return Math.floor(diff / 3600) + t('time.hours');
    return Math.floor(diff / 86400) + t('time.days');
  }
  function formatNum(n) {
    if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
    return n.toString();
  }
  function applyUsageBars(root) {
    qsa('.usage-fill[data-usage-pct]', root).forEach(el => {
      const pct = Math.max(0, Math.min(100, parseFloat(el.dataset.usagePct) || 0));
      el.style.width = pct + '%';
    });
  }

  // maskProfileLabel masks the profile identifier under privacy mode. The region
  // is not a secret and stays visible; only the ARN/short-id is masked.
  function maskProfileLabel(label) {
    if (!privacyModeEnabled || !label) return label;
    return label.length <= 4 ? '***' : label.substring(0, 4) + '***';
  }

  // renderProfileLine shows the account's currently pinned Kiro profile
  // (region + short identifier) as a dedicated line, distinct from the auth
  // region badge. Data comes from the /accounts payload — no per-card API call.
  function renderProfileLine(a) {
    if (!a.hasPinnedProfile) {
      return '<div class="account-profile account-profile-none" title="' + escapeAttr(t('accounts.profileNone')) + '">' +
        '<span class="account-profile-label">' + escapeHtml(t('accounts.profileNone')) + '</span>' +
        '</div>';
    }
    const region = a.currentProfileRegion || '';
    const label = maskProfileLabel(a.currentProfileLabel || '');
    const fullTitle = t('accounts.profile') + ': ' + region + ' · ' + (a.currentProfileLabel || '');
    return '<div class="account-profile" title="' + escapeAttr(fullTitle) + '">' +
      '<span class="account-profile-tag">' + escapeHtml(t('accounts.profile')) + '</span>' +
      '<span class="account-profile-region">' + escapeHtml(region) + '</span>' +
      '<span class="account-profile-sep">·</span>' +
      '<span class="account-profile-label">' + escapeHtml(label) + '</span>' +
      '</div>';
  }

  function renderAccounts() {
    const container = $('accountsList');
    if (!container) return;
    const filtered = getFilteredAccounts();
    if (filtered.length === 0) {
      container.innerHTML = '<div class="empty-state">' + escapeHtml(t('accounts.empty')) + '</div>';
      return;
    }
    container.innerHTML = filtered.map(a => {
      const usagePct = (a.usagePercent || 0) * 100;
      const usageClass = usagePct > 90 ? 'critical' : usagePct > 70 ? 'high' : '';
      const trialPct = (a.trialUsagePercent || 0) * 100;
      const trialClass = trialPct > 90 ? 'critical' : trialPct > 70 ? 'high' : '';
      const isSelected = selectedAccounts.has(a.id);
      const weight = a.weight || 0;
      const weightBadge = weight >= 2 ? '<span class="badge badge-warning">' + escapeHtml(t('accounts.weightShort')) + ':' + weight + '</span>' : '';
      const overageBadge = renderOverageBadge(a);
      const banned = a.banStatus && a.banStatus !== 'ACTIVE';
      const idAttr = escapeAttr(a.id);
      const displayEmail = getDisplayEmail(a.email, a.id);
      const selectLabel = t('accounts.selectAccount', displayEmail);

      const refreshSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M23 4v6h-6M1 20v-6h6"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>';
      const userSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/></svg>';
      const copySvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';

      return '' +
        '<div class="account-card' + (isSelected ? ' selected' : '') + '" data-id="' + idAttr + '">' +
        '<div class="account-header">' +
        '<div class="account-info">' +
        '<input type="checkbox" class="account-checkbox" ' + (isSelected ? 'checked' : '') + ' data-id="' + idAttr + '" aria-label="' + escapeAttr(selectLabel) + '" />' +
        '<div class="account-info-text">' +
        '<div class="account-email">' + escapeHtml(displayEmail) + '</div>' +
        '<div class="account-meta">' +
        getSubBadge(a.subscriptionType) +
        getTrialBadge(a) +
        weightBadge +
        overageBadge +
        '<span class="badge badge-info">' + escapeHtml(formatAuthMethod(a.provider || a.authMethod)) + '</span>' +
        getStatusBadge(a) +
        '</div>' +
        '</div>' +
        '</div>' +
        '<div class="account-actions">' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="refresh" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.refresh')) + '">' + refreshSvg + '</button>' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="detail" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.detail')) + '">' + userSvg + '</button>' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="copyJSON" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.copyJSON')) + '">' + copySvg + '</button>' +
        (banned ? '' :
          '<button class="btn btn-sm ' + (a.enabled ? 'btn-outline' : 'btn-primary') + '" data-action="toggle" data-id="' + idAttr + '" data-enabled="' + (!a.enabled) + '">' +
          escapeHtml(a.enabled ? t('accounts.disable') : t('accounts.enable')) +
          '</button>') +
        '<button class="btn btn-sm btn-secondary" data-action="test" data-id="' + idAttr + '" id="test-' + idAttr + '">' + escapeHtml(t('accounts.test')) + '</button>' +
        (injectFeatureAvailable ?
          '<button class="btn btn-sm btn-outline" data-action="inject" data-id="' + idAttr + '" id="inject-' + idAttr + '" title="Ghi credential vào Kiro IDE để đăng nhập account này">Inject</button>' : '') +
        '<button class="btn btn-sm btn-danger" data-action="delete" data-id="' + idAttr + '">' + escapeHtml(t('accounts.delete')) + '</button>' +
        '</div>' +
        '</div>' +
        renderProfileLine(a) +
        (a.usageLimit > 0 ?
          '<div class="account-usage">' +
          '<div class="usage-label">' + escapeHtml(t('accounts.mainQuota')) + '</div>' +
          '<div class="usage-bar"><div class="usage-fill ' + usageClass + '" data-usage-pct="' + escapeAttr(usagePct) + '"></div></div>' +
          '<div class="usage-text"><span>' + (a.usageCurrent != null ? a.usageCurrent.toFixed(1) : 0) + ' / ' + (a.usageLimit != null ? a.usageLimit.toFixed(0) : 0) + '</span><span>' + usagePct.toFixed(1) + '%</span></div>' +
          '</div>' : '') +
        (a.trialUsageLimit > 0 ?
          '<div class="account-usage">' +
          '<div class="usage-label">' + escapeHtml(t('accounts.trialQuota')) + ' ' + escapeHtml(formatTrialExpiry(a.trialExpiresAt)) + '</div>' +
          '<div class="usage-bar"><div class="usage-fill ' + trialClass + '" data-usage-pct="' + escapeAttr(trialPct) + '"></div></div>' +
          '<div class="usage-text"><span>' + (a.trialUsageCurrent != null ? a.trialUsageCurrent.toFixed(1) : 0) + ' / ' + (a.trialUsageLimit != null ? a.trialUsageLimit.toFixed(0) : 0) + '</span><span>' + trialPct.toFixed(1) + '%</span></div>' +
          '</div>' : '') +
        '<div class="account-stats">' +
        '<div class="account-stat"><div class="account-stat-value">' + (a.requestCount || 0) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.requests')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + formatNum(a.totalTokens || 0) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.tokens')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + (a.totalCredits || 0).toFixed(1) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.credits')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + escapeHtml(formatTokenExpiry(a.expiresAt)) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.expiry')) + '</div></div>' +
        '</div>' +
        '</div>';
    }).join('');
    applyUsageBars(container);
    enhanceCustomSelects(container);
  }

  // Account actions
  async function refreshAccount(id, card) {
    if (card) card.classList.add('loading');
    try {
      const res = await api('/accounts/' + id + '/refresh', { method: 'POST' });
      const d = await res.json();
      if (d.success) loadAccounts();
      else toastError(t('accounts.refreshFailed') + ': ' + (d.error || ''));
    } catch (e) {
      toastError(t('accounts.refreshFailed'));
    }
    if (card) card.classList.remove('loading');
  }
  async function injectAccount(id, btn) {
    const acc = accountsData.find(a => a.id === id);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : id;
    const ok = await confirmAction(
      'Ghi credential của "' + email + '" vào Kiro IDE trên máy này? File đăng nhập hiện tại sẽ bị ghi đè. Sau khi xong, hãy khởi động lại Kiro IDE.',
      { title: 'Inject vào Kiro IDE', confirmText: 'Inject' }
    );
    if (!ok) return;
    if (btn) btn.setAttribute('aria-busy', 'true');
    try {
      const res = await api('/inject', { method: 'POST', body: JSON.stringify({ id }) });
      const d = await res.json();
      if (res.ok && d.success) {
        toast(d.message || 'Đã ghi credential. Khởi động lại Kiro IDE để đăng nhập.', 'success', { duration: 8000 });
        if (d.cliNote) toastWarning(d.cliNote, { duration: 8000 });
      } else {
        toastError('Inject thất bại: ' + (d.error || t('common.failed')));
      }
    } catch (e) {
      toastError((e && e.message) || t('common.failed'));
    }
    if (btn) btn.removeAttribute('aria-busy');
  }
  async function toggleAccount(id, enabled) {
    await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify({ enabled }) });
    loadAccounts();
  }
  async function deleteAccount(id) {
    const ok = await confirmAction(t('accounts.confirmDelete'), {
      title: t('accounts.delete'),
      confirmText: t('accounts.delete'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/accounts/' + id, { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('accounts.deleteSuccess'), 'danger', { icon: 'fa-solid fa-trash' });
      loadAccounts(); loadStats();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  function credentialImportPayloadFromFullAccount(a) {
    const payload = {
      clientId: a.clientId || '',
      clientSecret: a.clientSecret || '',
      accessToken: a.accessToken || '',
      refreshToken: a.refreshToken || ''
    };
    if (a.authMethod) payload.authMethod = a.authMethod;
    if (a.provider) payload.provider = a.provider;
    if (a.tokenEndpoint) payload.tokenEndpoint = a.tokenEndpoint;
    if (a.issuerUrl) payload.issuerUrl = a.issuerUrl;
    if (a.scopes) payload.scopes = a.scopes;
    if (a.userId) payload.userId = a.userId;
    if (a.profileArn) payload.profileArn = a.profileArn;
    if (a.region) payload.region = a.region;
    // Kiro Hosted SSO metadata: copied JSON must keep these or the account
    // re-imports as social and fails auth.
    if (a.idpClientId) payload.idpClientId = a.idpClientId;
    if (a.loginHint) payload.loginHint = a.loginHint;
    if (a.idpTokenEndpoint) payload.idpTokenEndpoint = a.idpTokenEndpoint;
    if (a.expiresAt) payload.expiresAt = a.expiresAt;
    return payload;
  }
  function credentialImportPayloadFromExportAccount(a) {
    const credentials = a.credentials || {};
    return credentialImportPayloadFromFullAccount({
      clientId: credentials.clientId,
      clientSecret: credentials.clientSecret,
      accessToken: credentials.accessToken,
      refreshToken: credentials.refreshToken,
      authMethod: credentials.authMethod || a.authMethod,
      provider: credentials.provider || a.provider || a.idp,
      tokenEndpoint: credentials.tokenEndpoint,
      issuerUrl: credentials.issuerUrl,
      scopes: credentials.scopes,
      idpClientId: credentials.idpClientId || a.idpClientId,
      loginHint: credentials.loginHint || a.loginHint,
      idpTokenEndpoint: credentials.idpTokenEndpoint || a.idpTokenEndpoint,
      expiresAt: credentials.expiresAt || a.expiresAt,
      userId: a.userId,
      profileArn: a.profileArn,
      region: credentials.region || a.region
    });
  }
  async function copyAccountJSON(id, btn) {
    try {
      const jsonPromise = api('/accounts/' + id + '/full').then(async res => {
        if (!res.ok) throw new Error('Failed');
        const a = await res.json();
        return JSON.stringify(credentialImportPayloadFromFullAccount(a), null, 2);
      });
      await copyText(jsonPromise);
      flashCopySuccess(btn);
      toastPrimary(t('accounts.copyJSONSuccess'));
    } catch (e) {
      toastError(t('common.failed'));
    }
  }
  function flashCopySuccess(btn) {
    if (!btn) return;
    const html = btn.innerHTML, cls = btn.className;
    btn.disabled = true;
    btn.className = 'btn btn-icon btn-sm btn-success';
    btn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>';
    setTimeout(() => { btn.disabled = false; btn.className = cls; btn.innerHTML = html; }, 800);
  }

  // Batch actions
  async function batchAction(action) {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmKey = 'batch.confirm' + action.charAt(0).toUpperCase() + action.slice(1);
    const ok = await confirmAction(t(confirmKey, ids.length), {
      title: t('common.confirm'),
      confirmText: t('common.confirm'),
      variant: action === 'disable' ? 'danger' : 'primary'
    });
    if (!ok) return;
    const dismiss = toast(t('batch.processing'), 'info', { duration: 0 });
    try {
      const res = await api('/accounts/batch', { method: 'POST', body: JSON.stringify({ ids, action }) });
      const d = await res.json();
      if (!res.ok || !d.success) throw new Error(d.error || t('common.failed'));
      dismiss();
      if (action === 'refresh') {
        toast(t('batch.refreshResult', d.refreshed || 0, d.failed || 0), d.failed ? 'warning' : 'success');
      } else if (action === 'enable') {
        toast(t('batch.enableResult', d.count || ids.length), 'success');
      } else if (action === 'disable') {
        toast(t('batch.disableResult', d.count || ids.length), 'success');
      } else {
        toast(t('batch.done'), 'success');
      }
      selectedAccounts.clear();
      updateBatchBar();
      loadAccounts(); loadStats();
    } catch (e) {
      dismiss();
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function batchRefreshModels() {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmed = await confirmAction(t('batch.confirmRefreshModels', ids.length), {
      title: t('models.refreshAll'),
      confirmText: t('common.confirm')
    });
    if (!confirmed) return;
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    let ok = 0, fail = 0;
    for (const id of ids) {
      try {
        const res = await api('/accounts/' + id + '/models/refresh', { method: 'POST' });
        const d = await res.json();
        if (d.success) ok++; else fail++;
      } catch { fail++; }
    }
    dismiss();
    toast(t('batch.refreshModelsResult', ok, fail), fail ? 'warning' : 'success');
    selectedAccounts.clear();
    updateBatchBar();
    loadAccounts();
  }
  async function batchDelete() {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmed = await confirmAction(t('batch.confirmDelete', ids.length), {
      title: t('accounts.delete'),
      confirmText: t('accounts.delete'),
      variant: 'danger'
    });
    if (!confirmed) return;
    const dismiss = toast(t('batch.deleting'), 'info', { duration: 0 });
    let ok = 0, fail = 0;
    for (const id of ids) {
      try {
        const res = await api('/accounts/' + id, { method: 'DELETE' });
        const d = await res.json().catch(() => ({}));
        if (res.ok && d.success !== false) ok++; else fail++;
      } catch { fail++; }
    }
    dismiss();
    toast(t('batch.deleteResult', ok, fail), fail ? 'warning' : 'success', { icon: 'fa-solid fa-trash' });
    selectedAccounts.clear();
    updateBatchBar();
    loadAccounts(); loadStats();
  }
  async function refreshAllModels() {
    const ok = await confirmAction(t('models.confirmRefreshAll'), {
      title: t('models.refreshAll'),
      confirmText: t('models.refreshAll')
    });
    if (!ok) return;
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/models/refresh', { method: 'POST' });
      const d = await res.json();
      dismiss();
      toast(t('models.refreshAllDone', d.refreshed || 0), 'success');
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    }
  }
  async function refreshAccountModels(id) {
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/' + id + '/models/refresh', { method: 'POST' });
      const d = await res.json();
      dismiss();
      if (d.success) toast(t('detail.refreshModelCache') + ' · ' + (d.count || 0), 'success');
      else toast(t('common.failed') + (d.error ? ': ' + d.error : ''), 'error');
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    }
  }

  // Detail modal
  function detailItem(label, value) {
    return '<div class="detail-item"><div class="detail-label">' + escapeHtml(label) + '</div><div class="detail-value">' + escapeHtml(value) + '</div></div>';
  }
  function showDetail(id) {
    const a = accountsData.find(x => x.id === id);
    if (!a) return;
    const idAttr = escapeAttr(id);
    $('detailBody').innerHTML =
      '<div class="detail-section"><h4>' + escapeHtml(t('detail.basicInfo')) + '</h4><div class="detail-grid">' +
      detailItem(t('detail.email'), getDisplayEmail(a.email, null)) +
      detailItem(t('detail.userId'), a.userId || '-') +
      detailItem(t('detail.authMethod'), formatAuthMethod(a.provider || a.authMethod)) +
      detailItem(t('detail.region'), a.region || 'us-east-1') +
      '</div></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.machineId')) + '</h4><div class="machine-id-row">' +
      '<input type="text" id="machineIdInput" value="' + escapeAttr(a.machineId || '') + '" placeholder="UUID" />' +
      '<button class="btn btn-sm btn-outline" id="generateMachineIdBtn" type="button">' + escapeHtml(t('detail.generate')) + '</button>' +
      '<button class="btn btn-sm btn-primary" data-detail-action="saveMachineId" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.weight')) + '</h4>' +
      '<div class="form-group">' +
      '<input type="number" id="weightInput" value="' + (a.weight || 0) + '" min="0" max="10" />' +
      '<small>' + escapeHtml(t('detail.weightHint')) + '</small>' +
      '</div>' +
      '<button class="btn btn-sm btn-primary" data-detail-action="saveWeight" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div>' +

      '<div class="detail-section">' +
      '<h4>' + escapeHtml(t('detail.overage')) +
      ' <button class="btn btn-sm btn-outline" data-detail-action="refreshOverage" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.overageRefresh')) + '</button>' +
      '</h4>' +
      '<p class="help-block">' + escapeHtml(t('detail.overageHint')) + '</p>' +
      renderOverageBlock(a, idAttr) +
      '</div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.proxyURL')) + '</h4><div class="machine-id-row">' +
      '<input type="text" id="proxyURLInput" value="' + escapeAttr(a.proxyURL || '') + '" placeholder="socks5://host:port" />' +
      '<button class="btn btn-sm btn-primary" data-detail-action="saveProxyURL" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div><p class="help-block">' + escapeHtml(t('detail.proxyHint')) + '</p></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.subscription')) + '</h4><div class="detail-grid">' +
      detailItem(t('detail.subscriptionType'), a.subscriptionTitle || (a.subscriptionType ? formatSubscriptionLabel(a.subscriptionType) : '-')) +
      detailItem(t('detail.tokenExpiry'), a.expiresAt ? new Date(a.expiresAt * 1000).toLocaleString() : '-') +
      detailItem(t('detail.mainQuota'), (a.usageCurrent != null ? a.usageCurrent.toFixed(1) : 0) + ' / ' + (a.usageLimit != null ? a.usageLimit.toFixed(0) : 0)) +
      detailItem(t('detail.resetDate'), a.nextResetDate || '-') +
      (a.trialUsageLimit > 0 ?
        detailItem(t('detail.trialQuota'), (a.trialUsageCurrent != null ? a.trialUsageCurrent.toFixed(1) : 0) + ' / ' + a.trialUsageLimit.toFixed(0)) +
        detailItem(t('detail.trialStatus'), a.trialStatus || '-') +
        detailItem(t('detail.trialExpiry'), a.trialExpiresAt ? new Date(a.trialExpiresAt * 1000).toLocaleString() : '-')
        : '') +
      '</div></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.statistics')) + '</h4><div class="detail-grid">' +
      detailItem(t('detail.requestCount'), a.requestCount || 0) +
      detailItem(t('detail.errorCount'), a.errorCount || 0) +
      detailItem(t('detail.totalTokens'), formatNum(a.totalTokens || 0)) +
      detailItem(t('detail.totalCredits'), (a.totalCredits || 0).toFixed(2)) +
      '</div></div>' +

      '<div class="detail-section">' +
      '<h4>' + escapeHtml(t('detail.models')) +
      ' <button class="btn btn-sm btn-outline" data-detail-action="loadModels" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.loadModels')) + '</button>' +
      ' <button class="btn btn-sm btn-outline" data-detail-action="refreshModels" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.refreshModelCache')) + '</button>' +
      '</h4>' +
      '<div id="modelsList" class="model-list"></div>' +
      '</div>' +

      '<div class="detail-section">' +
      '<h4>' + escapeHtml(t('detail.profiles')) +
      ' <button class="btn btn-sm btn-outline" data-detail-action="discoverProfiles" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.discover')) + '</button>' +
      '</h4>' +
      '<div id="profilesList" class="model-list"><p class="empty-state">' + escapeHtml(t('detail.profilesHint')) + '</p></div>' +
      '</div>';

    openDialog('detailModal');
  }

  // discoverProfiles lists an account's profiles across all candidate regions and
  // renders a picker. Selecting a profile pins it (with a confirm) and refreshes
  // that account's model cache. No secrets are shown or stored.
  async function discoverProfiles(id) {
    const c = $('profilesList');
    if (!c) return;
    c.innerHTML = '<p class="empty-state">' + escapeHtml(t('detail.loading')) + '</p>';
    let btns = document.querySelectorAll('[data-detail-action="discoverProfiles"][data-id="' + id + '"]');
    btns.forEach(b => { b.disabled = true; });
    try {
      const res = await api('/accounts/' + id + '/profiles');
      const d = await res.json();
      if (!res.ok || !d.profiles || d.profiles.length === 0) {
        c.innerHTML = '<p class="message message-error">' + escapeHtml(d.error || t('detail.noProfiles')) + '</p>';
        return;
      }
      let pinnedArn = '';
      try {
        const pr = await api('/accounts/' + id + '/profile');
        const pd = await pr.json();
        if (pd.pinned) pinnedArn = pd.pinned.arn || '';
      } catch (e) { /* non-fatal */ }

      const warn = (d.regionErrors && Object.keys(d.regionErrors).length)
        ? '<p class="message message-warning">' + escapeHtml(t('detail.profileRegionsFailed', Object.keys(d.regionErrors).join(', '))) + '</p>'
        : '';
      c.innerHTML = warn + d.profiles.map(p => {
        const isPinned = p.arn === pinnedArn;
        const tag = isPinned ? ' <span class="credit-ratio">' + escapeHtml(t('detail.profilePinned')) + '</span>' : '';
        const disabled = isPinned ? ' disabled' : '';
        return '<div class="model-item">' +
          '<div class="model-name">' + escapeHtml(p.displayName || p.arn) + tag + '</div>' +
          '<div class="model-info">' + escapeHtml(p.region) + ' — ' + escapeHtml(p.arn) + '</div>' +
          '<button class="btn btn-sm btn-primary" data-profile-select="1" data-id="' + escapeHtml(id) +
          '" data-arn="' + escapeHtml(p.arn) + '" data-region="' + escapeHtml(p.region) + '" type="button"' + disabled + '>' + escapeHtml(t('detail.profileUse')) + '</button>' +
          '</div>';
      }).join('');
    } catch (e) {
      c.innerHTML = '<p class="message message-error">' + escapeHtml(t('detail.loadFailed')) + '</p>';
    } finally {
      btns.forEach(b => { b.disabled = false; });
    }
  }

  async function selectProfile(id, arn, region, btn) {
    if (!confirm(t('detail.profileSwitchConfirm', arn, region))) return;
    if (btn) btn.disabled = true;
    try {
      const res = await api('/accounts/' + id + '/profile', {
        method: 'POST',
        body: JSON.stringify({ arn: arn, region: region }),
      });
      const d = await res.json();
      if (res.ok && d.success) {
        if (d.modelCacheRefreshed) {
          toast(t('detail.profileSwitchedModels', d.modelCount != null ? d.modelCount : 0), 'success');
        } else {
          toast(t('detail.profileSwitched'), 'success');
        }
        loadAccounts();
        discoverProfiles(id);
      } else {
        toast(d.error || t('detail.profileSwitchFailed'), 'error');
        if (btn) btn.disabled = false;
      }
    } catch (e) {
      toast(t('detail.profileSwitchFailed'), 'error');
      if (btn) btn.disabled = false;
    }
  }
  async function loadModels(id) {
    const c = $('modelsList');
    c.innerHTML = '<p class="empty-state">' + escapeHtml(t('detail.loading')) + '</p>';
    try {
      const res = await api('/accounts/' + id + '/models');
      const d = await res.json();
      if (d.success && d.models) {
        const sorted = d.models.slice().sort((a, b) => {
          if (a.modelId === 'auto') return -1;
          if (b.modelId === 'auto') return 1;
          return (a.rateMultiplier || 1) - (b.rateMultiplier || 1);
        });
        c.innerHTML = sorted.map(m => {
          const ratio = m.rateMultiplier || 1;
          return '<div class="model-item">' +
            '<div class="model-name">' + escapeHtml(m.modelId) + '</div>' +
            '<div class="model-credit"><span class="credit-ratio">' + escapeHtml(t('detail.creditMultiplier', ratio)) + '</span></div>' +
            '<div class="model-info">' + escapeHtml(m.description || '') + '</div>' +
            '</div>';
        }).join('') || '<p class="empty-state">' + escapeHtml(t('detail.noModels')) + '</p>';
      } else {
        c.innerHTML = '<p class="message message-error">' + escapeHtml(t('detail.loadFailed')) + ': ' + escapeHtml(d.error || '') + '</p>';
        toast(t('detail.loadFailed') + (d.error ? ': ' + d.error : ''), 'error');
      }
    } catch (e) {
      c.innerHTML = '<p class="message message-error">' + escapeHtml(t('detail.loadFailed')) + '</p>';
      toast(t('detail.loadFailed'), 'error');
    }
  }
  async function generateMachineId() {
    try {
      const res = await api('/generate-machine-id');
      const d = await res.json();
      if (d.machineId) $('machineIdInput').value = d.machineId;
    } catch (e) {
      toast(t('detail.generateFailed'), 'error');
    }
  }
  async function putAccount(id, body, successMsg) {
    try {
      const res = await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify(body) });
      const d = await res.json();
      if (d.success) {
        toast(successMsg, 'success');
        loadAccounts();
      } else {
        toast(t('detail.saveFailed') + (d.error ? ': ' + d.error : ''), 'error');
      }
    } catch (e) {
      toast(t('detail.saveFailed'), 'error');
    }
  }
  async function saveMachineId(id) {
    const m = $('machineIdInput').value.trim();
    if (m && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(m) && !/^[0-9a-f]{32}$/i.test(m)) {
      toast(t('detail.machineIdError'), 'warning'); return;
    }
    await putAccount(id, { machineId: m }, t('detail.saved'));
  }
  async function saveWeight(id) {
    const weight = parseInt($('weightInput').value, 10) || 0;
    await putAccount(id, { weight }, t('detail.saved'));
  }
  function renderOverageBadge(a) {
    const status = (a.overageStatus || '').toUpperCase();
    if (status === 'ENABLED') {
      return '<span class="badge badge-warning">' + escapeHtml(t('accounts.overageOn')) + '</span>';
    }
    if (status === 'DISABLED') {
      return '<span class="badge badge-muted">' + escapeHtml(t('accounts.overageOff')) + '</span>';
    }
    return '';
  }
  function renderOverageBlock(a, idAttr) {
    const status = (a.overageStatus || '').toUpperCase();
    const capable = !a.overageCapability || a.overageCapability === 'OVERAGE_CAPABLE';
    const checked = status === 'ENABLED';
    const checkedAt = a.overageCheckedAt ? new Date(a.overageCheckedAt * 1000).toLocaleString() : '-';
    const statusText = status === 'ENABLED' ? t('detail.overageEnabled')
      : status === 'DISABLED' ? t('detail.overageDisabled')
      : t('detail.overageUnknown');
    const disabledAttr = capable ? '' : ' disabled';
    return '<div class="form-group flex items-center gap-2">' +
      '<label class="switch"><input type="checkbox" id="overageSwitchInput-' + idAttr + '" data-detail-action="toggleOverage" data-id="' + idAttr + '" ' + (checked ? 'checked' : '') + disabledAttr + ' /><span class="slider"></span></label>' +
      '<span id="overageSwitchLabel-' + idAttr + '">' + escapeHtml(statusText) + '</span>' +
      '</div>' +
      (capable ? '' : '<p class="help-block" style="color:#ef4444">' + escapeHtml(t('detail.overageNotCapable')) + '</p>') +
      '<div class="detail-grid">' +
      detailItem(t('detail.overageStatus'), status || '-') +
      detailItem(t('detail.overageCap'), a.overageCap ? '$' + Number(a.overageCap).toFixed(2) : '-') +
      detailItem(t('detail.overageRate'), a.overageRate ? '$' + Number(a.overageRate).toFixed(4) : '-') +
      detailItem(t('detail.overageCurrent'), a.currentOverages ? '$' + Number(a.currentOverages).toFixed(4) : '$0') +
      detailItem(t('detail.overageCheckedAt'), checkedAt) +
      '</div>';
  }
  async function toggleOverageSwitch(id, inputEl) {
    const desired = inputEl.checked;
    const labelEl = $('overageSwitchLabel-' + id);
    const oldLabel = labelEl ? labelEl.textContent : '';
    inputEl.disabled = true;
    if (labelEl) labelEl.textContent = t('detail.overageSwitching');
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/overage', {
        method: 'POST',
        body: JSON.stringify({ enabled: desired }),
      });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) {
        throw new Error(d.error || t('accounts.overageSwitchFailed'));
      }
      if (labelEl) {
        labelEl.textContent = d.overageStatus === 'ENABLED' ? t('detail.overageEnabled')
          : d.overageStatus === 'DISABLED' ? t('detail.overageDisabled')
          : t('detail.overageUnknown');
      }
      inputEl.checked = d.overageStatus === 'ENABLED';
      await loadAccounts();
    } catch (e) {
      inputEl.checked = !desired;
      if (labelEl) labelEl.textContent = oldLabel;
      toast(t('accounts.overageSwitchFailed') + ': ' + (e.message || e), 'warning');
    } finally {
      inputEl.disabled = false;
    }
  }
  async function refreshAccountOverage(id) {
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/overage', { method: 'GET' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) {
        throw new Error(d.error || t('accounts.overageSwitchFailed'));
      }
      await loadAccounts();
      showDetail(id);
    } catch (e) {
      toast(t('accounts.overageSwitchFailed') + ': ' + (e.message || e), 'warning');
    }
  }
  async function saveProxyURL(id) {
    const url = $('proxyURLInput').value.trim();
    if (url && !/^(socks5|socks5h|http|https):\/\//.test(url)) {
      toast(t('detail.proxyFormatError'), 'warning'); return;
    }
    await putAccount(id, { proxyURL: url }, t('detail.proxySaved'));
  }
  function closeDetailModal() { closeDialog('detailModal'); }

  // Test flow
  function getTestAccount(id) {
    return accountsData.find(a => a.id === id) || null;
  }
  function getTestModelValue() {
    const choice = $('testModelChoice');
    return (choice && choice.value.trim()) || 'claude-sonnet-4';
  }
  function renderTestLog() {
    const c = $('testModalLog');
    if (!c) return;
    if (!testLogs.length) {
      c.innerHTML = '<div class="test-log-empty">' + escapeHtml(t('accounts.testLog.empty')) + '</div>';
      return;
    }
    c.innerHTML = testLogs.map(log =>
      '<div class="test-log-line ' + escapeAttr(log.type || 'info') + '">' +
      '<span class="test-log-time">' + escapeHtml(log.time) + '</span>' +
      '<span class="test-log-message">' + escapeHtml(log.msg) + '</span>' +
      '</div>'
    ).join('');
    c.scrollTop = c.scrollHeight;
  }
  function addTestLog(msg, type) {
    const time = new Date().toLocaleTimeString();
    testLogs.push({ time, msg, type });
    if (testLogs.length > 100) testLogs.shift();
    renderTestLog();
  }
  function clearTestLog() {
    testLogs = [];
    renderTestLog();
  }
  function renderTestModal() {
    const body = $('testBody');
    if (!body) return;
    const acc = getTestAccount(testModalAccountId);
    const idAttr = escapeAttr(testModalAccountId);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : testModalAccountId;
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    const statusText = testModalLoadingModels
      ? t('accounts.testModelsLoading')
      : testModalModelError
        ? t('accounts.testModelsFallback')
        : t('accounts.testModelsReady', testModalModels.length);
    const modelField = testModalLoadingModels
      ? '<div class="test-model-loading">' + escapeHtml(t('accounts.testModelsLoading')) + '</div>'
      : testModalModels.length
        ? '<select id="testModelChoice">' +
        testModalModels.map(m => '<option value="' + escapeAttr(m) + '">' + escapeHtml(m) + '</option>').join('') +
        '</select>'
        : '<input type="text" id="testModelChoice" placeholder="claude-sonnet-4" value="claude-sonnet-4" />';

    body.innerHTML =
      '<div class="test-modal-account">' +
      '<div class="test-modal-account-main">' +
      '<div class="test-modal-email">' + escapeHtml(email) + '</div>' +
      '<div class="test-modal-meta">' +
      '<span>' + escapeHtml(formatAuthMethod(acc && (acc.provider || acc.authMethod))) + '</span>' +
      '<span>' + escapeHtml(proxy) + '</span>' +
      '</div>' +
      '</div>' +
      '<span class="test-modal-status">' + escapeHtml(statusText) + '</span>' +
      '</div>' +
      '<div class="test-modal-grid">' +
      '<div class="form-group test-model-field">' +
      '<label for="testModelChoice">' + escapeHtml(t('accounts.selectModel')) + '</label>' +
      modelField +
      '</div>' +
      '<div class="test-log-card">' +
      '<div class="test-log-header">' +
      '<span class="test-log-title">' + escapeHtml(t('accounts.testLog.title')) + '</span>' +
      '<button class="btn btn-xs btn-outline test-log-clear" id="testLogClear" type="button">' + escapeHtml(t('accounts.testLog.clear')) + '</button>' +
      '</div>' +
      '<div class="test-log-content" id="testModalLog"></div>' +
      '</div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="testModalCancelBtn" type="button">' + escapeHtml(t('common.close')) + '</button>' +
      '<button class="btn btn-primary" id="testRunBtn" data-id="' + idAttr + '" type="button" ' + (testModalLoadingModels ? 'disabled' : '') + '>' + escapeHtml(t('accounts.test')) + '</button>' +
      '</div>';

    if (!testModalLoadingModels) enhanceCustomSelects(body);
    renderTestLog();
  }
  async function testAccount(id) {
    testModalAccountId = id;
    testModalModels = [];
    testModalLoadingModels = true;
    testModalModelError = false;
    testModalRunning = false;
    testLogs = [];
    renderTestModal();
    openDialog('testModal');
    try {
      const res = await api('/accounts/' + id + '/models/cached');
      const d = await res.json();
      testModalModels = Array.isArray(d.models) ? d.models.slice().sort() : [];
    } catch (e) {
      testModalModelError = true;
    } finally {
      testModalLoadingModels = false;
      renderTestModal();
    }
  }
  function closeTestModal() {
    closeAllCustomSelects();
    closeDialog('testModal');
  }
  async function runTestAccount(id, model) {
    if (testModalRunning) return;
    testModalRunning = true;
    const modalBtn = $('testRunBtn');
    if (modalBtn) modalBtn.setAttribute('aria-busy', 'true');
    const acc = accountsData.find(a => a.id === id);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : id;
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    addTestLog(t('accounts.testLog.start', email, model, proxy), 'info');
    try {
      const startTime = Date.now();
      const res = await api('/accounts/' + id + '/test', { method: 'POST', body: JSON.stringify({ model }) });
      const elapsed = ((Date.now() - startTime) / 1000).toFixed(1);
      const d = await res.json();
      if (d.success) {
        addTestLog(t('accounts.testLog.success', email, elapsed, d.reply), 'ok');
      } else {
        addTestLog(t('accounts.testLog.failed', email, elapsed, d.error || t('common.unknownError')), 'err');
      }
    } catch (e) {
      addTestLog(t('accounts.testLog.error', email, e.message), 'err');
    }
    testModalRunning = false;
    if (modalBtn) modalBtn.removeAttribute('aria-busy');
  }

  // Settings
  async function loadSettings() {
    const res = await api('/settings');
    const d = await res.json();
    $('requireApiKey').checked = d.requireApiKey;
    $('allowOverUsage').checked = d.allowOverUsage || false;
    $('maxPayloadBytes').value = String(d.maxPayloadBytes || 2000000);
    if ($('publicModelCatalog')) $('publicModelCatalog').value = d.publicModelCatalog || '';
    await Promise.all([loadThinkingConfig(), loadEndpointConfig(), loadProxyConfig(), loadPromptFilter(), loadMemoryConfig(), loadApiKeys(), loadUpstreams(), loadSecurityConfig(), loadKiroGoModels()]);
    refreshCustomSelects();
  }
  async function loadThinkingConfig() {
    const res = await api('/thinking');
    const d = await res.json();
    $('thinkingSuffix').value = d.suffix || '-thinking';
    $('openaiThinkingFormat').value = d.openaiFormat || 'reasoning_content';
    $('claudeThinkingFormat').value = d.claudeFormat || 'thinking';
    // The server stores "let the model choose" as an empty string; the picker
    // spells that "auto".
    $('thinkingDefaultEffort').value = d.defaultEffort || 'auto';
    $('advertiseEffortModels').checked = d.advertiseEffortModels || false;
  }
  async function saveThinkingConfig() {
    const res = await api('/thinking', {
      method: 'POST', body: JSON.stringify({
        suffix: $('thinkingSuffix').value || '-thinking',
        openaiFormat: $('openaiThinkingFormat').value,
        claudeFormat: $('claudeThinkingFormat').value,
        defaultEffort: $('thinkingDefaultEffort').value,
        advertiseEffortModels: $('advertiseEffortModels').checked
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.thinkingSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function loadEndpointConfig() {
    const res = await api('/endpoint');
    const d = await res.json();
    $('preferredEndpoint').value = d.preferredEndpoint || 'auto';
    $('endpointFallback').checked = d.endpointFallback !== false;
  }
  async function saveEndpointConfig() {
    const res = await api('/endpoint', {
      method: 'POST', body: JSON.stringify({
        preferredEndpoint: $('preferredEndpoint').value,
        endpointFallback: $('endpointFallback').checked
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.endpointSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function loadSecurityConfig() {
    const res = await api('/security');
    const d = await res.json();
    $('adminAllowlist').value = (d.adminAllowlist || []).join('\n');
    $('trustProxy').checked = !!d.trustProxy;
    $('tlsCertFile').value = d.tlsCertFile || '';
    $('tlsKeyFile').value = d.tlsKeyFile || '';
    $('secYourIp').textContent = d.clientIP || '';
    renderSecurityWarnings(d.warnings || []);
  }
  function renderSecurityWarnings(list) {
    if (arguments.length > 0) securityWarnings = Array.isArray(list) ? list : [];
    const el = $('securityWarnings');
    if (!el) return;
    if (securityWarnings.length === 0) {
      el.classList.add('hidden');
      return;
    }
    const text = securityWarnings.map(w => t('security.warn.' + w.code)).join(' · ');
    el.querySelector('span').textContent = text;
    el.classList.remove('hidden');
  }
  async function saveSecurityConfig() {
    const allowlist = $('adminAllowlist').value.split('\n').map(s => s.trim()).filter(Boolean);
    const tlsCert = $('tlsCertFile').value.trim();
    const tlsKey = $('tlsKeyFile').value.trim();
    const res = await api('/security', {
      method: 'POST', body: JSON.stringify({
        adminAllowlist: allowlist,
        trustProxy: $('trustProxy').checked,
        tlsCertFile: tlsCert,
        tlsKeyFile: tlsKey
      })
    });
    const d = await res.json();
    if (d.success) {
      toast(t('settings.securitySaved'), 'success');
      renderSecurityWarnings(d.warnings || []);
      if (tlsCert || tlsKey) toast(t('settings.tlsRestartHint'), 'warning');
    } else {
      toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
    }
  }
  async function loadProxyConfig() {
    const res = await api('/proxy');
    const d = await res.json();
    const url = d.proxyURL || '';
    if (!url) {
      $('proxyType').value = 'none';
      $('proxyFields').classList.add('hidden');
      return;
    }
    try {
      const u = new URL(url);
      const scheme = u.protocol.replace(':', '');
      $('proxyType').value = scheme.startsWith('socks5') ? 'socks5' : 'http';
      $('proxyHost').value = u.hostname;
      $('proxyPort').value = u.port;
      $('proxyUsername').value = decodeURIComponent(u.username);
      $('proxyPassword').value = decodeURIComponent(u.password);
      $('proxyFields').classList.remove('hidden');
    } catch (e) {
      $('proxyType').value = 'none';
      $('proxyFields').classList.add('hidden');
    }
  }
  function onProxyTypeChange() {
    const type = $('proxyType').value;
    $('proxyFields').classList.toggle('hidden', type === 'none');
  }
  async function saveProxyConfig() {
    const type = $('proxyType').value;
    let url = '';
    if (type !== 'none') {
      const host = $('proxyHost').value.trim();
      const port = $('proxyPort').value.trim();
      if (!host || !port) { toast(t('settings.proxyHostRequired'), 'warning'); return; }
      const u = $('proxyUsername').value.trim();
      const p = $('proxyPassword').value.trim();
      const auth = u ? (p ? encodeURIComponent(u) + ':' + encodeURIComponent(p) + '@' : encodeURIComponent(u) + '@') : '';
      url = type + '://' + auth + host + ':' + port;
    }
    const res = await api('/proxy', { method: 'POST', body: JSON.stringify({ proxyURL: url }) });
    const d = await res.json();
    if (d.success) toast(t('settings.proxySaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function importProxies() {
    const raw = $('proxyImportList').value.trim();
    if (!raw) { toast(t('proxyImport.listRequired'), 'warning'); return; }
    const autoTest = $('proxyImportAutoTest').checked;
    const dryRun = $('proxyImportDryRun').checked;
    const btn = $('proxyImportBtn');
    btn.disabled = true;
    const dismiss = toast(t('proxyImport.processing'), 'info', { duration: 0 });
    try {
      const res = await api('/proxy/import', {
        method: 'POST',
        body: JSON.stringify({ proxies: raw, autoTest, dryRun })
      });
      const d = await res.json();
      dismiss();
      if (!d.success) {
        toast(t('common.failed') + ': ' + (d.error || ''), 'error');
        return;
      }
      renderProxyImportResults(d);
      toast(t('proxyImport.summary', d.assigned || 0, d.reachable || 0, d.total || 0), d.reachable < d.total ? 'warning' : 'success');
      loadAccounts();
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    } finally {
      btn.disabled = false;
    }
  }
  function renderProxyImportResults(d) {
    const box = $('proxyImportResults');
    const rows = (d.results || []).map(r => {
      let status, cls;
      if (r.error) { status = '✗ ' + r.error; cls = 'error-text'; }
      else if (r.tested && !r.testPassed) { status = '⚠ ' + t('proxyImport.testFailed'); cls = 'warning-text'; }
      else if (r.tested && r.testPassed) { status = '✓ ' + t('proxyImport.testOk'); cls = 'success-text'; }
      else if (r.assigned) { status = '✓ ' + t('proxyImport.assigned'); cls = 'success-text'; }
      else if (r.reachable) { status = '✓ ' + t('proxyImport.reachable'); cls = 'success-text'; }
      else { status = '✗'; cls = 'error-text'; }
      const target = r.assignedEmail ? ' → ' + escapeHtml(r.assignedEmail) : '';
      const scheme = r.scheme ? '[' + escapeHtml(r.scheme) + '] ' : '';
      const label = escapeHtml(r.maskedUrl || r.raw);
      return '<div class="test-log-line"><span class="font-mono text-xs">' + scheme + label + target +
        '</span> <span class="' + cls + '">' + escapeHtml(status) + '</span></div>';
    }).join('');
    box.innerHTML = rows || '<p class="help-block">' + escapeHtml(t('proxyImport.noResults')) + '</p>';
  }
  async function saveRequireApiKey() {
    try {
      const requireApiKey = $('requireApiKey').checked;
      if (requireApiKey) {
        const hasEnabledKey = Array.isArray(apiKeysCache) && apiKeysCache.some(k => k && k.enabled);
        if (!hasEnabledKey) {
          if (!confirm(t('apiKeys.requireWithoutEnabledKeyWarning'))) {
            $('requireApiKey').checked = false;
            return;
          }
        }
      }
      const res = await api('/settings', { method: 'POST', body: JSON.stringify({ requireApiKey }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      toast(t('detail.saved'), 'success');
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  async function saveModelCatalogConfig() {
    const publicModelCatalog = $('publicModelCatalog').value;
    const res = await api('/settings', { method: 'POST', body: JSON.stringify({ publicModelCatalog }) });
    const d = await res.json().catch(() => ({}));
    if (res.ok && d.success !== false) toast(t('settings.modelCatalogSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function saveOverUsageConfig() {
    const allowOverUsage = $('allowOverUsage').checked;
    const maxPayloadBytes = parseInt($('maxPayloadBytes').value, 10);
    await api('/settings', { method: 'POST', body: JSON.stringify({ allowOverUsage, maxPayloadBytes }) });
    toast(t('settings.overUsageSaved'), 'success');
  }
  async function changePassword() {
    const np = $('newPassword').value;
    if (!np) return toast(t('settings.passwordRequired'), 'warning');
    try {
      const res = await api('/settings', { method: 'POST', body: JSON.stringify({ password: np }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      setActivePassword(np, localStorage.getItem('kiro_remember') === '1');
      toast(t('settings.passwordChanged'), 'success');
      $('newPassword').value = '';
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  async function resetStats() {
    const ok = await confirmAction(t('settings.confirmReset'), {
      title: t('settings.statistics'),
      confirmText: t('settings.resetStats'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/stats/reset', { method: 'POST' });
      if (!res.ok) throw new Error(t('common.failed'));
      loadStats();
      toastPrimary(t('settings.statsReset'));
    } catch (e) {
      toastError((e && e.message) || t('common.failed'));
    }
  }
  // Multi API Key management
  let apiKeysCache = [];
  let apiKeysTotal = 0;
  let apiKeyEditingId = '';
  let apiKeyModalMode = 'create';
  let apiKeyModalSubmitting = false;
  let apiKeyLastPortalUrl = '';
  let apiKeyBatchSecrets = [];
  let apiKeysQuery = { q: '', status: '', quota: '', usage: '', sort: 'created_desc', offset: 0, limit: 50 };

  let upstreamCache = { providers: [], routes: [] };
  let upstreamEditingId = '';
  let routeEditingId = '';
  // Working copy of the route modal's target list. Held apart from upstreamCache
  // so Cancel discards edits; only submitRouteModal writes it back.
  let routeTargetDraft = [];
  // Sentinel upstream id meaning "the built-in Kiro account pool" rather than a
  // configured provider. Must match metrics.KiroPoolID on the backend, which
  // ResolveRoute turns into a synthetic target and the forwarder treats as "stop
  // relaying, fall through to the pool". Selecting it in a route makes the pool a
  // ranked failover step instead of the all-or-nothing default.
  const KIRO_POOL_ID = '__kiro_pool__';
  // Models fetched per provider (keyed by provider id) for the browse/copy/test UI.
  let providerModels = {};
  let providerModelsLoading = {};
  let modelsModalPid = '';
  let modelsModalSearch = '';
  // Hidden providers are collapsed behind a "show hidden" disclosure rather than
  // dropped, so an operator can always get back to one. The expanded/collapsed
  // choice is a view preference, so it lives in localStorage; which providers are
  // hidden is config, and lives on the server.
  let showHiddenProviders = localStorage.getItem('kiro_show_hidden_providers') === '1';
  // Same split for hidden route targets, one level down: the flag on the target is
  // config (it rides along with the route, and with export/import), the
  // expanded/collapsed choice is a per-browser view preference.
  //
  // Unlike providers, targets are an ORDERED chain, so hidden rows are elided in
  // place behind a run marker instead of being gathered at the bottom: a chain
  // that silently reads as contiguous when it is not would misdescribe the tier
  // above/below relationships the editor is built to show.
  let showHiddenRouteTargets = localStorage.getItem('kiro_show_hidden_route_targets') === '1';
  // Per-target probe results, keyed by "providerId|model" rather than by row index.
  // A probe describes a provider+model PAIR, so reordering or hiding rows must not
  // carry a verdict onto a different target, and two rows aiming at the same pair
  // legitimately share one result. Cleared when the modal opens: a verdict from a
  // previous editing session would be presented as if it were current.
  let routeTargetTests = {};
  // Model names served by this kiro-go instance (from /v1/models), used to
  // suggest Target Model values in the route modal. Still free-text + optional.
  let kiroGoModels = [];
  let connectionEditing = { providerId: '', connectionId: '' };
  let bulkImportPid = '';
  let bulkPreview = null;
  let bulkResolutions = {};
  // Per-provider sequential test runner. Results are session-only.
  let connTest = {};

  async function loadApiKeys() {
    const list = $('apiKeysList');
    if (!list) return;
    const loading = $('apiKeysLoading');
    const errEl = $('apiKeysError');
    if (loading) loading.classList.remove('hidden');
    if (errEl) { errEl.classList.add('hidden'); errEl.textContent = ''; }
    try {
      const q = apiKeysQuery;
      const params = new URLSearchParams();
      if (q.q) params.set('q', q.q);
      if (q.status) params.set('status', q.status);
      if (q.quota) params.set('quota', q.quota);
      if (q.usage) params.set('usage', q.usage);
      if (q.sort) params.set('sort', q.sort);
      params.set('offset', String(q.offset || 0));
      params.set('limit', String(q.limit || 50));
      const res = await api('/api-keys?' + params.toString());
      if (!res.ok) throw new Error('http ' + res.status);
      const d = await res.json();
      apiKeysCache = Array.isArray(d.apiKeys) ? d.apiKeys : [];
      apiKeysTotal = typeof d.total === 'number' ? d.total : apiKeysCache.length;
      renderApiKeys();
    } catch (e) {
      apiKeysCache = [];
      apiKeysTotal = 0;
      if (errEl) {
        errEl.textContent = t('apiKeys.loadFailed');
        errEl.classList.remove('hidden');
      }
      renderApiKeys();
    } finally {
      if (loading) loading.classList.add('hidden');
    }
  }

  function formatNumber(n) {
    if (n == null || isNaN(n)) return '0';
    if (Math.abs(n) >= 1 && Math.floor(n) === n) return Number(n).toLocaleString('en-US');
    return Number(n).toLocaleString('en-US', { maximumFractionDigits: 4 });
  }

  function usageBar(used, limit) {
    if (!limit || limit <= 0) return '';
    const ratio = Math.max(0, Math.min(1, used / limit));
    const pct = (ratio * 100).toFixed(1);
    let color = '#3b82f6';
    if (ratio >= 0.95) color = '#ef4444';
    else if (ratio >= 0.8) color = '#f59e0b';
    return '<div style="height:6px;background:rgba(127,127,127,0.2);border-radius:3px;overflow:hidden;margin-top:4px;">' +
      '<div style="height:100%;width:' + pct + '%;background:' + color + ';transition:width 0.3s;"></div>' +
      '</div>';
  }

  function usageLine(label, used, limit, options) {
    options = options || {};
    const fmt = options.fmt || formatNumber;
    if (!limit || limit <= 0) {
      return '<div class="text-xs muted-text">' + escapeHtml(label) + ': ' + escapeHtml(fmt(used)) + ' / ' + escapeHtml(t('apiKeys.unlimited')) + '</div>';
    }
    return '<div class="text-xs muted-text">' + escapeHtml(label) + ': ' + escapeHtml(fmt(used)) + ' / ' + escapeHtml(fmt(limit)) + '</div>' + usageBar(used, limit);
  }

  function apiKeyStatusBadge(item) {
    const st = item.status || (item.enabled ? 'active' : 'disabled');
    const map = { active: 'apiKeys.statusActive', disabled: 'apiKeys.statusDisabled', expired: 'apiKeys.statusExpired', exhausted: 'apiKeys.statusExhausted' };
    const cls = 'ak-status ak-status-' + st;
    return '<span class="' + cls + '">' + escapeHtml(t(map[st] || 'apiKeys.statusActive')) + '</span>';
  }

  function apiKeyQuotaLabel(item) {
    const parts = [];
    if (item.creditLimit > 0) parts.push(t('apiKeys.credits'));
    if (item.tokenLimit > 0) parts.push(t('apiKeys.tokens'));
    if (item.requestLimit > 0) parts.push(t('apiKeys.requests'));
    return parts.length ? parts.join(' + ') : t('apiKeys.unlimited');
  }

  function apiKeyUsedLabel(item) {
    if (item.creditLimit > 0) return formatNumber(item.creditsUsed || 0) + ' / ' + formatNumber(item.creditLimit);
    if (item.tokenLimit > 0) return formatNumber(item.tokensUsed || 0) + ' / ' + formatNumber(item.tokenLimit);
    if (item.requestLimit > 0) return formatNumber(item.requestsCount || 0) + ' / ' + formatNumber(item.requestLimit);
    return formatNumber(item.tokensUsed || 0);
  }

  function apiKeyRemainLabel(item) {
    if (item.creditsRemaining != null) return formatNumber(item.creditsRemaining);
    if (item.tokensRemaining != null) return formatNumber(item.tokensRemaining);
    if (item.requestsRemaining != null) return formatNumber(item.requestsRemaining);
    return '—';
  }

  function fmtUnixLocal(sec) {
    if (!sec) return '—';
    try { return new Date(sec * 1000).toLocaleString(); } catch (e) { return '—'; }
  }

  function renderApiKeys() {
    const list = $('apiKeysList');
    if (!list) return;
    if (!apiKeysCache.length) {
      list.innerHTML = '<tr><td colspan="11" class="muted-text" style="padding:0.75rem;">' + escapeHtml(t('apiKeys.empty')) + '</td></tr>';
      renderApiKeysPager();
      return;
    }
    list.innerHTML = apiKeysCache.map(item => {
      const id = escapeAttr(item.id || '');
      const name = item.name ? escapeHtml(item.name) : escapeHtml(t('apiKeys.unnamed'));
      const warn = usageWarnClass(item);
      return '<tr data-apikey-id="' + id + '"' + (warn ? ' class="' + warn + '"' : '') + '>' +
        '<td>' + name + '</td>' +
        '<td class="font-mono text-xs">' + escapeHtml(item.keyMasked || '') + '</td>' +
        '<td>' + apiKeyStatusBadge(item) + '</td>' +
        '<td>' + escapeHtml(apiKeyQuotaLabel(item)) + '</td>' +
        '<td>' + escapeHtml(apiKeyUsedLabel(item)) + usageBar(primaryUsed(item), primaryLimit(item)) + '</td>' +
        '<td>' + escapeHtml(apiKeyRemainLabel(item)) + '</td>' +
        '<td>' + escapeHtml(formatNumber(item.requestsCount || 0)) + '</td>' +
        '<td class="text-xs">' + escapeHtml(fmtUnixLocal(item.lastUsedAt)) + '</td>' +
        '<td class="text-xs">' + escapeHtml(fmtUnixLocal(item.expiresAt)) + '</td>' +
        '<td class="text-xs">' + escapeHtml(fmtUnixLocal(item.createdAt)) + '</td>' +
        '<td><div class="flex items-center gap-1" style="flex-wrap:wrap;">' +
          '<label class="switch" title="' + escapeAttr(item.enabled ? t('accounts.disable') : t('accounts.enable')) + '">' +
            '<input type="checkbox" data-apikey-action="toggle" data-id="' + id + '"' + (item.enabled ? ' checked' : '') + ' />' +
            '<span class="slider"></span></label>' +
          '<button class="btn btn-outline btn-sm" type="button" data-apikey-action="edit" data-id="' + id + '">' + escapeHtml(t('apiKeys.actionEdit')) + '</button>' +
          '<button class="btn btn-outline btn-sm" type="button" data-apikey-action="rotate" data-id="' + id + '">' + escapeHtml(t('apiKeys.actionRotate')) + '</button>' +
          '<button class="btn btn-outline btn-sm" type="button" data-apikey-action="portal" data-id="' + id + '">' + escapeHtml(t('apiKeys.actionPortal')) + '</button>' +
          '<button class="btn btn-outline btn-sm" type="button" data-apikey-action="reset" data-id="' + id + '">' + escapeHtml(t('apiKeys.actionReset')) + '</button>' +
          '<button class="btn btn-danger btn-sm" type="button" data-apikey-action="delete" data-id="' + id + '">' + escapeHtml(t('apiKeys.actionDelete')) + '</button>' +
        '</div></td></tr>';
    }).join('');
    renderApiKeysPager();
  }

  function primaryLimit(item) {
    if (item.creditLimit > 0) return item.creditLimit;
    if (item.tokenLimit > 0) return item.tokenLimit;
    if (item.requestLimit > 0) return item.requestLimit;
    return 0;
  }
  function primaryUsed(item) {
    if (item.creditLimit > 0) return item.creditsUsed || 0;
    if (item.tokenLimit > 0) return item.tokensUsed || 0;
    if (item.requestLimit > 0) return item.requestsCount || 0;
    return 0;
  }
  function usageWarnClass(item) {
    const lim = primaryLimit(item);
    if (!lim) return '';
    const ratio = primaryUsed(item) / lim;
    if (ratio >= 1) return 'ak-row-100';
    if (ratio >= 0.9) return 'ak-row-90';
    if (ratio >= 0.8) return 'ak-row-80';
    return '';
  }

  function renderApiKeysPager() {
    const el = $('apiKeysPager');
    if (!el) return;
    const limit = apiKeysQuery.limit || 50;
    const offset = apiKeysQuery.offset || 0;
    const page = Math.floor(offset / limit) + 1;
    const pages = Math.max(1, Math.ceil((apiKeysTotal || 0) / limit));
    el.innerHTML = '<span class="text-xs muted-text">' + escapeHtml(t('apiKeys.pageOf', page, pages, apiKeysTotal || 0)) + '</span>' +
      '<button class="btn btn-outline btn-sm" type="button" id="apiKeysPrev"' + (offset <= 0 ? ' disabled' : '') + '>' + escapeHtml(t('apiKeys.prev')) + '</button>' +
      '<button class="btn btn-outline btn-sm" type="button" id="apiKeysNext"' + (offset + limit >= apiKeysTotal ? ' disabled' : '') + '>' + escapeHtml(t('apiKeys.next')) + '</button>';
    const prev = $('apiKeysPrev');
    const next = $('apiKeysNext');
    if (prev) prev.onclick = () => { apiKeysQuery.offset = Math.max(0, offset - limit); loadApiKeys(); };
    if (next) next.onclick = () => { apiKeysQuery.offset = offset + limit; loadApiKeys(); };
  }

  function syncLimitByUI() {
    const by = $('apiKeyForm_limitBy') ? $('apiKeyForm_limitBy').value : 'unlimited';
    const grp = $('apiKeyForm_amountGroup');
    if (grp) grp.classList.toggle('hidden', by === 'unlimited');
    const box = $('apiKeyForm_presets');
    if (!box) return;
    const presets = by === 'credits' ? [1, 5, 10, 20, 50, 100]
      : by === 'tokens' ? [
        { n: 5000000, label: '5' },
        { n: 10000000, label: '10m' },
        { n: 50000000, label: '50' },
        { n: 100000000, label: '100' },
        { n: 200000000, label: '200' },
        { n: 1000000000, label: '1b' },
        { n: 2000000000, label: '2b' }
      ]
      : by === 'requests' ? [100, 1000, 5000, 10000] : [];
    box.innerHTML = presets.map((item) => {
      const n = typeof item === 'object' ? item.n : item;
      const label = typeof item === 'object' ? item.label : formatPreset(n, by);
      return '<button type="button" class="ak-preset" data-preset="' + n + '">' + escapeHtml(label) + '</button>';
    }).join('');
  }
  function formatPreset(n, by) {
    if (by === 'tokens') {
      if (n >= 1000000000) return (n / 1000000000) + 'b';
      if (n >= 1000000) return (n / 1000000) + 'm';
      if (n >= 1000) return (n / 1000) + 'k';
    }
    if (n >= 1000) return (n / 1000) + 'K';
    return String(n);
  }

  function syncApiKeyModalMode() {
    const batch = apiKeyModalMode === 'batch';
    const titleEl = $('apiKeyModalTitle');
    if (titleEl) {
      titleEl.textContent = t(apiKeyEditingId ? 'apiKeys.modalTitleEdit' : (batch ? 'apiKeys.modalTitleBatch' : 'apiKeys.modalTitleCreate'));
    }
    const nameLabel = document.querySelector('label[for="apiKeyForm_name"]');
    if (nameLabel) {
      nameLabel.setAttribute('data-i18n', batch ? 'apiKeys.formNamePrefix' : 'apiKeys.formName');
      nameLabel.textContent = t(batch ? 'apiKeys.formNamePrefix' : 'apiKeys.formName');
    }
    const nameInput = $('apiKeyForm_name');
    if (nameInput) {
      nameInput.setAttribute('data-i18n-placeholder', batch ? 'apiKeys.formNamePrefixPlaceholder' : 'apiKeys.formNamePlaceholder');
      nameInput.placeholder = t(batch ? 'apiKeys.formNamePrefixPlaceholder' : 'apiKeys.formNamePlaceholder');
    }
    const countGroup = $('apiKeyForm_countGroup');
    if (countGroup) countGroup.classList.toggle('hidden', !batch);
    const saveBtn = $('apiKeyModalSaveBtn');
    if (saveBtn) {
      saveBtn.setAttribute('data-i18n', batch ? 'apiKeys.batchCreateBtn' : 'apiKeys.saveBtn');
      saveBtn.textContent = t(batch ? 'apiKeys.batchCreateBtn' : 'apiKeys.saveBtn');
    }
  }

  function openApiKeyModal(entry, mode) {
    apiKeyEditingId = entry ? (entry.id || '') : '';
    apiKeyModalMode = mode || (entry ? 'edit' : 'create');
    $('apiKeyForm_name').value = entry ? (entry.name || '') : '';
    const keyEl = $('apiKeyForm_key');
    const keyGroup = $('apiKeyForm_keyGroup');
    if (apiKeyEditingId || apiKeyModalMode === 'batch') {
      keyEl.value = entry ? (entry.keyMasked || '') : '';
      keyEl.readOnly = !!apiKeyEditingId;
      if (keyGroup) keyGroup.classList.add('hidden');
    } else {
      keyEl.value = '';
      keyEl.readOnly = false;
      if (keyGroup) keyGroup.classList.remove('hidden');
    }
    if ($('apiKeyForm_count')) $('apiKeyForm_count').value = '10';
    syncApiKeyModalMode();
    $('apiKeyForm_enabled').checked = entry ? !!entry.enabled : true;
    $('apiKeyForm_tokenLimit').value = entry ? String(entry.tokenLimit || 0) : '0';
    $('apiKeyForm_creditLimit').value = entry ? String(entry.creditLimit || 0) : '0';
    if ($('apiKeyForm_requestLimit')) $('apiKeyForm_requestLimit').value = entry ? String(entry.requestLimit || 0) : '0';
    if ($('apiKeyForm_resetPolicy')) $('apiKeyForm_resetPolicy').value = (entry && entry.resetPolicy) || 'lifetime';
    if ($('apiKeyForm_enforcement')) $('apiKeyForm_enforcement').value = (entry && entry.enforcementMode) || 'soft';
    let by = 'unlimited';
    if (entry) {
      if (entry.creditLimit > 0) by = 'credits';
      else if (entry.tokenLimit > 0) by = 'tokens';
      else if (entry.requestLimit > 0) by = 'requests';
    }
    if ($('apiKeyForm_limitBy')) $('apiKeyForm_limitBy').value = by;
    if ($('apiKeyForm_amount')) {
      $('apiKeyForm_amount').value = by === 'credits' ? (entry && entry.creditLimit || 0)
        : by === 'tokens' ? (entry && entry.tokenLimit || 0)
        : by === 'requests' ? (entry && entry.requestLimit || 0) : 0;
    }
    if ($('apiKeyForm_expires')) {
      if (entry && entry.expiresAt) {
        const d = new Date(entry.expiresAt * 1000);
        const pad = n => String(n).padStart(2, '0');
        $('apiKeyForm_expires').value = d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + 'T' + pad(d.getHours()) + ':' + pad(d.getMinutes());
      } else {
        $('apiKeyForm_expires').value = '';
      }
    }
    syncLimitByUI();
    apiKeyModalSubmitting = false;
    $('apiKeyModalSaveBtn').disabled = false;
    openDialog('apiKeyModal');
  }

  function closeApiKeyModal() {
    closeDialog('apiKeyModal');
    apiKeyEditingId = '';
    apiKeyModalMode = 'create';
    apiKeyModalSubmitting = false;
    $('apiKeyModalSaveBtn').disabled = false;
  }

  async function submitApiKeyModal() {
    if (apiKeyModalSubmitting) return;
    apiKeyModalSubmitting = true;
    const saveBtn = $('apiKeyModalSaveBtn');
    saveBtn.disabled = true;
    try {
      const name = $('apiKeyForm_name').value.trim();
      const enabled = $('apiKeyForm_enabled').checked;
      let tokenLimit = parseInt($('apiKeyForm_tokenLimit').value, 10);
      let creditLimit = parseFloat($('apiKeyForm_creditLimit').value);
      let requestLimit = $('apiKeyForm_requestLimit') ? parseInt($('apiKeyForm_requestLimit').value, 10) : 0;
      const by = $('apiKeyForm_limitBy') ? $('apiKeyForm_limitBy').value : 'unlimited';
      const amount = parseFloat($('apiKeyForm_amount') ? $('apiKeyForm_amount').value : '0');
      if (by === 'unlimited') { tokenLimit = 0; creditLimit = 0; requestLimit = 0; }
      else if (by === 'credits') { creditLimit = isNaN(amount) ? 0 : amount; tokenLimit = 0; requestLimit = 0; }
      else if (by === 'tokens') { tokenLimit = isNaN(amount) ? 0 : amount; creditLimit = 0; requestLimit = 0; }
      else if (by === 'requests') { requestLimit = isNaN(amount) ? 0 : amount; tokenLimit = 0; creditLimit = 0; }
      const payload = {
        name: name,
        enabled: enabled,
        tokenLimit: isNaN(tokenLimit) || tokenLimit < 0 ? 0 : tokenLimit,
        creditLimit: isNaN(creditLimit) || creditLimit < 0 ? 0 : creditLimit,
        requestLimit: isNaN(requestLimit) || requestLimit < 0 ? 0 : requestLimit,
        resetPolicy: $('apiKeyForm_resetPolicy') ? $('apiKeyForm_resetPolicy').value : 'lifetime',
        enforcementMode: $('apiKeyForm_enforcement') ? $('apiKeyForm_enforcement').value : 'soft'
      };
      const exp = $('apiKeyForm_expires') && $('apiKeyForm_expires').value;
      if (exp) payload.expiresAt = Math.floor(new Date(exp).getTime() / 1000);
      else payload.expiresAt = 0;
      let res, d;
      if (apiKeyEditingId) {
        res = await api('/api-keys/' + encodeURIComponent(apiKeyEditingId), { method: 'PUT', body: JSON.stringify(payload) });
        d = await res.json().catch(() => ({}));
        if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
        toast(t('apiKeys.updated'), 'success');
        closeApiKeyModal();
        await loadApiKeys();
      } else if (apiKeyModalMode === 'batch') {
        const count = parseInt($('apiKeyForm_count') ? $('apiKeyForm_count').value : '0', 10);
        if (!count || count < 1 || count > 100) throw new Error(t('apiKeys.batchInvalidCount'));
        payload.count = count;
        res = await api('/api-keys/batch', { method: 'POST', body: JSON.stringify(payload) });
        d = await res.json().catch(() => ({}));
        if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
        toast(t('apiKeys.batchCreated', d.count || count), 'success');
        closeApiKeyModal();
        await loadApiKeys();
        showBatchApiKeys(Array.isArray(d.keys) ? d.keys : []);
      } else {
        const keyVal = $('apiKeyForm_key').value.trim();
        if (keyVal) payload.key = keyVal;
        res = await api('/api-keys', { method: 'POST', body: JSON.stringify(payload) });
        d = await res.json().catch(() => ({}));
        if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
        toast(t('apiKeys.created'), 'success');
        closeApiKeyModal();
        await loadApiKeys();
        if (d.key) showNewApiKey(d.key, d.id);
      }
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
      apiKeyModalSubmitting = false;
      saveBtn.disabled = false;
    }
  }

  async function toggleApiKeyEntry(id, enabled) {
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id), { method: 'PUT', body: JSON.stringify({ enabled }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      const item = apiKeysCache.find(x => x.id === id);
      if (item) item.enabled = enabled;
      renderApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
      await loadApiKeys();
    }
  }

  async function deleteApiKeyEntry(id, name) {
    const ok = await confirmAction(t('apiKeys.confirmDelete', name || t('apiKeys.unnamed')), {
      title: t('apiKeys.actionDelete'),
      confirmText: t('apiKeys.actionDelete'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id), { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('apiKeys.deleteSuccess'), 'success');
      await loadApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  async function resetApiKeyUsageEntry(id, name) {
    const ok = await confirmAction(t('apiKeys.confirmReset', name || t('apiKeys.unnamed')), {
      title: t('apiKeys.actionReset'),
      confirmText: t('apiKeys.actionReset')
    });
    if (!ok) return;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id) + '/reset-usage', { method: 'POST' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('apiKeys.usageReset'), 'success');
      await loadApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  async function showNewApiKey(plaintext, id) {
    $('apiKeyShowValue').value = plaintext || '';
    apiKeyLastPortalUrl = '';
    const openBtn = $('apiKeyShowPortalBtn');
    const copyBtn = $('apiKeyShowPortalCopyBtn');
    if (openBtn) openBtn.classList.add('hidden');
    if (copyBtn) copyBtn.classList.add('hidden');
    if (id) {
      try {
        const res = await api('/api-keys/' + encodeURIComponent(id) + '/portal-token', { method: 'POST', body: '{}' });
        const d = await res.json().catch(() => ({}));
        if (res.ok && d.url) {
          apiKeyLastPortalUrl = d.url;
          if (openBtn) openBtn.classList.remove('hidden');
          if (copyBtn) copyBtn.classList.remove('hidden');
        }
      } catch (e) { /* portal optional */ }
    }
    openDialog('apiKeyShowModal');
    setTimeout(() => {
      const el = $('apiKeyShowValue');
      if (el) { try { el.select(); } catch (_) { } }
    }, 0);
  }

  function closeShowApiKeyModal() {
    closeDialog('apiKeyShowModal');
    $('apiKeyShowValue').value = '';
    apiKeyLastPortalUrl = '';
  }

  function csvCell(v) {
    const s = v == null ? '' : String(v);
    if (/[",\n]/.test(s)) return '"' + s.replace(/"/g, '""') + '"';
    return s;
  }

  function showBatchApiKeys(rows) {
    apiKeyBatchSecrets = (rows || []).map((row) => ({
      name: row && row.name ? String(row.name) : '',
      key: row && row.key ? String(row.key) : ''
    })).filter((row) => row.key);
    const tbody = $('apiKeyBatchResultRows');
    if (tbody) {
      tbody.innerHTML = apiKeyBatchSecrets.map((row, idx) => {
        return '<tr>' +
          '<td>' + escapeHtml(row.name || t('apiKeys.unnamed')) + '</td>' +
          '<td class="ak-batch-key">' + escapeHtml(row.key) + '</td>' +
          '<td><button class="btn btn-outline btn-sm" type="button" data-batch-copy="' + idx + '">' + escapeHtml(t('apiKeys.copyBtn')) + '</button></td>' +
          '</tr>';
      }).join('') || '<tr><td colspan="3" class="muted-text">' + escapeHtml(t('apiKeys.empty')) + '</td></tr>';
    }
    openDialog('apiKeyBatchResultModal');
  }

  function closeBatchResultModal() {
    closeDialog('apiKeyBatchResultModal');
    apiKeyBatchSecrets = [];
    const tbody = $('apiKeyBatchResultRows');
    if (tbody) tbody.innerHTML = '';
  }

  async function copyBatchSecret(idx) {
    const row = apiKeyBatchSecrets[idx];
    if (!row || !row.key) return;
    try {
      await copyText(row.key);
      toast(t('apiKeys.copySuccess'), 'success');
    } catch (e) {
      toast(t('common.failed'), 'error');
    }
  }

  async function copyAllBatchSecrets() {
    if (!apiKeyBatchSecrets.length) return;
    const text = apiKeyBatchSecrets.map((row) => (row.name ? row.name + '\t' : '') + row.key).join('\n');
    try {
      await copyText(text);
      toast(t('apiKeys.copySuccess'), 'success');
    } catch (e) {
      toast(t('common.failed'), 'error');
    }
  }

  function downloadBatchCsv() {
    if (!apiKeyBatchSecrets.length) return;
    const lines = ['name,key'].concat(apiKeyBatchSecrets.map((row) => csvCell(row.name) + ',' + csvCell(row.key)));
    const blob = new Blob([lines.join('\n')], { type: 'text/csv;charset=utf-8' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'api-keys.csv';
    a.click();
    URL.revokeObjectURL(a.href);
  }

  async function copyNewApiKey() {
    const val = $('apiKeyShowValue').value;
    if (!val) return;
    try {
      await copyText(val);
      toast(t('apiKeys.copySuccess'), 'success');
    } catch (e) {
      toast(t('common.failed'), 'error');
    }
  }

  function bindApiKeyEvents() {
    const list = $('apiKeysList');
    if (list) {
      list.addEventListener('click', e => {
        const btn = e.target.closest('[data-apikey-action]');
        if (!btn) return;
        const action = btn.dataset.apikeyAction;
        const id = btn.dataset.id;
        if (!id) return;
        const entry = apiKeysCache.find(x => x.id === id);
        const name = entry ? entry.name : '';
        if (action === 'edit') openApiKeyModal(entry);
        else if (action === 'delete') deleteApiKeyEntry(id, name);
        else if (action === 'reset') resetApiKeyUsageEntry(id, name);
        else if (action === 'rotate') rotateApiKeyEntry(id, name);
        else if (action === 'portal') sharePortalLink(id);
      });
      list.addEventListener('change', e => {
        const cb = e.target.closest('input[data-apikey-action="toggle"]');
        if (!cb) return;
        const id = cb.dataset.id;
        if (!id) return;
        toggleApiKeyEntry(id, cb.checked);
      });
    }
    const addBtn = $('addApiKeyBtn');
    if (addBtn) addBtn.addEventListener('click', () => openApiKeyModal(null, 'create'));
    const batchBtn = $('batchApiKeyBtn');
    if (batchBtn) batchBtn.addEventListener('click', () => openApiKeyModal(null, 'batch'));
    const saveBtn = $('apiKeyModalSaveBtn');
    if (saveBtn) saveBtn.addEventListener('click', submitApiKeyModal);
    const cancelBtn = $('apiKeyModalCancelBtn');
    if (cancelBtn) cancelBtn.addEventListener('click', closeApiKeyModal);
    const closeBtn = $('apiKeyModalClose');
    if (closeBtn) closeBtn.addEventListener('click', closeApiKeyModal);
    const showCloseBtn = $('apiKeyShowCloseBtn');
    if (showCloseBtn) showCloseBtn.addEventListener('click', closeShowApiKeyModal);
    const showCloseX = $('apiKeyShowClose');
    if (showCloseX) showCloseX.addEventListener('click', closeShowApiKeyModal);
    const copyBtn = $('apiKeyShowCopyBtn');
    if (copyBtn) copyBtn.addEventListener('click', copyNewApiKey);
    const portalOpen = $('apiKeyShowPortalBtn');
    if (portalOpen) portalOpen.addEventListener('click', () => { if (apiKeyLastPortalUrl) window.open(apiKeyLastPortalUrl, '_blank'); });
    const portalCopy = $('apiKeyShowPortalCopyBtn');
    if (portalCopy) portalCopy.addEventListener('click', async () => {
      if (!apiKeyLastPortalUrl) return;
      try { await copyText(apiKeyLastPortalUrl); toast(t('apiKeys.copySuccess'), 'success'); } catch (e) { toast(t('common.failed'), 'error'); }
    });
    const limitBy = $('apiKeyForm_limitBy');
    if (limitBy) limitBy.addEventListener('change', syncLimitByUI);
    const presets = $('apiKeyForm_presets');
    if (presets) presets.addEventListener('click', e => {
      const btn = e.target.closest('[data-preset]');
      if (!btn || !$('apiKeyForm_amount')) return;
      $('apiKeyForm_amount').value = btn.dataset.preset;
      presets.querySelectorAll('.ak-preset').forEach((el) => {
        el.classList.toggle('is-active', el === btn);
      });
    });
    ['apiKeysSearch', 'apiKeysFilterStatus', 'apiKeysFilterQuota', 'apiKeysFilterUsage', 'apiKeysSort'].forEach(id => {
      const el = $(id);
      if (!el) return;
      const apply = () => {
        apiKeysQuery.q = $('apiKeysSearch') ? $('apiKeysSearch').value.trim() : '';
        apiKeysQuery.status = $('apiKeysFilterStatus') ? $('apiKeysFilterStatus').value : '';
        apiKeysQuery.quota = $('apiKeysFilterQuota') ? $('apiKeysFilterQuota').value : '';
        apiKeysQuery.usage = $('apiKeysFilterUsage') ? $('apiKeysFilterUsage').value : '';
        apiKeysQuery.sort = $('apiKeysSort') ? $('apiKeysSort').value : 'created_desc';
        apiKeysQuery.offset = 0;
        loadApiKeys();
      };
      el.addEventListener(el.tagName === 'INPUT' ? 'input' : 'change', apply);
    });
    bindDialogBackdropClose('apiKeyModal', closeApiKeyModal);
    bindDialogBackdropClose('apiKeyShowModal', closeShowApiKeyModal);
    bindDialogBackdropClose('apiKeyBatchResultModal', closeBatchResultModal);
    const batchRows = $('apiKeyBatchResultRows');
    if (batchRows) batchRows.addEventListener('click', (e) => {
      const btn = e.target.closest('[data-batch-copy]');
      if (!btn) return;
      copyBatchSecret(parseInt(btn.getAttribute('data-batch-copy'), 10));
    });
    const batchCopyAll = $('apiKeyBatchCopyAllBtn');
    if (batchCopyAll) batchCopyAll.addEventListener('click', copyAllBatchSecrets);
    const batchCsv = $('apiKeyBatchCsvBtn');
    if (batchCsv) batchCsv.addEventListener('click', downloadBatchCsv);
    const batchClose = $('apiKeyBatchResultCloseBtn');
    if (batchClose) batchClose.addEventListener('click', closeBatchResultModal);
    const batchCloseX = $('apiKeyBatchResultClose');
    if (batchCloseX) batchCloseX.addEventListener('click', closeBatchResultModal);
  }

  async function rotateApiKeyEntry(id, name) {
    const ok = await confirmAction(t('apiKeys.confirmRotate', name || t('apiKeys.unnamed')), {
      title: t('apiKeys.actionRotate'), confirmText: t('apiKeys.actionRotate'), variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id) + '/rotate', { method: 'POST' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('apiKeys.rotated'), 'success');
      await loadApiKeys();
      if (d.key) showNewApiKey(d.key, id);
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  async function sharePortalLink(id) {
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id) + '/portal-token', { method: 'POST', body: '{}' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || !d.url) throw new Error(d.error || t('common.failed'));
      await copyText(d.url);
      toast(t('apiKeys.portalCopied'), 'success');
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  // ==================== Upstream forwarding ====================
  async function loadUpstreams() {
    const provList = $('upstreamsList');
    const routeList = $('modelRoutesList');
    if (!provList && !routeList) return;
    try {
      const res = await api('/upstreams');
      if (!res.ok) throw new Error('http ' + res.status);
      const d = await res.json();
      upstreamCache = {
        providers: Array.isArray(d.providers) ? d.providers : [],
        routes: Array.isArray(d.routes) ? d.routes : []
      };
      renderUpstreams();
    } catch (e) {
      upstreamCache = { providers: [], routes: [] };
      if (provList) provList.innerHTML = '<div class="muted-text" style="padding:0.5rem 0;">' + escapeHtml(t('upstreams.loadFailed')) + '</div>';
      if (routeList) routeList.innerHTML = '';
    }
  }

  function providerName(id) {
    // The pool sentinel names no configured provider, so resolve it by hand or
    // every route listing it would render the raw "__kiro_pool__" id.
    if (id === KIRO_POOL_ID) return t('upstreams.kiroPoolTarget');
    const p = upstreamCache.providers.find(x => x.id === id);
    return p ? (p.name || p.baseUrl || id) : id;
  }

  function renderUpstreams() {
    renderProviders();
    renderModelRoutes();
  }

  // providerCard renders one provider row. Hidden providers get the same markup —
  // every action stays reachable — plus a marker class and a badge, so the only
  // difference between hidden and visible is where the row is placed and how it
  // looks, never what the operator can do to it.
  function providerCard(item) {
    const id = escapeAttr(item.id || '');
    const name = item.name ? escapeHtml(item.name) : '<span class="muted-text">' + escapeHtml(t('upstreams.unnamed')) + '</span>';
    const baseUrl = escapeHtml(item.baseUrl || '');
    const disabled = !item.enabled
      ? '<span class="text-xs" style="background:rgba(239,68,68,0.15);color:#ef4444;padding:1px 6px;border-radius:4px;">' + escapeHtml(t('upstreams.disabled')) + '</span>'
      : '';
    // A hidden provider that is still enabled keeps forwarding, which is easy to
    // forget once it is out of sight. The badge says so explicitly rather than
    // letting "hidden" read as "off".
    const hiddenBadge = item.hidden
      ? '<span class="text-xs" style="background:rgba(148,163,184,0.18);color:var(--muted-foreground);padding:1px 6px;border-radius:4px;"' +
          ' title="' + escapeAttr(t(item.enabled ? 'upstreams.hiddenStillLive' : 'upstreams.hiddenHint')) + '">' +
          escapeHtml(t('upstreams.hidden')) + '</span>'
      : '';
    const hideIcon = item.hidden ? 'fa-eye' : 'fa-eye-slash';
    const hideTitle = item.hidden ? t('upstreams.actionUnhide') : t('upstreams.actionHide');
    return '<div class="card provider-card' + (item.hidden ? ' is-hidden-provider' : '') + '" data-upstream-id="' + id + '"' +
      ' style="margin-top:0.5rem;padding:0.75rem;">' +
      '<div class="flex items-center gap-2" style="flex-wrap:wrap;justify-content:space-between;">' +
        '<div class="flex items-center gap-2" style="flex-wrap:wrap;">' +
          '<span class="font-semibold">' + name + '</span>' +
          disabled +
          hiddenBadge +
          '<span class="text-xs muted-text font-mono">' + baseUrl + '</span>' +
        '</div>' +
        '<div class="flex items-center gap-2">' +
          '<label class="switch" title="' + escapeAttr(item.enabled ? t('accounts.disable') : t('accounts.enable')) + '">' +
            '<input type="checkbox" data-upstream-action="toggle" data-id="' + id + '"' + (item.enabled ? ' checked' : '') + ' />' +
            '<span class="slider"></span>' +
          '</label>' +
          '<button class="btn btn-outline btn-sm" type="button" data-upstream-action="hide" data-id="' + id + '"' +
            ' title="' + escapeAttr(hideTitle) + '" aria-label="' + escapeAttr(hideTitle) + '">' +
            '<i class="fa-solid ' + hideIcon + '" aria-hidden="true"></i></button>' +
          '<button class="btn btn-outline btn-sm" type="button" data-upstream-action="details" data-id="' + id + '" aria-expanded="false">' + escapeHtml(t('stats.details')) + '</button>' +
          '<button class="btn btn-outline btn-sm" type="button" data-upstream-action="add-key" data-id="' + id + '">' + escapeHtml(t('upstreams.addApiKey')) + '</button>' +
          '<button class="btn btn-outline btn-sm" type="button" data-upstream-action="bulk" data-id="' + id + '">' + escapeHtml(t('upstreams.bulkImport')) + '</button>' +
          '<button class="btn btn-outline btn-sm" type="button" data-upstream-action="load" data-id="' + id + '">' + escapeHtml(t('upstreams.loadModels')) + '</button>' +
          '<button class="btn btn-outline btn-sm" type="button" data-upstream-action="edit" data-id="' + id + '">' + escapeHtml(t('upstreams.actionEdit')) + '</button>' +
          '<button class="btn btn-danger btn-sm" type="button" data-upstream-action="delete" data-id="' + id + '">' + escapeHtml(t('upstreams.actionDelete')) + '</button>' +
        '</div>' +
      '</div>' +
      '<div class="text-xs muted-text font-mono" data-fwd-inline-for="' + id + '" style="margin-top:0.35rem;"></div>' +
      connectionsPanel(item) +
      '<div class="provider-detail hidden" data-provider-detail="' + id + '"></div>' +
    '</div>';
  }

  function providerConnections(item) {
    return (item && Array.isArray(item.connections)) ? item.connections : [];
  }

  function connTestState(pid) {
    if (!connTest[pid]) connTest[pid] = { running: false, abort: null, model: '', results: {}, selected: null };
    return connTest[pid];
  }

  function connHealthLabel(h) {
    switch (h) {
      case 'rate_limited': return t('upstreams.connHealthRateLimited');
      case 'auth_failed': return t('upstreams.connHealthAuthFailed');
      case 'unhealthy': return t('upstreams.connHealthUnhealthy');
      default: return t('upstreams.connHealthOk');
    }
  }

  function connTestLabel(res) {
    if (!res) return '';
    const ms = res.latencyMs != null ? '  ' + res.latencyMs + ' ms' : '';
    switch (res.status) {
      case 'queued': return t('upstreams.connTestQueued');
      case 'testing': return t('upstreams.connTestTesting');
      case 'success': return t('upstreams.connTestSuccess') + ms;
      case 'failed': return t('upstreams.connTestFailed') + (res.http ? '  ' + res.http : '') + ms;
      case 'timeout': return t('upstreams.connTestTimeout') + ms;
      case 'skipped': return t('upstreams.connTestSkipped');
      case 'stopped': return t('upstreams.connTestStopped');
      default: return '';
    }
  }

  function connectionsPanel(item) {
    const id = item.id || '';
    const conns = providerConnections(item);
    const st = connTestState(id);
    const models = providerModels[id] || [];
    const testModel = st.model || models[0] || '';
    const modelOpts = models.map(m =>
      '<option value="' + escapeAttr(m) + '"' + (m === testModel ? ' selected' : '') + '>' + escapeHtml(m) + '</option>'
    ).join('');
    const rrOn = (item.connectionStrategy || 'round_robin') !== 'primary';
    let passed = 0, failed = 0, completed = 0;
    conns.forEach(c => {
      const r = st.results[c.id];
      if (!r || r.status === 'queued' || r.status === 'testing') return;
      completed++;
      if (r.status === 'success') passed++;
      else if (r.status === 'failed' || r.status === 'timeout') failed++;
    });
    const rows = conns.map(c => {
      const cid = escapeAttr(c.id || '');
      const checked = !st.selected || st.selected[c.id] ? ' checked' : '';
      const res = st.results[c.id];
      const testLine = connTestLabel(res);
      return '<div class="conn-row">' +
        '<label class="conn-check"><input type="checkbox" data-conn-select="' + cid + '" data-pid="' + escapeAttr(id) + '"' + checked + ' /></label>' +
        '<div class="conn-main">' +
          '<div class="flex items-center gap-2" style="flex-wrap:wrap;">' +
            '<span class="font-semibold">' + escapeHtml(c.name || t('upstreams.unnamed')) + '</span>' +
            '<span class="text-xs font-mono muted-text">' + escapeHtml(c.apiKeyMasked || '****') + '</span>' +
            '<span class="conn-health conn-health-' + escapeAttr(c.health || 'ok') + '">' + escapeHtml(connHealthLabel(c.health)) + '</span>' +
            (testLine ? '<span class="text-xs muted-text">' + escapeHtml(testLine) + '</span>' : '') +
          '</div>' +
        '</div>' +
        '<div class="flex items-center gap-2">' +
          '<label class="switch" title="' + escapeAttr(t('upstreams.formEnabled')) + '">' +
            '<input type="checkbox" data-conn-action="toggle" data-pid="' + escapeAttr(id) + '" data-cid="' + cid + '"' + (c.enabled ? ' checked' : '') + ' />' +
            '<span class="slider"></span></label>' +
          '<button class="btn btn-outline btn-xs" type="button" data-conn-action="edit" data-pid="' + escapeAttr(id) + '" data-cid="' + cid + '">' + escapeHtml(t('upstreams.actionEdit')) + '</button>' +
          '<button class="btn btn-danger btn-xs" type="button" data-conn-action="delete" data-pid="' + escapeAttr(id) + '" data-cid="' + cid + '">' + escapeHtml(t('upstreams.actionDelete')) + '</button>' +
        '</div>' +
      '</div>';
    }).join('');
    return '<div class="conn-panel" data-conn-panel="' + escapeAttr(id) + '">' +
      '<div class="conn-panel-head">' +
        '<span class="font-semibold text-xs">' + escapeHtml(t('upstreams.connectionsTitle')) + '</span>' +
        '<span class="text-xs muted-text">' +
          escapeHtml(t('upstreams.connStats', String(conns.length), String(completed), String(passed), String(failed))) +
        '</span>' +
      '</div>' +
      '<div class="conn-toolbar">' +
        '<button class="btn btn-ghost btn-xs" type="button" data-conn-action="select-all" data-pid="' + escapeAttr(id) + '">' + escapeHtml(t('upstreams.connSelectAll')) + '</button>' +
        '<label class="text-xs">' + escapeHtml(t('upstreams.connTestModel')) +
          ' <select data-conn-action="test-model" data-pid="' + escapeAttr(id) + '">' +
            (modelOpts || '<option value="">' + escapeHtml(t('upstreams.connTestModelNone')) + '</option>') +
          '</select></label>' +
        '<label class="flex items-center gap-1 text-xs">' +
          '<span class="switch"><input type="checkbox" data-conn-action="rr" data-pid="' + escapeAttr(id) + '"' + (rrOn ? ' checked' : '') + ' /><span class="slider"></span></span>' +
          escapeHtml(t('upstreams.roundRobin')) +
        '</label>' +
        (st.running
          ? '<button class="btn btn-outline btn-xs" type="button" data-conn-action="stop" data-pid="' + escapeAttr(id) + '">' + escapeHtml(t('upstreams.connStop')) + '</button>'
          : '<button class="btn btn-outline btn-xs" type="button" data-conn-action="test-all" data-pid="' + escapeAttr(id) + '">' + escapeHtml(t('upstreams.connTestOneByOne')) + '</button>') +
      '</div>' +
      (rows || '<div class="text-xs muted-text" style="padding:0.35rem 0;">' + escapeHtml(t('upstreams.connectionsEmpty')) + '</div>') +
    '</div>';
  }

  // Hidden providers are moved below a disclosure row instead of being dropped
  // from the DOM: the point of Hide is decluttering a long list, and a provider
  // you cannot see at all is one you cannot unhide.
  function renderProviders() {
    const list = $('upstreamsList');
    if (!list) return;
    if (!upstreamCache.providers.length) {
      list.innerHTML = '<div class="muted-text" style="padding:0.5rem 0;">' + escapeHtml(t('upstreams.providersEmpty')) + '</div>';
      return;
    }
    const shown = upstreamCache.providers.filter(p => !p.hidden);
    const hiddenOnes = upstreamCache.providers.filter(p => p.hidden);
    let html = shown.map(providerCard).join('');
    // Hiding every provider would otherwise leave a blank panel that looks like a
    // load failure, so say what happened.
    if (!shown.length) {
      html += '<div class="muted-text text-xs" style="padding:0.5rem 0;">' +
        escapeHtml(t('upstreams.allHidden')) + '</div>';
    }
    if (hiddenOnes.length) {
      const caret = showHiddenProviders ? 'fa-chevron-down' : 'fa-chevron-right';
      html += '<button class="btn btn-ghost btn-sm provider-hidden-toggle" type="button"' +
        ' data-upstream-toggle-hidden="1" aria-expanded="' + (showHiddenProviders ? 'true' : 'false') + '">' +
        '<i class="fa-solid ' + caret + '" aria-hidden="true"></i>' +
        escapeHtml(t('upstreams.hiddenCount', String(hiddenOnes.length))) +
        '</button>';
      if (showHiddenProviders) html += hiddenOnes.map(providerCard).join('');
    }
    list.innerHTML = html;
  }

  // Render the fetched-model list into the models modal, filtered by the search box.
  function renderModelsModal() {
    const list = $('upstreamModelsList');
    if (!list) return;
    const pid = modelsModalPid;
    const models = providerModels[pid] || [];
    const kw = (modelsModalSearch || '').trim().toLowerCase();
    const filtered = kw ? models.filter(m => m.toLowerCase().includes(kw)) : models;
    const countEl = $('upstreamModelsCount');
    if (countEl) countEl.textContent = t('upstreams.modelsCount', String(filtered.length), String(models.length));
    if (!filtered.length) {
      list.innerHTML = '<div class="muted-text text-xs" style="padding:0.75rem 0;">' + escapeHtml(models.length ? t('upstreams.noModelMatch') : t('upstreams.noModels')) + '</div>';
      return;
    }
    const pidAttr = escapeAttr(pid);
    list.innerHTML = filtered.map(m => {
      const mid = escapeHtml(m);
      const midAttr = escapeAttr(m);
      return '<div class="upstream-model-row">' +
        '<span class="font-mono text-xs upstream-model-id">' + mid + '</span>' +
        '<span class="upstream-model-test text-xs muted-text" data-model-test-for="' + pidAttr + '|' + midAttr + '"></span>' +
        '<span class="flex items-center gap-1">' +
          '<button class="btn btn-outline btn-xs" type="button" data-model-action="copy" data-model="' + midAttr + '">' + escapeHtml(t('upstreams.copy')) + '</button>' +
          '<button class="btn btn-outline btn-xs" type="button" data-model-action="test" data-pid="' + pidAttr + '" data-model="' + midAttr + '">' + escapeHtml(t('upstreams.test')) + '</button>' +
          '<button class="btn btn-outline btn-xs" type="button" data-model-action="route" data-pid="' + pidAttr + '" data-model="' + midAttr + '">' + escapeHtml(t('upstreams.makeRoute')) + '</button>' +
        '</span>' +
      '</div>';
    }).join('');
  }

  function renderModelRoutes() {
    const list = $('modelRoutesList');
    if (!list) return;
    if (!upstreamCache.routes.length) {
      list.innerHTML = '<div class="muted-text" style="padding:0.5rem 0;">' + escapeHtml(t('upstreams.routesEmpty')) + '</div>';
      return;
    }
    list.innerHTML = upstreamCache.routes.map(item => {
      const id = escapeAttr(item.id || '');
      const model = escapeHtml(item.model || '');
      const disabled = !item.enabled
        ? '<span class="text-xs" style="background:rgba(239,68,68,0.15);color:#ef4444;padding:1px 6px;border-radius:4px;">' + escapeHtml(t('upstreams.disabled')) + '</span>'
        : '';
      // The target chain in try-order. The first is what a request normally uses;
      // the rest are failover, so they are dimmed and arrow-chained rather than
      // listed as equals.
      const targets = routeTargets(item);
      // A pool target ends the chain: the forwarder stops relaying there and the
      // request falls through to the account pool, so anything listed after it is
      // never tried. The modal marks those rows unreachable; this view has to agree
      // or the two descriptions of one config contradict each other.
      const poolAt = targets.findIndex(tg => tg.upstreamId === KIRO_POOL_ID && tg.enabled !== false);
      // Targets hidden in the editor collapse into a single "+N" chip here instead
      // of being listed: this view and the modal describe ONE config, so a target
      // the operator tidied out of the editor must not reappear in full — but
      // hiding never stops it being routed, so the chip has to say it is there.
      const hiddenTargets = targets.filter(tg => tg.hidden);
      const shown = [];
      targets.forEach((tg, i) => {
        if (tg.hidden) return;
        const nm = escapeHtml(providerName(tg.upstreamId));
        const rewrite = tg.targetModel ? '<span class="muted-text">:' + escapeHtml(tg.targetModel) + '</span>' : '';
        const dead = poolAt >= 0 && i > poolAt;
        const off = (tg.enabled === false || dead)
          ? ' style="text-decoration:line-through;opacity:0.5;"'
          : (i === 0 ? ' style="font-weight:600;"' : ' style="opacity:0.65;"');
        const tip = dead
          ? ' title="' + escapeAttr(t('upstreams.routeTargetUnreachableHint')) + '"'
          : '';
        shown.push('<span class="text-xs font-mono"' + off + tip + '>' + nm + rewrite + '</span>');
      });
      const hiddenChip = hiddenTargets.length
        ? '<span class="text-xs muted-text" title="' +
            escapeAttr(t('upstreams.routeChainHiddenHint',
              hiddenTargets.map(tg => providerName(tg.upstreamId)).join(', ')) +
              // Same caveat as the editor's run marker: the chain reads top-first, so
              // a hidden targets[0] means the target actually serving the route is the
              // one not shown.
              (targets[0] && targets[0].hidden ? ' · ' + t('upstreams.routeTargetHiddenPrimary') : '')) + '">' +
            escapeHtml(t('upstreams.routeChainHidden', String(hiddenTargets.length))) + '</span>'
        : '';
      const chain = targets.length
        ? shown.join(' <span class="muted-text text-xs">&rsaquo;</span> ')
        : '<span class="text-xs" style="color:#ef4444;">' + escapeHtml(t('upstreams.routeNoTargets')) + '</span>';
      const count = targets.length > 1
        ? '<span class="text-xs muted-text" title="' + escapeAttr(t('upstreams.routeFailoverHint')) + '">(' + targets.length + ')</span>'
        : '';
      return '<div class="card" data-route-id="' + id + '" style="margin-top:0.5rem;padding:0.75rem;">' +
        '<div class="flex items-center gap-2" style="flex-wrap:wrap;justify-content:space-between;">' +
          '<div class="flex items-center gap-2" style="flex-wrap:wrap;">' +
            '<span class="font-semibold font-mono">' + model + '</span>' +
            '<span class="muted-text text-xs">&rarr;</span>' +
            chain +
            hiddenChip +
            count +
            disabled +
          '</div>' +
          '<div class="flex items-center gap-2">' +
            '<label class="switch" title="' + escapeAttr(item.enabled ? t('accounts.disable') : t('accounts.enable')) + '">' +
              '<input type="checkbox" data-route-action="toggle" data-id="' + id + '"' + (item.enabled ? ' checked' : '') + ' />' +
              '<span class="slider"></span>' +
            '</label>' +
            '<button class="btn btn-outline btn-sm" type="button" data-route-action="edit" data-id="' + id + '">' + escapeHtml(t('upstreams.actionEdit')) + '</button>' +
            '<button class="btn btn-danger btn-sm" type="button" data-route-action="delete" data-id="' + id + '">' + escapeHtml(t('upstreams.actionDelete')) + '</button>' +
          '</div>' +
        '</div>' +
      '</div>';
    }).join('');
  }

  // routeTargets normalizes a route to its target list. A route saved by an older
  // build (or hand-edited into config.json) carries only upstreamId/targetModel,
  // and the server migrates those on load — but the UI can be looking at a
  // response produced before that, so it degrades the same way rather than
  // rendering the route as broken.
  function routeTargets(route) {
    if (!route) return [];
    if (Array.isArray(route.targets) && route.targets.length) return route.targets;
    if (route.upstreamId) {
      return [{ upstreamId: route.upstreamId, targetModel: route.targetModel || '', priority: 0, weight: 1, enabled: true }];
    }
    return [];
  }

  async function persistUpstreams() {
    // Never POST connection secrets back: GET only has masks, and connection
    // mutations go through the dedicated CRUD endpoints. Omitting the array
    // tells the server to keep the stored connections.
    const providers = upstreamCache.providers.map(p => {
      const copy = Object.assign({}, p);
      delete copy.connections;
      return copy;
    });
    const res = await api('/upstreams', {
      method: 'POST',
      body: JSON.stringify({ providers, routes: upstreamCache.routes })
    });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
  }

  function openUpstreamModal(entry) {
    upstreamEditingId = entry ? (entry.id || '') : '';
    $('upstreamModalTitle').textContent = t(upstreamEditingId ? 'upstreams.providerModalEdit' : 'upstreams.providerModalCreate');
    $('upstreamForm_name').value = entry ? (entry.name || '') : '';
    $('upstreamForm_baseUrl').value = entry ? (entry.baseUrl || '') : '';
    const keyGroup = $('upstreamForm_apiKeyGroup');
    if (keyGroup) keyGroup.style.display = upstreamEditingId ? 'none' : '';
    $('upstreamForm_apiKey').value = '';
    $('upstreamForm_proxyUrl').value = entry ? (entry.proxyURL || '') : '';
    // Prices are omitted from JSON when zero, and an empty input is what "not
    // priced" should look like — so 0 renders as blank rather than "0".
    $('upstreamForm_priceIn').value = entry && entry.priceInPerM ? String(entry.priceInPerM) : '';
    $('upstreamForm_priceOut').value = entry && entry.priceOutPerM ? String(entry.priceOutPerM) : '';
    $('upstreamForm_enabled').checked = entry ? !!entry.enabled : true;
    openDialog('upstreamModal');
  }

  function closeUpstreamModal() {
    closeDialog('upstreamModal');
    upstreamEditingId = '';
  }

  async function submitUpstreamModal() {
    const name = $('upstreamForm_name').value.trim();
    const baseUrl = $('upstreamForm_baseUrl').value.trim();
    const apiKey = $('upstreamForm_apiKey').value.trim();
    const proxyURL = $('upstreamForm_proxyUrl').value.trim();
    // parseFloat of "" is NaN; normalize to 0 so the field means "unpriced".
    const priceInPerM = parseFloat($('upstreamForm_priceIn').value) || 0;
    const priceOutPerM = parseFloat($('upstreamForm_priceOut').value) || 0;
    const enabled = $('upstreamForm_enabled').checked;
    if (!baseUrl) { toast(t('upstreams.baseUrlRequired'), 'error'); return; }
    if (priceInPerM < 0 || priceOutPerM < 0) { toast(t('upstreams.priceInvalid'), 'error'); return; }
    const prev = JSON.parse(JSON.stringify(upstreamCache));
    try {
      if (upstreamEditingId) {
        const p = upstreamCache.providers.find(x => x.id === upstreamEditingId);
        if (p) {
          p.name = name; p.baseUrl = baseUrl; p.proxyURL = proxyURL; p.enabled = enabled;
          p.priceInPerM = priceInPerM; p.priceOutPerM = priceOutPerM;
        }
      } else {
        upstreamCache.providers.push({ id: '', name, baseUrl, apiKey, proxyURL, enabled, priceInPerM, priceOutPerM });
      }
      await persistUpstreams();
      toast(t('common.saved'), 'success');
      closeUpstreamModal();
      await loadUpstreams();
    } catch (e) {
      upstreamCache = prev;
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }

  async function toggleProvider(id, enabled) {
    const prev = JSON.parse(JSON.stringify(upstreamCache));
    const p = upstreamCache.providers.find(x => x.id === id);
    if (p) p.enabled = enabled;
    try {
      await persistUpstreams();
      renderUpstreams();
    } catch (e) {
      upstreamCache = prev;
      toast((e && e.message) || t('common.saveFailed'), 'error');
      renderUpstreams();
    }
  }

  // Hiding is presentation-only: it never touches `enabled`, so a hidden provider
  // keeps receiving forwards exactly as before. The flag is stored server-side
  // rather than in localStorage so it follows the config across browsers and
  // rides along with export/import.
  async function setProviderHidden(id, hidden) {
    const prev = JSON.parse(JSON.stringify(upstreamCache));
    const p = upstreamCache.providers.find(x => x.id === id);
    if (!p) return;
    p.hidden = hidden;
    // Unhiding from inside the collapsed group would otherwise move the row out
    // of a section the operator can no longer see.
    if (!hidden) showHiddenProviders = true;
    try {
      await persistUpstreams();
      // A hidden-but-enabled provider is the case worth naming out loud, since
      // the row vanishing could otherwise read as "turned off". The same two
      // strings back the badge tooltip, so the two surfaces cannot drift apart.
      toast(t(hidden ? (p.enabled ? 'upstreams.hiddenStillLive' : 'upstreams.hiddenHint') : 'upstreams.unhidden'),
        hidden && p.enabled ? 'warning' : 'success');
      renderUpstreams();
    } catch (e) {
      upstreamCache = prev;
      toast((e && e.message) || t('common.saveFailed'), 'error');
      renderUpstreams();
    }
  }

  function openConnModal(pid, conn) {
    connectionEditing = { providerId: pid, connectionId: conn ? (conn.id || '') : '' };
    $('upstreamConnModalTitle').textContent = t(connectionEditing.connectionId ? 'upstreams.connModalEdit' : 'upstreams.connModalCreate');
    $('upstreamConnForm_name').value = conn ? (conn.name || '') : '';
    $('upstreamConnForm_apiKey').value = '';
    $('upstreamConnForm_enabled').checked = conn ? !!conn.enabled : true;
    const hint = $('upstreamConnForm_apiKeyHint');
    if (hint) hint.textContent = t(connectionEditing.connectionId ? 'upstreams.connApiKeyHintEdit' : 'upstreams.connApiKeyHint');
    openDialog('upstreamConnModal');
  }

  function closeConnModal() {
    closeDialog('upstreamConnModal');
    connectionEditing = { providerId: '', connectionId: '' };
  }

  async function submitConnModal() {
    const pid = connectionEditing.providerId;
    if (!pid) return;
    const name = $('upstreamConnForm_name').value.trim();
    const apiKey = $('upstreamConnForm_apiKey').value.trim();
    const enabled = $('upstreamConnForm_enabled').checked;
    try {
      let res;
      if (connectionEditing.connectionId) {
        const body = { name, enabled };
        if (apiKey) body.apiKey = apiKey;
        res = await api('/upstreams/' + encodeURIComponent(pid) + '/connections/' + encodeURIComponent(connectionEditing.connectionId), {
          method: 'PATCH', body: JSON.stringify(body)
        });
      } else {
        if (!apiKey) { toast(t('upstreams.connApiKeyRequired'), 'error'); return; }
        res = await api('/upstreams/' + encodeURIComponent(pid) + '/connections', {
          method: 'POST', body: JSON.stringify({ name, apiKey, enabled })
        });
      }
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      toast(t('common.saved'), 'success');
      closeConnModal();
      await loadUpstreams();
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }

  async function deleteConnection(pid, cid, name) {
    const ok = await confirmAction(t('upstreams.confirmDeleteConnection', name || t('upstreams.unnamed')), {
      title: t('upstreams.actionDelete'), confirmText: t('upstreams.actionDelete'), variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/upstreams/' + encodeURIComponent(pid) + '/connections/' + encodeURIComponent(cid), { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('upstreams.deleteSuccess'), 'success');
      await loadUpstreams();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  async function toggleConnection(pid, cid, enabled) {
    try {
      const res = await api('/upstreams/' + encodeURIComponent(pid) + '/connections/' + encodeURIComponent(cid), {
        method: 'PATCH', body: JSON.stringify({ enabled })
      });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      await loadUpstreams();
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
      renderProviders();
    }
  }

  async function setRoundRobin(pid, on) {
    const p = upstreamCache.providers.find(x => x.id === pid);
    if (p) p.connectionStrategy = on ? 'round_robin' : 'primary';
    try {
      await persistUpstreams();
      await loadUpstreams();
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }

  function selectedConnections(pid) {
    const p = upstreamCache.providers.find(x => x.id === pid);
    const conns = providerConnections(p);
    const st = connTestState(pid);
    if (!st.selected) return conns;
    return conns.filter(c => st.selected[c.id]);
  }

  async function testConnectionsOneByOne(pid) {
    const st = connTestState(pid);
    if (st.running) return;
    const p = upstreamCache.providers.find(x => x.id === pid);
    const conns = selectedConnections(pid);
    if (!conns.length) { toast(t('upstreams.connNoneSelected'), 'warning'); return; }
    let model = st.model || (providerModels[pid] && providerModels[pid][0]) || '';
    if (!model) {
      try { await fetchProviderModelsSilently(pid); } catch (e) { /* optional */ }
      model = (providerModels[pid] && providerModels[pid][0]) || '';
    }
    if (!model) { toast(t('upstreams.connTestModelRequired'), 'error'); return; }
    st.model = model;
    st.running = true;
    st.abort = new AbortController();
    st.results = {};
    conns.forEach(c => { st.results[c.id] = { status: 'queued' }; });
    renderProviders();
    for (const c of conns) {
      if (!st.running) break;
      st.results[c.id] = { status: 'testing' };
      renderProviders();
      try {
        const res = await api('/upstreams/' + encodeURIComponent(pid) + '/connections/' + encodeURIComponent(c.id) + '/test', {
          method: 'POST',
          body: JSON.stringify({ model }),
          signal: st.abort.signal
        });
        const d = await res.json().catch(() => ({}));
        if (d.error === 'timeout') st.results[c.id] = { status: 'timeout', latencyMs: d.latencyMs };
        else if (d.ok) st.results[c.id] = { status: 'success', latencyMs: d.latencyMs };
        else st.results[c.id] = { status: 'failed', latencyMs: d.latencyMs, http: d.status, error: d.error };
      } catch (e) {
        if (e && e.name === 'AbortError') {
          st.results[c.id] = { status: 'stopped' };
          break;
        }
        st.results[c.id] = { status: 'failed', error: (e && e.message) || 'error' };
      }
    }
    conns.forEach(c => {
      if (st.results[c.id] && st.results[c.id].status === 'queued') st.results[c.id] = { status: 'stopped' };
    });
    st.running = false;
    st.abort = null;
    renderProviders();
  }

  function stopConnectionTests(pid) {
    const st = connTestState(pid);
    st.running = false;
    if (st.abort) st.abort.abort();
  }

  function openBulkModal(pid) {
    bulkImportPid = pid;
    bulkPreview = null;
    bulkResolutions = {};
    const box = $('upstreamBulkText');
    if (box) box.value = '';
    const sum = $('upstreamBulkSummary');
    if (sum) sum.textContent = '';
    const prev = $('upstreamBulkPreview');
    if (prev) prev.innerHTML = '';
    const imp = $('upstreamBulkImportBtn');
    if (imp) imp.disabled = true;
    openDialog('upstreamBulkModal');
  }

  function closeBulkModal() {
    closeDialog('upstreamBulkModal');
    bulkImportPid = '';
    bulkPreview = null;
    bulkResolutions = {};
  }

  function bulkNaming() {
    const el = document.querySelector('input[name="upstreamBulkNaming"]:checked');
    return el ? el.value : 'first_non_key';
  }

  function renderBulkPreview() {
    const el = $('upstreamBulkPreview');
    const sum = $('upstreamBulkSummary');
    const imp = $('upstreamBulkImportBtn');
    if (!el) return;
    if (!bulkPreview) {
      el.innerHTML = '';
      if (sum) sum.textContent = '';
      if (imp) imp.disabled = true;
      return;
    }
    if (sum) {
      sum.textContent = t('upstreams.bulkSummary',
        String(bulkPreview.ready || 0), String(bulkPreview.duplicate || 0),
        String(bulkPreview.ambiguous || 0), String(bulkPreview.invalid || 0));
    }
    const lines = Array.isArray(bulkPreview.lines) ? bulkPreview.lines : [];
    el.innerHTML = '<table class="conn-preview-table"><thead><tr>' +
      '<th>' + escapeHtml(t('upstreams.bulkColLine')) + '</th>' +
      '<th>' + escapeHtml(t('upstreams.bulkColName')) + '</th>' +
      '<th>' + escapeHtml(t('upstreams.bulkColKey')) + '</th>' +
      '<th>' + escapeHtml(t('upstreams.bulkColStatus')) + '</th>' +
      '</tr></thead><tbody>' +
      lines.map(row => {
        let status = row.status || '';
        let extra = '';
        if (status === 'ambiguous' && Array.isArray(row.candidates)) {
          extra = row.candidates.map(c =>
            '<button class="btn btn-outline btn-xs" type="button" data-bulk-col="' + escapeAttr(String(c.index)) + '" data-bulk-line="' + escapeAttr(String(row.line)) + '">' +
              escapeHtml(t('upstreams.bulkChooseCol', String(c.index + 1))) + ' ' + escapeHtml(c.masked || '') +
            '</button>'
          ).join(' ');
        }
        return '<tr class="conn-preview-' + escapeAttr(status) + '">' +
          '<td>' + escapeHtml(String(row.line || '')) + '</td>' +
          '<td>' + escapeHtml(row.name || '') + '</td>' +
          '<td class="font-mono">' + escapeHtml(row.keyMasked || '') + '</td>' +
          '<td>' + escapeHtml(t('upstreams.bulkStatus_' + status) || status) + ' ' + extra + '</td>' +
        '</tr>';
      }).join('') +
      '</tbody></table>';
    if (imp) imp.disabled = !(bulkPreview.ready > 0);
  }

  async function analyzeBulkImport() {
    if (!bulkImportPid) return;
    const text = ($('upstreamBulkText').value || '').trim();
    if (!text) { toast(t('upstreams.bulkEmpty'), 'error'); return; }
    try {
      const res = await api('/upstreams/' + encodeURIComponent(bulkImportPid) + '/connections/preview', {
        method: 'POST',
        body: JSON.stringify({
          text,
          delimiter: 'auto',
          naming: bulkNaming(),
          resolutions: Object.keys(bulkResolutions).map(line => ({ line: parseInt(line, 10), column: bulkResolutions[line] }))
        })
      });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('upstreams.bulkFailed'));
      bulkPreview = d.preview || d;
      renderBulkPreview();
    } catch (e) {
      toast((e && e.message) || t('upstreams.bulkFailed'), 'error');
    }
  }

  async function commitBulkImport() {
    if (!bulkImportPid) return;
    const text = ($('upstreamBulkText').value || '').trim();
    if (!text) { toast(t('upstreams.bulkEmpty'), 'error'); return; }
    try {
      const res = await api('/upstreams/' + encodeURIComponent(bulkImportPid) + '/connections/import', {
        method: 'POST',
        body: JSON.stringify({
          text,
          delimiter: 'auto',
          naming: bulkNaming(),
          resolutions: Object.keys(bulkResolutions).map(line => ({ line: parseInt(line, 10), column: bulkResolutions[line] }))
        })
      });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('upstreams.bulkFailed'));
      toast(t('upstreams.bulkImported', String(d.added || 0), String(d.duplicate || 0), String(d.ambiguous || 0), String(d.invalid || 0)), 'success');
      closeBulkModal();
      await loadUpstreams();
    } catch (e) {
      toast((e && e.message) || t('upstreams.bulkFailed'), 'error');
    }
  }

  async function deleteProvider(id, name) {
    const ok = await confirmAction(t('upstreams.confirmDeleteProvider', name || t('upstreams.unnamed')), {
      title: t('upstreams.actionDelete'), confirmText: t('upstreams.actionDelete'), variant: 'danger'
    });
    if (!ok) return;
    const prev = JSON.parse(JSON.stringify(upstreamCache));
    upstreamCache.providers = upstreamCache.providers.filter(x => x.id !== id);
    try {
      await persistUpstreams();
      toast(t('upstreams.deleteSuccess'), 'success');
      await loadUpstreams();
    } catch (e) {
      upstreamCache = prev;
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  // Fetch the model list from a provider's /models endpoint (like 9router's
  // "Import from /models"). Uses the stored provider ID so the backend supplies
  // the saved API key even though the panel only ever sees a masked value.
  async function loadProviderModels(pid) {
    const p = upstreamCache.providers.find(x => x.id === pid);
    if (!p) return;
    const btn = document.querySelector('[data-upstream-action="load"][data-id="' + (window.CSS && CSS.escape ? CSS.escape(pid) : pid) + '"]');
    if (btn) { btn.disabled = true; btn.textContent = t('upstreams.loadingModels'); }
    try {
      const res = await api('/upstream-models', {
        method: 'POST',
        body: JSON.stringify({ id: pid, baseUrl: p.baseUrl, proxyURL: p.proxyURL })
      });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.error) throw new Error(d.error || t('upstreams.loadModelsFailed'));
      providerModels[pid] = Array.isArray(d.models) ? d.models : [];
      if (!providerModels[pid].length) { toast(t('upstreams.noModels'), 'warning'); return; }
      openModelsModal(pid);
    } catch (e) {
      providerModels[pid] = [];
      toast((e && e.message) || t('upstreams.loadModelsFailed'), 'error');
    } finally {
      if (btn) { btn.disabled = false; btn.textContent = t('upstreams.loadModels'); }
    }
  }

  // Open the model-browser modal for a provider (list can be long, so it scrolls
  // and is searchable).
  function openModelsModal(pid) {
    modelsModalPid = pid;
    modelsModalSearch = '';
    const p = upstreamCache.providers.find(x => x.id === pid);
    const titleEl = $('upstreamModelsModalTitle');
    if (titleEl) titleEl.textContent = t('upstreams.modelsModalTitle', (p && (p.name || p.baseUrl)) || '');
    const search = $('upstreamModelsSearch');
    if (search) search.value = '';
    renderModelsModal();
    openDialog('upstreamModelsModal');
    if (search) setTimeout(() => search.focus(), 50);
  }

  function closeModelsModal() {
    closeDialog('upstreamModelsModal');
    modelsModalPid = '';
    modelsModalSearch = '';
  }

  // Send a minimal probe to one model and show status + latency inline.
  async function testModel(pid, model) {
    const p = upstreamCache.providers.find(x => x.id === pid);
    if (!p) return;
    const slot = document.querySelector('[data-model-test-for="' + (window.CSS && CSS.escape ? CSS.escape(pid) : pid) + '|' + (window.CSS && CSS.escape ? CSS.escape(model) : model) + '"]');
    if (slot) slot.textContent = t('upstreams.testing');
    try {
      const res = await api('/upstream-test', {
        method: 'POST',
        body: JSON.stringify({ id: pid, baseUrl: p.baseUrl, proxyURL: p.proxyURL, model })
      });
      const d = await res.json().catch(() => ({}));
      if (!slot) {
        toast(d.ok ? t('upstreams.testOk', String(d.latencyMs || 0)) : t('upstreams.testFail'), d.ok ? 'success' : 'error');
        return;
      }
      if (d.ok) {
        slot.textContent = '✓ ' + t('upstreams.testOk', String(d.latencyMs || 0));
        slot.style.color = 'var(--success, #22c55e)';
      } else {
        slot.textContent = '✗ ' + (d.status ? ('HTTP ' + d.status) : t('upstreams.testFail'));
        slot.style.color = 'var(--danger, #ef4444)';
      }
    } catch (e) {
      if (slot) { slot.textContent = '✗ ' + t('upstreams.testFail'); slot.style.color = 'var(--danger, #ef4444)'; }
    }
  }

  // Open the route modal prefilled from a fetched model (one-click route creation).
  // The browsed provider becomes the primary target, and the model name is used
  // on both sides: it is what the client will send and what that provider expects.
  function makeRouteFromModel(pid, model) {
    closeModelsModal();
    routeEditingId = '';
    $('modelRouteModalTitle').textContent = t('upstreams.routeModalCreate');
    $('routeForm_model').value = model;
    $('routeForm_enabled').checked = true;
    routeTargetDraft = [{ upstreamId: pid, targetModel: '', priority: 0, weight: 1, enabled: true }];
    renderRouteTargets();
    populateClientModelDatalist();
    openDialog('modelRouteModal');
  }

  // Fetch the model names this kiro-go instance serves (public /v1/models, no auth).
  async function loadKiroGoModels() {
    try {
      const res = await fetch('/v1/models', { headers: { 'Accept': 'application/json' } });
      if (!res.ok) return;
      const d = await res.json().catch(() => ({}));
      const arr = Array.isArray(d.data) ? d.data : [];
      kiroGoModels = arr.map(m => (m && (m.id || m.name)) || '').filter(Boolean);
    } catch (e) { /* suggestions only; ignore failures */ }
  }

  // Fill the Client Model datalist with the models this kiro-go instance serves.
  // These are the names clients send; the field stays free-text and optional.
  function populateClientModelDatalist() {
    const dl = $('routeClientModelList');
    if (!dl) return;
    dl.innerHTML = kiroGoModels.map(m => '<option value="' + escapeAttr(m) + '"></option>').join('');
  }

  // routeProviderOptions returns the selectable targets (configured upstreams plus
  // the Kiro pool) with labels made UNIQUE. A searchable text field maps a chosen
  // label back to exactly one id, so duplicate provider names (allowed in config)
  // must be disambiguated with a short id suffix or the mapping would be ambiguous.
  function routeProviderOptions() {
    const out = (upstreamCache.providers || []).map(p => ({
      id: p.id || '',
      label: p.name || p.baseUrl || p.id || ''
    }));
    out.push({ id: KIRO_POOL_ID, label: t('upstreams.kiroPoolTarget') });
    const counts = {};
    out.forEach(o => { counts[o.label] = (counts[o.label] || 0) + 1; });
    out.forEach(o => {
      if (counts[o.label] > 1) o.label = o.label + ' (' + String(o.id).slice(0, 6) + ')';
    });
    return out;
  }

  function routeProviderLabel(id) {
    const o = routeProviderOptions().find(x => x.id === id);
    return o ? o.label : '';
  }

  function routeProviderIdFromLabel(label) {
    const o = routeProviderOptions().find(x => x.label === label);
    return o ? o.id : '';
  }

  // Fill the shared provider datalist so every target row's provider field is a
  // searchable dropdown (type to filter among many upstreams).
  function populateProviderDatalist() {
    const dl = $('routeProviderList');
    if (!dl) return;
    dl.innerHTML = routeProviderOptions()
      .map(o => '<option value="' + escapeAttr(o.label) + '"></option>')
      .join('');
  }

  // targetModelOptionsHTML builds the <option>s for one row's model datalist from
  // the row provider's fetched model list.
  function targetModelOptionsHTML(pid) {
    const opts = (providerModels[pid] || []).filter(Boolean);
    return opts.map(m => '<option value="' + escapeAttr(m) + '"></option>').join('');
  }

  // ensureProviderModels kicks a quiet fetch of a provider's model list (no modal)
  // the first time it is needed, so each row's model dropdown fills in on its own.
  function ensureProviderModels(pid) {
    if (!pid || pid === KIRO_POOL_ID) return;
    if (providerModels[pid] === undefined && !providerModelsLoading[pid]) {
      fetchProviderModelsSilently(pid);
    }
  }

  // Fetch a provider's model list without opening the browser modal. Populates the
  // providerModels cache and re-renders the route targets so the matching row's
  // model dropdown fills in. Failures are swallowed (suggestions only).
  async function fetchProviderModelsSilently(pid) {
    const p = upstreamCache.providers.find(x => x.id === pid);
    if (!p) return;
    providerModelsLoading[pid] = true;
    try {
      const res = await api('/upstream-models', {
        method: 'POST',
        body: JSON.stringify({ id: pid, baseUrl: p.baseUrl, proxyURL: p.proxyURL })
      });
      const d = await res.json().catch(() => ({}));
      providerModels[pid] = (res.ok && !d.error && Array.isArray(d.models)) ? d.models : [];
    } catch (e) {
      providerModels[pid] = [];
    } finally {
      providerModelsLoading[pid] = false;
      // Refresh the open route modal so the row(s) targeting this provider show
      // the freshly fetched models. Guarded so this is a no-op when closed.
      if (isDialogOpen('modelRouteModal') && routeTargetDraft.length) renderRouteTargets();
    }
  }

  function openRouteModal(entry) {
    routeEditingId = entry ? (entry.id || '') : '';
    $('modelRouteModalTitle').textContent = t(routeEditingId ? 'upstreams.routeModalEdit' : 'upstreams.routeModalCreate');
    $('routeForm_model').value = entry ? (entry.model || '') : '';
    $('routeForm_enabled').checked = entry ? !!entry.enabled : true;
    // draftFromTargets copies as it derives sameTier, so Cancel discards target
    // edits; the cache is only touched on save.
    routeTargetTests = {};
    routeTargetDraft = entry ? draftFromTargets(routeTargets(entry)) : [newRouteTarget()];
    if (!routeTargetDraft.length) routeTargetDraft = [newRouteTarget()];
    // A stored route can carry a pool target sharing a tier — written by an older
    // build, or by hand. Normalize on load so the badges describe the routing the
    // resolver will actually perform rather than the stale config.
    normalizeDraftTiers();
    populateProviderDatalist();
    renderRouteTargets();
    populateClientModelDatalist();
    openDialog('modelRouteModal');
  }

  // newRouteTarget defaults to the first provider: with one configured provider
  // (the common case) the operator never has to touch the select.
  //
  // With NO providers it defaults to the Kiro pool rather than to an empty id.
  // submitRouteModal drops targets with a blank upstreamId, so an empty default
  // would make the row silently vanish on save; the pool is always available and
  // is the only destination that needs no configuration at all.
  function newRouteTarget() {
    const first = upstreamCache.providers[0];
    return {
      upstreamId: (first && first.id) || KIRO_POOL_ID,
      targetModel: '', priority: 0, weight: 1, enabled: true, hidden: false, sameTier: false
    };
  }

  // draftFromTargets rebuilds the editable draft from stored targets, deriving
  // each row's sameTier flag from whether it repeats the previous row's priority.
  // Storage keeps explicit priority numbers; the UI shows tier membership. This is
  // the inverse of the numbering done in submitRouteModal, so an edit round-trip
  // preserves tiers instead of flattening them.
  function draftFromTargets(targets) {
    return targets.map((tg, i) => {
      const prev = targets[i - 1];
      return {
        upstreamId: tg.upstreamId || '',
        targetModel: tg.targetModel || '',
        priority: tg.priority || 0,
        weight: tg.weight > 0 ? tg.weight : 1,
        enabled: tg.enabled !== false,
        hidden: !!tg.hidden,
        sameTier: !!prev && (prev.priority || 0) === (tg.priority || 0)
      };
    });
  }

  // Tier membership belongs to the SLOT, not to the target that happens to sit in
  // it. Reordering is the documented "switch provider without losing the old
  // setup" gesture, so moving a target into the primary slot must not drag its
  // old sameTier flag along and collapse a shared tier. These two helpers let a
  // reorder restore the flags by position after the rows have moved.
  function draftTierFlags() {
    return routeTargetDraft.map(tg => !!tg.sameTier);
  }
  function applyPositionalTiers(flags) {
    routeTargetDraft.forEach((tg, i) => { tg.sameTier = !!flags[i]; });
    normalizeDraftTiers();
  }

  // The first row opens the first tier by definition. A stale sameTier there
  // would make draftTierNumbers start counting at 1 and mislabel every badge, so
  // every structural change funnels through this.
  //
  // It also enforces that the Kiro pool never shares a tier. Reaching a pool
  // target ends the upstream walk, so a tier containing both the pool and an
  // upstream is not a thing the router can honor: the resolver holds the sentinel
  // at the end of its tier (config/route_resolve.go), which means a shared tier
  // silently means something different from what the badges show. Rather than
  // render a state the backend reinterprets, the pool always opens its own tier —
  // both the pool row itself and the row below it, which would otherwise join the
  // pool's tier.
  function normalizeDraftTiers() {
    if (!routeTargetDraft.length) return;
    routeTargetDraft[0].sameTier = false;
    routeTargetDraft.forEach((tg, i) => {
      if (i === 0) return;
      const prev = routeTargetDraft[i - 1];
      if (tg.upstreamId === KIRO_POOL_ID || prev.upstreamId === KIRO_POOL_ID) {
        tg.sameTier = false;
      }
    });
  }

  // renderedTargetIndexes is the draft filtered down to the rows the operator can
  // actually see: hidden rows drop out of it while their run is collapsed.
  // Everything that means "the row above/below" reads this rather than the raw
  // draft — otherwise the ↑ next to a visible row would swap it with an invisible
  // one and read as a button that does nothing.
  function renderedTargetIndexes() {
    const out = [];
    routeTargetDraft.forEach((tg, i) => {
      if (showHiddenRouteTargets || !tg.hidden) out.push(i);
    });
    return out;
  }

  // moveRenderedRow backs the ↑/↓ buttons, which are also the keyboard-accessible
  // path to what dragging does. dir is -1 (up) or +1 (down) counted in RENDERED
  // positions, so one press always moves the row exactly one visible slot,
  // stepping over a whole collapsed run when that is what sits between. Returns
  // whether anything moved.
  function moveRenderedRow(from, dir) {
    const order = renderedTargetIndexes();
    const at = order.indexOf(from);
    if (at < 0) return false;
    const neighbour = order[at + dir];
    if (neighbour === undefined) return false;
    // moveDraftRow takes a gap index: moving up lands in the neighbour's slot,
    // moving down lands just past it. It also restores the tier flags by position,
    // which is what keeps "same tier as above" attached to the slot instead of
    // riding along with the row.
    return moveDraftRow(from, dir > 0 ? neighbour + 1 : neighbour);
  }

  // moveDraftRow relocates one row to an insertion slot, as produced by a drop.
  // insertAt is a gap index (0 == above the first row), so dropping either side
  // of the row's own position is a no-op. Returns whether anything moved.
  function moveDraftRow(from, insertAt) {
    if (!routeTargetDraft[from]) return false;
    if (insertAt === from || insertAt === from + 1) return false;
    const flags = draftTierFlags();
    const row = routeTargetDraft.splice(from, 1)[0];
    routeTargetDraft.splice(insertAt > from ? insertAt - 1 : insertAt, 0, row);
    applyPositionalTiers(flags);
    return true;
  }

  // draftTierNumbers returns each draft row's tier index, using the same rule as
  // submitRouteModal so the badges always describe what will actually be saved.
  function draftTierNumbers() {
    let tier = -1;
    return routeTargetDraft.map((tg, i) => {
      if (i === 0 || !tg.sameTier) tier++;
      return tier;
    });
  }

  // Renders the draft target list. Tiers, not raw positions, carry the meaning:
  // consecutive rows can share a tier, and rows in the same tier split traffic by
  // weight instead of acting as each other's fallback. Reordering is still the
  // "switch provider" gesture; the tier toggle is what makes weight usable.
  function renderRouteTargets() {
    const box = $('routeTargetsList');
    if (!box) return;
    const tiers = draftTierNumbers();
    // A tier with more than one member is the only case where weight does
    // anything, so the weight input is enabled exactly there.
    const tierSizes = {};
    tiers.forEach(n => { tierSizes[n] = (tierSizes[n] || 0) + 1; });
    // Reaching a pool target ends the upstream walk (proxy/upstream_forward.go
    // returns false there and the request falls through to the account pool), so
    // every row below the first one is unreachable. Rows are not dropped for it —
    // moving the pool row back down revives them — but they are marked, because a
    // silently dead target is worse than a visibly dead one.
    const poolRow = routeTargetDraft.findIndex(tg => tg.upstreamId === KIRO_POOL_ID);
    // Rendered order drives the ↑/↓ enabled state below; it equals the draft while
    // hidden rows are expanded.
    const rendered = renderedTargetIndexes();
    const renderedAt = {};
    rendered.forEach((idx, pos) => { renderedAt[idx] = pos; });
    // The marker names what stands behind it: how many targets, how many of those
    // are still enabled — the case where "out of sight" is easiest to misread as
    // "off" — and, in the tooltip, which providers. That tooltip is also what
    // explains an "unreachable" badge whose cause (a Kiro Pool row) is hidden.
    const hiddenRunMarker = (start, end) => {
      const run = routeTargetDraft.slice(start, end);
      const live = run.filter(tg => tg.enabled !== false).length;
      // "Which target serves this route" is the question the editor exists to
      // answer, so a run that swallowed the top tier has to say so — otherwise
      // collapsing (or reordering across a collapsed run) leaves the primary
      // invisible with nothing on screen admitting it.
      let hasPrimary = false;
      for (let j = start; j < end; j++) {
        if (tiers[j] === 0) hasPrimary = true;
      }
      const label = t('upstreams.routeTargetHiddenRun', String(run.length)) +
        (live ? ' · ' + t('upstreams.routeTargetHiddenLiveCount', String(live)) : '') +
        (hasPrimary ? ' · ' + t('upstreams.routeTargetHiddenPrimary') : '');
      const action = showHiddenRouteTargets
        ? t('upstreams.routeTargetHiddenCollapse')
        : t('upstreams.routeTargetHiddenShow');
      const names = run.map(tg => routeProviderLabel(tg.upstreamId) || tg.upstreamId).join(', ');
      return '<button class="route-target-hidden-run" type="button" data-target-toggle-hidden="1"' +
        ' aria-expanded="' + (showHiddenRouteTargets ? 'true' : 'false') + '"' +
        ' title="' + escapeAttr(t('upstreams.routeTargetHiddenNames', names)) + '">' +
        '<span>' + escapeHtml(label) + '</span>' +
        '<span class="route-target-hidden-run-action">' + escapeHtml(action) + '</span>' +
      '</button>';
    };
    const targetRow = (tg, i) => {
      const isPool = tg.upstreamId === KIRO_POOL_ID;
      const pos = renderedAt[i] === undefined ? -1 : renderedAt[i];
      const unreachable = poolRow >= 0 && i > poolRow;
      const tier = tiers[i];
      // The pool is never a weighted member of a tier: it terminates the chain
      // rather than being relayed to, so splitting traffic with it is not a thing
      // the router can do.
      const shares = tierSizes[tier] > 1 && !isPool;
      const badge = tier === 0
        ? '<span class="text-xs" style="background:rgba(34,197,94,0.15);color:#16a34a;padding:1px 6px;border-radius:4px;">' + escapeHtml(t('upstreams.routeTargetPrimary')) + '</span>'
        : '<span class="text-xs muted-text">' + escapeHtml(t('upstreams.routeTargetFallback')) + ' ' + tier + '</span>';
      // Rows after the first can join the tier above; joining is what puts two
      // targets at equal priority so their weights become meaningful.
      //
      // Not offered where it cannot mean anything: the pool never shares a tier
      // (it terminates the chain rather than splitting traffic), so neither a pool
      // row nor the row directly below one gets the toggle. normalizeDraftTiers
      // enforces the same rule on the data, so a stale flag cannot survive either;
      // hiding the control keeps the UI from advertising a state it will undo.
      const noTierToggle = i === 0 || isPool ||
        (routeTargetDraft[i - 1] && routeTargetDraft[i - 1].upstreamId === KIRO_POOL_ID);
      const tierToggle = noTierToggle
        ? ''
        : '<label class="text-xs muted-text flex items-center gap-1" title="' + escapeAttr(t('upstreams.routeTargetSameTierHint')) + '">' +
            '<input type="checkbox" data-target-field="sameTier" data-index="' + i + '"' + (tg.sameTier ? ' checked' : '') + ' />' +
            escapeHtml(t('upstreams.routeTargetSameTier')) +
          '</label>';
      const shareLabel = shares
        ? '<span class="text-xs muted-text">' + escapeHtml(t('upstreams.routeTargetSplitting')) + '</span>'
        : '';
      const poolNote = isPool
        ? '<span class="text-xs muted-text" title="' + escapeAttr(t('upstreams.kiroPoolTargetHint')) + '">' +
            escapeHtml(t('upstreams.kiroPoolTargetNote')) + '</span>'
        : '';
      const deadNote = unreachable
        ? '<span class="text-xs" style="background:rgba(239,68,68,0.15);color:#ef4444;padding:1px 6px;border-radius:4px;"' +
            ' title="' + escapeAttr(t('upstreams.routeTargetUnreachableHint')) + '">' +
            escapeHtml(t('upstreams.routeTargetUnreachable')) + '</span>'
        : '';
      // A hidden target that is still enabled keeps serving traffic from a slot it
      // no longer visibly occupies. The badge says so on the row rather than
      // letting the eye icon imply "off" — the same reasoning, and the same pair of
      // strings-per-state shape, as the hidden-provider badge.
      const hiddenBadge = tg.hidden
        ? '<span class="text-xs" style="background:rgba(148,163,184,0.18);color:var(--muted-foreground);padding:1px 6px;border-radius:4px;"' +
            ' title="' + escapeAttr(t(tg.enabled === false ? 'upstreams.routeTargetHiddenHint' : 'upstreams.routeTargetHiddenLive')) + '">' +
            escapeHtml(t('upstreams.hidden')) + '</span>'
        : '';
      const hideIcon = tg.hidden ? 'fa-eye' : 'fa-eye-slash';
      const hideTitle = tg.hidden ? t('upstreams.actionUnhide') : t('upstreams.actionHide');
      // The probe is keyed by what this row will actually send, so the slot follows
      // the row's own provider+model pair and a verdict cannot outlive an edit that
      // changed either half.
      const probeModel = routeTargetProbeModel(tg);
      const testKey = tg.upstreamId + '|' + probeModel;
      // The pool is answered locally instead of being relayed to, so there is no
      // endpoint to probe; with no model name on either side there is nothing to ask
      // for. Both are disabled-with-a-reason rather than hidden, so the button does
      // not silently come and go.
      const testTitle = isPool
        ? t('upstreams.routeTargetTestPoolHint')
        : (probeModel ? t('upstreams.routeTargetTestHint', probeModel) : t('upstreams.routeTargetTestNoModel'));
      const testSlot = '<span class="route-target-test text-xs font-mono" data-target-test-for="' +
        escapeAttr(testKey) + '"></span>';
      // The row is only made draggable on mousedown over the handle (see the
      // dragstart wiring), so dragging never starts from the text inputs and
      // ordinary text selection inside them keeps working.
      const handle = '<span class="route-target-handle" data-target-handle="1" aria-hidden="true"' +
        ' title="' + escapeAttr(t('common.dragToReorder')) + '">' +
        '<i class="fa-solid fa-grip-vertical"></i></span>';
      // Each row's model field suggests ITS OWN provider's models via a per-row
      // datalist. A single shared datalist (the old behaviour) could only ever
      // reflect one provider, so fallback rows suggested the primary's models.
      const modelListId = 'routeTargetModelList-' + i;
      const providerField =
        '<input type="text" data-target-field="upstreamProvider" data-index="' + i + '" list="routeProviderList"' +
          ' autocomplete="off" value="' + escapeAttr(routeProviderLabel(tg.upstreamId)) + '" style="flex:1;min-width:9rem;"' +
          ' placeholder="' + escapeAttr(t('upstreams.providerSearchPlaceholder')) + '" />';
      return '<div class="card route-target-row' + (tg.hidden ? ' is-hidden-target' : '') + '" draggable="false"' +
        ' data-target-index="' + i + '" style="margin-top:0.5rem;padding:0.5rem;">' +
        '<div class="flex items-center gap-2" style="flex-wrap:wrap;justify-content:space-between;">' +
          '<div class="flex items-center gap-2" style="flex-wrap:wrap;">' + handle + badge + hiddenBadge + tierToggle + shareLabel + poolNote + deadNote + testSlot + '</div>' +
          '<div class="flex items-center gap-1">' +
            '<button class="btn btn-outline btn-sm" type="button" data-target-action="up" data-index="' + i + '"' +
              (pos > 0 ? '' : ' disabled') + ' title="' + escapeAttr(t('upstreams.routeTargetUp')) + '">&uarr;</button>' +
            '<button class="btn btn-outline btn-sm" type="button" data-target-action="down" data-index="' + i + '"' +
              (pos >= 0 && pos < rendered.length - 1 ? '' : ' disabled') + ' title="' + escapeAttr(t('upstreams.routeTargetDown')) + '">&darr;</button>' +
            '<button class="btn btn-outline btn-sm" type="button" data-target-action="test" data-index="' + i + '"' +
              (isPool || !probeModel ? ' disabled' : '') +
              ' title="' + escapeAttr(testTitle) + '" aria-label="' + escapeAttr(t('upstreams.test')) + '">' +
              '<i class="fa-solid fa-bolt" aria-hidden="true"></i></button>' +
            '<button class="btn btn-outline btn-sm" type="button" data-target-action="hide" data-index="' + i + '"' +
              ' title="' + escapeAttr(hideTitle) + '" aria-label="' + escapeAttr(hideTitle) + '">' +
              '<i class="fa-solid ' + hideIcon + '" aria-hidden="true"></i></button>' +
            '<button class="btn btn-danger btn-sm" type="button" data-target-action="remove" data-index="' + i + '"' +
              (routeTargetDraft.length <= 1 ? ' disabled' : '') + '>&times;</button>' +
          '</div>' +
        '</div>' +
        '<div class="flex items-center gap-2" style="flex-wrap:wrap;margin-top:0.35rem;">' +
          providerField +
          // Target Model rewrites the model name sent to an upstream HTTP endpoint.
          // The pool is not relayed to, so there is nothing to rewrite: the request
          // reaches the account pool under its original client model name. Showing
          // the field would invite an edit that silently does nothing.
          (isPool
            ? '<span class="muted-text text-xs" style="flex:1;min-width:9rem;">' +
                escapeHtml(t('upstreams.kiroPoolTargetNoRewrite')) + '</span>'
            : '<input type="text" data-target-field="targetModel" data-index="' + i + '" list="' + modelListId + '" autocomplete="off"' +
                ' value="' + escapeAttr(tg.targetModel || '') + '" style="flex:1;min-width:9rem;"' +
                ' placeholder="' + escapeAttr(t('upstreams.targetModelPlaceholder')) + '" />' +
              '<datalist id="' + modelListId + '">' + targetModelOptionsHTML(tg.upstreamId) + '</datalist>') +
          '<input type="number" min="1" data-target-field="weight" data-index="' + i + '"' +
            ' value="' + escapeAttr(String(tg.weight > 0 ? tg.weight : 1)) + '" style="width:4.5rem;"' +
            (shares ? '' : ' disabled') +
            ' title="' + escapeAttr(t(shares ? 'upstreams.routeTargetWeightHint' : 'upstreams.routeTargetWeightInert')) + '" />' +
          '<label class="switch" title="' + escapeAttr(t('upstreams.formEnabled')) + '">' +
            '<input type="checkbox" data-target-field="enabled" data-index="' + i + '"' + (tg.enabled === false ? '' : ' checked') + ' />' +
            '<span class="slider"></span>' +
          '</label>' +
        '</div>' +
      '</div>';
    };
    // Hidden rows are elided as a run, with the marker standing exactly where they
    // sit, so the visible chain never claims two rows are adjacent when they are
    // not — "same tier as above" and the pool's end-of-chain rule are both stated
    // relative to the row above, and a silently closed gap would misdescribe both.
    // Expanding keeps the marker as that run's header, which is what makes the
    // collapse reversible from the same control.
    let html = '';
    for (let i = 0; i < routeTargetDraft.length;) {
      if (!showHiddenRouteTargets && routeTargetDraft[i].hidden) {
        const start = i;
        while (i < routeTargetDraft.length && routeTargetDraft[i].hidden) i++;
        html += hiddenRunMarker(start, i);
        continue;
      }
      if (showHiddenRouteTargets && routeTargetDraft[i].hidden &&
          (i === 0 || !routeTargetDraft[i - 1].hidden)) {
        let end = i;
        while (end < routeTargetDraft.length && routeTargetDraft[end].hidden) end++;
        html += hiddenRunMarker(i, end);
      }
      html += targetRow(routeTargetDraft[i], i);
      i++;
    }
    // Hiding every target would otherwise leave the editor looking empty, which
    // reads as "this route lost its targets" rather than "they are collapsed".
    if (!rendered.length) {
      html += '<div class="muted-text text-xs" style="padding:0.5rem 0;">' +
        escapeHtml(t('upstreams.routeTargetAllHidden')) + '</div>';
    }
    box.innerHTML = html;
    // The row markup ships its verdict slot empty, so restore what has been probed
    // this session: reorder / hide / provider change all rebuild this list, and a
    // result that vanished on the next click would read as "the test was lost".
    Object.keys(routeTargetTests).forEach(paintRouteTargetTest);
    // Fill each row's model dropdown from its own provider, fetching quietly the
    // first time a provider's model list is needed.
    routeTargetDraft.forEach(tg => ensureProviderModels(tg.upstreamId));
  }

  function closeRouteModal() {
    closeDialog('modelRouteModal');
    routeEditingId = '';
    routeTargetDraft = [];
    routeTargetTests = {};
  }

  // The model a target will ACTUALLY send: its rewrite when set, otherwise the
  // client model, because an empty Target Model means "keep the original name"
  // (see the field's own note). Probing anything else would test a request the
  // forwarder never makes.
  function routeTargetProbeModel(tg) {
    const rewrite = ((tg && tg.targetModel) || '').trim();
    if (rewrite) return rewrite;
    const el = $('routeForm_model');
    return el ? el.value.trim() : '';
  }

  // Paint one verdict into every row aiming at that pair WITHOUT re-rendering the
  // list: a probe can land while the operator is typing in a sibling field, and a
  // full re-render would take the caret with it.
  function paintRouteTargetTest(key) {
    const st = routeTargetTests[key];
    const sel = '[data-target-test-for="' + (window.CSS && CSS.escape ? CSS.escape(key) : key) + '"]';
    qsa(sel).forEach(el => {
      el.textContent = !st ? '' : (st.state === 'testing' ? t('upstreams.testing') : st.text);
      el.style.color = st && st.state === 'ok' ? 'var(--success, #22c55e)'
        : st && st.state === 'fail' ? 'var(--danger, #ef4444)' : '';
      el.title = (st && st.detail) || '';
    });
  }

  // Send the same one-token probe the models browser uses (POST /upstream-test),
  // but aimed at the pair THIS row will forward. Creds are resolved server-side
  // from the provider id, so the masked key the admin UI holds is not a problem.
  async function testRouteTarget(i) {
    const tg = routeTargetDraft[i];
    if (!tg || tg.upstreamId === KIRO_POOL_ID) return;
    const model = routeTargetProbeModel(tg);
    if (!model) { toast(t('upstreams.routeTargetTestNoModel'), 'error'); return; }
    const key = tg.upstreamId + '|' + model;
    const p = upstreamCache.providers.find(x => x.id === tg.upstreamId);
    routeTargetTests[key] = { state: 'testing' };
    paintRouteTargetTest(key);
    try {
      const res = await api('/upstream-test', {
        method: 'POST',
        body: JSON.stringify({ id: tg.upstreamId, baseUrl: p ? p.baseUrl : '', proxyURL: p ? p.proxyURL : '', model })
      });
      const d = await res.json().catch(() => ({}));
      // Which path answered matters: the server probes OpenAI's /chat/completions and
      // falls back to Anthropic's /messages, and a target that only speaks one shape
      // still forwards fine for the clients that use it.
      const via = d.path ? t('upstreams.routeTargetTestVia', d.path) : '';
      routeTargetTests[key] = d.ok
        ? { state: 'ok', text: '✓ ' + t('upstreams.testOk', String(d.latencyMs || 0)), detail: via }
        : {
            state: 'fail',
            text: '✗ ' + (d.status ? ('HTTP ' + d.status) : t('upstreams.testFail')),
            // The upstream's own error body, on the tooltip: "HTTP 404" alone does not
            // say whether the model name or the base URL is the wrong one.
            detail: (via ? via + ' — ' : '') + String(d.error || '').slice(0, 400)
          };
    } catch (e) {
      routeTargetTests[key] = { state: 'fail', text: '✗ ' + t('upstreams.testFail'), detail: (e && e.message) || '' };
    }
    paintRouteTargetTest(key);
  }

  async function submitRouteModal() {
    const model = $('routeForm_model').value.trim();
    const enabled = $('routeForm_enabled').checked;
    if (!model) { toast(t('upstreams.routeModelRequired'), 'error'); return; }
    // Priority comes from list position, but rows flagged sameTier share the tier
    // of the row above instead of starting a new one. That distinction is what
    // makes weight reachable at all: the resolver only splits traffic by weight
    // among targets of EQUAL priority (config/route_resolve.go), so numbering
    // every row 0,1,2,... — as this did before — left every tier with a single
    // member and the weight field permanently inert.
    //
    // Normalize once more before numbering: what gets SAVED must match the badges,
    // and a pool row may never share a tier (the resolver holds the sentinel last
    // within its tier, so a shared tier would persist an order the router refuses
    // to honor).
    normalizeDraftTiers();
    const kept = routeTargetDraft.filter(tg => tg.upstreamId);
    let tier = -1;
    const targets = kept.map((tg, i) => {
      // The first row always opens a tier; sameTier is meaningless there.
      if (i === 0 || !tg.sameTier) tier++;
      return {
        upstreamId: tg.upstreamId,
        targetModel: (tg.targetModel || '').trim(),
        priority: tier,
        weight: tg.weight > 0 ? tg.weight : 1,
        enabled: tg.enabled !== false,
        // Presentation-only, but it belongs in the saved route: the operator's
        // tidy-up of a long chain should survive a reload and follow the config
        // through export/import, exactly like a hidden provider.
        hidden: !!tg.hidden
      };
    });
    if (!targets.length) { toast(t('upstreams.providerRequired'), 'error'); return; }
    const prev = JSON.parse(JSON.stringify(upstreamCache));
    try {
      if (routeEditingId) {
        const r = upstreamCache.routes.find(x => x.id === routeEditingId);
        if (r) {
          r.model = model;
          r.targets = targets;
          r.enabled = enabled;
          // Clear the legacy 1:1 fields so they cannot contradict targets. The
          // server re-derives them for export; keeping a stale value here would
          // make an old build resolve a provider the operator already replaced.
          r.upstreamId = '';
          r.targetModel = '';
        }
      } else {
        upstreamCache.routes.push({ id: '', model, targets, enabled });
      }
      await persistUpstreams();
      toast(t('common.saved'), 'success');
      closeRouteModal();
      await loadUpstreams();
    } catch (e) {
      upstreamCache = prev;
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }

  async function toggleRoute(id, enabled) {
    const prev = JSON.parse(JSON.stringify(upstreamCache));
    const r = upstreamCache.routes.find(x => x.id === id);
    if (r) r.enabled = enabled;
    try {
      await persistUpstreams();
      renderUpstreams();
    } catch (e) {
      upstreamCache = prev;
      toast((e && e.message) || t('common.saveFailed'), 'error');
      renderUpstreams();
    }
  }

  async function deleteRoute(id, model) {
    const ok = await confirmAction(t('upstreams.confirmDeleteRoute', model || ''), {
      title: t('upstreams.actionDelete'), confirmText: t('upstreams.actionDelete'), variant: 'danger'
    });
    if (!ok) return;
    const prev = JSON.parse(JSON.stringify(upstreamCache));
    upstreamCache.routes = upstreamCache.routes.filter(x => x.id !== id);
    try {
      await persistUpstreams();
      toast(t('upstreams.deleteSuccess'), 'success');
      await loadUpstreams();
    } catch (e) {
      upstreamCache = prev;
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  // ===== Forwarding config export / import =====
  // Export hits the dedicated endpoint rather than serializing upstreamCache:
  // the cache holds MASKED api keys (apiGetUpstreams masks them), which would
  // produce a file that imports but cannot authenticate.
  async function exportUpstreamsConfig() {
    const btn = $('upstreamExportBtn');
    if (btn) btn.disabled = true;
    try {
      const res = await api('/upstreams/export');
      if (!res.ok) throw new Error('http ' + res.status);
      const data = await res.json();
      downloadJson('kiro-forwarding-' + todayStamp() + '.json', data);
    } catch (e) {
      toastError(t('upstreams.exportFailed'));
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  function openUpstreamImportModal() {
    const txt = $('upstreamImportText');
    if (txt) txt.value = '';
    const file = $('upstreamImportFile');
    if (file) file.value = '';
    const results = $('upstreamImportResults');
    if (results) results.innerHTML = '';
    openDialog('upstreamImportModal');
  }

  function closeUpstreamImportModal() {
    closeDialog('upstreamImportModal');
  }

  async function submitUpstreamImport() {
    const txt = $('upstreamImportText');
    const raw = txt ? txt.value.trim() : '';
    if (!raw) { toastWarning(t('upstreams.importEmpty')); return; }
    // Parse client-side so a typo never reaches the network.
    let bundle;
    try {
      bundle = JSON.parse(raw);
    } catch (e) {
      toastError(t('upstreams.importInvalidJson'));
      return;
    }

    const btn = $('upstreamImportConfirmBtn');
    if (btn) btn.disabled = true;
    try {
      const res = await api('/upstreams/import', { method: 'POST', body: JSON.stringify(bundle) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) {
        // Leave the modal open with the pasted text intact so the user can fix it.
        toastError(t('upstreams.importFailed') + (d.error ? ': ' + d.error : ''));
        return;
      }
      renderUpstreamImportResult(d);
      const skipped = (d.providersSkipped || 0) + (d.routesSkipped || 0);
      toast(t('upstreams.importSummary',
        d.providersAdded || 0, d.providersSkipped || 0,
        d.routesAdded || 0, d.routesSkipped || 0),
        skipped ? 'warning' : 'success');
      await loadUpstreams();
      // Nothing was skipped, so there is nothing left to read: close. When
      // entries WERE skipped, stay open so the user can see which and why.
      if (!skipped) closeUpstreamImportModal();
    } catch (e) {
      toastError(t('upstreams.importFailed'));
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  // Renders import counts and skip lists. Every label comes from an untrusted
  // imported file and lands in innerHTML, so it MUST be escaped.
  function renderUpstreamImportResult(d) {
    const box = $('upstreamImportResults');
    if (!box) return;
    const reasonText = (reason) => {
      if (reason === 'duplicate') return t('upstreams.importReasonDuplicate');
      if (reason === 'unknownProvider') return t('upstreams.importReasonUnknownProvider');
      return reason || '';
    };
    const skipList = (titleKey, items) => {
      if (!items || !items.length) return '';
      return '<div class="mt-2"><strong class="text-xs">' + escapeHtml(t(titleKey)) + '</strong>' +
        items.map(it => '<div class="text-xs muted-text">· ' + escapeHtml(it.label || '') +
          ' <span class="warning-text">(' + escapeHtml(reasonText(it.reason)) + ')</span></div>').join('') +
        '</div>';
    };
    box.innerHTML = '<div class="text-sm">' +
      escapeHtml(t('upstreams.importSummary',
        d.providersAdded || 0, d.providersSkipped || 0,
        d.routesAdded || 0, d.routesSkipped || 0)) +
      '</div>' +
      skipList('upstreams.importSkippedProviders', d.skippedProviders) +
      skipList('upstreams.importSkippedRoutes', d.skippedRoutes);
  }

  function bindUpstreamEvents() {
    const provList = $('upstreamsList');
    if (provList) {
      provList.addEventListener('click', e => {
        const btn = e.target.closest('[data-upstream-action]');
        if (!btn) return;
        const action = btn.dataset.upstreamAction;
        const id = btn.dataset.id;
        if (!id) return;
        const entry = upstreamCache.providers.find(x => x.id === id);
        if (action === 'edit') openUpstreamModal(entry);
        else if (action === 'delete') deleteProvider(id, entry ? entry.name : '');
        else if (action === 'load') loadProviderModels(id);
        else if (action === 'details') toggleProviderDetail(id, btn, provList);
        else if (action === 'hide') setProviderHidden(id, !(entry && entry.hidden));
        else if (action === 'add-key') openConnModal(id, null);
        else if (action === 'bulk') openBulkModal(id);
      });
      // The hidden-group disclosure. Bound here rather than on the button itself
      // because the button is re-rendered on every list change.
      provList.addEventListener('click', e => {
        if (!e.target.closest('[data-upstream-toggle-hidden]')) return;
        showHiddenProviders = !showHiddenProviders;
        localStorage.setItem('kiro_show_hidden_providers', showHiddenProviders ? '1' : '0');
        renderProviders();
        // Newly rendered rows start with an empty stat line; fill it from the
        // stats already in memory instead of waiting for the next poll.
        renderProviderInlineStats();
      });
      // Actions rendered inside an expanded detail panel.
      provList.addEventListener('click', e => {
        const btn = e.target.closest('[data-pd-action]');
        if (!btn) return;
        const id = btn.dataset.id;
        if (!id) return;
        const action = btn.dataset.pdAction;
        if (action === 'reset') resetProviderStats(id);
        else if (action === 'export') exportProviderEvents(id);
        else if (action === 'events') filterEventsByProvider(id);
      });
      provList.addEventListener('change', e => {
        const cb = e.target.closest('input[data-upstream-action="toggle"]');
        if (cb) {
          const id = cb.dataset.id;
          if (id) toggleProvider(id, cb.checked);
          return;
        }
        const connToggle = e.target.closest('input[data-conn-action="toggle"]');
        if (connToggle) {
          toggleConnection(connToggle.dataset.pid, connToggle.dataset.cid, connToggle.checked);
          return;
        }
        const rr = e.target.closest('input[data-conn-action="rr"]');
        if (rr) {
          setRoundRobin(rr.dataset.pid, rr.checked);
          return;
        }
        const sel = e.target.closest('select[data-conn-action="test-model"]');
        if (sel) {
          connTestState(sel.dataset.pid).model = sel.value;
          return;
        }
        const pick = e.target.closest('input[data-conn-select]');
        if (pick) {
          const st = connTestState(pick.dataset.pid);
          if (!st.selected) st.selected = {};
          providerConnections(upstreamCache.providers.find(x => x.id === pick.dataset.pid)).forEach(c => {
            if (st.selected[c.id] === undefined) st.selected[c.id] = true;
          });
          st.selected[pick.dataset.connSelect] = pick.checked;
        }
      });
      provList.addEventListener('click', e => {
        const bulkCol = e.target.closest('[data-bulk-col]');
        if (bulkCol) return;
        const btn = e.target.closest('[data-conn-action]');
        if (!btn) return;
        const action = btn.dataset.connAction;
        const pid = btn.dataset.pid;
        if (!pid) return;
        const p = upstreamCache.providers.find(x => x.id === pid);
        const cid = btn.dataset.cid;
        const conn = p ? providerConnections(p).find(c => c.id === cid) : null;
        if (action === 'edit') openConnModal(pid, conn);
        else if (action === 'delete') deleteConnection(pid, cid, conn ? conn.name : '');
        else if (action === 'test-all') testConnectionsOneByOne(pid);
        else if (action === 'stop') stopConnectionTests(pid);
        else if (action === 'select-all') {
          const st = connTestState(pid);
          st.selected = {};
          providerConnections(p).forEach(c => { st.selected[c.id] = true; });
          renderProviders();
        }
      });
    }
    // Per-model actions (copy / test / make route) live inside the models modal.
    const modelsList = $('upstreamModelsList');
    if (modelsList) {
      modelsList.addEventListener('click', e => {
        const mbtn = e.target.closest('[data-model-action]');
        if (!mbtn) return;
        const action = mbtn.dataset.modelAction;
        const model = mbtn.dataset.model || '';
        const pid = mbtn.dataset.pid || '';
        if (action === 'copy') copyText(model).then(() => toast(t('upstreams.copied'), 'success'));
        else if (action === 'test') testModel(pid, model);
        else if (action === 'route') makeRouteFromModel(pid, model);
      });
    }
    const modelsSearch = $('upstreamModelsSearch');
    if (modelsSearch) {
      modelsSearch.addEventListener('input', () => {
        modelsModalSearch = modelsSearch.value;
        renderModelsModal();
      });
    }
    const modelsClose = $('upstreamModelsModalClose');
    if (modelsClose) modelsClose.addEventListener('click', closeModelsModal);
    const modelsCloseBtn = $('upstreamModelsModalCloseBtn');
    if (modelsCloseBtn) modelsCloseBtn.addEventListener('click', closeModelsModal);
    const routeList = $('modelRoutesList');
    if (routeList) {
      routeList.addEventListener('click', e => {
        const btn = e.target.closest('[data-route-action]');
        if (!btn) return;
        const action = btn.dataset.routeAction;
        const id = btn.dataset.id;
        if (!id) return;
        const entry = upstreamCache.routes.find(x => x.id === id);
        if (action === 'edit') openRouteModal(entry);
        else if (action === 'delete') deleteRoute(id, entry ? entry.model : '');
      });
      routeList.addEventListener('change', e => {
        const cb = e.target.closest('input[data-route-action="toggle"]');
        if (!cb) return;
        const id = cb.dataset.id;
        if (id) toggleRoute(id, cb.checked);
      });
    }
    const addProvBtn = $('addUpstreamBtn');
    if (addProvBtn) addProvBtn.addEventListener('click', () => openUpstreamModal(null));
    const addRouteBtn = $('addRouteBtn');
    // No provider gate: the Kiro pool is always a valid target, so a route is
    // configurable with zero upstreams configured ("send this model to the pool"
    // is a legitimate route on its own).
    if (addRouteBtn) addRouteBtn.addEventListener('click', () => openRouteModal(null));
    const upSave = $('upstreamModalSaveBtn');
    if (upSave) upSave.addEventListener('click', submitUpstreamModal);
    const upCancel = $('upstreamModalCancelBtn');
    if (upCancel) upCancel.addEventListener('click', closeUpstreamModal);
    const upClose = $('upstreamModalClose');
    if (upClose) upClose.addEventListener('click', closeUpstreamModal);
    const rtSave = $('modelRouteModalSaveBtn');
    if (rtSave) rtSave.addEventListener('click', submitRouteModal);
    const rtCancel = $('modelRouteModalCancelBtn');
    if (rtCancel) rtCancel.addEventListener('click', closeRouteModal);
    const rtClose = $('modelRouteModalClose');
    if (rtClose) rtClose.addEventListener('click', closeRouteModal);
    // Target rows are re-rendered on every structural change, so both listeners
    // are delegated from the stable container rather than bound per row.
    // The Client Model is the probe model for every row that does not rewrite it, so
    // editing it re-aims those rows: re-render to re-key their verdict slots (which
    // drops results that were true for the old name) and to flip their Test button
    // between enabled and "nothing to probe". The caret is in a field outside the
    // list, so rebuilding the rows cannot steal it.
    const routeModelInput = $('routeForm_model');
    if (routeModelInput) {
      routeModelInput.addEventListener('input', () => {
        if (isDialogOpen('modelRouteModal') && routeTargetDraft.length) renderRouteTargets();
      });
    }
    const rtTargets = $('routeTargetsList');
    if (rtTargets) {
      rtTargets.addEventListener('click', e => {
        const btn = e.target.closest('[data-target-action]');
        if (!btn || btn.disabled) return;
        const i = parseInt(btn.dataset.index, 10);
        if (isNaN(i) || !routeTargetDraft[i]) return;
        const action = btn.dataset.targetAction;
        if (action === 'remove') {
          if (routeTargetDraft.length <= 1) return;
          routeTargetDraft.splice(i, 1);
        } else if (action === 'up') {
          if (!moveRenderedRow(i, -1)) return;
        } else if (action === 'down') {
          if (!moveRenderedRow(i, 1)) return;
        } else if (action === 'test') {
          // No re-render: testRouteTarget paints its own slot, and rebuilding the list
          // here would drop the caret out of whatever field is being edited.
          testRouteTarget(i);
          return;
        } else if (action === 'hide') {
          // Draft-only, like every other field in this modal: Cancel discards it,
          // Save persists it with the route. Writing through immediately — the way
          // the provider list's Hide does — would also commit whatever half-finished
          // target edits are sitting next to it.
          routeTargetDraft[i].hidden = !routeTargetDraft[i].hidden;
        } else {
          return;
        }
        normalizeDraftTiers();
        renderRouteTargets();
      });
      // The hidden-run disclosure. Delegated from the stable container because the
      // markers are re-rendered on every list change, and it flips ALL runs at once:
      // one preference, mirroring the provider list's single "show hidden" toggle,
      // instead of per-run state the operator has to keep track of.
      rtTargets.addEventListener('click', e => {
        if (!e.target.closest('[data-target-toggle-hidden]')) return;
        showHiddenRouteTargets = !showHiddenRouteTargets;
        localStorage.setItem('kiro_show_hidden_route_targets', showHiddenRouteTargets ? '1' : '0');
        renderRouteTargets();
      });
      // Field edits write straight into the draft. Text inputs use 'input' so a
      // half-typed model name is not lost when the row re-renders for another
      // reason; selects and checkboxes only emit 'change'.
      const applyField = e => {
        const el = e.target.closest('[data-target-field]');
        if (!el) return;
        const i = parseInt(el.dataset.index, 10);
        if (isNaN(i) || !routeTargetDraft[i]) return;
        const field = el.dataset.targetField;
        if (field === 'upstreamProvider') {
          // The provider field is a searchable text combobox. Resolution runs on
          // 'change' ONLY (option picked, Enter, or blur) — never on 'input':
          // re-rendering mid-keystroke would destroy the field, close the
          // datalist and break both typing-to-filter and clicking a suggestion.
          // Letting 'input' fall through untouched lets the native datalist do
          // the live filtering.
          if (e.type !== 'change') return;
          const id = routeProviderIdFromLabel(el.value);
          if (!id) {
            // Partial / unknown label committed: snap back to the current provider
            // so the field never lingers in an invalid state.
            renderRouteTargets();
            return;
          }
          if (id === routeTargetDraft[i].upstreamId) {
            // Same provider re-selected: snap the text to the canonical label.
            renderRouteTargets();
            return;
          }
          routeTargetDraft[i].upstreamId = id;
          // Provider changed: the row's model dropdown, weight state, pool shape
          // and reachability of rows below can all change, so re-render the list.
          normalizeDraftTiers();
          renderRouteTargets();
          return;
        }
        if (field === 'enabled') routeTargetDraft[i].enabled = el.checked;
        else if (field === 'weight') routeTargetDraft[i].weight = Math.max(1, parseInt(el.value, 10) || 1);
        else if (field === 'sameTier') {
          // Both checkbox fields must read .checked, not .value: the generic
          // branch below would store the string "on" and never clear the flag.
          routeTargetDraft[i].sameTier = el.checked;
          // Tier membership decides the badges and whether weight is editable, so
          // the whole list has to re-render — unlike the other fields, which only
          // affect the row being typed into.
          renderRouteTargets();
          return;
        }
        else {
          routeTargetDraft[i][field] = el.value;
          // A verdict belongs to the pair that was probed. Once the model text moves,
          // the row aims somewhere else, so drop the stale ✓/✗ instead of letting it
          // vouch for a target nobody tested. (The map keeps it under the old key, so
          // undoing the edit brings the result back.)
          if (field === 'targetModel') {
            const row = el.closest('.route-target-row');
            const slot = row && row.querySelector('[data-target-test-for]');
            if (slot) { slot.textContent = ''; slot.title = ''; slot.style.color = ''; }
          }
        }
      };
      rtTargets.addEventListener('change', applyField);
      rtTargets.addEventListener('input', applyField);

      // Drag to reorder. The row carries draggable=false in the markup and is
      // only armed while the pointer is held on the grip: HTML5 drag on a
      // container would otherwise swallow text selection and caret placement in
      // the target-model input.
      let dragFrom = -1;
      rtTargets.addEventListener('mousedown', e => {
        const row = e.target.closest('.route-target-row');
        if (!row) return;
        row.draggable = !!e.target.closest('[data-target-handle]');
      });
      // Disarm on release so a later drag attempt from an input cannot inherit
      // the armed state left by an earlier grip press.
      rtTargets.addEventListener('mouseup', () => {
        qsa('.route-target-row', rtTargets).forEach(r => { r.draggable = false; });
      });
      rtTargets.addEventListener('dragstart', e => {
        const row = e.target.closest('.route-target-row');
        if (!row || !row.draggable) return;
        dragFrom = parseInt(row.dataset.targetIndex, 10);
        row.classList.add('is-dragging');
        if (e.dataTransfer) {
          e.dataTransfer.effectAllowed = 'move';
          // Firefox refuses to start a drag unless some payload is set.
          e.dataTransfer.setData('text/plain', String(dragFrom));
        }
      });
      // dragover fires continuously; the marker class is recomputed from the
      // pointer's position relative to each row's midpoint, which is what makes
      // the insertion point follow the cursor.
      rtTargets.addEventListener('dragover', e => {
        if (dragFrom < 0) return;
        e.preventDefault();
        if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
        const row = e.target.closest('.route-target-row');
        qsa('.route-target-row', rtTargets).forEach(r => r.classList.remove('drop-above', 'drop-below'));
        if (!row) return;
        const rect = row.getBoundingClientRect();
        row.classList.add(e.clientY < rect.top + rect.height / 2 ? 'drop-above' : 'drop-below');
      });
      rtTargets.addEventListener('drop', e => {
        if (dragFrom < 0) return;
        e.preventDefault();
        const row = e.target.closest('.route-target-row');
        const from = dragFrom;
        dragFrom = -1;
        if (!row) { renderRouteTargets(); return; }
        const to = parseInt(row.dataset.targetIndex, 10);
        const rect = row.getBoundingClientRect();
        // Convert "which row, which half" into a gap index so dropping below the
        // last row appends rather than landing on it.
        const insertAt = e.clientY < rect.top + rect.height / 2 ? to : to + 1;
        moveDraftRow(from, insertAt);
        renderRouteTargets();
      });
      // dragend also covers the cancelled drag (Esc, or a drop outside the
      // list), where no drop event ever arrives.
      rtTargets.addEventListener('dragend', () => {
        dragFrom = -1;
        qsa('.route-target-row', rtTargets).forEach(r => {
          r.draggable = false;
          r.classList.remove('is-dragging', 'drop-above', 'drop-below');
        });
      });
    }
    const rtAddTarget = $('routeAddTargetBtn');
    if (rtAddTarget) rtAddTarget.addEventListener('click', () => {
      // See addRouteBtn: newRouteTarget falls back to the pool sentinel, so a row
      // can be added without any provider configured.
      routeTargetDraft.push(newRouteTarget());
      renderRouteTargets();
    });
    const upExport = $('upstreamExportBtn');
    if (upExport) upExport.addEventListener('click', exportUpstreamsConfig);
    const upImport = $('upstreamImportBtn');
    if (upImport) upImport.addEventListener('click', openUpstreamImportModal);
    const upImpConfirm = $('upstreamImportConfirmBtn');
    if (upImpConfirm) upImpConfirm.addEventListener('click', submitUpstreamImport);
    const upImpCancel = $('upstreamImportCancelBtn');
    if (upImpCancel) upImpCancel.addEventListener('click', closeUpstreamImportModal);
    const upImpClose = $('upstreamImportModalClose');
    if (upImpClose) upImpClose.addEventListener('click', closeUpstreamImportModal);
    const upImpFile = $('upstreamImportFile');
    if (upImpFile) upImpFile.addEventListener('change', e => {
      const f = e.target.files && e.target.files[0];
      if (!f) return;
      f.text().then(txt => {
        const box = $('upstreamImportText');
        if (box) box.value = txt;
      }).catch(() => toastError(t('upstreams.importFailed')));
    });
    bindDialogBackdropClose('upstreamModal', closeUpstreamModal);
    bindDialogBackdropClose('modelRouteModal', closeRouteModal);
    bindDialogBackdropClose('upstreamModelsModal', closeModelsModal);
    bindDialogBackdropClose('upstreamImportModal', closeUpstreamImportModal);
    bindDialogBackdropClose('upstreamConnModal', closeConnModal);
    bindDialogBackdropClose('upstreamBulkModal', closeBulkModal);
    const connSave = $('upstreamConnModalSaveBtn');
    if (connSave) connSave.addEventListener('click', submitConnModal);
    const connCancel = $('upstreamConnModalCancelBtn');
    if (connCancel) connCancel.addEventListener('click', closeConnModal);
    const connClose = $('upstreamConnModalClose');
    if (connClose) connClose.addEventListener('click', closeConnModal);
    const bulkAnalyze = $('upstreamBulkAnalyzeBtn');
    if (bulkAnalyze) bulkAnalyze.addEventListener('click', analyzeBulkImport);
    const bulkImport = $('upstreamBulkImportBtn');
    if (bulkImport) bulkImport.addEventListener('click', commitBulkImport);
    const bulkCancel = $('upstreamBulkCancelBtn');
    if (bulkCancel) bulkCancel.addEventListener('click', closeBulkModal);
    const bulkClose = $('upstreamBulkModalClose');
    if (bulkClose) bulkClose.addEventListener('click', closeBulkModal);
    const bulkPrev = $('upstreamBulkPreview');
    if (bulkPrev) {
      bulkPrev.addEventListener('click', e => {
        const btn = e.target.closest('[data-bulk-col]');
        if (!btn) return;
        bulkResolutions[btn.dataset.bulkLine] = parseInt(btn.dataset.bulkCol, 10);
        analyzeBulkImport();
      });
    }
  }

  // ===== Forwarding dashboard =====
  let forwardStats = { overall: {}, providers: [], routes: [] };
  // Range-scoped copy of providers for the Stats tab. forwardStats stays
  // all-time because the Forwarding tab's inline stats and provider filter
  // depend on it; null means "no window loaded yet, fall back to all-time".
  let statsWindowProviders = null;
  let fwdEventsOffset = 0;
  const fwdEventsLimit = 50;
  let fwdEventsTotal = 0;
  let fwdSource = null;
  const fwdProviderNames = {}; // providerId -> name, for labeling events

  function fwdFmtLatency(ms) {
    if (ms == null) return '-';
    if (ms >= 1000) return (ms / 1000).toFixed(1) + 's';
    return ms + ' ms';
  }

  function fwdFmtTime(ms) {
    if (!ms) return '';
    const d = new Date(ms);
    return d.toLocaleTimeString();
  }

  // Aggregate stats: stat cards, chart, percentiles, and per-provider/route
  // counters used by the inline card stats in Settings.
  async function loadForwardStats() {
    try {
      const res = await api('/forward-stats');
      if (!res.ok) throw new Error('http ' + res.status);
      const d = await res.json();
      forwardStats = {
        overall: d.overall || {},
        providers: Array.isArray(d.providers) ? d.providers : [],
        routes: Array.isArray(d.routes) ? d.routes : []
      };
      forwardStats.providers.forEach(p => { fwdProviderNames[p.providerId] = p.providerName || p.providerId; });
      renderForwardStatCards(d);
      renderForwardChart(d.timeseries || [], d.percentiles || {});
      populateForwardProviderFilter();
      renderProviderInlineStats();
      refreshOpenProviderDetails();
    } catch (e) {
      // Non-fatal: dashboard just shows zeros.
    }
  }

  function renderForwardStatCards(d) {
    const o = d.overall || {};
    if ($('fwdStatRequests')) $('fwdStatRequests').textContent = o.requests || 0;
    if ($('fwdStatSuccess')) $('fwdStatSuccess').textContent = o.success || 0;
    if ($('fwdStatFailed')) $('fwdStatFailed').textContent = o.failed || 0;
    if ($('fwdStatLatency')) $('fwdStatLatency').textContent = fwdFmtLatency(o.avgLatencyMs || 0);
  }

  // Hand-rolled SVG bar chart (zero external deps): one bar per minute, success
  // stacked below failed, height scaled to the busiest minute.
  function renderForwardChart(series, pct) {
    const host = $('fwdChart');
    if (host) {
      if (!series.length) {
        host.innerHTML = '<div class="muted-text text-xs" style="padding:1rem 0;">' + escapeHtml(t('forward.noData')) + '</div>';
      } else {
        const w = 100, h = 40, n = series.length;
        const bw = w / n;
        const max = Math.max(1, ...series.map(b => b.requests || 0));
        let bars = '';
        series.forEach((b, i) => {
          const x = (i * bw).toFixed(2);
          const total = b.requests || 0;
          if (!total) return;
          const th = (total / max) * h;
          const fh = ((b.failed || 0) / max) * h;
          const sh = th - fh;
          const bwFill = (bw * 0.8).toFixed(2);
          if (sh > 0) bars += '<rect x="' + x + '" y="' + (h - th).toFixed(2) + '" width="' + bwFill + '" height="' + sh.toFixed(2) + '" fill="#22c55e"></rect>';
          if (fh > 0) bars += '<rect x="' + x + '" y="' + (h - fh).toFixed(2) + '" width="' + bwFill + '" height="' + fh.toFixed(2) + '" fill="#ef4444"></rect>';
        });
        host.innerHTML = '<svg viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none" style="width:100%;height:60px;">' + bars + '</svg>';
      }
    }
    const pctEl = $('fwdPercentiles');
    if (pctEl) {
      if (pct && pct.count) {
        pctEl.textContent = 'p50 ' + fwdFmtLatency(pct.p50) + ' · p95 ' + fwdFmtLatency(pct.p95) + ' · p99 ' + fwdFmtLatency(pct.p99);
      } else {
        pctEl.textContent = '';
      }
    }
  }

  function populateForwardProviderFilter() {
    const sel = $('fwdFilterProvider');
    if (!sel) return;
    const cur = sel.value;
    const opts = ['<option value="">' + escapeHtml(t('forward.allProviders')) + '</option>'];
    forwardStats.providers.forEach(p => {
      opts.push('<option value="' + escapeAttr(p.providerId) + '">' + escapeHtml(p.providerName || p.providerId) + '</option>');
    });
    sel.innerHTML = opts.join('');
    sel.value = cur;
  }

  // Inline mini-stat line on each provider card.
  function renderProviderInlineStats() {
    const byId = {};
    forwardStats.providers.forEach(p => { byId[p.providerId] = p; });
    qsa('#upstreamsList [data-fwd-inline-for]').forEach(el => {
      const p = byId[el.dataset.fwdInlineFor];
      if (!p || !p.requests) { el.textContent = ''; return; }
      let line = t('forward.inlineStat', String(p.requests), String(p.success), String(p.failed), fwdFmtLatency(p.avgLatencyMs));
      const tokens = (p.inputTokens || 0) + (p.outputTokens || 0);
      if (tokens) line += ' · ' + t('stats.inlineTokens', formatNum(tokens));
      if (p.rpm) line += ' · ' + t('stats.inlineRpm', fwdFmtRate(p.rpm));
      el.textContent = line;
    });
  }

  // ===== Per-provider detail panel =====
  // Shared by the expandable cards on the Forwarding tab and the drill-down on
  // the Stats tab, so both surfaces always show the same figures.

  const providerDetailCache = {}; // providerId -> last loaded detail payload
  const openProviderDetails = {}; // providerId -> true while expanded

  function fwdFmtRate(n) {
    if (!n) return '0';
    return n >= 10 ? Math.round(n).toString() : n.toFixed(1);
  }

  // fwdFmtPct renders a success rate. The API sends -1 for "no traffic yet",
  // which must read as unknown rather than 0%.
  function fwdFmtPct(v) {
    if (v == null || v < 0) return '—';
    return v.toFixed(1) + '%';
  }

  function fwdFmtCost(v, isPool) {
    if (!v) return '—';
    const n = v >= 1 ? v.toFixed(2) : v.toFixed(4);
    return isPool ? t('stats.creditsValue', n) : '$' + n;
  }

  function fwdFmtWhen(ms) {
    if (!ms) return '—';
    return new Date(ms).toLocaleString();
  }

  // statusClass buckets an HTTP status into the badge colors already used by the
  // event table.
  function statusClass(code) {
    if (code >= 200 && code < 300) return 'fwd-badge--ok';
    if (code === 499) return 'fwd-badge--warn';
    return 'fwd-badge--err';
  }

  // sparkline draws a compact SVG bar chart of per-minute buckets, matching the
  // main forwarding chart's stacked success/failure encoding.
  function sparkline(buckets) {
    if (!buckets || !buckets.length) return '';
    const w = 100, h = 24, n = buckets.length, bw = w / n;
    const max = Math.max(1, ...buckets.map(b => b.requests || 0));
    let bars = '';
    buckets.forEach((b, i) => {
      const total = b.requests || 0;
      if (!total) return;
      const th = (total / max) * h;
      const fh = ((b.failed || 0) / max) * h;
      const sh = th - fh;
      const x = (i * bw).toFixed(2);
      const fw = (bw * 0.8).toFixed(2);
      if (sh > 0) bars += '<rect x="' + x + '" y="' + (h - th).toFixed(2) + '" width="' + fw + '" height="' + sh.toFixed(2) + '" class="spark-ok"></rect>';
      if (fh > 0) bars += '<rect x="' + x + '" y="' + (h - fh).toFixed(2) + '" width="' + fw + '" height="' + fh.toFixed(2) + '" class="spark-err"></rect>';
    });
    return '<svg class="provider-spark" viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none">' + bars + '</svg>';
  }

  function metricTile(label, value, sub) {
    return '<div class="pd-tile">' +
      '<div class="pd-tile-label">' + escapeHtml(label) + '</div>' +
      '<div class="pd-tile-value">' + escapeHtml(value) + '</div>' +
      (sub ? '<div class="pd-tile-sub">' + escapeHtml(sub) + '</div>' : '') +
      '</div>';
  }

  function pdSection(title, inner) {
    if (!inner) return '';
    return '<div class="pd-section">' +
      '<div class="pd-section-title">' + escapeHtml(title) + '</div>' + inner + '</div>';
  }

  // rangeLabel maps a windowHours value to the same text the Stats tab selector
  // uses, so the badge reads "Last hour" rather than "1 hour window". Unknown
  // values (a hand-edited URL) degrade to a bare hour count instead of blank.
  function rangeLabel(hours) {
    if (!hours || hours <= 0) return '';
    const map = {
      1: t('stats.range1h'), 6: t('stats.range6h'), 24: t('stats.range24h'),
      168: t('stats.range7d'), 720: t('stats.range30d')
    };
    return map[hours] || (hours + 'h');
  }

  // renderProviderDetailHTML builds the whole panel from a /provider-stats
  // payload. isPool switches the cost column to credits, which is the pool's
  // real unit of spend.
  function renderProviderDetailHTML(payload) {
    const d = payload.detail || {};
    const isPool = !!payload.isPool;
    const pct = d.percentiles || {};

    const tiles =
      metricTile(t('stats.tileRequests'), formatNum(d.requests || 0),
        // Cancellations are counted in neither ok nor failed, so show them
        // explicitly — otherwise the two numbers silently fail to add up.
        d.canceled
          ? t('stats.tileOkFailCanceled', String(d.success || 0), String(d.failed || 0), String(d.canceled))
          : t('stats.tileOkFail', String(d.success || 0), String(d.failed || 0))) +
      metricTile(t('stats.tileSuccessRate'), fwdFmtPct(d.successRate),
        d.failStreak ? t('stats.tileStreak', String(d.failStreak)) : '') +
      metricTile(t('stats.tileTokens'), formatNum((d.inputTokens || 0) + (d.outputTokens || 0)),
        t('stats.tileTokenSplit', formatNum(d.inputTokens || 0), formatNum(d.outputTokens || 0))) +
      metricTile(t('stats.tileCost'), fwdFmtCost(d.costUsd, isPool), '') +
      metricTile(t('stats.tileRpm'), fwdFmtRate(d.rpm),
        t('stats.tileTpm', fwdFmtRate(d.tpm))) +
      metricTile(t('stats.tileLatency'), fwdFmtLatency(d.avgLatencyMs || 0),
        pct.count ? 'p50 ' + fwdFmtLatency(pct.p50) + ' · p95 ' + fwdFmtLatency(pct.p95) + ' · p99 ' + fwdFmtLatency(pct.p99) : '') +
      metricTile(t('stats.tileTtfb'), d.avgTtfbMs ? fwdFmtLatency(d.avgTtfbMs) : '—',
        d.tokensPerSec ? t('stats.tileTokensPerSec', fwdFmtRate(d.tokensPerSec)) : '') +
      metricTile(t('stats.tileConcurrency'), String(d.inFlight || 0),
        t('stats.tilePeak', String(d.peakInFlight || 0))) +
      metricTile(t('stats.tileStream'), formatNum(d.streamed || 0),
        t('stats.tileCanceled', String(d.canceled || 0))) +
      metricTile(t('stats.tileLastOk'), fwdFmtWhen(d.lastOk), '');

    // When headline numbers are scoped to a time window, show a badge so the
    // numbers in the tiles cannot be mistaken for all-time figures.
    const wh = d.windowHours || 0;
    const wLabel = rangeLabel(wh);
    const windowBadge = wLabel
      ? '<div class="pd-range-badge"><i class="fa-regular fa-clock" aria-hidden="true"></i>' +
          escapeHtml(wLabel) +
          '<span class="pd-range-alltime-note">' +
          escapeHtml(t('stats.detailAllTimeNote')) + '</span></div>'
      : '';

    let html = windowBadge + '<div class="pd-tiles">' + tiles + '</div>';

    const spark = sparkline(d.minutes);
    if (spark) html += pdSection(t('stats.sectionActivity'), spark);

    if (d.statuses && d.statuses.length) {
      const sTitle = wh ? t('stats.sectionAllTime', t('stats.sectionStatus')) : t('stats.sectionStatus');
      html += pdSection(sTitle,
        '<div class="pd-badges">' + d.statuses.map(s =>
          '<span class="fwd-badge ' + statusClass(s.status) + '">' +
          escapeHtml(String(s.status)) + ' × ' + escapeHtml(formatNum(s.count)) +
          '</span>').join('') + '</div>');
    }

    if (d.models && d.models.length) {
      const mTitle = wh ? t('stats.sectionAllTime', t('stats.sectionModels')) : t('stats.sectionModels');
      html += pdSection(mTitle,
        '<div class="pd-table-scroll"><table class="pd-table"><thead><tr>' +
        '<th>' + escapeHtml(t('stats.colModel')) + '</th>' +
        '<th>' + escapeHtml(t('stats.colRequests')) + '</th>' +
        '<th>' + escapeHtml(t('stats.colOkPct')) + '</th>' +
        '<th>' + escapeHtml(t('stats.colTokens')) + '</th>' +
        '<th>' + escapeHtml(t('stats.colAvg')) + '</th>' +
        '</tr></thead><tbody>' +
        d.models.map(m => '<tr>' +
          '<td class="font-mono">' + escapeHtml(m.model) + '</td>' +
          '<td>' + escapeHtml(formatNum(m.requests)) + '</td>' +
          '<td>' + escapeHtml(fwdFmtPct(m.successRate)) + '</td>' +
          '<td>' + escapeHtml(formatNum((m.inputTokens || 0) + (m.outputTokens || 0))) + '</td>' +
          '<td>' + escapeHtml(fwdFmtLatency(m.avgLatencyMs)) + '</td>' +
          '</tr>').join('') +
        '</tbody></table></div>');
    }

    if (d.accounts && d.accounts.length) {
      const aTitle = wh ? t('stats.sectionAllTime', t('stats.sectionAccounts')) : t('stats.sectionAccounts');
      html += pdSection(aTitle,
        '<div class="pd-table-scroll"><table class="pd-table"><thead><tr>' +
        '<th>' + escapeHtml(t('stats.colAccount')) + '</th>' +
        '<th>' + escapeHtml(t('stats.colRequests')) + '</th>' +
        '<th>' + escapeHtml(t('stats.colOkPct')) + '</th>' +
        '<th>' + escapeHtml(t('stats.colTokens')) + '</th>' +
        '<th>' + escapeHtml(t('stats.colAvg')) + '</th>' +
        '</tr></thead><tbody>' +
        d.accounts.map(a => {
          const label = a.accountLabel || a.accountId || '';
          return '<tr>' +
            '<td>' + escapeHtml(privacyModeEnabled ? maskEmail(label) : label) + '</td>' +
            '<td>' + escapeHtml(formatNum(a.requests)) + '</td>' +
            '<td>' + escapeHtml(fwdFmtPct(a.successRate)) + '</td>' +
            '<td>' + escapeHtml(formatNum((a.inputTokens || 0) + (a.outputTokens || 0))) + '</td>' +
            '<td>' + escapeHtml(fwdFmtLatency(a.avgLatencyMs)) + '</td>' +
            '</tr>';
        }).join('') +
        '</tbody></table></div>');
    }

    if (d.recentErrors && d.recentErrors.length) {
      html += pdSection(t('stats.sectionErrors'),
        '<div class="pd-errors">' + d.recentErrors.map(e =>
          '<div class="pd-error">' +
          '<span class="fwd-badge ' + statusClass(e.status) + '">' + escapeHtml(String(e.status || '-')) + '</span>' +
          '<span class="pd-error-time">' + escapeHtml(fwdFmtWhen(e.time)) + '</span>' +
          '<span class="pd-error-msg" title="' + escapeAttr(e.message || '') + '">' + escapeHtml(e.message || '') + '</span>' +
          '</div>').join('') + '</div>');
    }

    // Per-provider actions: a scoped event log and a scoped reset, so an
    // operator can investigate or clear one provider without touching the rest.
    const pid = escapeAttr(payload.providerId || (d.providerId || ''));
    html += '<div class="pd-actions">' +
      '<button class="btn btn-outline btn-sm" type="button" data-pd-action="events" data-id="' + pid + '">' +
      escapeHtml(t('stats.viewEvents')) + '</button>' +
      '<button class="btn btn-outline btn-sm" type="button" data-pd-action="export" data-id="' + pid + '">' +
      escapeHtml(t('stats.exportCsv')) + '</button>' +
      '<button class="btn btn-danger btn-sm" type="button" data-pd-action="reset" data-id="' + pid + '">' +
      escapeHtml(t('stats.resetProvider')) + '</button>' +
      '</div>';

    return html;
  }

  // loadProviderDetail fetches and renders one provider's panel into every host
  // element currently showing it (a card on Forwarding, a row on Stats).
  //
  // hours controls the headline-number scope: pass statsHistoryHours from the
  // Stats tab so the detail panel shows the same range as its row; pass 0 (or
  // omit) for the Forwarding tab, where there is no range selector and KPI
  // cards are intentionally all-time.
  async function loadProviderDetail(providerId, hours) {
    const hosts = qsa('[data-provider-detail="' + cssEscape(providerId) + '"]');
    if (!hosts.length) return;
    try {
      let url = '/provider-stats?id=' + encodeURIComponent(providerId);
      if (hours && hours > 0) url += '&hours=' + encodeURIComponent(hours);
      const res = await api(url);
      if (!res.ok) throw new Error('http ' + res.status);
      const payload = await res.json();
      payload.providerId = providerId;
      providerDetailCache[providerId] = payload;
      const html = renderProviderDetailHTML(payload);
      hosts.forEach(el => { el.innerHTML = html; });
    } catch (e) {
      hosts.forEach(el => {
        el.innerHTML = '<div class="muted-text text-xs" style="padding:0.75rem 0;">' +
          escapeHtml(t('stats.loadFailed')) + '</div>';
      });
    }
  }

  // cssEscape quotes an id for use inside an attribute selector. Provider ids are
  // UUIDs today, but a hand-imported bundle can carry anything.
  function cssEscape(s) {
    if (window.CSS && CSS.escape) return CSS.escape(s);
    return String(s).replace(/["\\]/g, '\\$&');
  }

  // toggleProviderDetail expands or collapses one provider's panel.
  //
  // scope matters: the same providerId has a detail host on BOTH the Forwarding
  // tab and the Stats table, so the host must be resolved within the container
  // the click came from. Resolving globally would toggle whichever surface
  // happens to appear first in the document.
  function toggleProviderDetail(providerId, btn, scope) {
    const sel = '[data-provider-detail="' + cssEscape(providerId) + '"]';
    const root = scope || document;
    const host = root.querySelector(sel);
    if (!host) return;
    const open = !host.classList.contains('hidden');
    if (open) {
      host.classList.add('hidden');
      delete openProviderDetails[providerId];
      if (btn) btn.setAttribute('aria-expanded', 'false');
      return;
    }
    host.classList.remove('hidden');
    openProviderDetails[providerId] = true;
    if (btn) btn.setAttribute('aria-expanded', 'true');
    if (!host.innerHTML) {
      host.innerHTML = '<div class="muted-text text-xs" style="padding:0.75rem 0;">' +
        escapeHtml(t('stats.loading')) + '</div>';
    }
    // Determine the range: panels opened from inside the Stats table carry the
    // current window; panels on the Forwarding tab have no range selector and
    // should always show all-time so the KPI cards there stay consistent.
    const hoursForDetail = scope && scope.id === 'statsTableBody' ? statsHistoryHours : 0;
    loadProviderDetail(providerId, hoursForDetail);
  }

  // refreshOpenProviderDetails re-fetches every expanded panel, so an open panel
  // tracks live traffic instead of freezing at its opening snapshot.
  function refreshOpenProviderDetails() {
    Object.keys(openProviderDetails).forEach(id => {
      const host = document.querySelector('[data-provider-detail="' + cssEscape(id) + '"]');
      if (!host) return;
      // Re-use the same scope heuristic as toggleProviderDetail: if the host
      // lives inside statsTableBody the panel respects the range selector.
      const inStats = !!host.closest('#statsTableBody');
      loadProviderDetail(id, inStats ? statsHistoryHours : 0);
    });
  }

  // resetProviderStats clears one provider's counters after confirmation.
  async function resetProviderStats(providerId) {
    const label = (providerDetailCache[providerId] && providerDetailCache[providerId].name) || providerId;
    if (!confirm(t('stats.confirmResetProvider', label))) return;
    try {
      const res = await api('/forward-stats/reset-provider', {
        method: 'POST',
        body: JSON.stringify({ id: providerId })
      });
      if (!res.ok) throw new Error('http ' + res.status);
      toast(t('stats.resetProviderDone'), 'success');
      loadForwardStats();
      loadStatsWindow();
      // After a reset the panel is always re-opened from Stats context, so
      // pass the current range so the fresh zeros match the windowed row.
      loadProviderDetail(providerId, statsHistoryHours);
    } catch (e) {
      toastError(t('stats.resetProviderFailed'));
    }
  }

  // exportProviderEvents downloads this provider's event log as CSV. The admin
  // password rides in a cookie (as for SSE), so a plain navigation authenticates.
  function exportProviderEvents(providerId) {
    setAdminCookie(password);
    const url = '/admin/api/forward-events/export?format=csv&provider=' + encodeURIComponent(providerId);
    window.open(url, '_blank');
  }

  // filterEventsByProvider scopes the shared event log to one provider and
  // scrolls to it, rather than duplicating a second log widget per panel.
  function filterEventsByProvider(providerId) {
    const sel = $('fwdFilterProvider');
    if (sel) {
      sel.value = providerId;
      // The custom-select wrapper mirrors the native value into its own label.
      if (typeof syncCustomSelect === 'function') syncCustomSelect(sel);
    }
    fwdEventsOffset = 0;
    loadForwardEvents();
    // The events table now lives in the activity pane; without this the scroll
    // below would target a hidden element and appear to do nothing.
    switchForwardPane('activity');
    const table = $('fwdEventsBody');
    if (table && table.closest('.card')) {
      table.closest('.card').scrollIntoView({ behavior: 'smooth', block: 'start' });
    }
  }

  // switchForwardPane toggles the Forwarding tab between config and activity.
  function switchForwardPane(pane) {
    const target = pane === 'activity' ? 'activity' : 'config';
    const cfg = $('fwdPaneConfig');
    const act = $('fwdPaneActivity');
    if (cfg) cfg.classList.toggle('hidden', target !== 'config');
    if (act) act.classList.toggle('hidden', target !== 'activity');
    const bar = $('fwdSubtabs');
    if (bar) {
      bar.querySelectorAll('[data-fwd-pane]').forEach(b => {
        const on = b.dataset.fwdPane === target;
        b.classList.toggle('active', on);
        b.setAttribute('aria-selected', on ? 'true' : 'false');
      });
    }
  }

  // ===== Stats tab: cross-provider comparison =====
  // Reads the same /forward-stats payload the Forwarding tab loads, so the two
  // views cannot disagree, and reuses the shared provider detail panel for
  // drill-down.

  let statsSortKey = 'requests';
  let statsSortDesc = true;
  let statsHistoryHours = 1;
  // Last trend payload, kept so a language switch can re-render the chart
  // heading and note (both are built in JS, not via data-i18n).
  let statsTrendLast = { buckets: [], unit: 'hour' };

  // statsSortValue projects a provider row onto its sort key. tokens is derived
  // rather than stored, and lastUsed sorts unused providers last regardless of
  // direction (a never-used provider is not "oldest").
  function statsSortValue(row, key) {
    if (key === 'tokens') return (row.inputTokens || 0) + (row.outputTokens || 0);
    if (key === 'providerName') return (row.providerName || '').toLowerCase();
    return row[key] || 0;
  }

  // statsRows is the single entry point to the data behind the Stats tab, so
  // the range filter only has to be honoured in one place.
  function statsRows() {
    return statsWindowProviders || forwardStats.providers || [];
  }

  function sortedStatsRows() {
    const rows = statsRows().slice();
    rows.sort((a, b) => {
      const av = statsSortValue(a, statsSortKey);
      const bv = statsSortValue(b, statsSortKey);
      let cmp;
      if (typeof av === 'string' || typeof bv === 'string') {
        cmp = String(av).localeCompare(String(bv));
      } else {
        cmp = av - bv;
      }
      return statsSortDesc ? -cmp : cmp;
    });
    return rows;
  }

  // ---- At-a-glance comparison ----
  // The numeric table is exact but demands that the reader do the ranking. This
  // block does the ranking for them: one "who wins" card per dimension, then a
  // normalised bar per provider so the size of the gap is visible too.
  //
  // Only providers with traffic take part. A provider with zero requests has no
  // latency and no success rate, and including it would either invent a 0 or
  // hand it a bogus win.

  function statsCompareRows() {
    return statsRows().filter(p => (p.requests || 0) > 0);
  }

  // costPer1M normalises spend so a low-volume provider is not flattered by a
  // small total bill. Returns null when the provider has no price configured or
  // no tokens yet — an unpriced provider is unknown, not free.
  function costPer1M(p) {
    const tokens = (p.inputTokens || 0) + (p.outputTokens || 0);
    if (!p.costUsd || !tokens) return null;
    return p.costUsd / tokens * 1e6;
  }

  function statsProviderLabel(p) {
    return p.providerName || p.providerId || t('upstreams.unnamed');
  }

  // compareMetric describes one comparable dimension: how to read the value off
  // a row, which direction is better, and how to format it.
  //
  // sortHint names the sort order rather than saying "lower is better". Since the
  // bars encode raw magnitude, the ranking is communicated by position and colour,
  // and the heading should tell the reader how the list is ordered.
  //
  // Cost is USD-only on purpose. The Kiro pool is metered in credits, so the two
  // units cannot share a scale or a winner; the pool still appears in the
  // latency, reliability and volume comparisons, where the units do match.
  function statsCompareMetrics() {
    return [
      {
        key: 'latency',
        label: t('stats.cmpLatency'),
        sortHint: t('stats.cmpSortFastest'),
        lowerBetter: true,
        value: p => p.avgLatencyMs || 0,
        format: v => fwdFmtLatency(v),
        champion: t('stats.cmpFastest')
      },
      {
        key: 'success',
        label: t('stats.cmpSuccess'),
        sortHint: t('stats.cmpSortReliable'),
        lowerBetter: false,
        // -1 means "no decided requests" (e.g. everything was canceled); such a
        // provider is excluded from this dimension rather than scored as 0%.
        value: p => (p.successRate == null || p.successRate < 0 ? null : p.successRate),
        format: v => fwdFmtPct(v),
        champion: t('stats.cmpMostReliable')
      },
      {
        key: 'cost',
        label: t('stats.cmpCost'),
        sortHint: t('stats.cmpSortCheapest'),
        lowerBetter: true,
        value: p => (p.isPool ? null : costPer1M(p)),
        format: v => '$' + (v >= 1 ? v.toFixed(2) : v.toFixed(4)),
        champion: t('stats.cmpCheapest'),
        note: t('stats.cmpCostNote')
      },
      {
        key: 'volume',
        label: t('stats.cmpVolume'),
        sortHint: t('stats.cmpSortBusiest'),
        lowerBetter: false,
        value: p => p.requests || 0,
        format: v => formatNum(v),
        champion: t('stats.cmpBusiest')
      }
    ];
  }

  // metricEntries pairs each eligible provider with its value for one metric,
  // best first.
  function metricEntries(metric, rows) {
    const out = [];
    rows.forEach(p => {
      const v = metric.value(p);
      if (v == null || !isFinite(v)) return;
      out.push({ provider: p, value: v });
    });
    out.sort((a, b) => metric.lowerBetter ? a.value - b.value : b.value - a.value);
    return out;
  }

  // barPct scales a value to bar width.
  //
  // The bar always encodes the RAW magnitude: a longer bar means a bigger number,
  // never "better". An earlier version inverted the scale for lower-is-better
  // metrics so the winner had the longest bar; that read backwards, because a
  // 364ms provider drawn as a full-width bar looks like the slowest one no matter
  // what the caption says. Direction is carried by sort order and colour instead.
  //
  // Skewed distributions get a log scale. With one provider at 13.1K requests and
  // the rest in the hundreds, a linear scale collapses everything below the leader
  // into an indistinguishable dot, so 347 and 140 look identical.
  function barPct(value, entries, useLog) {
    const vals = entries.map(e => e.value).filter(v => isFinite(v));
    const max = Math.max(...vals);
    if (!isFinite(max) || max <= 0) return 0;
    if (!useLog) return (value / max) * 100;
    // log1p keeps zero at zero and is defined for all non-negative values.
    return (Math.log1p(Math.max(0, value)) / Math.log1p(max)) * 100;
  }

  // shouldLogScale reports whether a metric's spread is wide enough that a linear
  // bar chart would hide the differences among the smaller entries. The threshold
  // is the ratio between the largest value and the median.
  function shouldLogScale(entries) {
    const vals = entries.map(e => e.value).filter(v => isFinite(v) && v > 0).sort((a, b) => a - b);
    if (vals.length < 3) return false;
    const median = vals[Math.floor(vals.length / 2)];
    const max = vals[vals.length - 1];
    return median > 0 && max / median >= 20;
  }

  // How many rows each bar group shows before "show all". The top of the ranking
  // is what an operator acts on; 12th versus 13th place changes no decision.
  const STATS_BAR_COLLAPSED = 5;
  const statsBarExpanded = {}; // metric key -> true while fully expanded

  function statsChampionCard(metric, entries) {
    // A "winner" chosen from a field of one is not a comparison. This happens in
    // practice on the cost metric, where usually only a provider or two has
    // pricing configured.
    if (entries.length < 2) {
      const only = entries.length === 1
        ? t('stats.cmpOnlyOne', statsProviderLabel(entries[0].provider), metric.format(entries[0].value))
        : t('stats.cmpNoData');
      return '<div class="stats-champ stats-champ--empty">' +
        '<div class="stats-champ-title">' + escapeHtml(metric.champion) + '</div>' +
        '<div class="stats-champ-name muted-text">' + escapeHtml(only) + '</div>' +
        '</div>';
    }
    const best = entries[0];
    // The runner-up gap is the actionable part: "fastest" matters much less when
    // the second place is 2% behind than when it is twice as slow.
    let gap = '';
    const next = entries[1];
    const a = best.value, b = next.value;
    if (a > 0 && b > 0) {
      const pct = metric.lowerBetter ? (b - a) / b * 100 : (a - b) / a * 100;
      if (pct >= 1) gap = t('stats.cmpGap', pct.toFixed(0), statsProviderLabel(next.provider));
    }
    return '<div class="stats-champ">' +
      '<div class="stats-champ-title">' + escapeHtml(metric.champion) + '</div>' +
      '<div class="stats-champ-name" title="' + escapeAttr(statsProviderLabel(best.provider)) + '">' +
        escapeHtml(statsProviderLabel(best.provider)) + '</div>' +
      '<div class="stats-champ-value">' + escapeHtml(metric.format(best.value)) + '</div>' +
      (gap ? '<div class="stats-champ-gap">' + escapeHtml(gap) + '</div>' : '') +
      '</div>';
  }

  // rankTone buckets a row's rank into a colour tier. Colour now carries rank
  // information for every row rather than marking only first and last, which left
  // the middle of the field as a wall of identical bars.
  function rankTone(i, n) {
    if (i === 0) return 'best';
    if (n > 2 && i === n - 1) return 'worst';
    // Top third / middle / bottom third.
    if (i < n / 3) return 'good';
    if (i < (n * 2) / 3) return 'mid';
    return 'poor';
  }

  function statsCompareBars(metric, entries) {
    // With fewer than two participants there is no comparison to draw; the
    // champion card already states the single value.
    if (entries.length < 2) return '';

    const useLog = shouldLogScale(entries);
    const expanded = !!statsBarExpanded[metric.key];
    const hidden = Math.max(0, entries.length - STATS_BAR_COLLAPSED);
    const shown = expanded ? entries : entries.slice(0, STATS_BAR_COLLAPSED);

    const rowsHtml = shown.map((e, i) => {
      const pct = Math.max(1.5, Math.min(100, barPct(e.value, entries, useLog)));
      const label = statsProviderLabel(e.provider);
      return '<div class="stats-bar-row">' +
        '<span class="stats-bar-rank">' + (i + 1) + '</span>' +
        '<span class="stats-bar-label" title="' + escapeAttr(label) + '">' +
          escapeHtml(label) +
          (e.provider.isPool ? ' <span class="stats-tag stats-tag--pool">' + escapeHtml(t('stats.poolTag')) + '</span>' : '') +
        '</span>' +
        '<span class="stats-bar-track">' +
          '<span class="stats-bar-fill stats-bar-fill--' + rankTone(i, entries.length) + '" style="width:' + pct.toFixed(1) + '%"></span>' +
        '</span>' +
        '<span class="stats-bar-value">' + escapeHtml(metric.format(e.value)) + '</span>' +
        '</div>';
    }).join('');

    const toggle = hidden
      ? '<button type="button" class="stats-bar-more" data-cmp-toggle="' + escapeAttr(metric.key) + '">' +
          escapeHtml(expanded ? t('stats.cmpShowLess') : t('stats.cmpShowAll', String(entries.length))) +
        '</button>'
      : '';

    return '<div class="stats-cmp-group">' +
      '<div class="stats-cmp-head">' +
        '<span class="stats-cmp-title">' + escapeHtml(metric.label) + '</span>' +
        '<span class="stats-cmp-hint">' + escapeHtml(metric.sortHint) + '</span>' +
      '</div>' +
      (metric.note ? '<div class="stats-cmp-note">' + escapeHtml(metric.note) + '</div>' : '') +
      (useLog ? '<div class="stats-cmp-note">' + escapeHtml(t('stats.cmpLogNote')) + '</div>' : '') +
      rowsHtml +
      toggle +
      '</div>';
  }

  function renderStatsCompare() {
    const host = $('statsCompare');
    if (!host) return;
    const rows = statsCompareRows();
    // With a single active provider there is nothing to compare against, so the
    // block would be pure decoration.
    if (rows.length < 2) {
      host.innerHTML = rows.length
        ? '<p class="muted-text text-xs stats-cmp-empty">' + escapeHtml(t('stats.cmpNeedTwo')) + '</p>'
        : '';
      return;
    }

    const metrics = statsCompareMetrics();
    const entriesByKey = {};
    metrics.forEach(m => { entriesByKey[m.key] = metricEntries(m, rows); });

    host.innerHTML =
      '<div class="stats-champ-grid">' +
        metrics.map(m => statsChampionCard(m, entriesByKey[m.key])).join('') +
      '</div>' +
      '<div class="stats-cmp-bars">' +
        metrics.map(m => statsCompareBars(m, entriesByKey[m.key])).join('') +
      '</div>';
  }

  // ---- Table heatmap ----
  // Tints the best and worst cell of each comparable column. Only columns with a
  // meaningful direction are tinted: total tokens and total cost measure how
  // much a provider was used, not how well it performed, so tinting them would
  // punish the provider carrying the most traffic.
  const STATS_HEAT_COLS = {
    requests: { lowerBetter: false },
    successRate: { lowerBetter: false },
    avgLatencyMs: { lowerBetter: true },
    rpm: { lowerBetter: false }
  };

  // statsHeatClasses maps providerId -> column key -> 'best' | 'worst'.
  function statsHeatClasses(rows) {
    const out = {};
    if (rows.length < 2) return out;
    Object.keys(STATS_HEAT_COLS).forEach(key => {
      const lower = STATS_HEAT_COLS[key].lowerBetter;
      const vals = [];
      rows.forEach(p => {
        let v = key === 'successRate'
          ? (p.successRate == null || p.successRate < 0 ? null : p.successRate)
          : p[key];
        if (v == null || !isFinite(v)) return;
        vals.push({ id: p.providerId, v: v });
      });
      if (vals.length < 2) return;
      vals.sort((a, b) => lower ? a.v - b.v : b.v - a.v);
      const bestV = vals[0].v, worstV = vals[vals.length - 1].v;
      // An all-equal column has no winner to highlight.
      if (bestV === worstV) return;
      vals.forEach(e => {
        if (e.v !== bestV && e.v !== worstV) return;
        out[e.id] = out[e.id] || {};
        out[e.id][key] = e.v === bestV ? 'best' : 'worst';
      });
    });
    return out;
  }

  function heatCls(heat, id, key) {
    const v = heat[id] && heat[id][key];
    return v ? ' stats-heat--' + v : '';
  }

  function renderStatsTable() {
    const body = $('statsTableBody');
    if (!body) return;
    const rows = sortedStatsRows();
    if (!rows.length) {
      body.innerHTML = '<tr><td colspan="8" class="muted-text" style="padding:1rem;text-align:center;">' +
        escapeHtml(t('stats.noProviders')) + '</td></tr>';
      return;
    }

    // Heatmap ranks only providers with traffic, matching the comparison block.
    const heat = statsHeatClasses(rows.filter(p => (p.requests || 0) > 0));

    body.innerHTML = rows.map(p => {
      const id = escapeAttr(p.providerId || '');
      const tokens = (p.inputTokens || 0) + (p.outputTokens || 0);
      const name = p.providerName || p.providerId || t('upstreams.unnamed');
      // Health dot: a provider on a failure streak is called out here, because
      // this table is where an operator decides which provider to stop using.
      const dotClass = p.requests === 0 ? 'idle' : (p.healthy ? 'ok' : 'bad');
      const badges =
        (p.isPool ? '<span class="stats-tag stats-tag--pool">' + escapeHtml(t('stats.poolTag')) + '</span>' : '') +
        (p.configured && !p.enabled && !p.isPool ? '<span class="stats-tag stats-tag--off">' + escapeHtml(t('upstreams.disabled')) + '</span>' : '') +
        (!p.configured ? '<span class="stats-tag stats-tag--orphan" title="' + escapeAttr(t('stats.orphanHint')) + '">' + escapeHtml(t('stats.orphanTag')) + '</span>' : '');
      const pid = p.providerId || '';

      return '<tr class="stats-row" data-stats-provider="' + id + '" tabindex="0">' +
        '<td><span class="stats-dot stats-dot--' + dotClass + '"></span>' +
          '<span class="stats-name">' + escapeHtml(name) + '</span>' + badges + '</td>' +
        '<td class="num' + heatCls(heat, pid, 'requests') + '">' + escapeHtml(formatNum(p.requests || 0)) + '</td>' +
        '<td class="num' + heatCls(heat, pid, 'successRate') + '">' + escapeHtml(fwdFmtPct(p.successRate)) + '</td>' +
        '<td class="num">' + escapeHtml(tokens ? formatNum(tokens) : '—') + '</td>' +
        '<td class="num">' + escapeHtml(fwdFmtCost(p.costUsd, p.isPool)) + '</td>' +
        '<td class="num' + heatCls(heat, pid, 'avgLatencyMs') + '">' + escapeHtml(p.requests ? fwdFmtLatency(p.avgLatencyMs) : '—') + '</td>' +
        '<td class="num' + heatCls(heat, pid, 'rpm') + '">' + escapeHtml(fwdFmtRate(p.rpm)) + '</td>' +
        '<td class="num">' + escapeHtml(p.lastUsed ? fwdFmtWhen(p.lastUsed) : '—') + '</td>' +
        '</tr>' +
        '<tr class="stats-detail-row"><td colspan="8">' +
          '<div class="provider-detail hidden" data-provider-detail="' + id + '"></div>' +
        '</td></tr>';
    }).join('');

    // Reflect the active sort in the header.
    qsa('#statsTable th[data-stats-sort]').forEach(th => {
      const active = th.dataset.statsSort === statsSortKey;
      th.classList.toggle('sorted', active);
      th.classList.toggle('desc', active && statsSortDesc);
      th.setAttribute('aria-sort', active ? (statsSortDesc ? 'descending' : 'ascending') : 'none');
    });

    // Re-expand whatever was open before the re-render.
    Object.keys(openProviderDetails).forEach(pid => {
      const host = document.querySelector('#statsTableBody [data-provider-detail="' + cssEscape(pid) + '"]');
      if (host) {
        host.classList.remove('hidden');
        if (providerDetailCache[pid]) host.innerHTML = renderProviderDetailHTML(providerDetailCache[pid]);
      }
    });
  }

  // renderStatsTrend draws the history chart. The bucket unit follows the
  // selected range (per-minute for the 1-hour view, hourly beyond that), so the
  // heading names the unit rather than assuming hours.
  function renderStatsTrend(buckets, unit) {
    statsTrendLast = { buckets: buckets || [], unit: unit };
    const host = $('statsTrendChart');
    const isMinute = unit === 'minute';
    const title = $('statsTrendTitle');
    if (title) title.textContent = t(isMinute ? 'stats.trendTitleMinute' : 'stats.trendTitle');
    const note = $('statsTrendNote');
    // Per-minute buckets live only in memory, so say so instead of letting an
    // empty 1-hour chart read as "no traffic" after a restart.
    if (note) note.textContent = isMinute ? t('stats.trendMinuteNote') : '';
    if (!host) return;
    const series = buckets || [];
    const total = series.reduce((n, b) => n + (b.requests || 0), 0);
    if (!total) {
      host.innerHTML = '<div class="muted-text text-xs" style="padding:1rem 0;">' +
        escapeHtml(t('forward.noData')) + '</div>';
    } else {
      const w = 100, h = 40, n = series.length, bw = w / n;
      const max = Math.max(1, ...series.map(b => b.requests || 0));
      let bars = '';
      series.forEach((b, i) => {
        const reqs = b.requests || 0;
        if (!reqs) return;
        const th = (reqs / max) * h;
        const fh = ((b.failed || 0) / max) * h;
        const sh = th - fh;
        const x = (i * bw).toFixed(2);
        const fw = (bw * 0.8).toFixed(2);
        if (sh > 0) bars += '<rect x="' + x + '" y="' + (h - th).toFixed(2) + '" width="' + fw + '" height="' + sh.toFixed(2) + '" class="spark-ok"></rect>';
        if (fh > 0) bars += '<rect x="' + x + '" y="' + (h - fh).toFixed(2) + '" width="' + fw + '" height="' + fh.toFixed(2) + '" class="spark-err"></rect>';
      });
      host.innerHTML = '<svg viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none" style="width:100%;height:60px;">' + bars + '</svg>';
    }

    const summary = $('statsTrendSummary');
    if (summary) {
      const failed = series.reduce((n, b) => n + (b.failed || 0), 0);
      const tokens = series.reduce((n, b) => n + (b.inputTokens || 0) + (b.outputTokens || 0), 0);
      summary.textContent = total
        ? t('stats.trendSummary', formatNum(total), formatNum(failed), formatNum(tokens))
        : '';
    }
  }

  async function loadStatsHistory() {
    try {
      const res = await api('/forward-history?hours=' + encodeURIComponent(statsHistoryHours));
      if (!res.ok) throw new Error('http ' + res.status);
      const d = await res.json();
      renderStatsTrend(d.buckets || [], d.unit);
    } catch (e) {
      renderStatsTrend([], statsHistoryHours <= 1 ? 'minute' : 'hour');
    }
  }

  // loadStatsWindow refetches the per-provider figures scoped to the selected
  // range and re-renders the two surfaces that read them. On failure the window
  // copy is dropped so the tab falls back to all-time rather than freezing on a
  // stale range.
  async function loadStatsWindow() {
    try {
      const res = await api('/forward-stats?hours=' + encodeURIComponent(statsHistoryHours));
      if (!res.ok) throw new Error('http ' + res.status);
      const d = await res.json();
      statsWindowProviders = Array.isArray(d.providers) ? d.providers : null;
    } catch (e) {
      statsWindowProviders = null;
    }
    renderStatsTable();
    renderStatsCompare();
  }

  // loadTopIps fetches the per-source-IP leaderboard and renders it. Failures
  // leave the previous table in place rather than blanking the panel.
  async function loadTopIps() {
    try {
      const res = await api('/top-ips?limit=50');
      if (!res.ok) throw new Error('http ' + res.status);
      const d = await res.json();
      renderTopIps(Array.isArray(d.ips) ? d.ips : [], !!d.trustProxy);
    } catch (e) {
      renderTopIps([], false);
    }
  }

  function renderTopIps(ips, trustProxy) {
    const body = $('topIpsBody');
    if (!body) return;
    const note = $('statsTopIpsNote');
    if (note) note.textContent = trustProxy ? t('stats.topIpsProxyOn') : '';
    if (!ips.length) {
      body.innerHTML = '<tr><td colspan="6" class="muted-text" style="padding:1rem;text-align:center;">' +
        escapeHtml(t('stats.topIpsEmpty')) + '</td></tr>';
      return;
    }
    body.innerHTML = ips.map(ip => {
      const ok = ip.requests ? (100 * (ip.success || 0) / ip.requests) : -1;
      return '<tr>' +
        '<td><span class="stats-name font-mono">' + escapeHtml(ip.ip || '—') + '</span></td>' +
        '<td class="num">' + escapeHtml(formatNum(ip.requests || 0)) + '</td>' +
        '<td class="num">' + escapeHtml(fwdFmtPct(ok)) + '</td>' +
        '<td class="num">' + escapeHtml(ip.totalTokens ? formatNum(ip.totalTokens) : '—') + '</td>' +
        '<td class="num">' + escapeHtml(ip.requests ? fwdFmtLatency(ip.avgLatencyMs) : '—') + '</td>' +
        '<td class="num">' + escapeHtml(ip.lastUsed ? fwdFmtWhen(ip.lastUsed) : '—') + '</td>' +
        '</tr>';
    }).join('');
  }

  function openStats() {
    loadForwardStats();
    loadStatsWindow();
    loadStatsHistory();
    loadTopIps();
  }

  function bindStatsEvents() {
    // Delegated so the per-group "show all" buttons survive every re-render.
    const cmp = $('statsCompare');
    if (cmp) {
      cmp.addEventListener('click', e => {
        const btn = e.target.closest('[data-cmp-toggle]');
        if (!btn) return;
        const key = btn.dataset.cmpToggle;
        statsBarExpanded[key] = !statsBarExpanded[key];
        renderStatsCompare();
      });
    }

    const table = $('statsTable');
    if (table) {
      table.addEventListener('click', e => {
        const th = e.target.closest('th[data-stats-sort]');
        if (th) {
          const key = th.dataset.statsSort;
          if (key === statsSortKey) {
            statsSortDesc = !statsSortDesc;
          } else {
            statsSortKey = key;
            // Names read best A→Z; every numeric column reads best largest-first.
            statsSortDesc = key !== 'providerName';
          }
          renderStatsTable();
          return;
        }
        const row = e.target.closest('tr[data-stats-provider]');
        if (row) toggleProviderDetail(row.dataset.statsProvider, null, $('statsTableBody'));
      });
      // Keyboard parity for row expansion.
      table.addEventListener('keydown', e => {
        if (e.key !== 'Enter' && e.key !== ' ') return;
        const row = e.target.closest('tr[data-stats-provider]');
        if (!row) return;
        e.preventDefault();
        toggleProviderDetail(row.dataset.statsProvider, null, $('statsTableBody'));
      });
    }

    const range = $('statsRangeSelect');
    if (range) {
      range.addEventListener('change', () => {
        statsHistoryHours = parseInt(range.value, 10) || 24;
        loadStatsWindow();
        loadStatsHistory();
      });
    }
    const refresh = $('statsRefreshBtn');
    if (refresh) refresh.addEventListener('click', openStats);
    const exportBtn = $('statsExportBtn');
    if (exportBtn) {
      exportBtn.addEventListener('click', () => {
        setAdminCookie(password);
        window.open('/admin/api/forward-events/export?format=csv', '_blank');
      });
    }
  }

  async function loadForwardEvents() {
    const body = $('fwdEventsBody');
    if (!body) return;
    const params = new URLSearchParams();
    params.set('offset', String(fwdEventsOffset));
    params.set('limit', String(fwdEventsLimit));
    const prov = $('fwdFilterProvider') ? $('fwdFilterProvider').value : '';
    const status = $('fwdFilterStatus') ? $('fwdFilterStatus').value : '';
    const model = $('fwdFilterModel') ? $('fwdFilterModel').value.trim() : '';
    if (prov) params.set('provider', prov);
    if (status) params.set('status', status);
    if (model) params.set('model', model);
    // The range is expressed as a lower bound resolved at request time, not a
    // stored timestamp, so paging and live refreshes always mean "the last N
    // hours from now" rather than from whenever the filter was picked.
    const sinceMs = fwdFilterSinceMs();
    if (sinceMs) params.set('since', String(sinceMs));
    try {
      const res = await api('/forward-events?' + params.toString());
      if (!res.ok) throw new Error('http ' + res.status);
      const d = await res.json();
      fwdEventsTotal = d.total || 0;
      renderForwardEvents(Array.isArray(d.items) ? d.items : []);
    } catch (e) {
      body.innerHTML = '<tr class="fwd-empty-row"><td colspan="6" class="muted-text text-xs" style="padding:0.75rem;">' + escapeHtml(t('common.failed')) + '</td></tr>';
    }
  }

  function renderForwardEvents(items) {
    const body = $('fwdEventsBody');
    if (!body) return;
    if (!items.length) {
      body.innerHTML = '<tr class="fwd-empty-row"><td colspan="6" class="muted-text text-xs" style="padding:0.75rem;">' + escapeHtml(t('forward.noEvents')) + '</td></tr>';
    } else {
      // A fresh page invalidates every retained event: uids are never reused, so
      // stale entries would otherwise accumulate for the life of the tab.
      fwdEventStore = {};
      fwdEventKeys = new Set();
      body.innerHTML = items.map(fwdEventRow).join('');
    }
    const countEl = $('fwdEventsCount');
    if (countEl) countEl.textContent = t('forward.eventsCount', String(fwdEventsTotal));
    updateForwardPagination();
  }

  // Events carry no server-side id, so rows get a client-side uid and the event
  // object is retained to render its detail panel (and copy its JSON) on demand.
  let fwdEventSeq = 0;
  let fwdEventStore = {};
  // Rendered events, keyed by natural identity. The SSE stream backfills its most
  // recent 50 events on connect, which overlaps whatever the REST page just
  // rendered; without this every event present at load time appears twice.
  let fwdEventKeys = new Set();

  // A stable identity for an event that has no server-side id. time is a
  // millisecond stamp and the remaining fields distinguish concurrent requests
  // that share one.
  function fwdEventKey(e) {
    return [e.time, e.providerId, e.accountId, e.clientModel, e.status, e.latencyMs].join('|');
  }

  function fwdEventRow(e) {
    const uid = 'ev' + (++fwdEventSeq);
    fwdEventStore[uid] = e;
    fwdEventKeys.add(fwdEventKey(e));
    const model = e.targetModel && e.targetModel !== e.clientModel
      ? escapeHtml(e.clientModel) + ' <span class="muted-text">&rarr;</span> ' + escapeHtml(e.targetModel)
      : escapeHtml(e.clientModel || '');
    const prov = escapeHtml(e.providerName || fwdProviderNames[e.providerId] || e.providerId || '');
    const badge = e.ok
      ? '<span class="fwd-badge fwd-badge--ok">' + (e.status || 200) + '</span>'
      : '<span class="fwd-badge fwd-badge--err">' + (e.status || 'ERR') + '</span>';
    // Failures are the reason to open a row, so surface a truncated reason inline
    // and keep the full text for the panel.
    const hint = !e.ok && e.errorMsg
      ? ' <span class="fwd-err-hint" title="' + escapeHtml(e.errorMsg) + '">' + escapeHtml(fwdTruncate(e.errorMsg, 48)) + '</span>'
      : '';
    return '<tr class="fwd-event-row" data-fwd-event="' + uid + '" tabindex="0" role="button" aria-expanded="false">' +
      '<td class="font-mono text-xs">' + escapeHtml(fwdFmtTime(e.time)) + '</td>' +
      '<td class="font-mono text-xs">' + model + '</td>' +
      '<td class="text-xs">' + prov + '</td>' +
      '<td>' + badge + hint + '</td>' +
      '<td class="font-mono text-xs">' + escapeHtml(fwdFmtLatency(e.latencyMs)) + '</td>' +
      '<td class="fwd-chev"><i class="fa-solid fa-chevron-down" aria-hidden="true"></i></td>' +
      '</tr>' +
      '<tr class="fwd-detail-row hidden" data-fwd-detail="' + uid + '"><td colspan="6"></td></tr>';
  }

  function fwdTruncate(s, n) {
    s = String(s).replace(/\s+/g, ' ').trim();
    return s.length > n ? s.slice(0, n - 1) + '…' : s;
  }

  // toggleForwardEventDetail expands one event row, rendering its panel lazily.
  async function toggleForwardEventDetail(uid) {
    const body = $('fwdEventsBody');
    if (!body) return;
    const row = body.querySelector('tr[data-fwd-event="' + cssEscape(uid) + '"]');
    const detail = body.querySelector('tr[data-fwd-detail="' + cssEscape(uid) + '"]');
    if (!row || !detail) return;
    const open = !detail.classList.contains('hidden');
    if (open) {
      detail.classList.add('hidden');
      row.setAttribute('aria-expanded', 'false');
      row.classList.remove('fwd-event-row--open');
      return;
    }
    const e = fwdEventStore[uid];
    if (!e) return;
    let extra = null;
    if (e.requestId) {
      try {
        const res = await api('/provider-errors?requestId=' + encodeURIComponent(e.requestId));
        if (res.ok) extra = await res.json();
      } catch (err) { /* admin diagnostic is optional */ }
    }
    detail.querySelector('td').innerHTML = fwdEventDetailHtml(e, uid, extra);
    detail.classList.remove('hidden');
    row.setAttribute('aria-expanded', 'true');
    row.classList.add('fwd-event-row--open');
  }

  // fwdEventDetailHtml renders the fields the table has no room for. ErrorMsg is
  // the upstream's own reason: it is deliberately withheld from API clients (it
  // can leak upstream host/proxy topology) but this page is behind admin auth.
  function fwdEventDetailHtml(e, uid, extra) {
    const isPool = e.providerId === '__kiro_pool__';
    const rows = [];
    const add = (label, value) => {
      if (value === '' || value == null) return;
      rows.push('<div class="fwd-detail-item"><span class="fwd-detail-label">' + escapeHtml(label) +
        '</span><span class="fwd-detail-value">' + value + '</span></div>');
    };
    add(t('forward.detailWhen'), escapeHtml(fwdFmtWhen(e.time)));
    if (e.requestId) add(t('forward.detailRequestId'), '<span class="font-mono">' + escapeHtml(e.requestId) + '</span>');
    add(t('forward.detailEndpoint'), escapeHtml(e.endpoint || '—'));
    add(t('forward.detailClientModel'), escapeHtml(e.clientModel || '—'));
    if (e.targetModel) add(t('forward.detailTargetModel'), escapeHtml(e.targetModel));
    add(t('forward.detailProvider'), escapeHtml(e.providerName || fwdProviderNames[e.providerId] || e.providerId || '—'));
    if (e.connectionName) add(t('forward.detailConnection'), escapeHtml(e.connectionName));
    if (e.accountLabel || e.accountId) add(t('forward.detailAccount'), escapeHtml(e.accountLabel || e.accountId));
    if (e.routeId) add(t('forward.detailRoute'), '<span class="font-mono">' + escapeHtml(e.routeId) + '</span>');
    add(t('forward.detailStatus'), String(e.status || '—'));
    add(t('forward.detailLatency'), escapeHtml(fwdFmtLatency(e.latencyMs)));
    add(t('forward.detailTtfb'), e.ttfbMs ? escapeHtml(fwdFmtLatency(e.ttfbMs)) : '—');
    add(t('forward.detailTokensIn'), String(e.inputTokens || 0));
    add(t('forward.detailTokensOut'), String(e.outputTokens || 0));
    add(t('forward.detailCost'), escapeHtml(fwdFmtCost(e.costUsd, isPool)));
    add(t('forward.detailStream'), e.stream ? t('forward.detailYes') : t('forward.detailNo'));
    if (e.canceled) add(t('forward.detailCanceled'), t('forward.detailYes'));

    let err = '';
    const attempts = extra && Array.isArray(extra.attempts) ? extra.attempts : [];
    const diag = attempts.length ? attempts[attempts.length - 1] : null;
    // With a key pool one request can be rejected several times over — once per key,
    // then once per backup provider. Showing only the last rejection would name the
    // wrong credential, so every attempt is listed whenever there is more than one.
    let ladder = '';
    if (attempts.length > 1) {
      ladder = '<div class="fwd-detail-error"><div class="fwd-detail-label">' +
        escapeHtml(t('forward.detailAttempts', String(attempts.length))) + '</div><ol class="fwd-attempt-list">' +
        attempts.map(a => {
          const who = [a.providerName || a.providerId || '—', a.connectionName || ''].filter(Boolean).join(' / ');
          const what = [a.upstreamStatus ? String(a.upstreamStatus) : '', a.upstreamCode || a.category || ''].filter(Boolean).join(' ');
          const why = a.upstreamMessage || a.detail || '';
          return '<li><span class="font-mono">' + escapeHtml(who) + '</span>' +
            (what ? ' <span class="fwd-attempt-status">' + escapeHtml(what) + '</span>' : '') +
            (why ? '<div class="fwd-attempt-why">' + escapeHtml(why) + '</div>' : '') + '</li>';
        }).join('') + '</ol></div>';
    }
    if (diag) {
      if (diag.connectionName && !e.connectionName) add(t('forward.detailConnection'), escapeHtml(diag.connectionName));
      if (diag.upstreamStatus) add(t('forward.detailUpstreamStatus'), String(diag.upstreamStatus));
      if (diag.upstreamCode) add(t('forward.detailUpstreamCode'), escapeHtml(diag.upstreamCode));
      if (diag.upstreamRequestId) add(t('forward.detailUpstreamRequestId'), '<span class="font-mono">' + escapeHtml(diag.upstreamRequestId) + '</span>');
      if (diag.retryAfter) add(t('forward.detailRetryAfter'), escapeHtml(diag.retryAfter));
      if (diag.publicCode) add(t('forward.detailPublicCode'), escapeHtml(diag.publicCode));
      const raw = diag.detail || diag.upstreamMessage || '';
      err = '<div class="fwd-detail-error"><div class="fwd-detail-label">' +
        escapeHtml(t('forward.detailError')) +
        (diag.detailTruncated ? ' <span class="muted-text">(' + escapeHtml(t('forward.detailTruncated')) + ')</span>' : '') +
        '</div><pre class="fwd-detail-errtext">' + escapeHtml(raw || e.errorMsg || '') + '</pre></div>';
    } else if (e.errorMsg) {
      err = '<div class="fwd-detail-error"><div class="fwd-detail-label">' +
        escapeHtml(t('forward.detailError')) + '</div><pre class="fwd-detail-errtext">' +
        escapeHtml(e.errorMsg) + '</pre></div>';
    } else if (!e.ok) {
      err = '<div class="fwd-detail-error"><div class="fwd-detail-label">' +
        escapeHtml(t('forward.detailError')) + '</div><p class="muted-text text-xs">' +
        escapeHtml(t('forward.detailNoError')) + '</p></div>';
    }

    return '<div class="fwd-detail-panel">' + err + ladder +
      '<div class="fwd-detail-grid">' + rows.join('') + '</div>' +
      '<div class="fwd-detail-actions"><button type="button" class="btn btn-outline btn-sm" ' +
      'data-fwd-copy="' + escapeHtml(uid) + '"><i class="fa-solid fa-copy" aria-hidden="true"></i>' +
      '<span class="btn-text">' + escapeHtml(t('forward.detailCopy')) + '</span></button></div></div>';
  }

  function updateForwardPagination() {
    const info = $('fwdPageInfo');
    const from = fwdEventsTotal === 0 ? 0 : fwdEventsOffset + 1;
    const to = Math.min(fwdEventsOffset + fwdEventsLimit, fwdEventsTotal);
    if (info) info.textContent = t('forward.pageInfo', String(from), String(to), String(fwdEventsTotal));
    const prev = $('fwdPrevBtn');
    const next = $('fwdNextBtn');
    if (prev) prev.disabled = fwdEventsOffset <= 0;
    if (next) next.disabled = fwdEventsOffset + fwdEventsLimit >= fwdEventsTotal;
  }

  function openForwardStream() {
    if (fwdSource) return;
    // EventSource authenticates via the admin_password cookie (no headers).
    const src = new EventSource('/admin/api/forward-events/stream');
    fwdSource = src;
    const dot = $('forwardLiveDot');
    src.onopen = () => { if (dot) dot.textContent = '● ' + t('forward.live'); };
    src.onmessage = ev => {
      let e;
      try { e = JSON.parse(ev.data); } catch (err) { return; }
      // Only reflect live events on the first, unfiltered page to avoid
      // fighting active filters/pagination.
      if (fwdEventsOffset === 0 && !fwdFilterActive()) {
        const bodyEl = $('fwdEventsBody');
        // The stream replays its recent history on connect, so an event the REST
        // page already rendered must not be prepended a second time.
        if (bodyEl && !fwdEventKeys.has(fwdEventKey(e))) {
          const empty = bodyEl.querySelector('tr.fwd-empty-row');
          if (empty) bodyEl.innerHTML = '';
          bodyEl.insertAdjacentHTML('afterbegin', fwdEventRow(e));
          // Each event is a pair of rows (summary + its detail row), so trim by
          // event rather than by <tr> or a detail row would outlive its parent.
          const evRows = bodyEl.querySelectorAll('tr[data-fwd-event]');
          for (let i = evRows.length - 1; i >= fwdEventsLimit; i--) {
            const uid = evRows[i].dataset.fwdEvent;
            const det = bodyEl.querySelector('tr[data-fwd-detail="' + cssEscape(uid) + '"]');
            if (det) det.remove();
            evRows[i].remove();
            // Release the identity too, or a trimmed event could never re-render.
            if (fwdEventStore[uid]) fwdEventKeys.delete(fwdEventKey(fwdEventStore[uid]));
            delete fwdEventStore[uid];
          }
        }
      }
      // Refresh aggregate cards/chart lazily. Coalesced: a busy proxy can emit
      // hundreds of events per second, and one refetch each would flood the
      // admin API (and now the open detail panels too).
      scheduleForwardStatsRefresh();
    };
    src.onerror = () => { if (dot) dot.textContent = ''; };
  }

  let fwdStatsRefreshTimer = null;
  function scheduleForwardStatsRefresh() {
    if (fwdStatsRefreshTimer) return;
    fwdStatsRefreshTimer = setTimeout(() => {
      fwdStatsRefreshTimer = null;
      loadForwardStats();
      // The Stats tab reads a separate range-scoped payload, so it needs its own
      // refresh; skipped when hidden to avoid a request nobody is looking at.
      const statsTab = $('tabStats');
      if (statsTab && !statsTab.classList.contains('hidden')) loadStatsWindow();
    }, 2000);
  }

  // Selected range as an absolute lower bound in epoch ms, or 0 for "all time".
  // Recomputed on every call so a page kept open does not drift.
  function fwdFilterSinceMs() {
    const sel = $('fwdFilterRange');
    const hours = sel ? parseFloat(sel.value) : NaN;
    if (!isFinite(hours) || hours <= 0) return 0;
    return Date.now() - Math.round(hours * 3600 * 1000);
  }

  function fwdFilterActive() {
    const prov = $('fwdFilterProvider') ? $('fwdFilterProvider').value : '';
    const status = $('fwdFilterStatus') ? $('fwdFilterStatus').value : '';
    const model = $('fwdFilterModel') ? $('fwdFilterModel').value.trim() : '';
    // A range counts as an active filter: live events must not be prepended
    // when the view is scoped, or a row outside the window would slip in.
    return !!(prov || status || model || fwdFilterSinceMs());
  }

  function closeForwardStream() {
    if (fwdSource) { fwdSource.close(); fwdSource = null; }
    if (fwdStatsRefreshTimer) { clearTimeout(fwdStatsRefreshTimer); fwdStatsRefreshTimer = null; }
    const dot = $('forwardLiveDot');
    if (dot) dot.textContent = '';
  }

  function openForwarding() {
    fwdEventsOffset = 0;
    loadForwardStats();
    loadForwardEvents();
    openForwardStream();
  }

  function closeForwarding() {
    closeForwardStream();
  }

  let fwdModelSearchTimer = null;
  function bindForwardEvents() {
    // Sub-tab switching between the config pane and the live activity pane.
    const subtabs = $('fwdSubtabs');
    if (subtabs) {
      subtabs.addEventListener('click', e => {
        const btn = e.target.closest('[data-fwd-pane]');
        if (btn) switchForwardPane(btn.dataset.fwdPane);
      });
    }

    // Row expansion: delegated, so it survives the table being re-rendered on
    // every page load and on each live SSE event.
    const eventsBody = $('fwdEventsBody');
    if (eventsBody) {
      eventsBody.addEventListener('click', e => {
        const copyBtn = e.target.closest('[data-fwd-copy]');
        if (copyBtn) {
          const ev = fwdEventStore[copyBtn.dataset.fwdCopy];
          if (ev) copyText(JSON.stringify(ev, null, 2)).then(() => toast(t('common.copied'), 'success'));
          return;
        }
        const row = e.target.closest('tr[data-fwd-event]');
        if (row) toggleForwardEventDetail(row.dataset.fwdEvent);
      });
      eventsBody.addEventListener('keydown', e => {
        if (e.key !== 'Enter' && e.key !== ' ') return;
        const row = e.target.closest('tr[data-fwd-event]');
        if (!row) return;
        e.preventDefault();
        toggleForwardEventDetail(row.dataset.fwdEvent);
      });
    }

    const prov = $('fwdFilterProvider');
    if (prov) prov.addEventListener('change', () => { fwdEventsOffset = 0; loadForwardEvents(); });
    const status = $('fwdFilterStatus');
    if (status) status.addEventListener('change', () => { fwdEventsOffset = 0; loadForwardEvents(); });
    const range = $('fwdFilterRange');
    if (range) range.addEventListener('change', () => { fwdEventsOffset = 0; loadForwardEvents(); });
    const model = $('fwdFilterModel');
    if (model) model.addEventListener('input', () => {
      clearTimeout(fwdModelSearchTimer);
      fwdModelSearchTimer = setTimeout(() => { fwdEventsOffset = 0; loadForwardEvents(); }, 300);
    });
    const prev = $('fwdPrevBtn');
    if (prev) prev.addEventListener('click', () => {
      if (fwdEventsOffset <= 0) return;
      fwdEventsOffset = Math.max(0, fwdEventsOffset - fwdEventsLimit);
      loadForwardEvents();
    });
    const next = $('fwdNextBtn');
    if (next) next.addEventListener('click', () => {
      if (fwdEventsOffset + fwdEventsLimit >= fwdEventsTotal) return;
      fwdEventsOffset += fwdEventsLimit;
      loadForwardEvents();
    });
    const reset = $('forwardResetBtn');
    if (reset) reset.addEventListener('click', async () => {
      const ok = await confirmAction(t('forward.confirmReset'));
      if (!ok) return;
      try {
        const res = await api('/forward-stats/reset', { method: 'POST' });
        if (!res.ok) throw new Error('http ' + res.status);
        fwdEventsOffset = 0;
        toast(t('forward.resetDone'), 'success');
        loadForwardStats();
        loadForwardEvents();
      } catch (e) { toast(t('common.failed'), 'error'); }
    });
  }

  // Prompt filter rules
  async function loadPromptFilter() {
    const res = await api('/prompt-filter');
    const d = await res.json();
    $('filterClaudeCode').checked = !!d.filterClaudeCode;
    $('filterEnvNoise').checked = !!d.filterEnvNoise;
    $('filterStripBoundaries').checked = !!d.filterStripBoundaries;
    promptRules = d.rules || [];
    renderPromptRules();
  }
  async function savePromptFilter() {
    const res = await api('/prompt-filter', {
      method: 'POST', body: JSON.stringify({
        filterClaudeCode: $('filterClaudeCode').checked,
        filterEnvNoise: $('filterEnvNoise').checked,
        filterStripBoundaries: $('filterStripBoundaries').checked,
        rules: promptRules
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.promptFilterSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function loadMemoryConfig() {
    const res = await api('/memory/config');
    const d = await res.json();
    $('memoryEnabled').checked = !!d.enabled;
    $('memoryBaseURL').value = d.baseURL || '';
    // The key is returned masked; show the mask as a placeholder so the operator
    // knows a key is stored, and leave the field empty so an unchanged save does
    // not overwrite it (backend preserves the stored key on a masked/empty value).
    $('memoryApiKey').value = '';
    $('memoryApiKey').placeholder = d.apiKeyMasked || '';
    $('memoryWriteMode').value = d.writeMode || 'explicit';
    $('memoryRetrievalLimit').value = d.retrievalLimit ? String(d.retrievalLimit) : '';
    $('memoryInject').checked = !!d.inject;
    $('memoryMaxInjectTokens').value = d.maxInjectTokens ? String(d.maxInjectTokens) : '';
    $('memoryRedactSecrets').checked = d.redactSecrets !== false;
    $('memoryStoreSourceCode').checked = !!d.storeSourceCode;
    $('memoryFailOpen').checked = d.failOpen !== false;
    updateMemoryWriteModeWarning();
    refreshCustomSelects();
  }
  function updateMemoryWriteModeWarning() {
    const warn = $('memoryWriteModeWarning');
    if (!warn) return;
    warn.classList.toggle('hidden', $('memoryWriteMode').value !== 'automatic');
  }
  async function saveMemoryConfig() {
    const body = {
      enabled: $('memoryEnabled').checked,
      baseURL: $('memoryBaseURL').value.trim(),
      writeMode: $('memoryWriteMode').value,
      retrievalLimit: parseInt($('memoryRetrievalLimit').value, 10) || 0,
      inject: $('memoryInject').checked,
      maxInjectTokens: parseInt($('memoryMaxInjectTokens').value, 10) || 0,
      redactSecrets: $('memoryRedactSecrets').checked,
      storeSourceCode: $('memoryStoreSourceCode').checked,
      failOpen: $('memoryFailOpen').checked,
    };
    // Only send the API key when the operator typed a new one; an empty field
    // means "keep the stored key" (backend preserves it).
    const key = $('memoryApiKey').value.trim();
    if (key) body.apiKey = key;
    const res = await api('/memory/config', { method: 'POST', body: JSON.stringify(body) });
    const d = await res.json();
    if (d.success) { toast(t('memory.saved'), 'success'); loadMemoryConfig(); }
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  function renderPromptRules() {
    const c = $('promptFilterRules');
    if (!c) return;
    if (!promptRules.length) {
      c.innerHTML = '<small class="text-xs muted-text">' + escapeHtml(t('promptFilter.noRules')) + '</small>';
      return;
    }
    c.innerHTML = promptRules.map((r, i) => {
      const isContains = r.type === 'lines-containing';
      const typeLabel = isContains ? t('promptFilter.typeContains') : t('promptFilter.typeRegex');
      const matchPh = isContains ? t('promptFilter.matchPlaceholderContains') : t('promptFilter.matchPlaceholderRegex');
      const replaceRow = !isContains
        ? '<div class="rule-field"><label>' + escapeHtml(t('promptFilter.replace')) + '</label>' +
        '<input value="' + escapeAttr(r.replace || '') + '" data-rule-idx="' + i + '" data-rule-field="replace" placeholder="' + escapeAttr(t('promptFilter.emptyRemove')) + '" />' +
        '</div>'
        : '';
      return '<div class="rule-card' + (r.enabled ? '' : ' disabled') + '">' +
        '<div class="rule-header">' +
        '<label class="switch"><input type="checkbox" ' + (r.enabled ? 'checked' : '') + ' data-rule-toggle="' + i + '" /><span class="slider"></span></label>' +
        '<div class="rule-meta">' +
        '<input class="rule-name-input" value="' + escapeAttr(r.name || '') + '" data-rule-idx="' + i + '" data-rule-field="name" placeholder="' + escapeAttr(t('promptFilter.unnamed')) + '" />' +
        '<span class="rule-type">' + escapeHtml(typeLabel) + '</span>' +
        '</div>' +
        '<button class="rule-remove" data-rule-remove="' + i + '" type="button" aria-label="' + escapeAttr(t('common.remove')) + '">&times;</button>' +
        '</div>' +
        '<div class="rule-body">' +
        '<div class="rule-field"><label>' + escapeHtml(t('promptFilter.match')) + '</label>' +
        '<input value="' + escapeAttr(r.match || '') + '" data-rule-idx="' + i + '" data-rule-field="match" placeholder="' + escapeAttr(matchPh) + '" />' +
        '</div>' +
        replaceRow +
        '</div>' +
        '</div>';
    }).join('');
  }
  function addPromptRule(type) {
    promptRules.push({ id: 'rule-' + Date.now(), name: '', type, match: '', replace: '', enabled: true });
    renderPromptRules();
  }

  // Add-account modal templates
  var METHOD_ICONS = {
    builderid: 'fa-solid fa-id-card',
    iam: 'fa-solid fa-key',
    microsoft: 'fa-brands fa-microsoft',
    sso: 'fa-solid fa-shield-halved',
    local: 'fa-solid fa-folder-open',
    credentials: 'fa-solid fa-code',
    cookie: 'fa-solid fa-cookie-bite',
    kiro: 'fa-solid fa-building',
    apikey: 'fa-solid fa-key'
  };
  function methodCard(type, title, desc) {
    var icon = METHOD_ICONS[type] || 'fa-solid fa-circle-plus';
    return '<button type="button" class="method-card" data-method="' + escapeAttr(type) + '">' +
      '<span class="method-icon"><i class="' + icon + '" aria-hidden="true"></i></span>' +
      '<span class="method-body">' +
      '<span class="method-title">' + escapeHtml(title) + '</span>' +
      '<span class="method-desc">' + escapeHtml(desc) + '</span>' +
      '</span>' +
      '<span class="method-arrow" aria-hidden="true"><i class="fa-solid fa-chevron-right"></i></span>' +
      '</button>';
  }
  function showModal(type) {
    const modal = $('addModal');
    const title = $('modalTitle');
    const body = $('modalBody');
    if (type === 'add') modalAdd(title, body);
    else if (type === 'builderid') modalBuilderId(title, body);
    else if (type === 'iam') modalIam(title, body);
    else if (type === 'microsoft') openMicrosoftModal(title, body);
    else if (type === 'sso') modalSso(title, body);
    else if (type === 'local') modalLocal(title, body);
    else if (type === 'localdetect') modalLocalDetect(title, body);
    else if (type === 'credentials') modalCredentials(title, body);
    else if (type === 'cookie') modalCookie(title, body);
    else if (type === 'apikey') modalApiKey(title, body);
    else if (type === 'apikeybatch') modalApiKeyBatch(title, body);
    else if (type === 'kiro') modalKiro(title, body);
    if (!modal.classList.contains('active')) openDialog('addModal');
    enhanceCustomSelects(body);
  }
  function closeModal() {
    closeDialog('addModal');
    resetMicrosoftFlow(true);
    iamSession = '';
    kiroSsoSession = '';
    if (builderIdPollTimer) { clearTimeout(builderIdPollTimer); builderIdPollTimer = null; }
    if (kiroSsoPollTimer) { clearTimeout(kiroSsoPollTimer); kiroSsoPollTimer = null; }
    builderIdSession = '';
  }
  function modalAdd(title, body) {
    title.textContent = t('modal.addAccount');
    body.innerHTML =
      '<div class="method-list">' +
      methodCard('builderid', t('modal.builderIdTitle'), t('modal.builderIdDesc')) +
      methodCard('iam', t('modal.iamTitle'), t('modal.iamDesc')) +
      methodCard('kiro', t('modal.kiroTitle'), t('modal.kiroDesc')) +
      methodCard('microsoft', t('modal.microsoftTitle'), t('modal.microsoftDesc')) +
      methodCard('sso', t('modal.ssoTitle'), t('modal.ssoDesc')) +
      methodCard('localdetect', t('modal.localDetectTitle'), t('modal.localDetectDesc')) +
      methodCard('local', t('modal.localTitle'), t('modal.localDesc')) +
      methodCard('credentials', t('modal.credentialsTitle'), t('modal.credentialsDesc')) +
      methodCard('cookie', t('modal.cookieTitle'), t('modal.cookieDesc')) +
      methodCard('apikey', t('modal.apikeyTitle'), t('modal.apikeyDesc')) +
      methodCard('apikeybatch', t('modal.apikeyBatchTitle'), t('modal.apikeyBatchDesc')) +
      '</div>' +
      '<div class="modal-footer"><button class="btn btn-secondary" data-close-add="1" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>';
  }
  function modalBuilderId(title, body) {
    title.textContent = t('modal.builderIdTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.builderIdDesc')) + '</p>' +
      '<div id="builderIdStep1">' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label>' +
      '<input type="text" id="builderIdRegion" value="us-east-1" list="builderIdRegionList" autocomplete="off" />' +
      '<datalist id="builderIdRegionList"><option value="us-east-1"><option value="us-east-2"><option value="us-west-2"><option value="eu-central-1"><option value="eu-west-1"><option value="ap-northeast-1"><option value="ap-southeast-1"><option value="ap-south-1"></datalist></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="startBuilderIdBtn" type="button">' + escapeHtml(t('builderid.startLogin')) + '</button>' +
      '</div>' +
      '</div>' +
      '<div id="builderIdStep2" class="hidden">' +
      '<div class="message message-info message-center"><p class="builder-code" id="builderIdUserCode"></p><p class="text-xs mt-2">' + escapeHtml(t('builderid.verifyCode')) + '</p></div>' +
      '<div class="form-group mt-4"><label>' + escapeHtml(t('builderid.verifyUrl')) + '</label>' +
      '<div class="endpoint"><span id="builderIdVerifyUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="builderIdOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="builderIdCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '</div>' +
      '<p id="builderIdStatus" class="text-center text-sm mt-4 muted-text">' + escapeHtml(t('builderid.waiting')) + '</p>' +
      '<div class="modal-footer"><button class="btn btn-secondary" id="builderIdCancelBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>' +
      '</div>';
    $('startBuilderIdBtn').addEventListener('click', startBuilderIdLogin);
  }
  function modalIam(title, body) {
    title.textContent = t('modal.iamTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.iamDesc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.startUrl')) + '</label><input type="text" id="iamStartUrl" placeholder="https://xxx.awsapps.com/start" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label>' +
      '<input type="text" id="iamRegion" value="us-east-1" list="iamRegionList" autocomplete="off" />' +
      '<datalist id="iamRegionList"><option value="us-east-1"><option value="us-east-2"><option value="us-west-2"><option value="eu-central-1"><option value="eu-west-1"><option value="ap-northeast-1"><option value="ap-southeast-1"><option value="ap-south-1"></datalist></div>' +
      '<div id="iamStep2" class="hidden">' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.loginUrl')) + '</label>' +
      '<div class="endpoint"><span id="iamAuthUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="iamOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="iamCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '</div>' +
      '<p class="text-sm mt-3 success-text">' + escapeHtml(t('iam.completeLogin')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.callbackUrl')) + '</label><input type="text" id="iamCallback" placeholder="http://127.0.0.1:xxx/?code=..." /></div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="iamBtn" type="button">' + escapeHtml(t('builderid.startLogin')) + '</button>' +
      '</div>';
    $('iamBtn').addEventListener('click', startIamSso);
  }
  function openMicrosoftModal(title, body) {
    resetMicrosoftFlow(true);
    renderMicrosoftModal(title, body);
  }
  function renderMicrosoftModal(title, body) {
    title = title || $('modalTitle');
    body = body || $('modalBody');
    title.textContent = t('modal.microsoftTitle');

    if (microsoftSelectionId && microsoftProfiles.length) {
      body.innerHTML =
        '<p class="help-block">' + escapeHtml(t('microsoft.selectProfileDesc')) + '</p>' +
        '<fieldset class="microsoft-profile-list"><legend class="sr-only">' + escapeHtml(t('microsoft.selectProfileTitle')) + '</legend>' +
        microsoftProfiles.map((profile, index) => {
          const arn = String(profile.arn || '');
          const checked = arn === microsoftSelectedProfileArn || (!microsoftSelectedProfileArn && index === 0);
          return '<label class="microsoft-profile-card' + (checked ? ' selected' : '') + '">' +
            '<input type="radio" name="microsoftProfile" value="' + escapeAttr(arn) + '"' + (checked ? ' checked' : '') + ' />' +
            '<span class="microsoft-profile-body">' +
            '<span class="microsoft-profile-name">' + escapeHtml(profile.name || arn) + '</span>' +
            '<span class="microsoft-profile-arn font-mono">' + escapeHtml(arn) + '</span>' +
            (profile.region ? '<span class="microsoft-profile-region">' + escapeHtml(t('microsoft.profileRegion', profile.region)) + '</span>' : '') +
            '</span></label>';
        }).join('') +
        '</fieldset>' +
        '<div class="modal-footer">' +
        '<button class="btn btn-secondary" data-microsoft-back="1" type="button">' + escapeHtml(t('common.back')) + '</button>' +
        '<button class="btn btn-primary" id="microsoftSelectProfileBtn" type="button">' + escapeHtml(t('microsoft.selectProfile')) + '</button>' +
        '</div>';
      qsa('input[name="microsoftProfile"]', body).forEach(radio => radio.addEventListener('change', e => {
        microsoftSelectedProfileArn = e.target.value;
        qsa('.microsoft-profile-card', body).forEach(card => {
          const input = card.querySelector('input');
          card.classList.toggle('selected', Boolean(input && input.checked));
        });
      }));
      $('microsoftSelectProfileBtn').addEventListener('click', selectMicrosoftProfile);
      syncMicrosoftBusyUI();
      return;
    }

    const hasAuthorizeUrl = Boolean(microsoftAuthorizeUrl);
    const loginLabel = microsoftStage === 'microsoft' ? t('microsoft.providerStep') : t('microsoft.portalStep');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.microsoftDesc')) + '</p>' +
      (hasAuthorizeUrl ?
        '<div class="form-group"><label>' + escapeHtml(loginLabel) + '</label>' +
        '<div class="endpoint"><span id="microsoftAuthUrl" class="font-mono text-xs"></span></div>' +
        '<div class="flex gap-2 mt-2">' +
        '<button class="btn btn-sm btn-outline flex-1" id="microsoftOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
        '<button class="btn btn-sm btn-outline flex-1" id="microsoftCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
        '</div></div>' +
        '<div class="message message-info microsoft-callback-note"><p>' + escapeHtml(t('microsoft.callbackInstructions')) + '</p></div>' +
        '<div class="form-group mt-4"><label>' + escapeHtml(t('microsoft.callbackUrl')) + '</label>' +
        '<textarea id="microsoftCallback" class="font-mono microsoft-callback-input" placeholder="' + escapeAttr(t('microsoft.callbackPlaceholder')) + '"></textarea></div>'
        : '') +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-microsoft-back="1" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="microsoftBtn" type="button">' +
      escapeHtml(hasAuthorizeUrl ? t('microsoft.complete') : t('microsoft.start')) +
      '</button></div>';

    if (hasAuthorizeUrl) {
      $('microsoftAuthUrl').textContent = microsoftAuthorizeUrl;
      $('microsoftOpenBtn').addEventListener('click', () => {
        const opened = window.open(microsoftAuthorizeUrl, '_blank', 'noopener');
        if (opened) opened.opener = null;
      });
      $('microsoftCopyBtn').addEventListener('click', async () => {
        await copyText(microsoftAuthorizeUrl);
        toast(t('common.copied'), 'primary');
      });
    }
    $('microsoftBtn').addEventListener('click', hasAuthorizeUrl ? completeMicrosoftLogin : startMicrosoftLogin);
    syncMicrosoftBusyUI();
  }
  function modalSso(title, body) {
    title.textContent = t('modal.ssoTitle');
    body.innerHTML =
      '<div class="help-block">' +
      '<b>' + escapeHtml(t('sso.howToGet')) + '</b>' +
      '<ol class="steps-list">' +
      '<li>' + escapeHtml(t('sso.step1')) + ' <code class="code-inline">view.awsapps.com/start</code></li>' +
      '<li>' + escapeHtml(t('sso.step2')) + '</li>' +
      '<li>' + escapeHtml(t('sso.step3')) + ' <code class="code-inline">x-amz-sso_authn</code></li>' +
      '</ol>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('sso.tokenLabel')) + ' <small>' + escapeHtml(t('sso.tokenHint')) + '</small></label>' +
      '<textarea id="ssoToken" placeholder="' + escapeAttr(t('sso.tokenPlaceholder')) + '"></textarea></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label>' +
      '<input type="text" id="ssoRegion" value="us-east-1" list="ssoRegionList" autocomplete="off" />' +
      '<datalist id="ssoRegionList"><option value="us-east-1"><option value="us-east-2"><option value="us-west-2"><option value="eu-central-1"><option value="eu-west-1"><option value="ap-northeast-1"><option value="ap-southeast-1"><option value="ap-south-1"></datalist></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importSsoBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importSsoBtn').addEventListener('click', importSsoToken);
  }
  function modalApiKey(title, body) {
    title.textContent = t('modal.apiKeyTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.apiKeyDesc')) + '</p>' +
      '<div class="help-block">' +
      '<p>' + escapeHtml(t('apikey.hint')) + '</p>' +
      '<p class="font-mono text-xs">ksk_xxxxxxxx</p>' +
      '<p class="font-mono text-xs">ksk_xxxxxxxx|eu-central-1</p>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('apikey.label')) + '</label>' +
      '<textarea id="kiroApiKeyInput" class="font-mono" placeholder="' + escapeAttr(t('apikey.placeholder')) + '"></textarea></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + ' <small>' + escapeHtml(t('apikey.regionHint')) + '</small></label>' +
      '<input type="text" id="kiroApiKeyRegion" list="kiroApiKeyRegionList" value="us-east-1" placeholder="us-east-1" autocomplete="off" />' +
      '<datalist id="kiroApiKeyRegionList"><option value="us-east-1"><option value="us-east-2"><option value="us-west-2"><option value="eu-central-1"><option value="eu-west-1"><option value="ap-northeast-1"><option value="ap-southeast-1"><option value="ap-south-1"></datalist></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('apikey.nickname')) + '</label>' +
      '<input type="text" id="kiroApiKeyNickname" placeholder="' + escapeAttr(t('apikey.nicknamePlaceholder')) + '" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importApiKeyBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importApiKeyBtn').addEventListener('click', importApiKey);
  }

  function modalLocal(title, body) {
    title.textContent = t('modal.localTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.localDesc')) + '</p>' +
      '<div class="help-block">' +
      '<p><b>' + escapeHtml(t('local.fileLocation')) + '</b></p>' +
      '<p>' + escapeHtml(t('local.windows')) + ': <code class="code-inline">%USERPROFILE%\\.aws\\sso\\cache\\</code></p>' +
      '<p>' + escapeHtml(t('local.macosLinux')) + ': <code class="code-inline">~/.aws/sso/cache/</code></p>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('local.loginChannel')) + '</label>' +
      '<select id="localProvider">' +
      '<option value="BuilderId">' + escapeHtml(t('local.providerBuilderId')) + '</option>' +
      '<option value="Enterprise">' + escapeHtml(t('local.providerEnterprise')) + '</option>' +
      '<option value="Google">' + escapeHtml(t('local.providerGoogle')) + '</option>' +
      '<option value="Github">' + escapeHtml(t('local.providerGithub')) + '</option>' +
      '</select>' +
      '</div>' +
      '<div class="form-group">' +
      '<label>' + escapeHtml(t('local.tokenFile')) + ' <small>' + escapeHtml(t('local.tokenRequired')) + '</small></label>' +
      '<div class="input-row">' +
      '<textarea id="localTokenJson" placeholder="' + escapeAttr(t('local.pasteOrUpload')) + '" class="font-mono"></textarea>' +
      '<label class="btn btn-outline btn-sm">' + escapeHtml(t('local.upload')) +
      '<input type="file" accept=".json" id="localTokenFile" class="file-input-hidden" />' +
      '</label>' +
      '</div>' +
      '</div>' +
      '<div id="localClientGroup" class="form-group">' +
      '<label>' + escapeHtml(t('local.clientFile')) + ' <small>' + escapeHtml(t('local.clientRequired')) + '</small></label>' +
      '<div class="input-row">' +
      '<textarea id="localClientJson" placeholder="' + escapeAttr(t('local.pasteOrUpload')) + '" class="font-mono"></textarea>' +
      '<label class="btn btn-outline btn-sm">' + escapeHtml(t('local.upload')) +
      '<input type="file" accept=".json" id="localClientFile" class="file-input-hidden" />' +
      '</label>' +
      '</div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importLocalBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('localProvider').addEventListener('change', updateLocalFields);
    $('localTokenFile').addEventListener('change', e => loadLocalFile(e.target, 'localTokenJson'));
    $('localClientFile').addEventListener('change', e => loadLocalFile(e.target, 'localClientJson'));
    $('importLocalBtn').addEventListener('click', importLocalKiro);
  }
  // Auto-detect Kiro credentials from the local SSO cache (same machine only).
  function modalLocalDetect(title, body) {
    title.textContent = t('modal.localDetectTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.localDetectDesc')) + '</p>' +
      '<div id="localDetectStatus" class="help-block">' + escapeHtml(t('localDetect.scanning')) + '</div>' +
      '<div id="localDetectList"></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="localDetectImportBtn" type="button" disabled>' + escapeHtml(t('localDetect.importSelected')) + '</button>' +
      '</div>';
    $('localDetectImportBtn').addEventListener('click', importLocalDetected);
    scanLocalCache();
  }
  async function scanLocalCache() {
    const statusEl = $('localDetectStatus');
    const listEl = $('localDetectList');
    const importBtn = $('localDetectImportBtn');
    try {
      const res = await api('/auth/local-cache/scan', { method: 'GET' });
      const d = await res.json();
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      if (!d.available || !d.accounts || d.accounts.length === 0) {
        statusEl.textContent = t('localDetect.notFound');
        listEl.innerHTML = '';
        importBtn.disabled = true;
        return;
      }
      statusEl.innerHTML = escapeHtml(t('localDetect.found', d.count)) +
        ' <code class="code-inline">' + escapeHtml(d.cacheDir || '') + '</code>';
      listEl.innerHTML = d.accounts.map(a => {
        const label = (a.loginHint || a.provider || a.authMethod || a.fingerprint);
        const meta = formatAuthMethod(a.provider || a.authMethod) + ' · ' + a.region +
          (a.hasClient ? '' : ' · ' + t('localDetect.noClient'));
        const disabled = a.importable ? '' : 'disabled';
        const reason = a.importable ? '' : ' <small class="muted-text">(' + escapeHtml(a.reason || '') + ')</small>';
        return '<label class="export-row' + (a.importable ? '' : ' opacity-50') + '">' +
          '<input type="checkbox" ' + (a.importable ? 'checked' : '') + ' ' + disabled +
          ' data-detect-fp="' + escapeAttr(a.fingerprint) + '" />' +
          '<div class="export-row-text">' +
          '<div class="export-row-email">' + escapeHtml(label) + reason + '</div>' +
          '<div class="export-row-meta">' + escapeHtml(meta) + '</div>' +
          '</div></label>';
      }).join('');
      importBtn.disabled = false;
    } catch (e) {
      statusEl.textContent = (e && e.message) || t('common.failed');
      importBtn.disabled = true;
    }
  }
  async function importLocalDetected() {
    const fps = qsa('[data-detect-fp]:checked', $('localDetectList')).map(cb => cb.dataset.detectFp);
    if (fps.length === 0) return toastWarning(t('localDetect.selectAtLeastOne'));
    const importBtn = $('localDetectImportBtn');
    importBtn.disabled = true;
    try {
      const res = await api('/auth/local-cache/import', {
        method: 'POST', body: JSON.stringify({ fingerprints: fps })
      });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('localDetect.importSuccess', d.imported || 0));
        (d.results || []).forEach(r => { if (r.success && r.accountId) autoRefreshNewAccount(r.accountId); });
      } else {
        const firstErr = (d.results || []).find(r => r.error);
        toastError(t('common.failed') + (firstErr ? ': ' + firstErr.error : ''));
        importBtn.disabled = false;
      }
    } catch (e) {
      toastError((e && e.message) || t('common.failed'));
      importBtn.disabled = false;
    }
  }
  function modalCredentials(title, body) {
    title.textContent = t('modal.credentialsTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.credentialsDesc')) + '</p>' +
      '<p class="help-block">' + escapeHtml(t('credentials.batchHint')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('credentials.label')) + '</label>' +
      '<textarea id="credJson" class="font-mono" placeholder=\'[{"refreshToken":"xxx","provider":"BuilderID"}]&#10;or&#10;email----password----refreshToken----clientId----clientSecret\'></textarea>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importCredBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importCredBtn').addEventListener('click', importCredentials);
  }
  function modalCookie(title, body) {
    title.textContent = t('modal.cookieTitle');
    body.innerHTML =
      '<div class="help-block">' +
      '<p><b>' + escapeHtml(t('cookie.howToGet')) + '</b></p>' +
      '<ol class="steps-list">' +
      '<li>' + escapeHtml(t('cookie.step1')) + ' <a href="' + escapeAttr(t('cookie.link')) + '" target="_blank">' + escapeHtml(t('cookie.link')) + '</a></li>' +
      '<li>' + escapeHtml(t('cookie.step2')) + '</li>' +
      '<li>' + escapeHtml(t('cookie.step3')) + '</li>' +
      '</ol>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('cookie.provider')) + '</label>' +
      '<select id="cookieProvider">' +
      '<option value="Google">' + escapeHtml(t('cookie.google')) + '</option>' +
      '<option value="Github">' + escapeHtml(t('cookie.github')) + '</option>' +
      '</select>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('cookie.refreshToken')) + '</label>' +
      '<textarea id="cookieRefreshToken" class="font-mono" placeholder="' + escapeAttr(t('cookie.refreshTokenPlaceholder')) + '"></textarea>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importCookieBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importCookieBtn').addEventListener('click', importFromCookie);
  }
  function modalApiKey(title, body) {
    title.textContent = t('apikey.title');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('apikey.desc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('apikey.keyLabel')) + '</label>' +
      '<input type="password" id="apikeyInput" placeholder="' + escapeAttr(t('apikey.keyPlaceholder')) + '" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('apikey.nickname')) + '</label>' +
      '<input type="text" id="apikeyNickname" placeholder="' + escapeAttr(t('apikey.nicknamePlaceholder')) + '" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('apikey.authRegion')) + '</label>' +
      '<input type="text" id="apikeyAuthRegion" list="apikeyRegionList" value="us-east-1" placeholder="us-east-1" autocomplete="off" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('apikey.apiRegion')) + '</label>' +
      '<input type="text" id="apikeyApiRegion" list="apikeyRegionList" value="us-east-1" placeholder="us-east-1" autocomplete="off" /></div>' +
      '<datalist id="apikeyRegionList"><option value="us-east-1"><option value="us-east-2"><option value="us-west-2"><option value="eu-central-1"><option value="eu-west-1"><option value="ap-northeast-1"><option value="ap-southeast-1"><option value="ap-south-1"></datalist>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importApikeyBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importApikeyBtn').addEventListener('click', importApiKey);
  }
  function modalApiKeyBatch(title, body) {
    title.textContent = t('apikeyBatch.title');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('apikeyBatch.desc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('apikeyBatch.listLabel')) + '</label>' +
      '<textarea id="apikeyBatchList" class="font-mono" rows="8" placeholder="' + escapeAttr(t('apikeyBatch.listPlaceholder')) + '"></textarea>' +
      '<p class="help-block">' + escapeHtml(t('apikeyBatch.listHint')) + '</p></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('apikey.apiRegion')) + '</label>' +
      '<input type="text" id="apikeyBatchRegion" list="apikeyRegionList" value="us-east-1" placeholder="us-east-1" autocomplete="off" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importApikeyBatchBtn" type="button">' + escapeHtml(t('apikeyBatch.import')) + '</button>' +
      '</div>' +
      '<div id="apikeyBatchResults" class="mt-3"></div>';
    $('importApikeyBatchBtn').addEventListener('click', importApiKeysBatch);
  }
  function modalKiro(title, body) {
    title.textContent = t('modal.kiroTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.kiroDesc')) + '</p>' +
      '<div id="kiroStep1">' +
      '<div class="form-group"><label>' + escapeHtml(t('kiro.emailLabel')) + '</label>' +
      '<input type="email" id="kiroEmail" placeholder="' + escapeAttr(t('kiro.emailPlaceholder')) + '" /></div>' +
      '<p class="text-xs muted-text">' + escapeHtml(t('kiro.emailHint')) + '</p>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="startKiroBtn" type="button">' + escapeHtml(t('kiro.startLogin')) + '</button>' +
      '</div>' +
      '</div>' +
      '<div id="kiroStep2" class="hidden">' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.loginUrl')) + '</label>' +
      '<textarea id="kiroAuthUrl" readonly rows="3" class="w-full font-mono text-xs p-2"' +
      ' style="word-break:break-all; resize:none; border:1px solid var(--border); border-radius:var(--radius); background:var(--surface); color:var(--text);"></textarea>' +
      '</div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-primary btn-sm" id="kiroOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-outline btn-sm" id="kiroCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '<p id="kiroStatus" class="text-center text-sm mt-4 muted-text">' + escapeHtml(t('builderid.waiting')) + '</p>' +
      '<div class="modal-footer"><button class="btn btn-secondary" id="kiroCancelBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>' +
      '</div>';
    $('startKiroBtn').addEventListener('click', startKiroSso);
  }

  // Kiro SSO state
  var kiroSsoSession = '';
  var kiroSsoPollTimer = null;

  async function startKiroSso() {
    try {
      var email = $('kiroEmail').value.trim();
      var res = await api('/auth/kiro-sso/start', {
        method: 'POST',
        body: JSON.stringify({ loginHint: email || undefined })
      });
      var d = await res.json();
      if (d.sessionId) {
        kiroSsoSession = d.sessionId;
        $('kiroStep1').classList.add('hidden');
        $('kiroStep2').classList.remove('hidden');
        $('kiroAuthUrl').textContent = d.authorizeUrl;
        $('kiroOpenBtn').addEventListener('click', function() {
          window.open($('kiroAuthUrl').textContent, '_blank');
        });
        $('kiroCopyBtn').addEventListener('click', async function() {
          await copyText($('kiroAuthUrl').textContent);
          toast(t('common.copied'), 'primary');
        });
        $('kiroCancelBtn').addEventListener('click', cancelKiroSso);
        pollKiroSso(2);
      } else {
        toastError(t('common.failed') + ': ' + (d.error || ''));
      }
    } catch (e) {
      toastError(t('login.connectError'));
    }
  }

  function pollKiroSso(interval) {
    kiroSsoPollTimer = setTimeout(async function() {
      try {
        var res = await api('/auth/kiro-sso/poll', {
          method: 'POST',
          body: JSON.stringify({ sessionId: kiroSsoSession })
        });
        var d = await res.json();
        if (d.status === 'completed') {
          closeModal(); loadAccounts(); loadStats();
          toastPrimary(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
          autoRefreshNewAccount(d.account?.id);
        } else if (d.status === 'pending') {
          $('kiroStatus').textContent = t('builderid.waiting');
          pollKiroSso(d.interval || interval);
        } else {
          cancelKiroSso();
          toastError(t('common.failed') + ': ' + (d.error || t('kiro.authFailed')));
        }
      } catch (e) {
        cancelKiroSso();
        toastError(t('login.connectError'));
      }
    }, interval * 1000);
  }

  function cancelKiroSso() {
    if (kiroSsoPollTimer) { clearTimeout(kiroSsoPollTimer); kiroSsoPollTimer = null; }
    if (kiroSsoSession) {
      api('/auth/kiro-sso/cancel', {
        method: 'POST',
        body: JSON.stringify({ sessionId: kiroSsoSession })
      }).catch(function() {});
    }
    kiroSsoSession = '';
    showModal('add');
  }

  function updateLocalFields() {
    const p = $('localProvider').value;
    $('localClientGroup').classList.toggle('hidden', p === 'Google' || p === 'Github');
  }
  function loadLocalFile(input, targetId) {
    const file = input.files[0];
    if (!file) return;
    const r = new FileReader();
    r.onload = e => { $(targetId).value = e.target.result; };
    r.readAsText(file);
  }

  // Import handlers
  async function importApiKey() {
    const raw = ($('kiroApiKeyInput') && $('kiroApiKeyInput').value || '').trim();
    if (!raw) return toastWarning(t('apikey.missing'));
    let key = raw;
    let regionFromKey = '';
    if (raw.includes('|')) {
      const parts = raw.split('|');
      key = (parts[0] || '').trim();
      regionFromKey = (parts[1] || '').trim();
    }
    if (!key) return toastWarning(t('apikey.missing'));
    const region = regionFromKey || ($('kiroApiKeyRegion') && $('kiroApiKeyRegion').value.trim()) || 'us-east-1';
    const nickname = ($('kiroApiKeyNickname') && $('kiroApiKeyNickname').value.trim()) || '';
    const payload = {
      kiroApiKey: key,
      authMethod: 'api_key',
      region,
      nickname
    };
    try {
      const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('apikey.importSuccess') + ': ' + (d.account?.email || d.account?.id));
        autoRefreshNewAccount(d.account?.id);
      } else {
        toastError(t('common.failed') + ': ' + (d.error || ''));
      }
    } catch (e) {
      toastError(t('common.failed') + ': ' + (e.message || e));
    }
  }
  async function importLocalKiro() {
    const provider = $('localProvider').value;
    const tokenJson = $('localTokenJson').value.trim();
    const clientJson = $('localClientJson').value.trim();
    const isSocial = provider === 'Google' || provider === 'Github';
    if (!tokenJson) return toastWarning(t('local.tokenMissing'));
    let tokenData, clientData;
    try { tokenData = JSON.parse(tokenJson); } catch { return toastWarning(t('local.tokenInvalid')); }
    if (!tokenData.refreshToken) return toastWarning(t('local.refreshTokenMissing'));
    if (!isSocial) {
      if (!clientJson) return toastWarning(t('local.clientMissing'));
      try { clientData = JSON.parse(clientJson); } catch { return toastWarning(t('local.clientInvalid')); }
      if (!clientData.clientId || !clientData.clientSecret) return toastWarning(t('local.clientSecretMissing'));
    }
    const authMethod = clientData ? 'idc' : 'social';
    const payload = {
      refreshToken: tokenData.refreshToken,
      accessToken: tokenData.accessToken || '',
      clientId: clientData?.clientId || '',
      clientSecret: clientData?.clientSecret || '',
      region: tokenData.region || '',
      authMethod, provider
    };
    const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('local.importSuccess') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function importCredentials() {
    const raw = $('credJson').value.trim();
    if (!raw) { toastWarning(t('credentials.jsonError')); return; }
    let items;
    let skipped = 0;
    try {
      const json = JSON.parse(raw);
      const source = json.accounts && Array.isArray(json.accounts)
        ? json.accounts
        : (Array.isArray(json) ? json : [json]);
      items = source.map(normalizeCredentialRecord);
    } catch {
      const parsed = parseLineCredentials(raw);
      items = parsed.items;
      skipped = parsed.skipped;
      if (items.length === 0 && skipped === 0) {
        toastWarning(t('credentials.jsonError'));
        return;
      }
      if (items.length === 0) {
        toastWarning(t('credentials.lineParseAllSkipped', skipped));
        return;
      }
    }
    let ok = 0, fail = 0, newIds = [];
    for (const item of items) {
      const rawMethod = String(item.authMethod || '').trim();
      const rawProvider = String(item.provider || '').trim();
      const methodKey = rawMethod.toLowerCase();
      const providerKey = rawProvider.toLowerCase();
      const kiroApiKey = String(item.kiroApiKey || '').trim();
      const isApiKey = Boolean(kiroApiKey) || methodKey === 'api_key' || methodKey === 'apikey' ||
        (!item.refreshToken && String(item.accessToken || '').trim().startsWith('ksk_'));
      if (!isApiKey && !item.refreshToken) { fail++; continue; }
      if (isApiKey) {
        const payload = {
          id: item.id || '',
          email: item.email || '',
          userId: item.userId || '',
          nickname: item.nickname || '',
          kiroApiKey: kiroApiKey || item.accessToken || '',
          authMethod: 'api_key',
          provider: rawProvider || 'APIKey',
          region: item.region || 'us-east-1'
        };
        try {
          const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
          const d = await res.json();
          if (d.success) { ok++; if (d.account?.id) newIds.push(d.account.id); }
          else fail++;
        } catch { fail++; }
        continue;
      }
      const externalAliases = [
        'external_idp', 'external-idp', 'external', 'microsoft', 'm365', 'office365',
        'azure', 'azuread', 'azure-ad', 'azure_ad', 'entra', 'entra-id'
      ];
      const isExternalIdp = externalAliases.includes(methodKey) ||
        externalAliases.includes(providerKey) ||
        Boolean(item.tokenEndpoint || item.issuerUrl);
      let authMethod;
      if (isExternalIdp) authMethod = 'external_idp';
      else if (item.clientId && item.clientSecret) authMethod = 'idc';
      else if (methodKey === 'idc') authMethod = 'idc';
      else if (methodKey === 'social' || methodKey === 'google' || methodKey === 'github') authMethod = 'social';
      else authMethod = methodKey ? 'social' : '';
      let provider = isExternalIdp ? 'AzureAD' : rawProvider;
      if (!provider && authMethod === 'social') provider = 'Google';
      if (!provider && authMethod === 'idc') provider = 'BuilderId';
      const payload = {
        id: item.id || '',
        email: item.email || '',
        userId: item.userId || '',
        nickname: item.nickname || '',
        profileArn: item.profileArn || '',
        refreshToken: item.refreshToken,
        accessToken: item.accessToken || '',
        clientId: item.clientId || '',
        clientSecret: item.clientSecret || '',
        authMethod, provider,
        region: item.region || (isExternalIdp ? '' : 'us-east-1'),
        tokenEndpoint: item.tokenEndpoint || '',
        issuerUrl: item.issuerUrl || '',
        scopes: item.scopes || '',
        // Kiro Hosted SSO ("Your organization") carries these three; upstream's
        // Microsoft SSO flow does not. Dropping them makes a re-imported Entra
        // account come back as social and fail with 401 Bad credentials, so they
        // are forwarded alongside upstream's fields rather than replaced by them.
        idpClientId: item.idpClientId || '',
        loginHint: item.loginHint || '',
        idpTokenEndpoint: item.idpTokenEndpoint || ''
      };
      try {
        const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
        const d = await res.json();
        if (d.success) { ok++; if (d.account?.id) newIds.push(d.account.id); }
        else fail++;
      } catch { fail++; }
    }
    closeModal(); loadAccounts(); loadStats();
    let msg = t('sso.importSuccess', ok);
    if (fail > 0) msg += t('sso.importPartial', fail);
    if (skipped > 0) msg += t('credentials.lineParseSkipped', skipped);
    toastPrimary(msg, { duration: 5200 });
    newIds.forEach(autoRefreshNewAccount);
  }
  function normalizeCredentialRecord(record) {
    const source = record && typeof record === 'object' ? record : {};
    const credentials = source.credentials && typeof source.credentials === 'object'
      ? source.credentials
      : {};
    const value = key => Object.prototype.hasOwnProperty.call(credentials, key)
      ? credentials[key]
      : source[key];
    return {
      id: value('id'),
      email: value('email'),
      userId: value('userId'),
      nickname: value('nickname'),
      profileArn: value('profileArn'),
      accessToken: value('accessToken'),
      refreshToken: value('refreshToken'),
      kiroApiKey: value('kiroApiKey'),
      clientId: value('clientId'),
      clientSecret: value('clientSecret'),
      authMethod: value('authMethod'),
      provider: value('provider') || source.idp,
      region: value('region'),
      tokenEndpoint: value('tokenEndpoint'),
      issuerUrl: value('issuerUrl'),
      scopes: value('scopes'),
      // Kiro Hosted SSO fields. Without them a re-imported Entra account loses
      // its IdP identity and comes back as social (401 Bad credentials).
      idpClientId: value('idpClientId'),
      loginHint: value('loginHint'),
      idpTokenEndpoint: value('idpTokenEndpoint'),
      expiresAt: value('expiresAt')
    };
  }
  function parseLineCredentials(text) {
    const items = [];
    let skipped = 0;
    for (const line of text.split(/\r?\n/)) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      let parts;
      if (trimmed.includes('----')) {
        parts = trimmed.split('----').map(s => s.trim());
      } else if (trimmed.includes('\t')) {
        parts = trimmed.split(/\t+/).map(s => s.trim());
      } else {
        parts = trimmed.split(/\s+/).map(s => s.trim());
      }
      if (parts.length < 5) { skipped++; continue; }
      const refreshToken = parts[2];
      if (!refreshToken) { skipped++; continue; }
      items.push({
        refreshToken,
        clientId: parts[3],
        clientSecret: parts[4],
      });
    }
    return { items, skipped };
  }
  async function importFromCookie() {
    const refreshToken = $('cookieRefreshToken').value.trim();
    if (!refreshToken) return toastWarning(t('cookie.refreshTokenMissing'));
    const provider = $('cookieProvider').value;
    const payload = { refreshToken, accessToken: '', clientId: '', clientSecret: '', authMethod: 'social', provider };
    const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('cookie.importSuccess') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function importApiKey() {
    const key = $('apikeyInput').value.trim();
    if (!key) return toastWarning(t('apikey.keyRequired'));
    const payload = {
      authMethod: 'api_key',
      kiroApiKey: key,
      nickname: $('apikeyNickname').value.trim() || '',
      region: $('apikeyApiRegion').value.trim() || 'us-east-1',
      authRegion: $('apikeyAuthRegion').value.trim() || 'us-east-1',
      apiRegion: $('apikeyApiRegion').value.trim() || 'us-east-1',
      enabled: true
    };
    const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('apikey.success') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function importApiKeysBatch() {
    const raw = $('apikeyBatchList').value.trim();
    if (!raw) { toastWarning(t('apikeyBatch.listRequired')); return; }
    const region = $('apikeyBatchRegion').value.trim() || 'us-east-1';
    const btn = $('importApikeyBatchBtn');
    btn.disabled = true;
    const dismiss = toast(t('apikeyBatch.processing'), 'info', { duration: 0 });
    try {
      const res = await api('/auth/apikeys-batch', {
        method: 'POST',
        body: JSON.stringify({ keys: raw, region, authRegion: region, apiRegion: region })
      });
      const d = await res.json();
      dismiss();
      if (!d.success) {
        toast(t('common.failed') + ': ' + (d.error || ''), 'error');
        return;
      }
      renderApiKeyBatchResults(d);
      toast(t('apikeyBatch.summary', d.imported || 0, d.skipped || 0, d.total || 0),
        (d.imported > 0) ? 'success' : 'warning');
      loadAccounts(); loadStats();
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    } finally {
      btn.disabled = false;
    }
  }
  function renderApiKeyBatchResults(d) {
    const box = $('apikeyBatchResults');
    if (!box) return;
    const rows = (d.results || []).map(r => {
      let status, cls;
      if (r.skipped) { status = '⊘ ' + t('apikeyBatch.skipped'); cls = 'warning-text'; }
      else if (r.error && !r.imported) { status = '✗ ' + r.error; cls = 'error-text'; }
      else if (r.imported) {
        const credit = r.infoOk
          ? (formatCredit(r.usageCurrent) + ' / ' + formatCredit(r.usageLimit))
          : t('apikeyBatch.infoUnavailable');
        const email = r.email ? ' ' + escapeHtml(r.email) : '';
        status = '✓ ' + escapeHtml(credit) + email;
        cls = 'success-text';
      } else { status = '✗'; cls = 'error-text'; }
      return '<div class="test-log-line"><span class="font-mono text-xs">' + escapeHtml(r.maskedKey || '') +
        '</span> <span class="' + cls + '">' + status + '</span></div>';
    }).join('');
    box.innerHTML = rows || '<p class="help-block">' + escapeHtml(t('apikeyBatch.noResults')) + '</p>';
  }
  function formatCredit(n) {
    if (typeof n !== 'number' || !isFinite(n)) return '0';
    return Number.isInteger(n) ? String(n) : n.toFixed(1);
  }
  async function importSsoToken() {
    const res = await api('/auth/sso-token', {
      method: 'POST', body: JSON.stringify({
        bearerToken: $('ssoToken').value,
        region: $('ssoRegion').value
      })
    });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      const count = d.accounts?.length || 0;
      const errs = d.errors?.length || 0;
      let msg = t('sso.importSuccess', count);
      if (errs > 0) msg += t('sso.importPartial', errs);
      toastPrimary(msg, { duration: 5200 });
      if (d.accounts) d.accounts.forEach(a => autoRefreshNewAccount(a.id));
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function startBuilderIdLogin() {
    const region = $('builderIdRegion').value || 'us-east-1';
    const res = await api('/auth/builderid/start', { method: 'POST', body: JSON.stringify({ region }) });
    const d = await res.json();
    if (d.sessionId) {
      builderIdSession = d.sessionId;
      $('builderIdUserCode').textContent = d.userCode;
      $('builderIdVerifyUrl').textContent = d.verificationUri;
      $('builderIdStep1').classList.add('hidden');
      $('builderIdStep2').classList.remove('hidden');
      $('builderIdOpenBtn').addEventListener('click', () => window.open($('builderIdVerifyUrl').textContent, '_blank'));
      $('builderIdCopyBtn').addEventListener('click', async () => {
        await copyText($('builderIdVerifyUrl').textContent);
        toast(t('common.copied'), 'primary');
      });
      $('builderIdCancelBtn').addEventListener('click', cancelBuilderIdLogin);
      pollBuilderIdAuth(d.interval || 5);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  function pollBuilderIdAuth(interval) {
    builderIdPollTimer = setTimeout(async () => {
      try {
        const res = await api('/auth/builderid/poll', { method: 'POST', body: JSON.stringify({ sessionId: builderIdSession }) });
        const d = await res.json();
        if (d.completed) {
          closeModal(); loadAccounts(); loadStats();
          toastPrimary(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
          autoRefreshNewAccount(d.account?.id);
        } else if (d.success && !d.completed) {
          $('builderIdStatus').textContent = t('builderid.waiting');
          pollBuilderIdAuth(d.interval || interval);
        } else {
          toastError(t('common.failed') + ': ' + (d.error || ''));
          cancelBuilderIdLogin();
        }
      } catch (e) {
        // Server restarted / network dropped mid-poll: surface it and reset the
        // modal instead of leaving the poll loop dead and the UI stuck on
        // "waiting" with no way out.
        toastError(t('login.connectError'));
        cancelBuilderIdLogin();
      }
    }, interval * 1000);
  }
  function cancelBuilderIdLogin() {
    if (builderIdPollTimer) { clearTimeout(builderIdPollTimer); builderIdPollTimer = null; }
    builderIdSession = '';
    showModal('add');
  }
  async function startIamSso() {
    // Wrap the whole flow: a dropped connection mid-request must surface an
    // error and re-enable the button, not throw uncaught and leave it stuck.
    try {
      if (iamSession) {
        const res = await api('/auth/iam-sso/complete', {
          method: 'POST', body: JSON.stringify({
            sessionId: iamSession, callbackUrl: $('iamCallback').value
          })
        });
        const d = await res.json();
        if (d.success) {
          closeModal(); loadAccounts(); loadStats();
          toastPrimary(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
          autoRefreshNewAccount(d.account?.id);
        } else toastError(t('common.failed') + ': ' + (d.error || ''));
      } else {
        const res = await api('/auth/iam-sso/start', {
          method: 'POST', body: JSON.stringify({
            startUrl: $('iamStartUrl').value, region: $('iamRegion').value
          })
        });
        const d = await res.json();
        if (d.authorizeUrl) {
          iamSession = d.sessionId;
          $('iamAuthUrl').textContent = d.authorizeUrl;
          $('iamStep2').classList.remove('hidden');
          $('iamBtn').textContent = t('iam.complete');
          $('iamOpenBtn').addEventListener('click', () => window.open($('iamAuthUrl').textContent, '_blank'));
          $('iamCopyBtn').addEventListener('click', async () => {
            await copyText($('iamAuthUrl').textContent);
            toast(t('common.copied'), 'primary');
          });
        } else toastError(t('common.failed') + ': ' + (d.error || ''));
      }
    } catch (e) {
      toastError(t('login.connectError'));
    }
  }
  function cancelMicrosoftServerSession(sessionId, selectionId) {
    if (!sessionId && !selectionId) return;
    api('/auth/microsoft-sso/cancel', {
      method: 'POST',
      body: JSON.stringify({
        sessionId: sessionId || '',
        selectionId: selectionId || ''
      })
    }).catch(() => {});
  }
  function resetMicrosoftFlow(notifyServer) {
    const sessionId = microsoftSession;
    const selectionId = microsoftSelectionId;
    microsoftGeneration++;
    microsoftSession = '';
    microsoftSelectionId = '';
    microsoftStage = 'kiro';
    microsoftAuthorizeUrl = '';
    microsoftProfiles = [];
    microsoftSelectedProfileArn = '';
    microsoftBusy = false;
    if (notifyServer) cancelMicrosoftServerSession(sessionId, selectionId);
  }
  function syncMicrosoftBusyUI() {
    const loginAction = $('microsoftBtn');
    if (loginAction) {
      loginAction.disabled = microsoftBusy;
      loginAction.textContent = microsoftBusy
        ? t('microsoft.processing')
        : (microsoftAuthorizeUrl ? t('microsoft.complete') : t('microsoft.start'));
    }
    const profileAction = $('microsoftSelectProfileBtn');
    if (profileAction) {
      profileAction.disabled = microsoftBusy;
      profileAction.textContent = microsoftBusy ? t('microsoft.processing') : t('microsoft.selectProfile');
    }
    qsa('[data-microsoft-back]', $('modalBody')).forEach(button => {
      button.disabled = microsoftBusy;
    });
  }
  async function startMicrosoftLogin() {
    if (microsoftBusy) return;
    microsoftBusy = true;
    syncMicrosoftBusyUI();
    const generation = microsoftGeneration;
    try {
      const res = await api('/auth/microsoft-sso/start', {
        method: 'POST',
        body: JSON.stringify({})
      });
      const d = await res.json().catch(() => ({}));
      if (generation !== microsoftGeneration) {
        cancelMicrosoftServerSession(d.sessionId || '', d.selectionId || '');
        return;
      }
      if (!res.ok || !d.sessionId || !d.authorizeUrl) {
        toastError(t('common.failed') + ': ' + (d.error || res.statusText || ''));
        return;
      }
      microsoftSession = d.sessionId;
      microsoftAuthorizeUrl = d.authorizeUrl;
      microsoftStage = 'kiro';
      microsoftBusy = false;
      renderMicrosoftModal();
    } catch (e) {
      if (generation === microsoftGeneration) {
        toastError(t('common.failed') + ': ' + (e.message || ''));
      }
    } finally {
      if (generation === microsoftGeneration && microsoftBusy) {
        microsoftBusy = false;
        syncMicrosoftBusyUI();
      }
    }
  }
  async function completeMicrosoftLogin() {
    if (microsoftBusy) return;
    const callback = ($('microsoftCallback')?.value || '').trim();
    if (!callback) {
      toastWarning(t('microsoft.callbackRequired'));
      $('microsoftCallback')?.focus();
      return;
    }
    microsoftBusy = true;
    syncMicrosoftBusyUI();
    const generation = microsoftGeneration;
    const sessionId = microsoftSession;
    try {
      const res = await api('/auth/microsoft-sso/complete', {
        method: 'POST',
        body: JSON.stringify({
          sessionId: microsoftSession,
          callbackUrl: callback
        })
      });
      const d = await res.json().catch(() => ({}));
      if (generation !== microsoftGeneration) {
        cancelMicrosoftServerSession(sessionId, d.selectionId || '');
        return;
      }
      if (!res.ok || d.error) {
        toastError(t('common.failed') + ': ' + (d.error || res.statusText || ''));
        return;
      }
      if (d.requiresProfileSelection && d.selectionId && Array.isArray(d.profiles) && d.profiles.length) {
        microsoftSelectionId = d.selectionId;
        microsoftProfiles = d.profiles;
        microsoftSelectedProfileArn = String(d.profiles[0]?.arn || '');
        microsoftBusy = false;
        renderMicrosoftModal();
        return;
      }
      if (d.account) {
        finishMicrosoftLogin(d.account, d.warning);
        return;
      }
      if (d.stage === 'microsoft' && d.authorizeUrl) {
        microsoftStage = 'microsoft';
        microsoftAuthorizeUrl = d.authorizeUrl;
        microsoftBusy = false;
        renderMicrosoftModal();
        return;
      }
      toastError(t('common.failed') + ': ' + (d.error || t('microsoft.invalidResponse')));
    } catch (e) {
      if (generation === microsoftGeneration) {
        toastError(t('common.failed') + ': ' + (e.message || ''));
      }
    } finally {
      if (generation === microsoftGeneration && microsoftBusy) {
        microsoftBusy = false;
        syncMicrosoftBusyUI();
      }
    }
  }
  async function selectMicrosoftProfile() {
    if (microsoftBusy) return;
    const selected = qsa('input[name="microsoftProfile"]:checked', $('modalBody'))[0];
    const profileArn = (selected?.value || microsoftSelectedProfileArn || '').trim();
    if (!profileArn) {
      toastWarning(t('microsoft.profileRequired'));
      return;
    }
    microsoftBusy = true;
    syncMicrosoftBusyUI();
    const generation = microsoftGeneration;
    try {
      const res = await api('/auth/microsoft-sso/select-profile', {
        method: 'POST',
        body: JSON.stringify({
          selectionId: microsoftSelectionId,
          profileArn
        })
      });
      const d = await res.json().catch(() => ({}));
      if (generation !== microsoftGeneration) return;
      if (!res.ok || !d.account) {
        toastError(t('common.failed') + ': ' + (d.error || res.statusText || ''));
        return;
      }
      finishMicrosoftLogin(d.account, d.warning);
    } catch (e) {
      if (generation === microsoftGeneration) {
        toastError(t('common.failed') + ': ' + (e.message || ''));
      }
    } finally {
      if (generation === microsoftGeneration && microsoftBusy) {
        microsoftBusy = false;
        syncMicrosoftBusyUI();
      }
    }
  }
  function finishMicrosoftLogin(account, warning) {
    resetMicrosoftFlow(false);
    closeModal();
    loadAccounts();
    loadStats();
    toastPrimary(t('microsoft.success') + ': ' + (account?.email || account?.id || ''));
    if (warning) toastWarning(String(warning));
    autoRefreshNewAccount(account?.id);
  }
  async function autoRefreshNewAccount(id) {
    if (!id) return;
    try { await api('/accounts/' + id + '/refresh', { method: 'POST' }); } catch (e) { }
    loadAccounts();
  }

  // Export modal
  function showExportModal() {
    if (!accountsData.length) return toastWarning(t('accounts.empty'));
    exportSelectedIds = new Set(accountsData.map(a => a.id));
    renderExportModal();
    openDialog('exportModal');
  }
  function closeExportModal() { closeDialog('exportModal'); }
  function renderExportModal() {
    const body = $('exportBody');
    const all = exportSelectedIds.size === accountsData.length;
    body.innerHTML =
      '<div class="flex items-center justify-between mb-3">' +
      '<span class="text-sm muted-text">' + escapeHtml(t('export.selected', exportSelectedIds.size)) + '</span>' +
      '<button class="btn btn-sm btn-outline" id="exportToggleAllBtn" type="button">' + escapeHtml(all ? t('export.deselectAll') : t('export.selectAll')) + '</button>' +
      '</div>' +
      '<div class="export-list">' +
      accountsData.map(a => {
        const checked = exportSelectedIds.has(a.id);
        return '<label class="export-row' + (checked ? ' selected' : '') + '">' +
          '<input type="checkbox" ' + (checked ? 'checked' : '') + ' data-export-toggle="' + escapeAttr(a.id) + '" />' +
          '<div class="export-row-text">' +
          '<div class="export-row-email">' + escapeHtml(getDisplayEmail(a.email, a.id)) + '</div>' +
          '<div class="export-row-meta">' + escapeHtml(formatAuthMethod(a.provider || a.authMethod)) + ' · ' + escapeHtml(formatSubscriptionLabel(a.subscriptionType)) + '</div>' +
          '</div>' +
          '</label>';
      }).join('') +
      '</div>' +
      '<div id="exportJsonPreview" class="hidden mb-3"><textarea id="exportJsonText" readonly class="font-mono"></textarea></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="exportCloseBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button>' +
      '<button class="btn btn-outline" id="exportShowJsonBtn" type="button">' + escapeHtml(t('export.showJson')) + '</button>' +
      '<button class="btn btn-outline" id="exportCopyJsonBtn" type="button">' + escapeHtml(t('export.copyJson')) + '</button>' +
      '<button class="btn btn-primary" id="exportDownloadBtn" type="button">' + escapeHtml(t('export.downloadJson')) + '</button>' +
      '</div>';
    $('exportToggleAllBtn').addEventListener('click', () => {
      if (exportSelectedIds.size === accountsData.length) exportSelectedIds.clear();
      else exportSelectedIds = new Set(accountsData.map(a => a.id));
      renderExportModal();
    });
    $('exportCloseBtn').addEventListener('click', closeExportModal);
    $('exportShowJsonBtn').addEventListener('click', exportShowJson);
    $('exportCopyJsonBtn').addEventListener('click', exportCopyJson);
    $('exportDownloadBtn').addEventListener('click', exportDownloadJson);
    qsa('[data-export-toggle]', body).forEach(cb => cb.addEventListener('change', e => {
      const id = e.target.dataset.exportToggle;
      if (exportSelectedIds.has(id)) exportSelectedIds.delete(id);
      else exportSelectedIds.add(id);
      renderExportModal();
    }));
  }
  async function getExportData() {
    if (exportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return null; }
    const res = await api('/export', { method: 'POST', body: JSON.stringify({ ids: Array.from(exportSelectedIds) }) });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      toastError(t('common.failed') + ': ' + (err.error || t('common.unknownError')));
      return null;
    }
    return res.json();
  }
  async function exportShowJson() {
    const data = await getExportData();
    if (!data) return;
    $('exportJsonPreview').classList.remove('hidden');
    $('exportJsonText').value = JSON.stringify(data, null, 2);
  }
  async function exportCopyJson() {
    if (exportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return; }
    const jsonPromise = getExportData().then(data => {
      if (!data) throw new Error('no-data');
      const filtered = (data.accounts || []).map(credentialImportPayloadFromExportAccount);
      return JSON.stringify(filtered, null, 2);
    });
    try {
      await copyText(jsonPromise);
      toast(t('export.copied'), 'primary');
    } catch (e) {
      if (e && e.message !== 'no-data') toastError(t('common.failed'));
    }
  }
  async function exportDownloadJson() {
    const data = await getExportData();
    if (!data) return;
    downloadJson('kiro-accounts-' + todayStamp() + '.json', data);
  }

  // Version and update
  function renderVersionBadge() {
    const badge = $('versionBadge');
    if (badge && currentVersion) badge.textContent = currentVersion.replace(/^v/i, '');
  }
  async function loadVersion() {
    try {
      const res = await api('/version');
      const d = await res.json();
      currentVersion = d.version || '';
      renderVersionBadge();
    } catch (e) { }
  }
  function compareVersions(a, b) {
    const pa = a.split('.').map(Number);
    const pb = b.split('.').map(Number);
    for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
      const na = pa[i] || 0, nb = pb[i] || 0;
      if (na > nb) return 1;
      if (na < nb) return -1;
    }
    return 0;
  }
  function setUpdateButtonLoading(loading) {
    const btn = $('checkUpdateBtn');
    if (!btn) return;
    btn.disabled = loading;
    if (loading) btn.setAttribute('aria-busy', 'true');
    else btn.removeAttribute('aria-busy');
    const label = btn.querySelector('[data-update-label]');
    const icon = btn.querySelector('i');
    if (label) label.textContent = t(loading ? 'update.checking' : 'update.check');
    if (icon) icon.classList.toggle('fa-spin', loading);
  }
  async function checkUpdate(manual) {
    if (manual) setUpdateButtonLoading(true);
    try {
      if (!currentVersion) await loadVersion();
      const current = currentVersion.replace(/^v/i, '');
      if (!current) throw new Error('Current version missing');
      const res = await fetch('https://raw.githubusercontent.com/Quorinex/Kiro-Go/main/version.json?t=' + Date.now());
      if (!res.ok) throw new Error('Fetch failed');
      const d = await res.json();
      const latest = (d.version || '').replace(/^v/i, '');
      if (!latest) throw new Error('Latest version missing');
      if (latest && latest !== current && compareVersions(latest, current) > 0) {
        if (manual) showUpdateModal(latest, d.download, d.changelog);
        else showUpdateToast('available', current, latest);
      } else if (manual) {
        showUpdateToast('current', current, latest || current);
      }
    } catch (e) {
      if (manual) showUpdateToast('error', '', '');
    } finally {
      if (manual) setUpdateButtonLoading(false);
    }
  }
  function showUpdateToast(status, current, latest) {
    if (status === 'available') {
      toast(t('update.availableToast') + (latest ? ': ' + latest : ''), 'warning', {
        icon: 'fa-solid fa-arrow-up',
        duration: 5200,
        onClick: function () { checkUpdate(true); }
      });
      return;
    }
    if (status === 'current') {
      toast(t('update.noUpdatesToast'), 'success', {
        icon: 'fa-solid fa-circle-check',
        duration: 3600
      });
      return;
    }
    toast(t('update.checkFailed'), 'error', {
      icon: 'fa-solid fa-triangle-exclamation',
      duration: 4200
    });
  }
  function showUpdateModal(version, url, changelog) {
    const current = currentVersion.replace(/^v/i, '');
    $('updateBody').innerHTML =
      '<div class="update-shell">' +
      '<div class="update-hero">' +
      '<div class="update-result-icon update-result-info"><i class="fa-solid fa-arrow-up"></i></div>' +
      '<div>' +
      '<h3 class="update-hero-title">' + escapeHtml(t('update.newVersion')) + '</h3>' +
      '<p class="update-hero-copy">' + escapeHtml(t('update.newVersionMessage')) + '</p>' +
      '</div>' +
      '</div>' +
      '<div class="update-version-grid">' +
      '<div class="update-version-card update-version-card-current"><p class="update-version-label">' + escapeHtml(t('update.current')) + '</p><p class="update-version-value update-version-value-current">' + escapeHtml(current) + '</p></div>' +
      '<div class="update-version-card update-version-card-latest"><p class="update-version-label">' + escapeHtml(t('update.latest')) + '</p><p class="update-version-value update-version-value-success">' + escapeHtml(version) + '</p></div>' +
      '</div>' +
      (changelog ? '<div class="update-notes"><p class="update-notes-title">' + escapeHtml(t('update.changelog')) + '</p><p class="update-notes-body">' + escapeHtml(changelog) + '</p></div>' : '') +
      '<div class="update-actions"><a href="' + escapeAttr(url) + '" target="_blank" rel="noopener" class="btn btn-primary">' + escapeHtml(t('update.goDownload')) + '</a></div>' +
      '</div>';
    openDialog('updateModal');
  }
  function showUpdateStatusModal(status, title, message, latest) {
    const current = currentVersion.replace(/^v/i, '');
    const isError = status === 'error';
    $('updateBody').innerHTML =
      '<div class="update-shell">' +
      '<div class="text-center mb-5">' +
      '<div class="update-result-icon update-status-icon update-result-' + (isError ? 'error' : 'success') + '">' +
      '<i class="fa-solid ' + (isError ? 'fa-triangle-exclamation' : 'fa-circle-check') + '"></i>' +
      '</div>' +
      '<p class="text-base font-semibold ' + (isError ? 'danger-text' : 'success-text') + '">' + escapeHtml(title) + '</p>' +
      '<p class="text-sm mt-2 muted-text">' + escapeHtml(message) + '</p>' +
      '</div>' +
      '<div class="update-version-grid">' +
      '<div class="update-version-card update-version-card-current"><p class="update-version-label">' + escapeHtml(t('update.current')) + '</p><p class="update-version-value update-version-value-current">' + escapeHtml(current || '-') + '</p></div>' +
      '<div class="update-version-card' + (!isError ? ' update-version-card-latest' : '') + '"><p class="update-version-label">' + escapeHtml(t('update.latest')) + '</p><p class="update-version-value' + (!isError ? ' update-version-value-success' : '') + '">' + escapeHtml(latest || '-') + '</p></div>' +
      '</div>' +
      '</div>';
    openDialog('updateModal');
  }
  function closeUpdateModal() { closeDialog('updateModal'); }

  // Console (realtime logs)
  let consoleSource = null;
  let consolePaused = false;
  let consoleAutoscroll = true;
  let consoleFilter = 'all';
  let consoleQueue = [];
  let consoleRafScheduled = false;
  const CONSOLE_MAX_LINES = 2000;
  const consoleLevelRank = { debug: 0, info: 1, warn: 2, error: 3 };
  // Older server builds prefixed each line with "LEVEL  YYYY/MM/DD HH:MM:SS ";
  // strip it so the web console doesn't render the level/timestamp twice.
  const consolePrefixRe = /^(?:DEBUG|INFO|WARN|ERROR)\s+\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2}\s+/;

  function consoleSetStatus(state) {
    const el = $('consoleStatus');
    const txt = $('consoleStatusText');
    if (!el || !txt) return;
    el.dataset.state = state;
    txt.textContent = t('console.' + state);
  }

  function consoleBuildLine(entry) {
    const level = entry.level || 'info';
    const line = document.createElement('div');
    line.className = 'console-line console-' + level;
    line.dataset.level = level;
    const ts = new Date(entry.ts || Date.now());
    const hh = String(ts.getHours()).padStart(2, '0');
    const mm = String(ts.getMinutes()).padStart(2, '0');
    const ss = String(ts.getSeconds()).padStart(2, '0');

    const tsEl = document.createElement('span');
    tsEl.className = 'console-ts';
    tsEl.textContent = hh + ':' + mm + ':' + ss;
    const badgeEl = document.createElement('span');
    badgeEl.className = 'console-badge';
    badgeEl.textContent = level.toUpperCase();
    const textEl = document.createElement('span');
    textEl.className = 'console-text';
    textEl.textContent = String(entry.text || '').replace(consolePrefixRe, '');

    line.appendChild(tsEl);
    line.appendChild(badgeEl);
    line.appendChild(textEl);
    if (consoleFilter !== 'all' && consoleLevelRank[level] < consoleLevelRank[consoleFilter]) {
      line.classList.add('console-hidden');
    }
    return line;
  }

  // Coalesce bursts: queued entries are flushed to the DOM once per animation
  // frame via a single DocumentFragment, so render cost stays bounded (~60fps)
  // no matter how fast log lines arrive.
  function consoleFlushQueue() {
    consoleRafScheduled = false;
    const out = $('consoleOutput');
    if (!out) { consoleQueue.length = 0; return; }
    if (!consoleQueue.length) return;

    const batch = consoleQueue;
    consoleQueue = [];
    const frag = document.createDocumentFragment();
    for (let i = 0; i < batch.length; i++) frag.appendChild(consoleBuildLine(batch[i]));
    out.appendChild(frag);
    while (out.childElementCount > CONSOLE_MAX_LINES) out.removeChild(out.firstChild);
    if (consoleAutoscroll) out.scrollTop = out.scrollHeight;
  }

  function consoleAppend(entry) {
    consoleQueue.push(entry);
    if (consoleQueue.length > CONSOLE_MAX_LINES) {
      consoleQueue.splice(0, consoleQueue.length - CONSOLE_MAX_LINES);
    }
    if (!consoleRafScheduled) {
      consoleRafScheduled = true;
      requestAnimationFrame(consoleFlushQueue);
    }
  }

  function consoleApplyFilter() {
    qsa('.console-line', $('consoleOutput')).forEach(line => {
      const hide = consoleFilter !== 'all' && consoleLevelRank[line.dataset.level] < consoleLevelRank[consoleFilter];
      line.classList.toggle('console-hidden', hide);
    });
  }

  function openConsole() {
    if (consoleSource) return;
    const out = $('consoleOutput');
    if (out) out.innerHTML = '';
    consoleQueue.length = 0;
    consoleSetStatus('connecting');
    // Sync the active level selector.
    api('/logs/level').then(r => r.ok ? r.json() : null).then(d => {
      if (d && d.level) $('consoleLevel').value = d.level;
      refreshCustomSelects($('tabConsole'));
    }).catch(() => { });
    // EventSource authenticates via the admin_password cookie (no headers).
    const src = new EventSource('/admin/api/logs/stream');
    consoleSource = src;
    src.onopen = () => consoleSetStatus('connected');
    src.onmessage = ev => {
      if (consolePaused) return;
      let entry;
      try { entry = JSON.parse(ev.data); } catch (e) { return; }
      consoleAppend(entry);
    };
    src.onerror = () => {
      consoleSetStatus('connecting');
      // EventSource auto-reconnects; status flips back on the next onopen.
    };
  }

  function closeConsole() {
    if (consoleSource) {
      consoleSource.close();
      consoleSource = null;
    }
  }

  function bindConsoleEvents() {
    const levelSel = $('consoleLevel');
    if (levelSel) levelSel.addEventListener('change', async () => {
      try {
        const res = await api('/logs/level', { method: 'POST', body: JSON.stringify({ level: levelSel.value }) });
        if (res.ok) toastPrimary(t('console.levelChanged', levelSel.value));
        else toastError(t('common.failed'));
      } catch (e) { toastError(t('common.failed')); }
    });
    const filterSel = $('consoleFilter');
    if (filterSel) filterSel.addEventListener('change', () => {
      consoleFilter = filterSel.value;
      consoleApplyFilter();
    });
    const pauseBtn = $('consolePauseBtn');
    if (pauseBtn) pauseBtn.addEventListener('click', () => {
      consolePaused = !consolePaused;
      pauseBtn.dataset.active = String(consolePaused);
      pauseBtn.textContent = t(consolePaused ? 'console.resume' : 'console.pause');
    });
    const autoBtn = $('consoleAutoscrollBtn');
    if (autoBtn) autoBtn.addEventListener('click', () => {
      consoleAutoscroll = !consoleAutoscroll;
      autoBtn.dataset.active = String(consoleAutoscroll);
    });
    const clearBtn = $('consoleClearBtn');
    if (clearBtn) clearBtn.addEventListener('click', () => {
      consoleQueue.length = 0;
      const out = $('consoleOutput');
      if (out) out.innerHTML = '';
    });
  }

  // Floating scroll navigation — a single pill that scrolls either the window
  // (normal tabs) or the console output pane (console tab, which scrolls in its
  // own overflow container rather than the page).
  let scrollNavTarget = null; // null => page/window scrolling
  let scrollNavRaf = false;
  const SCROLL_NAV_THRESHOLD = 24;

  function scrollNavMetrics() {
    if (scrollNavTarget) {
      return {
        top: scrollNavTarget.scrollTop,
        max: scrollNavTarget.scrollHeight - scrollNavTarget.clientHeight,
      };
    }
    const el = document.scrollingElement || document.documentElement;
    return {
      top: window.scrollY || el.scrollTop || 0,
      max: el.scrollHeight - window.innerHeight,
    };
  }

  function scrollNavUpdate() {
    scrollNavRaf = false;
    const nav = $('scrollNav');
    if (!nav) return;
    if ($('mainPage').classList.contains('hidden')) { nav.hidden = true; return; }
    const { top, max } = scrollNavMetrics();
    if (max <= SCROLL_NAV_THRESHOLD) { nav.hidden = true; return; }
    nav.hidden = false;
    const upBtn = nav.querySelector('[data-dir="up"]');
    const downBtn = nav.querySelector('[data-dir="down"]');
    if (upBtn) upBtn.disabled = top <= SCROLL_NAV_THRESHOLD;
    if (downBtn) downBtn.disabled = top >= max - SCROLL_NAV_THRESHOLD;
  }

  function scrollNavSchedule() {
    if (scrollNavRaf) return;
    scrollNavRaf = true;
    requestAnimationFrame(scrollNavUpdate);
  }

  function scrollNavSetTarget(el) {
    if (scrollNavTarget) scrollNavTarget.removeEventListener('scroll', scrollNavSchedule);
    scrollNavTarget = el || null;
    if (scrollNavTarget) scrollNavTarget.addEventListener('scroll', scrollNavSchedule, { passive: true });
    scrollNavSchedule();
  }

  function scrollNavTo(dir) {
    const { max } = scrollNavMetrics();
    const top = dir === 'up' ? 0 : max;
    (scrollNavTarget || window).scrollTo({ top, behavior: 'smooth' });
  }

  function initScrollNav() {
    const nav = $('scrollNav');
    if (!nav) return;
    nav.querySelectorAll('.scroll-nav-btn').forEach(btn => {
      btn.addEventListener('click', () => scrollNavTo(btn.dataset.dir));
    });
    window.addEventListener('scroll', scrollNavSchedule, { passive: true });
    window.addEventListener('resize', scrollNavSchedule);
    // Content height changes (rendering account cards, forwarding rows, console
    // log lines) don't fire scroll/resize, so observe the panes that grow.
    if (typeof ResizeObserver === 'function') {
      const ro = new ResizeObserver(scrollNavSchedule);
      const container = document.querySelector('.app-main > .container');
      if (container) ro.observe(container);
      const out = $('consoleOutput');
      if (out) ro.observe(out);
    }
    scrollNavSchedule();
  }

  // Tabs
  function switchTab(tab) {
    qsa('.tab').forEach(el => el.classList.toggle('active', el.dataset.tab === tab));
    qsa('.tab-content').forEach(c => c.classList.add('hidden'));
    $('tab' + tab.charAt(0).toUpperCase() + tab.slice(1)).classList.remove('hidden');
    if (tab === 'console') openConsole();
    else closeConsole();
    if (tab === 'forwarding') openForwarding();
    else closeForwarding();
    if (tab === 'stats') openStats();
    if (tab === 'logs') loadLogs();
    if (tab === 'apikeys') loadApiKeys();
    setSidebar(false);
    // Console scrolls in its own overflow pane; every other tab scrolls the page.
    scrollNavSetTarget(tab === 'console' ? $('consoleOutput') : null);
  }

  function setSidebar(open) {
    const page = $('mainPage');
    if (!page) return;
    page.classList.toggle('sidebar-open', open);
    const toggle = $('sidebarToggle');
    if (toggle) toggle.setAttribute('aria-expanded', String(open));
    const backdrop = $('sidebarBackdrop');
    if (backdrop) backdrop.hidden = !open;
  }

  // Event wiring
  function bindLoginEvents() {
    $('loginBtn').addEventListener('click', login);
    $('pwdField').addEventListener('keypress', e => { if (e.key === 'Enter') login(); });

    const pwdToggle = $('pwdToggle');
    if (pwdToggle) {
      pwdToggle.addEventListener('click', () => {
        const f = $('pwdField');
        const willShow = f.type === 'password';
        f.type = willShow ? 'text' : 'password';
        pwdToggle.dataset.shown = String(willShow);
        pwdToggle.setAttribute('aria-label', willShow ? t('login.hidePassword') : t('login.showPassword'));
        pwdToggle.innerHTML = willShow
          ? '<i class="fa-solid fa-eye-slash"></i>'
          : '<i class="fa-solid fa-eye"></i>';
      });
    }
  }

  function bindShellEvents() {
    const checkUpdateBtn = $('checkUpdateBtn');
    if (checkUpdateBtn) checkUpdateBtn.addEventListener('click', () => checkUpdate(true));

    document.body.addEventListener('click', e => {
      if (!e.target.closest('.custom-select')) closeAllCustomSelects();
    });
    // The switcher is a <select>, so it reports through change rather than click.
    // Delegated because both copies (login topbar and sidebar) share the class,
    // and the custom-select overlay re-dispatches change from the hidden native
    // element — the listener has to sit on an ancestor to see it either way.
    document.body.addEventListener('change', e => {
      const ls = e.target.closest('.lang-select');
      if (ls && ls.value !== currentLang) setLang(ls.value);
    });
    window.addEventListener('resize', positionOpenCustomSelects);
    window.addEventListener('scroll', positionOpenCustomSelects, true);

    $('loginThemeToggle').addEventListener('click', toggleTheme);
    $('mainThemeToggle').addEventListener('click', toggleTheme);
    $('logoutBtn').addEventListener('click', logout);

    qsa('#tabBar .tab').forEach(tab => tab.addEventListener('click', () => switchTab(tab.dataset.tab)));

    const sidebarToggle = $('sidebarToggle');
    if (sidebarToggle) sidebarToggle.addEventListener('click', () => {
      setSidebar(!$('mainPage').classList.contains('sidebar-open'));
    });
    const sidebarBackdrop = $('sidebarBackdrop');
    if (sidebarBackdrop) sidebarBackdrop.addEventListener('click', () => setSidebar(false));
    document.addEventListener('keydown', e => { if (e.key === 'Escape') setSidebar(false); });

    qsa('[data-copy]').forEach(btn => btn.addEventListener('click', async () => {
      const id = btn.dataset.copy;
      const target = $(id);
      if (!target) return;
      try {
        await copyText(target.dataset.rawValue || target.textContent);
        toast(t('common.copied'), 'primary');
      } catch (e) {
        toast(t('common.failed'), 'error');
      }
    }));

    // API View buttons
    $('viewModelsBtn').addEventListener('click', showModelsView);
    $('viewStatsBtn').addEventListener('click', showStatsView);
    $('apiViewModalClose').addEventListener('click', closeApiViewModal);
    bindDialogBackdropClose('apiViewModal', closeApiViewModal);

    // Logs tab
    const logsRefreshBtn = $('logsRefreshBtn');
    if (logsRefreshBtn) logsRefreshBtn.addEventListener('click', loadLogs);
    const logsClearBtn = $('logsClearBtn');
    if (logsClearBtn) logsClearBtn.addEventListener('click', clearLogs);
    const logsAuto = $('logsAutoRefresh');
    if (logsAuto) logsAuto.addEventListener('change', toggleLogsAutoRefresh);
    const logsFilterSel = $('logsFilterSelect');
    if (logsFilterSel) logsFilterSel.addEventListener('change', e => {
      logsFilter = e.target.value;
      loadLogs();
    });
  }

  function bindAccountEvents() {
    $('privacyModeToggle').addEventListener('change', e => {
      privacyModeEnabled = e.target.checked;
      localStorage.setItem('privacyMode', privacyModeEnabled);
      renderAccounts();
    });

    $('exportBtn').addEventListener('click', showExportModal);
    $('refreshAllModelsBtn').addEventListener('click', refreshAllModels);
    $('addAccountBtn').addEventListener('click', () => showModal('add'));

    $('selectAllCheckbox').addEventListener('change', e => toggleSelectAll(e.target.checked));
    qsa('[data-batch]').forEach(b => b.addEventListener('click', () => {
      const a = b.dataset.batch;
      if (a === 'refreshModels') batchRefreshModels();
      else if (a === 'delete') batchDelete();
      else batchAction(a);
    }));

    $('filterSearch').addEventListener('input', onFilterChange);
    $('filterStatusSelect').addEventListener('change', onFilterChange);

    $('accountsList').addEventListener('click', e => {
      const cb = e.target.closest('.account-checkbox');
      if (cb) {
        toggleSelectAccount(cb.dataset.id);
        const card = cb.closest('.account-card');
        if (card) card.classList.toggle('selected', cb.checked);
        return;
      }
      const btn = e.target.closest('button[data-action]');
      if (!btn) return;
      const id = btn.dataset.id;
      const action = btn.dataset.action;
      if (action === 'refresh') refreshAccount(id, btn.closest('.account-card'));
      else if (action === 'inject') injectAccount(id, btn);
      else if (action === 'detail') showDetail(id);
      else if (action === 'copyJSON') copyAccountJSON(id, btn);
      else if (action === 'toggle') toggleAccount(id, btn.dataset.enabled === 'true');
      else if (action === 'test') testAccount(id);
      else if (action === 'delete') deleteAccount(id);
    });
  }

  function bindSettingsEvents() {
    $('saveRequireApiKeyBtn').addEventListener('click', saveRequireApiKey);
    $('saveOverUsageBtn').addEventListener('click', saveOverUsageConfig);
    const saveModelCatalogBtn = $('saveModelCatalogBtn');
    if (saveModelCatalogBtn) saveModelCatalogBtn.addEventListener('click', saveModelCatalogConfig);
    $('saveThinkingBtn').addEventListener('click', saveThinkingConfig);
    $('saveEndpointBtn').addEventListener('click', saveEndpointConfig);
    $('changePasswordBtn').addEventListener('click', changePassword);
    $('saveSecurityBtn').addEventListener('click', saveSecurityConfig);
    $('proxyType').addEventListener('change', onProxyTypeChange);
    $('saveProxyBtn').addEventListener('click', saveProxyConfig);
    $('proxyImportBtn').addEventListener('click', importProxies);
    $('resetStatsBtn').addEventListener('click', resetStats);
    bindApiKeyEvents();
    bindUpstreamEvents();
  }

  function bindPromptFilterEvents() {
    $('savePromptFilterBtn').addEventListener('click', savePromptFilter);
    $('addRuleRegexBtn').addEventListener('click', () => addPromptRule('regex'));
    $('addRuleContainsBtn').addEventListener('click', () => addPromptRule('lines-containing'));

    $('promptFilterRules').addEventListener('input', e => {
      const idx = e.target.dataset.ruleIdx;
      const field = e.target.dataset.ruleField;
      if (idx != null && field) promptRules[idx][field] = e.target.value;
    });
    $('promptFilterRules').addEventListener('change', e => {
      if (e.target.dataset.ruleToggle != null) {
        promptRules[e.target.dataset.ruleToggle].enabled = e.target.checked;
        renderPromptRules();
      }
    });
    $('promptFilterRules').addEventListener('click', e => {
      const rm = e.target.closest('[data-rule-remove]');
      if (rm) { promptRules.splice(parseInt(rm.dataset.ruleRemove, 10), 1); renderPromptRules(); }
    });
  }

  function bindMemoryEvents() {
    $('saveMemoryBtn').addEventListener('click', saveMemoryConfig);
    $('memoryWriteMode').addEventListener('change', updateMemoryWriteModeWarning);
  }

  function bindModalEvents() {
    $('addModalClose').addEventListener('click', closeModal);
    $('detailModalClose').addEventListener('click', closeDetailModal);
    $('exportModalClose').addEventListener('click', closeExportModal);
    $('testModalClose').addEventListener('click', closeTestModal);
    $('updateModalClose').addEventListener('click', closeUpdateModal);
    [
      ['addModal', closeModal],
      ['detailModal', closeDetailModal],
      ['exportModal', closeExportModal],
      ['testModal', closeTestModal],
      ['updateModal', closeUpdateModal],
      ['confirmModal', () => closeConfirm(false)],
    ].forEach(([id, fn]) => bindDialogBackdropClose(id, fn));

    $('modalBody').addEventListener('click', e => {
      const m = e.target.closest('[data-method]');
      if (m) { showModal(m.dataset.method); return; }
      const microsoftBack = e.target.closest('[data-microsoft-back]');
      if (microsoftBack) {
        resetMicrosoftFlow(true);
        showModal('add');
        return;
      }
      const g = e.target.closest('[data-modal-goto]');
      if (g) { showModal(g.dataset.modalGoto); return; }
      if (e.target.dataset.closeAdd) closeModal();
    });
  }

  function bindDetailEvents() {
    $('detailBody').addEventListener('click', e => {
      if (e.target.id === 'generateMachineIdBtn') { generateMachineId(); return; }
      const ps = e.target.closest('[data-profile-select]');
      if (ps) { selectProfile(ps.dataset.id, ps.dataset.arn, ps.dataset.region, ps); return; }
      const b = e.target.closest('[data-detail-action]');
      if (!b) return;
      const id = b.dataset.id;
      const a = b.dataset.detailAction;
      if (a === 'saveMachineId') saveMachineId(id);
      else if (a === 'saveWeight') saveWeight(id);
      else if (a === 'toggleOverage') toggleOverageSwitch(id, b);
      else if (a === 'refreshOverage') refreshAccountOverage(id);
      else if (a === 'saveProxyURL') saveProxyURL(id);
      else if (a === 'loadModels') loadModels(id);
      else if (a === 'refreshModels') refreshAccountModels(id);
      else if (a === 'discoverProfiles') discoverProfiles(id);
    });
  }

  function bindTestEvents() {
    $('testBody').addEventListener('click', e => {
      if (e.target.id === 'testLogClear') { clearTestLog(); return; }
      if (e.target.id === 'testModalCancelBtn') { closeTestModal(); return; }
      const run = e.target.closest('#testRunBtn');
      if (run) runTestAccount(run.dataset.id, getTestModelValue());
    });
    $('testBody').addEventListener('keydown', e => {
      if (e.key !== 'Enter') return;
      if (!e.target.closest('#testModelChoice')) return;
      const run = $('testRunBtn');
      if (!run || run.disabled) return;
      e.preventDefault();
      runTestAccount(run.dataset.id, getTestModelValue());
    });
  }

  // ── API View Modal ──
  function closeApiViewModal() {
    closeDialog('apiViewModal');
  }

  function formatUptime(seconds) {
    const d = Math.floor(seconds / 86400);
    const h = Math.floor((seconds % 86400) / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    const s = seconds % 60;
    const parts = [];
    if (d > 0) parts.push(d + (currentLang === 'zh' ? '天' : 'd'));
    if (h > 0) parts.push(h + (currentLang === 'zh' ? '时' : 'h'));
    if (m > 0) parts.push(m + (currentLang === 'zh' ? '分' : 'm'));
    parts.push(s + (currentLang === 'zh' ? '秒' : 's'));
    return parts.join(' ');
  }

  async function showModelsView() {
    const title = $('apiViewTitle');
    const body = $('apiViewBody');
    title.textContent = t('api.viewModelsTitle');
    body.innerHTML = '<div class="api-view-loading"><i class="fa-solid fa-spinner fa-spin"></i> ' + escapeHtml(t('api.loading')) + '</div>';
    openDialog('apiViewModal');

    try {
      const res = await fetch(baseUrl + '/v1/models');
      if (!res.ok) throw new Error('HTTP ' + res.status);
      const data = await res.json();
      const models = data.data || [];
      renderModelsView(body, models);
    } catch (e) {
      body.innerHTML = '<div class="api-view-error"><i class="fa-solid fa-circle-exclamation"></i> ' + escapeHtml(t('api.fetchError') + ': ' + e.message) + '</div>';
    }
  }

  function renderModelsView(container, models) {
    const thinkingSuffix = '-thinking';
    let html = '<div class="api-view-toolbar">';
    html += '<span class="api-view-count">' + escapeHtml(t('api.totalModels').replace('{count}', models.length)) + '</span>';
    html += '<input type="text" class="api-view-search" id="modelsSearchInput" placeholder="' + escapeAttr(t('api.searchModels')) + '" />';
    html += '</div>';
    html += '<div id="modelsGridContainer">';
    html += buildModelsGroupedHtml(models, thinkingSuffix);
    html += '</div>';
    container.innerHTML = html;

    const searchInput = $('modelsSearchInput');
    if (searchInput) {
      searchInput.addEventListener('input', () => {
        const kw = searchInput.value.toLowerCase().trim();
        const filtered = kw ? models.filter(m => (m.id || '').toLowerCase().includes(kw) || (m.owned_by || '').toLowerCase().includes(kw)) : models;
        $('modelsGridContainer').innerHTML = buildModelsGroupedHtml(filtered, thinkingSuffix);
      });
    }
  }

  // SVG icons for model providers (inline style forces size over Tailwind preflight)
  const _svgStyle = 'style="width:1.375rem;height:1.375rem;max-width:1.375rem;max-height:1.375rem;flex:none;display:block"';
  const MODEL_SVGS = {
    claude: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M4.709 15.955l4.72-2.647.08-.23-.08-.128H9.2l-.79-.048-2.698-.073-2.339-.097-2.266-.122-.571-.121L0 11.784l.055-.352.48-.321.686.06 1.52.103 2.278.158 1.652.097 2.449.255h.389l.055-.157-.134-.098-.103-.097-2.358-1.596-2.552-1.688-1.336-.972-.724-.491-.364-.462-.158-1.008.656-.722.881.06.225.061.893.686 1.908 1.476 2.491 1.833.365.304.145-.103.019-.073-.164-.274-1.355-2.446-1.446-2.49-.644-1.032-.17-.619a2.97 2.97 0 01-.104-.729L6.283.134 6.696 0l.996.134.42.364.62 1.414 1.002 2.229 1.555 3.03.456.898.243.832.091.255h.158V9.01l.128-1.706.237-2.095.23-2.695.08-.76.376-.91.747-.492.584.28.48.685-.067.444-.286 1.851-.559 2.903-.364 1.942h.212l.243-.242.985-1.306 1.652-2.064.73-.82.85-.904.547-.431h1.033l.76 1.129-.34 1.166-1.064 1.347-.881 1.142-1.264 1.7-.79 1.36.073.11.188-.02 2.856-.606 1.543-.28 1.841-.315.833.388.091.395-.328.807-1.969.486-2.309.462-3.439.813-.042.03.049.061 1.549.146.662.036h1.622l3.02.225.79.522.474.638-.079.485-1.215.62-1.64-.389-3.829-.91-1.312-.329h-.182v.11l1.093 1.068 2.006 1.81 2.509 2.33.127.578-.322.455-.34-.049-2.205-1.657-.851-.747-1.926-1.62h-.128v.17l.444.649 2.345 3.521.122 1.08-.17.353-.608.213-.668-.122-1.374-1.925-1.415-2.167-1.143-1.943-.14.08-.674 7.254-.316.37-.729.28-.607-.461-.322-.747.322-1.476.389-1.924.315-1.53.286-1.9.17-.632-.012-.042-.14.018-1.434 1.967-2.18 2.945-1.726 1.845-.414.164-.717-.37.067-.662.401-.589 2.388-3.036 1.44-1.882.93-1.086-.006-.158h-.055L4.132 18.56l-1.13.146-.487-.456.061-.746.231-.243 1.908-1.312-.006.006z"/></svg>',
    openai: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M9.205 8.658v-2.26c0-.19.072-.333.238-.428l4.543-2.616c.619-.357 1.356-.523 2.117-.523 2.854 0 4.662 2.212 4.662 4.566 0 .167 0 .357-.024.547l-4.71-2.759a.797.797 0 00-.856 0l-5.97 3.473zm10.609 8.8V12.06c0-.333-.143-.57-.429-.737l-5.97-3.473 1.95-1.118a.433.433 0 01.476 0l4.543 2.617c1.309.76 2.189 2.378 2.189 3.948 0 1.808-1.07 3.473-2.76 4.163zM7.802 12.703l-1.95-1.142c-.167-.095-.239-.238-.239-.428V5.899c0-2.545 1.95-4.472 4.591-4.472 1 0 1.927.333 2.712.928L8.23 5.067c-.285.166-.428.404-.428.737v6.898zM12 15.128l-2.795-1.57v-3.33L12 8.658l2.795 1.57v3.33L12 15.128zm1.796 7.23c-1 0-1.927-.332-2.712-.927l4.686-2.712c.285-.166.428-.404.428-.737v-6.898l1.974 1.142c.167.095.238.238.238.428v5.233c0 2.545-1.974 4.472-4.614 4.472zm-5.637-5.303l-4.544-2.617c-1.308-.761-2.188-2.378-2.188-3.948A4.482 4.482 0 014.21 6.327v5.423c0 .333.143.571.428.738l5.947 3.449-1.95 1.118a.432.432 0 01-.476 0zm-.262 3.9c-2.688 0-4.662-2.021-4.662-4.519 0-.19.024-.38.047-.57l4.686 2.71c.286.167.571.167.856 0l5.97-3.448v2.26c0 .19-.07.333-.237.428l-4.543 2.616c-.619.357-1.356.523-2.117.523zm5.899 2.83a5.947 5.947 0 005.827-4.756C22.287 18.339 24 15.84 24 13.296c0-1.665-.713-3.282-1.998-4.448.119-.5.19-.999.19-1.498 0-3.401-2.759-5.947-5.946-5.947-.642 0-1.26.095-1.88.31A5.962 5.962 0 0010.205 0a5.947 5.947 0 00-5.827 4.757C1.713 5.447 0 7.945 0 10.49c0 1.666.713 3.283 1.998 4.448-.119.5-.19 1-.19 1.499 0 3.401 2.759 5.946 5.946 5.946.642 0 1.26-.095 1.88-.309a5.96 5.96 0 004.162 1.713z"/></svg>',
    deepseek: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M23.748 4.482c-.254-.124-.364.113-.512.234-.051.039-.094.09-.137.136-.372.397-.806.657-1.373.626-.829-.046-1.537.214-2.163.848-.133-.782-.575-1.248-1.247-1.548-.352-.156-.708-.311-.955-.65-.172-.241-.219-.51-.305-.774-.055-.16-.11-.323-.293-.35-.2-.031-.278.136-.356.276-.313.572-.434 1.202-.422 1.84.027 1.436.633 2.58 1.838 3.393.137.093.172.187.129.323-.082.28-.18.552-.266.833-.055.179-.137.217-.329.14a5.526 5.526 0 01-1.736-1.18c-.857-.828-1.631-1.742-2.597-2.458a11.365 11.365 0 00-.689-.471c-.985-.957.13-1.743.388-1.836.27-.098.093-.432-.779-.428-.872.004-1.67.295-2.687.684a3.055 3.055 0 01-.465.137 9.597 9.597 0 00-2.883-.102c-1.885.21-3.39 1.102-4.497 2.623C.082 8.606-.231 10.684.152 12.85c.403 2.284 1.569 4.175 3.36 5.653 1.858 1.533 3.997 2.284 6.438 2.14 1.482-.085 3.133-.284 4.994-1.86.47.234.962.327 1.78.397.63.059 1.236-.03 1.705-.128.735-.156.684-.837.419-.961-2.155-1.004-1.682-.595-2.113-.926 1.096-1.296 2.746-2.642 3.392-7.003.05-.347.007-.565 0-.845-.004-.17.035-.237.23-.256a4.173 4.173 0 001.545-.475c1.396-.763 1.96-2.015 2.093-3.517.02-.23-.004-.467-.247-.588zM11.581 18c-2.089-1.642-3.102-2.183-3.52-2.16-.392.024-.321.471-.235.763.09.288.207.486.371.739.114.167.192.416-.113.603-.673.416-1.842-.14-1.897-.167-1.361-.802-2.5-1.86-3.301-3.307-.774-1.393-1.224-2.887-1.298-4.482-.02-.386.093-.522.477-.592a4.696 4.696 0 011.529-.039c2.132.312 3.946 1.265 5.468 2.774.868.86 1.525 1.887 2.202 2.891.72 1.066 1.494 2.082 2.48 2.914.348.292.625.514.891.677-.802.09-2.14.11-3.054-.614zm1-6.44a.306.306 0 01.415-.287.302.302 0 01.2.288.306.306 0 01-.31.307.303.303 0 01-.304-.308zm3.11 1.596c-.2.081-.399.151-.59.16a1.245 1.245 0 01-.798-.254c-.274-.23-.47-.358-.552-.758a1.73 1.73 0 01.016-.588c.07-.327-.008-.537-.239-.727-.187-.156-.426-.199-.688-.199a.559.559 0 01-.254-.078c-.11-.054-.2-.19-.114-.358.028-.054.16-.186.192-.21.356-.202.767-.136 1.146.016.352.144.618.408 1.001.782.391.451.462.576.685.914.176.265.336.537.445.848.067.195-.019.354-.25.452z"/></svg>',
    qwen: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M12.604 1.34c.393.69.784 1.382 1.174 2.075a.18.18 0 00.157.091h5.552c.174 0 .322.11.446.327l1.454 2.57c.19.337.24.478.024.837-.26.43-.513.864-.76 1.3l-.367.658c-.106.196-.223.28-.04.512l2.652 4.637c.172.301.111.494-.043.77-.437.785-.882 1.564-1.335 2.34-.159.272-.352.375-.68.37-.777-.016-1.552-.01-2.327.016a.099.099 0 00-.081.05 575.097 575.097 0 01-2.705 4.74c-.169.293-.38.363-.725.364-.997.003-2.002.004-3.017.002a.537.537 0 01-.465-.271l-1.335-2.323a.09.09 0 00-.083-.049H4.982c-.285.03-.553-.001-.805-.092l-1.603-2.77a.543.543 0 01-.002-.54l1.207-2.12a.198.198 0 000-.197 550.951 550.951 0 01-1.875-3.272l-.79-1.395c-.16-.31-.173-.496.095-.965.465-.813.927-1.625 1.387-2.436.132-.234.304-.334.584-.335a338.3 338.3 0 012.589-.001.124.124 0 00.107-.063l2.806-4.895a.488.488 0 01.422-.246c.524-.001 1.053 0 1.583-.006L11.704 1c.341-.003.724.032.9.34zm-3.432.403a.06.06 0 00-.052.03L6.254 6.788a.157.157 0 01-.135.078H3.253c-.056 0-.07.025-.041.074l5.81 10.156c.025.042.013.062-.034.063l-2.795.015a.218.218 0 00-.2.116l-1.32 2.31c-.044.078-.021.118.068.118l5.716.008c.046 0 .08.02.104.061l1.403 2.454c.046.081.092.082.139 0l5.006-8.76.783-1.382a.055.055 0 01.096 0l1.424 2.53a.122.122 0 00.107.062l2.763-.02a.04.04 0 00.035-.02.041.041 0 000-.04l-2.9-5.086a.108.108 0 010-.113l.293-.507 1.12-1.977c.024-.041.012-.062-.035-.062H9.2c-.059 0-.073-.026-.043-.077l1.434-2.505a.107.107 0 000-.114L9.225 1.774a.06.06 0 00-.053-.031zm6.29 8.02c.046 0 .058.02.034.06l-.832 1.465-2.613 4.585a.056.056 0 01-.05.029.058.058 0 01-.05-.029L8.498 9.841c-.02-.034-.01-.052.028-.054l.216-.012 6.722-.012z"/></svg>',
    mistral: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path clip-rule="evenodd" fill="currentColor" d="M3.428 3.4h3.429v3.428h3.429v3.429h-.002 3.431V6.828h3.427V3.4h3.43v13.714H24v3.429H13.714v-3.428h-3.428v-3.429h-3.43v3.428h3.43v3.429H0v-3.429h3.428V3.4zm10.286 13.715h3.428v-3.429h-3.427v3.429z"/></svg>',
    gemini: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M20.616 10.835a14.147 14.147 0 01-4.45-3.001 14.111 14.111 0 01-3.678-6.452.503.503 0 00-.975 0 14.134 14.134 0 01-3.679 6.452 14.155 14.155 0 01-4.45 3.001c-.65.28-1.318.505-2.002.678a.502.502 0 000 .975c.684.172 1.35.397 2.002.677a14.147 14.147 0 014.45 3.001 14.112 14.112 0 013.679 6.453.502.502 0 00.975 0c.172-.685.397-1.351.677-2.003a14.145 14.145 0 013.001-4.45 14.113 14.113 0 016.453-3.678.503.503 0 000-.975 13.245 13.245 0 01-2.003-.678z"/></svg>',
    meta: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M6.897 4c1.915 0 3.516.932 5.43 3.376l.282-.373c.19-.246.383-.484.58-.71l.313-.35C14.588 4.788 15.792 4 17.225 4c1.273 0 2.469.557 3.491 1.516l.218.213c1.73 1.765 2.917 4.71 3.053 8.026l.011.392.002.25c0 1.501-.28 2.759-.818 3.7l-.14.23-.108.153c-.301.42-.664.758-1.086 1.009l-.265.142-.087.04a3.493 3.493 0 01-.302.118 4.117 4.117 0 01-1.33.208c-.524 0-.996-.067-1.438-.215-.614-.204-1.163-.56-1.726-1.116l-.227-.235c-.753-.812-1.534-1.976-2.493-3.586l-1.43-2.41-.544-.895-1.766 3.13-.343.592C7.597 19.156 6.227 20 4.356 20c-1.21 0-2.205-.42-2.936-1.182l-.168-.184c-.484-.573-.837-1.311-1.043-2.189l-.067-.32a8.69 8.69 0 01-.136-1.288L0 14.468c.002-.745.06-1.49.174-2.23l.1-.573c.298-1.53.828-2.958 1.536-4.157l.209-.34c1.177-1.83 2.789-3.053 4.615-3.16L6.897 4zm-.033 2.615l-.201.01c-.83.083-1.606.673-2.252 1.577l-.138.199-.01.018c-.67 1.017-1.185 2.378-1.456 3.845l-.004.022a12.591 12.591 0 00-.207 2.254l.002.188c.004.18.017.36.04.54l.043.291c.092.503.257.908.486 1.208l.117.137c.303.323.698.492 1.17.492 1.1 0 1.796-.676 3.696-3.641l2.175-3.4.454-.701-.139-.198C9.11 7.3 8.084 6.616 6.864 6.616zm10.196-.552l-.176.007c-.635.048-1.223.359-1.82.933l-.196.198c-.439.462-.887 1.064-1.367 1.807l.266.398c.18.274.362.56.55.858l.293.475 1.396 2.335.695 1.114c.583.926 1.03 1.6 1.408 2.082l.213.262c.282.326.529.54.777.673l.102.05c.227.1.457.138.718.138.176.002.35-.023.518-.073.338-.104.61-.32.813-.637l.095-.163.077-.162c.194-.459.29-1.06.29-1.785l-.006-.449c-.08-2.871-.938-5.372-2.2-6.798l-.176-.189c-.67-.683-1.444-1.074-2.27-1.074z"/></svg>',
    zhipu: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M11.991 23.503a.24.24 0 00-.244.248.24.24 0 00.244.249.24.24 0 00.245-.249.24.24 0 00-.22-.247l-.025-.001zM9.671 5.365a1.697 1.697 0 011.099 2.132l-.071.172-.016.04-.018.054c-.07.16-.104.32-.104.498-.035.71.47 1.279 1.186 1.314h.366c1.309.053 2.338 1.173 2.286 2.523-.052 1.332-1.152 2.38-2.478 2.327h-.174c-.715.018-1.274.64-1.239 1.368 0 .124.018.23.053.337.209.373.54.658.96.8.75.23 1.517-.125 1.9-.782l.018-.035c.402-.64 1.17-.96 1.92-.711.854.284 1.378 1.226 1.099 2.167a1.661 1.661 0 01-2.077 1.102 1.711 1.711 0 01-.907-.711l-.017-.035c-.2-.323-.463-.58-.851-.711l-.056-.018a1.646 1.646 0 00-1.954.746 1.66 1.66 0 01-1.065.764 1.677 1.677 0 01-1.989-1.279c-.209-.906.332-1.83 1.257-2.043a1.51 1.51 0 01.296-.035h.018c.68-.071 1.151-.622 1.116-1.333a1.307 1.307 0 00-.227-.693 2.515 2.515 0 01-.366-1.403 2.39 2.39 0 01.366-1.208c.14-.195.21-.444.227-.693.018-.71-.506-1.261-1.186-1.332l-.07-.018a1.43 1.43 0 01-.299-.07l-.05-.019a1.7 1.7 0 01-1.047-2.114 1.68 1.68 0 012.094-1.101zm-5.575 10.11c.26-.264.639-.367.994-.27.355.096.633.379.728.74.095.362-.007.748-.267 1.013-.402.41-1.053.41-1.455 0a1.062 1.062 0 010-1.482zm14.845-.294c.359-.09.738.024.992.297.254.274.344.665.237 1.025-.107.36-.396.634-.756.718-.551.128-1.1-.22-1.23-.781a1.05 1.05 0 01.757-1.26zm-.064-4.39c.314.32.49.753.49 1.206 0 .452-.176.886-.49 1.206-.315.32-.74.5-1.185.5-.444 0-.87-.18-1.184-.5a1.727 1.727 0 010-2.412 1.654 1.654 0 012.369 0zm-11.243.163c.364.484.447 1.128.218 1.691a1.665 1.665 0 01-2.188.923c-.855-.36-1.26-1.358-.907-2.228a1.68 1.68 0 011.33-1.038c.593-.08 1.183.169 1.547.652zm11.545-4.221c.368 0 .708.2.892.524.184.324.184.724 0 1.048a1.026 1.026 0 01-.892.524c-.568 0-1.03-.47-1.03-1.048 0-.579.462-1.048 1.03-1.048zm-14.358 0c.368 0 .707.2.891.524.184.324.184.724 0 1.048a1.026 1.026 0 01-.891.524c-.569 0-1.03-.47-1.03-1.048 0-.579.461-1.048 1.03-1.048zm10.031-1.475c.925 0 1.675.764 1.675 1.706s-.75 1.705-1.675 1.705-1.674-.763-1.674-1.705c0-.942.75-1.706 1.674-1.706zm-2.626-.684c.362-.082.653-.356.761-.718a1.062 1.062 0 00-.238-1.028 1.017 1.017 0 00-.996-.294c-.547.14-.881.7-.752 1.257.13.558.675.907 1.225.783zm0 16.876c.359-.087.644-.36.75-.72a1.062 1.062 0 00-.237-1.019 1.018 1.018 0 00-.985-.301 1.037 1.037 0 00-.762.717c-.108.361-.017.754.239 1.028.245.263.606.377.953.305l.043-.01zM17.19 3.5a.631.631 0 00.628-.64c0-.355-.279-.64-.628-.64a.631.631 0 00-.628.64c0 .355.28.64.628.64zm-10.38 0a.631.631 0 00.628-.64c0-.355-.28-.64-.628-.64a.631.631 0 00-.628.64c0 .355.279.64.628.64zm-5.182 7.852a.631.631 0 00-.628.64c0 .354.28.639.628.639a.63.63 0 00.627-.606l.001-.034a.62.62 0 00-.628-.64zm5.182 9.13a.631.631 0 00-.628.64c0 .355.279.64.628.64a.631.631 0 00.628-.64c0-.355-.28-.64-.628-.64zm10.38.018a.631.631 0 00-.628.64c0 .355.28.64.628.64a.631.631 0 00.628-.64c0-.355-.279-.64-.628-.64zm5.182-9.148a.631.631 0 00-.628.64c0 .354.279.639.628.639a.631.631 0 00.628-.64c0-.355-.28-.64-.628-.64zm-.384-4.992a.24.24 0 00.244-.249.24.24 0 00-.244-.249.24.24 0 00-.244.249c0 .142.122.249.244.249zM11.991.497a.24.24 0 00.245-.248A.24.24 0 0011.99 0a.24.24 0 00-.244.249c0 .133.108.236.223.247l.021.001zM2.011 6.36a.24.24 0 00.245-.249.24.24 0 00-.244-.249.24.24 0 00-.244.249.24.24 0 00.244.249zm0 11.263a.24.24 0 00-.243.248.24.24 0 00.244.249.24.24 0 00.244-.249.252.252 0 00-.244-.248zm19.995-.018a.24.24 0 00-.245.248.24.24 0 00.245.25.24.24 0 00.244-.25.252.252 0 00-.244-.248z"/></svg>',
    minimax: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M16.278 2c1.156 0 2.093.927 2.093 2.07v12.501a.74.74 0 00.744.709.74.74 0 00.743-.709V9.099a2.06 2.06 0 012.071-2.049A2.06 2.06 0 0124 9.1v6.561a.649.649 0 01-.652.645.649.649 0 01-.653-.645V9.1a.762.762 0 00-.766-.758.762.762 0 00-.766.758v7.472a2.037 2.037 0 01-2.048 2.026 2.037 2.037 0 01-2.048-2.026v-12.5a.785.785 0 00-.788-.753.785.785 0 00-.789.752l-.001 15.904A2.037 2.037 0 0113.441 22a2.037 2.037 0 01-2.048-2.026V18.04c0-.356.292-.645.652-.645.36 0 .652.289.652.645v1.934c0 .263.142.506.372.638.23.131.514.131.744 0a.734.734 0 00.372-.638V4.07c0-1.143.937-2.07 2.093-2.07zm-5.674 0c1.156 0 2.093.927 2.093 2.07v11.523a.648.648 0 01-.652.645.648.648 0 01-.652-.645V4.07a.785.785 0 00-.789-.78.785.785 0 00-.789.78v14.013a2.06 2.06 0 01-2.07 2.048 2.06 2.06 0 01-2.071-2.048V9.1a.762.762 0 00-.766-.758.762.762 0 00-.766.758v3.8a2.06 2.06 0 01-2.071 2.049A2.06 2.06 0 010 12.9v-1.378c0-.357.292-.646.652-.646.36 0 .653.29.653.646V12.9c0 .418.343.757.766.757s.766-.339.766-.757V9.099a2.06 2.06 0 012.07-2.048 2.06 2.06 0 012.071 2.048v8.984c0 .419.343.758.767.758.423 0 .766-.339.766-.758V4.07c0-1.143.937-2.07 2.093-2.07z"/></svg>',
    proxy: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M12 2l3.09 6.26L22 9.27l-5 4.87 1.18 6.88L12 17.77l-6.18 3.25L7 14.14 2 9.27l6.91-1.01L12 2z"/></svg>'
  };

  function getModelFamily(id) {
    const lower = id.toLowerCase();
    if (lower.startsWith('claude-') || lower.startsWith('anthropic')) return 'claude';
    if (lower.startsWith('gpt-') || lower === 'o1' || lower.startsWith('o1-') || lower.startsWith('o3-') || lower.startsWith('o4-')) return 'openai';
    if (lower.startsWith('deepseek')) return 'deepseek';
    if (lower.startsWith('qwen') || lower.startsWith('qwq') || lower.startsWith('qvq')) return 'qwen';
    if (lower.startsWith('glm') || lower.startsWith('chatglm') || lower.startsWith('zhipu') || lower.startsWith('codegeex')) return 'zhipu';
    if (lower.startsWith('minimax') || lower.startsWith('abab')) return 'minimax';
    if (lower.startsWith('mistral') || lower.startsWith('mixtral') || lower.startsWith('codestral')) return 'mistral';
    if (lower.startsWith('gemini') || lower.startsWith('gemma')) return 'gemini';
    if (lower.startsWith('llama') || lower.startsWith('meta-') || lower.startsWith('codellama')) return 'meta';
    if (lower === 'auto' || lower.startsWith('auto')) return 'proxy';
    return 'other';
  }

  function getModelFamilyLabel(family) {
    const labels = {
      claude: 'Claude (Anthropic)',
      openai: 'OpenAI',
      deepseek: 'DeepSeek',
      qwen: 'Qwen (Alibaba)',
      zhipu: 'GLM (Zhipu)',
      minimax: 'MiniMax',
      mistral: 'Mistral AI',
      gemini: 'Gemini (Google)',
      meta: 'LLaMA (Meta)',
      proxy: 'Proxy Aliases',
      other: currentLang === 'zh' ? '其他模型' : 'Other'
    };
    return labels[family] || family;
  }

  function getModelFamilyColor(family) {
    const colors = {
      claude: '#d97757',
      openai: '#10a37f',
      deepseek: '#4d6bfe',
      qwen: '#615ced',
      zhipu: '#3859ff',
      minimax: '#e1474f',
      mistral: '#ff7000',
      gemini: '#4285f4',
      meta: '#0668e1',
      proxy: '#888888',
      other: '#6b7280'
    };
    return colors[family] || '#6b7280';
  }

  function buildModelsGroupedHtml(models, thinkingSuffix) {
    if (models.length === 0) {
      return '<div class="api-view-loading">' + escapeHtml(t('api.noModels')) + '</div>';
    }

    // Group models by family
    const groups = {};
    const familyOrder = ['claude', 'openai', 'deepseek', 'qwen', 'zhipu', 'minimax', 'mistral', 'gemini', 'meta', 'proxy', 'other'];
    for (const m of models) {
      const family = getModelFamily(m.id || '');
      if (!groups[family]) groups[family] = [];
      groups[family].push(m);
    }

    let html = '';
    for (const family of familyOrder) {
      if (!groups[family] || groups[family].length === 0) continue;
      const familyModels = groups[family];
      const color = getModelFamilyColor(family);
      const svg = MODEL_SVGS[family] || MODEL_SVGS.proxy;
      const label = getModelFamilyLabel(family);

      html += '<div class="model-group">';
      html += '<div class="model-group-header">';
      html += '<span class="model-group-icon" style="color:' + color + '">' + svg + '</span>';
      html += '<span class="model-group-title">' + escapeHtml(label) + '</span>';
      html += '<span class="model-group-count">' + familyModels.length + '</span>';
      html += '</div>';
      html += '<div class="model-group-grid">';

      for (const m of familyModels) {
        const id = m.id || '';
        const isThinking = id.endsWith(thinkingSuffix);
        const supportsImage = m.supports_image || false;

        html += '<div class="model-item">';
        html += '<div class="model-info">';
        html += '<div class="model-name">' + escapeHtml(id) + '</div>';
        html += '<div class="model-badges">';
        if (isThinking) html += '<span class="model-badge model-badge--thinking"><i class="fa-solid fa-brain"></i> thinking</span>';
        if (supportsImage) html += '<span class="model-badge model-badge--image"><i class="fa-solid fa-image"></i> vision</span>';
        html += '</div>';
        html += '</div>';
        html += '</div>';
      }

      html += '</div></div>';
    }
    return html;
  }

  async function showStatsView() {
    const title = $('apiViewTitle');
    const body = $('apiViewBody');
    title.textContent = t('api.viewStatsTitle');
    body.innerHTML = '<div class="api-view-loading"><i class="fa-solid fa-spinner fa-spin"></i> ' + escapeHtml(t('api.loading')) + '</div>';
    openDialog('apiViewModal');

    try {
      const res = await api('/status');
      if (!res.ok) throw new Error('HTTP ' + res.status);
      const d = await res.json();
      renderStatsView(body, d);
    } catch (e) {
      body.innerHTML = '<div class="api-view-error"><i class="fa-solid fa-circle-exclamation"></i> ' + escapeHtml(t('api.fetchError') + ': ' + e.message) + '</div>';
    }
  }

  function renderStatsView(container, d) {
    const version = String(d.version || currentVersion || '-').replace(/^v/i, '');
    let html = '<div class="stats-view-grid">';
    html += statsCard(t('api.statsVersion'), version, '');
    html += statsCard(t('api.statsAccounts'), d.accounts || 0, '');
    html += statsCard(t('api.statsAvailable'), d.available || 0, 'success');
    html += statsCard(t('api.statsTotalReqs'), formatNum(d.totalRequests || 0), 'info');
    html += statsCard(t('api.statsSuccessReqs'), formatNum(d.successRequests || 0), 'success');
    html += statsCard(t('api.statsFailedReqs'), formatNum(d.failedRequests || 0), 'danger');
    html += statsCard(t('api.statsTotalTokens'), formatNum(d.totalTokens || 0), '');
    html += statsCard(t('api.statsTotalCredits'), (d.totalCredits || 0).toFixed(2), 'info');
    html += '</div>';
    if (d.uptime !== undefined) {
      html += '<div class="stats-view-uptime"><i class="fa-solid fa-clock"></i> ' + escapeHtml(t('api.statsUptime')) + ': <strong>' + escapeHtml(formatUptime(d.uptime)) + '</strong></div>';
    }
    container.innerHTML = html;
  }

  function statsCard(label, value, variant) {
    const cls = variant ? ' stats-view-item--' + variant : '';
    return '<div class="stats-view-item' + cls + '"><div class="stats-view-value">' + escapeHtml(String(value)) + '</div><div class="stats-view-label">' + escapeHtml(label) + '</div></div>';
  }

  function wireEvents() {
    bindLoginEvents();
    bindShellEvents();
    bindAccountEvents();
    bindSettingsEvents();
    bindPromptFilterEvents();
    bindMemoryEvents();
    bindModalEvents();
    bindDetailEvents();
    bindTestEvents();
    bindConsoleEvents();
    bindForwardEvents();
    bindStatsEvents();
  }

  // Init
  async function init() {
    initTheme();
    await loadLocale(currentLang);
    if (currentLang !== 'zh') await loadLocale('zh');
    applyTranslations();
    initCustomSelectObserver();
    initPrivacyMode();
    initRememberMe();
    initApiAddrSelect();
    initScrollNav();
    const yr = $('footerYear');
    if (yr) yr.textContent = new Date().getFullYear();
    wireEvents();
    if (password) tryAutoLogin();
    setInterval(() => {
      if (!$('mainPage').classList.contains('hidden')) loadStats();
    }, 10000);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
