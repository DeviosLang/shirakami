# Shirakami 架构改进设计 — 借鉴 GitNexus

## 1. 背景与对比

### 1.1 两个项目的定位

| | Shirakami | GitNexus |
|---|---|---|
| 定位 | 变更影响分析（输入 diff → 输出调用链 + 测试建议） | 代码知识图谱（索引 → 图查询 → MCP 暴露给 AI Agent） |
| 核心算法 | LLM Agent Loop（end_turn 状态机）+ ripgrep | Tree-sitter AST → 符号表 → 6 阶段 Call-Resolution DAG |
| 调用链解析 | 每次 ripgrep 搜索后 LLM 判断调用方向 | 6 阶段 DAG：extract → classify → infer → dispatch → resolve → emit；应用层 BFS 逐层查询图 |
| 多语言 | 无内建语言感知（纯文本匹配） | 16 语言 tree-sitter provider + 统一 capture tags |
| 多仓库 | YAML 声明仓库列表 → Worker 并发搜索 → LLM 判断跨仓库关系 | Group 机制 + Contract Bridge（HTTP/gRPC/MQ） |
| 准确率保证 | 无（LLM 驱动，有幻觉风险） | 有 confidence 分级（0.5-1.0）；静态语言（Go/Java/C#）准确率高，动态语言（Python/Ruby）依赖启发式，tier-3 置信度仅 0.5 |
| 索引策略 | 无索引，每次实时搜索 | 预索引 → LadybugDB 图数据库 → 秒级查询 |
| LLM 使用 | 核心路径（分析全程需要 LLM） | 仅 Wiki 生成（分析本身不用 LLM） |

### 1.2 Shirakami 当前痛点

1. **成本高**：一次分析需要 10-50 次 LLM 调用（Worker × 轮次），token 消耗大
2. **准确率不稳定**：LLM 可能遗漏函数、虚构跨仓库调用、对同一 diff 给出不同结果
3. **速度慢**：多轮 LLM 调用 → 分钟级响应
4. **重复劳动**：同一仓库反复分析时，不复用历史解析结果
5. **跨仓库不可靠**：完全依赖 ripgrep 文本匹配 + LLM 判断，recall 不稳定

---

## 2. 改进架构总览

### 2.1 目标架构（三层混合模型）

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Layer 3: LLM 补充层                          │
│  处理动态调用、配置注入、跨语言桥接等确定性方法无法覆盖的场景         │
│  （保留当前 AgentLoop，但大幅减少调用频次）                          │
└────────────────────────────────────────────────────────────────┬────┘
                                                                 │ fallback
┌─────────────────────────────────────────────────────────────────────┐
│                     Layer 2: 确定性分析层                            │
│  符号图遍历 → 调用链追踪 → 跨仓库 Contract Bridge 匹配              │
│  输入: 变更符号集   输出: 确定性调用链 + confidence                   │
└────────────────────────────────────────────────────────────────┬────┘
                                                                 │ provides
┌─────────────────────────────────────────────────────────────────────┐
│                      Layer 1: 索引基础层                             │
│  AST 解析 → 符号定义/引用 → 调用关系 → 继承关系 → 持久化             │
│  Go: go/ast + go/types (标准库) / Python: tree-sitter / fallback: rg │
└─────────────────────────────────────────────────────────────────────┘
```

> **注：** Python 仓库的 Layer 2 职责为"辅助 LLM"（提供 import graph + 符号位置作为上下文，
> 减少搜索轮次），而非"替代 LLM"。仅 Go 仓库的 Layer 2 能实现完整确定性调用链追踪。

### 2.2 数据流改进

**当前（v1）**：
```
diff → [LLM extract functions] → [LLM+ripgrep trace per-Worker] → [LLM merge] → report
         ↑ 每步都需要 LLM                  ↑ 多轮 LLM 调用
```

**目标（v2）**：
```
diff → [Go 解析 diff hunks] → [索引查询: line→symbol] → [图遍历: callers/callees]
         ↑ 零 LLM                  ↑ 零 LLM                  ↑ 零 LLM
                                                                    │
     ┌──────────────────────────────────────────────────────────────┘
     │ 仅对以下场景启用 LLM:
     │   1. 动态调用/反射 (字符串拼接的函数名)
     │   2. 配置注入 (YAML/JSON 中的 handler 注册)
     │   3. 索引未覆盖的仓库/文件
     │   4. 测试场景生成 (保留现有 UT/scenario follow-up)
     ▼
[LLM 补充: 仅处理 uncovered 部分] → report
```

---

## 3. 详细设计

### 3.1 Module: `internal/index` — 符号索引

#### 3.1.1 职责

- 从源代码提取符号定义（函数、方法、类、常量）
- 建立调用关系（谁调用谁、谁导入谁）
- 建立继承关系（类继承、接口实现）
- 持久化到数据库，支持增量更新

#### 3.1.2 数据模型

```go
// SymbolNode 表示一个代码符号
type SymbolNode struct {
    // ID 构造规则（防碰撞）:
    //   基础: "{repo}:{file}:{qualified_name}#{arity}"
    //     其中 qualified_name = 类名.方法名，如 "PaymentService.Handle"
    //     对顶层函数 qualified_name = 函数名本身
    //   碰撞（同 qualified_name + arity 存在多个）时追加类型哈希:
    //     "{repo}:{file}:{qualified_name}#{arity}~{param_type_hash}"
    //   最终 tiebreaker（如嵌套同名函数）: 追加 "@{start_line}"
    //
    // 注: Python 无类型注解时退化为 arity-only，接受部分碰撞（返回多个候选）
    ID         string    // 全局唯一 ID（见上方构造规则）
    Repo       string    // 所属仓库
    File       string    // 文件路径（仓库内相对路径）
    Name       string    // 符号名称（qualified name，含类名前缀）
    Kind       string    // function / method / class / interface / constant
    StartLine  int       // 定义起始行
    EndLine    int       // 定义结束行
    Signature  string    // 函数签名（参数列表）
    CommitHash string    // 索引时的 HEAD commit
    IndexedAt  time.Time
}

// SymbolEdge 表示两个符号间的关系
type SymbolEdge struct {
    ID         string
    SourceID   string    // 调用方/导入方
    TargetID   string    // 被调用方/被导入方
    Type       string    // CALLS / IMPORTS / EXTENDS / IMPLEMENTS
    File       string    // 发生关系的文件
    Line       int       // 发生关系的行号
    Confidence float64   // 0-1.0 置信度
}
```

#### 3.1.3 索引策略

```
索引触发时机:
  1. `shirakami workspace sync` 时（git pull 后检测 HEAD 变化）
  2. 首次分析某个仓库时
  3. 手动 `shirakami index --repo <name>`

增量更新:
  - 比对 HEAD commit 变化
  - 仅重新解析变更文件
  - 删除旧文件的符号和边，插入新的
  - 参考 GitNexus: "Early exit if lastCommit == HEAD"

索引源（按语言分层，能力不同）:

  Go 仓库（确定性层 = 生产级，confidence 1.0）:
    工具: go/ast + go/types + go/packages（标准库，零 CGO，零外部依赖）
    能力:
      - go/packages.Load() 获取完整类型信息（含跨文件、跨包）
      - go/ast 遍历 CallExpr / SelectorExpr 提取调用关系
      - go/types.Implements() 自动推导 interface → struct 实现关系
      - go/types.Uses 精确定位每个符号引用的定义位置
    覆盖率: ~99%（仅 reflect/unsafe/go:generate 等极少数场景缺失）
    为什么不用 tree-sitter:
      - Go 已有标准库级别的带类型 AST 解析，比 tree-sitter 更准确
      - tree-sitter 的 Go bindings 需要 CGO，在 Alpine Docker 中有编译问题
      - tree-sitter 的优势是"跨语言统一 API"，但 Shirakami 仅需 Go + Python

  Python 仓库（确定性层 = 辅助级，不替代 LLM）:
    策略: 不走"确定性替代 LLM"路线，改为"给 LLM 更好的起点"
    工具: tree-sitter-python（WASM 或 Python binding via subprocess）
    能力:
      - import 关系提取（确定性，覆盖率 ~90%）
      - 函数/类定义位置（确定性）
      - 装饰器识别（@app.route / @router.post 等）
    不做:
      - 动态调用链解析（getattr/dispatch/反射 — 留给 LLM）
      - 类型推导（Python 无静态类型系统）
    使用方式:
      把 import graph + 符号位置作为"已知上下文"注入 Worker prompt
      → 减少 LLM 搜索轮次（从 5-10 轮降到 1-2 轮）
      → 但保留 LLM 判断动态调用链的能力
    覆盖率: import graph ~90%, call graph ~30%（其余由 LLM 补充）

  gopls 的定位:
    保留现有用途：对 diff 中变更的少量函数做按需 callHierarchy 查询。
    不用于批量全仓库索引（LSP 协议不支持批量导出）。

  通用 fallback: ripgrep + 启发式函数定义匹配（索引失败时降级）

索引陈旧检测与处理:
  每次 analyze 启动时检查: symbol_nodes.commit_hash vs 当前 git HEAD
  - 一致: 正常使用索引
  - 不一致（索引过时）:
    MVP 阶段 — 选项 C: 使用过时索引 + 报告标注警告
      报告末尾附: "[警告: 索引过时 indexed@abc123, HEAD@def456，行号可能偏移]"
      DiffToSymbols 的行号匹配可能出现 ±N 行偏移，但仍比无索引好
    后续升级 — 选项 A: 自动增量重索引
      analyze 启动时检测到 HEAD 变化 → 自动执行增量索引（仅变更文件）
      增量索引耗时 < 10s（通常仅几个文件），不显著影响分析延迟
      通过 --no-auto-reindex flag 可禁用（CI 环境索引由 workspace sync 管理）
  - 索引完全不存在:
    降级到纯 LLM 模式（§3.5 Orchestrator fallback 路径）
    日志输出: "索引未建立，使用 LLM 模式。运行 shirakami index --repo <name> 加速后续分析"
