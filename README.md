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
| `004_add_result_columns.sql` | analysis_tasks 加 modes/source_repo/queue_position；analysis_results 加 ut_suggestions/function_analyses/impact_summary 等列 |

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

#### 多 patch 联合分析（`--analysis` YAML 配置文件）

当需要同时分析多个 patch（例如关联 MR），或希望**固化业务上下文**（`extra_prompt`）以便复用，可使用独立的 analysis YAML：

```yaml
# my-analysis.yaml
source_repo: payment-service
description: "MR-1234 + MR-1235 联合影响分析"

# extra_prompt：注入到 e2e/UT 场景生成 prompt 的业务上下文（可选）
# 适合沉淀无法从代码推断的领域知识，避免每次 HTTP 请求重复填写
extra_prompt: |
  该服务通过 SM4 国密算法加密磁盘，密钥由 KMS 服务下发。
  e2e 场景必须覆盖：KMS 不可达时的降级路径、加密设备挂载后 /dev/vd* 状态验证。
  UT mock 需模拟 libvirt.open() 和 kms.GetKey() 两个外部调用。

patches:
  - path: ./diffs/mr1234.patch
    description: "修复支付超时重试逻辑"
  - path: ./diffs/mr1235.patch
    description: "更新订单状态接口"

scope:
  only_cross_repos: [order-service, api-gateway]  # 可选，只分析这些仓库的跨仓调用
```

```bash
./bin/shirakami analyze --config shirakami.yaml --analysis my-analysis.yaml
```

`extra_prompt` 字段说明：

| 用法 | 场景 |
|------|------|
| YAML `extra_prompt`（`--analysis` 文件） | **沉淀型**：业务知识固化到文件，CI / 日常分析复用 |
| HTTP API `extra_prompt`（见下文） | **即席型**：单次请求临时补充，灵活但不持久 |

两种方式的 `extra_prompt` 内容语义完全相同，均注入到 e2e 场景生成和 UT 建议 prompt 的末尾。

### 6. 构建并更新符号图索引（Layer B）

符号图索引是 Layer B 的基础，存储在 PostgreSQL `symbol_nodes` / `symbol_edges` 表中。  
**每次代码仓库更新（git pull / checkout 新分支）后，都需要同步索引**，否则 Layer B 使用旧快照，自动降级到 LLM（Layer C）兜底。

```bash
# 查看所有仓库的索引状态（CURRENT / STALE / NOT INDEXED）
./bin/shirakami index check --config shirakami.yaml

# 增量更新（只重新索引有变化的仓库，检测依据：当前 HEAD 是否与上次 indexed commit 一致）
./bin/shirakami index update --config shirakami.yaml

# 只更新特定仓库
./bin/shirakami index update --config shirakami.yaml --repo vstation_compute

# 全量重建某个仓库（先删除旧数据再完整重新索引）
./bin/shirakami index rebuild --config shirakami.yaml --repo vstation_compute
```

**索引触发时机建议：**

| 场景 | 操作 |
|------|------|
| 日常分析前（代码已 pull） | `index update`（增量，几秒内完成无变化的仓库） |
| 切换 feature 分支后 | `index update --repo <name>`（只更新那个仓库） |
| 仓库重大重构 / 符号数量异常 | `index rebuild --repo <name>`（全量重建） |
| CI 流水线 / 定期任务 | `index update`（全量增量扫描） |

**语言支持：** Go 仓库（有 `go.mod`）通过 `go/ast` + `go/types` 构建精确调用边，覆盖率 ≥ 90%。  
Python 仓库（有 `requirements.txt` / `setup.py` / `pyproject.toml`）通过 pyright 索引，自动降级至 Layer C 兜底。

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

# HTTP API Server 配置（shirakami-server 专用）
server:
  addr: ":8080"                  # 监听地址
  max_concurrent_analyses: 1     # 最大并发分析任务数（NFS 场景建议保持 1）
  default_modes:                 # 默认分析模式（不传 modes 时生效）
    - chain
    - e2e
    - ut
  webhook_secret: ""             # Webhook 验签密钥（GitLab token / GitHub HMAC key）
  gitlab_token: ""               # GitLab API Token（回写 MR 评论）
  github_token: ""               # GitHub API Token（回写 PR 评论）
