// magpie cloud's portal (internal/gui/assets/cloud), driven through the real
// page: providers, house channels, gateway keys and administration, with the
// cloud API answered by fixtures in this file. The tests check what the
// reader sees and what reaches the API: a provider's upstream key is never
// drawn after Save, a new gateway secret is drawn exactly once and cannot
// come back from a list, and a member's page never asks for administration.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { settled } = require("./reader.cjs");

const ASSETS = path.resolve(__dirname, "../assets/cloud");
const BASE = "http://magpie.test";

const USER = { id: "user-ann", email: "ann@example.test", role: "user", emailVerified: true, createdAt: "2026-11-01T00:00:00Z" };
const ADMIN = { id: "user-ops", email: "ops@example.test", role: "admin", emailVerified: true, createdAt: "2026-10-01T00:00:00Z" };

const TEAM = "11111111-1111-4111-8111-111111111111";
const RESEARCH = "22222222-2222-4222-8222-222222222222";
const KEY_ID = "33333333-3333-4333-8333-333333333333";
const NEW_KEY_ID = "44444444-4444-4444-8444-444444444444";
const NEW_CHANNEL = "55555555-5555-4555-8555-555555555555";
const GATEWAY_SECRET = "sk-magpie-key-0123456789abcdef0123456789abcdef";
const UPSTREAM = "sk-upstream-never-drawn-4289";
const TEAM_PROVIDER = "h-123456789abc-team-relay";
const PRICES = [
  {
    channel: TEAM,
    provider: TEAM_PROVIDER,
    model: "m1",
    price: {
      input_micros_per_mtok: 2000000,
      output_micros_per_mtok: 8000000,
      cache_read_micros_per_mtok: 500000,
      cache_write_micros_per_mtok: 500000,
      fixed_per_call_micros: 1000
    },
    effectiveFrom: "2026-11-01T00:00:00Z"
  }
];

const provider = (id, name, extra = {}) => ({
  id, name, chat: "https://api.relay.example/v1", responses: "", anthropic: "", gemini: "",
  models: ["m1", "m2"], off: false, keySet: true,
  keys: [{ name: "main", masked: "sk-a…zzzz", off: false }], ...extra
});

function seeded(user) {
  return {
    user,
    wallet: { balanceMicros: 12340000, heldMicros: 500000, availableMicros: 11840000, currency: "CNY", paymentsEnabled: false, presets: [] },
    providers: [provider("u-123456789abc-my-relay", "My relay")],
    houses: [
      { id: TEAM, name: "Team", description: "Shared coding models", status: "published", createdAt: "2026-11-01T00:00:00Z", updatedAt: "2026-11-01T00:00:00Z", granted: false },
      { id: RESEARCH, name: "Research", description: "", status: "published", createdAt: "2026-11-02T00:00:00Z", updatedAt: "2026-11-02T00:00:00Z", granted: true }
    ],
    keys: [
      { id: KEY_ID, name: "Laptop", masked: "sk-magpie-key-…7890", models: [], houseChannels: [], disabled: false, maxConcurrency: 0, createdAt: "2026-11-03T00:00:00Z" }
    ],
    admin: [
      { id: TEAM, name: "Team", description: "Shared coding models", status: "draft", createdAt: "2026-11-01T00:00:00Z", updatedAt: "2026-11-01T00:00:00Z" }
    ],
    prices: { [TEAM]: JSON.parse(JSON.stringify(PRICES)) },
    usage: [],
    groups: [],
    users: [{ id: user.id, email: user.email, role: user.role, status: "active", createdAt: user.createdAt, balanceMicros: 1000000, heldMicros: 0 }],
    audit: [],
    adminProviders: { [TEAM]: [provider(TEAM_PROVIDER, "Team relay")] },
    policy: {
      registrationEnabled: true,
      passwordLoginEnabled: true,
      codeLoginEnabled: true,
      passwordMinLength: 12,
      version: 3,
      usersWithoutPassword: 1,
      mailReady: true
    },
    orders: {},
    nextOrder: 1,
    advanceOrder: false,
    pollStatus: "pending",
    pollCredit: "none",
    providerError: "",
    orderReject: 0,
    priceUnknown: false,
    credited: new Set(),
    calls: [],
    fail: {},
    delay: {},
    malformed: {},
    secret: GATEWAY_SECRET
  };
}

