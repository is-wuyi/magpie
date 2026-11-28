// magpie cloud — what every page of the deployment shares: the dictionary
// and the reader's language, the DOM and formatting helpers, the JSON call
// and its safe error reasons, the session and the public authentication
// policy. The landing page, the sign-in pages and the portal each load this
// file before their own controller. It holds no credential.
"use strict";

(function () {
  const DICT = globalThis.magpieCloudI18N;

  const LANGS = ["en", "zh", "zh-TW", "ja", "de"];
  const LANG_NAMES = { en: "English", zh: "简体中文", "zh-TW": "繁體中文", ja: "日本語", de: "Deutsch" };
  const LOCALES = { en: "en-US", zh: "zh-CN", "zh-TW": "zh-TW", ja: "ja-JP", de: "de-DE" };
  const LANG_KEY = "magpie.cloud.lang";

  // ---------------------------------------------------------------- DOM
  const byId = (id) => document.getElementById(id);

  function h(tag, cls, text) {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text instanceof Node) n.append(text);
    else if (text != null) n.textContent = text;
    return n;
  }

  function clear(node) {
    while (node && node.firstChild) node.removeChild(node.firstChild);
  }

  function show(node, on) {
    if (!node) return;
    if (on) node.removeAttribute("hidden");
    else node.setAttribute("hidden", "");
  }

  function setPipeText(node, value) {
    if (!node) return;
    node.textContent = value == null ? "" : String(value);
  }

  function setFormError(node, message) {
    if (!node) return;
    if (message) {
      node.textContent = message;
      show(node, true);
    } else {
      node.textContent = "";
      show(node, false);
    }
  }

  // ---------------------------------------------------------------- values
  function slugify(value) {
    return String(value || "").toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  }

  function isHttpURL(value) {
    try {
      const u = new URL(value);
      return u.protocol === "http:" || u.protocol === "https:";
    } catch (e) {
      return false;
    }
  }

  function readModels(value) {
    const out = [];
    const seen = new Set();
    for (const raw of String(value || "").split(/[,\n]+/)) {
      const m = raw.trim();
      if (!m || seen.has(m.toLowerCase())) continue;
      seen.add(m.toLowerCase());
      out.push(m);
    }
    return out;
  }

  function newRequestID() {
    try {
      return crypto.randomUUID();
    } catch (e) {
      return "admin-" + Date.now().toString(36) + "-" + Math.random().toString(16).slice(2, 12);
    }
  }

  // parseCNYMicros reads a CNY amount with at most six decimals as the
  // wallet's micro-CNY unit, without going through a float.
  function parseCNYMicros(raw) {
    const s = String(raw == null ? "" : raw).trim();
    if (!s) return 0;
    if (!/^\d+(?:\.\d{1,6})?$/.test(s)) return null;
    const parts = s.split(".");
    const micros = Number(parts[0]) * 1e6 + Number((parts[1] || "").padEnd(6, "0"));
    return Number.isSafeInteger(micros) ? micros : null;
  }

  function locale() {
    return LOCALES[currentLang()] || "en-US";
  }

  function fmtDate(value) {
    if (!value) return "";
    const d = new Date(value);
    if (Number.isNaN(d.getTime())) return "";
    try {
      return new Intl.DateTimeFormat(locale(), { dateStyle: "medium" }).format(d);
    } catch (e) {
      return d.toISOString().slice(0, 10);
    }
  }

  function formatCNY(micros, currency) {
    const cur = typeof currency === "string" && /^[A-Za-z]{3}$/.test(currency) ? currency.toUpperCase() : "CNY";
    const value = Number(micros) / 1e6;
    if (!Number.isFinite(value)) return "";
    try {
      return new Intl.NumberFormat(locale(), {
        style: "currency",
        currency: cur,
        minimumFractionDigits: 2,
        maximumFractionDigits: 6
      }).format(value);
    } catch (e) {
      return value.toFixed(2) + " " + cur;
    }
  }

  function formatFen(fen, currency) {
    return formatCNY(Number(fen) * 10000, currency);
  }

  function formatCount(n) {
    const value = Number(n);
    if (!Number.isFinite(value)) return "0";
    try {
      return new Intl.NumberFormat(locale(), { maximumFractionDigits: 0 }).format(value);
    } catch (e) {
      return String(value);
    }
  }

  async function copyValue(value, codeNode) {
    if (!value) return false;
    try {
      await navigator.clipboard.writeText(value);
      return true;
    } catch (e) {}
    try {
      const range = document.createRange();
      range.selectNodeContents(codeNode || document.body);
      const sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      const ok = document.execCommand("copy");
      sel.removeAllRanges();
      return ok;
    } catch (e) {
      return false;
    }
  }

  // ---------------------------------------------------------------- language
  let lang = detectLang();
  const listeners = [];

  function detectLang() {
    let wanted = null;
    try { wanted = new URLSearchParams(location.search).get("lang"); } catch (e) {}
    if (wanted && LANGS.includes(wanted)) return wanted;
    let saved = null;
    try { saved = localStorage.getItem(LANG_KEY); } catch (e) {}
    if (saved && LANGS.includes(saved)) return saved;
    const tags = (navigator.languages && navigator.languages.length ? navigator.languages : [navigator.language || ""]);
    for (const raw of tags) {
      const tag = String(raw).toLowerCase();
      if (tag === "zh-tw" || tag === "zh-hk" || tag === "zh-mo" || tag.startsWith("zh-hant")) return "zh-TW";
      if (tag.startsWith("zh")) return "zh";
      if (tag.startsWith("ja")) return "ja";
      if (tag.startsWith("de")) return "de";
      if (tag.startsWith("en")) return "en";
    }
    return "en";
  }

  function currentLang() {
    return lang;
  }

  function t(key, vars) {
    const pack = DICT[lang] || DICT.en;
    let s = pack[key];
    if (s == null) s = DICT.en[key];
    if (s == null) return key;
    if (vars) s = s.replace(/\{(\w+)\}/g, (m, name) => (vars[name] != null ? String(vars[name]) : m));
    return s;
  }

  // applyStatic fills every string the markup marks: data-t for its text,
  // data-tp for a placeholder, data-tt for a title and data-ta for an
  // accessible name. A page's own redraw runs through onChange.
  function applyStatic() {
    document.documentElement.lang = lang;
    const titleKey = document.body && document.body.dataset.title;
    document.title = t(titleKey || "appTitle");
    for (const node of document.querySelectorAll("[data-t]")) setPipeText(node, t(node.dataset.t));
    for (const node of document.querySelectorAll("[data-tp]")) node.placeholder = t(node.dataset.tp);
    for (const node of document.querySelectorAll("[data-tt]")) node.title = t(node.dataset.tt);
    for (const node of document.querySelectorAll("[data-ta]")) node.setAttribute("aria-label", t(node.dataset.ta));
    renderLangMenu();
  }

  function onChange(fn) {
    listeners.push(fn);
  }

  function setLang(next, persist) {
    if (!LANGS.includes(next)) return;
    lang = next;
    if (persist) {
      try { localStorage.setItem(LANG_KEY, next); } catch (e) {}
    }
    applyStatic();
    for (const fn of listeners) fn();
  }

  function closeLangMenu() {
    const menu = byId("langMenu");
    if (!menu || menu.hidden) return;
    show(menu, false);
    byId("langBtn").setAttribute("aria-expanded", "false");
  }

  function renderLangMenu() {
    const menu = byId("langMenu");
    const button = byId("langBtn");
    if (!menu || !button) return;
    clear(menu);
    for (const code of LANGS) {
      const item = h("button", "", LANG_NAMES[code]);
      item.type = "button";
      item.setAttribute("role", "menuitemradio");
      item.setAttribute("aria-checked", code === lang ? "true" : "false");
      const mark = h("span", "mark", code === lang ? "✓" : "");
      mark.setAttribute("aria-hidden", "true");
      item.prepend(mark);
      item.addEventListener("click", () => {
        closeLangMenu();
        setLang(code, true);
      });
      menu.append(item);
    }
    byId("langNow").textContent = LANG_NAMES[lang];
    button.setAttribute("aria-label", t("langLabel") + ": " + LANG_NAMES[lang]);
    menu.setAttribute("aria-label", t("langLabel"));
  }

  function mountLangMenu() {
    const button = byId("langBtn");
    const menu = byId("langMenu");
    if (!button || !menu) return;
    button.addEventListener("click", (event) => {
      event.stopPropagation();
      const open = menu.hidden;
      show(menu, open);
      button.setAttribute("aria-expanded", open ? "true" : "false");
      if (open) {
        const current = menu.querySelector('button[aria-checked="true"]');
        (current || menu.querySelector("button")).focus({ preventScroll: true });
      }
    });
    menu.addEventListener("click", (event) => event.stopPropagation());
    document.addEventListener("click", () => closeLangMenu());
    document.addEventListener("keydown", (event) => {
      if (event.key === "Escape" && !menu.hidden) {
        closeLangMenu();
        button.focus({ preventScroll: true });
      }
    });
  }

  // ---------------------------------------------------------------- API
  async function api(path, opts) {
    const init = Object.assign({ method: "GET", credentials: "same-origin" }, opts || {});
    init.headers = Object.assign({ "Accept": "application/json" }, init.headers || {});
    if (init.body !== undefined) {
      init.headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(init.body);
    }
    const res = await fetch(path, init);
    let data = null;
    let text = "";
    try { text = await res.text(); } catch (e) { text = ""; }
    if (text) {
      try { data = JSON.parse(text); } catch (e) { data = null; }
    }
    return { res: res, data: data };
  }

  // errorOf names a safe, localized reason: never a server message, which
  // could name an account, a database detail or a decrypted configuration.
  function errorOf(res, data) {
    const code = data && typeof data.error === "string" ? data.error.toLowerCase() : "";
    if (code === "rate_limited" || (res && res.status === 429)) {
      let n = data && Number(data.retryAfter);
      if (!n && res && res.headers && res.headers.get) n = Number(res.headers.get("Retry-After"));
      return n > 0 ? { key: "errRateLimitedIn", vars: { n: Math.ceil(n) } } : { key: "errRateLimited" };
    }
    const byCode = {
      invalid_email: "errInvalidEmail",
      invalid_code: "errInvalidCode",
      invalid_credentials: "errInvalidCredentials",
      account_disabled: "errAccountDisabled",
      sign_in_failed: "errSignInFailed",
      unauthenticated: "errSignIn",
      forbidden: "errForbidden",
      mail_failed: "errMailFailed",
      invalid_request: "errInvalidRequest",
      method_not_allowed: "errInvalidRequest",
      unavailable: "errUnavailable",
      not_found: "errNotFound",
      registration_closed: "errRegisterClosed",
      registration_disabled: "errRegisterClosed",
      password_login_disabled: "errPasswordLoginOff",
      password_disabled: "errPasswordLoginOff",
      code_login_disabled: "errCodeLoginOff",
      code_disabled: "errCodeLoginOff",
      weak_password: "errPasswordTooShort",
      invalid_password: "errPasswordTooShort",
      email_taken: "errEmailTaken",
      version_conflict: "errPolicyConflict",
      policy_conflict: "errPolicyConflict",
      conflict: "errPolicyConflict"
    };
    if (byCode[code]) return { key: byCode[code] };
    const status = res ? res.status : 0;
    if (status === 401) return { key: "errSignIn" };
    if (status === 403) return { key: "errForbidden" };
    if (status === 404) return { key: "errNotFound" };
    if (status === 409) return { key: "errPolicyConflict" };
    if (status === 400) return { key: "errInvalidRequest" };
    if (status >= 500) return { key: "errUnavailable" };
    return { key: "errRequest" };
  }

  function messageOf(reason) {
    return reason ? t(reason.key, reason.vars) : "";
  }

  // ---------------------------------------------------------------- notice
  let noticeState = null;
  let noticeTimer = 0;

  function renderNotice() {
    const node = byId("notice");
    if (!node) return;
    if (!noticeState) {
      node.textContent = "";
      show(node, false);
      return;
    }
    const error = noticeState.tone === "error";
    node.textContent = messageOf(noticeState);
    node.dataset.tone = error ? "error" : "ok";
    node.setAttribute("role", error ? "alert" : "status");
    node.setAttribute("aria-live", error ? "assertive" : "polite");
    show(node, true);
  }

  function notice(reason, tone) {
    const node = byId("notice");
    if (!node) return;
    clearTimeout(noticeTimer);
    noticeTimer = 0;
    if (!reason) {
      noticeState = null;
      renderNotice();
      return;
    }
    noticeState = { key: reason.key, vars: reason.vars, tone: tone || "ok" };
    renderNotice();
    // a passing outcome goes as the app's status line does; an error stays
    // long enough to read and select
    noticeTimer = setTimeout(() => {
      noticeTimer = 0;
      noticeState = null;
      renderNotice();
    }, tone === "error" ? 15000 : 9000);
  }

  function clearNotice() {
    clearTimeout(noticeTimer);
    noticeTimer = 0;
    if (!noticeState) return;
    noticeState = null;
    renderNotice();
  }

  // ---------------------------------------------------------------- session
  // fetchMe separates "signed out" (the server said 401) from "unknown" (a
  // read that failed): a page must never treat a failed read as a sign-out.
  async function fetchMe() {
    try {
      const { res, data } = await api("/api/auth/me");
      if (res.status === 401) return { state: "out", user: null, reason: null };
      if (res.ok && data && data.user && typeof data.user === "object") {
        return { state: "in", user: data.user, reason: null };
      }
      return { state: "unknown", user: null, reason: errorOf(res, data) };
    } catch (e) {
      return { state: "unknown", user: null, reason: { key: "errUnavailable" } };
    }
  }

  // ---------------------------------------------------------------- policy
  // The four public switches. A read that fails, or a body that does not
  // hold the four fields, is unknown: the pages keep working and say so
  // rather than guessing that a method is off.
  const POLICY_FALLBACK = Object.freeze({
    registrationEnabled: true,
    passwordLoginEnabled: true,
    codeLoginEnabled: true,
    passwordMinLength: 8
  });

  function readPolicy(data) {
    if (!data || typeof data !== "object") return null;
    if (typeof data.registrationEnabled !== "boolean") return null;
    if (typeof data.passwordLoginEnabled !== "boolean") return null;
    if (typeof data.codeLoginEnabled !== "boolean") return null;
    const min = Number(data.passwordMinLength);
    if (!Number.isInteger(min) || min < 8 || min > 72) return null;
    return {
      registrationEnabled: data.registrationEnabled,
      passwordLoginEnabled: data.passwordLoginEnabled,
      codeLoginEnabled: data.codeLoginEnabled,
      passwordMinLength: min
    };
  }

  async function fetchPolicy() {
    try {
      const { res, data } = await api("/api/auth/policy");
      const policy = res.ok ? readPolicy(data) : null;
      if (policy) return { known: true, policy: policy, reason: null };
      return { known: false, policy: Object.assign({}, POLICY_FALLBACK), reason: errorOf(res, data) };
    } catch (e) {
      return { known: false, policy: Object.assign({}, POLICY_FALLBACK), reason: { key: "errUnavailable" } };
    }
  }

  function passwordFloor(policy) {
    const min = Number(policy && policy.passwordMinLength);
    return Number.isInteger(min) && min >= 8 && min <= 72 ? min : 8;
  }

  // Exposed for the pages' tests: the dictionary they check for missing
  // keys and placeholders, and the language the page picked. It holds no
  // credential and changes no behavior.
  window.magpieCloud = {
    langs: LANGS,
    dicts: DICT,
    lang: () => lang,
    page: () => (document.body && document.body.dataset.page) || "portal"
  };

  window.magpieCloudShared = {
    LANGS: LANGS,
    LANG_NAMES: LANG_NAMES,
    LOCALES: LOCALES,
    POLICY_FALLBACK: POLICY_FALLBACK,
    byId: byId,
    h: h,
    clear: clear,
    show: show,
    setPipeText: setPipeText,
    setFormError: setFormError,
    slugify: slugify,
    isHttpURL: isHttpURL,
    readModels: readModels,
    newRequestID: newRequestID,
    parseCNYMicros: parseCNYMicros,
    fmtDate: fmtDate,
    formatCNY: formatCNY,
    formatFen: formatFen,
    formatCount: formatCount,
    copyValue: copyValue,
    lang: currentLang,
    t: t,
    setLang: setLang,
    applyStatic: applyStatic,
    onChange: onChange,
    closeLangMenu: closeLangMenu,
    renderLangMenu: renderLangMenu,
    mountLangMenu: mountLangMenu,
    api: api,
    errorOf: errorOf,
    messageOf: messageOf,
    notice: notice,
    clearNotice: clearNotice,
    renderNotice: renderNotice,
    fetchMe: fetchMe,
    readPolicy: readPolicy,
    fetchPolicy: fetchPolicy,
    passwordFloor: passwordFloor
  };

  mountLangMenu();
  applyStatic();
})();