```

### 环境变量

| 环境变量 | 说明 | 对应配置字段 |
|---------|------|------------|
| `SHIRAKAMI_LLM_API_KEY` | LLM API Key | `llm.api_key` |
| `SHIRAKAMI_LLM_ENDPOINT` | LLM API 基础地址 | `llm.endpoint` |
| `SHIRAKAMI_LLM_MODEL` | 模型名称 | `llm.model` |
| `SHIRAKAMI_DB_DSN` | PostgreSQL DSN | `db.dsn` |
| `SHIRAKAMI_REDIS_ADDR` | Redis 地址 | `redis.addr` |
| `SHIRAKAMI_SERVER_ADDR` | HTTP 服务监听地址（默认 `:8080`） | `server.addr` |
| `SHIRAKAMI_SERVER_MAX_CONCURRENT` | 最大并发分析任务数（默认 `1`） | `server.max_concurrent_analyses` |
| `SHIRAKAMI_SERVER_DEFAULT_MODES` | 默认分析模式（默认 `chain,e2e,ut`） | `server.default_modes` |
| `SHIRAKAMI_WEBHOOK_SECRET` | Webhook 验签密钥（GitLab token / GitHub HMAC key） | `server.webhook_secret` |
| `SHIRAKAMI_GITLAB_TOKEN` | GitLab API Token（用于 Webhook 回写 MR 评论） | `server.gitlab_token` |
| `SHIRAKAMI_GITHUB_TOKEN` | GitHub API Token（用于 Webhook 回写 PR 评论） | `server.github_token` |

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
./bin/shirakami-server --config shirakami.yaml
# 或通过环境变量指定监听地址
SHIRAKAMI_SERVER_ADDR=:8080 ./bin/shirakami-server --config shirakami.yaml
```

所有接口 BASE_URL 默认为 `http://localhost:8080`。

### 接口一览

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/healthz` | 健康检查 |
| `GET` | `/api/v1/repos` | 查询所有可用代码仓 |
| `POST` | `/api/v1/tasks` | 提交分析任务 |
| `GET` | `/api/v1/tasks` | 查询最近任务列表（最多 20 条） |
| `GET` | `/api/v1/tasks/{id}` | 查询任务状态 / 完整结果 |
| `GET` | `/api/v1/tasks/{id}/chain` | 仅返回调用链 |
| `GET` | `/api/v1/tasks/{id}/e2e` | 仅返回集成测试入口 |
| `GET` | `/api/v1/tasks/{id}/ut` | 仅返回 UT 建议 |
| `PUT` | `/api/v1/tasks/{id}/feedback` | 提交反馈 |
| `POST` | `/api/v1/webhook` | GitLab MR / GitHub PR 自动触发 |
| `GET` | `/metrics` | Prometheus 指标 |

---

#### 查询可用代码仓

在提交任务前，先查询所有已配置的仓库名称与 Git 地址对应关系：

```bash
GET /api/v1/repos

