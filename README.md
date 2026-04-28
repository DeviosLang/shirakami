# Shirakami

**跨仓库代码调用链分析系统** — 基于 LLM Agent Loop，分析代码变更影响的完整调用链路，识别集成测试入口，生成测试场景建议。

## 功能简介

- 输入代码变更（diff / 文字描述 / 两者组合），自动分析影响的完整调用链
- 双向追踪：向下追踪实现路径 + 向上追踪到业务入口仓库
- 支持跨多个 Git 仓库的调用链分析
- 自动识别集成测试入口（HTTP / gRPC / MQ / Cron / CLI），生成测试场景建议
- 输出格式：终端树状图 / JSON / Markdown

## 系统要求

- Go 1.21+
- PostgreSQL 14+
- Redis 7+
- [ripgrep](https://github.com/BurntSushi/ripgrep) (`rg` 命令)
- [gopls](https://pkg.go.dev/golang.org/x/tools/gopls) (`go install golang.org/x/tools/gopls@latest`)

## 快速上手

### 1. 克隆并编译

```bash
git clone https://github.com/DeviosLang/shirakami.git
cd shirakami
go build -o bin/shirakami ./cmd/analyze/
```

### 2. 启动依赖服务（PostgreSQL + Redis）

```bash
docker compose up -d
```

### 3. 运行数据库迁移

```bash
# 安装 goose
go install github.com/pressly/goose/v3/cmd/goose@latest

# 执行迁移
goose -dir migrations postgres "postgres://shirakami:shirakami@localhost:5432/shirakami?sslmode=disable" up
```

### 4. 配置文件

复制示例配置并填写参数：

```bash
cp config/shirakami.example.yaml shirakami.yaml
# 编辑 shirakami.yaml，填写 LLM API Key 等配置
```

### 5. 运行分析

```bash
# 使用 diff 文件分析
./bin/shirakami analyze --config shirakami.yaml --diff ./changes.patch

# 使用文字描述分析
./bin/shirakami analyze --config shirakami.yaml --desc "修改了支付超时重试逻辑"

# 组合模式
./bin/shirakami analyze --config shirakami.yaml --diff ./changes.patch --desc "修改支付超时重试"

# 指定输出格式（默认 terminal，可选 json / markdown）
./bin/shirakami analyze --config shirakami.yaml --diff ./changes.patch --format json
```

## 配置文件说明

```yaml
# 工作空间目录，所有 repo 将被 clone/pull 到此目录下
workspace: /tmp/shirakami-workspace

# LLM 配置（支持 OpenAI 兼容接口：OpenAI / Azure / Qwen / Claude via OpenAI proxy 等）
llm:
  endpoint: https://api.openai.com/v1   # API 基础地址
  api_key: "sk-..."                      # API Key（也可通过环境变量 SHIRAKAMI_LLM_API_KEY 设置）
  model: gpt-4o                          # 模型名称
  max_tokens: 128000                     # 最大 token 数（影响上下文管理策略）

# PostgreSQL 连接（也可通过环境变量 SHIRAKAMI_DB_DSN 设置）
db:
  dsn: postgres://user:password@localhost:5432/shirakami?sslmode=disable

# Redis 连接（也可通过环境变量 SHIRAKAMI_REDIS_ADDR 设置）
redis:
  addr: localhost:6379

# 需要分析的代码仓库列表
repos:
  - name: api-gateway           # 仓库短名（用于标识）
    url: git@github.com:org/api-gateway.git
    branch: main
    role: entry                 # 标记为业务对外入口仓库（集成测试入口从此处识别）
  - name: payment-service
    url: git@github.com:org/payment-service.git
    branch: main
  - name: order-service
    url: git@github.com:org/order-service.git
    branch: main

# 本次变更列表（支持多仓库）
changes:
  - repo: payment-service
    diff: ./diffs/payment.patch   # unified diff 文件路径
    desc: 修改支付超时重试逻辑
  - repo: order-service
    diff: ./diffs/order.patch
    desc: 更新订单状态接口
```

### 环境变量

| 环境变量 | 说明 | 对应配置字段 |
|---------|------|------------|
| `SHIRAKAMI_LLM_API_KEY` | LLM API Key | `llm.api_key` |
| `SHIRAKAMI_LLM_ENDPOINT` | LLM API 基础地址 | `llm.endpoint` |
| `SHIRAKAMI_LLM_MODEL` | 模型名称 | `llm.model` |
| `SHIRAKAMI_DB_DSN` | PostgreSQL DSN | `db.dsn` |
| `SHIRAKAMI_REDIS_ADDR` | Redis 地址 | `redis.addr` |

环境变量优先级高于配置文件。

## 输出示例

### 终端树状图（默认）

```
Shirakami Analysis Result
========================

Call Chain (Downward)
└── PaymentService.ProcessPayment (payment-service/service/payment.go:45)
    ├── PaymentRepo.Save (payment-service/repo/payment.go:120)
    │   └── db.Exec (external)
    └── OrderClient.NotifyPaid (payment-service/client/order.go:67)
        └── OrderService.UpdateStatus (order-service/service/order.go:89)

Call Chain (Upward — to entry)
└── PaymentService.ProcessPayment (payment-service)
    └── PaymentHandler.HandlePayment (api-gateway/handler/payment.go:34)  [ENTRY]
        └── Router.POST /api/v1/payments

Integration Test Entry Points
┌─────────────────────────────────────────────────────────────┐
│ Protocol: HTTP                                              │
│ Path:     POST /api/v1/payments                             │
│ Handler:  PaymentHandler.HandlePayment                      │
│                                                             │
│ Test Scenarios:                                             │
│   1. 正常支付流程（超时前完成）                                │
│   2. 支付超时后触发重试（验证重试次数上限）                      │
│   3. 重试后仍失败（验证错误响应格式）                           │
│   4. 并发支付请求（验证幂等性）                                │
└─────────────────────────────────────────────────────────────┘

Impact Summary
  Direct:   payment-service (2 functions modified)
  Indirect: order-service (1 function affected via client call)
  Cross-repo: api-gateway (entry point affected)
```

### JSON 格式

```bash
./bin/shirakami analyze --config shirakami.yaml --diff changes.patch --format json
```

```json
{
  "downward_chain": { ... },
  "upward_chains": [ ... ],
  "entry_points": [
    {
      "node": { "func_name": "PaymentHandler.HandlePayment", "repo": "api-gateway" },
      "protocol": "HTTP",
      "path": "POST /api/v1/payments",
      "test_scenarios": ["正常支付流程", "超时重试", "并发幂等性"]
    }
  ],
  "impact_summary": { ... }
}
```

## HTTP API

启动 API 服务器：

```bash
./bin/shirakami-server --config shirakami.yaml --listen :8080
```

### 接口列表

#### 提交分析任务

```
POST /analyze
Content-Type: application/json

{
  "diff": "--- a/payment.go\n+++ b/payment.go\n...",
  "desc": "修改支付超时重试逻辑"
}

Response:
{
  "task_id": "550e8400-e29b-41d4-a716-446655440000",
  "status": "pending"
}
```

#### 查询任务结果

```
GET /tasks/{task_id}

Response:
{
  "task_id": "...",
  "status": "completed",
  "result": { ... }
}
```

#### 提交反馈

```
POST /feedback
Content-Type: application/json

{
  "task_id": "550e8400-e29b-41d4-a716-446655440000",
  "type": "false_positive",
  "comment": "OrderService.UpdateStatus 实际不被调用"
}
```

#### Prometheus 指标

```
GET /metrics
```

## 架构概述

```
输入（diff / desc）
       │
       ▼
  Orchestrator
  ┌────────────────────────────────────────────────┐
  │  解析变更函数 → 并发启动 WorkerAgent            │
  │                                                │
  │  WorkerAgent × N（每个 repo 一个）              │
  │  ┌──────────────────────────────────────────┐ │
  │  │  AgentLoop (end_turn 状态机，最大100步)   │ │
  │  │  Tools:                                  │ │
  │  │    ripgrep   — 代码符号搜索               │ │
  │  │    file_read — 分层读取（3级）             │ │
  │  │    glob      — 文件模式匹配               │ │
  │  │    lsp       — gopls 调用链查询           │ │
  │  │    gitdiff   — 变更函数提取               │ │
  │  └──────────────────────────────────────────┘ │
  │                                                │
  │  Memory:                                       │
  │    Layer1: PostgreSQL 长期知识库               │
  │    Layer2: Redis 任务状态 + 断点恢复            │
  │    Layer3: System Prompt 动态注入               │
  │                                                │
  │  Token Budget Manager (ABCD 四方案):           │
  │    60% → 注入精简 reminder                     │
  │    70% → 限制文件读取级别                       │
  │    80% → 清空已分析代码块                       │
  │    92% → LLM 对话历史压缩                       │
  └────────────────────────────────────────────────┘
       │
       ▼
  Report Generator
  Terminal / JSON / Markdown
```

## 常见问题

**Q: gopls 未找到**

```bash
go install golang.org/x/tools/gopls@latest
export PATH=$PATH:$(go env GOPATH)/bin
```

**Q: PostgreSQL 连接失败**

确认 DSN 中的用户名、密码、数据库名正确，以及 PostgreSQL 服务已启动：

```bash
docker compose ps
```

**Q: 分析时 LLM 报错 "context length exceeded"**

Shirakami 内置 Token Budget Manager 自动管理上下文，如仍报错请尝试将 `llm.max_tokens` 配置调小（如 32000），使压缩策略更早触发。

**Q: 如何分析私有仓库**

SSH Key 方式：确保运行 Shirakami 的机器有访问目标仓库的 SSH Key。

Token 方式：将 URL 中的 `git@github.com:org/repo.git` 改为 `https://TOKEN@github.com/org/repo.git`。

## 开发

```bash
# 运行单元测试（不需要 Docker）
go test ./internal/agent/... ./internal/llm/... ./internal/report/... ./internal/workspace/...

# 运行集成测试（需要 Docker）
go test ./tests/... -v -count=1 -timeout=5m

# 代码检查
go vet ./...
```

## License

MIT
