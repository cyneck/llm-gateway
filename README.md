# llm-gateway

本地多模型 API 网关，一个**零依赖的 Go 单二进制**。核心能力是 **OpenAI ↔ Anthropic 协议双向互转 + OpenAI `/v1/responses` 协议入口**，让你用 Codex（新版 `wire_api=responses` 也支持）调 Claude、用 Claude Code 调任意 OpenAI 兼容模型，无需改动客户端代码。

带一个内嵌的 H5 图形界面，用于在线配置上游服务、模型路由、健康检查和查看请求日志。

## 定位

在原 `openai2claude_api`（Anthropic→OpenAI 单向转换）的基础上，补齐反向转换、Responses 协议，并升级为完整网关，替代闭源的 CLIProxyAPI。**只用标准 API Key，不碰 Google OAuth 那套易被风控的机制。**

```
Codex / Claude Code / 任意 OpenAI 客户端
        │  统一入口 http://127.0.0.1:8318/v1
        ▼
   llm-gateway（协议转换 + 路由 + 重试/failover）
   ├─ /v1/chat/completions  ↔ /v1/messages（双向互转，流式 + 工具）
   ├─ /v1/responses（Codex wire_api=responses）
   ├─ 上游超时/重试/failover
   └─ H5 界面：配置、健康检查、请求日志
```

## 功能特性

- **双向协议互转**：`/v1/chat/completions` ↔ `/v1/messages`，含流式 SSE 与工具调用（tool_calls / tool_use）转换
- **Responses 协议入口**：`/v1/responses` 原生支持，适配 Codex 新版 `wire_api=responses`
- **官方模型透传**：`gpt-*`/`codex-*`/`o1-o4` 模型保留订阅凭证原样直连 OpenAI 官方，与第三方模型共存同一入口
- **多上游路由**：按模型名自动匹配、显式路由映射、failover 自动切换、模型名重写
- **重试与超时**：单上游 429/5xx 指数退避（支持 Retry-After）、连接超时配置
- **流式透传/转换**：协议相同直接透传（零开销），协议不同逐事件转换；思维链 thinking/reasoning 双向透传
- **多模态图片**：`image_url` ↔ Anthropic `image` block 双向转换
- **H5 图形界面**：上游增删改、模型路由、健康检查、请求日志、admin key 鉴权
- **零依赖**：纯 Go 标准库，单 exe，双击即用
- **简单稳定**：原子写配置、请求日志环形缓冲、上游错误透传

## 快速开始

两种运行方式任选其一：**直接跑二进制**最简单，Windows 上推荐；**Docker** 适合想要隔离和开机自启的场景。

### 方式一：直接运行二进制（Windows 推荐）

**1. 编译**

```bash
# 当前平台
go build -o llm-gateway.exe ./cmd/llm-gateway      # Windows
go build -o llm-gateway     ./cmd/llm-gateway      # macOS / Linux

# 交叉编译（例如在 WSL / macOS 上产出 Windows 二进制）
GOOS=windows GOARCH=amd64 go build -o llm-gateway.exe ./cmd/llm-gateway
GOOS=linux   GOARCH=amd64 go build -o llm-gateway     ./cmd/llm-gateway
GOOS=darwin  GOARCH=arm64 go build -o llm-gateway     ./cmd/llm-gateway
```

零第三方依赖，只要装了 Go 就能编出来。

**2. 启动**

```bash
# 前台运行（首次运行会在程序同目录生成默认 config.json）
llm-gateway.exe

# 指定配置文件与监听地址
llm-gateway.exe -config C:\Users\<你>\.llm-gateway\config.json -host 127.0.0.1 -port 8318
```

Windows 后台常驻（PowerShell，纯 ASCII 参数避免编码问题）：

```powershell
Start-Process -FilePath .\llm-gateway.exe `
  -ArgumentList '-config','C:\Users\<你>\.llm-gateway\config.json' `
  -WindowStyle Hidden

# 停止
Get-Process llm-gateway -ErrorAction SilentlyContinue | Stop-Process
```

