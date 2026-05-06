# Shirakami

**跨仓库代码调用链分析系统** — 基于 LLM Agent Loop + 符号图索引混合架构，分析代码变更影响的完整调用链路，识别集成测试入口，生成测试场景建议。

## 功能简介

- 输入代码变更（diff / 文字描述 / 两者组合），自动分析影响的完整调用链
- 双向追踪：向下追踪实现路径 + 向上追踪到业务入口仓库
- 支持跨多个 Git 仓库的调用链分析
- **Layer A/B/C 混合分析**：纯文本 diff 解析 → 符号图索引 → LLM 补充，逐层降级，减少 LLM 调用
- **Contract Bridge**：从代码中自动扫描跨仓库 gRPC/HTTP 调用合约，写入 `contracts` 表
- 自动识别集成测试入口（HTTP / gRPC / MQ / Cron / CLI），生成测试场景建议
- 输出格式：终端树状图 / JSON / Markdown
- **Golden Test 基准框架**：人工标注的 expected.json 覆盖 Go/Python 多种场景，CI 门禁自动校验

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

# 执行迁移（含 symbol_nodes/symbol_edges/contracts 等表）
goose -dir migrations postgres "postgres://shirakami:shirakami@localhost:5432/shirakami?sslmode=disable" up
```

迁移文件说明：

| 文件 | 内容 |
|------|------|
| `001_init.sql` | tasks / task_results / feedback 基础表 |
| `002_symbol_graph.sql` | symbol_nodes / symbol_edges 符号图表 |
| `003_contracts.sql` | contracts / contract_links 跨仓库合约表 |

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

## HTTP API

启动 API 服务器：

```bash
./bin/shirakami-server --config shirakami.yaml --addr :8080
```

### 接口列表

#### 提交分析任务

```
POST /api/v1/tasks
Content-Type: application/json

{
  "input_diff": "--- a/payment.go\n+++ b/payment.go\n...",
  "input_desc": "修改支付超时重试逻辑"
}

Response 202:
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "status": "pending",
  "input_type": "combined",
  "created_at": "2024-01-01T00:00:00Z"
}
```

#### 查询任务列表

```
GET /api/v1/tasks
```

#### 查询任务结果

```
GET /api/v1/tasks/{id}

Response（completed 状态时含 call_chain / entry_points）:
{
  "id": "...",
  "status": "completed",
  "call_chain": [...],
  "entry_points": [...],
  "token_usage": 12345,
  "step_count": 42
}
```

#### 提交反馈

```
PUT /api/v1/tasks/{id}/feedback
Content-Type: application/json

{
  "type": "false_positive",
  "comment": "OrderService.UpdateStatus 实际不被调用"
}
```

type 取值：`false_positive` / `false_negative` / `correct`

#### Webhook（GitLab MR / GitHub PR 自动触发）

```
POST /api/v1/webhook
```

支持 GitLab Merge Request Hook（`X-Gitlab-Event`）和 GitHub pull_request 事件（`X-GitHub-Event`）。  
MR/PR open、update、reopen 时自动创建分析任务；close/merge 忽略。  
支持 GitLab plain-text token 和 GitHub HMAC-SHA256 签名验证。

#### Prometheus 指标

```
GET /metrics
```

#### 健康检查

```
GET /healthz
```

## 架构概述

```
输入（diff / desc）
       │
       ▼
  ┌─────────────────────────────────┐
  │  DiffToSymbols（Layer A）        │
  │  ParseDiffHunks → 精确行号定位   │
  │  纯文本解析，零 LLM 调用          │
  └──────────────┬──────────────────┘
                 │ changed_functions
                 ▼
  ┌─────────────────────────────────┐
  │  Symbol Graph（Layer B）         │
  │  symbol_nodes + symbol_edges     │
  │  WITH RECURSIVE BFS 调用链遍历   │
  │  contracts 表 — 跨仓库合约       │
  │  （Go 仓库：coverage ≥ 90%）     │
  └──────────────┬──────────────────┘
                 │ 未命中 → fallback
                 ▼
  ┌─────────────────────────────────────────────────────────┐
  │  Orchestrator + Worker（Layer C — LLM Agent Loop）       │
  │                                                         │
  │  WorkerAgent × N（每个 repo 一个）                       │
  │  ┌────────────────────────────────────────────────────┐ │
  │  │  AgentLoop (end_turn 状态机，最大 100 步)           │ │
  │  │  Tools: ripgrep / file_read / glob / lsp / gitdiff │ │
  │  └────────────────────────────────────────────────────┘ │
  │                                                         │
  │  Memory:                                                │
  │    Layer1: PostgreSQL 长期知识库                         │
  │    Layer2: Redis 任务状态 + 断点恢复                      │
  │    Layer3: System Prompt 动态注入                        │
  │                                                         │
  │  Token Budget Manager (ABCD 四方案):                    │
  │    60% → 注入精简 reminder                              │
  │    70% → 限制文件读取级别                                │
  │    80% → 清空已分析代码块                                │
  │    92% → LLM 对话历史压缩                               │
  └──────────────────────────────────────────────────────────┘
       │
       ▼
  Report Generator
  Terminal / JSON / Markdown
