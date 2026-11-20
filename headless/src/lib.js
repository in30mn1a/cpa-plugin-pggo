"use strict";
// 纯函数集合：Cookie 文件选择/转换、指纹 → 浏览器上下文参数、用户脚本注入包装。
// 不依赖 Playwright，便于单元测试。注意：任何日志都不得输出 Cookie 值。

const fs = require("fs");
const path = require("path");

// 会话必需 8 件套（与用户脚本推送前的校验一致）
const REQUIRED_COOKIES = [
    "SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID",
    "__Secure-1PSID", "__Secure-3PSID", "SID", "HSID", "SSID",
];

// 与 hub.go sanitize() 一致：推送文件名 = sanitize(email) + ".json"
function sanitizeLabel(s) {
    const out = String(s || "").replace(/[^@.\-_0-9a-zA-Z]/g, "_");
    return out || "unknown";
}

function isGoogleDomain(domain) {
    const d = String(domain || "").replace(/^\./, "").toLowerCase();
    return d === "google.com" || d.endsWith(".google.com") ||
        d === "googlegroups.com" || d.endsWith(".googlegroups.com");
}

// 选择 Cookie 推送文件：显式文件 > 按 label/email 匹配 > 目录里最新的。
// 返回 { file, mtimeMs } 或 null。
function selectCookieFile({ dir, file, label }) {
    if (file) {
        const p = path.isAbsolute(file) ? file : path.join(dir, file);
        try {
            return { file: p, mtimeMs: fs.statSync(p).mtimeMs };
        } catch (_) {
            return null;
        }
    }
    let names;
    try {
        names = fs.readdirSync(dir).filter((n) => n.toLowerCase().endsWith(".json"));
    } catch (_) {
        return null;
    }
    const candidates = [];
    for (const n of names) {
        const p = path.join(dir, n);
        try {
            const st = fs.statSync(p);
            if (st.isFile()) candidates.push({ file: p, mtimeMs: st.mtimeMs });
        } catch (_) {}
    }
    if (label) {
        const want = String(label).toLowerCase();
        const byName = sanitizeLabel(label).toLowerCase() + ".json";
        return candidates.find((c) => path.basename(c.file).toLowerCase() === byName) ||
            candidates.find((c) => {
                try {
                    const email = JSON.parse(fs.readFileSync(c.file, "utf8")).email;
                    return typeof email === "string" && email.toLowerCase() === want;
                } catch (_) {
                    return false;
                }
            }) || null;
    }
    candidates.sort((a, b) => b.mtimeMs - a.mtimeMs);
    return candidates[0] || null;
}

// Chrome 扩展 / GM_cookie 的 sameSite → Playwright。
// no_restriction 必须配 secure，否则浏览器会拒收，降级为 Lax。
function mapSameSite(v, secure) {
    switch (String(v || "").toLowerCase()) {
        case "no_restriction":
        case "none":
            return secure ? "None" : "Lax";
        case "lax":
            return "Lax";
        case "strict":
            return "Strict";
        default:
            return undefined; // unspecified：交给浏览器默认（Lax-by-default）
    }
}

// 推送 payload 的 cookies → Playwright context.addCookies() 入参。
// 规则：domain 以 "." 开头 = 域 Cookie，否则 host-only（__Host- 前缀必须如此）；
// expirationDate（秒）≤0 视为会话 Cookie；已过期的直接丢弃。
function toPlaywrightCookies(list, nowSec = Date.now() / 1000) {
    const out = [];
    let dropped = 0;
    for (const c of Array.isArray(list) ? list : []) {
        if (!c || typeof c.name !== "string" || !c.name || typeof c.value !== "string" || !isGoogleDomain(c.domain)) {
            dropped++;
            continue;
        }
        const exp = typeof c.expirationDate === "number" && isFinite(c.expirationDate) ? c.expirationDate : -1;
        if (exp > 0 && exp < nowSec) {
            dropped++;
            continue;
        }
        const secure = !!c.secure || c.name.startsWith("__Secure-") || c.name.startsWith("__Host-");
        let domain = String(c.domain);
        if (c.name.startsWith("__Host-")) domain = domain.replace(/^\./, "");
        const pc = {
            name: c.name,
            value: c.value,
            domain,
            path: c.name.startsWith("__Host-") ? "/" : (c.path || "/"),
            expires: exp > 0 ? Math.floor(exp) : -1,
            httpOnly: !!c.httpOnly,
            secure,
        };
        const ss = mapSameSite(c.sameSite, secure);
        if (ss) pc.sameSite = ss;
        out.push(pc);
    }
    const names = new Set(out.map((c) => c.name));
    const missing = REQUIRED_COOKIES.filter((n) => !names.has(n));
    return { cookies: out, dropped, missing };
}

