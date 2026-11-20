# pggo —— Catiecli Playground Executor 的 CLIProxyAPI 插件

把「浏览器 AI Studio 页面当执行器」这条链路做成 CPA 的原生插件：**一个进程、一套 Key、一个模型列表**。

```
                    ┌──────────────────────── CPA 进程 ────────────────────────┐
你的客户端           │                                                          │
(OpenAI/Claude/     │  /v1/chat/completions                                    │
 Gemini 协议) ──────┼─▶ model=pg-gemini-3.8-flash                              │
                    │        │                                                 │
                    │        ▼   pggo 插件（C ABI 原生插件）                    │
                    │   executor.execute  →  OpenAI chat  →  MakerSuite wire    │
                    │        │                                                 │
                    │        ▼   插件内嵌中枢 127.0.0.1:8788                     │
                    │   WS /api/pg/ws  ◀──────┐                                │
                    └─────────┼───────────────┼────────────────────────────────┘
                              │               │  HTTPS/WSS（脚本分发 + WS）
                              ▼               │
                     浏览器 AI Studio 页面 ────┘
                     （用户脚本做 WAA proof + fetch 调 MakerSuite）
```

和上一版 Node 自建服务的区别：**不再需要单独跑一个服务**。插件被 CPA 加载时顺手把中枢（WS + 脚本分发 + 推送存档）监听在本地端口上，执行器能力直接挂在 CPA 的模型路由里。

v1.1.0 起中枢有两个**执行器通道**，同一个 Key 可以同时在线：

| 通道 | 谁连上来 | 模型前缀（默认） | WS |
|---|---|---|---|
| `page` | 你自己浏览器里的 AI Studio 页面（Tampermonkey 脚本） | `model_prefix`（`pg-`） | `/api/pg/ws`（不带参数，兼容旧脚本） |
| `headless` | 云端无头 worker（本仓库 `headless/`，Playwright + Chromium 跑同一份脚本） | `headless_model_prefix`（`pggo-`） | `/api/pg/ws?channel=headless` |

## 插件做了什么

| 能力 | 说明 |
|---|---|
| `executor` | CPA 里 `pg-*` / `pggo-*` 模型的请求，编码成 MakerSuite `GenerateContent` wire，按前缀投给对应通道的在线执行器，响应解码回 OpenAI 格式 |
| `model_registrar` | 把 `models` 配置里的模型注册两遍：`<model_prefix><name>`（page）与 `<headless_model_prefix><name>`（headless） |
| 内嵌中枢 | `WS /api/pg/ws`、`GET /api/pg/script.user.js`、`POST /api/pggo/push`、`POST /api/robin/bind-by-key`、`GET /api/pg/status`、`POST /api/pg/exec` |
| 执行器 Key | 写在 `plugins.configs.pggo.api-keys`（或 JSON 的 `api_keys`），两处合并。建议填与 CPA 顶层 `api-keys` 相同的值，脚本面板与客户端就用同一个 Key |

