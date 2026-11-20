"use strict";
const test = require("node:test");
const assert = require("node:assert");
const fs = require("fs");
const os = require("os");
const path = require("path");
const vm = require("vm");
const lib = require("../src/lib");

const NOW = 1_800_000_000;

test("toPlaywrightCookies 转换属性", () => {
    const { cookies, dropped, missing } = lib.toPlaywrightCookies([
        { name: "SAPISID", value: "v1", domain: ".google.com", path: "/", expirationDate: NOW + 100.7, httpOnly: false, secure: true, sameSite: "unspecified" },
        { name: "__Secure-1PSID", value: "v2", domain: ".google.com", path: "/", expirationDate: NOW + 100, httpOnly: true, secure: true, sameSite: "no_restriction" },
        { name: "__Host-GAPS", value: "v3", domain: ".accounts.google.com", path: "/x", expirationDate: -1, httpOnly: true, secure: true, sameSite: "strict" },
        { name: "NID", value: "v4", domain: "aistudio.google.com", path: "/", expirationDate: NOW + 5, secure: false, sameSite: "no_restriction" },
        { name: "OLD", value: "x", domain: ".google.com", expirationDate: NOW - 1 },
        { name: "EVIL", value: "x", domain: ".example.com" },
        { name: "", value: "x", domain: ".google.com" },
    ], NOW);
    assert.strictEqual(dropped, 3);
    const by = Object.fromEntries(cookies.map((c) => [c.name, c]));
    assert.deepStrictEqual(by.SAPISID, { name: "SAPISID", value: "v1", domain: ".google.com", path: "/", expires: NOW + 100, httpOnly: false, secure: true });
    assert.strictEqual(by["__Secure-1PSID"].sameSite, "None");
    assert.strictEqual(by["__Secure-1PSID"].httpOnly, true);
    // __Host- 必须 host-only + path=/
    assert.strictEqual(by["__Host-GAPS"].domain, "accounts.google.com");
    assert.strictEqual(by["__Host-GAPS"].path, "/");
    assert.strictEqual(by["__Host-GAPS"].expires, -1);
    assert.strictEqual(by["__Host-GAPS"].sameSite, "Strict");
    // 非 secure 的 no_restriction 降级为 Lax；host-only 保持无点域名
    assert.strictEqual(by.NID.sameSite, "Lax");
    assert.strictEqual(by.NID.domain, "aistudio.google.com");
    assert.ok(missing.includes("SID") && !missing.includes("SAPISID"));
});

test("selectCookieFile：最新 / 显式文件 / label", () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pggo-ck-"));
    const a = path.join(dir, "a@gmail.com.json");
    const b = path.join(dir, "b_x@gmail.com.json");
    fs.writeFileSync(a, JSON.stringify({ email: "a@gmail.com", cookies: [] }));
    fs.writeFileSync(b, JSON.stringify({ email: "B+x@gmail.com", cookies: [] }));
    fs.writeFileSync(path.join(dir, "note.txt"), "x");
    fs.utimesSync(a, new Date(1000e3), new Date(1000e3));
    fs.utimesSync(b, new Date(2000e3), new Date(2000e3));
    assert.strictEqual(lib.selectCookieFile({ dir }).file, b);
    assert.strictEqual(lib.selectCookieFile({ dir, file: "a@gmail.com.json" }).file, a);
    assert.strictEqual(lib.selectCookieFile({ dir, label: "a@gmail.com" }).file, a);
    // label 按文件名 sanitize 匹配（+ → _），或按 payload email 不区分大小写匹配
    assert.strictEqual(lib.selectCookieFile({ dir, label: "b+x@gmail.com" }).file, b);
    assert.strictEqual(lib.selectCookieFile({ dir, label: "nobody@gmail.com" }), null);
    assert.strictEqual(lib.selectCookieFile({ dir: path.join(dir, "missing") }), null);
});

test("loadCookiePush 解析 authuser/fingerprint", () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pggo-ck-"));
    const f = path.join(dir, "x.json");
    fs.writeFileSync(f, JSON.stringify({ email: "x@gmail.com", authuser: "2", fingerprint: { timezone: "Asia/Tokyo" }, cookies: [{ name: "SID", value: "s", domain: ".google.com" }] }));
    const p = lib.loadCookiePush(f);
    assert.strictEqual(p.authuser, "2");
    assert.strictEqual(p.cookies.length, 1);
    assert.strictEqual(lib.startUrl(p.authuser), "https://aistudio.google.com/u/2/prompts/new_chat?temporary=true");
    assert.strictEqual(lib.startUrl("0"), "https://aistudio.google.com/prompts/new_chat?temporary=true");
});