```

#### 3.1.4 存储 Schema (PostgreSQL)

```sql
CREATE TABLE symbol_nodes (
    id          TEXT PRIMARY KEY,
    repo        TEXT NOT NULL,
    file_path   TEXT NOT NULL,
    name        TEXT NOT NULL,
    kind        TEXT NOT NULL,  -- function/method/class/interface
    start_line  INT NOT NULL,
    end_line    INT NOT NULL,
    signature   TEXT,
    commit_hash TEXT NOT NULL,
    indexed_at  TIMESTAMPTZ DEFAULT NOW(),
    
    -- 用于 diff→符号 映射的高效查询
    CONSTRAINT idx_file_lines UNIQUE (repo, file_path, start_line, end_line)
);

CREATE INDEX idx_symbol_repo_name ON symbol_nodes(repo, name);
CREATE INDEX idx_symbol_file ON symbol_nodes(repo, file_path);

CREATE TABLE symbol_edges (
    id          TEXT PRIMARY KEY,
    source_id   TEXT REFERENCES symbol_nodes(id) ON DELETE CASCADE,
    target_id   TEXT REFERENCES symbol_nodes(id) ON DELETE CASCADE,
    type        TEXT NOT NULL,  -- CALLS/IMPORTS/EXTENDS/IMPLEMENTS
    file_path   TEXT,
    line        INT,
    confidence  REAL DEFAULT 1.0,
    
    UNIQUE(source_id, target_id, type)
);

CREATE INDEX idx_edge_target ON symbol_edges(target_id, type);  -- 查找 callers
CREATE INDEX idx_edge_source ON symbol_edges(source_id, type);  -- 查找 callees
```

#### 3.1.5 索引覆盖率监控

每次索引完成后记录覆盖率指标，防止 tree-sitter 解析失败时系统静默降级
而用户完全不感知。

```go
// IndexCoverageStats 每次索引完成后写入 DB 并暴露为 Prometheus gauge
type IndexCoverageStats struct {
    Repo           string
    Language       string
    TotalFiles     int
    IndexedSymbols int     // 成功提取的符号数
    FallbackFiles  int     // tree-sitter 失败、退化到 ripgrep 的文件数
    CoverageRatio  float64 // IndexedSymbols / EstimatedTotal
    IndexedAt      time.Time
}
```

Prometheus 指标（复用现有 `internal/feedback/` 的 metrics 体系）：

```
shirakami_index_coverage{repo="payments", language="go"}    0.97
shirakami_index_fallback_files{repo="payments", language="python"} 12
```

分析报告末尾附注覆盖率：

```
[索引覆盖率] payments-service: 确定性覆盖 87%，13% 符号由 LLM 补充
             Go: 97% | Python: 71%（动态特性限制）
```

---

### 3.2 Module: `internal/resolve` — 确定性调用链解析

#### 3.2.1 职责

- 从变更符号出发，沿图遍历所有 caller 链路
- 支持 upstream（谁调用了我）和 downstream（我调用了谁）双向
- 跨仓库时查 Contract Bridge
- 输出带 depth 和 confidence 的影响范围

#### 3.2.2 算法

```go
// Impact 从 target 符号出发，BFS 遍历 caller 图
func (r *Resolver) Impact(ctx context.Context, opts ImpactOptions) (*ImpactResult, error) {
    // 1. Resolve target: name → symbolNode (可能有多个同名，用 file_path 消歧)
    // 2. BFS/DFS 遍历:
    //    - depth 1: 直接 callers (WILL BREAK)
    //    - depth 2: 间接 callers (LIKELY AFFECTED)
    //    - depth 3: 传递 callers (MAY NEED TESTING)
    // 3. 到达 entry-role 仓库时标记为 entry_point
    // 4. 跨仓库边: 查 contract bridge
    // 5. 返回按 depth 分组的结果 + risk 评估
}

type ImpactOptions struct {
    Target        string   // 符号名
    Repo          string   // 仓库名
    Direction     string   // upstream / downstream
    MaxDepth      int      // 默认 3
    RelationTypes []string // CALLS, IMPORTS, EXTENDS, IMPLEMENTS
    MinConfidence float64  // 最低置信度过滤
}

type ImpactResult struct {
    Risk           string          // LOW / MEDIUM / HIGH / CRITICAL
    DirectCount    int             // depth=1 数量
    TotalAffected  int
    ByDepth        map[int][]AffectedSymbol
    EntryPoints    []EntryPoint
    CrossRepoHops  []CrossRepoHop
    Uncovered      []string        // 索引未覆盖的符号（需 LLM 补充）
}
```

#### 3.2.3 Risk 评估规则

借鉴 GitNexus 的 impact tool 输出：

```
CRITICAL: depth=1 callers > 20 OR 影响 entry-role 仓库
HIGH:     depth=1 callers > 10 OR 跨 2+ 仓库
MEDIUM:   depth=1 callers > 3
LOW:      depth=1 callers ≤ 3 且无跨仓库影响
```

---

### 3.3 Module: `internal/contract` — 跨仓库 Contract Bridge

#### 3.3.1 概念模型

借鉴 GitNexus 的 Group 机制，但简化为适合 Shirakami 场景：

```
Contract = 仓库间的调用契约

Provider（提供方）:
  - HTTP route handler: POST /api/v1/payments → PaymentHandler.HandlePayment
  - gRPC service method: PaymentService.ProcessPayment
  - MQ consumer: topic "payment.completed" → OrderService.OnPaymentDone

Consumer（消费方）:
  - HTTP client call: requests.post("/api/v1/payments")
  - gRPC client stub: payment_client.ProcessPayment()
  - MQ publisher: publish("payment.completed", msg)
```

#### 3.3.2 提取策略（双策略，借鉴 GitNexus Pipeline）

```
Strategy A — 索引驱动（优先）:
  查询 symbol_edges 中 type=HANDLES_ROUTE / PUBLISHES / SUBSCRIBES 的边
  
Strategy B — 源码扫描（索引不足时）:
  - Python: tree-sitter 解析装饰器 (@app.route / @router.post)
  - Go: 正则匹配 router.Handle / mux.HandleFunc
  - 通用: ripgrep 搜索 "def handler" / "POST" / "subscribe" 等模式
```

#### 3.3.3 匹配算法

```go
type Contract struct {
    ID       string
    Repo     string
    Role     string  // "provider" / "consumer"
    Protocol string  // "http" / "grpc" / "mq"
    Path     string  // "/api/v1/payments" 或 "payment.completed"
    Method   string  // "POST" / "ProcessPayment" / ""
    Symbol   string  // 关联的 symbol_node ID
}

// MatchType 降级链（优先级从高到低）
// exact → prefix → wildcard → 放弃
// 每种 matchType 对应不同的 confidence 初始值
type MatchType string
const (
    MatchTypeExact    MatchType = "exact"    // confidence: 1.0
    MatchTypePrefix   MatchType = "prefix"   // confidence: 0.8
    MatchTypeWildcard MatchType = "wildcard" // confidence: 0.6
)

// NormalizeContractID 在匹配前统一规范化契约路径
// 必须在所有匹配逻辑之前执行：
//   HTTP:  参数统一为 {param}（兼容 :id / {id} / {userId}）
//          去尾部斜线，method 大写
//          例: "post::/api/v1/users/:id/" → "POST::/api/v1/users/{param}"
//   gRPC:  package/service 名小写，method 名保留大小写
//          例: "grpc::Auth.AuthService/Login" → "grpc::auth.authservice/Login"
//   Topic: 统一小写
func NormalizeContractID(id string) string { ... }

// IsNoisyContract 过滤产生 N×M 假链接的噪声路由
// 类目 1: 公共健康检查端点（每个服务都有，会产生大量误匹配）
//   默认过滤: /health /ping /ready /live /metrics /favicon.ico
//   可通过 contract.exclude_paths 配置追加
// 类目 2: 全参数路由（/{param} /{param}/{param} 无业务语义）
//   可通过 contract.exclude_param_only_paths: true 开启过滤
func IsNoisyContract(c *Contract, cfg ContractMatchConfig) bool { ... }

// Match 返回 provider-consumer 配对
// 执行顺序: 规范化 → 噪声过滤 → 精确匹配 → 前缀匹配 → 通配匹配 → 去重
func (r *Registry) Match(cfg ContractMatchConfig) []ContractLink {
    // Step 1: 规范化所有契约 ID
    // Step 2: 过滤噪声路由（避免 N×M 假链接）
    // Step 3: 精确匹配 (path + method 完全一致) → confidence 1.0
    // Step 4: 前缀匹配 (path 前缀 + method 兼容) → confidence 0.8
    // Step 5: 通配匹配 (gRPC wildcard /*) → confidence 0.6
    // Step 6: 去重（同一 consumer+provider+type 三元组保留最高 confidence）
}

// dedupe 去重逻辑
// 同一 (consumer_symbol, provider_symbol, type) 可能被多个提取器各发现一次
// 写入 contract_links 前按该三元组去重，保留最高 confidence 的记录
func dedupe(links []ContractLink) []ContractLink { ... }
```

配置项（新增到 `shirakami.yaml`）：

```yaml
contract:
  exclude_paths:               # 从匹配中排除的 HTTP 路径（健康检查等）
    - /health
    - /ping
    - /ready
    - /metrics
  exclude_param_only_paths: true  # 过滤全参数路由如 /{param}/{param}
```

#### 3.3.4 存储

```sql
CREATE TABLE contracts (
    id         TEXT PRIMARY KEY,
    repo       TEXT NOT NULL,
    role       TEXT NOT NULL,   -- provider / consumer
    protocol   TEXT NOT NULL,   -- http / grpc / mq
    path       TEXT NOT NULL,   -- route path or topic name（已规范化）
    method     TEXT,            -- HTTP method or RPC method name
    symbol_id  TEXT REFERENCES symbol_nodes(id),
    commit_hash TEXT NOT NULL,
    
    UNIQUE(repo, protocol, path, method, role)
);