```

## 测试 & 质量保障

### 单元测试

```bash
# 不需要 Docker
go test ./...

# 或通过 Docker（本机无 Go 工具链时）
docker run --rm -v /mnt/shirakami:/src -w /src golang:1.25-alpine go test ./...
```

### Golden Test 基准框架

`tests/golden/` 目录维护人工标注的 golden cases，覆盖 Go / Python 多种变更场景：

| case | 难度 | 场景 |
|------|------|------|
| `go-grpc-server-worker` | ★★ | Go 新方法 + 重构 |
| `go-prometheus-counter-vec` | ★★ | 接口签名变更 + 跨文件传播 |
| `go-gin-context-json` | ★ | 方法体改动（无新函数声明） |
| `go-cache-invalidation` | ★ | 方法签名变更 + 新方法 |
| `py-fastapi-serialize-response` | ★★ | Python 函数签名扩展 |
| `py-celery-task-retry` | ★★ | MQ 任务入口 + 重试链路 |
| `cross-grpc-microservices` | ★★★ | 跨仓库 gRPC 调用 |

```bash
# 运行 Layer A 测试（纯文本 diff 解析，无 Docker）
go test ./tests/golden/... -short -v

# 运行 Layer B 测试（需要 Docker，启动 PostgreSQL 测试实例）
go test ./tests/golden/... -v -count=1

# 运行单个 case
go test ./tests/golden/... -short -run TestParseDiffHunks_GoldenCases/go-gin-context-json -v
```

Golden case 来源与设计思路详见 [`tests/golden/SOURCES.md`](tests/golden/SOURCES.md)。

### Benchmark CLI 子命令

```bash
# 遍历所有 golden cases，输出 file_recall / func_recall 汇总表
./bin/shirakami benchmark run --golden-dir tests/golden/cases

# JSON 格式输出
./bin/shirakami benchmark run --golden-dir tests/golden/cases --format json

# 单 case 详细调试（打印 miss/extra/match）
./bin/shirakami benchmark debug go-gin-context-json

# 验证 ParseDiffHunks 文件覆盖率（CI 用，<0.80 exit 1）
./bin/shirakami benchmark verify --golden-dir tests/golden/cases

# Shadow parity 人工判定 / LLM 自动判定
./bin/shirakami benchmark judge   --id <record-id> --verdict tp
./bin/shirakami benchmark autojudge --config shirakami.yaml
```

### 集成测试

```bash
# 需要 Docker（PostgreSQL + Redis via testcontainers）
go test ./tests/integration/... -v -count=1 -timeout=5m
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

**Q: Symbol Graph（Layer B）什么时候生效**

Layer B 对 Go 仓库有效（通过 `go/packages` + ripgrep 构建符号边），Python 仓库自动降级到 Layer C（LLM）。`go-cache-invalidation` 私有 case 用于测试 Layer B 自身，不对外提交。

## License

MIT