// 读取并解析推送文件。返回 { email, authuser, fingerprint, headers, cookies, dropped, missing }。
function loadCookiePush(file) {
    const raw = JSON.parse(fs.readFileSync(file, "utf8"));
    const conv = toPlaywrightCookies(raw.cookies);
    const au = String(raw.authuser == null ? "0" : raw.authuser);
    return {
        email: typeof raw.email === "string" ? raw.email : "",
        authuser: /^\d+$/.test(au) ? au : "0",
        fingerprint: raw.fingerprint && typeof raw.fingerprint === "object" ? raw.fingerprint : null,
        headers: raw.headers && typeof raw.headers === "object" ? raw.headers : null,
        ...conv,
    };
}

// 与脚本 accountTemporaryChatURL() 一致：authuser>0 时带 /u/N/ 前缀
function startUrl(authuser) {
    const n = Number(authuser) || 0;
    const prefix = n > 0 ? `/u/${n}/` : "/";
    return "https://aistudio.google.com" + prefix + "prompts/new_chat?temporary=true";
}

function sanitizeUA(ua) {
    return String(ua || "").replace(/HeadlessChrome/g, "Chrome").trim();
}

function num(v, lo, hi) {
    return typeof v === "number" && isFinite(v) && v >= lo && v <= hi ? v : null;
}

// 指纹 → Playwright newContext 选项（UA 由调用方传入已处理的值）。
function contextOptions(fp, { userAgent, defaultTimezone = "UTC", defaultLocale = "en-US" } = {}) {
    fp = fp || {};
    const screenW = num(fp.screenWidth, 320, 10000) || 1920;
    const screenH = num(fp.screenHeight, 240, 10000) || 1080;
    const innerW = num(fp.innerWidth, 200, screenW) || Math.min(screenW, 1920);
    const innerH = num(fp.innerHeight, 200, screenH) || Math.max(240, screenH - 133);
    const tz = typeof fp.timezone === "string" && /^[A-Za-z0-9_+\-/]{3,64}$/.test(fp.timezone) ? fp.timezone : defaultTimezone;
    const locale = typeof fp.language === "string" && /^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$/.test(fp.language) ? fp.language : defaultLocale;
    const opts = {
        viewport: { width: Math.round(innerW), height: Math.round(innerH) },
        screen: { width: Math.round(screenW), height: Math.round(screenH) },
        deviceScaleFactor: num(fp.devicePixelRatio, 0.5, 5) || 1,
        locale,
        timezoneId: tz,
        hasTouch: (num(fp.maxTouchPoints, 0, 64) || 0) > 0,
        // 在主世界注入脚本后 WS/Worker 受页面 CSP 约束；真实浏览器里脚本管理器的
        // 隔离世界不受页面 CSP 限制，这里用 bypassCSP 对齐（脚本自带 Trusted Types 兜底）
        bypassCSP: true,
    };
    if (userAgent) opts.userAgent = userAgent;
    return opts;
}