Linux / macOS 后台：

```bash
nohup ./llm-gateway -config ~/.llm-gateway/config.json > /tmp/llm-gateway.log 2>&1 &
```

> **三点提醒**
> - **不带 `-config` 时，程序读的是「它自己所在目录」下的 `config.json`**。
>   直接在资源管理器里双击 exe 就会走这条路径，很容易读到一份陈旧的默认配置，
>   看上去像"配置丢了"。实际是读错了文件——想用哪份配置就显式指定：
>   `llm-gateway.exe -config C:\Users\<你>\.llm-gateway\config.json`。
>   程序启动时会在日志里打印**当前生效的配置文件路径**，不确定时先看一眼。
> - 上面用的是 `-ArgumentList` 数组形式，**路径含中文也不会被吞引号**。
>   若改写成一行字符串再交给 PowerShell 5.1，中文路径可能因 GBK 解码出错。
> - 换了新二进制后要先停旧进程再启动，否则端口 8318 被占：
>   `netstat -ano | findstr :8318` 找到 PID，`Stop-Process -Id <PID> -Force`。

### 方式二：Docker 运行

Go 单二进制天然适合容器化，多阶段构建后镜像约 15MB。

**1. 构建并启动**

```bash
# 准备配置目录（首次运行会在里面生成默认 config.json）
mkdir -p config

# A. docker run
docker build -t llm-gateway .
docker run -d --name llm-gateway \
  -p 8318:8318 \
  -v "$(pwd)/config:/config" \
  -v "$HOME/.codex:/root/.codex:ro" \
  -e CODEX_HOME=/root/.codex \
  -e TZ=Asia/Shanghai \
  --add-host host.docker.internal:host-gateway \
  --restart unless-stopped \
  llm-gateway

# B. docker compose（上面的挂载项已写在 docker-compose.yml 里）
docker compose up -d
```

**2. 改完代码后重建**

```bash
docker compose up -d --force-recreate --build
```

> **坑**：镜像 tag 没变时，`docker compose up -d` 会认为无需重建，容器继续跑旧二进制
> （`docker ps` 里 uptime 不重置就是没换）。必须加 `--force-recreate`。

**3. 常用命令**

```bash
docker logs -f --tail 50 llm-gateway   # 看日志
docker restart llm-gateway             # 重启
docker compose down                    # 停止并移除
```

容器内监听 `0.0.0.0:8318`（由 Dockerfile 的 `CMD` 指定），宿主机访问 `http://127.0.0.1:8318/`。

### 启动后验证（两种方式通用）

```bash
curl -s http://127.0.0.1:8318/api/info      # 应返回 name/version
curl -s http://127.0.0.1:8318/v1/models     # 应列出已配置模型
```

可用的服务端点：

- 图形界面：<http://127.0.0.1:8318/>
- OpenAI Chat 入口：`http://127.0.0.1:8318/v1/chat/completions`
- Anthropic 入口：`http://127.0.0.1:8318/v1/messages`
- Responses 入口：`http://127.0.0.1:8318/v1/responses`
- 模型列表：`http://127.0.0.1:8318/v1/models`（加 `?live=1` 会额外向上游拉取实时模型清单）
- 模型目录导出：`http://127.0.0.1:8318/api/model-catalog`（写入 Codex 的 `model_catalog_json`，让模型出现在选择器里）

### 两种方式怎么选

| | 直接跑二进制 | Docker |
|---|---|---|
| 上手成本 | 装 Go 编译一次即可 | 需要 Docker |
| Windows 上的稳定性 | **高**，不经过 WSL 转发层 | 依赖 WSL2 端口转发，可能间歇性不通 |
| 开机自启 | 需自己配计划任务 / 登录项 | `restart: unless-stopped` 自带 |
| 环境隔离 | 无 | 有 |
| 读本机 `~/.codex` | 直接读得到 | 必须挂载进去才读得到 |
| 适合场景 | 本机日常开发、Windows 主力机 | 服务器部署、想要隔离/自启 |