async function asset(route, pathname) {
  let rel = null;
  if (pathname === "/app" || pathname === "/cloud/portal.html") rel = "portal.html";
  else if (pathname === "/" || pathname === "/cloud/" || pathname === "/cloud/index.html") rel = "index.html";
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

const STATUS_TEXT = { 400: "Bad Request", 403: "Forbidden", 404: "Not Found", 429: "Too Many Requests", 500: "Internal Server Error", 503: "Service Unavailable" };

async function maybeFail(route, db, key) {
  const status = db.fail[key];
  if (!status) return false;
  if (status === "network") {
    await route.abort("failed");
    return true;
  }
  await route.fulfill({
    status,
    contentType: "application/json; charset=utf-8",
    body: JSON.stringify({ error: STATUS_TEXT[status] || "Internal Server Error" })
  });
  return true;
}

async function launch(engine) {
  return engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" });
}

async function open(browser, opts = {}) {
  const context = await browser.newContext({
    viewport: opts.viewport || { width: 900, height: 800 },
    locale: "en-US",
    reducedMotion: "reduce"
  });
  const page = await context.newPage();
  page.setDefaultTimeout(10000);
  page.setDefaultNavigationTimeout(60000);
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  const db = seeded(opts.user || USER);
  if (opts.edit) opts.edit(db);

  await page.route("**/*", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const p = url.pathname;
    const method = request.method();
    const json = (status, data) => route.fulfill({ status, contentType: "application/json; charset=utf-8", body: JSON.stringify(data) });
    let body = null;
    try { body = request.postDataJSON(); } catch (e) {}
    if (p.startsWith("/api/")) db.calls.push({ method, path: p, body });

    if (await asset(route, p)) return;
    if (p === "/api/auth/me") return db.user ? json(200, { user: db.user }) : json(401, { error: "unauthenticated" });
    if (p === "/api/auth/code" && method === "POST") return json(200, { ok: true, retryAfter: 0 });
    if (p === "/api/auth/login" && method === "POST") return json(401, { error: "invalid_code" });
    if (p === "/api/auth/logout" && method === "POST") {
      db.user = null;
      return route.fulfill({ status: 204, body: "" });
    }

    if (p === "/api/cloud/wallet" && method === "GET") {
      if (db.delay.wallet) await new Promise((r) => setTimeout(r, db.delay.wallet));
      if (await maybeFail(route, db, "wallet")) return;
      if (db.malformed.wallet) return json(200, { not: "a wallet" });
      const out = {
        balanceMicros: db.wallet.balanceMicros,
        heldMicros: db.wallet.heldMicros,
        availableMicros: db.wallet.availableMicros,
        currency: db.wallet.currency,
        paymentsEnabled: db.wallet.paymentsEnabled
      };
      if (db.wallet.paymentsEnabled) out.presets = db.wallet.presets;
      return json(200, out);
    }
    if (p === "/api/cloud/orders" && method === "GET") return json(200, Object.values(db.orders));
    if (p === "/api/cloud/orders" && method === "POST") {
      if (await maybeFail(route, db, "createOrder")) return;
      if (db.orderReject) return json(db.orderReject, { error: STATUS_TEXT[db.orderReject] || "Bad Request" });
      const preset = db.wallet.presets.find((x) => x.id === (body && body.presetId));
      if (!preset) return json(400, { error: "Bad Request" });
      const n = db.nextOrder++;
      const merchantNo = "MAGPIE" + String(n).padStart(6, "0");
      const order = {
        id: "order-" + n,
        merchant_order_no: merchantNo,
        user_id: db.user.id,
        provider: body.provider,
        amount_fen: preset.amount_fen,
        amount_micro_cny: preset.amount_fen * 10000,
        currency: "CNY",
        status: "pending",
        credit_state: "none",
        subject: "magpie recharge",
        created_at: "2026-11-26T10:00:00Z",
        updated_at: "2026-11-26T10:00:00Z",
        expires_at: "2026-11-26T10:30:00Z"
      };
      db.orders[merchantNo] = order;
      const checkout = { order: order };
      if (body.provider === "alipay") checkout.pay_url = "https://pay.example/alipay?order=" + merchantNo;
      else checkout.code_url = "weixin://wxpay/bizpayurl?pr=" + merchantNo;
      return json(200, { checkout: checkout, providerError: db.providerError || "" });
    }
    const orderGet = p.match(/^\/api\/cloud\/orders\/([^/]+)$/);
    if (orderGet && method === "GET") {
      if (await maybeFail(route, db, "order")) return;
      const order = db.orders[decodeURIComponent(orderGet[1])];
      if (!order) return json(404, { error: "Not Found" });
      if (db.advanceOrder && order.status === "pending") {
        order.status = db.pollStatus || "pending";
        order.credit_state = db.pollCredit || "none";
        if (order.status === "success" && order.credit_state === "credited" && !db.credited.has(order.merchant_order_no)) {
          db.credited.add(order.merchant_order_no);
          db.wallet.balanceMicros += order.amount_micro_cny;
          db.wallet.availableMicros += order.amount_micro_cny;
          order.credited_at = "2026-11-26T10:01:00Z";
        }
      }
      return json(200, order);
    }

    if (p === "/api/cloud/usage" && method === "GET") return json(200, db.usage);
    if (p === "/api/cloud/groups" && method === "GET") return json(200, db.groups);
    if (p === "/api/cloud/groups" && method === "POST") {
      const group = { id: "g-123456789abc-" + body.id, name: body.name, members: body.members, routing: body.routing, ownerUserId: db.user.id };
      db.groups.push(group);
      return json(200, group);
    }
    const deleteGroup = p.match(/^\/api\/cloud\/groups\/([^/]+)$/);
    if (deleteGroup && method === "DELETE") {
      const id = decodeURIComponent(deleteGroup[1]);
      const at = db.groups.findIndex((group) => group.id === id);
      if (at < 0) return json(404, { error: "Not Found" });
      db.groups.splice(at, 1);
      return json(200, { ok: true });
    }
    if (p === "/api/cloud/admin/users" && method === "GET") return json(200, db.users);
    const userStatus = p.match(/^\/api\/cloud\/admin\/users\/([^/]+)\/status$/);
    if (userStatus && method === "POST") {
      const user = db.users.find((u) => u.id === decodeURIComponent(userStatus[1]));
      if (!user) return json(404, { error: "Not Found" });
      user.status = body.status;
      db.audit.unshift({ id: "audit-status", action: "user." + body.status, targetType: "user", targetId: user.id, details: { status: body.status }, createdAt: "2026-11-26T10:00:00Z" });
      return json(200, { ok: true });
    }
    if (p === "/api/cloud/admin/wallet/credit" && method === "POST") {
      const user = db.users.find((u) => u.id === body.userId);
      if (!user) return json(404, { error: "Not Found" });
      user.balanceMicros += body.amountMicros;
      db.audit.unshift({ id: "audit-credit", action: "wallet.credit", targetType: "user", targetId: user.id, details: { amountMicros: body.amountMicros }, createdAt: "2026-11-26T10:01:00Z" });
      return json(200, { entryId: "credit-entry", userId: user.id, amountMicros: body.amountMicros, balanceMicros: user.balanceMicros, replayed: false });
    }
    if (p === "/api/cloud/admin/orders" && method === "GET") return json(200, Object.values(db.orders));
    if (p === "/api/cloud/admin/audit" && method === "GET") return json(200, db.audit);
    if (p === "/api/cloud/admin/auth-policy" && method === "GET") {
      if (db.delay.policy) await new Promise((r) => setTimeout(r, db.delay.policy));
      if (await maybeFail(route, db, "policy")) return;
      return json(200, db.policy);
    }
    if (p === "/api/cloud/admin/auth-policy" && method === "PUT") {
      if (await maybeFail(route, db, "savePolicy")) return;
      if (!body || body.version !== db.policy.version) return json(409, { error: "version_conflict" });
      for (const key of ["registrationEnabled", "passwordLoginEnabled", "codeLoginEnabled", "passwordMinLength"]) {
        if (!(key in body)) return json(400, { error: "invalid_request" });
        db.policy[key] = body[key];
      }
      db.policy.version++;
      db.audit.unshift({
        id: "audit-policy",
        action: "auth.policy",
        targetType: "deployment",
        targetId: "auth-policy",
        details: {
          registrationEnabled: body.registrationEnabled,
          passwordLoginEnabled: body.passwordLoginEnabled,
          codeLoginEnabled: body.codeLoginEnabled,
          passwordMinLength: body.passwordMinLength
        },
        createdAt: "2026-11-26T10:02:00Z"
      });
      return json(200, db.policy);
    }

    if (p === "/api/cloud/providers" && method === "GET") {
      if (db.delay.providers) await new Promise((r) => setTimeout(r, db.delay.providers));
      if (await maybeFail(route, db, "providers")) return;
      return json(200, db.malformed.providers ? { not: "a list" } : db.providers);
    }
    if (p === "/api/cloud/providers" && method === "POST") {
      if (await maybeFail(route, db, "createProvider")) return;
      const view = {
        id: "u-123456789abc-" + (body.id || "provider"),
        name: body.name || "",
        ownerUserId: db.user ? db.user.id : "",
        chat: body.chat || "",
        responses: body.responses || "",
        anthropic: body.anthropic || "",
        gemini: body.gemini || "",
        models: body.models || [],
        off: false,
        keySet: true,
        keys: [{ name: "", masked: "sk-u…test", off: false }]
      };
      db.providers.push(view);
      return json(200, view);
    }
    const deleteProvider = p.match(/^\/api\/cloud\/providers\/([^/]+)$/);
    if (deleteProvider && method === "DELETE") {
      const id = decodeURIComponent(deleteProvider[1]);
      const at = db.providers.findIndex((provider) => provider.id === id);
      if (at < 0) return json(404, { error: "Not Found" });
      db.providers.splice(at, 1);
      return json(200, { ok: true });
    }

    if (p === "/api/cloud/house-channels" && method === "GET") {
      if (db.delay.houses) await new Promise((r) => setTimeout(r, db.delay.houses));
      if (await maybeFail(route, db, "houses")) return;
      return json(200, db.malformed.houses ? { not: "a list" } : db.houses);
    }
    const grant = p.match(/^\/api\/cloud\/house-channels\/([^/]+)\/grant$/);
    if (grant) {
      if (await maybeFail(route, db, "grant")) return;
      const id = decodeURIComponent(grant[1]);
      const channel = db.houses.find((c) => c.id === id);
      if (!channel) return json(404, { error: "Not Found" });
      if (method === "DELETE") channel.granted = false;
      else channel.granted = true;
      return json(200, { ok: true });
    }

    if (p === "/api/cloud/keys" && method === "GET") {
      if (db.delay.keys) await new Promise((r) => setTimeout(r, db.delay.keys));
      if (await maybeFail(route, db, "keys")) return;
      return json(200, db.malformed.keys ? { not: "a list" } : db.keys);
    }
    if (p === "/api/cloud/keys" && method === "POST") {
      if (await maybeFail(route, db, "createKey")) return;
      const view = {
        id: NEW_KEY_ID,
        name: body.name || "",
        masked: "sk-magpie-key-…cdef",
        models: body.models || [],
        houseChannels: body.houseChannels || [],
        disabled: false,
        maxConcurrency: body.maxConcurrency || 0,
        createdAt: "2026-11-20T00:00:00Z"
      };
      if (body.expiresAt) view.expiresAt = body.expiresAt;
      db.keys.push(view);
      return json(200, Object.assign({}, view, { secret: db.secret }));
    }
    const deleteKey = p.match(/^\/api\/cloud\/keys\/([^/]+)$/);
    if (deleteKey && method === "DELETE") {
      if (await maybeFail(route, db, "revoke")) return;
      const key = db.keys.find((k) => k.id === decodeURIComponent(deleteKey[1]));
      if (!key) return json(404, { error: "Not Found" });
      key.disabled = true;
      return json(200, { ok: true });
    }

    if (p === "/api/cloud/admin/house-channels" && method === "GET") {
      if (await maybeFail(route, db, "admin")) return;
      return json(200, db.admin);
    }
    if (p === "/api/cloud/admin/house-channels" && method === "POST") {
      if (await maybeFail(route, db, "createChannel")) return;
      const channel = {
        id: NEW_CHANNEL, name: body.name, description: body.description || "", status: "draft",
        createdAt: "2026-11-21T00:00:00Z", updatedAt: "2026-11-21T00:00:00Z"
      };
      db.admin.push(channel);
      return json(200, channel);
    }
    const status = p.match(/^\/api\/cloud\/admin\/house-channels\/([^/]+)\/status$/);
    if (status && method === "POST") {
      if (await maybeFail(route, db, "status")) return;
      const channel = db.admin.find((c) => c.id === decodeURIComponent(status[1]));
      if (!channel) return json(404, { error: "Not Found" });
      channel.status = body.status;
      return json(200, { ok: true });
    }
    const houseProvider = p.match(/^\/api\/cloud\/admin\/house-channels\/([^/]+)\/providers$/);
    if (houseProvider && method === "POST") {
      if (await maybeFail(route, db, "houseProvider")) return;
      return json(200, {
        id: "h-123456789abc-" + (body.id || "provider"),
        name: body.name || "",
        houseChannelId: decodeURIComponent(houseProvider[1]),
        chat: body.chat || "",
        responses: body.responses || "",
        anthropic: body.anthropic || "",
        gemini: body.gemini || "",
        models: body.models || [],
        off: false,
        keySet: true,
        keys: []
      });
    }
    const adminProviders = p.match(/^\/api\/cloud\/admin\/house-channels\/([^/]+)\/providers$/);
    if (adminProviders && method === "GET") return json(200, db.adminProviders[decodeURIComponent(adminProviders[1])] || []);
    const deleteHouseProvider = p.match(/^\/api\/cloud\/admin\/house-channels\/([^/]+)\/providers\/([^/]+)$/);
    if (deleteHouseProvider && method === "DELETE") {
      const channel = decodeURIComponent(deleteHouseProvider[1]);
      const id = decodeURIComponent(deleteHouseProvider[2]);
      const list = db.adminProviders[channel] || [];
      const at = list.findIndex((provider) => provider.id === id);
      if (at < 0) return json(404, { error: "Not Found" });
      list.splice(at, 1);
      return json(200, { ok: true });
    }
    const prices = p.match(/^\/api\/cloud\/admin\/house-channels\/([^/]+)\/prices$/);
    if (prices && method === "GET") {
      if (await maybeFail(route, db, "prices")) return;
      return json(200, db.prices[decodeURIComponent(prices[1])] || []);
    }
    if (prices && method === "PUT") {
      if (await maybeFail(route, db, "savePrice")) return;
      if (db.priceUnknown) return json(404, { error: "Not Found" });
      const id = decodeURIComponent(prices[1]);
      const list = db.prices[id] || (db.prices[id] = []);
      const item = {
        channel: id,
        provider: body.provider,
        model: body.model,
        price: body.price,
        effectiveFrom: "2026-11-26T10:00:00Z"
      };
      const at = list.findIndex((x) => x.provider === body.provider && x.model === body.model);
      if (at >= 0) list[at] = item;
      else list.push(item);
      return json(200, { ok: true });
    }

    return route.fulfill({ status: 404, body: "" });
  });

  await page.goto(BASE + "/app?lang=en");
  return { context, page, db, errors };
}