# 示例
curl http://localhost:8080/api/v1/repos
```

返回字段（每个仓库）：

| 字段 | 说明 |
|------|------|
| `name` | 仓库短名，用于 `source_repo` / `branches[].repo` |
| `branch` | 配置的 base 分支（默认 master） |
| `role` | `"entry"`（入口仓）或空 |
| `url` | Git 仓库地址（已隐匿认证信息） |
| `local_path` | NFS 上的本地克隆路径 |

---

#### 提交分析任务

```
POST /api/v1/tasks
Content-Type: application/json
```

请求字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `input_diff` | string | unified diff 原文（与 `input_branch` 二选一） |
| `input_desc` | string | 变更描述（可选，辅助 LLM 理解） |
| `input_type` | string | `"diff"` \| `"description"` \| `"combined"`（可省略，自动推断） |
| `source_repo` | string | 主变更仓库名（来自 `/api/v1/repos` 的 `name`） |
| `input_branch` | string | 功能分支名；与 `source_repo` 配合，server 自动 git fetch + three-dot diff |
| `branches` | array | 多仓多分支模式（见下），与 `input_branch` 二选一 |
| `branches[].repo` | string | 仓库名（来自 `/api/v1/repos` 的 `name`） |
| `branches[].branch` | string | 功能分支名 |
| `modes` | string[] | 分析模式，省略则全跑：`"chain"` \| `"e2e"` \| `"ut"` |
| `extra_prompt` | string | 业务上下文补充（可选）。注入到 e2e/UT 场景生成 prompt，用于补充 LLM 无法从代码推断的领域知识，例如加密算法规范、外部系统约束、测试框架要求等 |

**方式 A：直接传 diff**

```bash
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "input_diff": "--- a/payment.go\n+++ b/payment.go\n...",
    "input_desc": "修改支付超时重试逻辑",
    "source_repo": "payment-service",
    "modes": ["chain","e2e","ut"]
  }'
```

**方式 B：单仓分支（server 自动算 diff）**

```bash
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "source_repo":  "payment-service",
    "input_branch": "feature/fix-timeout"
  }'
```

**方式 C：多仓多分支联合分析**

```bash
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "branches": [
      {"repo": "payment-service", "branch": "feature/fix-timeout"},
      {"repo": "order-service",   "branch": "feature/fix-timeout"},
      {"repo": "api-gateway",     "branch": "feature/fix-timeout"}
    ],
    "modes": ["chain","e2e","ut"]
  }'
```

**方式 D：附带业务上下文（extra_prompt）**

当 LLM 无法从代码本身推断出关键领域知识时，可通过 `extra_prompt` 补充，使生成的 e2e / UT 场景更贴近实际测试要求。

```bash
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "source_repo":  "cvm_api",
    "input_branch": "feature/encrypt-disk-v2",
    "modes": ["e2e","ut"],
    "extra_prompt": "该服务使用 SM4 国密算法加密磁盘，密钥通过 KMS 加载。e2e 场景必须覆盖：1) KMS 不可达时的降级路径；2) 加密设备挂载后 /dev/vd* 状态验证；3) libvirt XML 中 encryption 字段正确写入。UT mock 需模拟 libvirt.open() 和 kms.GetKey() 两个外部调用。"
  }'
```

`extra_prompt` 使用建议：
- **领域约束**：外部系统 SLA、加密规范、幂等要求等
- **测试框架**：指定 mock 库、测试工具（如 `pytest-mock`、`gomock`）
- **验证重点**：明确列出必须验证的副作用（DB 字段、MQ 消息、日志行）
- **不要**：重复描述 diff 内容（LLM 已经看过代码），保持简洁

返回 `202 Accepted`：

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "status": "pending",
  "source_repo": "payment-service",
  "modes": ["chain","e2e","ut"],
  "created_at": "2024-01-01T00:00:00Z"
}
```

---

#### 查询任务列表

```bash
GET /api/v1/tasks
```

返回最近 20 条任务，字段同下方「查询任务结果」，不含结果详情。running 状态任务附带 `progress`（当前 step 数）。

---

#### 查询任务结果

```bash
GET /api/v1/tasks/{id}          # 完整结果
GET /api/v1/tasks/{id}/chain    # 仅调用链 + 入口
GET /api/v1/tasks/{id}/e2e      # 仅集成测试入口 + 场景
GET /api/v1/tasks/{id}/ut       # 仅 UT 建议
```

`status` 含义：

| 值 | 说明 |
|----|------|
| `pending` | 在队列中等待（`queue_position` 表示前面还有几个） |
| `running` | 分析中（`progress` 为当前 agent step 数） |
| `completed` | 完成，结果字段非空 |
| `failed` | 分析失败 |