### Docker 方式在 WSL2 上的实测坑

1. **WSL 发行版空闲被回收 → 容器反复重启**。
   症状：`dockerd` 只活几十秒、`docker ps` 里容器 uptime 不断重置、
   Windows 访问 `127.0.0.1:8318` 间歇性失败（`curl` 报连接失败）。
   这不是网关的问题。解法：**在 WSL 里保持一个常驻会话**，例如开一个终端跑
   `sleep 86400`，发行版不被回收后访问即恢复稳定。
   应急：`docker restart llm-gateway` 能暂时恢复。
2. **容器内 `127.0.0.1` 是它自己**，不是宿主机。代理跑在宿主机上时要填宿主机可达地址。
3. **Clash Verge 默认只监听 `127.0.0.1`**，容器/WSL 连不上宿主机的 7897 端口。
   不过 WSL 与容器通常能直连境外域名，所以**容器内一般不配代理**即可；
   确实需要时，要在 Clash 里打开"允许局域网连接"。
4. **必须挂载 `~/.codex`** 才能读到 `models_cache.json` 与登录态，
   否则官方模型清单只能靠联网拉取（见「官方模型透传」章节）。

### 接入 Codex（推荐 responses 入口，新版默认）

编辑 `~/.codex/config.toml`：

```toml
model = "claude-sonnet-4-20250514"
model_provider = "openai"

[model_providers.openai]
base_url = "http://127.0.0.1:8318/v1"
# 留空或显式设置均可
wire_api = "responses"
```

> **为什么 Codex 模型选择器里看不到自定义模型？**
> Codex 下拉框里的模型来自**内置清单 / 官方 `/models` 接口**，它不会读取网关的 `/v1/models`，
> 因此即便网关已配置 DeepSeek 等模型，选择器也不会自动出现。
> 解决办法是给 Codex 指定一份自己的模型目录（与 CLIProxyAPI 时代的做法一致）：
>
> ```bash
> # 生成一份包含网关全部模型的目录文件
> curl -s http://127.0.0.1:8318/api/model-catalog > ~/.codex/model-catalog-local.json
> ```
>
> ```toml
> # ~/.codex/config.toml 顶层
> model_catalog_json = "C:\\Users\\<你>\\.codex\\model-catalog-local.json"
> ```
>
> 重启 Codex 后，网关里的模型就会出现在模型选择器中。**目录文件必须真实存在**，
> 指向不存在的路径会导致 Codex 启动异常。