CREATE TABLE contract_links (
    id              TEXT PRIMARY KEY,
    provider_id     TEXT REFERENCES contracts(id),
    consumer_id     TEXT REFERENCES contracts(id),
    confidence      REAL DEFAULT 1.0,
    match_type      TEXT,  -- exact / prefix / wildcard
    -- 同一 (provider_id, consumer_id, match_type) 三元组保证唯一
    -- 去重逻辑在应用层执行，保留最高 confidence 的记录
    UNIQUE(provider_id, consumer_id, match_type)
);
```

---

### 3.4 改进: `internal/tool/gitdiff.go` — 确定性 Diff 解析

#### 3.4.1 当前问题

`orchestrator.extractChangedFunctions` 使用 LLM 解析 diff → 有遗漏/幻觉。

#### 3.4.2 新增函数（两层，渐进实现）

**Layer A — 纯 diff 解析（MVP，无外部依赖）：**

```go
// parseDiffHunks 解析 unified diff 格式，提取变更行号范围
// 纯文本操作，不依赖索引、不依赖 DB
// MVP 阶段即可替代 LLM 的 extractChangedFunctions 中"识别变更文件+行号"的部分
func parseDiffHunks(diff string) []DiffHunk {
    // 解析 @@ -oldStart,oldCount +newStart,newCount @@ 行
    // 仅关注 new side (+ 行) 的范围
    // 返回: [{file: "service/payment.go", startLine: 45, endLine: 67}]
}

// DiffHunk 表示一段变更
type DiffHunk struct {
    File      string // 文件路径（从 +++ b/path 提取）
    StartLine int    // 变更起始行号（new side）
    EndLine   int    // 变更结束行号（new side）
}
```

**Layer B — 行号→符号映射（Week 3+，需要索引就绪）：**

```go
// DiffToSymbols 将 diff hunks 精确映射到已索引的符号
// 依赖 symbol_nodes 表（需要索引已构建）
func DiffToSymbols(hunks []DiffHunk, db *pgxpool.Pool, sourceRepo string) (matched []SymbolNode, uncovered []DiffHunk, err error) {
    // 对每个 hunk，查询索引找到覆盖这些行的符号:
    // SELECT * FROM symbol_nodes
    // WHERE repo = $1 AND file_path = $2
    //   AND start_line <= $3 AND end_line >= $4
    //
    // matched: 索引中有对应符号的 hunks
    // uncovered: 索引中未找到符号的 hunks（需 LLM 补充）
}
```

#### 3.4.3 集成点

修改 `orchestrator.go` 的 `extractChangedFunctions`:

```go
func (o *Orchestrator) extractChangedFunctions(ctx context.Context, input AnalysisInput) ([]string, error) {
    // Layer A: 纯文本解析 diff hunks（始终可用，MVP 即生效）
    hunks := parseDiffHunks(input.Diff)
    
    // Layer B: 如果有索引，做行号→符号精确映射
    if o.symbolIndex != nil {
        matched, uncovered, _ := DiffToSymbols(hunks, o.db, input.SourceRepo)
        if len(uncovered) == 0 {
            return symbolsToFuncList(matched), nil  // 完全覆盖，无需 LLM
        }
        // 部分覆盖: 确定性结果 + LLM 补充 uncovered 部分
        llmResult := o.extractViaLLM(ctx, hunksToPartialDiff(input.Diff, uncovered))
        return merge(symbolsToFuncList(matched), llmResult), nil
    }
    
    // 无索引时: 用 hunks 提取文件列表 + 行号范围，辅助 LLM 解析
    // （比纯 LLM 更好：LLM 只需确认"这些行属于哪个函数"，而非从零解析 diff）
    return o.extractViaLLMWithHints(ctx, input, hunks)
}
```

---

### 3.5 改进: Orchestrator 混合模式

#### 3.5.1 新 Run 流程

```go
func (o *Orchestrator) Run(ctx context.Context, input AnalysisInput) (*AnalysisOutput, error) {
    // Step 1 — 确定性 diff 解析（替代 LLM extract）
    changed := o.extractChangedFunctions(ctx, input)  // 优先用索引
    
    // Step 2 — 确定性图遍历（替代 Worker LLM 调用）
    if o.resolver != nil {
        // Phase A: 图遍历获取确定性调用链
        deterministicResult := o.resolver.Impact(ctx, ImpactOptions{...})
        
        // Phase B: 跨仓库部分查 Contract Bridge
        crossRepo := o.contractBridge.FindCrossRepoCalls(deterministicResult.EntryPoints)
        
        // Phase C: 标记索引未覆盖的部分
        uncovered := deterministicResult.Uncovered
        
        // Phase D: 仅对 uncovered 部分启动 LLM Worker
        if len(uncovered) > 0 {
            llmResult := o.runLLMWorkers(ctx, uncovered)
            return merge(deterministicResult, crossRepo, llmResult), nil
        }
        return merge(deterministicResult, crossRepo), nil
    }
    
    // Fallback: 完整 LLM 模式（当前行为，保持兼容）
    return o.runFullLLMMode(ctx, input)
}
```

---

## 4. 迁移策略

### 4.1 阶段划分

```
Phase 0 (当前): 纯 LLM 模式
    ↓ 添加 --index-mode=off|shadow|hybrid|deterministic flag
Phase 1: Shadow Mode（并行对比，质量验证）
    - 对同一 diff 同时运行：确定性层 + LLM 层
    - 记录两者差异（确定性层有但 LLM 没有 / 反之），写入日志
    - 对外仍输出 LLM 结果（确定性层作为 shadow，不影响用户）
    - 积累 false negative / false positive 样本，建立 golden test cases
    晋级标准（才能进入 Phase 2）:
      1. [准确率]  在 golden test cases 上 Recall ≥ 0.90（对 ground truth）
      2. [精确率]  在 golden test cases 上 Precision ≥ 0.85（不能制造太多误报）
      3. [回归防护] Shadow MissRate ≤ 10%（不能比 v1 遗漏更多真实 caller）
      4. [稳定性]  连续 5 次分析任务上述指标无下降趋势
Phase 2: 混合模式默认开启
    - 有索引时用确定性分析
    - 无索引时 fallback 到 LLM
    - CLI flag: --no-index 跳过索引
Phase 3: 确定性模式为主
    - LLM 仅用于测试场景生成和 uncovered 补充
    - 目标: LLM 调用减少 80%+
```

CLI 标志说明：

| 标志 | 行为 |
|------|------|
| `--index-mode=off` | 纯 LLM 模式（当前行为） |
| `--index-mode=shadow` | 并行运行两层，输出差异报告，结果以 LLM 为准 |
| `--index-mode=hybrid` | 有索引用确定性，无索引用 LLM（Phase 2 默认） |
| `--index-mode=deterministic` | 仅确定性层，uncovered 用 LLM 补充 |

### 4.2 兼容性

- 所有改进均为 additive，不破坏现有接口
- `shirakami analyze` 命令参数不变
- 报告格式不变（`AnalysisOutput` schema 保持）
- 仅新增 `shirakami index` 子命令

---

## 5. 预期收益

| 指标 | 当前 (v1) | 目标 (v2) |
|------|-----------|-----------|
| LLM 调用次数/分析 | 10-50 次 | 0-5 次 |
| Token 消耗/分析 | 50K-200K | 5K-30K |
| 分析耗时（首次） | 3-10 分钟 | 30-90 秒 |
| 分析耗时（已索引） | 3-10 分钟 | 5-15 秒 |
| 调用链准确率（Go） | ~70%（依赖 LLM） | ~95%（go/ast + go/types，编译器级准确率）|
| 调用链准确率（Python） | ~70%（依赖 LLM） | ~70-80%（动态特性限制，LLM 补充）|
| 跨仓库 recall | ~60% | ~90%（Contract Bridge） |
| 结果可复现性 | 低（LLM 随机性） | 高（确定性层 100% 可复现，LLM 补充层不保证）|

---

## 6. 风险与取舍

| 风险 | 影响 | 缓解 |
|------|------|------|
| tree-sitter-python 解析覆盖不全 | Python 仓库部分符号未建索引 | 通过 §3.1.5 覆盖率监控感知，fallback 到 ripgrep；Python 确定性层仅做辅助（§3.1.3） |
| Python 动态特性覆盖不全 | 部分调用链缺失 | Python 确定性层定位为"辅助 LLM"（仅提取 import + 符号位置）；动态调用链由 LLM 处理 |
| Contract 提取遗漏 | 跨仓库关系不完整 | YAML 手动声明 fallback（类似 GitNexus manifest）；噪声过滤防误报 |
| 索引存储空间 | PostgreSQL 数据量增加 | 仅存有意义的符号（跳过 test / vendor）；`WITH RECURSIVE` 深度 ≤ 3 在 50 万条边以内均毫秒级响应 |
| 迁移期双模式维护 | 代码复杂度增加 | Shadow Mode 阶段验证质量，达标后再切换；明确 feature flag |
| PostgreSQL 图遍历性能（超大仓库） | 超过 50 万条 symbol_edges 时性能下降 | MaxDepth 降至 2；或对 symbol_edges 按 repo 分区；Apache AGE 作为未来可选项（当前不引入） |

### 图遍历实现说明

**Phase 1（推荐）：内存邻接表 + PostgreSQL 持久化**

查询时使用纯内存 BFS，PostgreSQL 仅作持久化存储：

```go
// internal/resolve/graph.go — 纯 Go 内存图，零外部依赖

type InMemoryGraph struct {
    nodes    map[string]*SymbolNode
    inEdges  map[string][]*SymbolEdge  // target → []source（找 callers）
    outEdges map[string][]*SymbolEdge  // source → []target（找 callees）
}

// 启动时加载: SELECT * FROM symbol_edges WHERE repo IN (...)
// 增量更新时: 修改内存 + 写 DB

