// magpie cloud's public landing page (internal/gui/assets/cloud/index.html),
// run the way the other GUI tests run: the real files, every API answered by
// a fixture here, Chromium and WebKit, and every language the app ships. It
// checks the copy, the entry points that follow the public policy and the
// session, a failed policy read, a phone's width, and that the page carries
// no inline script, no native select and no external asset.
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
const POLICY = { registrationEnabled: true, passwordLoginEnabled: true, codeLoginEnabled: true, passwordMinLength: 12 };

// The page's own heading, the two entry points, a section head and the note a
// closed deployment shows. A missing or stale translation fails one of these,
// not only a lookup of the dictionary.
const EXPECT = {
  en: {
    langName: "English",
    title: "Your own model access, behind one endpoint.",
    signIn: "Sign in",
    create: "Create account",
    features: "What the relay does",
    closed: "Registration is closed on this deployment. Sign in if you already have an account."
  },
  zh: {
    langName: "简体中文",
    title: "你自己的模型访问，收在一个入口之后。",
    signIn: "登录",
    create: "创建账户",
    features: "中转服务提供什么",
    closed: "此部署已关闭注册。如已有账户，请直接登录。"
  },
  "zh-TW": {
    langName: "繁體中文",
    title: "你自己的模型存取，收在一個入口之後。",
    signIn: "登入",
    create: "建立帳號",
    features: "中繼服務提供什麼",
    closed: "此部署已關閉註冊。若已有帳號，請直接登入。"
  },
  ja: {
    langName: "日本語",
    title: "自分のモデルアクセスを、ひとつのエンドポイントの内側に。",
    signIn: "サインイン",
    create: "アカウント作成",
    features: "リレーが提供するもの",
    closed: "このデプロイでは新規登録を停止しています。アカウントをお持ちの場合はサインインしてください。"
  },
  de: {
    langName: "Deutsch",
    title: "Dein eigener Modellzugang, hinter einem Endpunkt.",
    signIn: "Anmelden",
    create: "Konto erstellen",
    features: "Was das Relay leistet",
    closed: "Die Registrierung ist auf dieser Installation geschlossen. Melde dich an, wenn du bereits ein Konto hast."
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
    viewport: opts.viewport || { width: 1200, height: 900 },
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
    meStatus: opts.meStatus || 0,
    calls: []
  };

  await page.route("**/*", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const p = url.pathname;
    const json = (status, data) => route.fulfill({ status, contentType: "application/json; charset=utf-8", body: JSON.stringify(data) });
    if (p.startsWith("/api/")) state.calls.push({ method: request.method(), path: p });
    if (await asset(route, p)) return;
    if (p === "/api/auth/policy") {
      if (state.policyStatus !== 200) return json(state.policyStatus, { error: "unavailable" });
      return json(200, state.policy);
    }
    if (p === "/api/auth/me") {
      if (state.meStatus) return json(state.meStatus, { error: "unavailable" });
      return state.user ? json(200, { user: state.user }) : json(401, { error: "unauthenticated" });
    }
    return route.fulfill({ status: 404, body: "" });
  });

  const query = opts.query != null ? opts.query : opts.lang ? "?lang=" + encodeURIComponent(opts.lang) : "";
  await page.goto(BASE + (opts.path || "/") + query);
  if (opts.path !== "/app") {
    await page.waitForFunction(() => !!(window.magpieCloud && window.magpieCloud.landing && window.magpieCloud.landing.done()));
  }
  return { context, page, state, errors };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: the landing page speaks every language and offers the way in`, async (t) => {
    assert(["chromium", "webkit"].includes(engine), "BROWSER must be chromium or webkit");
    const browser = await launch(engine);
    t.after(() => browser.close());
    for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
      await t.test(lang, async () => {
        const w = EXPECT[lang];
        const { context, page, errors } = await open(browser, { lang });
        try {
          assert.equal(await page.getAttribute("html", "lang"), lang);
          assert.equal(await page.textContent("#heroTitle"), w.title);
          assert.equal(await page.textContent("#navAuth a[href='/login']"), w.signIn);
          assert.equal(await page.textContent("#navAuth a[href='/register']"), w.create);
          assert.equal(await page.textContent("#featuresTitle"), w.features);
          assert.equal(await page.textContent("#langNow"), w.langName);
          assert.equal(await page.getAttribute("#heroNote", "hidden"), null, "the sign-in note is drawn once the policy is read");

          // the entry points lead where they say, and the demo card stays inside
          assert.equal(await page.getAttribute("#heroCta a[href='/register']", "class"), "btn primary big");
          assert.equal(await page.getAttribute("#heroCta a[href='/login']", "class"), "btn ghost big");
          assert.equal(await page.locator("select").count(), 0, "no native select");
          assert.deepEqual(errors, []);
        } finally {
          await context.close();
        }
      });
    }
  });

  test(`${engine}: a closed deployment offers sign-in alone`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, state, errors } = await open(browser, {
      lang: "en",
      policy: { registrationEnabled: false }
    });
    try {
      await page.locator("#heroNote").filter({ hasText: EXPECT.en.closed }).waitFor();
      assert.equal(await page.locator("#navAuth a[href='/register']").count(), 0);
      assert.equal(await page.locator("#heroCta a[href='/register']").count(), 0);
      assert.equal(await page.locator("#ctaBandActions a[href='/register']").count(), 0);
      assert.equal(await page.locator("#navAuth a[href='/login']").count(), 1);
      assert.equal(await page.getAttribute("#navAuth a[href='/login']", "class"), "btn primary");
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: a signed-in reader is offered the console`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, errors } = await open(browser, { lang: "en", user: USER });
    try {
      await page.locator("#heroNote").filter({ hasText: "Signed in as ann@example.test." }).waitFor();
      for (const box of ["#navAuth", "#heroCta", "#ctaBandActions", "#footAuth"]) {
        assert.equal(await page.locator(`${box} a[href='/app']`).count(), 1, box + " opens the portal");
        assert.equal(await page.locator(`${box} a[href='/login']`).count(), 0, box + " hides sign-in");
        assert.equal(await page.locator(`${box} a[href='/register']`).count(), 0, box + " hides registration");
      }
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: a policy read that fails shows the default entry points`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, errors } = await open(browser, { lang: "en", policyStatus: 503 });
    try {
      await page.locator("#heroNote").filter({ hasText: "Sign-in settings could not be loaded" }).waitFor();
      assert.equal(await page.locator("#navAuth a[href='/login']").count(), 1);
      assert.equal(await page.locator("#navAuth a[href='/register']").count(), 1);
      assert.equal(await page.locator("#heroTitle").isVisible(), true);
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: the landing page fits a phone and has no sideways scroll`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    for (const width of [390, 768, 1280]) {
      await t.test(`${width}px`, async () => {
        const { context, page, errors } = await open(browser, { lang: "de", viewport: { width, height: 900 } });
        try {
          await settled(page);
          assert.equal(
            await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth),
            true,
            "no sideways scroll at " + width + "px"
          );
          assert.equal(await page.locator("#featuresTitle").isVisible(), true);
          assert.equal(await page.locator(".demo-card").isVisible(), true);
          // the code sample scrolls inside its own box, never the page
          assert.ok(await page.evaluate(() => {
            const code = document.querySelector(".code");
            const style = getComputedStyle(code);
            return style.overflowX === "auto" && code.getBoundingClientRect().right <= window.innerWidth;
          }));
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
      await page.click("#langBtn");
      await page.locator("#langMenu").waitFor();
      assert.equal(await page.locator("#langMenu button[role=menuitemradio]").count(), 5);
      await page.locator("#langMenu button", { hasText: "日本語" }).click();
      await page.locator("#heroTitle").filter({ hasText: EXPECT.ja.title }).waitFor();
      assert.equal(await page.getAttribute("html", "lang"), "ja");
      assert.equal(await page.evaluate(() => localStorage.getItem("magpie.cloud.lang")), "ja");
      await page.goto(BASE + "/");
      await page.locator("#heroTitle").filter({ hasText: EXPECT.ja.title }).waitFor();
      assert.equal(await page.evaluate(() => window.magpieCloud.lang()), "ja");
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
      await page.locator("#heroTitle").filter({ hasText: EXPECT.en.title }).waitFor();
      assert.equal(await page.evaluate(() => !!window.magpieCloud), true);
      assert.equal(
        await page.evaluate(() => [...document.styleSheets].some((s) => s.href && s.href.endsWith("/cloud/landing.css"))),
        true
      );
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });
}

test("chromium: every marked string on every page is in five languages", async () => {
  const src = await fs.readFile(path.join(ASSETS, "i18n.js"), "utf8");
  const DICT = new Function(src + "\n;return DICT;")();
  const langs = Object.keys(DICT);
  assert.deepEqual(langs.sort(), ["de", "en", "ja", "zh", "zh-TW"]);
  for (const name of ["index.html", "login.html", "register.html", "reset.html", "portal.html"]) {
    const html = await fs.readFile(path.join(ASSETS, name), "utf8");
    const keys = [...html.matchAll(/data-(?:t|tp|tt|ta)="([^"]+)"/g)].map((m) => m[1]);
    assert.ok(keys.length > 10, name + " marks its strings");
    for (const key of keys) {
      for (const lang of langs) {
        assert.ok(key in DICT[lang], `${name}: ${lang} is missing ${key}`);
        assert.ok(String(DICT[lang][key]).trim(), `${name}: ${lang} has an empty ${key}`);
      }
    }
  }
});

test("chromium: every public page is script-file only and loads nothing remote", async () => {
  for (const name of ["index.html", "login.html", "register.html", "reset.html", "portal.html"]) {
    const html = await fs.readFile(path.join(ASSETS, name), "utf8");
    assert.doesNotMatch(html, /<script(?![^>]*\bsrc=)[^>]*>/i, name + ": every script is a file, for the CSP");
    assert.doesNotMatch(html, /\son[a-z]+\s*=/i, name + ": no inline event handler");
    assert.doesNotMatch(html, /<select\b/i, name + ": no native select");
    assert.doesNotMatch(html, /(?:src|href)="https?:\/\//i, name + ": no external asset");
    assert.doesNotMatch(html, /javascript:/i, name + ": no javascript: URL");
    assert.match(html, /http-equiv="Content-Security-Policy"[^>]*script-src 'self'/, name + ": carries the policy");
  }
  for (const name of ["shared.js", "landing.js", "auth.js", "app.js"]) {
    const js = await fs.readFile(path.join(ASSETS, name), "utf8");
    assert.doesNotMatch(js, /document\.write\s*\(/, name + ": nothing writes HTML from a reply");
    assert.doesNotMatch(js, /\.innerHTML\s*=/, name + ": nothing writes HTML from a reply");
  }
});