### 接入 Claude Code（走 Anthropic 入口）

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8318"
export ANTHROPIC_API_KEY="任意占位"
```

Claude Code 发 `/v1/messages`，网关转成 OpenAI 格式转发到配置的上游（如 DeepSeek）。

## 配置说明

`config.json` 结构：

```json
{
  "listen": { "host": "127.0.0.1", "port": 8318 },
  "failover": false,
  "admin_key": "",
  "upstreams": [
    {
      "name": "deepseek",
      "protocol": "openai",
      "base_url": "https://api.deepseek.com/v1",
      "api_key": "sk-xxx",
      "models": ["deepseek-chat", "deepseek-reasoner"],
      "timeout_seconds": 300,
      "max_retries": 2
    },
    {
      "name": "claude",
      "protocol": "anthropic",
      "base_url": "https://api.anthropic.com",
      "api_key": "sk-ant-xxx",
      "models": ["claude-sonnet-4-20250514"]
    }
  ],
  "routes": {}
}
```

字段说明：

| 字段 | 说明 |
|---|---|
| `listen.host/port` | 网关监听地址（改动需重启） |
| `failover` | 模型存在多个上游时失败是否自动切换 |
| `admin_key` | 管理 API Bearer Token（空=不鉴权） |
| `upstreams[].name` | 上游唯一标识 |
| `upstreams[].protocol` | `openai`、`anthropic` 或 `openai-official`（官方透传） |
| `upstreams[].base_url` | API 根地址（OpenAI 兼容通常含 `/v1`，Anthropic 通常不含） |
| `upstreams[].api_key` | 上游 API Key |
| `upstreams[].models` | 该上游提供的模型列表 |
| `upstreams[].timeout_seconds` | 单次上游请求超时（0=不限制） |
| `upstreams[].max_retries` | 429/5xx/网络错误重试次数（0=不重试） |
| `routes` | 模型名 → 上游名或 `上游名:目标模型名` 的显式映射（可选） |

### 路由规则（优先级从高到低）

1. **官方模型透传**：模型名匹配 `gpt-*`/`codex-*`/`o1-o4` 时，路由到 `openai-official` 上游（若配置了），保留客户端订阅凭证直连 OpenAI 官方
2. `routes` 显式映射
3. 上游 `models` 列表匹配（命中该模型的所有上游，开启 failover 时按顺序切换）
4. 只有一个上游时自动兜底

### 模型名重写

`routes` 值支持 `upstream:target_model` 格式。例如让 Codex 请求 `claude-sonnet-4` 时实际调用 DeepSeek：

```json
{
  "routes": {
    "claude-sonnet-4": "deepseek:deepseek-chat"
  }
}
```

### 协议转换规则

| 客户端入口 | 上游协议 | 行为 |
|---|---|---|
| OpenAI (`/v1/chat/completions`) | openai | 直接透传 |
| OpenAI | anthropic | OpenAI→Anthropic 转换 |
| Anthropic (`/v1/messages`) | anthropic | 直接透传 |
| Anthropic | openai | Anthropic→OpenAI 转换 |
| Responses (`/v1/responses`) | openai | Responses→Chat→Responses |
| Responses | anthropic | Responses→Chat→Anthropic→Chat→Responses |
| Responses (`/v1/responses`) | openai-official | 原样透传到 OpenAI 官方（保留订阅凭证，不转换） |

### 官方模型透传（Codex 官方 + 第三方共存）

配置一个 `protocol` 为 `openai-official` 的上游，即可让 Codex 官方模型（`gpt-*`/`codex-*`/`o1-o4`）与第三方模型共用同一个入口：

```json
{
  "upstreams": [
    {
      "name": "openai-official",
      "protocol": "openai-official",
      "base_url": "https://chatgpt.com/backend-api/codex",
      "api_key": "",
      "models": ["gpt-5.6-luna", "gpt-5.6-sol"]
    },
    {
      "name": "volcengine",
      "protocol": "openai",
      "base_url": "https://ark.cn-beijing.volces.com/api/v3",
      "api_key": "ark-xxx",
      "models": ["deepseek-v4-1-flash-260910"]
    }
  ]
}
```

此时 Codex 配置 `model_provider = "local"` 指向 llm-gateway 即可：`gpt-*` 模型自动透传到 OpenAI 官方（保留你的订阅凭证），其他模型走对应上游。`api_key` 留空——官方透传不替换凭证，而是原样转发客户端的 `Authorization` 头。

> **注意**：`openai-official` 是**客户端凭证透传**模式，要求 Codex 自己带着登录态打到网关，
> 所以 provider 必须能触发 Codex 注入凭证（`name = "openai"` / `requires_openai_auth = true`），
> 且代理链路要能访问 `chatgpt.com`。若上游不可达，客户端会报连接失败。

### 官方透传上游：系统内置且只读

`openai-official` 不是普通上游，它是**官方模型的透传通道**，与其他上游有三点本质区别：

| | 普通上游（openai / anthropic） | 官方透传（openai-official） |
|---|---|---|
| 凭证 | 在网关配置 `api_key` | **无需 API Key**，原样透传客户端订阅凭证 |
| 模型清单 | 配置里手写，或向上游 `/models` 拉取 | **向官方 `/models` 实时拉取**（5 分钟缓存） |
| 界面 | 可增删改 | **只读**，不显示 API Key 输入框、不可删除 |

因此它在 H5 界面上以「系统内置 · 只读」卡片呈现，只显示端点与模型清单，并提供「刷新模型」按钮。
后端也会兜底保护：即使通过 API 提交配置，内置上游的字段也不会被覆盖，被删除时会自动补回。

**模型清单的来源**（按可靠性排序）：

1. **本机 Codex 缓存 `~/.codex/models_cache.json`** —— 首选。
   Codex 客户端自己会周期性向官方同步并落盘，网关直接读它即可：
   **完全离线、不需要任何凭证**，而且与 Codex 实际可见的列表一致。
   缓存超过 24 小时会先尝试联网刷新，刷新失败也不会丢掉缓存内容。
2. 联网向官方端点拉取 —— 凭证优先用客户端请求带来的，其次本机登录态
3. 都拿不到则回退到配置里的静态列表

> 官方端点已实测确认：`<base_url>/models?client_version=<版本>`
> （即 `https://chatgpt.com/backend-api/codex/models?client_version=0.158.0`），
> 用 ChatGPT 订阅凭证可拉取到完整模型清单（约 300KB，每个模型 50+ 字段）。
> 缺少 `client_version` 参数会返回 HTTP 400。
> 访问官方域名通常需配置上面的**统一出站代理**。
> 容器部署时挂载 `~/.codex` 并设 `CODEX_HOME` 即可读到缓存（见 `docker-compose.yml`）。

