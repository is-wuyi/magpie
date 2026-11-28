// magpie cloud — the portal's page. It talks to the cloud API in
// internal/gui/cloud.go and to the session route, and to nothing else. The
// page holds no upstream credential beyond the field it was typed into: a
// provider's key is sent once, on Save, and the gateway's own new secret is
// drawn once, from the create reply, until Done clears it. Sign-in,
// registration and password reset live on pages of their own (auth.js).
"use strict";

(function () {
  const shared = window.magpieCloudShared;
  const {
    byId, h, clear, show, setPipeText, setFormError, slugify, isHttpURL, readModels,
    parseCNYMicros, fmtDate, formatCNY, formatFen, formatCount,
    copyValue, t, onChange, api, errorOf, messageOf, notice, clearNotice,
    renderNotice, fetchMe
  } = shared;
  const STATUSES = ["draft", "published", "offline"];

  // The portal's own redraw after a language change: static strings are the
  // shared layer's job, and the lists, forms and messages are this page's.
  function applyStatic() {
    // a message written in the language the reader just left would be
    // stale; the list states and the notice are kept as keys and redrawn
    for (const id of ["payError", "pError", "keyError", "chError", "hpError", "groupError", "policyError"]) {
      setFormError(byId(id), "");
    }
    renderHouseProviderTitle();
    renderSession();
    renderNotice();
    renderBootState();
    renderAllLists();
  }

  // ---------------------------------------------------------------- state
  const S = {
    user: null,
    boot: { phase: "loading", error: null },
    wallet: { phase: "idle", data: null, error: null },
    walletOrders: { phase: "idle", items: [], error: null },
    usage: { phase: "idle", items: [], error: null },
    providers: { phase: "idle", items: [], error: null },
    houses: { phase: "idle", items: [], error: null },
    keys: { phase: "idle", items: [], error: null },
    groups: { phase: "idle", items: [], error: null },
    admin: { phase: "idle", items: [], error: null },
    users: { phase: "idle", items: [], error: null },
    orders: { phase: "idle", items: [], error: null },
    audit: { phase: "idle", items: [], error: null },
    adminProviders: { phase: "idle", items: [], error: null },
    policy: { phase: "idle", data: null, error: null, saving: false },
    housePending: "",
    revoking: "",
    statusPending: "",
    keyBusy: false,
    keyChannelSel: new Set(),
    providerProtocol: "chat",
    hpProtocol: "chat",
    houseProviderFor: "",
    houseProviderName: "",
    presetId: "",
    payProvider: "alipay",
    payBusy: false,
    order: null,
    orderChecking: false,
    orderCredited: false,
    pricesFor: "",
    prices: { phase: "idle", items: [], error: null },
    priceFormOpen: false,
    lastHouseProvider: null,
    deletingProvider: "",
    providerBusy: false,
    groupFormOpen: false,
    groupBusy: false,
    deletingGroup: "",
    userStatusBusy: "",
    creditFor: "",
    creditRequestId: "",
    creditBusy: false,
    adminProvidersFor: "",
    adminProviderDeleting: "",
    adminProviderBusy: false
  };

  let orderTimer = 0;
  // a read or a write that answers after the session changed is dropped:
  // its box belongs to the session that asked for it
  let generation = 0;

  // A read that answers 401 means the session ended: the portal belongs to
  // nobody now, so it goes to the sign-in page. Anything else is unknown and
  // stays on this page as a safe reason.
  function onSignedOut() {
    generation++;
    stopOrderPoll();
    S.user = null;
    clearSecret(false);
    location.replace("/login");
  }

  function signedIn(user) {
    generation++;
    S.user = user;
    S.boot = { phase: "ready", error: null };
    clearNotice();
    show(byId("bootState"), false);
    show(byId("portal"), true);
    show(byId("session"), true);
    renderSession();
    loadAll();
  }

  function isAdmin() {
    return !!(S.user && S.user.role === "admin");
  }

  function renderSession() {
    const session = byId("session");
    show(session, !!S.user);
    if (S.user) {
      byId("meEmail").textContent = S.user.email || "";
      byId("meEmail").title = S.user.email || "";
      byId("meRole").textContent = t(S.user.role === "admin" ? "roleAdmin" : "roleUser");
    }
    show(byId("adminPanel"), isAdmin());
    show(byId("usersPanel"), isAdmin());
    show(byId("ordersPanel"), isAdmin());
    show(byId("auditPanel"), isAdmin());
    show(byId("policyPanel"), isAdmin());
  }

  async function logout() {
    const button = byId("logout");
    if (button.disabled) return;
    button.disabled = true;
    clearNotice();
    try {
      const { res, data } = await api("/api/auth/logout", { method: "POST" });
      if (res.ok || res.status === 401) {
        generation++;
        stopOrderPoll();
        clearSecret(false);
        location.replace("/login");
        return;
      }
      notice(errorOf(res, data), "error");
    } catch (e) {
      notice({ key: "errUnavailable" }, "error");
    }
    button.disabled = false;
  }

  // ---------------------------------------------------------------- boot
  // Only a 401 sends the reader to the sign-in page. A failed read is
  // unknown: it says so on this page with a Retry, and never pretends the
  // reader is signed out.
  function renderBootState() {
    const node = byId("bootState");
    if (!node) return;
    const box = S.boot;
    clear(node);
    node.className = "state boot";
    if (box.phase === "ready") {
      show(node, false);
      return;
    }
    show(node, true);
    if (box.phase === "loading") {
      node.append(h("span", "spinner"), h("span", "state-text", t("checkingSession")));
      return;
    }
    node.className = "state boot error";
    node.append(h("span", "state-text", messageOf(box.error) || t("errSession")));
    const retry = h("button", "btn small", t("retry"));
    retry.type = "button";
    retry.addEventListener("click", boot);
    const signIn = h("a", "btn small ghost", t("signIn"));
    signIn.href = "/login";
    node.append(retry, signIn);
  }

  async function boot() {
    hideForms();
    show(byId("portal"), false);
    S.boot = { phase: "loading", error: null };
    renderBootState();
    const me = await fetchMe();
    if (me.state === "in") {
      signedIn(me.user);
      return;
    }
    if (me.state === "out") {
      location.replace("/login");
      return;
    }
    S.boot = { phase: "error", error: me.reason };
    renderBootState();
  }

  // ---------------------------------------------------------------- lists
  async function loadAll() {
    if (!S.user) return;
    const gen = generation;
    const tasks = [loadWallet(gen), loadWalletOrders(gen), loadUsage(gen), loadProviders(gen), loadHouses(gen), loadKeys(gen), loadGroups(gen)];
    if (isAdmin()) tasks.push(loadAdmin(gen), loadUsers(gen), loadOrders(gen), loadAudit(gen), loadPolicy(gen));
    await Promise.all(tasks);
  }

  function beginLoad(box) {
    box.phase = "loading";
    box.error = null;
  }

  function failLoad(box, reason) {
    box.phase = "error";
    box.error = reason;
  }

  async function fetchList(path, box, loadKey, gen) {
    if (!S.user || gen !== generation) return;
    beginLoad(box);
    renderAllLists();
    try {
      const { res, data } = await api(path);
      if (gen !== generation) return;
      if (res.status === 401) {
        onSignedOut("errSignIn");
        return;
      }
      if (!res.ok) {
        failLoad(box, errorOf(res, data));
      } else if (!Array.isArray(data)) {
        failLoad(box, { key: loadKey });
      } else {
        box.items = data;
        box.phase = "ready";
        box.error = null;
      }
    } catch (e) {
      if (gen !== generation) return;
      failLoad(box, { key: "errUnavailable" });
    }
    renderAllLists();
  }

  function loadProviders(gen = generation) {
    return fetchList("/api/cloud/providers", S.providers, "errLoadProviders", gen).then(renderProviders);
  }
  function loadHouses(gen = generation) {
    return fetchList("/api/cloud/house-channels", S.houses, "errLoadHouses", gen).then(() => {
      renderHouses();
      renderKeyChannels();
    });
  }
  function loadKeys(gen = generation) {
    return fetchList("/api/cloud/keys", S.keys, "errLoadKeys", gen).then(renderKeys);
  }
  function loadAdmin(gen = generation) {
    return fetchList("/api/cloud/admin/house-channels", S.admin, "errLoadAdmin", gen).then(renderAdmin);
  }
  function loadUsage(gen = generation) {
    return fetchList("/api/cloud/usage", S.usage, "errLoadUsage", gen).then(renderUsage);
  }
  function loadGroups(gen = generation) {
    return fetchList("/api/cloud/groups", S.groups, "errLoadGroups", gen).then(renderGroups);
  }
  function loadUsers(gen = generation) {
    return fetchList("/api/cloud/admin/users", S.users, "errLoadUsers", gen).then(renderUsers);
  }
  function loadOrders(gen = generation) {
    return fetchList("/api/cloud/admin/orders", S.orders, "errLoadOrders", gen).then(renderOrders);
  }
  function loadAudit(gen = generation) {
    return fetchList("/api/cloud/admin/audit", S.audit, "errLoadAudit", gen).then(renderAudit);
  }
  function loadWalletOrders(gen = generation) {
    if (!S.user || gen !== generation || !S.wallet.data || !S.wallet.data.paymentsEnabled) {
      S.walletOrders = { phase: "idle", items: [], error: null };
      renderWalletOrders();
      return Promise.resolve();
    }
    return fetchList("/api/cloud/orders", S.walletOrders, "errLoadOrders", gen).then(renderWalletOrders);
  }
  function loadAdminProviders(gen = generation) {
    const id = S.adminProvidersFor;
    if (!id || !isAdmin() || gen !== generation) return Promise.resolve();
    S.adminProviders.phase = "loading";
    S.adminProviders.error = null;
    renderAdmin();
    return api("/api/cloud/admin/house-channels/" + encodeURIComponent(id) + "/providers").then(({ res, data }) => {
      if (gen !== generation || S.adminProvidersFor !== id) return;
      if (res.status === 401) {
        onSignedOut("errSignIn");
        return;
      }
      if (!res.ok) {
        S.adminProviders.phase = "error";
        S.adminProviders.error = errorOf(res, data);
      } else if (!Array.isArray(data)) {
        S.adminProviders.phase = "error";
        S.adminProviders.error = { key: "errLoadChannelProviders" };
      } else {
        S.adminProviders.items = data;
        S.adminProviders.phase = "ready";
        S.adminProviders.error = null;
      }
      renderAdmin();
    }, () => {
      if (gen !== generation || S.adminProvidersFor !== id) return;
      S.adminProviders.phase = "error";
      S.adminProviders.error = { key: "errUnavailable" };
      renderAdmin();
    });
  }

  // ---------------------------------------------------------------- wallet
  function canRecharge() {
    const d = S.wallet.data;
    return S.wallet.phase === "ready" && !!d && d.paymentsEnabled && Array.isArray(d.presets) && d.presets.length > 0;
  }

  function loadWallet(gen = generation) {
    if (!S.user || gen !== generation) return Promise.resolve();
    S.wallet.phase = "loading";
    S.wallet.error = null;
    renderAllLists();
    return api("/api/cloud/wallet").then(({ res, data }) => {
      if (gen !== generation) return;
      if (res.status === 401) {
        onSignedOut("errSignIn");
        return;
      }
      if (!res.ok) {
        S.wallet.phase = "error";
        S.wallet.error = errorOf(res, data);
      } else if (
        !data || typeof data !== "object" ||
        typeof data.balanceMicros !== "number" || typeof data.heldMicros !== "number" ||
        typeof data.availableMicros !== "number" ||
        (data.paymentsEnabled && !Array.isArray(data.presets))
      ) {
        S.wallet.phase = "error";
        S.wallet.error = { key: "errLoadWallet" };
      } else {
        const presets = (Array.isArray(data.presets) ? data.presets : []).filter((p) =>
          p && typeof p.id === "string" && typeof p.amount_fen === "number" && Number.isFinite(p.amount_fen) && p.amount_fen > 0
        );
        S.wallet.data = {
          balanceMicros: data.balanceMicros,
          heldMicros: data.heldMicros,
          availableMicros: data.availableMicros,
          currency: typeof data.currency === "string" ? data.currency : "CNY",
          paymentsEnabled: !!data.paymentsEnabled,
          presets: presets,
          providers: Array.isArray(data.providers) ? data.providers : null
        };
        S.wallet.phase = "ready";
        S.wallet.error = null;
        if (!presets.some((p) => String(p.id) === S.presetId)) S.presetId = presets.length ? String(presets[0].id) : "";
      }
      renderAllLists();
    }, () => {
      if (gen !== generation) return;
      S.wallet.phase = "error";
      S.wallet.error = { key: "errUnavailable" };
      renderAllLists();
    });
  }

  function renderWallet() {
    const state = byId("walletState");
    const box = S.wallet;
    const data = box.phase === "ready" ? box.data : null;
    clear(state);
    state.className = "state";
    if (box.phase === "loading") {
      show(state, true);
      state.append(h("span", "spinner"), h("span", "state-text", t("loading")));
    } else if (box.phase === "error") {
      show(state, true);
      state.className = "state error";
      state.append(h("span", "state-text", messageOf(box.error) || t("errLoadWallet")));
      const button = h("button", "btn small", t("retry"));
      button.type = "button";
      button.addEventListener("click", () => loadWallet());
      state.append(button);
    } else {
      show(state, false);
    }

    show(byId("walletBalance"), !!data);
    show(byId("walletUnavailable"), false);
    show(byId("noPresets"), false);
    if (data) {
      byId("walletAvailable").textContent = formatCNY(data.availableMicros, data.currency);
      byId("walletHeld").textContent = formatCNY(data.heldMicros, data.currency);
      byId("walletTotal").textContent = formatCNY(data.balanceMicros, data.currency);
      if (!data.paymentsEnabled) show(byId("walletUnavailable"), true);
      else if (!data.presets.length) show(byId("noPresets"), true);
    }
    show(byId("recharge"), canRecharge() && !S.order);
    if (!canRecharge()) show(byId("rechargeForm"), false);
    renderPresets();
    renderPayProvider();
    setPayBusy(S.payBusy);
  }

  function renderPresets() {
    const box = byId("presets");
    clear(box);
    const data = S.wallet.data;
    const presets = data && data.paymentsEnabled ? data.presets : [];
    if (!presets.length) return;
    if (!presets.some((p) => String(p.id) === S.presetId)) S.presetId = String(presets[0].id);
    for (const preset of presets) {
      const button = h("button", "chip", formatFen(preset.amount_fen, data.currency));
      button.type = "button";
      button.setAttribute("aria-pressed", String(preset.id) === S.presetId ? "true" : "false");
      button.addEventListener("click", () => {
        S.presetId = String(preset.id);
        renderPresets();
        setFormError(byId("payError"), "");
      });
      box.append(button);
    }
  }

  function renderPayProvider() {
    const allowed = S.wallet.data && S.wallet.data.providers;
    for (const button of byId("payProvider").querySelectorAll("button[data-provider]")) {
      const enabled = !allowed || allowed.includes(button.dataset.provider);
      button.hidden = !enabled;
      button.disabled = !enabled;
      button.setAttribute("aria-pressed", enabled && button.dataset.provider === S.payProvider ? "true" : "false");
    }
    if (allowed && !allowed.includes(S.payProvider)) S.payProvider = allowed[0] || "alipay";
  }

  function setPayBusy(on) {
    S.payBusy = on;
    byId("payGo").disabled = on;
    byId("payCancel").disabled = on;
    byId("payGo").textContent = t(on ? "creatingOrder" : "pay");
  }

  function openRechargeForm() {
    if (!canRecharge() || S.order) return;
    setFormError(byId("payError"), "");
    renderPresets();
    renderPayProvider();
    show(byId("rechargeForm"), true);
    const first = byId("presets").querySelector("button");
    if (first) first.focus({ preventScroll: true });
  }

  function closeRechargeForm() {
    show(byId("rechargeForm"), false);
    setFormError(byId("payError"), "");
  }

  async function createOrder() {
    if (S.payBusy || !canRecharge()) return;
    const data = S.wallet.data;
    const preset = data.presets.find((p) => String(p.id) === S.presetId);
    if (!preset) {
      setFormError(byId("payError"), t("errPreset"));
      return;
    }
    const gen = generation;
    setFormError(byId("payError"), "");
    setPayBusy(true);
    clearNotice();
    try {
      const { res, data: reply } = await api("/api/cloud/orders", {
        method: "POST",
        body: { provider: S.payProvider, presetId: String(preset.id) }
      });
      if (gen !== generation) return;
      if (res.status === 401) {
        onSignedOut("errSignIn");
        return;
      }
      if (!res.ok) {
        setFormError(byId("payError"), res.status === 400 || res.status === 503 ? t("errPaymentUnavailable") : messageOf(errorOf(res, reply)));
      } else if (!reply || !reply.checkout || !reply.checkout.order || !reply.checkout.order.merchant_order_no) {
        setFormError(byId("payError"), t("errOrder"));
      } else {
        S.order = reply.checkout;
        S.orderCredited = false;
        S.orderChecking = false;
        if (reply.providerError) notice({ key: "errPaymentProvider" }, "error");
        closeRechargeForm();
        renderWallet();
        renderPayBox();
        scheduleOrderPoll(1500);
      }
    } catch (e) {
      if (gen === generation) setFormError(byId("payError"), t("errUnavailable"));
    } finally {
      if (gen === generation) setPayBusy(false);
    }
  }

  function orderStatusKey(status) {
    if (status === "success") return "orderStatusSuccess";
    if (status === "failed") return "orderStatusFailed";
    if (status === "expired") return "orderStatusExpired";
    return "orderStatusPending";
  }

  function orderStatusKind(status) {
    if (status === "success") return "ok";
    if (status === "failed") return "revoked";
    if (status === "expired") return "off";
    return "warn";
  }

  function renderPayBox() {
    const box = byId("payBox");
    const checkout = S.order;
    if (!checkout || !checkout.order) {
      show(box, false);
      byId("payCode").textContent = "";
      byId("orderAmountLine").textContent = "";
      byId("orderStatus").textContent = "";
      byId("orderStatus").className = "pill";
      byId("orderNote").textContent = "";
      byId("orderNote").className = "order-note";
      show(byId("orderNote"), false);
      for (const id of ["payOpen", "wechatOpen", "payCodeRow", "orderCheck"]) show(byId(id), false);
      byId("payOpen").removeAttribute("href");
      byId("wechatOpen").removeAttribute("href");
      return;
    }
    const order = checkout.order;
    const known = order.status === "pending" || order.status === "success" || order.status === "failed" || order.status === "expired";
    const pending = !known || order.status === "pending";
    show(box, true);
    const pill = byId("orderStatus");
    pill.textContent = t(orderStatusKey(order.status));
    pill.className = "pill " + orderStatusKind(order.status);
    byId("orderAmountLine").textContent = t("orderAmount") + ": " + formatFen(order.amount_fen, order.currency);

    const payURL = typeof checkout.pay_url === "string" && isHttpURL(checkout.pay_url) ? checkout.pay_url : "";
    const code = typeof checkout.code_url === "string" ? checkout.code_url : "";
    const codeURL = isHttpURL(code) ? code : "";
    const openURL = payURL || codeURL;
    const openButton = byId("payOpen");
    show(openButton, pending && !!openURL);
    if (pending && openURL) openButton.href = openURL;
    else openButton.removeAttribute("href");

    const wechat = pending && /^weixin:\/\//i.test(code);
    const wechatButton = byId("wechatOpen");
    show(wechatButton, wechat);
    if (wechat) wechatButton.href = code;
    else wechatButton.removeAttribute("href");

    show(byId("payCodeRow"), pending && !!code);
    byId("payCode").textContent = pending ? code : "";
    byId("payCopy").disabled = !pending || !code;

    let noteKey = "orderNotePending";
    let noteTone = "";
    if (order.status === "success") {
      if (order.credit_state === "credited") {
        noteKey = "orderNoteSuccess";
        noteTone = " ok";
      } else {
        noteKey = "orderNoteCreditPending";
      }
    } else if (order.status === "failed") {
      noteKey = "orderNoteFailed";
      noteTone = " bad";
    } else if (order.status === "expired") {
      noteKey = "orderNoteExpired";
      noteTone = " bad";
    }
    byId("orderNote").textContent = t(noteKey);
    byId("orderNote").className = "order-note" + noteTone;
    show(byId("orderNote"), true);

    const check = byId("orderCheck");
    show(check, pending || (order.status === "success" && order.credit_state === "pending"));
    check.disabled = S.orderChecking;
  }

  function stopOrderPoll() {
    if (orderTimer) {
      clearTimeout(orderTimer);
      orderTimer = 0;
    }
  }

  function scheduleOrderPoll(ms) {
    stopOrderPoll();
    if (!S.order) return;
    const order = S.order.order;
    orderTimer = setTimeout(() => {
      orderTimer = 0;
      if (S.order && S.order.order === order) checkOrder(false);
    }, ms);
  }

  async function checkOrder(manual) {
    const current = S.order && S.order.order;
    if (!current || !current.merchant_order_no || S.orderChecking) return;
    const gen = generation;
    S.orderChecking = true;
    renderPayBox();
    try {
      const { res, data } = await api("/api/cloud/orders/" + encodeURIComponent(current.merchant_order_no));
      if (gen !== generation) return;
      if (res.status === 401) {
        onSignedOut("errSignIn");
        return;
      }
      if (!S.order || S.order.order !== current) return;
      if (!res.ok || !data || typeof data !== "object" || typeof data.merchant_order_no !== "string") {
        if (manual) notice({ key: "errOrder" }, "error");
        else scheduleOrderPoll(5000);
      } else {
        S.order.order = data;
        applyOrder();
      }
    } catch (e) {
      if (manual) notice({ key: "errUnavailable" }, "error");
      else if (S.order) scheduleOrderPoll(5000);
    } finally {
      S.orderChecking = false;
      renderPayBox();
    }
  }

  function applyOrder() {
    const order = S.order && S.order.order;
    if (!order) return;
    renderPayBox();
    if (order.status === "success") {
      if (order.credit_state === "credited") {
        stopOrderPoll();
        if (!S.orderCredited) {
          S.orderCredited = true;
          notice({ key: "rechargeComplete" }, "ok");
          loadWallet();
        }
      } else {
        scheduleOrderPoll(5000);
      }
      return;
    }
    if (order.status === "failed" || order.status === "expired") {
      stopOrderPoll();
      return;
    }
    scheduleOrderPoll(3000);
  }

  function closeOrder() {
    stopOrderPoll();
    S.order = null;
    S.orderCredited = false;
    S.orderChecking = false;
    renderPayBox();
    renderWallet();
    const recharge = byId("recharge");
    if (!recharge.hidden) recharge.focus({ preventScroll: true });
  }

  async function copyPayLink() {
    const code = byId("payCode").textContent;
    const ok = await copyValue(code, byId("payCode"));
    notice({ key: ok ? "copied" : "copyFailed" }, ok ? "ok" : "error");
  }

  function renderAllLists() {
    renderWallet();
    renderWalletOrders();
    renderUsage();
    renderProviders();
    renderHouses();
    renderKeys();
    renderKeyChannels();
    renderGroups();
    renderAdmin();
    renderUsers();
    renderOrders();
    renderAudit();
    renderPolicy();
    renderPayBox();
  }

  function renderState(node, box, retry, emptyTitle, emptyHint) {
    clear(node);
    node.className = "state";
    if (box.phase === "idle") {
      show(node, false);
      return;
    }
    show(node, true);
    if (box.phase === "loading") {
      node.append(h("span", "spinner"), h("span", "state-text", t("loading")));
      return;
    }
    if (box.phase === "error") {
      node.className = "state error";
      node.append(h("span", "state-text", messageOf(box.error) || t("errRequest")));
      const button = h("button", "btn small", t("retry"));
      button.type = "button";
      button.addEventListener("click", () => retry());
      node.append(button);
      return;
    }
    if (box.items.length === 0) {
      node.className = "state empty";
      node.append(h("div", "state-title", t(emptyTitle)), h("div", "state-text", t(emptyHint)));
      return;
    }
    show(node, false);
  }

  function renderProviders() {
    const list = byId("providersList");
    renderState(byId("providersState"), S.providers, loadProviders, "providersEmptyTitle", "providersEmptyHint");
    clear(list);
    if (S.providers.phase !== "ready") return;
    for (const p of S.providers.items) list.append(providerRow(p));
  }

  function providerSub(p) {
    const protocols = [];
    if (p.chat) protocols.push(t("protocolChat"));
    if (p.responses) protocols.push(t("protocolResponses"));
    if (p.anthropic) protocols.push(t("protocolAnthropic"));
    if (p.gemini) protocols.push(t("protocolGemini"));
    const models = Array.isArray(p.models) ? p.models : [];
    const parts = [];
    if (protocols.length) parts.push(protocols.join(" · "));
    parts.push(models.length ? t("modelsCount", { n: models.length }) : t("allModels"));
    return parts.join("  ·  ");
  }

  function providerRow(p, admin) {
    const li = h("li", "row");
    const main = h("div", "row-main");
    const line = h("div", "row-name");
    line.append(h("span", "name", p.name || p.id || ""));
    main.append(line);
    if (p.id) main.append(h("div", "row-sub mono", p.id));
    main.append(h("div", "row-sub", providerSub(p)));
    if (Array.isArray(p.models) && p.models.length) main.title = p.models.join(", ");
    const actions = h("div", "row-actions");
    actions.append(pill(t(p.keySet ? "keySaved" : "keyMissing"), p.keySet ? "ok" : "warn"));
    if (p.off) actions.append(pill(t("paused"), "off"));
    if (!admin) {
      const del = h("button", "btn small danger", t(S.deletingProvider === p.id ? "confirmDelete" : "deleteAction"));
      del.type = "button";
      del.disabled = S.providerBusy;
      del.addEventListener("click", () => {
        if (S.deletingProvider === p.id) deleteProvider(p);
        else {
          S.deletingProvider = p.id;
          renderProviders();
        }
      });
      actions.append(del);
    }
    li.append(main, actions);
    return li;
  }

  function pill(text, kind) {
    return h("span", "pill" + (kind ? " " + kind : ""), text);
  }

  function renderHouses() {
    const list = byId("housesList");
    renderState(byId("housesState"), S.houses, loadHouses, "housesEmptyTitle", "housesEmptyHint");
    clear(list);
    if (S.houses.phase !== "ready") return;
    for (const channel of S.houses.items) list.append(houseRow(channel));
  }

  function houseRow(channel) {
    const li = h("li", "row");
    const main = h("div", "row-main");
    const line = h("div", "row-name");
    line.append(h("span", "name", channel.name || ""));
    main.append(line);
    if (channel.description) main.append(h("div", "row-sub", channel.description));
    const actions = h("div", "row-actions");
    actions.append(pill(t(channel.granted ? "added" : "notAdded"), channel.granted ? "ok" : "off"));
    const button = h("button", "btn small" + (channel.granted ? " ghost" : ""), t(channel.granted ? "remove" : "add"));
    button.type = "button";
    button.disabled = S.housePending === String(channel.id);
    button.addEventListener("click", () => toggleChannel(channel));
    actions.append(button);
    li.append(main, actions);
    return li;
  }

  async function toggleChannel(channel) {
    if (S.housePending) return;
    const gen = generation;
    S.housePending = String(channel.id);
    clearNotice();
    renderHouses();
    let reload = false;
    try {
      const path = "/api/cloud/house-channels/" + encodeURIComponent(channel.id) + "/grant";
      const { res, data } = await api(path, { method: channel.granted ? "DELETE" : "POST" });
      if (gen !== generation) return;
      if (res.status === 401) {
        onSignedOut("errSignIn");
        return;
      }
      if (!res.ok) {
        notice(errorOf(res, data), "error");
        reload = res.status === 404;
      } else {
        channel.granted = !channel.granted;
        notice({ key: channel.granted ? "channelAdded" : "channelRemoved" }, "ok");
      }
    } catch (e) {
      if (gen === generation) notice({ key: "errUnavailable" }, "error");
    }
    S.housePending = "";
    renderHouses();
    renderKeyChannels();
    if (reload) loadHouses();
  }

  function channelLabel(id) {
    if (S.houses.phase === "ready") {
      for (const c of S.houses.items) if (String(c.id) === String(id)) return c.name || String(id);
    }
    if (S.admin.phase === "ready") {
      for (const c of S.admin.items) if (String(c.id) === String(id)) return c.name || String(id);
    }
    return String(id);
  }

  function renderKeys() {
    const list = byId("keysList");
    renderState(byId("keysState"), S.keys, loadKeys, "keysEmptyTitle", "keysEmptyHint");
    clear(list);
    if (S.keys.phase !== "ready") return;
    for (const key of S.keys.items) list.append(keyRow(key));
  }

  function keyRow(key) {
    const li = h("li", "row");
    const main = h("div", "row-main");
    const line = h("div", "row-name");
    line.append(h("span", "name", key.name || ""));
    line.append(pill(t(key.disabled ? "revoked" : "activeKey"), key.disabled ? "revoked" : "ok"));
    main.append(line);
    if (key.masked) main.append(h("div", "row-sub mono", key.masked));
    const models = Array.isArray(key.models) ? key.models : [];
    const channels = Array.isArray(key.houseChannels) ? key.houseChannels : [];
    const scope = [];
    scope.push(models.length ? t("modelsCount", { n: models.length }) : t("allModels"));
    scope.push(channels.length ? channels.map(channelLabel).join(", ") : t("allChannels"));
    main.append(h("div", "row-sub", scope.join("  ·  ")));
    const dates = [];
    if (key.createdAt) dates.push(t("createdOn", { date: fmtDate(key.createdAt) }));
    dates.push(key.expiresAt ? t("expiresOn", { date: fmtDate(key.expiresAt) }) : t("neverExpires"));
    dates.push(key.lastUsedAt ? t("lastUsedOn", { date: fmtDate(key.lastUsedAt) }) : t("neverUsed"));
    main.append(h("div", "row-sub", dates.join("  ·  ")));
    const actions = h("div", "row-actions");
    if (!key.disabled) {
      if (S.revoking === String(key.id)) {
        const yes = h("button", "btn small danger", t("confirmRevoke"));
        yes.type = "button";
        yes.disabled = S.keyBusy;
        yes.addEventListener("click", () => revokeKey(key));
        const no = h("button", "btn small ghost", t("cancel"));
        no.type = "button";
        no.disabled = S.keyBusy;
        no.addEventListener("click", () => {
          S.revoking = "";
          renderKeys();
        });
        actions.append(yes, no);
      } else {
        const button = h("button", "btn small danger", t("revoke"));
        button.type = "button";
        button.disabled = S.keyBusy;
        button.addEventListener("click", () => {
          S.revoking = String(key.id);
          renderKeys();
        });
        actions.append(button);
      }
    }
    li.append(main, actions);
    return li;
  }

  async function revokeKey(key) {
    if (S.keyBusy) return;
    const gen = generation;
    S.keyBusy = true;
    clearNotice();
    renderKeys();
    try {
      const { res, data } = await api("/api/cloud/keys/" + encodeURIComponent(key.id), { method: "DELETE" });
      if (gen !== generation) return;
      if (res.status === 401) {
        onSignedOut("errSignIn");
        return;
      }
      if (!res.ok) {
        notice(errorOf(res, data), "error");
      } else {
        key.disabled = true;
        S.revoking = "";
        notice({ key: "keyRevokedMsg" }, "ok");
      }
    } catch (e) {
      if (gen === generation) notice({ key: "errUnavailable" }, "error");
    }
    S.keyBusy = false;
    renderKeys();
  }

  // ---------------------------------------------------------------- keys form
  function openKeyForm() {
    const form = byId("keyForm");
    form.reset();
    S.keyChannelSel = new Set();
    setFormError(byId("keyError"), "");
    renderKeyChannels();
    show(form, true);
    byId("keyName").focus({ preventScroll: true });
  }

  function closeKeyForm() {
    show(byId("keyForm"), false);
    setFormError(byId("keyError"), "");
  }

  function renderKeyChannels() {
    const box = byId("keyChannels");
    const hint = byId("keyChannelsHint");
    clear(box);
    if (S.houses.phase === "loading" || S.houses.phase === "idle") {
      box.append(h("span", "hint", t("loading")));
      show(hint, false);
      return;
    }
    if (S.houses.phase === "error") {
      // a failed read is unknown, not "no channels": say what a key will
      // allow rather than telling the reader to add one
      box.append(h("span", "hint", t("keyChannelsUnknown")));
      show(hint, false);
      return;
    }
    const granted = S.houses.phase === "ready" ? S.houses.items.filter((c) => c.granted) : [];
    // a channel taken away while the form was open is no longer picked: the
    // key would be refused for a grant the reader no longer has
    for (const id of Array.from(S.keyChannelSel)) {
      if (!granted.some((c) => String(c.id) === id)) S.keyChannelSel.delete(id);
    }
    if (!granted.length) {
      const line = h("span", "hint", t("keyChannelsNone"));
      line.id = "keyChannelsNone";
      box.append(line);
      show(hint, false);
      return;
    }
    show(hint, true);
    for (const channel of granted) {
      const button = h("button", "chip", channel.name || String(channel.id));
      button.type = "button";
      const on = S.keyChannelSel.has(String(channel.id));
      button.setAttribute("aria-pressed", on ? "true" : "false");
      button.addEventListener("click", () => {
        const id = String(channel.id);
        if (S.keyChannelSel.has(id)) S.keyChannelSel.delete(id);
        else S.keyChannelSel.add(id);
        renderKeyChannels();
      });
      box.append(button);
    }
  }

  async function createKey() {
    if (S.keyBusy) return;
    const gen = generation;
    const errorNode = byId("keyError");
    setFormError(errorNode, "");
    const name = byId("keyName").value.trim();
    if (!name) {
      setFormError(errorNode, t("errNameRequired"));
      byId("keyName").focus({ preventScroll: true });
      return;
    }
    const models = readModels(byId("keyModels").value);
    if (models.some((m) => m.length > 300)) {
      setFormError(errorNode, t("errModelsTooLong"));
      return;
    }
    let concurrency = 0;
    const raw = byId("keyConcurrency").value.trim();
    if (raw) {
      if (!/^\d+$/.test(raw)) {
        setFormError(errorNode, t("errConcurrency"));
        return;
      }
      concurrency = Number(raw);
      if (concurrency > 1000) {
        setFormError(errorNode, t("errConcurrency"));
        return;
      }
    }
    const body = {
      name: name,
      models: models,
      houseChannels: Array.from(S.keyChannelSel),
      maxConcurrency: concurrency
    };
    const date = byId("keyExpires").value;
    if (date) {
      const when = new Date(date + "T23:59:59Z");
      if (!Number.isNaN(when.getTime())) body.expiresAt = when.toISOString();
    }
    S.keyBusy = true;
    byId("keySave").disabled = true;
    clearNotice();
    try {
      const { res, data } = await api("/api/cloud/keys", { method: "POST", body: body });
      if (gen !== generation) return;
      if (res.status === 401) {
        onSignedOut("errSignIn");
        return;
      }
      if (!res.ok) {
        setFormError(errorNode, messageOf(errorOf(res, data)));
      } else if (!data || typeof data.secret !== "string" || !data.secret) {
        closeKeyForm();
        notice({ key: "errNoSecret" }, "error");
      } else {
        const view = Object.assign({}, data);
        delete view.secret;
        S.keys.items.push(view);
        if (S.keys.phase !== "ready") S.keys.phase = "ready";
        showSecret(data.secret);
        data.secret = "";
        closeKeyForm();
        notice({ key: "keyCreated" }, "ok");
        renderKeys();
      }
    } catch (e) {
      if (gen === generation) setFormError(errorNode, t("errUnavailable"));
    } finally {
      S.keyBusy = false;
      byId("keySave").disabled = false;
    }
  }

  // The secret exists only in #secretValue's text, from the create reply
  // until Done. It is never written to storage, a log or another element,
  // and no list read can bring it back.
  function showSecret(secret) {
    byId("secretValue").textContent = secret;
    byId("secretStatus").textContent = "";
    show(byId("secretStatus"), false);
    show(byId("secretBox"), true);
    // focus the secret so a screen reader reads it, and the copy button is
    // the next stop
    byId("secretValue").focus({ preventScroll: true });
  }

  function clearSecret(focusBack) {
    const had = !byId("secretBox").hidden;
    byId("secretValue").textContent = "";
    byId("secretStatus").textContent = "";
    show(byId("secretStatus"), false);
    show(byId("secretBox"), false);
    if (focusBack && had) byId("newKey").focus({ preventScroll: true });
  }

  async function copySecret() {
    const value = byId("secretValue").textContent;
    if (!value) return;
    const ok = await copyValue(value, byId("secretValue"));
    const status = byId("secretStatus");
    status.textContent = t(ok ? "copied" : "copyFailed");
    show(status, true);
  }

  // ---------------------------------------------------------------- provider forms
  function setProtocol(group, protocol) {
    for (const button of group.querySelectorAll("button[data-protocol]")) {
      button.setAttribute("aria-pressed", button.dataset.protocol === protocol ? "true" : "false");
    }
  }

  function bindProtocol(groupId, onChange) {
    const group = byId(groupId);
    group.addEventListener("click", (event) => {
      const button = event.target.closest("button[data-protocol]");
      if (!button) return;
      setProtocol(group, button.dataset.protocol);
      onChange(button.dataset.protocol);
    });
  }

  function openProviderForm() {
    const form = byId("providerForm");
    form.reset();
    setProtocol(byId("pProtocol"), "chat");
    S.providerProtocol = "chat";
    pIdAuto = true;
    setFormError(byId("pError"), "");
    show(form, true);
    byId("pName").focus({ preventScroll: true });
  }

  function closeProviderForm() {
    show(byId("providerForm"), false);
    setFormError(byId("pError"), "");
    byId("pKey").value = "";
  }

  let pIdAuto = true;

  async function createProvider() {
    const gen = generation;
    const errorNode = byId("pError");
    setFormError(errorNode, "");
    const name = byId("pName").value.trim();
    const id = slugify(byId("pId").value.trim() || name);
    const base = byId("pBase").value.trim();
    const key = byId("pKey").value.trim();
    const models = readModels(byId("pModels").value);
    if (!name) {
      setFormError(errorNode, t("errNameRequired"));
      return;
    }
    if (!id) {
      setFormError(errorNode, t("errIdRequired"));
      return;
    }
    if (!base) {
      setFormError(errorNode, t("errEndpointRequired"));
      return;
    }
    if (!isHttpURL(base)) {
      setFormError(errorNode, t("errEndpointInvalid"));
      return;
    }
    if (!key) {
      setFormError(errorNode, t("errKeyRequired"));
      return;
    }
    if (models.some((m) => m.length > 300)) {
      setFormError(errorNode, t("errModelsTooLong"));
      return;
    }
    const body = { id: id, name: name, models: models, key: key };
    body[S.providerProtocol] = base;
    const save = byId("pSave");
    save.disabled = true;
    clearNotice();
    try {
      const { res, data } = await api("/api/cloud/providers", { method: "POST", body: body });
      if (gen !== generation) return;
      if (res.status === 401) {
        onSignedOut("errSignIn");
        return;
      }
      if (!res.ok) {
        setFormError(errorNode, messageOf(errorOf(res, data)));
      } else if (data && typeof data === "object") {
        // the key is gone from the page the moment the server has it
        byId("pKey").value = "";
        S.providers.items.push(data);
        if (S.providers.phase !== "ready") S.providers.phase = "ready";
        closeProviderForm();
        notice({ key: "providerAdded" }, "ok");
        renderProviders();
      } else {
        setFormError(errorNode, t("errRequest"));
      }
    } catch (e) {
      if (gen === generation) setFormError(errorNode, t("errUnavailable"));
    } finally {
      save.disabled = false;
    }
  }

  function openHouseProviderForm(channel) {
    S.houseProviderFor = String(channel.id);
    S.houseProviderName = channel.name || "";
    const form = byId("houseProviderForm");
    form.reset();
    setProtocol(byId("hpProtocol"), "chat");
    S.hpProtocol = "chat";
    hpIdAuto = true;
    byId("hpChannel").value = String(channel.id);
    renderHouseProviderTitle();
    setFormError(byId("hpError"), "");
    show(byId("channelForm"), false);
    show(form, true);
    byId("hpName").focus({ preventScroll: true });
  }

  function renderHouseProviderTitle() {
    if (!S.houseProviderFor) {
      byId("hpTitle").textContent = "";
      return;
    }
    byId("hpTitle").textContent = t("houseProviderTitle", { name: S.houseProviderName });
  }

  function closeHouseProviderForm() {
    show(byId("houseProviderForm"), false);
    setFormError(byId("hpError"), "");
    byId("hpKey").value = "";
    S.houseProviderFor = "";
    S.houseProviderName = "";
  }

  let hpIdAuto = true;

  async function createHouseProvider() {
    const gen = generation;
    const channelID = S.houseProviderFor || byId("hpChannel").value;
    const errorNode = byId("hpError");
    setFormError(errorNode, "");
    const name = byId("hpName").value.trim();
    const id = slugify(byId("hpId").value.trim() || name);
    const base = byId("hpBase").value.trim();
    const key = byId("hpKey").value.trim();
    const models = readModels(byId("hpModels").value);
    if (!channelID) {
      setFormError(errorNode, t("errRequest"));
      return;
    }
    if (!name) {
      setFormError(errorNode, t("errNameRequired"));
      return;
    }
    if (!id) {
      setFormError(errorNode, t("errIdRequired"));
      return;
    }
    if (!base) {
      setFormError(errorNode, t("errEndpointRequired"));
      return;
    }
    if (!isHttpURL(base)) {
      setFormError(errorNode, t("errEndpointInvalid"));
      return;
    }
    if (!key) {
      setFormError(errorNode, t("errKeyRequired"));
      return;
    }
    if (models.some((m) => m.length > 300)) {
      setFormError(errorNode, t("errModelsTooLong"));
      return;
    }
    const body = { id: id, name: name, models: models, key: key };
    body[S.hpProtocol] = base;
    const save = byId("hpSave");
    save.disabled = true;
    clearNotice();
    try {
      const { res, data } = await api(
        "/api/cloud/admin/house-channels/" + encodeURIComponent(channelID) + "/providers",
        { method: "POST", body: body }
      );
      if (gen !== generation) return;
      if (res.status === 401) {
        onSignedOut("errSignIn");
        return;
      }
      if (!res.ok) {
        setFormError(errorNode, messageOf(errorOf(res, data)));
      } else {
        byId("hpKey").value = "";
        if (data && typeof data === "object" && data.id) {
          S.lastHouseProvider = { channel: String(channelID), id: String(data.id), name: String(data.name || data.id) };
        }
        closeHouseProviderForm();
        notice({ key: "houseProviderAdded" }, "ok");
        renderAdmin();
      }
    } catch (e) {
      if (gen === generation) setFormError(errorNode, t("errUnavailable"));
    } finally {
      save.disabled = false;
    }
  }

  // ------------------------------------------------------- auth policy
  // The administrator's four switches. Every read is validated as a whole;
  // a write carries the version it was read at, so two administrators
  // cannot silently overwrite each other.
  function readAdminPolicy(data) {
    if (!data || typeof data !== "object") return null;
    if (typeof data.registrationEnabled !== "boolean") return null;
    if (typeof data.passwordLoginEnabled !== "boolean") return null;
    if (typeof data.codeLoginEnabled !== "boolean") return null;
    const min = Number(data.passwordMinLength);
    if (!Number.isInteger(min) || min < 8 || min > 72) return null;
    const version = Number(data.version);
    if (!Number.isInteger(version) || version < 0) return null;
    const users = Number(data.usersWithoutPassword);
    if (!Number.isInteger(users) || users < 0) return null;
    if (typeof data.mailReady !== "boolean") return null;
    return {
      registrationEnabled: data.registrationEnabled,
      passwordLoginEnabled: data.passwordLoginEnabled,
      codeLoginEnabled: data.codeLoginEnabled,
      passwordMinLength: min,
      version: version,
      usersWithoutPassword: users,
      mailReady: data.mailReady
    };
  }

  function loadPolicy(gen = generation) {
    if (!isAdmin() || gen !== generation) return Promise.resolve();
    S.policy.phase = "loading";
    S.policy.error = null;
    renderPolicy();
    return api("/api/cloud/admin/auth-policy").then(({ res, data }) => {
      if (gen !== generation) return;
      if (res.status === 401) {
        onSignedOut();
        return;
      }
      if (!res.ok) {
        S.policy.phase = "error";
        S.policy.error = errorOf(res, data);
      } else {
        const read = readAdminPolicy(data);
        if (!read) {
          S.policy.phase = "error";
          S.policy.error = { key: "errLoadPolicy" };
        } else {
          S.policy.data = read;
          S.policy.phase = "ready";
          S.policy.error = null;
        }
      }
      renderPolicy();
    }, () => {
      if (gen !== generation) return;
      S.policy.phase = "error";
      S.policy.error = { key: "errUnavailable" };
      renderPolicy();
    });
  }

  function policySwitch(id) {
    return byId(id);
  }

  function policyDraft() {
    return {
      registrationEnabled: policySwitch("policyRegistration").getAttribute("aria-checked") === "true",
      passwordLoginEnabled: policySwitch("policyPassword").getAttribute("aria-checked") === "true",
      codeLoginEnabled: policySwitch("policyCode").getAttribute("aria-checked") === "true",
      passwordMinLength: Number(byId("policyMinLength").value)
    };
  }

  function renderPolicyFacts() {
    const data = S.policy.data;
    if (!data) return;
    const draft = policyDraft();
    setPipeText(byId("policyUsersNoPassword"), t("policyNoPasswordUsers", { n: formatCount(data.usersWithoutPassword) }));
    const mail = byId("policyMail");
    mail.textContent = t(data.mailReady ? "policyMailReady" : "policyMailMissing");
    mail.className = "pill " + (data.mailReady ? "ok" : "warn");
    const warn = byId("policyWarning");
    if (!draft.codeLoginEnabled && data.usersWithoutPassword > 0) {
      setPipeText(warn, t("policyCodeOffWarning", { n: formatCount(data.usersWithoutPassword) }));
      show(warn, true);
    } else {
      setPipeText(warn, "");
      show(warn, false);
    }
    setPipeText(byId("policyVersion"), t("policyVersion", { n: formatCount(data.version) }));
  }

  function populatePolicy(data) {
    for (const [id, on] of [
      ["policyRegistration", data.registrationEnabled],
      ["policyPassword", data.passwordLoginEnabled],
      ["policyCode", data.codeLoginEnabled]
    ]) {
      policySwitch(id).setAttribute("aria-checked", on ? "true" : "false");
    }
    byId("policyMinLength").value = String(data.passwordMinLength);
    setFormError(byId("policyError"), "");
  }

  function renderPolicy() {
    const panel = byId("policyPanel");
    if (!isAdmin()) {
      show(panel, false);
      return;
    }
    show(panel, true);
    const box = S.policy;
    const state = byId("policyState");
    clear(state);
    state.className = "state";
    show(byId("policyForm"), box.phase === "ready");
    show(byId("policyVersion"), box.phase === "ready");
    if (box.phase === "loading") {
      show(state, true);
      state.append(h("span", "spinner"), h("span", "state-text", t("loading")));
      return;
    }
    if (box.phase === "error") {
      show(state, true);
      state.className = "state error";
      state.append(h("span", "state-text", messageOf(box.error) || t("errLoadPolicy")));
      const retry = h("button", "btn small", t("retry"));
      retry.type = "button";
      retry.addEventListener("click", () => loadPolicy());
      state.append(retry);
      return;
    }
    show(state, false);
    if (box.phase !== "ready") return;
    if (box.shownFor !== box.data) {
      box.shownFor = box.data;
      populatePolicy(box.data);
    }
    renderPolicyFacts();
  }

  function togglePolicySwitch(id) {
    const button = policySwitch(id);
    if (!button || S.policy.saving) return;
    button.setAttribute("aria-checked", button.getAttribute("aria-checked") === "true" ? "false" : "true");
    setFormError(byId("policyError"), "");
    renderPolicyFacts();
  }

  async function savePolicy() {
    const data = S.policy.data;
    if (!data || S.policy.saving) return;
    const draft = policyDraft();
    if (!Number.isInteger(draft.passwordMinLength) || draft.passwordMinLength < 8 || draft.passwordMinLength > 72) {
      setFormError(byId("policyError"), t("errPolicyMinLength"));
      byId("policyMinLength").focus({ preventScroll: true });
      return;
    }
    if (!draft.passwordLoginEnabled && !draft.codeLoginEnabled) {
      setFormError(byId("policyError"), t("policyAtLeastOne"));
      return;
    }
    const gen = generation;
    S.policy.saving = true;
    byId("policySave").disabled = true;
    clearNotice();
    try {
      const { res, data: reply } = await api("/api/cloud/admin/auth-policy", {
        method: "PUT",
        body: {
          registrationEnabled: draft.registrationEnabled,
          passwordLoginEnabled: draft.passwordLoginEnabled,
          codeLoginEnabled: draft.codeLoginEnabled,
          passwordMinLength: draft.passwordMinLength,
          version: data.version
        }
      });
      if (gen !== generation) return;
      if (res.status === 401) {
        onSignedOut();
        return;
      }
      if (res.status === 409) {
        // the read that comes with the answer is newer than the draft; the
        // reader sees why their save did not land after it is drawn
        await loadPolicy();
        setFormError(byId("policyError"), t("errPolicyConflict"));
        return;
      }
      if (!res.ok) {
        setFormError(byId("policyError"), messageOf(errorOf(res, reply)));
        return;
      }
      const read = readAdminPolicy(reply);
      if (read) {
        S.policy.data = read;
        S.policy.shownFor = null;
      } else {
        await loadPolicy();
      }
      notice({ key: "policySaved" }, "ok");
    } catch (e) {
      if (gen === generation) setFormError(byId("policyError"), t("errUnavailable"));
    } finally {
      S.policy.saving = false;
      byId("policySave").disabled = false;
      renderPolicy();
    }
  }

  // ---------------------------------------------------------------- admin
  function renderAdmin() {
    const panel = byId("adminPanel");
    if (!isAdmin()) {
      show(panel, false);
      return;
    }
    show(panel, true);
    if (S.pricesFor && S.admin.phase === "ready" && !S.admin.items.some((c) => String(c.id) === S.pricesFor)) {
      S.pricesFor = "";
      S.prices = { phase: "idle", items: [], error: null };
      S.priceFormOpen = false;
    }
    const list = byId("adminList");
    renderState(byId("adminState"), S.admin, loadAdmin, "adminEmptyTitle", "adminEmptyHint");
    clear(list);
    if (S.admin.phase !== "ready") return;
    for (const channel of S.admin.items) {
      list.append(adminRow(channel));
      if (S.adminProvidersFor === String(channel.id)) list.append(adminProvidersPanel(channel));
      if (S.pricesFor === String(channel.id)) list.append(pricePanel(channel));
    }
  }

  function statusKey(status) {
    if (status === "published") return "statusPublished";
    if (status === "offline") return "statusOffline";
    return "statusDraft";
  }

  function statusPillKind(status) {
    if (status === "published") return "ok";
    if (status === "draft") return "warn";
    return "off";
  }

  function adminRow(channel) {
    const li = h("li", "row");
    const main = h("div", "row-main");
    const line = h("div", "row-name");
    line.append(h("span", "name", channel.name || ""));
    line.append(pill(t(statusKey(channel.status)), statusPillKind(channel.status)));
    main.append(line);
    if (channel.description) main.append(h("div", "row-sub", channel.description));
    if (S.lastHouseProvider && S.lastHouseProvider.channel === String(channel.id) && S.lastHouseProvider.id) {
      main.append(h("div", "row-sub", t("lastProvider", { name: S.lastHouseProvider.name, id: S.lastHouseProvider.id })));
    }
    const actions = h("div", "row-actions");
    const seg = h("div", "seg");
    seg.setAttribute("role", "group");
    seg.setAttribute("aria-label", t("statusLabel"));
    for (const status of STATUSES) {
      const button = h("button", "", t(statusKey(status)));
      button.type = "button";
      button.dataset.status = status;
      button.setAttribute("aria-pressed", channel.status === status ? "true" : "false");
      button.disabled = S.statusPending === String(channel.id);
      button.addEventListener("click", () => setChannelStatus(channel, status));
      seg.append(button);
    }
    actions.append(seg);
    const prices = h("button", "btn small ghost", t("managePrices"));
    prices.type = "button";
    prices.setAttribute("aria-expanded", S.pricesFor === String(channel.id) ? "true" : "false");
    prices.disabled = S.statusPending === String(channel.id);
    prices.addEventListener("click", () => togglePrices(channel));
    actions.append(prices);
    const add = h("button", "btn small", t("addHouseProvider"));
    add.type = "button";
    add.disabled = S.statusPending === String(channel.id);
    add.addEventListener("click", () => openHouseProviderForm(channel));
    actions.append(add);
    const providers = h("button", "btn small ghost", t("providersLabel"));
    providers.type = "button";
    providers.setAttribute("aria-expanded", S.adminProvidersFor === String(channel.id) ? "true" : "false");
    providers.addEventListener("click", () => toggleAdminProviders(channel));
    actions.append(providers);
    li.append(main, actions);
    return li;
  }

  // ---------------------------------------------------------------- prices
  function togglePrices(channel) {
    const id = String(channel.id);
    if (S.pricesFor === id) {
      closePrices();
      return;
    }
    S.pricesFor = id;
    S.prices = { phase: "loading", items: [], error: null };
    S.priceFormOpen = false;
    closeChannelForm();
    closeHouseProviderForm();
    renderAdmin();
    loadPrices();
  }

  function closePrices() {
    S.pricesFor = "";
    S.prices = { phase: "idle", items: [], error: null };
    S.priceFormOpen = false;
    renderAdmin();
  }

  function loadPrices() {
    const id = S.pricesFor;
    if (!id || !isAdmin()) return;
    const gen = generation;
    S.prices.phase = "loading";
    S.prices.error = null;
    renderAdmin();
    api("/api/cloud/admin/house-channels/" + encodeURIComponent(id) + "/prices").then(({ res, data }) => {
      if (gen !== generation || S.pricesFor !== id) return;
      if (res.status === 401) {
        onSignedOut("errSignIn");
        return;
      }
      if (!res.ok) {
        S.prices.phase = "error";
        S.prices.error = errorOf(res, data);
      } else if (!Array.isArray(data)) {
        S.prices.phase = "error";
        S.prices.error = { key: "errLoadPrices" };
      } else {
        S.prices.items = data;
        S.prices.phase = "ready";
        S.prices.error = null;
      }
      renderAdmin();
    }, () => {
      if (gen !== generation || S.pricesFor !== id) return;
      S.prices.phase = "error";
      S.prices.error = { key: "errUnavailable" };
      renderAdmin();
    });
  }

  function pricePanel(channel) {
    const li = h("li", "price-panel");
    const head = h("div", "price-head");
    head.append(h("h3", "", t("pricesFor", { name: channel.name || "" })));
    const close = h("button", "btn small ghost", t("closeOrder"));
    close.type = "button";
    close.addEventListener("click", closePrices);
    head.append(close);
    li.append(head);

    const box = S.prices;
    if (box.phase === "loading") {
      const state = h("div", "state");
      state.append(h("span", "spinner"), h("span", "state-text", t("loading")));
      li.append(state);
      return li;
    }
    if (box.phase === "error") {
      const state = h("div", "state error");
      state.append(h("span", "state-text", messageOf(box.error) || t("errLoadPrices")));
      const retry = h("button", "btn small", t("retry"));
      retry.type = "button";
      retry.addEventListener("click", () => loadPrices());
      state.append(retry);
      li.append(state);
      return li;
    }
    if (box.phase !== "ready") return li;
    if (box.items.length) {
      const list = h("ul", "price-list");
      for (const item of box.items) list.append(priceItem(item));
      li.append(list);
    } else {
      const state = h("div", "state empty");
      state.append(h("div", "state-title", t("pricesEmpty")), h("div", "state-text", t("pricesEmptyHint")));
      li.append(state);
    }
    if (S.priceFormOpen) {
      li.append(priceForm(channel));
    } else {
      const add = h("button", "btn small", t("addPrice"));
      add.type = "button";
      add.addEventListener("click", () => {
        S.priceFormOpen = true;
        renderAdmin();
        const first = byId("priceProvider");
        if (first) first.focus({ preventScroll: true });
      });
      li.append(add);
    }
    return li;
  }

  function priceItem(item) {
    const li = h("li", "price-item");
    li.append(h("div", "mono", (item.provider || "") + " / " + (item.model || "")));
    const price = item.price || {};
    li.append(h("div", "price-rates", t("priceRates", {
      input: formatCNY(Number(price.input_micros_per_mtok) || 0, "CNY"),
      output: formatCNY(Number(price.output_micros_per_mtok) || 0, "CNY"),
      cacheRead: formatCNY(Number(price.cache_read_micros_per_mtok) || 0, "CNY"),
      cacheWrite: formatCNY(Number(price.cache_write_micros_per_mtok) || 0, "CNY"),
      fixed: formatCNY(Number(price.fixed_per_call_micros) || 0, "CNY")
    })));
    return li;
  }

  function priceField(id, labelKey, placeholderKey, mono, decimal) {
    const label = h("label", "field");
    label.setAttribute("for", id);
    const input = h("input", "input" + (mono ? " mono" : ""));
    input.id = id;
    input.type = "text";
    input.autocomplete = "off";
    input.spellcheck = false;
    if (mono) input.setAttribute("autocapitalize", "off");
    if (decimal) input.setAttribute("inputmode", "decimal");
    if (placeholderKey) input.placeholder = t(placeholderKey);
    label.append(h("span", "", t(labelKey)), input);
    return label;
  }

  function priceForm(channel) {
    const form = h("form", "form");
    form.noValidate = true;
    const grid = h("div", "form-grid");
    grid.append(
      priceField("priceProvider", "priceProviderLabel", "priceProviderPlaceholder", true, false),
      priceField("priceModel", "priceModelLabel", "priceModelPlaceholder", true, false)
    );
    form.append(grid);
    const rates = h("div", "form-grid");
    rates.append(
      priceField("priceInput", "priceInputLabel", null, false, true),
      priceField("priceOutput", "priceOutputLabel", null, false, true),
      priceField("priceCacheRead", "priceCacheReadLabel", null, false, true),
      priceField("priceCacheWrite", "priceCacheWriteLabel", null, false, true),
      priceField("priceFixed", "priceFixedLabel", null, false, true)
    );
    form.append(rates, h("p", "hint", t("priceHint")));
    const error = h("p", "form-error");
    error.id = "priceError";
    error.setAttribute("role", "alert");
    error.hidden = true;
    form.append(error);
    const actions = h("div", "form-actions");
    const save = h("button", "btn primary", t("save"));
    save.type = "submit";
    save.id = "priceSave";
    const cancel = h("button", "btn ghost", t("cancel"));
    cancel.type = "button";
    cancel.id = "priceCancel";
    cancel.addEventListener("click", () => {
      S.priceFormOpen = false;
      renderAdmin();
    });
    actions.append(save, cancel);
    form.append(actions);
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      savePrice(channel);
    });
    if (S.lastHouseProvider && S.lastHouseProvider.channel === String(channel.id)) {
      const provider = form.querySelector("#priceProvider");
      if (provider) provider.value = S.lastHouseProvider.id;
    }
    return form;
  }

  async function savePrice(channel) {
    const errorNode = byId("priceError");
    setFormError(errorNode, "");
    const provider = byId("priceProvider").value.trim();
    const model = byId("priceModel").value.trim();
    if (!provider) {
      setFormError(errorNode, t("errPriceProvider"));
      byId("priceProvider").focus({ preventScroll: true });
      return;
    }
    if (!model) {
      setFormError(errorNode, t("errPriceModel"));
      byId("priceModel").focus({ preventScroll: true });
      return;
    }
    const rates = ["priceInput", "priceOutput", "priceCacheRead", "priceCacheWrite", "priceFixed"].map((id) => parseCNYMicros(byId(id).value));
    if (rates.some((v) => v === null)) {
      setFormError(errorNode, t("errPriceAmount"));
      return;
    }
    const body = {
      provider: provider,
      model: model,
      price: {
        input_micros_per_mtok: rates[0],
        output_micros_per_mtok: rates[1],
        cache_read_micros_per_mtok: rates[2],
        cache_write_micros_per_mtok: rates[3],
        fixed_per_call_micros: rates[4]
      }
    };
    const id = String(channel.id);
    const gen = generation;
    const save = byId("priceSave");
    save.disabled = true;
    clearNotice();
    try {
      const { res, data } = await api(
        "/api/cloud/admin/house-channels/" + encodeURIComponent(id) + "/prices",
        { method: "PUT", body: body }
      );
      if (gen !== generation || S.pricesFor !== id) return;
      if (res.status === 401) {
        onSignedOut("errSignIn");
        return;
      }
      if (!res.ok) {
        setFormError(errorNode, res.status === 404 ? t("errPriceProviderUnknown") : messageOf(errorOf(res, data)));
      } else {
        S.priceFormOpen = false;
        notice({ key: "priceSaved" }, "ok");
        loadPrices();
      }
    } catch (e) {
      if (gen === generation && S.pricesFor === id) setFormError(errorNode, t("errUnavailable"));
    } finally {
      save.disabled = false;
    }
  }

  async function setChannelStatus(channel, status) {
    if (S.statusPending || channel.status === status) return;
    const gen = generation;
    S.statusPending = String(channel.id);
    clearNotice();
    renderAdmin();
    try {
      const { res, data } = await api(
        "/api/cloud/admin/house-channels/" + encodeURIComponent(channel.id) + "/status",
        { method: "POST", body: { status: status } }
      );
      if (gen !== generation) return;
      if (res.status === 401) {
        onSignedOut("errSignIn");
        return;
      }
      if (!res.ok) {
        notice(errorOf(res, data), "error");
      } else {
        channel.status = status;
        notice({ key: "statusSaved", vars: { status: t(statusKey(status)) } }, "ok");
        // the reader's own list shows published channels only
        loadHouses();
      }
    } catch (e) {
      if (gen === generation) notice({ key: "errUnavailable" }, "error");
    }
    S.statusPending = "";
    renderAdmin();
  }

  function openChannelForm() {
    const form = byId("channelForm");
    form.reset();
    setFormError(byId("chError"), "");
    show(byId("houseProviderForm"), false);
    show(form, true);
    byId("chName").focus({ preventScroll: true });
  }

  function closeChannelForm() {
    show(byId("channelForm"), false);
    setFormError(byId("chError"), "");
  }

  async function createChannel() {
    const gen = generation;
    const errorNode = byId("chError");
    setFormError(errorNode, "");
    const name = byId("chName").value.trim();
    if (!name) {
      setFormError(errorNode, t("errNameRequired"));
      byId("chName").focus({ preventScroll: true });
      return;
    }
    const description = byId("chDesc").value.trim();
    const save = byId("chSave");
    save.disabled = true;
    clearNotice();
    try {
      const { res, data } = await api("/api/cloud/admin/house-channels", {
        method: "POST",
        body: { name: name, description: description }
      });
      if (gen !== generation) return;
      if (res.status === 401) {
        onSignedOut("errSignIn");
        return;
      }
      if (!res.ok) {
        setFormError(errorNode, messageOf(errorOf(res, data)));
      } else if (data && typeof data === "object") {
        S.admin.items.push(data);
        if (S.admin.phase !== "ready") S.admin.phase = "ready";
        closeChannelForm();
        notice({ key: "channelCreated" }, "ok");
        renderAdmin();
      } else {
        setFormError(errorNode, t("errRequest"));
      }
    } catch (e) {
      if (gen === generation) setFormError(errorNode, t("errUnavailable"));
    } finally {
      save.disabled = false;
    }
  }

  function renderWalletOrders() {
    const box = byId("walletOrders");
    clear(box);
    if (!S.walletOrders || S.walletOrders.phase !== "ready") return;
    for (const order of S.walletOrders.items) {
      const row = h("div", "row");
      const main = h("div", "row-main");
      main.append(h("div", "row-name", h("span", "name", order.merchant_order_no || "")));
      main.append(h("div", "row-sub", (order.provider || "") + " · " + formatCNY(order.amount_micro_cny || 0, "CNY") + " · " + fmtDate(order.created_at)));
      const status = order.credit_state === "credited" ? "creditCredited" : "creditPending";
      row.append(main, h("div", "row-actions", pill(t(status), order.credit_state === "credited" ? "ok" : "warn")));
      box.append(row);
    }
  }

  // ---------------------------------------------------------- usage/groups
  function renderUsage() {
    const list = byId("usageList");
    renderState(byId("usageState"), S.usage, loadUsage, "usageEmpty", "usageEmptyHint");
    clear(list);
    if (S.usage.phase !== "ready") return;
    for (const row of S.usage.items) {
      const li = h("li", "row");
      const main = h("div", "row-main");
      main.append(h("div", "row-name", h("span", "name", row.model || row.provider || row.requestId || "")));
      main.append(h("div", "row-sub mono", (row.provider || "") + (row.channel ? " · " + channelLabel(row.channel) : "")));
      main.append(h("div", "row-sub", t("usageTokens", { input: row.inputTokens || 0, output: row.outputTokens || 0, cacheRead: row.cacheReadTokens || 0, cacheWrite: row.cacheWriteTokens || 0 })));
      main.append(h("div", "row-sub", fmtDate(row.createdAt)));
      li.append(main, h("div", "row-actions", pill(formatCNY(row.costMicros || 0, "CNY"), "ok")));
      list.append(li);
    }
  }

  function renderGroups() {
    show(byId("groupForm"), S.groupFormOpen);
    const list = byId("groupsList");
    renderState(byId("groupsState"), S.groups, loadGroups, "groupsEmpty", "groupsEmptyHint");
    clear(list);
    if (S.groups.phase !== "ready") return;
    for (const group of S.groups.items) {
      const li = h("li", "row");
      const main = h("div", "row-main");
      main.append(h("div", "row-name", h("span", "name", group.name || group.id || "")));
      main.append(h("div", "row-sub mono", group.id || ""));
      if (Array.isArray(group.members)) main.append(h("div", "row-sub", group.members.join(" · ")));
      const actions = h("div", "row-actions");
      const del = h("button", "btn small danger", t(S.deletingGroup === group.id ? "confirmDelete" : "deleteAction"));
      del.type = "button";
      del.disabled = S.groupBusy;
      del.addEventListener("click", () => {
        if (S.deletingGroup === group.id) deleteGroup(group);
        else { S.deletingGroup = group.id; renderGroups(); }
      });
      actions.append(del);
      li.append(main, actions);
      list.append(li);
    }
  }

  async function createGroup() {
    if (S.groupBusy) return;
    const name = byId("gName").value.trim();
    const id = slugify(byId("gId").value.trim() || name);
    const members = readModels(byId("gMembers").value);
    if (!name || !id || !members.length) { setFormError(byId("groupError"), t("errGroup")); return; }
    if (members.some((m) => !m.includes("/"))) { setFormError(byId("groupError"), t("errGroupMember")); return; }
    S.groupBusy = true;
    setFormError(byId("groupError"), "");
    try {
      const { res, data } = await api("/api/cloud/groups", { method: "POST", body: { id: id, name: name, members: members, routing: "order" } });
      if (!res.ok) { setFormError(byId("groupError"), messageOf(errorOf(res, data))); return; }
      S.groupFormOpen = false;
      notice({ key: "groupCreated" }, "ok");
      await loadGroups();
    } catch { setFormError(byId("groupError"), t("errUnavailable")); } finally { S.groupBusy = false; renderGroups(); }
  }

  async function deleteGroup(group) {
    if (S.groupBusy) return;
    S.groupBusy = true;
    try {
      const { res, data } = await api("/api/cloud/groups/" + encodeURIComponent(group.id), { method: "DELETE" });
      if (!res.ok) { notice(errorOf(res, data), "error"); return; }
      S.deletingGroup = "";
      notice({ key: "groupDeleted" }, "ok");
      await loadGroups();
    } catch { notice({ key: "errUnavailable" }, "error"); } finally { S.groupBusy = false; renderGroups(); }
  }

  async function deleteProvider(provider) {
    if (S.providerBusy) return;
    S.providerBusy = true;
    try {
      const { res, data } = await api("/api/cloud/providers/" + encodeURIComponent(provider.id), { method: "DELETE" });
      if (!res.ok) { notice(errorOf(res, data), "error"); return; }
      S.deletingProvider = "";
      notice({ key: "providerDeleted" }, "ok");
      await loadProviders();
    } catch { notice({ key: "errUnavailable" }, "error"); } finally { S.providerBusy = false; renderProviders(); }
  }

  // ------------------------------------------------------------- admin users
  function renderUsers() {
    const list = byId("usersList");
    renderState(byId("usersState"), S.users, loadUsers, "usersEmpty", "usersEmptyHint");
    clear(list);
    if (S.users.phase !== "ready") return;
    for (const user of S.users.items) {
      const li = h("li", "row");
      const main = h("div", "row-main");
      main.append(h("div", "row-name", h("span", "name", user.email || user.id || ""), pill(t(user.status === "active" ? "accountActive" : "accountDisabled"), user.status === "active" ? "ok" : "off")));
      main.append(h("div", "row-sub", (user.role || "user") + " · " + formatCNY(user.balanceMicros || 0, "CNY") + " · " + fmtDate(user.createdAt)));
      const actions = h("div", "row-actions");
      const toggle = h("button", "btn small", t(user.status === "active" ? "userDisable" : "userEnable"));
      toggle.type = "button";
      toggle.disabled = S.userStatusBusy === user.id;
      toggle.addEventListener("click", () => setUserStatus(user, user.status === "active" ? "disabled" : "active"));
      const credit = h("button", "btn small ghost", t("credit"));
      credit.type = "button";
      credit.setAttribute("aria-expanded", S.creditFor === user.id ? "true" : "false");
      credit.addEventListener("click", () => { S.creditFor = S.creditFor === user.id ? "" : user.id; S.creditRequestId = ""; renderUsers(); });
      actions.append(toggle, credit);
      li.append(main, actions);
      list.append(li);
      if (S.creditFor === user.id) list.append(creditPanel(user));
    }
  }

  function creditPanel(user) {
    const li = h("li", "credit-panel");
    const form = h("form", "form");
    form.noValidate = true;
    form.append(h("p", "form-title", t("creditTitle") + " · " + user.email));
    const amount = h("input", "input");
    amount.id = "creditAmount";
    amount.inputMode = "decimal";
    amount.placeholder = "10.00";
    amount.setAttribute("aria-label", t("creditAmountLabel"));
    const reason = h("input", "input");
    reason.id = "creditReason";
    reason.placeholder = t("creditReasonPlaceholder");
    reason.setAttribute("aria-label", t("creditReasonLabel"));
    const error = h("p", "form-error");
    error.id = "creditError";
    error.hidden = true;
    const save = h("button", "btn primary", t("credit"));
    save.type = "submit";
    form.append(amount, reason, error, save);
    form.addEventListener("submit", (event) => { event.preventDefault(); creditUser(user, amount.value, reason.value, error, save); });
    li.append(form);
    return li;
  }

  async function setUserStatus(user, status) {
    if (S.userStatusBusy) return;
    S.userStatusBusy = user.id;
    try {
      const { res, data } = await api("/api/cloud/admin/users/" + encodeURIComponent(user.id) + "/status", { method: "POST", body: { status: status } });
      if (!res.ok) { notice(errorOf(res, data), "error"); return; }
      user.status = status;
      notice({ key: status === "active" ? "userEnabled" : "userDisabled" }, "ok");
    } catch { notice({ key: "errUnavailable" }, "error"); } finally { S.userStatusBusy = ""; renderUsers(); }
  }

  async function creditUser(user, rawAmount, reason, errorNode, save) {
    if (S.creditBusy) return;
    const amount = parseCNYMicros(rawAmount);
    if (amount === null || amount <= 0) { setFormError(errorNode, t("errCreditAmount")); return; }
    if (!S.creditRequestId) S.creditRequestId = window.crypto && crypto.randomUUID ? crypto.randomUUID() : "credit-" + Date.now().toString(36) + "-" + Math.random().toString(36).slice(2);
    S.creditBusy = true;
    save.disabled = true;
    try {
      const { res, data } = await api("/api/cloud/admin/wallet/credit", { method: "POST", body: { userId: user.id, amountMicros: amount, reason: reason.trim(), requestId: S.creditRequestId } });
      if (!res.ok) { setFormError(errorNode, messageOf(errorOf(res, data))); return; }
      notice({ key: data && data.replayed ? "creditReplayed" : "creditAdded" }, "ok");
      S.creditFor = "";
      S.creditRequestId = "";
      await Promise.all([loadUsers(), loadAudit()]);
    } catch { setFormError(errorNode, t("errUnavailable")); } finally { S.creditBusy = false; save.disabled = false; }
  }

  function renderOrders() {
    const list = byId("ordersList");
    renderState(byId("ordersState"), S.orders, loadOrders, "ordersEmpty", "ordersEmptyHint");
    clear(list);
    if (S.orders.phase !== "ready") return;
    for (const order of S.orders.items) {
      const li = h("li", "row");
      const main = h("div", "row-main");
      main.append(h("div", "row-name", h("span", "name", order.merchantOrderNo || "")));
      main.append(h("div", "row-sub", (order.provider || "") + " · " + formatCNY(order.amountMicros || 0, "CNY") + " · " + fmtDate(order.createdAt)));
      main.append(h("div", "row-sub mono", order.userId || ""));
      li.append(main, h("div", "row-actions", pill(t(order.creditState === "credited" ? "creditCredited" : "creditPending"), order.creditState === "credited" ? "ok" : "warn")));
      list.append(li);
    }
  }

  function renderAudit() {
    const list = byId("auditList");
    renderState(byId("auditState"), S.audit, loadAudit, "auditEmpty", "auditEmptyHint");
    clear(list);
    if (S.audit.phase !== "ready") return;
    for (const entry of S.audit.items) {
      const li = h("li", "row");
      const main = h("div", "row-main");
      main.append(h("div", "row-name", h("span", "name", entry.action || "")));
      main.append(h("div", "row-sub", (entry.targetType || "") + " · " + (entry.targetId || "") + " · " + fmtDate(entry.createdAt)));
      if (entry.details) main.append(h("div", "row-sub mono", JSON.stringify(entry.details)));
      li.append(main);
      list.append(li);
    }
  }

  // ------------------------------------------------------ admin providers
  function toggleAdminProviders(channel) {
    const id = String(channel.id);
    S.adminProvidersFor = S.adminProvidersFor === id ? "" : id;
    if (S.adminProvidersFor) {
      S.pricesFor = "";
      closeHouseProviderForm();
      loadAdminProviders();
    }
    renderAdmin();
  }

  function adminProvidersPanel(channel) {
    const li = h("li", "providers-panel");
    const head = h("div", "panel-head");
    head.append(h("h3", "", t("providersFor", { name: channel.name || "" })));
    const close = h("button", "btn small ghost", t("closeOrder"));
    close.type = "button";
    close.addEventListener("click", toggleAdminProviders.bind(null, channel));
    head.append(close);
    li.append(head);
    const state = h("div", "state");
    renderState(state, S.adminProviders, loadAdminProviders, "adminProvidersEmpty", "adminProvidersEmptyHint");
    li.append(state);
    if (S.adminProviders.phase !== "ready") return li;
    const list = h("ul", "rows");
    for (const provider of S.adminProviders.items) {
      const row = providerRow(provider, true);
      const del = h("button", "btn small danger", t(S.adminProviderDeleting === provider.id ? "confirmDelete" : "deleteAction"));
      del.type = "button";
      del.disabled = S.adminProviderBusy;
      del.addEventListener("click", () => {
        if (S.adminProviderDeleting === provider.id) deleteAdminProvider(channel, provider);
        else { S.adminProviderDeleting = provider.id; renderAdmin(); }
      });
      row.querySelector(".row-actions").append(del);
      list.append(row);
    }
    li.append(list);
    return li;
  }

  async function deleteAdminProvider(channel, provider) {
    if (S.adminProviderBusy) return;
    S.adminProviderBusy = true;
    try {
      const { res, data } = await api("/api/cloud/admin/house-channels/" + encodeURIComponent(channel.id) + "/providers/" + encodeURIComponent(provider.id), { method: "DELETE" });
      if (!res.ok) { notice(errorOf(res, data), "error"); return; }
      S.adminProviderDeleting = "";
      notice({ key: "adminProviderDeleted" }, "ok");
      await loadAdminProviders();
    } catch { notice({ key: "errUnavailable" }, "error"); } finally { S.adminProviderBusy = false; renderAdmin(); }
  }

  // ---------------------------------------------------------------- forms
  function hideForms() {
    closeProviderForm();
    closeKeyForm();
    closeChannelForm();
    closeHouseProviderForm();
    S.groupFormOpen = false;
    show(byId("groupForm"), false);
  }

  // ---------------------------------------------------------------- wiring
  function wire() {
    byId("logout").addEventListener("click", logout);

    byId("recharge").addEventListener("click", () => {
      if (byId("rechargeForm").hidden) openRechargeForm();
      else closeRechargeForm();
    });
    byId("rechargeForm").addEventListener("submit", (event) => {
      event.preventDefault();
      createOrder();
    });
    byId("payCancel").addEventListener("click", closeRechargeForm);
    byId("payProvider").addEventListener("click", (event) => {
      const button = event.target.closest("button[data-provider]");
      if (!button) return;
      S.payProvider = button.dataset.provider;
      renderPayProvider();
      setFormError(byId("payError"), "");
    });
    byId("orderCheck").addEventListener("click", () => checkOrder(true));
    byId("orderClose").addEventListener("click", closeOrder);
    byId("payCopy").addEventListener("click", copyPayLink);

    byId("addProvider").addEventListener("click", () => {
      if (byId("providerForm").hidden) openProviderForm();
      else closeProviderForm();
    });
    byId("pCancel").addEventListener("click", closeProviderForm);
    byId("providerForm").addEventListener("submit", (event) => {
      event.preventDefault();
      createProvider();
    });
    byId("pName").addEventListener("input", () => {
      if (pIdAuto) byId("pId").value = slugify(byId("pName").value);
    });
    byId("pId").addEventListener("input", () => {
      pIdAuto = byId("pId").value.trim() === "";
    });
    bindProtocol("pProtocol", (protocol) => { S.providerProtocol = protocol; });

    byId("newKey").addEventListener("click", () => {
      if (byId("keyForm").hidden) openKeyForm();
      else closeKeyForm();
    });
    byId("keyCancel").addEventListener("click", closeKeyForm);
    byId("keyForm").addEventListener("submit", (event) => {
      event.preventDefault();
      createKey();
    });
    byId("secretCopy").addEventListener("click", copySecret);
    byId("secretDone").addEventListener("click", () => clearSecret(true));

    byId("newGroup").addEventListener("click", () => {
      S.groupFormOpen = !S.groupFormOpen;
      byId("groupForm").reset();
      setFormError(byId("groupError"), "");
      renderGroups();
      if (S.groupFormOpen) byId("gName").focus({ preventScroll: true });
    });
    byId("gCancel").addEventListener("click", () => {
      S.groupFormOpen = false;
      renderGroups();
    });
    byId("groupForm").addEventListener("submit", (event) => {
      event.preventDefault();
      createGroup();
    });

    byId("newChannel").addEventListener("click", () => {
      if (byId("channelForm").hidden) openChannelForm();
      else closeChannelForm();
    });
    byId("chCancel").addEventListener("click", closeChannelForm);
    byId("channelForm").addEventListener("submit", (event) => {
      event.preventDefault();
      createChannel();
    });

    byId("hpCancel").addEventListener("click", closeHouseProviderForm);
    byId("houseProviderForm").addEventListener("submit", (event) => {
      event.preventDefault();
      createHouseProvider();
    });
    byId("hpName").addEventListener("input", () => {
      if (hpIdAuto) byId("hpId").value = slugify(byId("hpName").value);
    });
    byId("hpId").addEventListener("input", () => {
      hpIdAuto = byId("hpId").value.trim() === "";
    });
    bindProtocol("hpProtocol", (protocol) => { S.hpProtocol = protocol; });

    for (const id of ["policyRegistration", "policyPassword", "policyCode"]) {
      byId(id).addEventListener("click", () => togglePolicySwitch(id));
    }
    byId("policyForm").addEventListener("submit", (event) => {
      event.preventDefault();
      savePolicy();
    });

    window.addEventListener("pagehide", () => {
      stopOrderPoll();
      clearSecret(false);
    });
  }

  // Exposed for this page's tests: the dictionary they check for missing
  // keys and placeholders, the language the page picked and the session it
  // read. It holds no credential and changes no behavior.
  window.magpieCloud.session = () => S.user;
  window.magpieCloud.boot = () => S.boot;
  window.magpieCloud.policy = () => S.policy;

  onChange(applyStatic);
  applyStatic();
  wire();
  boot();
})();