// BFS — 纯内存操作，微秒级
func (g *InMemoryGraph) Impact(startID string, direction string, maxDepth int) []AffectedSymbol {
    visited := make(map[string]bool)
    queue := []bfsItem{{id: startID, depth: 0}}
    var result []AffectedSymbol
    for len(queue) > 0 {
        item := queue[0]; queue = queue[1:]
        if visited[item.id] || item.depth > maxDepth { continue }
        visited[item.id] = true
        edges := g.inEdges[item.id] // upstream
        for _, e := range edges {
            result = append(result, AffectedSymbol{ID: e.SourceID, Depth: item.depth + 1})
            queue = append(queue, bfsItem{id: e.SourceID, depth: item.depth + 1})
        }
    }
    return result
}
```

内存开销估算：
- 50 万条边 × (~200 bytes/edge + 指针) ≈ 150-200 MB RAM
- BFS depth=3 ≈ < 1ms（内存 map 查找）
- 远超 PostgreSQL WITH RECURSIVE（10-100ms）

**Phase 2（大数据集备用）：PostgreSQL `WITH RECURSIVE`**

当内存受限或需要支持 ad-hoc SQL 查询时：

```sql
-- 查找 $symbol 在 $repo 中所有 upstream callers（深度 ≤ 3）
WITH RECURSIVE impact(id, depth, path) AS (
  -- seed: depth=1 直接调用者
  SELECT e.source_id, 1, ARRAY[e.source_id]
  FROM symbol_edges e
  JOIN symbol_nodes n ON n.id = e.target_id
  WHERE n.name = $1 AND n.repo = $2
    AND e.type = 'CALLS'

  UNION ALL

  -- 递归展开
  SELECT e.source_id, i.depth + 1, i.path || e.source_id
  FROM symbol_edges e
  JOIN impact i ON i.id = e.target_id
  WHERE i.depth < 3
    AND e.source_id <> ALL(i.path)   -- 防环
    AND e.type = 'CALLS'
)
SELECT id, MIN(depth) AS min_depth
FROM impact
GROUP BY id;
```

性能边界（`idx_edge_target` 索引覆盖）：
- 10 万条边，MaxDepth=3：< 10ms
- 50 万条边，MaxDepth=3：< 100ms
- 超过 50 万条边时建议 MaxDepth 降至 2

**不使用 LadybugDB 的原因：** LadybugDB 是纯 npm 包（`@ladybugdb/core`），无 Go 绑定；
GitNexus 的实际实现也是应用层 for 循环逐层查询，并非原生图遍历；
Shirakami 已有 PostgreSQL，`WITH RECURSIVE` 零额外依赖。

**Apache AGE**：当前不引入。待功能稳定后，如需对外暴露 Cypher 查询接口（如 MCP tool），再评估作为查询层叠加。

---

## 7. 对现有代码的影响

### 7.1 新增目录

```
internal/index/       # 符号索引（新）
internal/resolve/     # 确定性调用链解析（新）
internal/contract/    # 跨仓库 Contract Bridge（新）
migrations/002_symbol_graph.sql  # 新表
migrations/003_contracts.sql     # 新表
```

### 7.2 修改文件

| 文件 | 改动 |
|------|------|
| `internal/agent/orchestrator.go` | 新增索引查询路径（hybrid mode）+ shadow mode 差异记录 |
| `internal/tool/gitdiff.go` | 新增 `DiffToSymbols` 确定性解析 |
| `internal/tool/lsp.go` | 保留现有按需 callHierarchy 用途，不扩展为批量索引数据源 |
| `cmd/analyze/main.go` | 新增 `--index-mode` flag（off/shadow/hybrid/deterministic）+ 注入 resolver；新增 `benchmark` 子命令（run/debug/perf） |
| `internal/config/config.go` | 新增 index / contract 相关配置（含 exclude_paths 等） |
| `pkg/schema/result.go` | 新增 `Confidence` / `Depth` / `IndexCoverage` 字段 |

### 7.3 不变的部分

- `internal/agent/loop.go` — AgentLoop 状态机保持不变
- `internal/compress/` — Token Budget Manager 保持不变
- `internal/memory/` — 三层 Memory 保持不变
- `internal/report/` — 报告格式保持兼容
- `tests/` — 现有测试保持通过

---

## 8. 基准测试体系

> 借鉴 GitNexus 的两套验证体系：Shadow Parity Harness（代码级准确率逐 call site 对比）和 SWE-bench Evaluation Harness（端到端效果评测）。

### 8.1 设计目标

| 验证层级 | 问题 | 方法 |
|----------|------|------|
| 符号级准确率 | 确定性层是否找对了调用链？ | Golden Test Cases + Shadow Parity |
| 端到端效果 | v2 分析结果对用户是否更有用？ | 真实 diff 对比评测 |
| 性能基线 | 各环节耗时是否达标？ | 性能基准 + 回归监控 |
| 迁移安全 | 切换模式后旧功能是否保留？ | CI 门禁 |

### 8.2 Golden Test Cases（Ground Truth 语料库）

#### 8.2.1 构建方式

利用 Shirakami v1（LLM 模式）的历史分析结果 + 人工标注，建立 ground truth 数据集：

```
tests/golden/
├── cases/
│   ├── payment-timeout-retry/
│   │   ├── input.patch          # unified diff 输入
│   │   ├── input.yaml           # 分析配置（repos, source_repo）
│   │   ├── expected.json        # 标注的正确结果
│   │   ├── fixtures.sql         # 该 case 需要的 symbol_nodes / symbol_edges 预设数据
│   │   └── metadata.json        # 标注者、日期、难度分级
│   ├── order-status-update/
│   │   └── ...
│   └── cross-repo-encrypt-disk/
│       └── ...
├── schema.json                  # expected.json 的 JSON Schema（含 symbol_id 正则约束）
└── README.md                    # 标注规范
```

#### 8.2.2 expected.json 结构

```json
{
  "changed_functions": [
    {
      "name": "PaymentService.ProcessPayment",
      "repo": "payment-service",
      "file": "service/payment.go",
      "start_line": 45,
      "symbol_id": "payment-service:service/payment.go:PaymentService.ProcessPayment#1"
    }
  ],
  "call_chain": [
    {
      "source": "payment-service:service/payment.go:PaymentService.ProcessPayment#1",
      "target": "payment-service:repo/payment.go:PaymentRepository.Save#1",
      "type": "CALLS",
      "depth": 1,
      "confidence": 1.0
    }
  ],
  "entry_points": [
    {
      "repo": "api-gateway",
      "function": "PaymentHandler.HandlePayment",
      "file": "handler/payment.go",
      "symbol_id": "api-gateway:handler/payment.go:PaymentHandler.HandlePayment#1",
      "protocol": "HTTP",
      "path": "POST /api/v1/payments",
      "depth": 2
    }
  ],
  "cross_repo_calls": [
    {
      "from_repo": "payment-service",
      "to_repo": "api-gateway",
      "function": "PaymentHandler.HandlePayment",
      "symbol_id": "api-gateway:handler/payment.go:PaymentHandler.HandlePayment#1",
      "confidence": 1.0
    }
  ]
}
```

**注意：** `symbol_id` 字段须严格遵循 §3.1.2 的格式 `{repo}:{file}:{qualified_name}#{arity}`。  
`schema.json` 中用正则约束：`"pattern": "^[^:]+:[^:]+:[^#]+#\\d+"`。  
`call_chain[].depth` 对应 `ImpactResult.ByDepth` 的层级（1=WILL BREAK，2=LIKELY AFFECTED，3=MAY NEED TESTING），  
Golden runner 在比较时应验证 depth 正确性，不能把 depth=3 的传递调用误报为 depth=1 的直接调用。

#### 8.2.3 标注流程

```
1. 选取有代表性的历史 diff（覆盖：单仓库/跨仓库/深链路/wide-impact）
2. 运行 v1 LLM 模式获取初步结果
3. 人工审核 + 修正（标记 false positive / false negative）
   注意: v1 LLM 的输出存在锚定偏差——标注者容易认可 LLM 的结果，
   需主动补充 v1 遗漏的调用（这些后来如果被 v2 找到，会出现在 extra_tp 中，
   届时应反向更新 golden cases）
4. 为每个 case 编写 fixtures.sql（预建 symbol_nodes + symbol_edges 测试数据，
   覆盖该 diff 涉及的全部符号关系，供 golden runner 加载到临时 PostgreSQL）
5. 保存为 golden case
6. 每积累 10 个 case 提交一次 PR（tests/golden/ 纳入 git）
```

目标：初期 20-30 个 golden cases，覆盖主要场景分类。

---

### 8.3 Shadow Parity（v1 vs v2 对比验证）

借鉴 GitNexus 的 Shadow Parity Harness，在 `--index-mode=shadow` 阶段实现：

#### 8.3.1 机制