`completed` 时的完整结果字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `call_chain` | array | 调用链节点列表 |
| `entry_points` | array | 集成测试入口列表 |
| `ut_suggestions` | string | UT 建议文本 |
| `function_analyses` | object | 函数级详细分析（含 UT 场景优先级） |
| `impact_summary` | string | 影响范围摘要 |
| `cross_repo_hops` | int | 跨仓跳转次数 |
| `risk` | string | 风险等级 |
| `token_usage` | int | 消耗 token 数 |
| `step_count` | int | agent 执行步数 |

---

#### 提交反馈

```bash
PUT /api/v1/tasks/{id}/feedback
Content-Type: application/json

{
  "type": "false_positive",
  "comment": "OrderService.UpdateStatus 实际不被调用"
}
```

`type` 取值：`correct`（结果正确）/ `false_positive`（误报）/ `false_negative`（漏报）

---

#### Webhook（GitLab MR / GitHub PR 自动触发）

```
POST /api/v1/webhook
```

支持 GitLab Merge Request Hook（`X-Gitlab-Event`）和 GitHub pull_request 事件（`X-GitHub-Event`）。  
MR/PR open、update、reopen 时自动创建分析任务；close/merge 忽略。  
支持 GitLab plain-text token 和 GitHub HMAC-SHA256 签名验证（通过 `SHIRAKAMI_WEBHOOK_SECRET` 配置）。

---

#### Prometheus 指标 / 健康检查

```bash
GET /metrics    # Prometheus 指标（内部监控用）
GET /healthz    # 健康检查，返回 200 ok
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

`tests/golden/` 目录维护人工标注的 golden cases，覆盖 Go / Python / 跨仓库多种变更场景。

**当前基线（Layer A，ParseDiffHunks 文件覆盖率）：12 个 case 全部 file_recall = 1.00**

| case | 难度 | 场景 | file_recall |
|------|------|------|-------------|
| `go-gin-context-json` | ★ | 方法体改动（无新函数声明） | 1.00 |
| `go-cache-invalidation` | ★ | 方法签名变更 + 新方法 | 1.00 |
| `shallow-config-change` | ★ | 配置文件小改动 | 1.00 |
| `single-file-utils-change` | ★ | 单文件工具函数变更 | 1.00 |
| `go-grpc-server-worker` | ★★ | Go 新方法 + 重构 | 1.00 |
| `go-prometheus-counter-vec` | ★★ | 接口签名变更 + 跨文件传播 | 1.00 |
| `py-fastapi-serialize-response` | ★★ | Python 函数签名扩展 | 1.00 |
| `py-celery-task-retry` | ★★ | MQ 任务入口 + 重试链路 | 1.00 |
| `wide-impact-common-utils` | ★★ | 公共工具函数广泛影响 | 1.00 |
| `cross-grpc-microservices` | ★★★ | 跨仓库 gRPC 调用 | 1.00 |
| `cross-repo-dispatch` | ★★★ | 跨仓库调度链路 | 1.00 |
| `compute-mr1681-encrypt-disk` | ★★★ | 真实 MR：Python 磁盘加密 + 跨仓库影响 | 1.00 |

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

Layer B 对 Go 仓库有效（通过 `go/ast` + `go/types` 构建精确调用边，覆盖率 ≥ 90%），Python 仓库自动降级到 Layer C（LLM）。

Layer B 生效的前提是**已对目标仓库建立索引**。若索引未建立或已过时（`index check` 显示 STALE），分析时自动降级到 Layer C，不影响结果正确性，但 LLM 消耗增加。

更新方式见「[构建并更新符号图索引](#6-构建并更新符号图索引layer-b)」章节。`go-cache-invalidation` 私有 case 用于测试 Layer B 自身，不对外提交。

**Q: 代码仓库切换了分支，索引需要更新吗**

需要。索引以 commit hash 为版本标识，切换分支后 HEAD 变化，`index check` 会显示 STALE。
执行 `./bin/shirakami index update --repo <name>` 即可增量更新该仓库索引（通常几十秒内完成）。

## License

MIT