**调用官方模型时的凭证处理（重要）**：

官方通道是**纯透传**——网关把客户端（Codex）带来的全部非逐跳请求头原样转发给官方，
**不会**拿本机 `auth.json` 里的 token 去"补位"。原因有两个：

- 官方端点要靠 `ChatGPT-Account-Id`、`originator`、`session_id` 等头一起识别请求，
  只留一个 `Authorization` 会被判未授权（实测 401）
- `auth.json` 里的 access_token 实测只能用于"列模型"（`/models` 返回 200），
  用于"发请求"（`/responses`）一律 401 —— 拿它注入只会产生误导性的失败

第三方上游则相反：只注入该上游自己的 API Key，绝不把客户端凭证带出去。

**官方清单的实际结构**（实测，与常见 OpenAI 风格不同）：

```json
{ "models": [ { "slug": "gpt-5.6-luna", "visibility": "list", ... } ] }
```

- 顶层键是 `models`，**不是** `data`；模型标识字段是 `slug`，不是 `id`
- `visibility` 为 `hide` 的是内部模型（如 `codex-auto-review`、`gpt-reserve`），
  官方客户端也不会展示，网关照此过滤，避免污染模型选择器
- 拉取失败时**不再静默**：管理界面会显示具体原因（HTTP 状态码、响应片段、缺凭证等），
  同时保留静态清单，保证模型不会凭空消失

**官方通道返回 401/403 怎么排查**：

网关对官方通道只做透传，所以**被拒一定不是网关的凭证问题**。此时网关会在上游响应里
追加一个 `hint` 字段、并在请求日志里标注原因，直接看返回体或管理界面即可。
按可能性从高到低排查：

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| `/models` 200，但 `/responses` 401 | 账号套餐不含 Codex 权限（免费账号只能列模型，不能发请求） | 换有订阅的账号重新 `codex login`，或改用第三方上游 |
| 两个接口都 401 | 登录态过期 | 重新 `codex login` |
| 时好时坏 | 上游风控 | 稍后重试；确认出站代理出口稳定 |

快速确认自己的套餐：解码 `~/.codex/auth.json` 里 `tokens.access_token` 的 JWT，
看 `https://api.openai.com/auth.chatgpt_plan_type` 字段 —— 值为 `free` 就是免费账号，
无权调用官方推理接口。

## 统一出站代理

