"use strict";
// pggo 无头执行器 worker：用推送的 Google Cookie 在 Chromium 里打开 AI Studio，
// 注入 Catiecli Playground Executor 用户脚本（GM 环境由 shim 提供），让脚本以
// channel=headless 连上 pggo 中枢，像真实浏览器一样执行请求。
//
// 日志约定：一行一条简短状态；绝不输出 Cookie 值 / 执行器 Key。

const fs = require("fs");
const net = require("net");
const path = require("path");
const { spawn } = require("child_process");
const lib = require("./lib");

function log(msg) {
    process.stdout.write(`${new Date().toISOString()} [pggo-headless] ${msg}\n`);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function envInt(name, def) {
    const v = parseInt(process.env[name] || "", 10);
    return Number.isFinite(v) && v >= 0 ? v : def;
}

function readEnv(env = process.env) {
    const mode = String(env.PGGO_HEADLESS_MODE || "xvfb").toLowerCase();
    if (!["xvfb", "new"].includes(mode)) throw new Error(`PGGO_HEADLESS_MODE 只能是 xvfb 或 new，得到 ${mode}`);
    const apiKey = String(env.PGGO_API_KEY || "").trim();
    if (!apiKey) throw new Error("必须设置 PGGO_API_KEY（与插件 api-keys 中的执行器 Key 一致）");
    return {
        wsUrl: lib.normalizeWsUrl(env.PGGO_WS_URL || "ws://cliproxy:8788/api/pg/ws?channel=headless"),
        apiKey,
        cookieDir: env.PGGO_COOKIE_DIR || "/data/cookies",
        cookieFile: env.PGGO_COOKIE_FILE || "",
        cookieLabel: env.PGGO_COOKIE_LABEL || "",
        scriptPath: env.PGGO_SCRIPT_PATH || "/app/userscript.user.js",
        mode,
        startUrl: env.PGGO_START_URL || "",
        defaultTimezone: env.PGGO_DEFAULT_TIMEZONE || "UTC",
        registerTimeoutMs: envInt("PGGO_REGISTER_TIMEOUT_SEC", 180) * 1000,
        maxReloads: envInt("PGGO_MAX_RELOADS", 3),
        recycleMs: envInt("PGGO_RECYCLE_HOURS", 6) * 3600 * 1000,
        pageLog: env.PGGO_PAGE_LOG !== "0",
        cookiePollMs: envInt("PGGO_COOKIE_POLL_SEC", 10) * 1000,
        stateDir: env.PGGO_STATE_DIR === undefined ? "/state" : env.PGGO_STATE_DIR,
        stateSaveMs: envInt("PGGO_STATE_SAVE_MIN", 10) * 60 * 1000,
        drainMs: envInt("PGGO_DRAIN_SEC", 60) * 1000,
        proxy: lib.proxyConfig(env.PGGO_PROXY, env.PGGO_PROXY_BYPASS),
        proxyCheck: env.PGGO_PROXY_CHECK !== "0",
    };
}

// ------------------------------------------------------------------ 出口自检 ----
// 经同一代理从 Node 侧取 ipinfo.io，记录出口国家/组织，便于确认代理生效。失败只记日志。
async function proxySelfCheck(proxy, request) {
    let ctx;
    try {
        request = request || require("playwright").request;
        ctx = await request.newContext({ proxy, timeout: 15000 });
        const resp = await ctx.get("https://ipinfo.io/json", { timeout: 15000 });
        if (!resp.ok()) {
            log(`出口自检：ipinfo.io 返回 HTTP ${resp.status()}（不影响运行）`);
            return;
        }
        const info = await resp.json();
        log(`出口自检：${info.ip || "?"} ${info.country || "?"} ${info.org || ""}`.trim());
    } catch (e) {
        log("出口自检失败（不影响运行）: " + String(e && e.message || e).split("\n")[0]);
    } finally {
        if (ctx) await ctx.dispose().catch(() => {});
    }
}

// ------------------------------------------------------------------ 环回转发 ----
// https 页面里连明文 ws://cliproxy:8788 会被当作混合内容拦截；环回地址
// （127.0.0.1）属于「潜在可信」源不受此限。所以对非环回的 ws:// 目标，在
// worker 进程里开一个 127.0.0.1 随机端口做 TCP 透传，页面只连本机。
async function startForwarder(target) {
    const u = new URL(target);
    if (u.protocol !== "ws:" || lib.isLoopbackHost(u.hostname)) {
        return { pageUrl: target, close: async () => {} };
    }
    const host = u.hostname;
    const port = Number(u.port || 80);
    const sockets = new Set();
    const server = net.createServer((client) => {
        const upstream = net.connect(port, host);
        sockets.add(client);
        sockets.add(upstream);
        const done = () => {
            client.destroy();
            upstream.destroy();
            sockets.delete(client);
            sockets.delete(upstream);
        };
        client.on("error", done).on("close", done);
        upstream.on("error", done).on("close", done);
        client.pipe(upstream);
        upstream.pipe(client);
    });
    await new Promise((resolve, reject) => {
        server.once("error", reject);
        server.listen(0, "127.0.0.1", resolve);
    });
    const local = new URL(target);
    local.hostname = "127.0.0.1";
    local.port = String(server.address().port);
    log(`WS 环回转发 127.0.0.1:${local.port} → ${host}:${port}`);
    return {
        pageUrl: local.toString(),
        close: async () => {
            for (const s of sockets) s.destroy();
            await new Promise((r) => server.close(() => r()));
        },
    };
}

// --------------------------------------------------------------------- Xvfb ----
async function startXvfb(width, height) {
    const display = ":" + (90 + Math.floor(Math.random() * 9));
    const num = display.slice(1);
    const sock = `/tmp/.X11-unix/X${num}`;
    try { fs.rmSync(sock, { force: true }); } catch (_) {}
    try { fs.rmSync(`/tmp/.X${num}-lock`, { force: true }); } catch (_) {}
    const proc = spawn("Xvfb", [display, "-screen", "0", `${width}x${height}x24`, "-nolisten", "tcp", "-ac"], {
        stdio: ["ignore", "ignore", "pipe"],
    });
    let stderr = "";
    proc.stderr.on("data", (d) => { stderr = (stderr + d).slice(-500); });
    for (let i = 0; i < 50; i++) {
        if (fs.existsSync(sock)) break;
        if (proc.exitCode !== null) throw new Error("Xvfb 启动失败: " + stderr.trim());
        await sleep(100);
    }
    if (!fs.existsSync(sock)) {
        proc.kill("SIGKILL");
        throw new Error("Xvfb 5s 内未就绪");
    }
    return {
        display,
        stop: () => { try { proc.kill("SIGTERM"); } catch (_) {} },
    };
}

// ------------------------------------------------------------------- 浏览器 ----
// Playwright 已默认带上 --disable-dev-shm-usage / --no-first-run / --disable-renderer-backgrounding /
// --disable-backgrounding-occluded-windows 等；这里只补充额外项。注意不要再传 --disable-features：
// Chromium 对重复开关取最后一个，会冲掉 Playwright 自带的 disable-features 列表。
function chromiumArgs(opts, mode) {
    const args = [
        "--disable-blink-features=AutomationControlled",
        "--renderer-process-limit=2",
        "--mute-audio",
        `--lang=${opts.locale}`,
    ];
    if (mode === "xvfb") {
        args.push("--window-position=0,0", `--window-size=${opts.screen.width},${opts.screen.height}`);
    }
    return args;
}

async function launchBrowser(chromium, ctxOpts, mode, display) {
    const launchOpts = {
        headless: mode !== "xvfb",
        args: chromiumArgs(ctxOpts, mode),
        // 防御性：若某版本 Playwright 默认带 --enable-automation，则去掉（会令 navigator.webdriver=true）
        ignoreDefaultArgs: ["--enable-automation"],
        env: { ...process.env, ...(display ? { DISPLAY: display } : {}) },
    };
    // Playwright 里 channel=chromium 才是「新 headless」（完整浏览器内核）；
    // 不设则用精简的 chromium-headless-shell（特征明显，本镜像不安装）。
    if (mode === "new") launchOpts.channel = "chromium";
    return chromium.launch(launchOpts);
}

async function nativeUserAgent(browser) {
    try {
        const cdp = await browser.newBrowserCDPSession();
        const v = await cdp.send("Browser.getVersion");
        await cdp.detach().catch(() => {});
        return v.userAgent || "";
    } catch (_) {
        return "";
    }
}

// 页面级 CDP 覆盖：UA-CH 元数据（让 userAgentData 与 UA 字符串一致）+ CPU 核数。
async function applyPageOverrides(page, ua, fp, locale, fullVersion) {
    let cdp;
    try {
        cdp = await page.context().newCDPSession(page);
    } catch (e) {
        log("CDP 会话创建失败（跳过 UA-CH 覆盖）: " + e.message);
        return;
    }
    const meta = lib.uaMetadata(ua, fp, fullVersion);
    if (meta) {
        try {
            await cdp.send("Emulation.setUserAgentOverride", {
                userAgent: ua,
                acceptLanguage: locale,
                platform: meta.platform,
                userAgentMetadata: meta.userAgentMetadata,
            });
        } catch (e) {
            log("UA-CH 覆盖失败: " + e.message);
        }
    }
    const hc = fp && Number(fp.hardwareConcurrency);
    if (hc && hc >= 1 && hc <= 256) {
        try {
            await cdp.send("Emulation.setHardwareConcurrencyOverride", { hardwareConcurrency: Math.floor(hc) });
        } catch (_) {}
    }
}

// --------------------------------------------------------------------- 会话 ----
// 一个会话 = 一次浏览器启动 + 一份 Cookie。返回时调用方按需重启（Cookie 变更 /
// 浏览器崩溃 / 重载多次仍未注册）。
class Session {
    constructor(cfg, chromium, wsPageUrl, extra = {}) {
        this.cfg = cfg;
        this.chromium = chromium;
        this.wsPageUrl = wsPageUrl;
        this.extra = extra; // 测试钩子：{ setupContext(context) }
        this.registered = false;
        this.everRegistered = false;
        this.lastCloseCode = 0;
        this.inflight = new Set();
        this.crashed = false;
        this.stopped = false;
        this.loginRedirect = ""; // 非空 = 被重定向到的主机（Cookie 失效）
    }

    async run(selected, isStopping) {
        const cfg = this.cfg;
        const push = lib.loadCookiePush(selected.file);
        const tag = push.email || path.basename(selected.file);
        this.push = push;
        this.pushMtimeMs = selected.mtimeMs;
        log(`Cookie: ${tag}（${push.cookies.length} 条，丢弃 ${push.dropped}，authuser=${push.authuser}）`);
        if (push.missing.length) log(`⚠ 缺少会话必需 Cookie: ${push.missing.join(", ")}（Google 可能判 401）`);
        if (!push.cookies.length) throw new Error("推送文件里没有可用 Cookie");
        let cookies = push.cookies;
        if (cfg.stateDir) {
            this.stateFile = lib.stateFileFor(cfg.stateDir, push, selected.file);
            const saved = lib.loadState(this.stateFile, push, selected.mtimeMs);
            if (saved) {
                cookies = saved;
                log(`使用已保存的轮换 Cookie（${saved.length} 条，派生自同一份推送）`);
            }
        }

        const script = fs.readFileSync(cfg.scriptPath, "utf8");
        const initScript = lib.buildInitScript(script, { wsUrl: this.wsPageUrl, apiKey: cfg.apiKey });

        const fp = push.fingerprint;
        const baseOpts = lib.contextOptions(fp, { defaultTimezone: cfg.defaultTimezone });
        if (cfg.mode === "xvfb") {
            this.xvfb = await startXvfb(baseOpts.screen.width, baseOpts.screen.height);
        }
        this.browser = await launchBrowser(this.chromium, baseOpts, cfg.mode, this.xvfb && this.xvfb.display);
        this.browser.on("disconnected", () => { this.browserGone = true; });
        this.browserVersion = this.browser.version();
        // 保留推送 UA 的平台信息，版本号对齐实际 Chromium
        const ua = lib.alignUAVersion(lib.sanitizeUA((fp && fp.userAgent) || (await nativeUserAgent(this.browser))), this.browserVersion);
        const ctxOpts = lib.contextOptions(fp, { userAgent: ua, defaultTimezone: cfg.defaultTimezone });
        log(`浏览器 ${this.browser.version()}（${cfg.mode}）视口 ${ctxOpts.viewport.width}x${ctxOpts.viewport.height} 屏幕 ${ctxOpts.screen.width}x${ctxOpts.screen.height} ${ctxOpts.locale} ${ctxOpts.timezoneId}`);

        // 代理设在上下文级（CDP Target.createBrowserContext 的 proxyServer/proxyBypassList）：
        // 启动级的 socks5 代理会被 Playwright 附加 --host-resolver-rules="MAP * ~NOTFOUND"，
        // 连直连的 127.0.0.1 也会解析失败；上下文级没有这个副作用。
        this.context = await this.browser.newContext(cfg.proxy ? { ...ctxOpts, proxy: cfg.proxy } : ctxOpts);
        // 页面（公网 https 源）连 127.0.0.1 的环回转发属于 Local Network Access，
        // 新版 Chromium 需授权，无头下没有弹窗可点，这里只对 AI Studio 源预先授权。
        try {
            await this.context.grantPermissions(["local-network-access"], { origin: "https://aistudio.google.com" });
        } catch (e) {
            log("授予 local-network-access 失败（旧版 Chromium 可忽略）: " + e.message.split("\n")[0]);
        }
        if (this.extra.setupContext) await this.extra.setupContext(this.context);
        await this.context.addCookies(cookies);
        await this.context.addInitScript({ content: initScript });
        this.ua = ua;
        this.fp = fp;
        this.locale = ctxOpts.locale;
        this.url = cfg.startUrl || lib.startUrl(push.authuser);

        await this.openPage();
        let reloads = 0;
        let waitSince = Date.now();
        let pageLoadedAt = Date.now();
        let lastCookieCheck = Date.now();
        let lastStateSave = 0;

        while (!isStopping()) {
            await sleep(1000);
            if (this.browserGone) throw new Error("浏览器进程已退出");
            if (this.loginRedirect) {
                // Cookie 已失效：重载只会反复撞 Google 登录页，交给主循环等新推送
                return "cookies-invalid";
            }
            if (this.crashed || this.page.isClosed()) {
                log("⚠ 页面崩溃/关闭，重建页面");
                await this.openPage();
                waitSince = pageLoadedAt = Date.now();
                continue;
            }
            const now = Date.now();
            if (now - lastCookieCheck >= cfg.cookiePollMs) {
                lastCookieCheck = now;
                const cur = lib.selectCookieFile({ dir: cfg.cookieDir, file: cfg.cookieFile, label: cfg.cookieLabel });
                if (cur && (cur.file !== selected.file || cur.mtimeMs !== selected.mtimeMs)) {
                    log(`Cookie 文件已更新（${path.basename(cur.file)}），重启会话`);
                    await this.drain(isStopping);
                    return "cookie-changed";
                }
            }
            if (this.registered) {
                reloads = 0;
                waitSince = now;
                if (this.stateFile && now - lastStateSave >= cfg.stateSaveMs) {
                    lastStateSave = now;
                    await this.saveState();
                }
                if (cfg.recycleMs > 0 && now - pageLoadedAt > cfg.recycleMs && this.inflight.size === 0) {
                    log(`页面已运行 ${Math.round((now - pageLoadedAt) / 3600000)}h 且空闲，定期重载以回收内存`);
                    await this.reload();
                    waitSince = pageLoadedAt = Date.now();
                }
                continue;
            }
            // 未注册：4001（被另一个 headless worker 顶替）/ 4008（Key 被拒）重载也无济于事，放慢节奏
            const slow = this.lastCloseCode === 4001 || this.lastCloseCode === 4008;
            const limit = slow ? Math.max(cfg.registerTimeoutMs, 10 * 60 * 1000) : cfg.registerTimeoutMs;
            if (now - waitSince < limit) continue;
            if (reloads >= cfg.maxReloads) {
                throw new Error(`连续 ${reloads} 次重载后仍未注册`);
            }
            reloads++;
            log(`⚠ ${Math.round(limit / 1000)}s 内未注册到中枢${this.lastCloseCode ? `（最近关闭码 ${this.lastCloseCode}）` : ""}，重载页面（${reloads}/${cfg.maxReloads}）`);
            this.lastCloseCode = 0;
            await this.reload();
            waitSince = pageLoadedAt = Date.now();
        }
        return "stopped";
    }

    // 等进行中的请求结束（有上限），再重启会话，避免把正在流式返回的请求截断。
    async drain(isStopping) {
        const deadline = Date.now() + this.cfg.drainMs;
        if (this.inflight.size) log(`等待 ${this.inflight.size} 个进行中的请求结束（最多 ${Math.round(this.cfg.drainMs / 1000)}s）…`);
        while (this.inflight.size && Date.now() < deadline && !isStopping()) await sleep(500);
        if (this.inflight.size) log(`⚠ 仍有 ${this.inflight.size} 个请求未结束，强制重启`);
    }

    // 把当前 Cookie 罐（含 Google 轮换后的值）存到状态卷；Cookie 失效时不存。
    async saveState() {
        if (!this.stateFile || !this.context || !this.everRegistered || this.loginRedirect || this.stateDisabled) return;
        try {
            const cookies = await this.context.cookies();
            lib.saveState(this.stateFile, { email: this.push.email, pushMtimeMs: this.pushMtimeMs, cookies });
        } catch (e) {
            this.stateDisabled = true;
            log(`⚠ 保存 Cookie 状态失败，本会话不再尝试（检查 PGGO_STATE_DIR 是否可写）: ${e.message.split("\n")[0]}`);
        }
    }

    markRedirect(rawUrl) {
        let host = "";
        try { host = new URL(rawUrl).hostname; } catch (_) { return; }
        if (!host || host === "aistudio.google.com" || this.loginRedirect) return;
        if (!/(^|\.)google\.com$/.test(host)) return; // about:blank 等不算
        this.loginRedirect = host;
        log(`❌ 页面被重定向到 ${host}：Cookie 已失效或账号需重新验证。请在真实浏览器登录后重新推送 Cookie；在此之前不再访问 Google`);
    }

    async openPage() {
        if (this.page && !this.page.isClosed()) await this.page.close().catch(() => {});
        this.crashed = false;
        this.registered = false;
        this.inflight.clear();
        const page = await this.context.newPage();
        this.page = page;
        page.on("crash", () => { this.crashed = true; });
        page.on("websocket", (ws) => this.watchWs(ws));
        page.on("console", (m) => this.onConsole(m));
        page.on("framenavigated", (frame) => {
            if (frame === page.mainFrame()) this.markRedirect(frame.url());
        });
        await applyPageOverrides(page, this.ua, this.fp, this.locale, this.browserVersion);
        await this.goto();
    }

    async goto() {
        log(`打开 ${this.url}`);
        try {
            await this.page.goto(this.url, { waitUntil: "domcontentloaded", timeout: 90000 });
        } catch (e) {
            log("⚠ 页面加载失败: " + e.message.split("\n")[0]);
            return;
        }
        this.markRedirect(this.page.url());
    }

    async reload() {
        this.registered = false;
        this.inflight.clear();
        try {
            await this.page.goto(this.url, { waitUntil: "domcontentloaded", timeout: 90000 });
        } catch (e) {
            log("⚠ 重载失败: " + e.message.split("\n")[0]);
            return;
        }
        this.markRedirect(this.page.url());
    }

    watchWs(ws) {
        if (!ws.url().includes("/api/pg/ws")) return;
        ws.on("framereceived", (f) => {
            const s = typeof f.payload === "string" ? f.payload : "";
            if (s.includes('"registered"')) {
                try {
                    if (JSON.parse(s).type === "registered") {
                        this.registered = true;
                        this.lastCloseCode = 0;
                        log(this.everRegistered ? "✅ 已重新注册到中枢（channel=headless）" : "✅ 已注册到中枢（channel=headless）");
                        this.everRegistered = true;
                    }
                } catch (_) {}
            } else if (s.includes('"request"')) {
                try {
                    const m = JSON.parse(s);
                    if (m.type === "request" && m.requestId) {
                        this.inflight.add(m.requestId);
                        log(`▶ 请求 ${String(m.requestId).slice(0, 8)} model=${m.model}（进行中 ${this.inflight.size}）`);
                    }
                } catch (_) {}
            }
        });
        ws.on("framesent", (f) => {
            const s = typeof f.payload === "string" ? f.payload : "";
            if (!/"type"\s*:\s*"(stream-end|response|error)"/.test(s)) return;
            try {
                const m = JSON.parse(s);
                if (m.requestId && this.inflight.delete(m.requestId)) {
                    log(`◀ 请求 ${String(m.requestId).slice(0, 8)} ${m.type}${m.type === "error" ? ` ${m.status || ""} ${(m.error && m.error.code) || ""}` : ""}`);
                }
            } catch (_) {}
        });
        ws.on("close", () => {
            if (this.registered) log("⚠ 中枢 WS 断开（脚本会自动重连）");
            this.registered = false;
            this.inflight.clear();
        });
    }

    onConsole(m) {
        const text = m.text();
        if (!text.startsWith("[PG-EXEC]")) return;
        const code = /WS 关闭\((\d+)\)/.exec(text);
        if (code) this.lastCloseCode = Number(code[1]);
        if (this.cfg.pageLog) log("page: " + text.slice(10, 300));
    }

    close() {
        if (!this.closing) {
            this.closing = (async () => {
                this.stopped = true;
                await this.saveState();
                try { if (this.browser) await this.browser.close(); } catch (_) {}
                if (this.xvfb) this.xvfb.stop();
            })();
        }
        return this.closing;
    }
}

// --------------------------------------------------------------------- 主循环 ----
async function runWorker(cfg, { chromium, request, extra, isStopping, onSession } = {}) {
    chromium = chromium || require("playwright").chromium;
    if (!fs.existsSync(cfg.scriptPath)) throw new Error(`用户脚本不存在: ${cfg.scriptPath}`);
    const fwd = await startForwarder(cfg.wsUrl);
    log(`启动：模式 ${cfg.mode}，中枢 ${redactUrl(cfg.wsUrl)}，Cookie 目录 ${cfg.cookieDir}`);
    // 中枢 WS 走 Node 侧 TCP 转发，不经过浏览器代理；页面只连 127.0.0.1（已在 bypass 中）
    log(`出口代理 ${lib.describeProxy(cfg.proxy)}${cfg.proxy ? `，直连: ${cfg.proxy.bypass}` : ""}`);
    if (cfg.proxy && cfg.proxyCheck) proxySelfCheck(cfg.proxy, request); // 不阻塞启动
    let backoff = 5000;
    let warnedFile = "";
    try {
        while (!isStopping()) {
            const selected = lib.selectCookieFile({ dir: cfg.cookieDir, file: cfg.cookieFile, label: cfg.cookieLabel });
            if (!selected) {
                log(`等待 Cookie 推送文件（${cfg.cookieFile || cfg.cookieLabel || "目录内最新 *.json"}）…`);
                await sleepUntil(30000, isStopping);
                continue;
            }
            if (selected.total > 1 && warnedFile !== selected.file) {
                warnedFile = selected.file;
                log(`⚠ 未设置 PGGO_COOKIE_LABEL，目录里有 ${selected.total} 个账号文件，使用最新的: ${path.basename(selected.file, ".json")}（建议显式设置 PGGO_COOKIE_LABEL）`);
            }
            const session = new Session(cfg, chromium, fwd.pageUrl, extra);
            if (onSession) onSession(session);
            let reason;
            try {
                reason = await session.run(selected, isStopping);
            } catch (e) {
                reason = "error";
                log("❌ 会话异常: " + String(e && e.message || e).split("\n")[0]);
            } finally {
                await session.close();
            }
            if (isStopping()) break;
            if (reason === "cookie-changed") {
                backoff = 5000;
                continue;
            }
            if (reason === "cookies-invalid") {
                await waitForCookieChange(cfg, selected, isStopping);
                backoff = 5000;
                continue;
            }
            // 只有真正注册成功过才重置退避；一直注册不上则持续加大间隔
            if (session.everRegistered) backoff = 5000;
            log(`${Math.round(backoff / 1000)}s 后重启浏览器`);
            await sleepUntil(backoff, isStopping);
            backoff = Math.min(backoff * 2, 5 * 60 * 1000);
        }
    } finally {
        await fwd.close();
    }
}

// Cookie 失效后阻塞，直到推送文件变化（重新推送）。期间不启动浏览器、不访问 Google。
async function waitForCookieChange(cfg, selected, isStopping) {
    log("等待重新推送 Cookie…");
    while (!isStopping()) {
        await sleepUntil(cfg.cookiePollMs, isStopping);
        const cur = lib.selectCookieFile({ dir: cfg.cookieDir, file: cfg.cookieFile, label: cfg.cookieLabel });
        if (cur && (cur.file !== selected.file || cur.mtimeMs !== selected.mtimeMs)) {
            log(`检测到新的 Cookie 推送（${path.basename(cur.file)}），重新启动`);
            return;
        }
    }
}

async function sleepUntil(ms, isStopping) {
    const end = Date.now() + ms;
    while (Date.now() < end && !isStopping()) await sleep(Math.min(500, end - Date.now()));
}

function redactUrl(raw) {
    try {
        const u = new URL(raw);
        u.username = "";
        u.password = "";
        return u.toString();
    } catch (_) {
        return "(invalid url)";
    }
}

async function main() {
    let cfg;
    try {
        cfg = readEnv();
    } catch (e) {
        log("❌ 配置错误: " + e.message);
        process.exit(2);
    }
    let stopping = false;
    let current = null;
    const stop = (sig) => {
        if (stopping) return;
        stopping = true;
        log(`收到 ${sig}，正在退出…`);
        if (current) current.close().catch(() => {});
        setTimeout(() => process.exit(0), 10000).unref();
    };
    process.on("SIGTERM", () => stop("SIGTERM"));
    process.on("SIGINT", () => stop("SIGINT"));
    try {
        await runWorker(cfg, { isStopping: () => stopping, onSession: (s) => { current = s; } });
    } catch (e) {
        log("❌ 致命错误: " + e.message);
        process.exit(1);
    }
    log("已退出");
    process.exit(0);
}

module.exports = { readEnv, runWorker, startForwarder, proxySelfCheck, log };

if (require.main === module) main();
