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

## 插件做了什么

| 能力 | 说明 |
|---|---|
| `executor` | CPA 里 `pg-*` 模型的请求，编码成 MakerSuite `GenerateContent` wire，投给在线页面，响应解码回 OpenAI 格式 |
| `model_registrar` | 把 `models` 配置里的模型以 `pg-<name>` 形式注册进 `/v1/models` |
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
make build            # 产出 bin/pggo-v1.0.0.so
# 或直接
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags "-s -w" -o pggo-v1.0.0.so .
```

Windows（装了 MinGW-w64 的机器）：

```powershell
$env:CGO_ENABLED=1; go build -trimpath -buildmode=c-shared -ldflags "-s -w" -o pggo-v1.0.0.dll .
```

**没有 gcc？用 Docker 出 Linux 版**（`Dockerfile.build`，已锁 `GOAMD64=v1`）：

```bash
docker build -f Dockerfile.build --output type=local,dest=bin/linux-amd64 .
# 产出 bin/linux-amd64/pggo-v1.0.0.so
```

产物是 glibc 版，适配 CPA 官方镜像（`debian:bookworm`）和 Debian/Ubuntu 主机。
**不支持 Alpine/musl 环境**：Go c-shared 库在 musl 上无法被 dlopen（实测段错误 / TLS 重定位错误）。
可在 CPA 容器里 `ldd --version 2>&1 | head -1` 确认，输出含 `GLIBC` 即可用。

> 插件文件名就是插件 ID：`pggo-v1.0.0.so` → ID `pggo`、版本 `1.0.0`。
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

商店会从本仓库 Release 下载 `pggo_<版本>_linux_amd64.zip` 并用 `checksums.txt` 校验。装完仍需按下文在 `plugins.configs.pggo` 里填 `config_file` 与 `api-keys`，并准备 `pggo.json` 与用户脚本。

### 方式二：手动放文件

```bash
# CPA 的 plugins 目录（config.yaml 里的 plugins.dir，默认 ./plugins）
mkdir -p plugins/linux/amd64
cp bin/pggo-v1.0.0.so plugins/linux/amd64/
```

`config.yaml`：

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    pggo:
      enabled: true
      priority: 1
      config_file: /opt/cliproxyapi/pggo/pggo.json   # 插件自己的 JSON 配置
      api-keys:                                      # 页面执行器 register 用的 Key
        - "<与 CPA 顶层 api-keys 相同的值>"
```

> ⚠ CPA 只把 `plugins.configs.pggo` **这一节**下发给插件（不含顶层 `api-keys`），
> 所以执行器 Key 必须在本节或 JSON 里显式填写；两处都没填时，页面 register 会全部被 `4008` 拒绝，CPA 日志里会有提示。

`pggo.json`（模板见 `config.example.json`）：

```json
{
  "listen": "127.0.0.1:8788",
  "script_path": "/opt/cliproxyapi/pggo/dist/Catiecli Playground Executor.user.js",
  "data_dir": "/opt/cliproxyapi/pggo/data",
  "api_keys": [],
  "model_prefix": "pg-",
  "models": ["gemini-3.8-flash", "gemini-3.6-flash", "gemini-2.5-pro", "gemini-2.5-flash"],
  "timezone": "Asia/Shanghai",
  "max_output_tokens": 65536,
  "request_timeout_sec": 600
}
```

- `models` 里的名字会注册成 `pg-` + 名字，**上游模型 ID 必须真实存在**（插件不做 ListModels 探测）。
- `model_map` 可把 CPA 侧模型名硬映射到上游 ID（例如 `{"pg-fast":"gemini-3.6-flash"}`）。
- `api_keys` 与 YAML 节里的 `api-keys` 合并生效，任填一处即可。

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

## 调用

```bash
curl https://your.domain.example/v1/chat/completions \
  -H "Authorization: Bearer <CPA api-key>" -H "Content-Type: application/json" \
  -d '{"model":"pg-gemini-3.8-flash","stream":true,"messages":[{"role":"user","content":"你好"}]}'
```

`GET /v1/models` 里能看到 `pg-*` 那一批，和其它 provider 的模型混在一起，客户端不用区分。

## 自检

```bash
curl https://your.domain.example/api/pg/status
# {"online":true,"connected_at":...,"pending":0,"models":[...]}

curl -X POST https://your.domain.example/api/pg/exec \
  -H "X-API-Key: <key>" -H "Content-Type: application/json" \
  -d '{"model":"pg-gemini-3.8-flash","request_json":"{\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"}'
# {"text":"...","thoughts":"...","usage":{...}}   ← 绕过 CPA 直接验页面链路
```

`online:false` 就是浏览器页面没连上（脚本没装 / Key 不对 / 页面没开）。

## 已知边界

- **流式是聚合后下发**：C ABI 一次调用只能返回一个结果信封，插件把页面推回来的所有 SSE 帧攒齐再一次性交给 CPA。客户端能看到 `stream:true` 的完整帧序列，但不是逐字实时。要真流式得走 `host.stream.emit` 宿主回调（宿主请求里带 `stream_id`、需在 init 时保存 host API），可行但本版未实现。
- **单页面执行器**：同一 Key 新连接顶替旧连接（关闭码 `4001`），被顶的页面不自动重连。要多开就多签 Key，并让每个页面用不同 Key。
- **只有文本**：图片/音频/函数调用/grounding 没有编码，请求里出现图片会丢。
- **思考摘要不进正文**：`part[12]=1` 的帧归到 `thoughts` 字段，不下发到 `choices`。
- `pggo-`（云端 Cookie 推送）模式只存档，没有无头执行器；`/api/pggo/push` 收到就落 `data/cookies/*.json`。
- Robin 凭证绑定落 `data/robin.jsonl`，消费方自理。

## 排错

| 现象 | 定位 |
|---|---|
| `/v1/models` 没有 `pg-*` | 插件没加载：看 CPA 启动日志有没有 `plugin` 报错；文件名必须叫 `pggo-v<版本>.so` |
| 模型列表有、请求 503 `executor_offline` | 页面没连上 WS：看 `/api/pg/status`，检查 `listen` 端口和反代 |
| 请求 502 `upstream_not_json` | 页面拿到了非 200（proof/headers 没就绪），面板日志里有具体上游错误 |
| 页面反复 4008 断开 | 面板 Key 不在执行器 Key 里（`plugins.configs.pggo.api-keys` ∪ JSON `api_keys`；CPA 顶层 `api-keys` **不会**自动继承） |
| 请求 504 `executor_timeout` | 页面被后台冻结 / 长请求超 `request_timeout_sec` |

## 测试

```bash
CGO_ENABLED=0 go test -count=1 ./...
```

14 个用例覆盖：wire 形状（含 proof 槽位、时区路径、generationConfig 下标）、真实帧解码（正文/思考/usage/流尾错误）、
SSE 收尾、YAML 配置抽取、WS 注册鉴权（4008）、顶替（4001）、分块补发拼接、`/api/pg/exec` 全链路、推送与绑定存档。