const calls = (db, method, pathname) => db.calls.filter((c) => c.method === method && c.path === pathname);
const bodyText = (page) => page.evaluate(() => document.body.innerText);

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: a provider is listed and added without drawing its upstream key again`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, db, errors } = await open(browser, {});
    try {
      await page.locator("#providersList .row").first().waitFor();
      const first = page.locator("#providersList .row").first();
      const shown = await first.innerText();
      assert.match(shown, /My relay/);
      assert.match(shown, /u-123456789abc-my-relay/);
      assert.match(shown, /Chat/);
      assert.match(shown, /Models: 2/);
      assert.match(shown, /Key saved/);
      assert.equal(await page.locator("#providersList .row button").count(), 1, "a provider can be deleted");

      // the form refuses what the API would only answer with a bare 400
      await page.click("#addProvider");
      await page.locator("#providerForm").waitFor();
      await page.click("#pSave");
      await page.locator("#pError").filter({ hasText: "Enter a name." }).waitFor();
      assert.equal(calls(db, "POST", "/api/cloud/providers").length, 0);
      await page.fill("#pName", "Relay two");
      assert.equal(await page.inputValue("#pId"), "relay-two");
      await page.click('#pProtocol [data-protocol="responses"]');
      assert.equal(await page.getAttribute('#pProtocol [data-protocol="responses"]', "aria-pressed"), "true");
      await page.fill("#pBase", "not a url");
      await page.fill("#pKey", UPSTREAM);
      await page.click("#pSave");
      await page.locator("#pError").filter({ hasText: "An endpoint starts with http:// or https://." }).waitFor();
      assert.equal(calls(db, "POST", "/api/cloud/providers").length, 0);

      await page.fill("#pBase", "https://relay.example/v1");
      await page.fill("#pModels", "alpha, beta\nAlpha");
      await page.click("#pSave");
      await page.locator("#notice").filter({ hasText: "Provider added." }).waitFor();
      const posted = calls(db, "POST", "/api/cloud/providers");
      assert.equal(posted.length, 1);
      assert.deepEqual(posted[0].body, {
        id: "relay-two",
        name: "Relay two",
        responses: "https://relay.example/v1",
        models: ["alpha", "beta"],
        key: UPSTREAM
      });
      // the key left the page with the request
      assert.equal(await page.inputValue("#pKey"), "");
      assert.equal(await page.locator("#providerForm").isHidden(), true);
      assert.equal(await page.locator("#providersList .row").count(), 2);
      assert.match(await page.locator("#providersList .row").nth(1).innerText(), /Relay two/);
      assert.equal((await bodyText(page)).includes(UPSTREAM), false, "the upstream key is never drawn");
      assert.equal(
        await page.evaluate((s) => document.documentElement.outerHTML.includes(s), UPSTREAM),
        false,
        "the upstream key is gone from the DOM"
      );
      assert.equal(
        await page.evaluate(() => [...document.querySelectorAll("input,textarea")].some((e) => e.value.includes("sk-upstream"))),
        false,
        "no field keeps the upstream key"
      );
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: house channels are added and removed, and a refusal changes nothing`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, db, errors } = await open(browser, {});
    try {
      await page.locator("#housesList .row").first().waitFor();
      const team = () => page.locator("#housesList .row").filter({ hasText: "Team" });
      assert.match(await team().innerText(), /Not added/);

      // a failed grant is said safely and leaves the channel as it was
      db.fail.grant = 500;
      await team().getByRole("button", { name: "Add" }).click();
      await page.locator("#notice[data-tone=error]").filter({ hasText: "The server is unavailable. Try again." }).waitFor();
      assert.match(await team().innerText(), /Not added/);
      assert.equal((await page.locator("#notice").innerText()).includes("Internal Server Error"), false);

      db.fail.grant = 0;
      await team().getByRole("button", { name: "Add" }).click();
      await team().getByRole("button", { name: "Remove" }).waitFor();
      assert.match(await team().innerText(), /Added/);
      // the refused attempt and the accepted one both reached the fixture
      assert.equal(calls(db, "POST", "/api/cloud/house-channels/" + TEAM + "/grant").length, 2);

      await team().getByRole("button", { name: "Remove" }).click();
      await team().getByRole("button", { name: "Add" }).waitFor();
      assert.match(await team().innerText(), /Not added/);
      assert.equal(calls(db, "DELETE", "/api/cloud/house-channels/" + TEAM + "/grant").length, 1);
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: a gateway key is created, its secret drawn once and revoked`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, db, errors } = await open(browser, {});
    try {
      await page.locator("#keysList .row").first().waitFor();
      await page.locator("#housesList .row").first().waitFor();
      await page.click("#newKey");
      await page.locator("#keyForm").waitFor();

      // an empty name is refused before anything is sent
      await page.click("#keySave");
      await page.locator("#keyError").filter({ hasText: "Enter a name." }).waitFor();
      assert.equal(calls(db, "POST", "/api/cloud/keys").length, 0);

      await page.fill("#keyName", "Laptop two");
      await page.fill("#keyModels", "m1, m2");
      await page.fill("#keyConcurrency", "4");
      await page.fill("#keyExpires", "2027-01-31");
      const research = page.locator("#keyChannels .chip").filter({ hasText: "Research" });
      await research.click();
      assert.equal(await research.getAttribute("aria-pressed"), "true");
      await page.click("#keySave");
      await page.locator("#secretBox").waitFor();

      // the secret is here, in the one-time box, and nowhere else
      assert.equal(await page.locator("#secretValue").textContent(), GATEWAY_SECRET);
      const posted = calls(db, "POST", "/api/cloud/keys");
      assert.equal(posted.length, 1);
      assert.deepEqual(posted[0].body, {
        name: "Laptop two",
        models: ["m1", "m2"],
        houseChannels: [RESEARCH],
        maxConcurrency: 4,
        expiresAt: "2027-01-31T23:59:59.000Z"
      });
      assert.equal(await page.locator("#keyForm").isHidden(), true);
      await page.click("#newKey");
      await page.locator("#keyForm").waitFor();
      assert.equal(await page.inputValue("#keyName"), "", "a new key starts from an empty form");
      assert.equal(await page.inputValue("#keyModels"), "");
      await page.click("#keyCancel");
      assert.equal(((await bodyText(page)).split(GATEWAY_SECRET).length - 1), 1, "the secret is drawn exactly once");
      assert.equal(
        await page.evaluate((s) => [...document.querySelectorAll("input,textarea")].some((e) => e.value.includes(s)), GATEWAY_SECRET),
        false,
        "no field holds the secret"
      );
      assert.equal(
        await page.evaluate((s) => Object.keys(localStorage).some((k) => String(localStorage.getItem(k)).includes(s)) || Object.keys(sessionStorage).some((k) => String(sessionStorage.getItem(k)).includes(s)), GATEWAY_SECRET),
        false,
        "the secret is not stored"
      );
      assert.equal(page.url().includes(GATEWAY_SECRET), false);
      assert.match(await page.locator("#keysList").innerText(), /Laptop two/);

      // Done takes it away for good; no list read can bring it back
      await page.click("#secretDone");
      assert.equal(await page.locator("#secretBox").isHidden(), true);
      assert.equal(await page.locator("#secretValue").textContent(), "", "the secret is removed from the page, not only hidden");
      assert.equal(await page.evaluate((s) => document.documentElement.outerHTML.includes(s), GATEWAY_SECRET), false, "the secret is gone from the DOM");
      assert.equal((await bodyText(page)).includes(GATEWAY_SECRET), false);
      await page.reload();
      await page.locator("#keysList .row").first().waitFor();
      assert.equal(await page.locator("#secretBox").isHidden(), true);
      assert.equal(await page.evaluate((s) => document.documentElement.outerHTML.includes(s), GATEWAY_SECRET), false);
      assert.equal((await bodyText(page)).includes(GATEWAY_SECRET), false);
      assert.match(await page.locator("#keysList").innerText(), /Laptop two/);

      // revoking asks once, then marks the key and tells the API
      const row = page.locator("#keysList .row").filter({ hasText: "Laptop two" });
      await row.getByRole("button", { name: "Revoke" }).click();
      await row.getByRole("button", { name: "Confirm revoke" }).click();
      await page.locator("#keysList .row").filter({ hasText: "Laptop two" }).filter({ hasText: "Revoked" }).waitFor();
      assert.equal(calls(db, "DELETE", "/api/cloud/keys/" + NEW_KEY_ID).length, 1);
      assert.equal(await page.locator("#keysList .row").filter({ hasText: "Laptop two" }).getByRole("button", { name: "Revoke" }).count(), 0);
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: an administrator creates a house channel, sets its state and adds a provider`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, db, errors } = await open(browser, { user: ADMIN });
    try {
      await page.locator("#adminPanel").waitFor({ state: "visible" });
      await page.locator("#adminList .row").first().waitFor();
      assert.match(await page.locator("#adminList .row").first().innerText(), /Team/);
      assert.match(await page.locator("#adminList .row").first().innerText(), /Draft/);

      // a channel needs a name
      await page.click("#newChannel");
      await page.locator("#channelForm").waitFor();
      await page.click("#chSave");
      await page.locator("#chError").filter({ hasText: "Enter a name." }).waitFor();
      assert.equal(calls(db, "POST", "/api/cloud/admin/house-channels").length, 0);

      await page.fill("#chName", "Ops");
      await page.fill("#chDesc", "Operations team");
      await page.click("#chSave");
      await page.locator("#notice").filter({ hasText: "House channel created." }).waitFor();
      const created = calls(db, "POST", "/api/cloud/admin/house-channels");
      assert.equal(created.length, 1);
      assert.deepEqual(created[0].body, { name: "Ops", description: "Operations team" });
      const ops = () => page.locator("#adminList .row").filter({ hasText: "Ops" });
      await ops().waitFor();
      assert.match(await ops().innerText(), /Draft/);

      // every state the API accepts, each sent by its own button
      await ops().getByRole("button", { name: "Published" }).click();
      await page.locator("#notice").filter({ hasText: "Status set to Published." }).waitFor();
      assert.match(await ops().innerText(), /Published/);
      await ops().getByRole("button", { name: "Offline" }).click();
      await page.locator("#notice").filter({ hasText: "Status set to Offline." }).waitFor();
      assert.match(await ops().innerText(), /Offline/);
      await ops().getByRole("button", { name: "Draft" }).click();
      await page.locator("#notice").filter({ hasText: "Status set to Draft." }).waitFor();
      assert.match(await ops().innerText(), /Draft/);
      const statuses = db.calls.filter((c) => c.method === "POST" && c.path === "/api/cloud/admin/house-channels/" + NEW_CHANNEL + "/status").map((c) => c.body.status);
      assert.deepEqual(statuses, ["published", "offline", "draft"]);

      // a provider added to the channel keeps its key off the page
      await ops().getByRole("button", { name: "Add provider" }).click();
      await page.locator("#houseProviderForm").waitFor();
      assert.match(await page.locator("#hpTitle").innerText(), /Ops/);
      await page.fill("#hpName", "Shared relay");
      assert.equal(await page.inputValue("#hpId"), "shared-relay");
      await page.click('#hpProtocol [data-protocol="anthropic"]');
      await page.fill("#hpBase", "https://shared.example");
      await page.fill("#hpKey", UPSTREAM);
      await page.fill("#hpModels", "shared-model");
      await page.click("#hpSave");
      await page.locator("#notice").filter({ hasText: "Provider added to this channel." }).waitFor();
      const added = calls(db, "POST", "/api/cloud/admin/house-channels/" + NEW_CHANNEL + "/providers");
      assert.equal(added.length, 1);
      assert.deepEqual(added[0].body, {
        id: "shared-relay",
        name: "Shared relay",
        models: ["shared-model"],
        key: UPSTREAM,
        anthropic: "https://shared.example"
      });
      assert.equal(await page.inputValue("#hpKey"), "");
      assert.equal(await page.locator("#houseProviderForm").isHidden(), true);
      // the channel names the provider it just took, and the price form
      // starts from that id
      assert.match(await ops().innerText(), /Last added provider/);
      assert.match(await ops().innerText(), /h-123456789abc-shared-relay/);
      await ops().getByRole("button", { name: "Prices" }).click();
      await page.locator(".price-panel .state-title").filter({ hasText: "No prices set yet" }).waitFor();
      await page.locator(".price-panel").getByRole("button", { name: "Add price" }).click();
      assert.equal(await page.inputValue("#priceProvider"), "h-123456789abc-shared-relay");
      await page.click("#priceCancel");
      await page.locator(".price-panel").getByRole("button", { name: "Close" }).click();
      assert.equal((await bodyText(page)).includes(UPSTREAM), false);
      assert.equal(
        await page.evaluate((s) => document.documentElement.outerHTML.includes(s), UPSTREAM),
        false,
        "the house provider's key is gone from the DOM"
      );
      assert.equal(
        await page.evaluate(() => [...document.querySelectorAll("input,textarea")].some((e) => e.value.includes("sk-upstream"))),
        false,
        "no field keeps the house provider's key"
      );
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: a member never sees or asks for administration`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, db, errors } = await open(browser, { user: USER });
    try {
      await page.locator("#keysList .row").first().waitFor();
      await page.locator("#housesList .row").first().waitFor();
      assert.equal(await page.locator("#adminPanel").isHidden(), true);
      assert.equal(await page.locator("#newChannel").isHidden(), true);
      assert.equal(db.calls.some((c) => c.path.startsWith("/api/cloud/admin/")), false);
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: empty, loading and failed reads each say what they are`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());

    // empty: three lists, each with its own line
    {
      const { context, page, errors } = await open(browser, {
        edit: (db) => {
          db.providers = [];
          db.houses = [];
          db.keys = [];
        }
      });
      try {
        await page.locator("#providersState .state-title").waitFor();
        assert.equal(await page.textContent("#providersState .state-title"), "No providers yet");
        assert.equal(await page.textContent("#housesState .state-title"), "No house channels yet");
        assert.equal(await page.textContent("#keysState .state-title"), "No API keys yet");
        assert.equal(await page.locator("#providersList .row").count(), 0);
        assert.equal(await page.locator("#housesList .row").count(), 0);
        assert.equal(await page.locator("#keysList .row").count(), 0);
        assert.deepEqual(errors, []);
      } finally {
        await context.close();
      }
    }

    // loading: the list's own spinner until the read answers
    {
      const { context, page, errors } = await open(browser, {
        edit: (db) => { db.delay.providers = 900; }
      });
      try {
        await page.locator("#providersState .spinner").waitFor();
        assert.equal(await page.textContent("#providersState"), "Loading…");
        await page.locator("#providersList .row").first().waitFor();
        assert.equal(await page.locator("#providersState").isHidden(), true);
        assert.deepEqual(errors, []);
      } finally {
        await context.close();
      }
    }

    // failed: a safe reason and a Retry that really reads again
    {
      const { context, page, db, errors } = await open(browser, {
        edit: (db) => { db.fail.providers = 500; }
      });
      try {
        await page.locator("#providersState.state.error").waitFor();
        assert.equal(await page.textContent("#providersState .state-text"), "The server is unavailable. Try again.");
        assert.equal(await page.locator("#providersState.state.empty").count(), 0, "a failed read is not an empty list");
        assert.equal((await bodyText(page)).includes("Internal Server Error"), false);
        assert.equal(await page.locator("#providersList .row").count(), 0);

        db.fail.providers = 0;
        await page.locator("#providersState").getByRole("button", { name: "Retry" }).click();
        await page.locator("#providersList .row").first().waitFor();
        assert.equal(await page.locator("#providersState").isHidden(), true);
        assert.deepEqual(errors, []);
      } finally {
        await context.close();
      }
    }

    // a body that is not a list is a failed read, never an empty one
    {
      const { context, page, errors } = await open(browser, {
        edit: (db) => { db.malformed.providers = true; }
      });
      try {
        await page.locator("#providersState.state.error").waitFor();
        assert.equal(await page.textContent("#providersState .state-text"), "Your providers could not be loaded.");
        assert.equal(await page.locator("#providersState.state.empty").count(), 0);
        assert.deepEqual(errors, []);
      } finally {
        await context.close();
      }
    }

    // house channels that could not be read are unknown, not none: the key
    // form says a key will allow all of them, and never sends the reader to
    // add one that may already be there
    {
      const { context, page, errors } = await open(browser, {
        edit: (db) => { db.fail.houses = 500; }
      });
      try {
        await page.locator("#housesState.state.error").waitFor();
        await page.click("#newKey");
        await page.locator("#keyForm").waitFor();
        const chips = await page.locator("#keyChannels").innerText();
        assert.match(chips, /could not be loaded/);
        assert.doesNotMatch(chips, /Add a house channel first/);
        assert.equal(await page.locator("#keyChannelsHint").isHidden(), true);
        assert.deepEqual(errors, []);
      } finally {
        await context.close();
      }
    }
  });

  test(`${engine}: the wallet shows its balance and a recharge is paid once`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, db, errors } = await open(browser, {
      edit: (db) => {
        db.wallet.paymentsEnabled = true;
        db.wallet.presets = [
          { id: "cny-10", amount_fen: 1000, label: "magpie recharge 10.00 CNY" },
          { id: "cny-50", amount_fen: 5000, label: "magpie recharge 50.00 CNY" }
        ];
      }
    });
    try {
      await page.locator("#walletBalance").waitFor();
      assert.match(await page.textContent("#walletAvailable"), /11\.84/);
      assert.match(await page.textContent("#walletHeld"), /0\.50/);
      assert.match(await page.textContent("#walletTotal"), /12\.34/);
      assert.equal(await page.locator("#walletUnavailable").isHidden(), true);
      assert.equal(await page.locator("#recharge").isVisible(), true);

      await page.click("#recharge");
      await page.locator("#rechargeForm").waitFor();
      assert.equal(await page.locator("#presets .chip").count(), 2);
      assert.match(await page.locator("#presets").innerText(), /10\.00/);
      assert.match(await page.locator("#presets").innerText(), /50\.00/);
      assert.equal(await page.locator('#presets .chip[aria-pressed="true"]').count(), 1);

      await page.click('#payProvider [data-provider="wechat"]');
      assert.equal(await page.getAttribute('#payProvider [data-provider="wechat"]', "aria-pressed"), "true");
      await page.click("#payGo");
      await page.locator("#payBox").waitFor();
      const created = calls(db, "POST", "/api/cloud/orders");
      assert.equal(created.length, 1);
      assert.deepEqual(created[0].body, { provider: "wechat", presetId: "cny-10" });
      assert.equal(await page.textContent("#orderStatus"), "Waiting for payment");
      assert.match(await page.textContent("#payCode"), /^weixin:\/\//);
      assert.equal(await page.locator("#wechatOpen").isVisible(), true);
      assert.match(await page.getAttribute("#wechatOpen", "href"), /^weixin:\/\//);
      assert.equal(await page.locator("#payOpen").isHidden(), true);
      assert.equal(await page.locator("#orderCheck").isVisible(), true);

      // the next read of the order says the money arrived and was credited
      db.advanceOrder = true;
      db.pollStatus = "success";
      db.pollCredit = "credited";
      await page.locator("#orderStatus").filter({ hasText: "Paid" }).waitFor({ timeout: 15000 });
      await page.locator("#orderNote").filter({ hasText: "Paid and added to your balance." }).waitFor();
      await page.locator("#walletAvailable").filter({ hasText: "21.84" }).waitFor();
      assert.match(await page.textContent("#notice"), /Recharge complete/);
      assert.equal(await page.locator("#orderCheck").isHidden(), true);
      assert.equal(await page.locator("#payCodeRow").isHidden(), true);
      assert.equal(await page.locator("#payOpen").isHidden(), true);

      await page.click("#orderClose");
      assert.equal(await page.locator("#payBox").isHidden(), true);
      assert.equal(await page.locator("#recharge").isVisible(), true);

      // Alipay's page payment is a real link to the provider
      await page.click("#recharge");
      await page.click('#payProvider [data-provider="alipay"]');
      await page.click("#payGo");
      await page.locator("#payBox").waitFor();
      assert.equal(await page.locator("#payOpen").isVisible(), true);
      assert.match(await page.getAttribute("#payOpen", "href"), /^https:\/\/pay\.example\/alipay\?order=/);
      assert.equal(await page.locator("#payCodeRow").isHidden(), true);
      assert.equal(await page.textContent("#orderStatus"), "Waiting for payment");
      await page.click("#orderClose");

      // a deployment without payments draws no button, only the line saying so
      db.wallet.paymentsEnabled = false;
      await page.reload();
      await page.locator("#walletBalance").waitFor();
      assert.equal(await page.locator("#recharge").isHidden(), true);
      assert.equal(await page.locator("#walletUnavailable").isVisible(), true);
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: wallet reads that fail and orders that cannot start say so safely`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, db, errors } = await open(browser, {
      edit: (db) => { db.malformed.wallet = true; }
    });
    try {
      await page.locator("#walletState.state.error").waitFor();
      assert.equal(await page.textContent("#walletState .state-text"), "Your balance could not be loaded.");
      assert.equal(await page.locator("#walletBalance").isHidden(), true);
      assert.equal(await page.locator("#recharge").isHidden(), true);

      db.malformed.wallet = false;
      await page.locator("#walletState").getByRole("button", { name: "Retry" }).click();
      await page.locator("#walletBalance").waitFor();
      assert.match(await page.textContent("#walletAvailable"), /11\.84/);
      // no payments on this deployment: the line, never a fake button
      assert.equal(await page.locator("#walletUnavailable").isVisible(), true);
      assert.equal(await page.locator("#recharge").isHidden(), true);

      // payments configured, but no amounts to pay
      db.wallet.paymentsEnabled = true;
      db.wallet.presets = [];
      await page.reload();
      await page.locator("#walletBalance").waitFor();
      assert.equal(await page.locator("#noPresets").isVisible(), true);
      assert.equal(await page.locator("#walletUnavailable").isHidden(), true);
      assert.equal(await page.locator("#recharge").isHidden(), true);

      // a payment method the deployment does not have
      db.wallet.presets = [{ id: "cny-10", amount_fen: 1000, label: "magpie recharge 10.00 CNY" }];
      db.orderReject = 400;
      await page.reload();
      await page.locator("#walletBalance").waitFor();
      await page.click("#recharge");
      await page.click("#payGo");
      await page.locator("#payError").filter({ hasText: "This payment method is not available right now." }).waitFor();
      assert.equal(await page.locator("#payBox").isHidden(), true);
      assert.equal((await page.locator("#payError").innerText()).includes("Bad Request"), false);

      // a refused payment ends the order, with no pay link left to press
      db.orderReject = 0;
      db.advanceOrder = true;
      db.pollStatus = "failed";
      db.pollCredit = "none";
      await page.click("#payCancel");
      await page.click("#recharge");
      await page.click("#payGo");
      await page.locator("#payBox").waitFor();
      await page.click("#orderCheck");
      await page.locator("#orderStatus").filter({ hasText: "Failed" }).waitFor({ timeout: 10000 });
      await page.locator("#orderNote").filter({ hasText: "The payment failed. You can start again." }).waitFor();
      assert.equal(await page.locator("#payOpen").isHidden(), true);
      assert.equal(await page.locator("#wechatOpen").isHidden(), true);
      assert.equal(await page.locator("#orderCheck").isHidden(), true);
      await page.click("#orderClose");

      // a provider failure leaves a pending order and says so without its words
      db.advanceOrder = false;
      db.pollStatus = "pending";
      db.providerError = "the provider said order 123 cannot be made";
      await page.click("#recharge");
      await page.click("#payGo");
      await page.locator("#payBox").waitFor();
      await page.locator("#notice").filter({ hasText: "The payment page could not be created. Check the order status or try again." }).waitFor();
      assert.equal((await page.textContent("#notice")).includes("provider said order"), false);
      assert.equal(await page.textContent("#orderStatus"), "Waiting for payment");
      await page.click("#orderClose");
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: an administrator manages a house channel's prices`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, db, errors } = await open(browser, {
      user: ADMIN,
      edit: (db) => {
        db.admin.push({
          id: RESEARCH, name: "Research", description: "", status: "draft",
          createdAt: "2026-11-02T00:00:00Z", updatedAt: "2026-11-02T00:00:00Z"
        });
      }
    });
    try {
      await page.locator("#adminList .row").first().waitFor();
      const team = () => page.locator("#adminList .row").filter({ hasText: "Team" });
      await team().getByRole("button", { name: "Prices" }).click();
      await page.locator(".price-panel").waitFor();
      await page.locator(".price-item").first().waitFor();
      const panel = () => page.locator(".price-panel");
      assert.match(await panel().innerText(), /Prices for Team/);
      assert.match(await panel().innerText(), /m1/);
      assert.match(await panel().innerText(), /2\.00/);
      assert.match(await panel().innerText(), /8\.00/);
      assert.match(await panel().innerText(), /0\.50/);
      assert.match(await panel().innerText(), /0\.001/);

      // an added price is sent as micro-CNY per million tokens
      await panel().getByRole("button", { name: "Add price" }).click();
      await page.fill("#priceProvider", TEAM_PROVIDER);
      await page.fill("#priceModel", "m2");
      await page.fill("#priceInput", "3");
      await page.fill("#priceOutput", "9.5");
      await page.fill("#priceCacheRead", "0.25");
      await page.fill("#priceCacheWrite", "0.25");
      await page.fill("#priceFixed", "0.002");
      await page.click("#priceSave");
      await page.locator("#notice").filter({ hasText: "Price saved." }).waitFor();
      const puts = db.calls.filter((c) => c.method === "PUT" && c.path === "/api/cloud/admin/house-channels/" + TEAM + "/prices");
      assert.equal(puts.length, 1);
      assert.deepEqual(puts[0].body, {
        provider: TEAM_PROVIDER,
        model: "m2",
        price: {
          input_micros_per_mtok: 3000000,
          output_micros_per_mtok: 9500000,
          cache_read_micros_per_mtok: 250000,
          cache_write_micros_per_mtok: 250000,
          fixed_per_call_micros: 2000
        }
      });
      const added = page.locator(".price-item").filter({ hasText: "m2" });
      await added.waitFor();
      assert.match(await added.innerText(), /3\.00/);
      assert.match(await added.innerText(), /9\.50/);
      assert.match(await added.innerText(), /0\.002/);

      // a bad amount is refused before any request
      await panel().getByRole("button", { name: "Add price" }).click();
      await page.fill("#priceProvider", TEAM_PROVIDER);
      await page.fill("#priceModel", "m3");
      await page.fill("#priceInput", "abc");
      await page.click("#priceSave");
      await page.locator("#priceError").filter({ hasText: "Enter a non-negative amount with at most 6 decimals." }).waitFor();
      assert.equal(db.calls.filter((c) => c.method === "PUT").length, 1);

      // a provider that is not on this channel is told as such
      await page.fill("#priceInput", "1");
      db.priceUnknown = true;
      await page.click("#priceSave");
      await page.locator("#priceError").filter({ hasText: "That provider is not on this channel." }).waitFor();
      db.priceUnknown = false;

      // another channel has no prices yet
      await panel().getByRole("button", { name: "Close" }).click();
      const research = page.locator("#adminList .row").filter({ hasText: "Research" });
      await research.getByRole("button", { name: "Prices" }).click();
      await page.locator(".price-panel .state-title").filter({ hasText: "No prices set yet" }).waitFor();
      assert.match(await page.locator(".price-panel .state-text").innerText(), /Add a price/);

      // a failed read is a failed read, with a Retry that reads again
      await page.locator(".price-panel").getByRole("button", { name: "Close" }).click();
      db.fail.prices = 500;
      await team().getByRole("button", { name: "Prices" }).click();
      await page.locator(".price-panel .state.error").waitFor();
      assert.equal(await page.textContent(".price-panel .state-text"), "The server is unavailable. Try again.");
      db.fail.prices = 0;
      await page.locator(".price-panel").getByRole("button", { name: "Retry" }).click();
      await page.locator(".price-item").first().waitFor();
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: usage, groups, provider deletion and account administration work end to end`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, db, errors } = await open(browser, {
      user: ADMIN,
      edit: (db) => {
        db.usage = [{ requestId: "request-1", reservationId: "reservation-1", channel: TEAM, provider: TEAM_PROVIDER, model: "m1", inputTokens: 12, outputTokens: 8, cacheReadTokens: 2, cacheWriteTokens: 1, costMicros: 345000, createdAt: "2026-11-26T10:00:00Z" }];
      }
    });
    try {
      await page.locator("#usageList .row").waitFor();
      assert.match(await page.locator("#usageList").innerText(), /m1/);
      assert.match(await page.locator("#usageList").innerText(), /0\.345/);

      await page.click("#newGroup");
      await page.fill("#gName", "My group");
      await page.fill("#gId", "mine");
      await page.fill("#gMembers", "u-123456789abc-my-relay/m1");
      await page.click("#gSave");
      await page.locator("#notice").filter({ hasText: "Routing group created." }).waitFor();
      await page.locator("#groupsList .row").waitFor();
      await page.locator("#groupsList .row .danger").click();
      await page.locator("#groupsList .row .danger").click();
      await page.locator("#groupsState .state-title").filter({ hasText: "No routing groups yet" }).waitFor();

      await page.locator("#providersList .row .danger").click();
      await page.locator("#providersList .row .danger").click();
      await page.locator("#providersState .state-title").filter({ hasText: "No providers yet" }).waitFor();

      await page.locator("#usersPanel").waitFor();
      await page.click("#usersList .row-actions .btn:not(.ghost)");
      await page.locator("#usersList .row-actions .btn:not(.ghost)").filter({ hasText: "Enable" }).waitFor();
      assert.equal(db.users[0].status, "disabled");
      await page.locator("#usersList .row-actions .ghost").click();
      await page.fill("#creditAmount", "2.50");
      await page.fill("#creditReason", "manual adjustment");
      await page.locator(".credit-panel .btn.primary").click();
      await page.locator("#notice").filter({ hasText: "Credit added." }).waitFor();
      const credit = calls(db, "POST", "/api/cloud/admin/wallet/credit").at(-1);
      assert.equal(credit.body.amountMicros, 2500000);
      assert.equal(credit.body.reason, "manual adjustment");
      assert.ok(credit.body.requestId.length >= 8);

      await page.locator("#ordersPanel").waitFor();
      await page.locator("#auditPanel").waitFor();
      assert.match(await page.locator("#auditList").innerText(), /wallet.credit/);
      await page.getByRole("button", { name: "Providers", exact: true }).click();
      await page.locator(".providers-panel .row .danger").click();
      await page.locator(".providers-panel .row .danger").click();
      await page.locator(".providers-panel .state-title").filter({ hasText: "No providers in this channel yet" }).waitFor();
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: an administrator edits the authentication policy at its read version`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, db, errors } = await open(browser, { user: ADMIN });
    try {
      await page.locator("#policyForm").waitFor();
      assert.equal(await page.textContent("#policyPanel h2"), "Authentication policy");
      assert.equal(await page.getAttribute("#policyRegistration", "aria-checked"), "true");
      assert.equal(await page.getAttribute("#policyPassword", "aria-checked"), "true");
      assert.equal(await page.getAttribute("#policyCode", "aria-checked"), "true");
      assert.equal(await page.inputValue("#policyMinLength"), "12");
      assert.equal(await page.textContent("#policyVersion"), "Version 3");
      assert.match(await page.textContent("#policyUsersNoPassword"), /Users without a password: 1/);
      assert.equal(await page.textContent("#policyMail"), "Email delivery is ready");

      // the form never offers to turn off the last way in
      await page.click("#policyCode");
      assert.equal(await page.getAttribute("#policyCode", "aria-checked"), "false");
      assert.match(await page.textContent("#policyWarning"), /1 users have no password/);
      await page.click("#policyPassword");
      await page.click("#policySave");
      await page.locator("#policyError").waitFor();
      assert.equal(await page.textContent("#policyError"), "Keep at least one sign-in method on.");
      assert.equal(calls(db, "PUT", "/api/cloud/admin/auth-policy").length, 0);

      await page.click("#policyPassword");
      await page.fill("#policyMinLength", "10");
      await page.click("#policySave");
      await page.locator("#notice").filter({ hasText: "Authentication policy saved." }).waitFor();
      const put = calls(db, "PUT", "/api/cloud/admin/auth-policy").pop();
      assert.deepEqual(put.body, {
        registrationEnabled: true,
        passwordLoginEnabled: true,
        codeLoginEnabled: false,
        passwordMinLength: 10,
        version: 3
      });
      assert.equal(await page.textContent("#policyVersion"), "Version 4");
      assert.equal(await page.inputValue("#policyMinLength"), "10");

      // a stale write says so and shows the policy as it now is
      db.policy.version = 7;
      await page.click("#policySave");
      await page.locator("#policyError").filter({ hasText: "Someone else changed the policy" }).waitFor();
      assert.equal(await page.textContent("#policyVersion"), "Version 7");
      assert.equal(await page.inputValue("#policyMinLength"), "10");
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });

  test(`${engine}: the dictionary is complete and the page fits a phone`, async (t) => {
    const browser = await launch(engine);
    t.after(() => browser.close());
    const { context, page, errors } = await open(browser, {
      user: ADMIN,
      viewport: { width: 390, height: 844 },
      edit: (db) => {
        db.wallet.paymentsEnabled = true;
        db.wallet.presets = [{ id: "cny-10", amount_fen: 1000, label: "magpie recharge 10.00 CNY" }];
      }
    });
    try {
      await page.locator("#adminList .row").first().waitFor();
      await settled(page);

      const report = await page.evaluate(() => {
        const c = window.magpieCloud;
        const problems = [];
        const base = Object.keys(c.dicts.en);
        const placeholders = (s) => [...String(s).matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort().join(",");
        if (c.langs.length !== 5) problems.push("expected five languages");
        for (const lang of c.langs) {
          const pack = c.dicts[lang];
          if (!pack) { problems.push(lang + ": no dictionary"); continue; }
          for (const key of base) {
            if (!(key in pack)) problems.push(lang + ": missing " + key);
            else if (!String(pack[key]).trim()) problems.push(lang + ": empty " + key);
            else if (placeholders(pack[key]) !== placeholders(c.dicts.en[key])) problems.push(lang + ": placeholders differ in " + key);
          }
          for (const key of Object.keys(pack)) {
            if (!(key in c.dicts.en)) problems.push(lang + ": extra " + key);
          }
        }
        const used = [...document.querySelectorAll("[data-t]")].map((n) => n.dataset.t);
        for (const key of used) if (!(key in c.dicts.en)) problems.push("unknown data-t key " + key);
        return { problems, used: used.length };
      });
      assert.deepEqual(report.problems, []);
      assert.ok(report.used > 40, "the page's static strings are looked up");

      assert.equal(await page.locator("select").count(), 0, "no native select");
      const noSideScroll = async (what) => assert.equal(
        await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth),
        true,
        "no sideways scroll at a phone's width: " + what
      );
      await noSideScroll("the portal");
      // the forms at a phone's width: fields, tabs and chips stay inside
      await page.click("#recharge");
      await page.locator("#rechargeForm").waitFor();
      await noSideScroll("the recharge form");
      await page.click("#payGo");
      await page.locator("#payBox").waitFor();
      await noSideScroll("the payment order");
      await page.click("#orderClose");
      await page.locator("#adminList .row").filter({ hasText: "Team" }).getByRole("button", { name: "Prices" }).click();
      await page.locator(".price-panel").waitFor();
      await noSideScroll("the price panel");
      await page.locator(".price-panel").getByRole("button", { name: "Add price" }).click();
      await page.locator("#priceProvider").waitFor();
      await noSideScroll("the price form");
      await page.click("#priceCancel");
      await page.locator(".price-panel").getByRole("button", { name: "Close" }).click();
      await page.click("#addProvider");
      await page.locator("#providerForm").waitFor();
      await noSideScroll("the provider form");
      await page.click("#pCancel");
      await page.click("#newKey");
      await page.locator("#keyForm").waitFor();
      await noSideScroll("the key form");
      await page.click("#keyCancel");
      await page.click("#newChannel");
      await page.locator("#channelForm").waitFor();
      await noSideScroll("the house channel form");
      await page.click("#chCancel");
      assert.equal(await page.locator("#langMenu").isHidden(), true);
      assert.equal(await page.locator("#langMenu button").count(), 5);
      assert.deepEqual(errors, []);
    } finally {
      await context.close();
    }
  });
}
