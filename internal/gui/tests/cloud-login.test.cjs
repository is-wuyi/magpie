// magpie cloud's sign-in page (internal/gui/assets/cloud), run the way the
// other GUI tests run: the real files, every API answered by a fixture in
// this file, Chromium and WebKit, and every language the app ships. It
// checks the email → one-time code → session path, sign-out, the language
// menu, and that no server message or upstream credential reaches the page.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { settled } = require("./reader.cjs");

const ASSETS = path.resolve(__dirname, "../assets/cloud");
const BASE = "http://magpie.test";
const LOCALE = { en: "en-US", zh: "zh-CN", "zh-TW": "zh-TW", ja: "ja-JP", de: "de-DE" };
const USER = { id: "user-ann", email: "ann@example.test", role: "user", emailVerified: true, createdAt: "2026-11-01T00:00:00Z" };

// A few visible strings per language: the page's own heading, the two
// buttons of the auth flow, its two answers, and the portal's section heads.
// A missing or stale translation makes one of these fail, not only a lookup
// of the dictionary.
const EXPECT = {
  en: {
    langName: "English",
    title: "Sign in to magpie cloud",
    sendCode: "Send code",
    signIn: "Sign in",
    codeSent: "If that address can sign in, a code is on its way.",
    invalidCode: "That code is not valid or has expired.",
    member: "Member",
    signOut: "Sign out",
    providers: "Your providers",
    houses: "House channels",
    keys: "API keys",
    admin: "Administration",
    addProvider: "Add provider",
    newKey: "Create key",
    wallet: "Wallet",
    emptyProviders: "No providers yet",
    emptyHouses: "No house channels yet",
    emptyKeys: "No API keys yet"
  },
  zh: {
    langName: "简体中文",
    title: "登录 magpie 云",
    sendCode: "发送验证码",
    signIn: "登录",
    codeSent: "如果该邮箱可以登录，验证码正在发送中。",
    invalidCode: "这个验证码无效或已过期。",
    member: "成员",
    signOut: "退出登录",
    providers: "我的供应商",
    houses: "公共频道",
    keys: "API 密钥",
    admin: "管理",
    addProvider: "添加供应商",
    newKey: "创建密钥",
    wallet: "钱包",
    emptyProviders: "还没有供应商",
    emptyHouses: "还没有公共频道",
    emptyKeys: "还没有 API 密钥"
  },
  "zh-TW": {
    langName: "繁體中文",
    title: "登入 magpie 雲端",
    sendCode: "傳送驗證碼",
    signIn: "登入",
    codeSent: "如果這個地址可以登入，驗證碼正在傳送中。",
    invalidCode: "這個驗證碼無效或已過期。",
    member: "成員",
    signOut: "登出",
    providers: "我的供應商",
    houses: "公共頻道",
    keys: "API 金鑰",
    admin: "管理",
    addProvider: "新增供應商",
    newKey: "建立金鑰",
    wallet: "錢包",
    emptyProviders: "還沒有供應商",
    emptyHouses: "還沒有公共頻道",
    emptyKeys: "還沒有 API 金鑰"
  },
  ja: {
    langName: "日本語",
    title: "magpie クラウドにサインイン",
    sendCode: "コードを送信",
    signIn: "サインイン",
    codeSent: "そのアドレスでサインインできる場合、コードを送信しました。",
    invalidCode: "このコードは無効か、期限切れです。",
    member: "メンバー",
    signOut: "サインアウト",
    providers: "自分のプロバイダー",
    houses: "ハウスチャンネル",
    keys: "API キー",
    admin: "管理",
    addProvider: "プロバイダーを追加",
    newKey: "キーを作成",
    wallet: "ウォレット",
    emptyProviders: "プロバイダーはまだありません",
    emptyHouses: "ハウスチャンネルはまだありません",
    emptyKeys: "API キーはまだありません"
  },
  de: {
    langName: "Deutsch",
    title: "Bei magpie Cloud anmelden",
    sendCode: "Code senden",
    signIn: "Anmelden",
    codeSent: "Wenn diese Adresse sich anmelden kann, ist ein Code unterwegs.",
    invalidCode: "Dieser Code ist ungültig oder abgelaufen.",
    member: "Mitglied",
    signOut: "Abmelden",
    providers: "Deine Anbieter",
    houses: "Hauskanäle",
    keys: "API-Schlüssel",
    admin: "Verwaltung",
    addProvider: "Anbieter hinzufügen",
    newKey: "Schlüssel erstellen",
    wallet: "Guthaben",
    emptyProviders: "Noch keine Anbieter",
    emptyHouses: "Noch keine Hauskanäle",
    emptyKeys: "Noch keine API-Schlüssel"
  }
};