```go
// internal/benchmark/shadow.go

// ShadowRecord 记录单个符号的新旧两条路径结果
type ShadowRecord struct {
    Symbol       string          // 被分析的符号
    Repo         string
    LegacyResult *WorkerResult   // v1 LLM 路径的结果
    NewResult    *ImpactResult   // v2 确定性路径的结果
    Diff         ShadowDiff      // 差异分类
}

// 比较前，两侧结果必须规范化为同一结构，才能做集合 diff
// WorkerResult 和 ImpactResult 数据结构完全不同，不能直接比较
type NormalizedEdge struct {
    SourceID string  // 按 §3.1.2 格式: "{repo}:{file}:{qualified_name}#{arity}"
    TargetID string
    EdgeType string  // CALLS / IMPORTS / EXTENDS / IMPLEMENTS
}

// NormalizeWorkerResult 将 v1 LLM 输出的 CallNode 树投影为 NormalizedEdge 集合
func NormalizeWorkerResult(r *WorkerResult) []NormalizedEdge { ... }

// NormalizeImpactResult 将 v2 图遍历输出的 AffectedSymbol 投影为 NormalizedEdge 集合
func NormalizeImpactResult(r *ImpactResult) []NormalizedEdge { ... }

// CompareEdgeSets 计算两个 NormalizedEdge 集合的差异
// 集合操作（顺序无关），返回 match / miss（legacy有new无）/ extra（new有legacy无）
func CompareEdgeSets(legacy, new []NormalizedEdge) ShadowDiff { ... }

// ShadowDiffCategory 差异类别
// 注意: "extra" 不能混用 true positive 和 false positive，需要人工评判后细分
type ShadowDiffCategory string

const (
    CategoryMatch      ShadowDiffCategory = "match"         // 两侧一致
    CategoryMiss       ShadowDiffCategory = "miss"          // v2 漏了（false negative，需修复）
    CategoryExtraTP    ShadowDiffCategory = "extra_tp"      // v2 额外发现 + 人工确认正确（v1 遗漏的真实 caller）
    CategoryExtraFP    ShadowDiffCategory = "extra_fp"      // v2 额外发现 + 人工确认错误（虚假 caller）
    CategoryExtraPend  ShadowDiffCategory = "extra_pending" // 等待人工评判（shadow 运行时默认状态）
    CategoryDivergent  ShadowDiffCategory = "divergent"     // 两侧都有结果但内容不同（如跨仓库路由不同）
)

// ShadowDiff 分类结果差异（借鉴 GitNexus diffResolutions）
type ShadowDiff struct {
    Category ShadowDiffCategory
    Details  string  // 具体差异描述
}

// 差异分类规则:
//   match        — 两侧找到同一组 callers（顺序无关）
//   miss         — v2 缺失了 v1 找到的 caller（false negative，需修复）
//   extra_tp     — v2 额外发现，人工确认是 v1 遗漏的真实 caller（v2 更好）
//   extra_fp     — v2 额外发现，人工确认是虚假 caller（需修复）
//   extra_pending — 待人工评判（所有 extra 初始状态）
//   divergent    — 两侧都有结果但内容不同（如跨仓库路由不同）
//
// 重要: extra_tp 不计入惩罚指标；extra_fp 和 miss 才是需要修复的问题
```

#### 8.3.1b 评判工作流（extra_pending → tp/fp）

```
Shadow 运行时:
  所有 extra 条目初始标记为 extra_pending

评判流程（每周一次，或 PR review 时触发）:
  1. 查看 reports/shadow-parity/latest.json 中的 pending 条目
  2. 人工判断每个 extra 是真实 caller (tp) 还是虚假 caller (fp)
  3. 通过 CLI 命令回写:
     shirakami benchmark judge --run latest --symbol "order-service:..." --verdict tp
     shirakami benchmark judge --run latest --symbol "payment-service:..." --verdict fp
  4. 回写后:
     - tp: 同步更新对应 golden case 的 expected.json（v2 发现了 v1 遗漏的调用）
     - fp: 创建 issue 追踪 v2 的误报原因
  5. 更新后的 parity-trend.csv 反映真实质量（pending 不计入 MissRate/FPRate）

自动化辅助:
  - 如果 extra 的符号存在于 symbol_edges 表中（有索引证据）→ 大概率 tp
  - 如果 extra 的符号只出现在 LLM 输出中（无索引佐证）→ 优先 review
```

#### 8.3.2 聚合报告

```go
// ShadowParityReport 聚合所有 record 的统计
type ShadowParityReport struct {
    TotalSymbols      int
    MatchCount        int     // 两侧一致
    MissCount         int     // v2 缺失（false negative，需修复）
    ExtraTPCount      int     // v2 额外发现 + 人工确认正确（v1 漏报的真实 caller）
    ExtraFPCount      int     // v2 额外发现 + 人工确认错误（需修复）
    ExtraPendingCount int     // 等待人工评判的 extra 条目
    DivergentCount    int     // 内容不同

    // 质量指标（extra_tp 不计入惩罚）
    // MissRate  = MissCount / (MatchCount + MissCount)
    //   — 衡量 v2 遗漏真实 caller 的比例，越低越好
    // FPRate    = ExtraFPCount / (MatchCount + MissCount + ExtraFPCount)
    //   — 衡量 v2 制造虚假 caller 的比例，越低越好
    // ParityRate 已废弃旧的 MatchCount/TotalSymbols 公式（该公式会把 extra_tp 误判为退化）
    MissRate       float64 // MissCount / (MatchCount + MissCount)
    FPRate         float64 // ExtraFPCount / (MatchCount + MissCount + ExtraFPCount)

    // Golden accuracy（仅 golden cases 上计算，才是真正的质量指标）
    GoldenRecall   float64  // 对 ground truth 的 recall
    GoldenPrecision float64 // 对 ground truth 的 precision

    ByRepo    map[string]RepoParityStats
    Timestamp time.Time
}
```

#### 8.3.3 晋级门槛（与 §4.1 保持一致）

```
Shadow Mode → Hybrid Mode 切换条件（四个指标必须同时满足）:
  1. [准确率]  在 golden test cases 上 Recall ≥ 0.90（v2 对 ground truth 的覆盖率）
  2. [精确率]  在 golden test cases 上 Precision ≥ 0.85（不能制造太多误报）
  3. [回归防护] Shadow MissRate ≤ 10%（不能比 v1 遗漏更多真实 caller）
               MissRate = MissCount / (MatchCount + MissCount)
  4. [稳定性]  连续 5 次分析任务上述指标无下降趋势

说明:
  - extra_tp（v2 发现 v1 遗漏的真实 caller）不计入惩罚指标，反而应更新 golden cases
  - extra_fp（虚假 caller）计入 FPRate，FPRate ≤ 5% 作为软性参考
  - entry_point 级别的 miss 单独追踪，即使总 MissRate 达标，EP Miss 也不能 > 1 个
```

#### 8.3.4 持久化与可视化

```
reports/shadow-parity/
├── 2024-05-01T14:30:00Z.json   # 每次分析保存一份
├── 2024-05-02T09:15:00Z.json
└── latest.json                  # 指向最新报告（CI 读取）
```

报告终端输出（附加在分析结果末尾）：

```
[Shadow Parity Report]
  Symbols analyzed: 23
  Match: 19 (82.6%)  |  Miss: 2 (8.7%)  |  Extra: 1 (4.3%)  |  Divergent: 1 (4.3%)
  
  Misses (v2 未找到):
    - order-service:OrderClient.NotifyPaid → 跨仓库调用未被 Contract Bridge 覆盖
    - payment-service:RetryPolicy.Execute → 动态反射调用
  
  Extras (v2 额外发现):
    + api-gateway:MetricsMiddleware.RecordLatency → v1 LLM 遗漏的 depth=2 caller
```

---

### 8.4 端到端效果评测

借鉴 GitNexus SWE-bench Evaluation 的模式对比框架，但适配 Shirakami 的场景：

#### 8.4.1 评测维度

| 指标 | 定义 | 计算方式 |
|------|------|----------|
| **Precision** | 输出的调用链中正确的比例 | `correct_edges / total_output_edges`（仅计 confidence ≥ 0.7 的边，默认阈值可配置） |
| **Recall** | ground truth 中被找到的比例 | `found_edges / total_golden_edges`（同上阈值） |
| **F1** | Precision 和 Recall 的调和均值 | `2 * P * R / (P + R)` |
| **Entry Point Recall** | 集成测试入口的覆盖率（按 depth 拆分） | `found_entries / golden_entries`；细分 `ep_recall_d1`（depth=1直接入口）和 `ep_recall_d2`（depth=2间接） |
| **Cross-Repo Recall** | 跨仓库调用的覆盖率 | `found_cross / golden_cross` |
| **Cost** | Token 消耗（美元） | 累计 LLM 调用费用 |
| **Latency** | 端到端分析耗时 | wall-clock 秒 |
| **LLM Calls** | LLM 调用次数 | 统计 Complete() 调用 |

#### 8.4.2 评测矩阵

```bash
# 运行评测（对 golden test cases 执行所有模式）
shirakami benchmark run --golden-dir tests/golden/cases/

# 指定模式对比
shirakami benchmark run --modes off,shadow,hybrid --golden-dir tests/golden/cases/

# 单个 case 调试
shirakami benchmark debug --case payment-timeout-retry --mode hybrid
```

输出对比表：

```
┌─────────────────┬──────────┬──────────┬──────────┬──────────┐
│ Mode            │ F1       │ EP Recall│ Cost ($) │ Latency  │
├─────────────────┼──────────┼──────────┼──────────┼──────────┤
│ off (v1 LLM)   │ 0.71     │ 0.80     │ $0.42    │ 4m 12s   │
│ shadow (LLM出) │ 0.71     │ 0.80     │ $0.48    │ 4m 30s   │
│ shadow (Det层) │ 0.83     │ 0.90     │  —       │  —       │
│ hybrid (v2)    │ 0.87     │ 0.95     │ $0.08    │ 28s      │
│ deterministic   │ 0.82     │ 0.90     │ $0.02    │ 12s      │
└─────────────────┴──────────┴──────────┴──────────┴──────────┘
注: shadow 模式对外输出 LLM 结果（"LLM出"行），Det层指标仅作内部对比参考
```

#### 8.4.3 评测结果持久化

```
reports/benchmark/
├── 2024-05-01_all-modes.json     # 完整评测结果（gitignore，单次产物较大）
├── 2024-05-02_hybrid-only.json
└── summary.csv                    # 追加模式，便于趋势分析（纳入 git 追踪）
```

> **注意：** `summary.csv` 与 `per-run/*.json` 需区别对待：
> - `per-run/` 目录 gitignore（单次 JSON 文件较大，CI 环境全新无法累积）
> - `summary.csv` **纳入 git 追踪**（仅聚合指标，文件小，提 PR 时随代码一起提交），
>   才能跨次对比趋势（如"本次 F1 比上周下降了 3%"）
> - 如果使用 PostgreSQL `benchmark_runs` 表存储（已有 DB 基础设施），
>   可通过 Grafana 看趋势，此时 `summary.csv` 可选