test("contextOptions 按指纹设置视口/屏幕/时区/语言", () => {
    const o = lib.contextOptions({
        screenWidth: 2560, screenHeight: 1440, innerWidth: 1280, innerHeight: 720,
        devicePixelRatio: 1.5, timezone: "America/New_York", language: "zh-CN", maxTouchPoints: 0,
    }, { userAgent: "UA" });
    assert.deepStrictEqual(o.viewport, { width: 1280, height: 720 });
    assert.deepStrictEqual(o.screen, { width: 2560, height: 1440 });
    assert.strictEqual(o.deviceScaleFactor, 1.5);
    assert.strictEqual(o.timezoneId, "America/New_York");
    assert.strictEqual(o.locale, "zh-CN");
    assert.strictEqual(o.userAgent, "UA");
    assert.strictEqual(o.hasTouch, false);
    const d = lib.contextOptions(null, { defaultTimezone: "UTC" });
    assert.strictEqual(d.timezoneId, "UTC");
    assert.ok(d.viewport.width > 0 && d.screen.width >= d.viewport.width);
    assert.strictEqual(lib.contextOptions({ timezone: "bad zone;" }).timezoneId, "UTC");
});

test("UA 去 HeadlessChrome 与 UA-CH 元数据", () => {
    assert.strictEqual(lib.sanitizeUA("Mozilla/5.0 HeadlessChrome/140.0.0.0 Safari"), "Mozilla/5.0 Chrome/140.0.0.0 Safari");
    const win = lib.uaMetadata("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.7390.55 Safari/537.36", null);
    assert.strictEqual(win.platform, "Win32");
    assert.strictEqual(win.userAgentMetadata.platform, "Windows");
    assert.strictEqual(win.userAgentMetadata.fullVersion, "141.0.7390.55");
    assert.ok(win.userAgentMetadata.brands.some((b) => b.brand === "Google Chrome" && b.version === "141"));
    const mac = lib.uaMetadata("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/140.0.0.0", { platform: "MacIntel" });
    assert.strictEqual(mac.userAgentMetadata.platform, "macOS");
    assert.strictEqual(mac.userAgentMetadata.platformVersion, "10.15.7");
    assert.strictEqual(lib.uaMetadata("Firefox/1", null), null);
});

test("normalizeWsUrl 补 channel=headless", () => {
    assert.strictEqual(lib.normalizeWsUrl("ws://cliproxy:8788/api/pg/ws"), "ws://cliproxy:8788/api/pg/ws?channel=headless");
    assert.strictEqual(lib.normalizeWsUrl("ws://h/api/pg/ws?channel=headless"), "ws://h/api/pg/ws?channel=headless");
    assert.ok(lib.isLoopbackHost("127.0.0.1") && lib.isLoopbackHost("localhost") && lib.isLoopbackHost("[::1]"));
    assert.ok(!lib.isLoopbackHost("cliproxy"));
});

// 用最小假脚本跑注入包装：验证 @noframes/@match 守卫、GM shim、WS_URL 改写。
const FAKE_SCRIPT = `// ==UserScript==
// @grant GM_getValue
// ==/UserScript==
(function () {
    "use strict";
    const W = typeof unsafeWindow !== "undefined" ? unsafeWindow : window;
    const WS_URL = "wss://example.invalid/api/pg/ws";
    W.__result = {
        ws: WS_URL,
        key: GM_getValue("pg_api_key", ""),
        intercept: GM_getValue("pg_bootstrap_intercept", false),
        missing: GM_getValue("nope", "def"),
        hasXhr: typeof GM_xmlhttpRequest === "function" || (typeof GM !== "undefined" && typeof GM.xmlHttpRequest === "function"),
        hasCookie: typeof GM_cookie !== "undefined",
        sameWindow: W === window,
    };
    GM_setValue("k", 1);
    W.__result.k = GM_getValue("k");
    GM.xmlHttpRequest({ onerror: (e) => { W.__xhrErr = e.error; } });
})();`;

function runIn(href, top) {
    const u = new URL(href);
    const win = {};
    win.window = win;
    win.top = top === "self" ? win : {};
    win.location = { hostname: u.hostname, pathname: u.pathname, search: u.search };
    win.setTimeout = (fn) => fn();
    const ctx = vm.createContext(win);
    const src = lib.buildInitScript(FAKE_SCRIPT, { wsUrl: "ws://127.0.0.1:1234/api/pg/ws?channel=headless", apiKey: "cat-key" });
    vm.runInContext(src, ctx);
    return win;
}