wire 编解码按 [AI Studio 私有协议](https://github.com/Mag1cFall/AIStudio2API/blob/main/docs/protocol.md) 的字段表实现：
根数组 `[0]=models/<id>`、`[1]=contents`、`[3]=generationConfig`、`[4]` 留给脚本注入 WAA proof、`[5]=systemInstruction`、`[10]=1`、`[13]=时区`；
响应取 `$[0][frame][0][0][0]` 的 parts 拼正文，`part[12]=1` 归入思考摘要。

## 构建

`c-shared` 动态库必须用**目标平台的 C 工具链**编译，不能纯 Go 交叉编译——所以请在 CPA 那台机器上编：

```bash
# Linux（CPA 主机上，需 gcc + Go 1.22+）
cd cpa-plugin-pggo
make build            # 产出 bin/pggo-v1.1.0.so
# 或直接
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags "-s -w" -o pggo-v1.1.0.so .
```

Windows（装了 MinGW-w64 的机器）：

```powershell
$env:CGO_ENABLED=1; go build -trimpath -buildmode=c-shared -ldflags "-s -w" -o pggo-v1.1.0.dll .
```

**没有 gcc？用 Docker 出 Linux 版**（`Dockerfile.build`，已锁 `GOAMD64=v1`）：

```bash
docker build -f Dockerfile.build --output type=local,dest=bin/linux-amd64 .
# 产出 bin/linux-amd64/pggo.so
```

产物是 glibc 版，适配 CPA 官方镜像（`debian:bookworm`）和 Debian/Ubuntu 主机。
**不支持 Alpine/musl 环境**：Go c-shared 库在 musl 上无法被 dlopen（实测段错误 / TLS 重定位错误）。
可在 CPA 容器里 `ldd --version 2>&1 | head -1` 确认，输出含 `GLIBC` 即可用。

> 插件目录下的文件名决定 ID 与版本：`pggo-v1.1.0.so` → ID `pggo`、版本 `1.1.0`。
> CI 交叉编译时记得锁指令集基线：`GOAMD64=v1 CGO_CFLAGS="-march=x86-64"`，否则老 CPU 上会 SIGILL。

## 安装

### 方式一：CPA 插件商店（推荐，仅 linux/amd64）

在 CPA 的 `config.yaml` 里加一个商店源，然后在管理面板的插件商店里找到 **PGGO Playground Executor** 点安装：

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/in30mn1a/cpa-plugin-pggo/main/registry.json"
```

商店会从本仓库 Release 下载 `pggo_<版本>_linux_amd64.zip` 并用 `checksums.txt` 校验。

### 方式二：手动放文件

```bash
# CPA 的 plugins 目录（config.yaml 里的 plugins.dir，默认 ./plugins）
mkdir -p plugins/linux/amd64
cp bin/linux-amd64/pggo.so plugins/linux/amd64/pggo-v1.1.0.so
```

## 配置

装好后在 CPA 管理面板「插件 → pggo → 配置」里直接填（≥ v1.0.1；前缀字段 ≥ v1.1.0），保存即生效：

| 字段 | 说明 |
|---|---|
| `api-keys` | **必填**。页面执行器 register 用的 Key，即脚本面板里填的那个；建议与 CPA 顶层 `api-keys` 相同 |
| `listen` | 内嵌中枢监听地址，默认 `127.0.0.1:8788`。**CPA 跑在 Docker 里要改成 `0.0.0.0:8788` 并映射端口**，否则反代连不进容器。修改后自动重绑 |
| `script_path` | 用户脚本文件路径（容器内路径，需挂载），供 `/api/pg/script.user.js` 分发；不填就不提供分发 |
| `data_dir` | Cookie 推送 / Robin 绑定落盘目录，默认 `pggo-data` |
| `models` | 上游模型 ID 列表，每个注册两遍（见下两行）；不填用内置默认列表。**上游 ID 必须真实存在**（插件不做 ListModels 探测） |
| `model_prefix` | page 通道（真实浏览器）模型前缀，默认 `pg-`，可改成 `pg/`；写成显式空串 `""` 即停用 page 通道（不注册、不路由） |
| `headless_model_prefix` | headless 通道（云端无头 worker）模型前缀，默认 `pggo-`，可改成 `pggo/`；**必须与 `model_prefix` 不同**（相同时只注册 page 一套并在日志告警）；显式空串 `""` 即停用 headless 通道 |
| `config_file` | 可选。JSON 配置文件（模板 `config.example.json`），用于 `model_map` / `timezone` / `max_output_tokens` / `request_timeout_sec` 等高级项；面板字段非空时优先 |

等价的 `config.yaml` 写法：

```yaml
plugins:
  enabled: true
  configs:
    pggo:
      enabled: true
      api-keys:
        - "<与 CPA 顶层 api-keys 相同的值>"
      listen: "0.0.0.0:8788"
      script_path: "/CLIProxyAPI/pggo/Catiecli Playground Executor.user.js"
      data_dir: "/CLIProxyAPI/pggo/data"
      model_prefix: "pg/"
      headless_model_prefix: "pggo/"
```

**路由规则**：请求模型名按**最长匹配前缀**决定通道并剥掉该前缀（`pg` 与 `pggo` 这种互为前缀的写法也能正确区分）；
两个前缀都不匹配时走 page（兼容 `model_map` 里的裸名）。目标通道离线时直接返回 `503 executor_offline` 并点名是哪个通道，
**不会**静默回落到另一个通道——两条链路的账号/风控状态不同，悄悄切换会让问题难以定位。需要兜底请在客户端或 CPA 侧配置模型级重试。

> **模型 ID 里带 `/` 是否安全？** 按 CPA 源码（`router-for-me/CLIProxyAPI` main @ `6faa8d2`）核对：插件静态模型按原样注册
>（`internal/pluginhost/adapters.go` `RegisterModels` 只做 `TrimSpace`；`adapters_executors.go` `RegisterClient("plugin:<id>:<provider>:executor", …)`），
> 请求时按模型名精确查表（`internal/util/provider.go` `GetProviderName` → `internal/registry/model_registry.go` `GetModelProviders` 的 `r.models[modelID]`），
> 「`<前缀>/<模型>`」的剥离只发生在**所选凭证自身配置了 `prefix`** 时（`sdk/cliproxy/auth/conductor_models.go` `rewriteModelForAuth`），插件静态执行器没有凭证前缀，
> 所以 `pg/gemini-x` 会原样交给插件，由插件自己剥前缀。注意两点：① 不要给 CPA 里其它凭证配置名为 `pg` / `pggo` 的 `prefix`，否则它们注册的 `pg/<模型>` 会与本插件同名混在一起；
> ② Gemini 原生路由（`/v1beta/models/{model}:generateContent`）把模型放在 URL 路径里，含 `/` 的模型名不适合走这条入口，用 OpenAI / Claude 兼容入口（模型在 body 里）即可。
> 以上为源码静态核对，未在运行中的 CPA 上实测；保守起见默认值仍是 `pg-` / `pggo-`。

> ⚠ CPA 只把 `plugins.configs.pggo` **这一节**下发给插件，**不会继承顶层 `api-keys`**。
> 本节和 JSON 都没有 Key 时，页面 register 会全部被 `4008` 拒绝，CPA 日志里会有提示。

## 脚本 & 反代

本仓库**不附带用户脚本**。把 Catiecli Playground Executor 脚本里的作者域名换成你自己的域名（可用独立后端方案里的 `patch-userscript.mjs`），放到 `script_path` 指向的位置；
之后 Tampermonkey 直接访问 `https://your.domain.example/api/pg/script.user.js` 安装。

浏览器首次 `register` 用的是**面板里填的 Key**，必须在上面配置的执行器 Key 之列。

nginx（wss 必须走 HTTPS，Google 页面是 https，明文 ws 会被当混合内容拦掉）：

```nginx
location /api/pg/ {
    proxy_pass http://127.0.0.1:8788;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_read_timeout 1800s;
    proxy_send_timeout 1800s;
}
location /api/pggo/ { proxy_pass http://127.0.0.1:8788; proxy_read_timeout 1800s; }
location /api/robin/ { proxy_pass http://127.0.0.1:8788; proxy_read_timeout 1800s; }
```

## 云端无头执行器（headless worker，v1.1.0）

`headless/` 是一个独立的小服务（Node.js + Playwright + Chromium，自带 Dockerfile），跑在 CPA 旁边，让 `pggo-*` 模型**不依赖你的浏览器挂机**：

1. 读取中枢落盘的 Cookie 推送文件（浏览器脚本面板里点「推送 Cookie」→ `POST /api/pggo/push` → `data_dir/cookies/<邮箱>.json`）；
2. 按推送里的浏览器现场建 Chromium 上下文：UA（保留推送的平台/OS，Chrome 版本号对齐镜像里实际的 Chromium 大版本，去掉 `HeadlessChrome`，并同步 `navigator.userAgentData` / `navigator.platform`）、语言、时区、视口/屏幕、DPR、CPU 核数；注入 Cookie（含 HttpOnly 的 `SAPISID` / `__Secure-*`，按 host-only / 域 Cookie、`__Host-`、`sameSite`、过期时间正确换算；**日志只打 Cookie 名，从不打值**）；
3. 打开 `https://aistudio.google.com[/u/N]/prompts/new_chat?temporary=true`（`N` = 推送里的 `authuser`，与脚本换号逻辑一致）；
4. 在 `document-start` 用 `addInitScript` 注入同一份用户脚本（脚本是 `@run-at document-start`，必须早于页面 bundle 拿走 `fetch` 引用，fetch/XHR 捕获才有效），并提供脚本用到的 GM 环境：
   - `unsafeWindow` = `window`；`@noframes` / `@match` 守卫（子帧、非 `aistudio.google.com/*prompts*` 页面不执行）；
   - `GM_getValue` / `GM_setValue` / `GM.getValue` / `GM.setValue`：页内内存存储，预置执行器 Key（`pg_api_key`）与「自动引导拦截」开关；页面整页跳转后回落到预置值（丢的只是可重建的请求头快照和面板记录）；
   - `GM.xmlHttpRequest`：只被「检查更新 / 推送 Cookie / Robin 绑定」使用，无头场景不需要，统一异步回调 `onerror`，不发任何外部请求；
   - `GM_registerMenuCommand` / `GM_openInTab`：空操作；`GM_cookie`：不提供（无头 worker 不推送 Cookie）；
   - 这些 shim 只存在于注入闭包内，不挂到 `window` 上；
5. 把脚本里的 `WS_URL` 改写成内部地址（默认 `ws://cliproxy:8788/api/pg/ws?channel=headless`）。https 页面连明文 `ws://cliproxy` 会被当混合内容拦截，所以 worker 在进程内开一个 `127.0.0.1` 随机端口做 TCP 透传，页面实际连的是环回地址，并为 AI Studio 源预授 `local-network-access`；
6. 脚本随后像真实浏览器一样 `register`，中枢把它放进 `headless` 通道。

**轮换 Cookie 持久化**：Google 会在运行中轮换 Cookie（`__Secure-*PSIDTS` 等），新值只在浏览器内存里。worker 注册成功后每 `PGGO_STATE_SAVE_MIN` 分钟、以及优雅退出时，把 Cookie 罐写到 `PGGO_STATE_DIR/<邮箱>.state.json`（0600）；重启时若该状态文件**属于同一账号且派生自当前这份推送**就优先使用。浏览器重新推送后（推送文件变了）旧状态自动作废。Cookie 失效时不保存。

**Cookie 失效**：页面被重定向到 Google 登录/验证页（任何非 `aistudio.google.com` 的 `*.google.com`）时，worker 不再重载，日志明确提示「重新推送 Cookie」，关闭浏览器并**一直等到推送文件变化**才重新启动，避免反复访问登录页。

**看门狗**：监听页面 WS 帧判断是否已注册；超过 `PGGO_REGISTER_TIMEOUT_SEC` 未注册就重载页面，连续 `PGGO_MAX_RELOADS` 次仍不行则重启浏览器（指数退避 5s → 5min）；页面崩溃自动重建；被另一个 headless worker 顶替（4001）或 Key 被拒（4008）时放慢到 10 分钟一次，避免两个 worker 互顶；Cookie 文件变化（重新推送）时先等进行中的请求结束（最多 `PGGO_DRAIN_SEC` 秒）再重启会话；浏览器重启的退避只在成功注册过之后才重置；页面空闲且运行超过 `PGGO_RECYCLE_HOURS` 时定期重载回收内存；`SIGTERM` 优雅退出。

### 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `PGGO_API_KEY` | （必填） | 执行器 Key，必须在插件 `api-keys` 之列 |
| `PGGO_WS_URL` | `ws://cliproxy:8788/api/pg/ws?channel=headless` | 中枢 WS 地址；缺 `channel` 参数会自动补 `channel=headless`；非环回的 `ws://` 自动走本机透传，`wss://` 直连 |
| `PGGO_COOKIE_DIR` | `/data/cookies` | Cookie 推送目录（挂载 `data_dir/cookies`，只读即可） |
| `PGGO_COOKIE_FILE` | 空 | 指定文件名（相对 `PGGO_COOKIE_DIR`）或绝对路径 |
| `PGGO_COOKIE_LABEL` | 空（**强烈建议设置**） | 按邮箱选文件（匹配文件名或文件内 `email`）；`FILE` 与 `LABEL` 都不填时用目录里**最新**的 `*.json`，目录里有多个账号时日志会告警并写明选中的账号（只写邮箱/文件名） |
| `PGGO_SCRIPT_PATH` | `/app/userscript.user.js` | 换过域名的用户脚本（与 `script_path` 同一份即可） |
| `PGGO_HEADLESS_MODE` | `xvfb` | `xvfb` = 有头 Chromium 跑在 Xvfb 虚拟屏上；`new` = Chromium 新 headless |
| `PGGO_START_URL` | 按 `authuser` 推导 | 覆盖起始页 |
| `PGGO_DEFAULT_TIMEZONE` | `UTC` | 推送里没有时区时使用 |
| `PGGO_REGISTER_TIMEOUT_SEC` | `180` | 多久未注册就重载（脚本会先等页面 bundle 最多 120s） |
| `PGGO_MAX_RELOADS` | `3` | 连续重载上限，超过重启浏览器 |
| `PGGO_RECYCLE_HOURS` | `6` | 空闲时定期重载页面；`0` 关闭 |
| `PGGO_COOKIE_POLL_SEC` | `10` | 检查 Cookie 文件变化的间隔 |
| `PGGO_PAGE_LOG` | `1` | 转发脚本 `[PG-EXEC]` 日志；`0` 关闭 |
| `PGGO_STATE_DIR` | `/state` | 轮换 Cookie 的保存目录（需可写，挂命名卷）；设为空串关闭持久化 |
| `PGGO_STATE_SAVE_MIN` | `10` | 注册后保存 Cookie 状态的间隔（分钟） |
| `PGGO_DRAIN_SEC` | `60` | Cookie 更新重启前等待进行中请求的上限 |

`PGGO_WS_URL` 里的 `channel` 参数总是被强制改成 `headless`，worker 不可能注册到 page 通道去顶掉你的真实浏览器。

**`xvfb` 还是 `new`？** 默认 `xvfb`。这条链路成败取决于页面里的 BotGuard/WAA proof，有头浏览器是与真实用户差异最小的形态；
Chromium「新 headless」已与有头共用同一内核，差异很小，但仍有少量 headless 特有信号。代价是镜像多一个 `xvfb` 包（约 +10–20MB）、运行时多一个 Xvfb 进程（约 +20–50MB 内存）。
两者用同一份完整 Chromium（镜像不装特征明显的 `chromium-headless-shell`），切换只需改环境变量；Xvfb 起不来时可退到 `new`。

### docker compose

```yaml
services:
  pggo-headless:
    build: ./cpa-plugin-pggo/headless     # 或预先 docker build -t pggo-headless ./headless 后用 image:
    restart: unless-stopped
    mem_limit: 1g                          # 目标常驻 < ~600MB，留余量给 AI Studio 页面
    shm_size: 256m
    # 非 root 运行：Playwright 下 Chromium 带 --no-sandbox，不能用 root。
    # 中枢把 Cookie 写成 root:root 0640（目录 0750），gid 0 即可只读访问。
    user: "10001:0"
    environment:
      PGGO_API_KEY: "<与插件 api-keys 相同>"
      PGGO_WS_URL: "ws://cliproxy:8788/api/pg/ws?channel=headless"
      PGGO_COOKIE_DIR: /data/cookies
      PGGO_SCRIPT_PATH: /app/userscript.user.js
      PGGO_HEADLESS_MODE: xvfb
      PGGO_COOKIE_LABEL: "you@gmail.com"   # 强烈建议：明确用哪个账号的推送
    volumes:
      - ./pggo/data/cookies:/data/cookies:ro                                   # = 插件 data_dir/cookies
      - pggo-headless-state:/state                                             # 轮换后的 Cookie（worker 可写）
      - "./pggo/Catiecli Playground Executor.user.js:/app/userscript.user.js:ro"
    networks: [default]                     # 与 cliproxy 同一网络；不对外发布端口
    depends_on: [cliproxy]

volumes:
  pggo-headless-state:
```

> v1.1.0 之前推送的旧文件是 0600，需要重新推送一次（或手动 `chmod 640`、目录 `chmod 750`）worker 才读得到。

插件侧 `listen` 要能被同网络容器访问（`0.0.0.0:8788`）。worker 不需要经过 Caddy，也不需要公网域名。

### ⚠ 风险说明

- **已知限制**：UA / UA-CH / `navigator.platform` 的覆盖作用于页面主线程；脚本或页面创建的**专用 Worker**（Dedicated Worker）里 `navigator.platform` / `userAgentData` 仍可能暴露真实平台（Linux）。没有找到低成本的可靠修复，若 Google 在 Worker 里采集这些字段，平台不一致可被识别。

- **数据中心 IP + 自动化浏览器可能触发 Google 风控**（要求重新登录、`CookieMismatch`、账号受限等）。worker 尽量复刻推送时的浏览器现场，但出口 IP、TLS/网络特征、GPU（软件渲染）与真实设备不同，**无法保证不被识别**，也无法保证 Cookie 长期有效。建议用非主力账号，出问题时在真实浏览器里重新登录并重新推送 Cookie（worker 会自动加载新文件）。
- 推送文件就是账号凭据：目录只读挂载给 worker，不要放进镜像或版本库。

### 本地集成测试（无 Google 账号）

`headless/test/integration-entry.js` 与生产 worker 走同一流程，只是把 `https://aistudio.google.com/**` 拦截成桩页面；配合 Go 侧的
`TestHeadlessWorkerIntegration`（默认跳过）可验证：推送落盘 → Cookie 解析/注入 → 指纹 → 脚本注入与 GM shim → WS 改写与环回透传 → 脚本注册到 `headless` 通道（同 Key 的 page 执行器同时在线）→ `pggo-*` 请求到达脚本并回包 → 重新推送 Cookie 后会话自动重启并重新注册。

```bash
# 终端 1：中枢 + 断言
PGGO_HEADLESS_IT=1 PGGO_HEADLESS_IT_LISTEN=0.0.0.0:18788 PGGO_HEADLESS_IT_DATA=/tmp/pggo-it \
  go test -count=1 -run TestHeadlessWorkerIntegration -v -timeout 10m .
# 终端 2：worker（本机 Node，或在镜像里挂载 test/ 后运行同一入口）
cd headless && npm ci && npx playwright install --no-shell chromium
PGGO_API_KEY=cat-it PGGO_WS_URL=ws://<本机局域网 IP>:18788/api/pg/ws PGGO_COOKIE_DIR=/tmp/pggo-it/cookies \
  PGGO_SCRIPT_PATH="<用户脚本路径>" PGGO_HEADLESS_MODE=new node test/integration-entry.js
# 单元测试
cd headless && npm test
```

桩页面没有真实的 AI Studio bundle，脚本的自动引导会以 `bootstrap_composer_missing` 失败——这正是预期的「请求已到达脚本并由脚本回包」。真实 Google 链路（WAA proof、上游请求）只能用真账号验证。

## 调用

```bash
curl https://your.domain.example/v1/chat/completions \
  -H "Authorization: Bearer <CPA api-key>" -H "Content-Type: application/json" \
  -d '{"model":"pg-gemini-3.8-flash","stream":true,"messages":[{"role":"user","content":"你好"}]}'
```

`GET /v1/models` 里能看到 `pg-*` 与 `pggo-*` 两批，和其它 provider 的模型混在一起；`pggo-*` 走云端无头 worker。

## 自检

```bash
curl https://your.domain.example/api/pg/status
# {"online":true,"connected_at":1795178734,"pending":0,"models":[...],
#  "channels":{"page":{"online":true,"connected_at":1795178734},"headless":{"online":false}}}

curl -X POST https://your.domain.example/api/pg/exec \
  -H "X-API-Key: <key>" -H "Content-Type: application/json" \
  -d '{"model":"pg-gemini-3.8-flash","request_json":"{\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"}'
# {"text":"...","thoughts":"...","usage":{...}}   ← 绕过 CPA 直接验页面链路
```

顶层 `online` = 任一通道在线（兼容旧版）；各通道看 `channels.<name>.online`。`connected_at` 是最近一次注册时间，从未连接过则不输出该字段。
`channels.page.online:false` 就是浏览器页面没连上（脚本没装 / Key 不对 / 页面没开）；`channels.headless.online:false` 看 worker 容器日志。

## 已知边界

- **流式是聚合后下发**：C ABI 一次调用只能返回一个结果信封，插件把页面推回来的所有 SSE 帧攒齐再一次性交给 CPA。客户端能看到 `stream:true` 的完整帧序列，但不是逐字实时。要真流式得走 `host.stream.emit` 宿主回调（宿主请求里带 `stream_id`、需在 init 时保存 host API），可行但本版未实现。
- **每通道单执行器**：同一通道内新连接顶替旧连接（关闭码 `4001`），被顶的页面不自动重连；page 与 headless 两个通道互不顶替。
- **只有文本**：图片/音频/函数调用/grounding 没有编码，请求里出现图片会丢。
- **思考摘要不进正文**：`part[12]=1` 的帧归到 `thoughts` 字段，不下发到 `choices`。
- `/api/pggo/push` 收到的 Cookie 原子写入 `data_dir/cookies/<邮箱>.json`（文件 0640、目录 0750），由云端无头 worker 读取（见下节）。
- 执行器在请求完成前断开时，它名下的请求立即以 `502 executor_disconnected` 结束；只有接收请求的那条连接能续写/结束该请求。
- Robin 凭证绑定落 `data/robin.jsonl`，消费方自理。

## 排错

| 现象 | 定位 |
|---|---|
| `/v1/models` 没有 `pg-*` | 插件没加载：看 CPA 启动日志有没有 `plugin` 报错；文件名必须叫 `pggo-v<版本>.so` |
| 模型列表有、请求 503 `executor_offline` | 报错里点名的通道没有执行器：看 `/api/pg/status` 的 `channels`；page 查 `listen` 端口和反代，headless 查 worker 容器日志 |
| 请求 502 `upstream_not_json` | 页面拿到了非 200（proof/headers 没就绪），面板日志里有具体上游错误 |
| 页面反复 4008 断开 | 面板 Key 不在执行器 Key 里（`plugins.configs.pggo.api-keys` ∪ JSON `api_keys`；CPA 顶层 `api-keys` **不会**自动继承） |
| 请求 504 `executor_timeout` | 页面被后台冻结 / 长请求超 `request_timeout_sec` |

## 测试

```bash
CGO_ENABLED=0 go test -count=1 ./...
```

Go 侧用例覆盖：wire 形状（含 proof 槽位、时区路径、generationConfig 下标）、真实帧解码（正文/思考/usage/流尾错误）、
SSE 收尾、YAML 配置抽取、WS 注册鉴权（4008）、顶替（4001）、分块补发拼接、`/api/pg/exec` 全链路、推送与绑定存档。
v1.1.0 新增：双通道同 Key 共存与通道内顶替、按前缀路由（含 `pg/` 与 `pggo/`、互为前缀的最长匹配）、离线通道 503、status JSON（未连接不输出零值时间）、前缀面板字段解析与双套注册。
无头 worker 的单元测试见 `headless/test/lib.test.js`（`npm test`）。
