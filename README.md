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
- **多上游路由**：按模型名自动匹配、显式路由映射、failover 自动切换、模型名重写
- **重试与超时**：单上游 429/5xx 指数退避（支持 Retry-After）、连接超时配置
- **流式透传/转换**：协议相同直接透传（零开销），协议不同逐事件转换；思维链 thinking/reasoning 双向透传
- **多模态图片**：`image_url` ↔ Anthropic `image` block 双向转换
- **H5 图形界面**：上游增删改、模型路由、健康检查、请求日志、admin key 鉴权
- **零依赖**：纯 Go 标准库，单 exe，双击即用
- **简单稳定**：原子写配置、请求日志环形缓冲、上游错误透传

## 快速开始

### 编译

```bash
go build -o llm-gateway.exe ./cmd/llm-gateway
```

### 运行

```bash
# 首次运行会在程序同目录生成默认 config.json
llm-gateway.exe

# 指定配置文件
llm-gateway.exe -config D:\path\config.json
```

启动后：

- 图形界面：<http://127.0.0.1:8318/>
- OpenAI Chat 入口：`http://127.0.0.1:8318/v1/chat/completions`
- Anthropic 入口：`http://127.0.0.1:8318/v1/messages`
- Responses 入口：`http://127.0.0.1:8318/v1/responses`
- 模型列表：`http://127.0.0.1:8318/v1/models`

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

### 接入 Claude Code（走 Anthropic 入口）

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8318"
export ANTHROPIC_API_KEY="任意占位"
```

Claude Code 发 `/v1/messages`，网关转成 OpenAI 格式转发到配置的上游（如 DeepSeek）。

### Docker 部署

Go 单二进制天然适合容器化，多阶段构建后镜像约 15MB。

```bash
# 方式一：docker build
docker build -t llm-gateway .
mkdir -p config
docker run -d --name llm-gateway \
  -p 8318:8318 \
  -v "$(pwd)/config:/config" \
  llm-gateway

# 方式二：docker compose
mkdir -p config
docker compose up -d
```

首次运行会在 `config/` 目录生成默认 `config.json`，编辑后重启容器生效。容器内监听 `0.0.0.0:8318`（通过 `-host 0.0.0.0` 覆盖），宿主机访问 `http://127.0.0.1:8318/`。

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
| `upstreams[].protocol` | `openai` 或 `anthropic` |
| `upstreams[].base_url` | API 根地址（OpenAI 兼容通常含 `/v1`，Anthropic 通常不含） |
| `upstreams[].api_key` | 上游 API Key |
| `upstreams[].models` | 该上游提供的模型列表 |
| `upstreams[].timeout_seconds` | 单次上游请求超时（0=不限制） |
| `upstreams[].max_retries` | 429/5xx/网络错误重试次数（0=不重试） |
| `routes` | 模型名 → 上游名或 `上游名:目标模型名` 的显式映射（可选） |

### 路由规则（优先级从高到低）

1. `routes` 显式映射
2. 上游 `models` 列表匹配（命中该模型的所有上游，开启 failover 时按顺序切换）
3. 只有一个上游时自动兜底

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

## 管理 API

| 接口 | 方法 | 说明 |
|---|---|---|
| `/api/info` | GET | 网关信息（不强制鉴权） |
| `/api/config` | GET/PUT | 读取/保存配置 |
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