async function asset(route, pathname) {
  let rel = null;
  if (pathname === "/" || pathname === "/cloud/" || pathname === "/cloud/index.html") rel = "index.html";
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
    viewport: { width: 900, height: 800 },
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
    calls: [],
    codeStatus: 200,
    codeReply: { ok: true, retryAfter: 0 },
    loginStatus: 0,
    loginReply: null,
    adminCalls: 0
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
    if (p === "/api/auth/me") {
      return state.user ? json(200, { user: state.user }) : json(401, { error: "unauthenticated", message: "Sign in to continue." });
    }
    if (p === "/api/auth/code" && method === "POST") {
      if (state.codeStatus !== 200) return json(state.codeStatus, state.codeReply);
      return json(200, state.codeReply);
    }
    if (p === "/api/auth/login" && method === "POST") {
      if (state.loginStatus) return json(state.loginStatus, state.loginReply);
      const email = body && body.email;
      const code = body && body.code;
      if (email === "ann@example.test" && code === "123456") {
        state.user = USER;
        return json(200, { user: USER, expiresAt: "2027-01-01T00:00:00Z" });
      }
      return json(401, { error: "invalid_code", message: "That code is not valid or has expired." });
    }
    if (p === "/api/auth/logout" && method === "POST") {
      state.user = null;
      return route.fulfill({ status: 204, body: "" });
    }
    if (p === "/api/cloud/wallet") return json(200, { balanceMicros: 0, heldMicros: 0, availableMicros: 0, currency: "CNY", paymentsEnabled: false });
    if (p === "/api/cloud/providers") return json(200, []);
    if (p === "/api/cloud/house-channels") return json(200, []);
    if (p === "/api/cloud/keys") return json(200, []);
    if (p.startsWith("/api/cloud/admin/house-channels")) {
      state.adminCalls++;
      return json(200, []);
    }
    return route.fulfill({ status: 404, body: "" });
  });

  const query = opts.query != null ? opts.query : opts.lang ? "?lang=" + encodeURIComponent(opts.lang) : "";
  await page.goto(BASE + (opts.path || "/") + query);
  return { context, page, state, errors };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: cloud sign-in, session and sign-out in every language`, async (t) => {
    assert(["chromium", "webkit"].includes(engine), "BROWSER must be chromium or webkit");
    const browser = await launch(engine);
    t.after(() => browser.close());
    for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
      await t.test(lang, async () => {
        const w = EXPECT[lang];
        const { context, page, state, errors } = await open(browser, { lang });
        try {
          // the page picks the language asked for, before any read
          assert.equal(await page.getAttribute("html", "lang"), lang);
          assert.equal(await page.textContent("#signin h1"), w.title);
          assert.equal(await page.textContent("#authGo"), w.sendCode);
          assert.equal(await page.textContent("#langNow"), w.langName);
          assert.equal(await page.locator("#signin").isVisible(), true);
          assert.equal(await page.locator("#portal").isHidden(), true);
          assert.equal(await page.locator("#session").isHidden(), true);

          // sending a code tells nothing about whether the address has an account
          await page.fill("#email", "ann@example.test");
          await page.click("#authGo");
          await page.locator("#codeGroup").waitFor();
          const sent = state.calls.find((c) => c.method === "POST" && c.path === "/api/auth/code");
          assert.deepEqual(sent.body, { email: "ann@example.test" });
          assert.equal(await page.textContent("#authStatus"), w.codeSent);
          assert.equal(await page.textContent("#authGo"), w.signIn);

          // a wrong code is said in the page's language, and opens nothing
          await page.fill("#code", "000000");
          await page.click("#authGo");
          await page.locator("#authError").waitFor();
          assert.equal(await page.textContent("#authError"), w.invalidCode);
          assert.equal(await page.locator("#portal").isHidden(), true);
          const wrong = state.calls.find((c) => c.method === "POST" && c.path === "/api/auth/login");
          assert.deepEqual(wrong.body, { email: "ann@example.test", code: "000000" });

          // the code from the mail signs in; the account and its role are shown
          await page.fill("#code", "123456");
          await page.click("#authGo");
          await page.locator("#portal").waitFor();
          await page.locator("#providersState .state-title").waitFor();
          assert.equal(await page.textContent("#meEmail"), "ann@example.test");
          assert.equal(await page.textContent("#meRole"), w.member);
          assert.equal(await page.textContent("#logout"), w.signOut);
          assert.equal(await page.locator("#signin").isHidden(), true);
          assert.equal(await page.locator("#session").isVisible(), true);
          assert.equal(await page.locator("#adminPanel").isHidden(), true, "a member is not shown administration");
          assert.equal(state.adminCalls, 0, "a member's page never asks for administration");

          // every section speaks the language, and an empty read says so
          assert.equal(await page.textContent("#walletPanel h2"), w.wallet);
          assert.equal(await page.textContent("#providersPanel h2"), w.providers);
          assert.equal(await page.textContent("#housesPanel h2"), w.houses);
          assert.equal(await page.textContent("#keysPanel h2"), w.keys);
          assert.equal(await page.textContent("#adminPanel h2"), w.admin);
          assert.equal(await page.textContent("#addProvider"), w.addProvider);
          assert.equal(await page.textContent("#newKey"), w.newKey);
          assert.equal(await page.textContent("#providersState .state-title"), w.emptyProviders);
          assert.equal(await page.textContent("#housesState .state-title"), w.emptyHouses);
          assert.equal(await page.textContent("#keysState .state-title"), w.emptyKeys);
          assert.equal(await page.locator("#providersList .row").count(), 0);

          // a reload keeps the server-side session: no code is asked again
          await page.reload();
          await page.locator("#portal").waitFor();
          assert.equal(await page.locator("#signin").isHidden(), true);
          assert.equal(await page.textContent("#meEmail"), "ann@example.test");

          // sign-out clears the cookie at the server and returns to the form
          await page.click("#logout");
          await page.locator("#signin").waitFor();
          assert.equal(await page.locator("#portal").isHidden(), true);
          assert.equal(await page.locator("#session").isHidden(), true);
          assert.equal(state.user, null);
          assert.ok(state.calls.some((c) => c.method === "POST" && c.path === "/api/auth/logout"));
          await page.reload();
          await page.locator("#signin").waitFor();
          assert.equal(await page.locator("#portal").isHidden(), true);

          assert.deepEqual(errors, []);
        } finally {
          await context.close();
        }
      });
    }
  });

  test(`${engine}: the language menu changes every string and is kept`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, errors } = await open(browser, { lang: "en" });
    try {
      assert.equal(await page.textContent("#signin h1"), EXPECT.en.title);

      await page.click("#langBtn");
      await page.locator("#langMenu").waitFor();
      assert.equal(await page.locator("#langMenu button[role=menuitemradio]").count(), 5);
      assert.match(await page.locator('#langMenu button[aria-checked="true"]').textContent(), /English/);
      await settled(page);
      await page.keyboard.press("Escape");
      await page.locator("#langMenu").waitFor({ state: "hidden" });
      assert.equal(await page.getAttribute("#langBtn", "aria-expanded"), "false");

      await page.click("#langBtn");
      await page.locator("#langMenu").waitFor();
      await page.locator("#langMenu button", { hasText: "日本語" }).click();
      await page.locator("#signin h1").filter({ hasText: EXPECT.ja.title }).waitFor();
      assert.equal(await page.textContent("#authGo"), EXPECT.ja.sendCode);
      assert.equal(await page.getAttribute("html", "lang"), "ja");
      assert.equal(await page.evaluate(() => localStorage.getItem("magpie.cloud.lang")), "ja");

      // a later visit without the query keeps the reader's pick
      await page.goto(BASE + "/");
      await page.locator("#signin h1").filter({ hasText: EXPECT.ja.title }).waitFor();
      assert.equal(await page.evaluate(() => window.magpieCloud.lang()), "ja");
      assert.equal(await page.textContent("#authGo"), EXPECT.ja.sendCode);
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: the page opens at /cloud/ too`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, errors } = await open(browser, { lang: "en", path: "/cloud/" });
    try {
      await page.locator("#signin h1").filter({ hasText: EXPECT.en.title }).waitFor();
      assert.equal(await page.getAttribute("html", "lang"), "en");
      // the script and the sheet were found beside the page, not at the root
      assert.equal(await page.evaluate(() => !!window.magpieCloud), true);
      assert.equal(
        await page.evaluate(() => [...document.styleSheets].some((s) => s.href && s.href.endsWith("/cloud/style.css"))),
        true
      );
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: the browser's own language is used when the page names none`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, errors } = await open(browser, { query: "", locale: "zh-TW" });
    try {
      await page.locator("#signin h1").filter({ hasText: EXPECT["zh-TW"].title }).waitFor();
      assert.equal(await page.getAttribute("html", "lang"), "zh-TW");
      assert.equal(await page.textContent("#authGo"), EXPECT["zh-TW"].sendCode);
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });
}

