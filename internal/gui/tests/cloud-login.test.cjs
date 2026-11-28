// magpie cloud's sign-in family (internal/gui/assets/cloud), run the way the
// other GUI tests run: the real files, every API answered by a fixture in
// this file, Chromium and WebKit, and every language the app ships. It
// checks password sign-in, the email-code path, registration, password
// reset, the public policy that chooses which of them exist, sign-out, and
// that no server message or password reaches a page.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const ASSETS = path.resolve(__dirname, "../assets/cloud");
const BASE = "http://magpie.test";
const LOCALE = { en: "en-US", zh: "zh-CN", "zh-TW": "zh-TW", ja: "ja-JP", de: "de-DE" };
const USER = { id: "user-ann", email: "ann@example.test", role: "user", emailVerified: true, createdAt: "2026-11-01T00:00:00Z" };
const POLICY = { registrationEnabled: true, passwordLoginEnabled: true, codeLoginEnabled: true, passwordMinLength: 12 };
const PASSWORD = "correct-horse-battery";

const EXPECT = {
  en: {
    langName: "English",
    login: "Sign in",
    intro: "Use your password, or ask for a one-time code by email.",
    passwordMode: "Password",
    codeMode: "Email code",
    forgot: "Forgot your password?",
    noAccount: "New to magpie cloud?",
    create: "Create account",
    register: "Create your account",
    reset: "Reset your password",
    closed: "Registration is closed",
    codeSent: "If that address can sign in, a code is on its way."
  },
  zh: {
    langName: "简体中文",
    login: "登录",
    intro: "使用密码登录，或获取发往邮箱的一次性验证码。",
    passwordMode: "密码",
    codeMode: "邮箱验证码",
    forgot: "忘记密码？",
    noAccount: "还没有账户？",
    create: "创建账户",
    register: "创建账户",
    reset: "重置密码",
    closed: "注册已关闭",
    codeSent: "如果该邮箱可以登录，验证码正在发送中。"
  },
  "zh-TW": {
    langName: "繁體中文",
    login: "登入",
    intro: "使用密碼登入，或取得寄到電子郵件的一次性驗證碼。",
    passwordMode: "密碼",
    codeMode: "電子郵件驗證碼",
    forgot: "忘記密碼？",
    noAccount: "還沒有帳號？",
    create: "建立帳號",
    register: "建立帳號",
    reset: "重設密碼",
    closed: "註冊已關閉",
    codeSent: "如果該電子郵件可以登入，驗證碼正在傳送中。"
  },
  ja: {
    langName: "日本語",
    login: "サインイン",
    intro: "パスワード、またはメールで届くワンタイムコードを使います。",
    passwordMode: "パスワード",
    codeMode: "メールコード",
    forgot: "パスワードをお忘れですか？",
    noAccount: "magpie クラウドは初めてですか？",
    create: "アカウント作成",
    register: "アカウントを作成",
    reset: "パスワードを再設定",
    closed: "新規登録は停止しています",
    codeSent: "そのアドレスでサインインできる場合、コードを送信しました。"
  },
  de: {
    langName: "Deutsch",
    login: "Anmelden",
    intro: "Nutze dein Passwort oder fordere einen Einmalcode per E-Mail an.",
    passwordMode: "Passwort",
    codeMode: "E-Mail-Code",
    forgot: "Passwort vergessen?",
    noAccount: "Neu bei magpie Cloud?",
    create: "Konto erstellen",
    register: "Konto erstellen",
    reset: "Passwort zurücksetzen",
    closed: "Registrierung geschlossen",
    codeSent: "Wenn diese Adresse sich anmelden kann, ist ein Code unterwegs."
  }
};