// 从 UA 推导 UA-CH 元数据（navigator.userAgentData / Sec-CH-UA*），避免
// 只覆盖 UA 字符串导致 userAgentData 与 UA 不一致。
function uaMetadata(ua, fp) {
    const m = /Chrome\/(\d+)(?:\.(\d+)\.(\d+)\.(\d+))?/.exec(ua || "");
    if (!m) return null;
    const major = m[1];
    const full = m[2] ? `${m[1]}.${m[2]}.${m[3]}.${m[4]}` : `${major}.0.0.0`;
    let platform = "Linux";
    let platformVersion = "";
    let navPlatform = "Linux x86_64";
    let mobile = false;
    if (/Windows NT/.test(ua)) {
        platform = "Windows";
        platformVersion = "10.0.0";
        navPlatform = "Win32";
    } else if (/Android/.test(ua)) {
        platform = "Android";
        mobile = /Mobile/.test(ua);
        navPlatform = "Linux armv8l";
    } else if (/Mac OS X ([\d_]+)/.test(ua)) {
        platform = "macOS";
        platformVersion = /Mac OS X ([\d_]+)/.exec(ua)[1].replace(/_/g, ".");
        navPlatform = "MacIntel";
    } else if (/CrOS/.test(ua)) {
        platform = "Chrome OS";
    }
    if (fp && typeof fp.platform === "string" && fp.platform) navPlatform = fp.platform;
    const brands = [
        { brand: "Google Chrome", version: major },
        { brand: "Chromium", version: major },
        { brand: "Not_A Brand", version: "24" },
    ];
    return {
        platform: navPlatform,
        userAgentMetadata: {
            brands,
            fullVersionList: brands.map((b) => ({ brand: b.brand, version: b.brand === "Not_A Brand" ? "24.0.0.0" : full })),
            fullVersion: full,
            platform,
            platformVersion,
            architecture: platform === "Android" ? "" : "x86",
            bitness: platform === "Android" ? "" : "64",
            model: "",
            mobile,
            wow64: false,
        },
    };
}

// 把 WS 地址补上 channel=headless（已有 channel 参数则保持）。
function normalizeWsUrl(raw) {
    const u = new URL(raw);
    if (!u.searchParams.has("channel")) u.searchParams.set("channel", "headless");
    return u.toString();
}

function isLoopbackHost(host) {
    const h = String(host || "").replace(/^\[|\]$/g, "").toLowerCase();
    return h === "localhost" || h === "::1" || /^127\./.test(h);
}

const WS_CONST_RE = /const\s+WS_URL\s*=\s*(["'`])[^"'`]*\1\s*;/;

// 生成 addInitScript 的完整源码：模拟 Tampermonkey 的 @noframes/@match/@grant 环境，
// 把脚本里的 WS_URL 常量改写成 worker 内部地址。GM_* 只在闭包作用域可见，不挂到 window。
//
// GM 存储：同步读写走页内 Map，以 seed 预置执行器 Key；不回写到 Node（页面整页跳转后
// 回落到 seed，丢失的只有可重建的请求头快照与面板记录）。
// GM.xmlHttpRequest：只被「检查更新 / Cookie 推送 / Robin 绑定」用到，无头场景不需要，
// 统一异步回调 onerror，不产生任何外部请求。
function buildInitScript(scriptSource, { wsUrl, apiKey }) {
    if (!WS_CONST_RE.test(scriptSource)) {
        throw new Error("用户脚本里找不到 `const WS_URL = \"...\";`，无法改写 WS 地址（脚本版本不兼容？）");
    }
    const patched = scriptSource.replace(WS_CONST_RE, () => `const WS_URL = ${JSON.stringify(wsUrl)};`);
    const seed = { pg_api_key: apiKey, pg_bootstrap_intercept: true };
    return `(() => {
if (window.top !== window) return; // @noframes
if (location.hostname !== "aistudio.google.com" || !/prompts/.test(location.pathname + location.search)) return; // @match
const __pggoStore = new Map(Object.entries(${JSON.stringify(seed)}));
const GM_getValue = (k, d) => (__pggoStore.has(k) ? __pggoStore.get(k) : d);
const GM_setValue = (k, v) => { __pggoStore.set(k, v); };
const GM_registerMenuCommand = () => 0;
const GM_openInTab = () => null;
const GM = {
  getValue: async (k, d) => GM_getValue(k, d),
  setValue: async (k, v) => GM_setValue(k, v),
  xmlHttpRequest: (o) => {
    setTimeout(() => { try { o && o.onerror && o.onerror({ error: "GM.xmlHttpRequest disabled in headless worker" }); } catch (_) {} }, 0);
    return { abort() {} };
  },
};
const unsafeWindow = window;
${patched}
})();`;
}

module.exports = {
    REQUIRED_COOKIES,
    sanitizeLabel,
    selectCookieFile,
    mapSameSite,
    toPlaywrightCookies,
    loadCookiePush,
    startUrl,
    sanitizeUA,
    contextOptions,
    uaMetadata,
    normalizeWsUrl,
    isLoopbackHost,
    buildInitScript,
};