```json
// reports/benchmark/2024-05-01_all-modes.json
{
  "run_at": "2024-05-01T14:30:00Z",
  "golden_cases": 25,
  "results": {
    "off": {
      "precision": 0.78, "recall": 0.65, "f1": 0.71,
      "entry_point_recall": 0.80, "cross_repo_recall": 0.60,
      "avg_cost_usd": 0.42, "avg_latency_sec": 252,
      "avg_llm_calls": 18
    },
    "hybrid": {
      "precision": 0.91, "recall": 0.83, "f1": 0.87,
      "entry_point_recall": 0.95, "cross_repo_recall": 0.85,
      "avg_cost_usd": 0.08, "avg_latency_sec": 28,
      "avg_llm_calls": 2
    }
  },
  "per_case": { ... }
}
```

---

### 8.5 性能基准

#### 8.5.1 索引性能基准

```
shirakami benchmark index --repo <name>
```

| 仓库规模 | 文件数 | 目标索引时间 | 目标符号数 |
|----------|--------|------------|-----------|
| 小（< 100 files） | ~100 | < 10s | 500-2000 |
| 中（100-1000 files） | ~500 | < 60s | 2000-20000 |
| 大（1000+ files） | ~3000 | < 5min | 20000-100000 |

#### 8.5.2 查询性能基准

| 操作 | 数据规模 | 目标延迟 |
|------|----------|----------|
| DiffToSymbols | 100 hunks | < 50ms |
| Impact BFS depth=3 | 10万条边 | < 10ms |
| Impact BFS depth=3 | 50万条边 | < 100ms |
| Contract Match | 1000 contracts | < 20ms |
| 完整 hybrid 分析 | 已索引仓库 | < 15s |

#### 8.5.3 回归检测

在 CI 中运行性能基准，检测退化：

```yaml
# .github/workflows/ci-benchmark.yml (伪代码)
- name: Run performance benchmarks
  run: |
    shirakami benchmark perf --baseline reports/benchmark/perf-baseline.json
    # 如果任何指标退化 > 20%，CI 失败
```

---

### 8.6 CI 门禁集成

| 触发条件 | 运行的验证 | 失败策略 |
|----------|-----------|----------|
| PR 修改 `internal/index/` | Golden Test Cases 全量 | 阻断合并 |
| PR 修改 `internal/resolve/` | Golden Test Cases + Shadow Parity | 阻断合并 |
| PR 修改 `internal/contract/` | Contract 匹配 golden cases | 阻断合并 |
| PR 修改 `internal/agent/` | 原有 unit tests + Golden subset | 阻断合并 |
| 定时（每周） | **仅** deterministic 模式（无 LLM 费用）+ 性能基准 | 报告，不阻断 |
| Release 前（手动触发，需 `BENCHMARK_LLM=true`） | 完整 benchmark matrix（含 LLM 模式） | 阻断发布 |

#### 8.6.1 最小可行 CI（Phase 1 起步）

```yaml
# 不需要等全部基础设施就绪，Phase 1 起步只需:
benchmark-smoke:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - run: go test ./tests/golden/... -v -timeout=5m
    # golden test runner 流程:
    # 1. 用 testcontainers-go 启动临时 PostgreSQL（复用 tests/integration/ 已有基础设施）
    # 2. 加载 cases/<name>/fixtures.sql 预建索引数据
    # 3. 运行 DiffToSymbols + Impact，对比 expected.json（含 symbol_id / depth 校验）
    # 4. 清理容器
    #
    # 注意: DiffToSymbols 和 Impact 均需查询 symbol_nodes / symbol_edges 表；
    # 若无 fixtures.sql，查询返回空，case 将因"全部 uncovered"而失败。
    # 因此每个 golden case 必须包含 fixtures.sql。
```

#### 8.6.2 含 LLM 的完整评测（Release 前手动触发）

```yaml
# 完整 benchmark matrix（含 LLM 模式会产生真实 API 费用）
# 每周定时 CI 不运行此步骤，避免非预期费用（25 cases × ~18 LLM calls × $0.42 ≈ $10+/次）
release-full-benchmark:
  if: env.BENCHMARK_LLM == 'true'   # 必须显式设置才触发
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - run: |
        shirakami benchmark run \
          --modes off,shadow,hybrid,deterministic \
          --golden-dir tests/golden/cases/ \
          --output reports/benchmark/release-$(date +%Y%m%d).json
    # Release 前需人工确认 summary 中 F1 / EP Recall 未退化后再继续发布流程
```

---

### 8.7 目录结构总览

```
tests/
├── golden/
│   ├── cases/                    # ground truth 语料库
│   │   ├── payment-timeout-retry/
│   │   │   ├── input.patch
│   │   │   ├── input.yaml
│   │   │   ├── expected.json
│   │   │   ├── fixtures.sql      # 预建 symbol_nodes / symbol_edges 数据（必须）
│   │   │   └── metadata.json
│   │   ├── order-status-update/
│   │   └── ...
│   ├── runner_test.go            # golden test runner（go test 集成，用 testcontainers 起 PG）
│   └── schema.json               # expected.json 的 JSON Schema（含 symbol_id 格式约束）
├── e2e/                          # 现有 e2e 测试（保持不变）
└── integration/                  # 现有集成测试（保持不变）

internal/benchmark/
├── shadow.go                     # Shadow Parity 引擎（含 NormalizeWorkerResult / NormalizeImpactResult）
├── golden.go                     # Golden Test Case 评估器
├── metrics.go                    # 评测指标计算（P/R/F1，含 confidence 阈值参数）
└── report.go                     # 评测报告生成

reports/
├── shadow-parity/
│   ├── per-run/                  # gitignore：单次对比 JSON（运行时生成）
│   └── parity-trend.csv          # git 追踪：聚合趋势指标（随 PR 提交）
└── benchmark/
    ├── per-run/                  # gitignore：单次完整 JSON（运行时生成）
    ├── summary.csv               # git 追踪：聚合指标趋势（随 PR 提交）
    └── perf-baseline.json        # git 追踪：性能回归基线（CI --baseline 引用）

cmd/analyze/main.go               # 新增 `benchmark` 子命令（见 §7.2）
```

---

## 9. 成功标准与退出条件

### 9.1 v2 成功标准

满足以下 **任意两项** 即可宣布 v2 达标：

| # | 标准 | 衡量方式 |
|---|------|----------|
| 1 | Go 仓库 hybrid F1 ≥ 0.85 | golden cases 中 Go-only cases 的 F1 |
| 2 | Entry Point Recall ≥ v1 水平 | hybrid 模式不能比 off 模式丢失更多入口 |
| 3 | 跨仓库 Contract Bridge recall ≥ 80% | 仅限已声明 entry-role 的仓库 |
| 4 | 平均分析成本降低 ≥ 50% | 对比 off vs hybrid 的 avg_cost_usd |

### 9.2 退出条件（何时止损）

```
如果 3 个月内未达到 §9.1 任意两项:

  1. 放弃"确定性替代 LLM"路线
  2. 转为"LLM-with-better-context"轻量方案:
     - 保留 DiffToSymbols（纯 Go diff 解析，无需索引）
     - 保留 contracts.yaml 手动声明（注入 Worker prompt 作为 hint）
     - 不做 go/ast 全仓库索引
     - 不做内存图 BFS
  3. 上述轻量方案预计:
     - LLM 调用减半（5-10 次/分析）
     - 跨仓库 recall 显著提升（hint 驱动）
     - 零基础设施投入
```

---

## 10. MVP 定义与执行时间线

### 10.1 MVP 范围（2 周可交付）

```
做:
  ✅ DiffToSymbols — 纯 Go 解析 unified diff，提取变更行号范围
     （不依赖索引，不依赖 DB — 直接替代 extractChangedFunctions 的 LLM 调用）
  ✅ contracts.yaml 手动声明 — 跨仓库已知调用关系，注入 Worker prompt
  ✅ 首批 3 个 golden cases — 选取 reports/ 中已有结果，人工标注
  ✅ Plateau/Early-Stop — Orchestrator 连续 3 轮无新 node 自动停止（§12.1，~20 行改动）

不做（后续迭代）:
  ❌ go/ast 全仓库索引
  ❌ 内存图 BFS
  ❌ Contract Bridge 自动提取
  ❌ Shadow Mode
  ❌ tree-sitter Python
  ❌ 性能基准 CI
```

### 10.2 执行时间线

```
Week 1-2:  DiffToSymbols + contracts.yaml hint 注入 + 3 个 golden cases
           → 可验证: extractChangedFunctions 零 LLM 调用，跨仓库 hint 生效
           [对应 Phase 0 → Phase 0 + DiffToSymbols 增强]

Week 3-4:  go/ast + go/types 索引 Go 仓库（metadata_go 作为试点）
           → 写入 symbol_nodes + symbol_edges 表
           → 内存图 + BFS 输出调用链
           → 可验证: 对 Go 仓库的分析 LLM 调用降至 0-2 次
           [Phase 1 Shadow Mode 前置条件就绪]

Week 5-6:  扩充至 5-10 个 golden cases + runner_test.go CI
           → 可验证: go test ./tests/golden/... 通过
           [Phase 1 Shadow Mode 启动]

Week 7-8:  Shadow Mode 运行（只记录差异，不切换）
           → 可验证: shadow parity 报告显示 MissRate / FPRate
           [Phase 1 数据积累中]

Week 9+:   根据 shadow 数据评估晋级条件（§4.1 四项指标）:
           → 全部达标? → 切换 Phase 2 Hybrid Mode
           → MissRate > 10%? → 定位缺失场景，逐个修复
           → 3 个月仍未达标? → 执行 §9.2 退出条件，退回轻量方案
```

### 10.3 MVP 交付物