async function asset(route, pathname) {
  let rel = null;
  if (pathname === "/" || pathname === "/cloud/" || pathname === "/cloud/index.html") rel = "index.html";
  else if (pathname === "/login" || pathname === "/cloud/login.html") rel = "login.html";
  else if (pathname === "/register" || pathname === "/cloud/register.html") rel = "register.html";
  else if (pathname === "/reset" || pathname === "/cloud/reset.html") rel = "reset.html";
  else if (pathname === "/app" || pathname === "/cloud/portal.html") rel = "portal.html";
  else if (pathname.startsWith("/cloud/")) rel = pathname.slice("/cloud/".length);
  if (rel == null) return false;
  const file = path.join(ASSETS, rel);
  if (!file.startsWith(ASSETS + path.sep)) {
    await route.fulfill({ status: 404, body: "" });
    return true;
  }
  const type = { ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8", ".svg": "image/svg+xml" }[path.extname(file)] || "application/octet-stream";
  try {
    await route.fulfill({ body: await fs.readFile(file), contentType: type });
  } catch {
    await route.fulfill({ status: 404, body: "" });
  }
  return true;
}

async function launch(engine) {
  return engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" });
}

async function open(browser, opts = {}) {
  const context = await browser.newContext({
    viewport: opts.viewport || { width: 900, height: 900 },
    locale: opts.locale || LOCALE[opts.lang] || "en-US",
    reducedMotion: "reduce"
  });
  const page = await context.newPage();
  page.setDefaultTimeout(10000);
  page.setDefaultNavigationTimeout(60000);
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));

  const state = {
    user: opts.user || null,
    policy: Object.assign({}, POLICY, opts.policy || {}),
    policyStatus: opts.policyStatus || 200,
    codeStatus: 200,
    codeReply: { ok: true, retryAfter: 0 },
    passwordStatus: 0,
    passwordReply: null,
    loginStatus: 0,
    loginReply: null,
    registerStatus: 0,
    registerReply: null,
    resetStatus: 0,
    resetReply: null,
    calls: []
  };

  await page.route("**/*", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const p = url.pathname;
    const method = request.method();
    const json = (status, data) => route.fulfill({ status, contentType: "application/json; charset=utf-8", body: JSON.stringify(data) });
    let body = null;
    try { body = request.postDataJSON(); } catch (e) {}
    if (p.startsWith("/api/")) state.calls.push({ method, path: p, body });

    if (await asset(route, p)) return;
    if (p === "/api/auth/policy") {
      if (state.policyStatus !== 200) return json(state.policyStatus, { error: "unavailable" });
      return json(200, state.policy);
    }
    if (p === "/api/auth/me") {
      return state.user ? json(200, { user: state.user }) : json(401, { error: "unauthenticated", message: "Sign in to continue." });
    }
    if ((p === "/api/auth/code" || p === "/api/auth/register/code" || p === "/api/auth/reset/code") && method === "POST") {
      if (state.codeStatus !== 200) return json(state.codeStatus, state.codeReply);
      return json(200, { ok: true, retryAfter: 0 });
    }
    if (p === "/api/auth/password" && method === "POST") {
      if (state.passwordStatus) return json(state.passwordStatus, state.passwordReply);
      if (body && body.email === "ann@example.test" && body.password === PASSWORD) {
        state.user = USER;
        return json(200, { user: USER, expiresAt: "2027-01-01T00:00:00Z" });
      }
      return json(401, { error: "invalid_credentials", message: "That email and password do not match." });
    }
    if (p === "/api/auth/login" && method === "POST") {
      if (state.loginStatus) return json(state.loginStatus, state.loginReply);
      if (body && body.email === "ann@example.test" && body.code === "123456") {
        state.user = USER;
        return json(200, { user: USER, expiresAt: "2027-01-01T00:00:00Z" });
      }
      return json(401, { error: "invalid_code", message: "That code is not valid or has expired." });
    }
    if (p === "/api/auth/register" && method === "POST") {
      if (state.registerStatus) return json(state.registerStatus, state.registerReply);
      if (!body || body.code !== "123456") return json(401, { error: "invalid_code" });
      if (String(body.password || "").length < state.policy.passwordMinLength) return json(400, { error: "weak_password" });
      state.user = Object.assign({}, USER, { email: body.email });
      return json(200, { user: state.user, expiresAt: "2027-01-01T00:00:00Z" });
    }
    if (p === "/api/auth/reset" && method === "POST") {
      if (state.resetStatus) return json(state.resetStatus, state.resetReply);
      if (!body || body.code !== "123456") return json(401, { error: "invalid_code" });
      state.user = USER;
      return json(200, { user: USER, expiresAt: "2027-01-01T00:00:00Z" });
    }
    if (p === "/api/auth/logout" && method === "POST") {
      state.user = null;
      return route.fulfill({ status: 204, body: "" });
    }

    // the portal a successful sign-in opens
    if (p === "/api/cloud/wallet") return json(200, { balanceMicros: 0, heldMicros: 0, availableMicros: 0, currency: "CNY", paymentsEnabled: false });
    if (p === "/api/cloud/providers" || p === "/api/cloud/house-channels" || p === "/api/cloud/keys" || p === "/api/cloud/groups" || p === "/api/cloud/usage" || p === "/api/cloud/orders") {
      return json(200, []);
    }
    return route.fulfill({ status: 404, body: "" });
  });

  const query = opts.query != null ? opts.query : opts.lang ? "?lang=" + encodeURIComponent(opts.lang) : "";
  await page.goto(BASE + (opts.path || "/login") + query);
  if (opts.signedIn !== true) {
    await page.waitForFunction(() => !!(window.magpieCloud && window.magpieCloud.auth && window.magpieCloud.auth.done()));
  }
  return { context, page, state, errors };
}

