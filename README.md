# llm-gateway

本地多模型 API 网关，一个**零依赖的 Go 单二进制**。核心能力是 **OpenAI ↔ Anthropic 协议双向互转**，让你用 Codex 调 Claude、用 Claude Code 调任意 OpenAI 兼容模型，无需改动客户端代码。

带一个内嵌的 H5 图形界面，用于在线配置上游服务、模型路由、健康检查和查看请求日志。

## 定位

在原 `openai2claude_api`（Anthropic→OpenAI 单向转换）的基础上，补齐反向转换并升级为完整网关，替代闭源的 CLIProxyAPI。**只用标准 API Key，不碰 Google OAuth 那套易被风控的机制。**

```
Codex / Claude Code / 任意 OpenAI 客户端
        │  统一入口 http://127.0.0.1:8318/v1
        ▼
   llm-gateway（协议转换 + 路由）
   ├─ 收到 OpenAI 格式 → 按需转 Anthropic → Claude 官方/兼容后端
   ├─ 收到 Anthropic 格式 → 转 OpenAI → 任意 OpenAI 兼容后端
   └─ H5 界面：配置、健康检查、请求日志
```

## 功能特性

- **双向协议互转**：`/v1/chat/completions` ↔ `/v1/messages`，含流式 SSE 与工具调用（tool_calls / tool_use）转换
- **多上游路由**：按模型名自动匹配上游，也支持显式路由映射
- **流式透传/转换**：协议相同直接透传（零开销），协议不同逐事件转换
- **H5 图形界面**：上游增删改、模型路由、健康检查、请求日志，全部在线配置
- **零依赖**：纯 Go 标准库，单 exe，双击即用
- **简单稳定**：原子写配置、请求日志环形缓冲、上游错误透传

## 快速开始

### 编译

```bash
go build -o llm-gateway.exe .
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
- OpenAI 入口：`http://127.0.0.1:8318/v1/chat/completions`
- Anthropic 入口：`http://127.0.0.1:8318/v1/messages`
- 模型列表：`http://127.0.0.1:8318/v1/models`

### 接入 Codex（走 OpenAI 入口）

编辑 `~/.codex/config.toml`：

```toml
model = "claude-sonnet-4-20250514"
model_provider = "openai"

[model_providers.openai]
base_url = "http://127.0.0.1:8318/v1"
wire_api = "chat"
```

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
  "upstreams": [
    {
      "name": "deepseek",
      "protocol": "openai",
      "base_url": "https://api.deepseek.com/v1",
      "api_key": "sk-xxx",
      "models": ["deepseek-chat", "deepseek-reasoner"]
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
| `upstreams[].name` | 上游唯一标识 |
| `upstreams[].protocol` | `openai` 或 `anthropic` |
| `upstreams[].base_url` | API 根地址（OpenAI 兼容通常含 `/v1`，Anthropic 通常不含） |
| `upstreams[].api_key` | 上游 API Key |
| `upstreams[].models` | 该上游提供的模型列表 |
| `routes` | 模型名 → 上游名的显式映射（可选） |

### 路由规则（优先级从高到低）

1. `routes` 显式映射
2. 上游 `models` 列表匹配
3. 只有一个上游时自动兜底

### 协议转换规则

| 客户端入口 | 上游协议 | 行为 |
|---|---|---|
| OpenAI (`/v1/chat/completions`) | openai | 直接透传 |
| OpenAI | anthropic | OpenAI→Anthropic 转换 |
| Anthropic (`/v1/messages`) | anthropic | 直接透传 |
| Anthropic | openai | Anthropic→OpenAI 转换 |

## 管理 API

| 接口 | 方法 | 说明 |
|---|---|---|
| `/api/info` | GET | 网关信息 |
| `/api/config` | GET/PUT | 读取/保存配置 |
| `/api/health` | GET | 上游健康检查 |
| `/api/logs` | GET | 请求日志 |
| `/api/logs/clear` | POST | 清空日志 |

## 与 openai2claude 的关系

`openai2claude_api` 是单向转换（Anthropic→OpenAI）。本项目的 `convert.go` / `stream.go` 复用了其转换思路，补齐了：

- OpenAI→Anthropic 方向（原来只有反向）
- 流式转换状态机（原版流式 tool_calls 有补丁逻辑）
- 多上游路由与 H5 界面（原版单后端 + 环境变量配置）

## 限制

- 首版聚焦 `/v1/chat/completions` ↔ `/v1/messages`，尚未支持 OpenAI `/v1/responses` 协议（Codex 新版 wire_api）
- 流式转换对思维链（reasoning/thinking）暂不透传
- 未做计费、多用户、限流（本地单用户工具，刻意砍掉）

## License

MIT