当上游在境外（例如官方透传要访问 `chatgpt.com`）时，给网关配一个出站代理即可，
**所有上游请求统一走它**，不必逐个上游设置：

```json
{
  "proxy": {
    "url": "http://127.0.0.1:7897",
    "no_proxy": "localhost,127.0.0.1"
  }
}
```

- `url`：代理地址，留空则直连（默认）
- `no_proxy`：逗号分隔的直连主机，支持 `.example.com` 形式匹配子域名，`*` 表示全部直连
- **保存后立即生效，无需重启**（代理决策每次请求实时读取配置）
- 代理地址非法时会明确报错，不会静默退回直连——避免把"配置写错"误判成"网络不通"

也可在 H5 界面「统一出站代理」面板直接填写。

> **容器部署注意**：容器内 `127.0.0.1` 是容器自己，不是宿主机。
> 若代理跑在宿主机上，需填宿主机可达的地址（Linux 可用 `host.docker.internal`
> 或 docker 网桥地址），否则会连接失败。
>
> 实测坑：Clash Verge 默认只监听 `127.0.0.1`，**不会**在局域网网卡上开端口，
> 所以容器/WSL 里填宿主机 IP:7897 是连不上的。要么在 Clash 里打开"允许局域网连接"，
> 要么干脆不配代理——WSL 与容器通常能直连境外域名，只有 Windows 本机进程才需要代理。

## 管理 API

| 接口 | 方法 | 说明 |
|---|---|---|
| `/api/info` | GET | 网关信息（不强制鉴权） |
| `/api/config` | GET/PUT | 读取/保存配置（内置上游不会被覆盖） |
| `/api/model-catalog` | GET | 导出 Codex 模型目录（写入 `model_catalog_json`） |
| `/api/official-models/refresh` | POST | 重新拉取官方模型清单 |
| `/api/health` | GET | 上游健康检查 |
| `/api/logs` | GET | 请求日志 |
| `/api/logs/clear` | POST | 清空日志 |

若配置了 `admin_key`，管理 API（除 `/api/info`）需带 `Authorization: Bearer <admin_key>`，H5 界面会提示输入。

## 项目结构

采用 Go 社区标准布局（`cmd/` + `internal/` + 同目录测试）：

```text
llm-gateway/
├── cmd/llm-gateway/          # 可执行入口（main）
│   └── main.go
├── internal/                 # 私有实现（模块外无法 import）
│   ├── config/               # 配置加载、存储、路由解析
│   ├── convert/              # 协议转换（chat/messages/responses + 流式）
│   ├── gateway/              # 网关核心（转发、重试、failover）
│   │   ├── official.go       # 官方模型清单拉取（凭证透传 + 缓存降级）
│   │   └── catalogdata/      # 内嵌 Codex 模型目录官方基线 schema
│   └── web/                  # 内嵌 H5 管理界面
├── Dockerfile                # 多阶段构建
├── docker-compose.yml
├── go.mod
└── README.md
```

说明：

- **`internal/`**：Go 强制语义，该目录下的包只能被本模块代码导入，是对外私有实现。
- **测试同目录**：Go 惯例是测试文件（`*_test.go`）与源码放在同一目录，便于访问包内私有符号，`go test ./...` 自动发现。

## 与 openai2claude 的关系

`openai2claude_api` 是单向转换（Anthropic→OpenAI）。本项目的 `convert.go` / `stream.go` 复用了其转换思路，补齐了：

- OpenAI→Anthropic 方向（原来只有反向）
- 流式转换状态机（原版流式 tool_calls 有补丁逻辑）
- `/v1/responses` 协议入口
- 思维链与多模态透传
- 多上游路由、重试、failover、H5 界面（原版单后端 + 环境变量配置）

## 限制

- 未做计费、多用户、限流（本地单用户工具，刻意砍掉）
- Anthropic `cache_control` 在跨协议转换时会丢失（OpenAI 无对应语义）

## License

MIT