test("chromium: a rate-limited send names the wait, and a failed one names no server detail", async (t) => {
  const browser = await launch("chromium");
  t.after(() => browser.close());
  const { context, page, state, errors } = await open(browser, { lang: "en" });
  try {
    await page.fill("#email", "ann@example.test");

    // a 429's retryAfter is a number, and it is said in words
    state.codeStatus = 429;
    state.codeReply = { error: "rate_limited", message: "Too many requests. Try again later.", retryAfter: 7 };
    await page.click("#authGo");
    await page.locator("#authError").waitFor();
    assert.equal(await page.textContent("#authError"), "Too many requests. Try again in 7 seconds.");

    // a 500 whose body names the database says only that the server is down
    state.codeStatus = 500;
    state.codeReply = { error: "unavailable", message: "pq: password authentication failed for user magpie" };
    await page.click("#authGo");
    await page.locator("#authError").filter({ hasText: "The server is unavailable. Try again." }).waitFor();
    const shown = await page.textContent("#authError");
    assert.doesNotMatch(shown, /password|pq:|magpie/i);
    assert.equal(await page.locator("#codeGroup").isHidden(), true);
    assert.equal(await page.locator("#portal").isHidden(), true);
    assert.deepEqual(errors, []);
  } finally {
    await context.close();
  }
});

test("chromium: the page has no inline script, no native select and no external asset", async () => {
  const html = await fs.readFile(path.join(ASSETS, "index.html"), "utf8");
  assert.doesNotMatch(html, /<script(?![^>]*\bsrc=)[^>]*>/i, "every script is a file, for the CSP");
  assert.doesNotMatch(html, /\son[a-z]+\s*=/i, "no inline event handler");
  assert.doesNotMatch(html, /<select\b/i, "no native select");
  assert.doesNotMatch(html, /(?:src|href)="https?:\/\//i, "no external asset");
  assert.doesNotMatch(html, /javascript:/i);
  assert.match(html, /http-equiv="Content-Security-Policy"[^>]*script-src 'self'/, "the page carries the policy on /cloud/ too");
  const js = await fs.readFile(path.join(ASSETS, "app.js"), "utf8");
  assert.doesNotMatch(js, /document\.write\s*\(/, "nothing writes HTML from a reply");
  assert.doesNotMatch(js, /\.innerHTML\s*=/, "nothing writes HTML from a reply");
});