test("buildInitScript：shim 与 WS 改写", () => {
    const w = runIn("https://aistudio.google.com/prompts/new_chat?temporary=true", "self");
    assert.deepStrictEqual({ ...w.__result }, {
        ws: "ws://127.0.0.1:1234/api/pg/ws?channel=headless",
        key: "cat-key",
        intercept: true,
        missing: "def",
        hasXhr: true,
        hasCookie: false,
        sameWindow: true,
        k: 1,
    });
    assert.match(w.__xhrErr, /disabled/);
    // GM_* 不泄漏到 window
    assert.strictEqual(w.GM_getValue, undefined);
});

test("buildInitScript：子帧与非匹配页不执行", () => {
    assert.strictEqual(runIn("https://aistudio.google.com/prompts/new_chat", "other").__result, undefined);
    assert.strictEqual(runIn("https://accounts.google.com/v3/signin/x", "self").__result, undefined);
    assert.strictEqual(runIn("https://aistudio.google.com/apikey", "self").__result, undefined);
});

test("buildInitScript：真实用户脚本可改写并通过语法检查", () => {
    const candidates = [
        process.env.PGGO_TEST_USERSCRIPT,
        path.join(__dirname, "..", "..", "..", "selfhost", "dist", "Catiecli Playground Executor.user.js"),
    ].filter(Boolean);
    const real = candidates.find((p) => fs.existsSync(p));
    if (!real) return; // 本地没有脚本副本时跳过
    const src = lib.buildInitScript(fs.readFileSync(real, "utf8"), { wsUrl: "ws://127.0.0.1:9/api/pg/ws?channel=headless", apiKey: "k" });
    assert.ok(src.includes('const WS_URL = "ws://127.0.0.1:9/api/pg/ws?channel=headless";'));
    assert.ok(!/const WS_URL = "wss:/.test(src));
    new vm.Script(src); // 语法检查
    assert.throws(() => lib.buildInitScript("no ws here", { wsUrl: "ws://x", apiKey: "k" }), /WS_URL/);
});

test("normalizeWsUrl 强制 channel=headless", () => {
    assert.strictEqual(lib.normalizeWsUrl("ws://h/api/pg/ws?channel=page"), "ws://h/api/pg/ws?channel=headless");
    assert.strictEqual(lib.normalizeWsUrl("ws://h/api/pg/ws?channel=page&channel=x&a=1"), "ws://h/api/pg/ws?a=1&channel=headless");
});

test("alignUAVersion 保留平台、对齐实际 Chromium 大版本；UA-CH 用实际完整版本", () => {
    const pushed = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.7390.55 Safari/537.36";
    const ua = lib.alignUAVersion(pushed, "156.0.8078.4");
    assert.strictEqual(ua, "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36");
    const meta = lib.uaMetadata(ua, null, "156.0.8078.4");
    assert.strictEqual(meta.userAgentMetadata.platform, "Windows");
    assert.strictEqual(meta.userAgentMetadata.fullVersion, "156.0.8078.4");
    assert.ok(meta.userAgentMetadata.brands.every((b) => b.brand === "Not_A Brand" || b.version === "156"));
    assert.strictEqual(lib.alignUAVersion(pushed, ""), pushed);
});

test("selectCookieFile 返回目录内文件总数（用于多账号告警）", () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pggo-ck-"));
    fs.writeFileSync(path.join(dir, "a.json"), "{}");
    fs.writeFileSync(path.join(dir, "b.json"), "{}");
    fs.writeFileSync(path.join(dir, ".b.json.tmp-1"), "{}"); // 中枢原子写的临时文件不计入
    assert.strictEqual(lib.selectCookieFile({ dir }).total, 2);
});

test("状态文件：同账号且派生自同一份推送才使用；新推送使其作废", () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pggo-st-"));
    const push = { email: "a@gmail.com" };
    const file = lib.stateFileFor(dir, push, "/x/a@gmail.com.json");
    assert.strictEqual(path.basename(file), "a@gmail.com.state.json");
    const pushMtimeMs = Date.now() - 5000;
    lib.saveState(file, {
        email: "a@gmail.com",
        pushMtimeMs,
        cookies: [
            { name: "__Secure-1PSIDTS", value: "rotated", domain: ".google.com", path: "/", expires: -1, httpOnly: true, secure: true, sameSite: "None" },
            { name: "X", value: "y", domain: ".example.com", path: "/", expires: -1 },
        ],
    });
    const got = lib.loadState(file, push, pushMtimeMs);
    assert.strictEqual(got.length, 1);
    assert.strictEqual(got[0].value, "rotated");
    assert.strictEqual(lib.loadState(file, push, pushMtimeMs + 1), null, "新推送（mtime 变化）应使状态作废");
    assert.strictEqual(lib.loadState(file, { email: "b@gmail.com" }, pushMtimeMs), null, "不同账号不能用");
    assert.strictEqual(lib.loadState(path.join(dir, "missing.json"), push, pushMtimeMs), null);
    assert.deepStrictEqual(fs.readdirSync(dir), ["a@gmail.com.state.json"], "不应残留临时文件");
});

