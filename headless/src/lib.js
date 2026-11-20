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
    if (!candidates[0]) return null;
    return { ...candidates[0], total: candidates.length };
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

// 保留推送 UA 的平台/OS 部分，但把 Chrome 版本换成实际 Chromium 的大版本
// （Chrome 的精简 UA 形如 Chrome/156.0.0.0）。否则 UA 宣称的版本与内核实际
// 支持的 Web API/行为不一致，本身就是可识别的特征。
function alignUAVersion(ua, browserVersion) {
    const major = String(browserVersion || "").split(".")[0];
    if (!/^\d+$/.test(major)) return ua;
    return String(ua).replace(/Chrome\/[\d.]+/, `Chrome/${major}.0.0.0`);
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
// fullVersion 传实际 Chromium 版本（如 156.0.8078.4），保证 UA-CH 与真实内核一致。
function uaMetadata(ua, fp, fullVersion) {
    const m = /Chrome\/(\d+)(?:\.(\d+)\.(\d+)\.(\d+))?/.exec(ua || "");
    if (!m) return null;
    const major = m[1];
    let full = m[2] ? `${m[1]}.${m[2]}.${m[3]}.${m[4]}` : `${major}.0.0.0`;
    if (fullVersion && String(fullVersion).split(".")[0] === major) full = String(fullVersion);
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
// 强制 channel=headless（覆盖任何其它 channel 值）：worker 绝不能注册到 page 通道，
// 否则会用 4001 顶掉真实浏览器。
function normalizeWsUrl(raw) {
    const u = new URL(raw);
    u.searchParams.delete("channel");
    u.searchParams.set("channel", "headless");
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

// ------------------------------------------------------------- 状态持久化 ----
// Google 会在运行中轮换 Cookie（__Secure-*PSIDTS 等），轮换后的值只在浏览器内存里。
// worker 定期把 Cookie 罐存到可写卷，重启时若状态文件派生自「同一份推送」就优先用它；
// 浏览器重新推送（推送文件 mtime 变化）后旧状态自动作废。

function stateFileFor(stateDir, push, pushFile) {
    const label = push.email || path.basename(pushFile, ".json");
    return path.join(stateDir, sanitizeLabel(label) + ".state.json");
}

// 返回可用的 Cookie 列表或 null。条件：同一账号、派生自同一份推送（sourceMtimeMs 相同）、
// 且保存时间晚于推送。
function loadState(file, push, pushMtimeMs, nowSec = Date.now() / 1000) {
    let st;
    try {
        st = JSON.parse(fs.readFileSync(file, "utf8"));
    } catch (_) {
        return null;
    }
    if (!st || !Array.isArray(st.cookies)) return null;
    if (String(st.email || "") !== String(push.email || "")) return null;
    if (st.sourceMtimeMs !== pushMtimeMs || !(st.savedAt > pushMtimeMs)) return null;
    const cookies = st.cookies.filter((c) => c && typeof c.name === "string" && typeof c.value === "string" &&
        isGoogleDomain(c.domain) && !(c.expires > 0 && c.expires < nowSec));
    return cookies.length ? cookies : null;
}

function saveState(file, { email, pushMtimeMs, cookies }) {
    const dir = path.dirname(file);
    fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
    const tmp = path.join(dir, `.${path.basename(file)}.tmp-${process.pid}`);
    const body = JSON.stringify({
        email: email || "",
        sourceMtimeMs: pushMtimeMs,
        savedAt: Date.now(),
        cookies: cookies.filter((c) => isGoogleDomain(c.domain)),
    });
    fs.writeFileSync(tmp, body, { mode: 0o600 });
    fs.renameSync(tmp, file);
}

// ------------------------------------------------------------------- 出口代理 ----
// Playwright 在 Chromium 上默认「强制代理环回地址」（往 bypass 里塞 <-loopback>），除非
// bypass 里显式列出某个环回主机。页面连的是 ws://127.0.0.1:<port>（worker 的环回转发），
// 若走代理会被转到代理机自己的 127.0.0.1 而失败，所以这里总是显式加上环回直连。
const LOOPBACK_BYPASS = ["127.0.0.1", "localhost", "[::1]"];
const PROXY_SCHEMES = { http: "http", https: "https", socks: "socks5", socks5: "socks5", socks5h: "socks5", socks4: "socks4" };

// proxyConfig 把 PGGO_PROXY / PGGO_PROXY_BYPASS 解析为 Playwright 的 proxy 选项；未设置返回 null。
// 凭据（http://user:pass@host:port）拆到 username/password，server 里不含凭据。
function proxyConfig(raw, bypassRaw) {
    raw = String(raw || "").trim();
    if (!raw) return null;
    let u;
    try {
        u = new URL(/^[a-z][a-z0-9+.-]*:\/\//i.test(raw) ? raw : `http://${raw}`);
    } catch (_) {
        throw new Error("PGGO_PROXY 不是合法的代理地址（例：http://singbox:2080 或 socks5://singbox:2080）");
    }
    const scheme = PROXY_SCHEMES[u.protocol.replace(/:$/, "").toLowerCase()];
    if (!scheme) throw new Error(`PGGO_PROXY 协议不支持: ${u.protocol}（支持 http / https / socks5 / socks4）`);
    if (!u.hostname) throw new Error("PGGO_PROXY 缺少主机名");
    const out = { server: `${scheme}://${u.host}` };
    if (u.username || u.password) {
        if (scheme !== "http" && scheme !== "https") {
            throw new Error("Chromium 不支持带认证的 SOCKS 代理：请改用 http://user:pass@host:port，或去掉代理入站的认证");
        }
        out.username = decodeURIComponent(u.username);
        out.password = decodeURIComponent(u.password);
    }
    const extra = String(bypassRaw || "").split(/[,;\s]+/).map((s) => s.trim()).filter(Boolean);
    out.bypass = [...new Set([...LOOPBACK_BYPASS, ...extra])].join(",");
    return out;
}

// describeProxy 用于日志：只含协议/主机/端口，不含凭据。
function describeProxy(proxy) {
    if (!proxy) return "直连（未设置 PGGO_PROXY）";
    return `${proxy.server}${proxy.username ? "（带认证）" : ""}`;
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
    alignUAVersion,
    stateFileFor,
    loadState,
    saveState,
    contextOptions,
    uaMetadata,
    normalizeWsUrl,
    isLoopbackHost,
    buildInitScript,
    proxyConfig,
    describeProxy,
    LOOPBACK_BYPASS,
};
