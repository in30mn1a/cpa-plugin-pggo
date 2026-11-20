"use strict";
// 本地集成测试入口（仅测试用）：与生产 worker 相同的流程，只是把
// https://aistudio.google.com/** 拦截成一个最小桩页面——没有 Google 账号也能验证
// Cookie 解析 → 上下文/指纹 → 脚本注入与 GM shim → WS 改写/环回转发 →
// 脚本注册到 channel=headless → 中枢下发的请求到达脚本。
const path = require("path");
const fs = require("fs");
const { readEnv, runWorker, log } = require("../src/worker");

const STUB = fs.readFileSync(path.join(__dirname, "stub-aistudio.html"), "utf8");

async function main() {
    const cfg = readEnv();
    let stopping = false;
    process.on("SIGTERM", () => { stopping = true; });
    process.on("SIGINT", () => { stopping = true; });
    await runWorker(cfg, {
        isStopping: () => stopping,
        extra: {
            async setupContext(context) {
                log("⚠ 测试模式：aistudio.google.com 被桩页面替换");
                let cookieHeaderSeen = false;
                await context.route("https://aistudio.google.com/**", async (route) => {
                    const cookie = (await route.request().allHeaders()).cookie || "";
                    if (!cookieHeaderSeen) {
                        cookieHeaderSeen = true;
                        // 只打印 Cookie 名，不打印值
                        const names = cookie.split(/;\s*/).map((kv) => kv.split("=")[0]).filter(Boolean);
                        log(`测试：桩页面请求携带 Cookie 名: ${names.sort().join(",")}`);
                    }
                    if (process.env.PGGO_TEST_LOGIN_REDIRECT === "1") {
                        // 模拟 Cookie 失效：跳到 Google 登录页（Playwright 对导航请求 fulfill 302 会挂起，
                        // 这里用客户端跳转代替；生产中的 HTTP 302 由 goto 后检查最终 URL 覆盖）
                        await route.fulfill({ status: 200, contentType: "text/html", body: '<script>location.replace("https://accounts.google.com/ServiceLogin?continue=x")</script>' });
                        return;
                    }
                    await route.fulfill({ status: 200, contentType: "text/html; charset=utf-8", body: STUB });
                });
                await context.route("https://accounts.google.com/**", async (route) => {
                    log("测试：访问了（桩）Google 登录页");
                    await route.fulfill({ status: 200, contentType: "text/html", body: "<html><body>login stub</body></html>" });
                });
            },
        },
    });
    process.exit(0);
}

main().catch((e) => {
    log("❌ " + e.message);
    process.exit(1);
});