// Playwright 的 shouldProxyLoopback：bypass 中不含任何环回主机时会强制代理环回（追加 <-loopback>）
function playwrightForcesLoopbackProxy(bypass) {
    const hosts = (bypass || "").split(",").map((s) => s.trim());
    return !["localhost", "127.0.0.1", "::1", "[::]", "[::1]", "<loopback>", "<-loopback>"].some((h) => hosts.includes(h));
}

test("proxyConfig：未设置时直连", () => {
    assert.strictEqual(lib.proxyConfig("", "x"), null);
    assert.strictEqual(lib.proxyConfig(undefined), null);
    assert.match(lib.describeProxy(null), /直连/);
});

test("proxyConfig：http / socks5 / 无协议，环回总是直连且排在前面", () => {
    for (const [raw, server] of [
        ["http://singbox:2080", "http://singbox:2080"],
        ["socks5://singbox:2080", "socks5://singbox:2080"],
        ["socks5h://singbox:2080", "socks5://singbox:2080"],
        ["singbox:2080", "http://singbox:2080"],
        ["socks5://[::1]:1080", "socks5://[::1]:1080"],
    ]) {
        const p = lib.proxyConfig(raw);
        assert.strictEqual(p.server, server, raw);
        assert.strictEqual(p.bypass, "127.0.0.1,localhost,[::1]", raw);
        assert.strictEqual(p.username, undefined);
        assert.strictEqual(playwrightForcesLoopbackProxy(p.bypass), false, "环回不能被 Playwright 强制走代理");
    }
});

test("proxyConfig：PGGO_PROXY_BYPASS 合并去重，支持逗号/分号/空白分隔", () => {
    const p = lib.proxyConfig("http://singbox:2080", " cliproxy; .internal  10.0.0.0/8,localhost,cliproxy ");
    assert.strictEqual(p.bypass, "127.0.0.1,localhost,[::1],cliproxy,.internal,10.0.0.0/8");
});

test("proxyConfig：凭据拆出，server 与日志不含凭据；SOCKS 认证与未知协议报错", () => {
    const p = lib.proxyConfig("http://us%40er:p%3Ass@proxy.local:3128");
    assert.deepStrictEqual(
        { server: p.server, username: p.username, password: p.password },
        { server: "http://proxy.local:3128", username: "us@er", password: "p:ss" },
    );
    const d = lib.describeProxy(p);
    assert.ok(!d.includes("p:ss") && !d.includes("us@er") && d.includes("proxy.local:3128"), d);
    assert.throws(() => lib.proxyConfig("socks5://u:p@singbox:2080"), /SOCKS/);
    assert.throws(() => lib.proxyConfig("ftp://singbox:21"), /协议不支持/);
    assert.throws(() => lib.proxyConfig("http://"), /PGGO_PROXY/);
});

test("readEnv 读取 PGGO_PROXY / PGGO_PROXY_BYPASS / PGGO_PROXY_CHECK", () => {
    const { readEnv } = require("../src/worker");
    const base = { PGGO_API_KEY: "k" };
    assert.strictEqual(readEnv(base).proxy, null);
    const cfg = readEnv({ ...base, PGGO_PROXY: "socks5://singbox:2080", PGGO_PROXY_BYPASS: "cliproxy" });
    assert.deepStrictEqual(cfg.proxy, { server: "socks5://singbox:2080", bypass: "127.0.0.1,localhost,[::1],cliproxy" });
    assert.strictEqual(cfg.proxyCheck, true);
    assert.strictEqual(readEnv({ ...base, PGGO_PROXY: "http://h:1", PGGO_PROXY_CHECK: "0" }).proxyCheck, false);
    assert.throws(() => readEnv({ ...base, PGGO_PROXY: "gopher://h:1" }), /协议不支持/);
});

test("proxySelfCheck 经代理请求 ipinfo，失败不抛出", async () => {
    const { proxySelfCheck } = require("../src/worker");
    const seen = [];
    const fake = {
        async newContext(opts) {
            seen.push(opts.proxy);
            return {
                async get(url) {
                    seen.push(url);
                    return { ok: () => true, status: () => 200, json: async () => ({ ip: "203.0.113.9", country: "JP", org: "AS0 Test" }) };
                },
                async dispose() { seen.push("disposed"); },
            };
        },
    };
    const proxy = lib.proxyConfig("http://singbox:2080");
    await proxySelfCheck(proxy, fake);
    assert.deepStrictEqual(seen, [proxy, "https://ipinfo.io/json", "disposed"]);
    await proxySelfCheck(proxy, { async newContext() { throw new Error("boom"); } });
});
