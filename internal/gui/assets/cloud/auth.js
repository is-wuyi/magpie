// magpie cloud — the sign-in, registration and password-reset pages. Each
// page asks the public policy and the current session first, then draws the
// methods this deployment allows. A password never leaves this file except
// in the JSON body of its own request, and no reply is drawn raw: failures
// become one of the dictionary's own reasons.
"use strict";

(function () {
  const S = window.magpieCloudShared;
  const {
    byId, show, setPipeText, setFormError, t, onChange, api, errorOf, messageOf,
    fetchMe, fetchPolicy, passwordFloor
  } = S;

  const PAGE = document.body.dataset.page || "login";
  const MAX_PASSWORD = 72;

  let policyState = { known: false, policy: Object.assign({}, S.POLICY_FALLBACK), reason: null };
  let mode = "password";        // login: "password" | "code"
  let codeSent = false;
  let busy = false;
  let errorReason = null;
  let statusKey = null;

  function reasonForLogin(res, data) {
    const code = data && typeof data.error === "string" ? data.error.toLowerCase() : "";
    if (res.status === 401 && (!code || code === "unauthenticated")) return { key: "errInvalidCredentials" };
    return errorOf(res, data);
  }

  function showError(reason) {
    errorReason = reason || null;
    setFormError(byId("authError"), messageOf(errorReason));
  }

  function showStatus(key) {
    statusKey = key || null;
    setFormError(byId("authStatus"), statusKey ? t(statusKey) : "");
  }

  function readEmail() {
    return (byId("email").value || "").trim();
  }

  function emailOK(email) {
    return !!email && email.indexOf("@") > 0 && !/\s/.test(email);
  }

  function floor() {
    return passwordFloor(policyState.policy);
  }

  function passwordProblem(password, confirm) {
    if (!password) return { key: "errPasswordRequired" };
    if (password.length < floor()) return { key: "errPasswordTooShort" };
    if (password.length > MAX_PASSWORD) return { key: "errPasswordTooLong" };
    if (confirm != null && password !== confirm) return { key: "errPasswordMismatch" };
    return null;
  }

  function goConsole() {
    // replace, not assign: Back must not return to a form for a session
    // that already exists (the sign-in page would only forward again)
    location.replace("/app");
  }

  function formControls() {
    const form = byId("loginForm") || byId("registerForm") || byId("resetForm");
    return form ? Array.from(form.querySelectorAll("input, button")) : [];
  }

  function setBusy(on) {
    busy = on;
    for (const node of formControls()) node.disabled = on;
    const go = byId("authGo");
    if (go) go.setAttribute("aria-busy", on ? "true" : "false");
    render();
  }

  // Read the email and ask for a code. `path` is the one place the purpose
  // (login, registration, reset) is named; every other line is shared.
  async function requestCode(path) {
    if (busy) return false;
    const email = readEmail();
    if (!emailOK(email)) {
      showError({ key: "errInvalidEmail" });
      byId("email").focus({ preventScroll: true });
      return false;
    }
    showError(null);
    showStatus(null);
    setBusy(true);
    let ok = false;
    try {
      const { res, data } = await api(path, { method: "POST", body: { email: email } });
      if (res.ok) {
        codeSent = true;
        const sentKey = PAGE === "register" ? "registerCodeSent" : PAGE === "reset" ? "resetCodeSent" : "codeSent";
        showStatus(sentKey);
        ok = true;
      } else {
        showError(errorOf(res, data));
      }
    } catch (e) {
      showError({ key: "errUnavailable" });
    }
    setBusy(false);
    if (ok && byId("code")) byId("code").focus({ preventScroll: true });
    return ok;
  }

  // ---------------------------------------------------------------- login
  function setLoginMode(next) {
    if (busy || next === mode) return;
    mode = next;
    codeSent = false;
    showError(null);
    showStatus(null);
    const code = byId("code");
    if (code) code.value = "";
    render();
  }

  function renderLogin() {
    const { policy } = policyState;
    const passwordOn = policy.passwordLoginEnabled;
    const codeOn = policy.codeLoginEnabled;
    const usable = passwordOn || codeOn;

    show(byId("loginUnavailable"), !usable);
    show(byId("loginForm"), usable);
    show(byId("loginMode"), usable && passwordOn && codeOn);
    if (!usable) {
      show(byId("forgotLink"), false);
      show(byId("registerLine"), false);
      return;
    }
    if (!passwordOn) mode = "code";
    if (!codeOn) mode = "password";

    const pButton = byId("modePassword");
    const cButton = byId("modeCode");
    if (pButton) pButton.setAttribute("aria-pressed", mode === "password" ? "true" : "false");
    if (cButton) cButton.setAttribute("aria-pressed", mode === "code" ? "true" : "false");

    const passwordMode = mode === "password";
    show(byId("passwordGroup"), passwordMode);
    show(byId("sendRow"), !passwordMode && !codeSent);
    show(byId("codeGroup"), !passwordMode && codeSent);
    show(byId("forgotLink"), passwordMode && passwordOn);
    show(byId("registerLine"), policy.registrationEnabled);
    show(byId("registerClosedLine"), policyState.known && !policy.registrationEnabled);

    const note = byId("loginPolicyNote");
    if (!passwordOn) setPipeText(note, t("passwordLoginOff"));
    else if (!codeOn) setPipeText(note, t("codeLoginOff"));
    else setPipeText(note, "");
    show(note, !passwordOn || !codeOn);

    const go = byId("authGo");
    if (go) {
      const labelKey = passwordMode ? "signIn" : codeSent ? "signIn" : "sendCode";
      setPipeText(go, t(busy ? (passwordMode ? "signingIn" : "sending") : labelKey));
    }
  }

  async function submitLogin() {
    if (busy) return;
    const email = readEmail();
    if (!emailOK(email)) {
      showError({ key: "errInvalidEmail" });
      byId("email").focus({ preventScroll: true });
      return;
    }
    if (mode === "password") {
      const password = byId("password").value;
      if (!password) {
        showError({ key: "errPasswordRequired" });
        byId("password").focus({ preventScroll: true });
        return;
      }
      showError(null);
      showStatus(null);
      setBusy(true);
      try {
        const { res, data } = await api("/api/auth/password", { method: "POST", body: { email: email, password: password } });
        if (res.ok && data && data.user) {
          showStatus("signingIn");
          goConsole();
        } else if (res.ok) {
          showError({ key: "errRequest" });
        } else {
          showError(reasonForLogin(res, data));
        }
      } catch (e) {
        showError({ key: "errUnavailable" });
      }
      setBusy(false);
      return;
    }
    if (!codeSent) {
      await requestCode("/api/auth/code");
      return;
    }
    const code = byId("code").value.trim();
    if (!code) {
      showError({ key: "errCodeRequired" });
      byId("code").focus({ preventScroll: true });
      return;
    }
    showError(null);
    showStatus(null);
    setBusy(true);
    try {
      const { res, data } = await api("/api/auth/login", { method: "POST", body: { email: email, code: code } });
      if (res.ok && data && data.user) {
        showStatus("signingIn");
        goConsole();
      } else if (res.ok) {
        showError({ key: "errRequest" });
      } else {
        showError(errorOf(res, data));
      }
    } catch (e) {
      showError({ key: "errUnavailable" });
    }
    setBusy(false);
  }

  // ------------------------------------------------------------- register
  function renderRegister() {
    const { policy } = policyState;
    const open = !policyState.known || policy.registrationEnabled;
    show(byId("registerForm"), open);
    show(byId("registerClosed"), policyState.known && !policy.registrationEnabled);
    show(byId("registerLine"), open);
    if (!open) return;
    show(byId("sendRow"), !codeSent);
    show(byId("codeGroup"), codeSent);
    const hint = byId("passwordHint");
    if (hint) setPipeText(hint, t("passwordHint", { n: floor() }));
    const go = byId("authGo");
    if (go) setPipeText(go, t(busy ? "sending" : "registerSubmit"));
  }

  async function submitRegister() {
    if (busy) return;
    const email = readEmail();
    if (!emailOK(email)) {
      showError({ key: "errInvalidEmail" });
      byId("email").focus({ preventScroll: true });
      return;
    }
    if (!codeSent) {
      await requestCode("/api/auth/register/code");
      return;
    }
    const code = byId("code").value.trim();
    if (!code) {
      showError({ key: "errCodeRequired" });
      byId("code").focus({ preventScroll: true });
      return;
    }
    const password = byId("password").value;
    const problem = passwordProblem(password, byId("passwordConfirm").value);
    if (problem) {
      showError(problem);
      (problem.key === "errPasswordMismatch" ? byId("passwordConfirm") : byId("password")).focus({ preventScroll: true });
      return;
    }
    showError(null);
    showStatus(null);
    setBusy(true);
    try {
      const { res, data } = await api("/api/auth/register", { method: "POST", body: { email: email, code: code, password: password } });
      if (res.ok && data && data.user) {
        showStatus("registerDone");
        goConsole();
      } else if (res.ok) {
        showError({ key: "errRequest" });
      } else {
        showError(errorOf(res, data));
      }
    } catch (e) {
      showError({ key: "errUnavailable" });
    }
    setBusy(false);
  }

  // ---------------------------------------------------------------- reset
  function renderReset() {
    const { policy } = policyState;
    const usable = !policyState.known || policy.passwordLoginEnabled;
    show(byId("resetForm"), usable);
    show(byId("resetUnavailable"), policyState.known && !policy.passwordLoginEnabled);
    if (!usable) return;
    show(byId("sendRow"), !codeSent);
    show(byId("codeGroup"), codeSent);
    const hint = byId("passwordHint");
    if (hint) setPipeText(hint, t("passwordHint", { n: floor() }));
    const go = byId("authGo");
    if (go) setPipeText(go, t(busy ? "sending" : "resetSubmit"));
  }

  async function submitReset() {
    if (busy) return;
    const email = readEmail();
    if (!emailOK(email)) {
      showError({ key: "errInvalidEmail" });
      byId("email").focus({ preventScroll: true });
      return;
    }
    if (!codeSent) {
      await requestCode("/api/auth/reset/code");
      return;
    }
    const code = byId("code").value.trim();
    if (!code) {
      showError({ key: "errCodeRequired" });
      byId("code").focus({ preventScroll: true });
      return;
    }
    const password = byId("password").value;
    const problem = passwordProblem(password, byId("passwordConfirm").value);
    if (problem) {
      showError(problem);
      (problem.key === "errPasswordMismatch" ? byId("passwordConfirm") : byId("password")).focus({ preventScroll: true });
      return;
    }
    showError(null);
    showStatus(null);
    setBusy(true);
    try {
      const { res, data } = await api("/api/auth/reset", { method: "POST", body: { email: email, code: code, password: password } });
      if (res.ok && data && data.user) {
        showStatus("resetDone");
        goConsole();
      } else if (res.ok) {
        showError({ key: "errRequest" });
      } else {
        showError(errorOf(res, data));
      }
    } catch (e) {
      showError({ key: "errUnavailable" });
    }
    setBusy(false);
  }

  // ---------------------------------------------------------------- wiring
  function render() {
    setFormError(byId("authError"), messageOf(errorReason));
    setFormError(byId("authStatus"), statusKey ? t(statusKey) : "");
    if (PAGE === "login") renderLogin();
    else if (PAGE === "register") renderRegister();
    else renderReset();
  }

  function wire() {
    const formNode = byId("loginForm") || byId("registerForm") || byId("resetForm");
    if (formNode) {
      formNode.addEventListener("submit", (event) => {
        event.preventDefault();
        if (PAGE === "login") submitLogin();
        else if (PAGE === "register") submitRegister();
        else submitReset();
      });
    }
    const send = byId("sendCodeBtn");
    if (send) {
      send.addEventListener("click", () => {
        if (PAGE === "login") requestCode("/api/auth/code");
        else if (PAGE === "register") requestCode("/api/auth/register/code");
        else requestCode("/api/auth/reset/code");
      });
    }
    const resend = byId("resend");
    if (resend) {
      resend.addEventListener("click", () => {
        if (PAGE === "login") requestCode("/api/auth/code");
        else if (PAGE === "register") requestCode("/api/auth/register/code");
        else requestCode("/api/auth/reset/code");
      });
    }
    const modePassword = byId("modePassword");
    const modeCode = byId("modeCode");
    if (modePassword) modePassword.addEventListener("click", () => setLoginMode("password"));
    if (modeCode) modeCode.addEventListener("click", () => setLoginMode("code"));
  }

  onChange(render);

  let readyDone = false;
  const ready = (async () => {
    try {
      const [me, policy] = await Promise.all([fetchMe(), fetchPolicy()]);
      policyState = policy;
      if (me.state === "in") {
        location.replace("/app");
        return;
      }
      render();
    } finally {
      readyDone = true;
    }
  })();

  wire();
  render();

  window.magpieCloud.auth = {
    page: PAGE,
    policy: () => policyState,
    mode: () => mode,
    codeSent: () => codeSent,
    ready: ready,
    done: () => readyDone
  };
})();