const calls = (state, method, pathname) => state.calls.filter((c) => c.method === method && c.path === pathname);
const at = (page) => page.evaluate(() => location.pathname);
const line = async (page, selector) => (await page.locator(selector).innerText()).replace(/\s+/g, " ").trim();

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: a password signs in, the portal opens, and sign-out returns`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, state, errors } = await open(browser, { lang: "en" });
    try {
      assert.equal(await page.getAttribute("html", "lang"), "en");
      assert.equal(await page.textContent("#authTitle"), EXPECT.en.login);
      assert.equal(await page.textContent(".hero-sub"), EXPECT.en.intro);
      assert.equal(await page.textContent("#authGo"), EXPECT.en.login);
      assert.equal(await page.locator("#loginMode").isVisible(), true, "both methods are offered");
      assert.equal(await page.textContent("#modePassword"), EXPECT.en.passwordMode);
      assert.equal(await page.textContent("#modeCode"), EXPECT.en.codeMode);
      assert.equal(await page.textContent("#forgotLink"), EXPECT.en.forgot);
      assert.equal(await line(page, "#registerLine"), EXPECT.en.noAccount + " " + EXPECT.en.create);
      assert.equal(await page.locator("#passwordGroup").isVisible(), true);
      assert.equal(await page.locator("#codeGroup").isHidden(), true);

      await page.fill("#email", "ann@example.test");
      await page.fill("#password", PASSWORD);
      await page.click("#authGo");
      await page.locator("#portal").waitFor();
      assert.equal(await at(page), "/app");
      assert.equal(await page.textContent("#meEmail"), "ann@example.test");
      const sent = calls(state, "POST", "/api/auth/password").pop();
      assert.deepEqual(sent.body, { email: "ann@example.test", password: PASSWORD });
      // the password is not left on the page after the page moved on
      assert.equal(await page.evaluate(() => document.body.innerText.includes("correct-horse-battery")), false);

      await page.click("#logout");
      await page.locator("#loginForm").waitFor();
      assert.equal(await at(page), "/login");
      assert.equal(state.user, null);
      assert.equal(await page.locator("#portal").count(), 0);
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: a wrong password is named without a server detail`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, state, errors } = await open(browser, { lang: "en" });
    try {
      await page.fill("#email", "ann@example.test");
      await page.fill("#password", "not-the-password");
      await page.click("#authGo");
      await page.locator("#authError").waitFor();
      assert.equal(await page.textContent("#authError"), "That email and password do not match.");
      assert.equal(await at(page), "/login");
      assert.equal(await page.locator("#portal").count(), 0);

      // a 500 whose body names the database says only that the server is down
      state.passwordStatus = 500;
      state.passwordReply = { error: "unavailable", message: "pq: password authentication failed for user magpie" };
      await page.click("#authGo");
      await page.locator("#authError").filter({ hasText: "The server is unavailable. Try again." }).waitFor();
      const shown = await page.textContent("#authError");
      assert.doesNotMatch(shown, /pq:|magpie|authentication/i);
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: the email-code path sends a code and signs in`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, state, errors } = await open(browser, { lang: "en" });
    try {
      await page.click("#modeCode");
      assert.equal(await page.locator("#passwordGroup").isHidden(), true);
      assert.equal(await page.locator("#sendRow").isVisible(), true);
      assert.equal(await page.textContent("#authGo"), "Send code");

      await page.fill("#email", "ann@example.test");
      await page.click("#authGo");
      await page.locator("#codeGroup").waitFor();
      assert.equal(await page.textContent("#authStatus"), EXPECT.en.codeSent);
      assert.equal(await page.textContent("#authGo"), "Sign in");
      assert.deepEqual(calls(state, "POST", "/api/auth/code").pop().body, { email: "ann@example.test" });

      await page.fill("#code", "000000");
      await page.click("#authGo");
      await page.locator("#authError").waitFor();
      assert.equal(await page.textContent("#authError"), "That code is not valid or has expired.");
      assert.equal(await page.locator("#portal").count(), 0);

      await page.fill("#code", "123456");
      await page.click("#authGo");
      await page.locator("#portal").waitFor();
      assert.equal(await at(page), "/app");

      // switching back to a password puts the code away
      state.user = null;
      await page.goto(BASE + "/login?lang=en");
      await page.locator("#loginForm").waitFor();
      await page.click("#modeCode");
      await page.fill("#email", "ann@example.test");
      await page.click("#sendCodeBtn");
      await page.locator("#codeGroup").waitFor();
      await page.click("#modePassword");
      assert.equal(await page.locator("#codeGroup").isHidden(), true);
      assert.equal(await page.locator("#passwordGroup").isVisible(), true);
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: the policy chooses which sign-in methods exist`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());

    {
      const { context, page, errors } = await open(browser, { lang: "en", policy: { passwordLoginEnabled: false } });
      try {
        assert.equal(await page.locator("#loginMode").isHidden(), true);
        assert.equal(await page.locator("#passwordGroup").isHidden(), true);
        assert.equal(await page.locator("#sendRow").isVisible(), true);
        assert.equal(await page.textContent("#loginPolicyNote"), "Password sign-in is off on this deployment. Use an email code.");
        assert.equal(await page.textContent("#authGo"), "Send code");
        assert.equal(await page.locator("#forgotLink").isHidden(), true);
        assert.deepEqual(errors, []);
      } finally {
        await context.close();
      }
    }

    {
      const { context, page, errors } = await open(browser, { lang: "en", policy: { codeLoginEnabled: false } });
      try {
        assert.equal(await page.locator("#loginMode").isHidden(), true);
        assert.equal(await page.locator("#passwordGroup").isVisible(), true);
        assert.equal(await page.locator("#sendRow").isHidden(), true);
        assert.equal(await page.textContent("#loginPolicyNote"), "Email-code sign-in is off on this deployment. Use your password.");
        assert.equal(await page.locator("#forgotLink").isVisible(), true);
        assert.deepEqual(errors, []);
      } finally {
        await context.close();
      }
    }

    {
      const { context, page, errors } = await open(browser, { lang: "en", policy: { passwordLoginEnabled: false, codeLoginEnabled: false } });
      try {
        assert.equal(await page.locator("#loginUnavailable").isVisible(), true);
        assert.equal(await page.locator("#loginForm").isHidden(), true);
        assert.equal(await page.locator("#registerLine").isHidden(), true);
        assert.equal(await page.textContent("#loginUnavailable h2"), "Sign-in is unavailable");
        assert.deepEqual(errors, []);
      } finally {
        await context.close();
      }
    }

    {
      const { context, page, errors } = await open(browser, { lang: "en", policy: { registrationEnabled: false } });
      try {
        assert.equal(await page.locator("#registerLine").isHidden(), true);
        assert.equal(await page.locator("#registerClosedLine").isVisible(), true);
        assert.equal(await page.getAttribute("#registerClosedLine", "hidden"), null);
        assert.deepEqual(errors, []);
      } finally {
        await context.close();
      }
    }
  });

  test(`${engine}: the portal sends an anonymous reader to sign in`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, state, errors } = await open(browser, { lang: "en", path: "/app", signedIn: true });
    try {
      await page.locator("#loginForm").waitFor();
      assert.equal(await at(page), "/login");
      assert.equal(state.user, null);
      assert.equal(await page.textContent("#authTitle"), EXPECT.en.login);
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: a signed-in reader never sees a sign-in form`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    for (const pagePath of ["/login", "/register", "/reset"]) {
      await t.test(pagePath, async () => {
        const { context, page, state, errors } = await open(browser, { lang: "en", path: pagePath, user: USER, signedIn: true });
        try {
          await page.locator("#portal").waitFor();
          assert.equal(await page.textContent("#meEmail"), "ann@example.test");
          assert.equal(await page.locator("#loginForm").count(), 0);
          assert.equal(state.calls.some((c) => c.path === "/api/auth/password"), false);
          assert.deepEqual(errors, []);
        } finally {
          await context.close();
        }
      });
    }
  });

  test(`${engine}: a verified code and a password create the account`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, state, errors } = await open(browser, { lang: "en", path: "/register" });
    try {
      assert.equal(await page.textContent("#authTitle"), EXPECT.en.register);
      assert.equal(await page.locator("#registerForm").isVisible(), true);
      assert.equal(await page.textContent("#passwordHint"), "At least 12 characters.");

      await page.fill("#email", "new@example.test");
      await page.click("#sendCodeBtn");
      await page.locator("#codeGroup").waitFor();
      assert.equal(await page.textContent("#authStatus"), "If that address can be registered, a code is on its way.");
      assert.deepEqual(calls(state, "POST", "/api/auth/register/code").pop().body, { email: "new@example.test" });

      await page.fill("#code", "123456");
      await page.fill("#password", "short");
      await page.fill("#passwordConfirm", "short");
      await page.click("#authGo");
      await page.locator("#authError").waitFor();
      assert.equal(await page.textContent("#authError"), "That password is too short.");
      assert.equal(calls(state, "POST", "/api/auth/register").length, 0);

      await page.fill("#password", "a-long-password");
      await page.fill("#passwordConfirm", "a-different-one");
      await page.click("#authGo");
      assert.equal(await page.textContent("#authError"), "The two passwords do not match.");
      assert.equal(calls(state, "POST", "/api/auth/register").length, 0);

      await page.fill("#password", "a-long-password");
      await page.fill("#passwordConfirm", "a-long-password");
      await page.click("#authGo");
      await page.locator("#portal").waitFor();
      assert.equal(await at(page), "/app");
      assert.deepEqual(calls(state, "POST", "/api/auth/register").pop().body, {
        email: "new@example.test",
        code: "123456",
        password: "a-long-password"
      });
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: a closed deployment takes no registration`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, state, errors } = await open(browser, { lang: "en", path: "/register", policy: { registrationEnabled: false } });
    try {
      assert.equal(await page.locator("#registerClosed").isVisible(), true);
      assert.equal(await page.locator("#registerForm").isHidden(), true);
      assert.equal(await page.textContent("#registerClosed h2"), EXPECT.en.closed);
      assert.equal(await page.textContent("#registerClosed a"), "Sign in");
      await page.click("#registerClosed a");
      await page.locator("#loginForm").waitFor();
      assert.equal(await at(page), "/login");
      assert.equal(state.calls.some((c) => c.path === "/api/auth/register/code"), false);
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: a code sets a new password and signs the reader in`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, state, errors } = await open(browser, { lang: "en", path: "/reset" });
    try {
      assert.equal(await page.textContent("#authTitle"), EXPECT.en.reset);
      assert.equal(await page.locator("#resetForm").isVisible(), true);
      assert.equal(await page.textContent("#passwordHint"), "At least 12 characters.");

      await page.fill("#email", "ann@example.test");
      await page.click("#sendCodeBtn");
      await page.locator("#codeGroup").waitFor();
      assert.equal(await page.textContent("#authStatus"), "If that address has an account, a code is on its way.");
      assert.deepEqual(calls(state, "POST", "/api/auth/reset/code").pop().body, { email: "ann@example.test" });

      await page.fill("#code", "123456");
      await page.fill("#password", "a-long-password");
      await page.fill("#passwordConfirm", "a-long-password");
      await page.click("#authGo");
      await page.locator("#portal").waitFor();
      assert.equal(await at(page), "/app");
      assert.deepEqual(calls(state, "POST", "/api/auth/reset").pop().body, {
        email: "ann@example.test",
        code: "123456",
        password: "a-long-password"
      });
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }

    // a deployment with passwords off does not offer a reset that cannot work
    const off = await open(browser, { lang: "en", path: "/reset", policy: { passwordLoginEnabled: false } });
    try {
      assert.equal(await off.page.locator("#resetUnavailable").isVisible(), true);
      assert.equal(await off.page.locator("#resetForm").isHidden(), true);
      assert.deepEqual(off.errors, []);
    } finally {
      await off.context.close();
    }
  });

  test(`${engine}: a rate-limited send names the wait`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, state, errors } = await open(browser, { lang: "en" });
    try {
      await page.click("#modeCode");
      await page.fill("#email", "ann@example.test");
      state.codeStatus = 429;
      state.codeReply = { error: "rate_limited", message: "Too many requests. Try again later.", retryAfter: 7 };
      await page.click("#sendCodeBtn");
      await page.locator("#authError").waitFor();
      assert.equal(await page.textContent("#authError"), "Too many requests. Try again in 7 seconds.");
      assert.equal(await page.locator("#codeGroup").isHidden(), true);
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: the sign-in pages fit a phone`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    for (const pagePath of ["/login", "/register", "/reset"]) {
      await t.test(pagePath, async () => {
        const { context, page, errors } = await open(browser, {
          lang: "de",
          path: pagePath,
          viewport: { width: 360, height: 800 }
        });
        try {
          assert.equal(
            await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth),
            true,
            pagePath + " has no sideways scroll"
          );
          assert.equal(await page.locator(".auth-card").isVisible(), true);
          assert.equal(await page.locator("#authGo").isVisible(), true);
          assert.deepEqual(errors, []);
        } finally {
          await context.close();
        }
      });
    }
  });

  test(`${engine}: every sign-in page speaks every language`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
      await t.test(lang, async () => {
        const w = EXPECT[lang];
        const login = await open(browser, { lang });
        try {
          assert.equal(await login.page.getAttribute("html", "lang"), lang);
          assert.equal(await login.page.textContent("#authTitle"), w.login);
          assert.equal(await login.page.textContent(".hero-sub"), w.intro);
          assert.equal(await login.page.textContent("#authGo"), w.login);
          assert.equal(await login.page.textContent("#modePassword"), w.passwordMode);
          assert.equal(await login.page.textContent("#modeCode"), w.codeMode);
          assert.equal(await login.page.textContent("#forgotLink"), w.forgot);
          assert.equal(await line(login.page, "#registerLine"), w.noAccount + " " + w.create);
          assert.equal(await login.page.textContent("#langNow"), w.langName);
          assert.deepEqual(login.errors, []);
        } finally {
          await login.context.close();
        }

        for (const [pagePath, key] of [["/register", "register"], ["/reset", "reset"]]) {
          const w2 = await open(browser, { lang, path: pagePath });
          try {
            assert.equal(await w2.page.textContent("#authTitle"), w[key], pagePath + " " + lang);
            assert.deepEqual(w2.errors, []);
          } finally {
            await w2.context.close();
          }
        }
      });
    }
  });
}

test("chromium: the password is only posted, never stored or drawn", async (t) => {
  const browser = await launch("chromium");
  t.after(() => browser.close());
  const { context, page, state, errors } = await open(browser, { lang: "en" });
  try {
    await page.fill("#email", "ann@example.test");
    await page.fill("#password", PASSWORD);
    await page.click("#authGo");
    await page.locator("#portal").waitFor();
    const posted = calls(state, "POST", "/api/auth/password").pop();
    assert.equal(posted.body.password, PASSWORD);
    const stored = await page.evaluate(() => {
      const all = [];
      for (let i = 0; i < localStorage.length; i++) all.push(localStorage.getItem(localStorage.key(i)));
      for (let i = 0; i < sessionStorage.length; i++) all.push(sessionStorage.getItem(sessionStorage.key(i)));
      all.push(document.body.innerText, document.documentElement.outerHTML);
      return all.join("\n");
    });
    assert.equal(stored.includes(PASSWORD), false, "the password is not kept in storage or the page");
    assert.deepEqual(errors, []);
  } finally {
    await context.close();
  }
});