| 交付物 | 文件 | 改动量 |
|--------|------|--------|
| DiffToSymbols (Layer A) | `internal/tool/gitdiff.go` | 新增 ~150 行 |
| contracts.yaml 支持 | `internal/config/config.go` + `internal/agent/worker.go` prompt 注入 | ~100 行 |
| Plateau/Early-Stop | `internal/agent/orchestrator.go` | ~20 行 |
| Golden case #1-3 | `tests/golden/cases/` | 3 组文件 |
| Golden runner | `tests/golden/runner_test.go` | ~50 行 |

---

## 11. 分发渠道规划

### 11.1 优先级排序

| 优先级 | 渠道 | 理由 | 时间线 |
|--------|------|------|--------|
| **P0** | CLI（已有） | 核心使用方式 | 已完成 |
| **P0** | HTTP API（已有） | 供内部平台集成 | 已完成 |
| **P1** | CI/CD Webhook | 最高增量价值 — PR 提交自动触发，结果评论到 MR | Week 5-6 |
| **P2** | Git Hook (pre-push) | 推送前自动检查影响范围 | Week 7+ |
| **P3** | MCP Server | 让 AI Agent 在 coding 时查询影响范围 | 视需求 |
| **P4** | Web UI | 可视化调用链图 | 不急 |

### 11.2 CI/CD Webhook 设计（P1）

```
触发: Merge Request 创建/更新时
流程:
  1. Webhook 接收 MR event（diff URL + source branch）
  2. 调用 shirakami analyze --diff <mr_diff> --format markdown
  3. 将分析结果作为 MR comment 发布
  4. 如果 risk = CRITICAL，设置 MR approval 规则（需要额外审批人）

集成方式:
  - GitLab: Webhook + GitLab API（POST /projects/:id/merge_requests/:iid/notes）
  - GitHub: GitHub Actions + PR comment
```

### 11.3 MCP Server 设计（P3，备选）

如果未来需要让 AI Agent（Claude Code / Cursor）在 coding 时查询 Shirakami：

```go
// cmd/mcp/main.go — 极简 MCP server，仅暴露 2 个 tool

tools:
  - shirakami_impact:
      description: "分析符号变更的影响范围（blast radius）"
      input: {target, repo, direction, maxDepth}
      
  - shirakami_analyze_diff:
      description: "分析 diff 的完整影响 + 测试建议"
      input: {diff, description, source_repo}
```

当前不实现。待 CI/CD Webhook 验证产品价值后再评估。

---

## 12. 自主优化与运行时改进（借鉴 autoresearch）

