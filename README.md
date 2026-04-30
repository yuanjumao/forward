# AI Proxy - 轻量级 OpenAI 兼容转发代理

用 Go 编写的轻量级 AI 模型转发代理，核心能力是**自动故障切换**：当一个后端不可用时，自动 fallback 到下一个健康后端。

## 核心特性

- **自动 Fallback** — 同一模型可配置多个后端，请求失败时自动切换到下一个健康后端
- **实时故障剔除** — 转发失败时立即标记不健康，不用等定时检查
- **流式输出支持** — 使用 `go-openai` 库完整支持 SSE 流式响应，流建立前支持 fallback
- **定时健康检查** — 定时向后端发送简单消息验证可用性，恢复后自动加入
- **启动前检查** — 首次启动时检查所有后端，无可用模型则直接退出
- **OpenAI 兼容** — 对外暴露标准 `/v1/chat/completions` 和 `/v1/models` 接口

## 快速开始

```bash
# 编译
go build -o ai-proxy .

# 启动
./ai-proxy -config config.yaml
```

## 配置文件

```yaml
server:
  listen: ":8080"

health_check:
  interval: 60        # 健康检查间隔（秒）
  timeout: 30         # 单次检查超时（秒）
  message: "hello"    # 健康检查发送的消息
  expected: "hello"   # 期望返回内容

backends:
  # 同一个模型配置多个后端，请求时按顺序 fallback
  - name: "openai-primary"
    base_url: "https://api.openai.com/v1"
    api_key: "sk-xxxx"
    models:                    # 支持多模型
      - "gpt-4o"
      - "gpt-4o-mini"

  - name: "openai-backup"
    base_url: "https://api.openai-backup.com/v1"
    api_key: "sk-yyyy"
    models:
      - "gpt-4o"              # gpt-4o 有两个后端，primary 挂了自动切 backup

  - name: "deepseek"
    base_url: "https://api.deepseek.com/v1"
    api_key: "sk-xxxx"
    model: "deepseek-chat"    # 单模型兼容写法
```

## 自动切换流程

```
客户端请求 model=gpt-4o
    │
    ├─→ openai-primary (健康) ──→ 请求成功 ──→ 返回响应
    │                           └─→ 请求失败 ──→ 立即标记不健康
    │                                              │
    ├─→ openai-backup (健康) ──→ 请求成功 ──→ 返回响应
    │                          └─→ 请求失败 ──→ 标记不健康
    │
    └─→ 所有后端失败 ──→ 返回 502 错误
```

- 非流式请求：逐个尝试后端，失败自动切换
- 流式请求：在流建立前支持 fallback，流建立后直接转发

## API 接口

### POST /v1/chat/completions

```bash
# 非流式
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-4o", "messages": [{"role": "user", "content": "你好"}]}'

# 流式
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-4o", "stream": true, "messages": [{"role": "user", "content": "你好"}]}'
```

### GET /v1/models

返回当前所有健康可用的模型。

### GET /status

返回所有后端+模型的详细状态：

```json
{
  "status": "degraded",
  "healthy_count": 3,
  "total_count": 5,
  "backends": [
    {
      "backend_name": "openai-primary",
      "model": "gpt-4o",
      "healthy": true,
      "last_check": "2025-01-01T12:00:00Z"
    },
    {
      "backend_name": "openai-backup",
      "model": "gpt-4o",
      "healthy": false,
      "last_check": "2025-01-01T12:00:00Z",
      "last_error": "转发失败: 401 Unauthorized"
    }
  ]
}
```

### GET /health

简单存活探测，有可用后端返回 200，否则 503。