> 借鉴 [autoresearch](https://github.com/uditgoenka/autoresearch) 的核心理念：
> 约束范围 + 机械化指标 + 自动验证 + 自动回滚 = 自主改进循环。
> 以下改进将 autoresearch 的方法论应用到 Shirakami 的分析流程和质量保障中。

### 12.1 Plateau Detection / Early-Stop（Orchestrator 优化）

**问题**：当前 Orchestrator 的 `maxRounds` 是硬上限（deep=10, fast=3），但常见情况是后 3-5 轮 Worker 只找到重复 node 或完全无新结果，纯粹浪费 LLM 调用。

**借鉴 autoresearch**：跟踪 `iterations_since_best`，连续 N 轮无改善 → 自动停止。

**实现（修改 `internal/agent/orchestrator.go`）**：

```go
// 在 Run() 的 round 循环中新增 early-stop 检测
const earlyStopPatience = 3 // 连续 3 轮无新发现即停止

consecutiveNoProgress := 0
prevTotalNodes := 0

for round := 0; round < maxRounds && len(pending) > 0; round++ {
    results := o.runWorkerBatch(ctx, pending)
    // ... 现有 merge 逻辑 ...

    // Plateau detection
    currentTotalNodes := len(output.CallGraph)
    currentTotalEntries := len(output.EntryPoints)
    
    newNodesThisRound := currentTotalNodes - prevTotalNodes
    if newNodesThisRound == 0 && len(nextPending) == 0 {
        consecutiveNoProgress++
    } else {
        consecutiveNoProgress = 0
    }
    prevTotalNodes = currentTotalNodes

    if consecutiveNoProgress >= earlyStopPatience {
        log.Infow("analyse.early_stop",
            "reason", "plateau_detected",
            "round", round,
            "consecutive_no_progress", consecutiveNoProgress,
            "total_nodes", currentTotalNodes,
            "saved_rounds", maxRounds - round - 1,
        )
        break
    }

    pending = nextPending
}
```

**价值**：
- 深度模式平均节省 2-4 轮 LLM 调用（~30% token 节约）
- 不影响 recall（plateau 说明已无新发现）
- 可通过 `--no-early-stop` flag 禁用（强制跑满所有轮次）

**纳入时间线**：MVP（Week 1-2），改动量极小（~20 行）。

---

### 12.2 机械化 Verify 命令（Benchmark 可消费输出）

**问题**：当前 `shirakami benchmark` 输出是表格/JSON，但 CI 管道和 autoresearch-style 循环需要单一数值 + exit code。

**借鉴 autoresearch**：每个 Verify 命令必须输出单一可解析数值，exit code 表示 pass/fail。

**实现（新增到 `cmd/analyze/main.go` benchmark 子命令）**：

```bash
# 输出单一 F1 数值到 stdout，可被管道消费
shirakami benchmark verify --golden-dir tests/golden/cases/ --mode hybrid
# stdout: 0.87
# exit code: 0

# 带阈值门控（低于阈值返回 exit code 1）
shirakami benchmark verify --golden-dir tests/golden/cases/ --mode hybrid --threshold 0.80
# stdout: 0.87
# exit code: 0 (0.87 >= 0.80)

# 指定输出指标（默认 F1，可选 ep_recall / cross_recall / cost）
shirakami benchmark verify --metric ep_recall --threshold 0.85
# stdout: 0.90
# exit code: 0
```

**Guard 支持**：

```bash
# 多指标 guard（所有条件必须满足）
shirakami benchmark verify \
  --guard "f1 >= 0.80" \
  --guard "ep_recall >= 0.85" \
  --guard "latency_sec <= 600" \
  --guard "llm_calls <= 10"
# stdout: PASS (f1=0.87, ep_recall=0.90, latency=28s, llm_calls=2)
# exit code: 0

# guard 失败时
# stdout: FAIL (ep_recall=0.72 < 0.85)
# exit code: 1
```

**纳入时间线**：Week 5-6（与 golden runner 同步）。

---

### 12.3 自主 Prompt 优化循环（Week 9+）

**问题**：Worker prompt（`internal/agent/prompt.go` + `worker.go`）的搜索策略是手工调优的。是否可以让 Agent 自主迭代优化？

**借鉴 autoresearch 核心循环**：

```
Goal: 提高 golden test cases F1 score
Scope: internal/agent/prompt.go, internal/agent/triage.go, internal/agent/worker.go
Metric: F1 (higher is better)
Verify: shirakami benchmark verify --golden-dir tests/golden/cases/ --mode hybrid
Guard: shirakami benchmark verify --metric ep_recall --threshold 0.80
```

**执行方式**（使用 autoresearch 作为 Claude Code skill）：

```bash
# 在 Shirakami 项目中安装 autoresearch skill
cp -r /path/to/autoresearch/claude-plugin/skills/autoresearch .claude/skills/autoresearch

# 然后在 Claude Code 中运行
/autoresearch
Goal: Improve Shirakami analysis F1 score from 0.71 to 0.85
Scope: internal/agent/prompt.go, internal/agent/triage.go, internal/agent/worker.go
Metric: F1 score (higher is better)
Verify: go run ./cmd/analyze benchmark verify --golden-dir tests/golden/cases/ --mode off
Guard: go run ./cmd/analyze benchmark verify --metric ep_recall --threshold 0.75
Iterations: 20
```

**可优化的维度**：
- Worker system prompt 的搜索策略措辞
- Triage 的优先级分类规则
- cross_repo_calls 的 JSON schema 约束措辞
- 搜索结果上限（当前 `callers > 20 → wide_impact`）
- follow-up prompt 的场景生成引导

**安全约束**：
- Guard 确保 EP Recall 不降（防止优化 F1 时丢失关键入口）
- 每次只改一处（原子修改）
- Git 记录所有实验（可回溯）
- golden cases 不被修改（只改 prompt/策略代码）

**纳入时间线**：Week 9+（需要 golden cases + verify 命令先就绪）。

---

### 12.4 Chain 可组合流程

**问题**：当前 `shirakami analyze` 是单一命令，输出后用户需手动决定下一步。

**借鉴 autoresearch `--chain` 机制**：子命令之间通过 `handoff.json` 传递上下文。

**实现**：

```bash
# 分析 + 自动推送到 MR（chain 到 webhook）
shirakami analyze --diff changes.patch --chain webhook --mr-url https://...

# 分析 + 只输出测试场景（跳过调用链展示）
shirakami analyze --diff changes.patch --chain scenarios

# 分析 + 自动对比 golden case（self-verify）
shirakami analyze --diff changes.patch --chain verify --golden-case payment-timeout

# 分析 + 索引更新（如果索引陈旧，先更新再分析）
shirakami analyze --diff changes.patch --chain index-first
```

**Chain 内部机制**：

```go
// internal/chain/chain.go

type ChainStep struct {
    Name   string          // "analyze" / "webhook" / "verify" / "scenarios"
    Input  json.RawMessage // 上一步的输出
    Config map[string]string
}

// Handoff 文件（临时，分析完成后自动清理）
// /tmp/shirakami-handoff-{task_id}.json
type Handoff struct {
    TaskID      string           `json:"task_id"`
    AnalysisOut *AnalysisOutput  `json:"analysis_output"`
    Timestamp   time.Time        `json:"timestamp"`
    NextStep    string           `json:"next_step"`
}
```

**纳入时间线**：Week 5+（webhook 阶段一并实现）。

---

### 12.5 Constraint Probe — LLM 补充层增强

**问题**：LLM 补充层（处理 uncovered 符号时）直接用 ripgrep 搜索函数名，但常遗漏"间接相关"的符号。

**借鉴 autoresearch probe 工作流**：多视角对抗性审问，直到"新约束饱和"。

**实现（增强 `internal/agent/worker.go` 的 LLM 补充路径）**：

```go
// probeConstraints 在 LLM 搜索前，先让 LLM 从多视角扩展搜索范围
func (w *WorkerAgent) probeConstraints(ctx context.Context, uncoveredFunc string, desc string) []string {
    probePrompt := fmt.Sprintf(`Before searching for callers of "%s", think from these perspectives:

1. CALLER PERSPECTIVE: Who would need to call this function? What module/service logically depends on it?
2. DATA FLOW PERSPECTIVE: What data does this function consume/produce? Who provides that data upstream?
3. ERROR PERSPECTIVE: If this function fails, who handles the error? Is there retry/fallback logic elsewhere?
4. CONFIG PERSPECTIVE: Is this function registered in any config file (YAML/JSON routing, decorator, DI container)?
5. EVENT PERSPECTIVE: Does this function publish/subscribe to events? Who reacts to those events?

For each perspective, output 1-2 additional search keywords (function names, module names, config keys) 
that I should also search for when tracing the call chain of "%s".
Output format: one keyword per line, no explanation.`, uncoveredFunc, uncoveredFunc)

    result, _ := w.loop.RunFollowUpNoTools(ctx, "probe-"+uncoveredFunc, "", probePrompt)
    return parseKeywordList(result.Content)
}
```

**集成点**：在 Worker 的 ripgrep 搜索前调用 probe，扩展搜索关键词池：

```go
// worker.go Analyse() 中的搜索步骤
func (w *WorkerAgent) Analyse(ctx context.Context, task WorkerTask) (*WorkerResult, error) {
    // 如果是 uncovered 的 LLM 补充路径:
    if task.IsUncoveredFallback {
        // 新增: 先 probe 扩展搜索范围
        extraKeywords := w.probeConstraints(ctx, task.ChangedFunctions[0], task.Description)
        task.SearchHints = append(task.SearchHints, extraKeywords...)
    }
    // ... 原有搜索逻辑（现在搜索范围更广）...
}
```

**价值**：提高 LLM 补充层的 recall（从搜索"精确函数名"扩展到搜索"相关上下文"）。

**纳入时间线**：Week 7+（需要 shadow mode 数据验证效果）。

---

### 12.6 对抗验证 — 减少 Shadow Extra 的人工审判

**问题**：Shadow Mode 中的 `extra_pending` 条目需要人工判断 tp/fp，工作量大。

**借鉴 autoresearch reason 工作流**：Generate → Critic → Judge 三阶段对抗。

**实现（增强 `internal/benchmark/shadow.go`）**：

```go
// AutoJudgeExtra 对 extra_pending 条目进行自动对抗验证
// 减少人工审判量（仅对 "不确定" 的结果保留 pending）
func AutoJudgeExtra(ctx context.Context, llm LLMClient, record ShadowRecord) ShadowDiffCategory {
    caller := record.NewResult.ExtraCallers[0] // v2 发现但 v1 没有的 caller
    target := record.Symbol

    // Agent A: 证明调用存在
    proofPrompt := fmt.Sprintf(
        "Given this code context, prove that %s calls %s. "+
        "Provide: file path, line number, exact call expression. "+
        "If you cannot find concrete evidence, say 'NO EVIDENCE'.",
        caller, target)
    proof, _ := llm.Complete(ctx, []Message{{Content: proofPrompt}}, nil)

    // Agent B: 证明调用不存在（对抗性）
    disprovePrompt := fmt.Sprintf(
        "Given this code context, argue that %s does NOT actually call %s. "+
        "Consider: Is the match a false positive from ripgrep? "+
        "Is it a comment/string/dead code? Is it a different function with the same name?",
        caller, target)
    disproof, _ := llm.Complete(ctx, []Message{{Content: disprovePrompt}}, nil)

    // Judge: 综合判断
    judgePrompt := fmt.Sprintf(
        "Evidence FOR the call:\n%s\n\nEvidence AGAINST the call:\n%s\n\n"+
        "Verdict (one word): EXISTS or NOT_EXISTS or UNCERTAIN",
        proof.Content, disproof.Content)
    verdict, _ := llm.Complete(ctx, []Message{{Content: judgePrompt}}, nil)

    switch strings.TrimSpace(strings.ToUpper(verdict.Content)) {
    case "EXISTS":
        return CategoryExtraTP
    case "NOT_EXISTS":
        return CategoryExtraFP
    default:
        return CategoryExtraPend // 不确定 → 保留给人工
    }
}
```

**使用时机**：
- Shadow Mode 每次运行后，对所有 `extra_pending` 自动运行 AutoJudge
- 仅对 Judge 结果为 "UNCERTAIN" 的条目保留 `extra_pending` 状态
- 预期减少 60-80% 的人工审判工作量

**成本控制**：
- 每个 extra 条目需要 3 次 LLM 调用（proof + disproof + judge）
- 典型 shadow 运行产生 1-5 个 extra → 额外 3-15 次 LLM 调用
- 远低于人工审判的时间成本

**纳入时间线**：Week 7-8（与 Shadow Mode 同步）。

---

### 12.7 Index Check/Update 子命令

**问题**：§3.1.3 定义了索引策略，但缺少用户可操作的维护命令。

**借鉴 autoresearch learn 的 4 种模式**（init/update/check/summarize）：

```bash
# 检查索引健康（只读，秒级返回）
shirakami index check [--repo <name>]
# 输出:
#   payment-service: indexed@abc123 (3 days ago), HEAD@def456, 42 files changed
#   order-service:   indexed@def456 (current), 0 files changed ✓
#   api-gateway:     NOT INDEXED

# 增量更新（仅变更文件，通常 < 30s）
shirakami index update [--repo <name>]
# 输出:
#   payment-service: reindexed 42 files (+127 symbols, -34 stale), 8.2s

# 全量重建
shirakami index rebuild --repo <name>
# 输出:
#   payment-service: full rebuild, 3847 symbols, 12891 edges, 47.3s

# 快速概览（类似 learn 的 summarize 模式）
shirakami index status
# 输出:
#   ┌─────────────────────┬───────────┬──────────┬─────────────┬──────────┐
#   │ Repo                │ Symbols   │ Edges    │ Coverage    │ Status   │
#   ├─────────────────────┼───────────┼──────────┼─────────────┼──────────┤
#   │ payment-service     │ 2,341     │ 8,912    │ Go:97%      │ STALE    │
#   │ order-service       │ 1,876     │ 6,234    │ Go:98%      │ CURRENT  │
#   │ api-gateway         │ —         │ —        │ —           │ MISSING  │
#   │ vstation_compute    │ 4,521     │ 15,677   │ Py:71%      │ CURRENT  │
#   └─────────────────────┴───────────┴──────────┴─────────────┴──────────┘
```

**纳入时间线**：Week 3-4（与索引构建同步）。

---

### 12.8 迭代式场景生成（增强 test scenario 质量）

**问题**：当前 `buildScenarioFollowUp` 是单次 LLM 调用生成所有测试场景。容易遗漏边界条件，且无法"越想越深"。

**借鉴 autoresearch scenario 工作流**：种子 → 12 维度分解 → 迭代探索 → 每轮发现新组合 → 直到饱和。

**改进方案**（修改 Worker 的 scenario 生成逻辑）：

```go
// iterativeScenarioGeneration 替代现有的单次 buildScenarioFollowUp
func (w *WorkerAgent) iterativeScenarioGeneration(
    ctx context.Context,
    taskID string,
    priorContent string,
    entryPoints []CallNode,
    changedFunctions []string,
    maxRounds int,  // 默认 3 轮
) []EntryPointScenario {
    allScenarios := make([]EntryPointScenario, 0)
    coveredDimensions := make(map[string]bool)

    for round := 0; round < maxRounds; round++ {
        // 每轮告诉 LLM 已覆盖的维度，要求探索新维度
        prompt := buildIterativeScenarioPrompt(
            changedFunctions, entryPoints,
            allScenarios,          // 已有场景（避免重复）
            coveredDimensions,     // 已覆盖维度
            round,
        )

        result, err := w.loop.RunFollowUpNoTools(ctx, taskID, priorContent, prompt)
        if err != nil {
            break
        }

        newScenarios := parseEntryScenarios(result.Content)
        if len(newScenarios) == 0 {
            break // 饱和：LLM 无法再发现新场景
        }

        // 更新已覆盖维度
        for _, s := range newScenarios {
            for _, sc := range s.Scenarios {
                coveredDimensions[sc.Type] = true
            }
        }
        allScenarios = append(allScenarios, newScenarios...)

        // 饱和检测：如果本轮新场景 < 2 个，认为已饱和
        if len(newScenarios) < 2 {
            break
        }
    }
    return allScenarios
}

func buildIterativeScenarioPrompt(changed []string, entries []CallNode, 
    existing []EntryPointScenario, covered map[string]bool, round int) string {
    
    // 第 1 轮：标准场景生成（P0 核心流程）
    // 第 2 轮：要求探索 boundary / exception / compatibility
    // 第 3 轮：要求探索 concurrency / race condition / data corruption
    
    dimensionHints := []string{}
    if round >= 1 && !covered["boundary"] {
        dimensionHints = append(dimensionHints, "boundary conditions (size limits, empty inputs, max values)")
    }
    if round >= 1 && !covered["exception"] {
        dimensionHints = append(dimensionHints, "exception paths (network timeout, DB down, auth expired)")
    }
    if round >= 2 && !covered["concurrency"] {
        dimensionHints = append(dimensionHints, "concurrency (parallel requests, race conditions, idempotency)")
    }
    // ...
}
```

**价值**：
- 测试场景从平均 3-5 个增加到 8-12 个
- 覆盖更多边界条件和异常路径
- 饱和检测避免无限循环

**纳入时间线**：Week 9+（测试建议质量提升属于 Phase 3）。
