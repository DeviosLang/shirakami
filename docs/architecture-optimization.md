# Shirakami 架构优化分析

> 生成时间：2026-05-07  
> 基于：Shirakami 当前代码库深度分析

---

## 第一轮分析：基于 Shirakami 自身代码库

### 🔴 P0 — 影响生产稳定性

#### 1. Checkpoint 防损坏恢复（`internal/checkpoint/`）

**现状短板**：checkpoint 文件写入无原子性——进程崩溃在 `os.WriteFile` 中间会产生半截 JSON，下次 resume 直接 unmarshal 失败，任务永久卡住无法重试。

**优化方案**：写入时先写 `.tmp` 文件，成功后 `os.Rename` 替换原文件（atomic swap）。同时对损坏的 checkpoint 捕获 unmarshal 错误后自动清除并重新开始，而非返回 error 给用户。

```go
// checkpoint/store.go
func (s *Store) Save(id string, state *State) error {
    data, _ := json.Marshal(state)
    tmpPath := s.path(id) + ".tmp"
    if err := os.WriteFile(tmpPath, data, 0644); err != nil {
        return err
    }
    return os.Rename(tmpPath, s.path(id)) // atomic
}
```

#### 2. 消息历史滑动窗口截断（`internal/agent/loop.go`）

**现状短板**：`AgentLoop` 消息数组无上界，深度分析跑满 300 步时消息列表可达数千条，context 窗口溢出时 compress 压缩整体 conversation，但压缩本身也耗时耗 token，在某些极端场景下压缩后的消息仍然超长。

**优化方案**：在压缩前先做消息截断策略——保留 system message + 最后 N 条 tool result + 最后 K 条对话，其余中间消息完全丢弃（不是压缩）。这是"滑动窗口"策略。

---

### 🟠 P1 — 影响分析质量

#### 3. 工具错误分类反馈（`internal/agent/loop.go`）

**现状短板**：工具执行出错时统一返回 `"error: %s"` 字符串给 LLM，LLM 无法区分"文件不存在"、"超时"、"权限问题"，容易陷入无效重试循环。

**优化方案**：工具结果结构化分类，加入 `error_type` hint：
```
{"error": "file_not_found", "hint": "try ripgrep to locate the correct path"}
{"error": "timeout", "hint": "reduce search scope"}
```

#### 4. 跨 repo 调用追踪的循环检测（`internal/agent/orchestrator.go`）

**现状短板**：Orchestrator 的 `crossRepoCalls` 迭代最多 10 轮，但没有节点级别的去重——同一个 `(repo, function)` 可能在第 3 轮和第 7 轮都被派发 Worker 分析，浪费 LLM 调用。

**优化方案**：在 `Orchestrator` 中维护 `analyzedNodes map[string]bool`（key = `repo:function`），派发前检查，已分析过的直接跳过。

#### 5. CallNode 可信度来源字段（`internal/agent/worker.go`）

**现状短板**：Worker 输出的节点没有置信度标记，Ghost Node 救活后和 LLM 确定找到的节点在 API 响应中无法区分。

**优化方案**：在 `CallNode` 上加 `Source` 字段（`llm_direct` / `ghost_rescued` / `lsp_verified`），API 层透传：
```go
type CallNode struct {
    // existing fields...
    Source string `json:"source,omitempty"` // "llm_direct"|"ghost_rescued"|"lsp_verified"
}
```

---

### 🟡 P2 — 可观测性和运维效率

#### 6. 结构化 Prometheus Metrics（`internal/feedback/`）

**现状短板**：没有 LLM token 消耗的 histogram、Ghost Node 救活率、Worker 超时率等关键运维指标。

**补充 metrics**：
```go
var (
    llmTokensTotal    = prometheus.NewHistogramVec(...)  // by model/task_type
    ghostNodeTotal    = prometheus.NewCounterVec(...)    // by outcome: rescued/lost
    workerDuration    = prometheus.NewHistogramVec(...)  // by repo/triage_tier
    checkpointResumed = prometheus.NewCounter(...)
)
```

#### 7. 分析请求链路追踪（`cmd/server/main.go`）

**现状短板**：HTTP 请求进来后，日志里 task_id 是有的，但没有 OpenTelemetry trace，无法在 Grafana/Jaeger 里看到各阶段耗时分布。

**优化方案**：引入 `go.opentelemetry.io/otel`，在 HTTP handler、Orchestrator、Worker、LLM client 各层打 span。

#### 8. 配置热重载（`internal/config/`）

**现状短板**：修改 `shirakami.yaml` 需要重启服务（影响正在运行的分析任务）。

**优化方案**：用 `fsnotify` 监听配置文件变更，对安全字段（LLM endpoint/model、并发数、超时）热更新。

---

### 🔵 P3 — 长期架构演进

#### 9. 工具结果 LRU 缓存层（`internal/tool/`）

**现状短板**：同一个 ripgrep 搜索在一次分析任务中可能被多个 Worker 重复调用。

**优化方案**：在 `Registry` 层加内存 LRU 缓存（key = `tool_name + args_hash`，TTL = 任务生命周期），避免重复 I/O。特别适合 LSP call hierarchy 这种慢操作。

#### 10. Worker 结果回写 Layer1（`internal/memory/layer1.go`）

**现状短板**：Layer1 目前只存 LLM 主动写入的摘要，每次 Worker 分析完成后产生的高质量节点信息没有回写，下次分析同一个函数时 LLM 要从头搜索。

**优化方案**：Orchestrator 合并结果后，异步将新发现的 `CallNode` 写入 Layer1（以 `(repo, function, commit_hash)` 为 key），减少 30%+ 的 ripgrep 调用。

---

### 汇总表（第一轮）

| # | 优化点 | 优先级 | 改动范围 | 预估工作量 |
|---|--------|--------|----------|-----------|
| 1 | Checkpoint 原子写+损坏自愈 | P0 | checkpoint/ | 0.5天 |
| 2 | 消息历史滑动窗口截断 | P0 | loop.go | 1天 |
| 3 | 工具错误结构化分类 | P1 | loop.go + tool/* | 1天 |
| 4 | 跨 repo 调用循环检测 | P1 | orchestrator.go | 0.5天 |
| 5 | CallNode 可信度来源字段 | P1 | worker.go + schema/ | 0.5天 |
| 6 | 补充 Prometheus metrics | P2 | feedback/ | 1天 |
| 7 | OpenTelemetry 链路追踪 | P2 | cmd/server + agent/ | 2天 |
| 8 | 配置热重载 | P2 | config/ | 1天 |
| 9 | 工具结果 LRU 缓存 | P3 | tool/registry | 1天 |
| 10 | Worker 结果回写 Layer1 | P3 | orchestrator + memory/ | 2天 |

---


---

## 第二轮分析：借鉴参考仓库（已补充）

基于 GitNexus、claude-code、autoresearch 三个参考项目的深度分析，提取以下高价值设计模式。

### GitNexus：DAG Pipeline + Type Safety

**关键模式：** GitNexus 的 12 阶段 DAG pipeline（`scan → structure → parse → …→ processes`）展示了如何用静态类型管理复杂多步流程。

**借鉴价值：**

1. **显式依赖声明**（`deps: string[]`）
   - Shirakami Orchestrator 的 cross-repo 迭代目前是隐式循环（10 轮最多）
   - GitNexus 的 DAG runner 做拓扑排序 + 静态验证，拒绝循环和未声明依赖
   - **建议：** Worker 之间的数据流显式化为 DAG

2. **类型安全的阶段输出**（`getPhaseOutput<T>(deps, 'name')`）
   - 每个 DAG 阶段强类型化其输出结果
   - **建议：** 将 Shirakami Worker 的输出定义为强类型 schema

3. **Binding Accumulator 生命周期管理**
   - GitNexus parse 阶段创建绑定、crossFile 阶段释放
   - **建议：** 为 Worker 结果设置明确的缓存生命周期（task-scoped LRU cache）

### claude-code：Token 预算 + 渐进式压缩

**关键模式：** Token 预算分层管理（60% Plan D → 70% Plan B → 80% Plan C → 92% Plan A）

**借鉴价值：**

1. **预算级联阈值**（budget tiers）
   - Shirakami 当前只有一阶压缩
   - claude-code 的四层阈值提供细粒度控制
   - **建议：** 应用到 Shirakami LLM token 管理

2. **错误恢复栈**（five-level recovery before surfacing error）
   - claude-code 对 prompt-too-long 错误有 5 层尝试
   - **建议：** Shirakami Worker 遇到 context overflow 时应有序尝试 N 种恢复策略

3. **消息结果预算**
   - 按优先级截断工具结果
   - **建议：** 应用到 ripgrep/LSP 结果集

### autoresearch：Git 作为内存 + 原子修改

**关键模式：** 8 阶段自主迭代循环，Git commit log 作为记忆和回滚机制

**借鉴价值：**

1. **原子修改 + 一句话验证**（one-sentence test）
   - autoresearch 要求每次迭代**恰好一个逻辑变更**
   - **建议：** 应用到 Shirakami Worker 的每次搜索/LSP 调用

2. **奔溃检测三恢复点**（uncommitted → unverified → clean）
   - autoresearch checkpoint 设计：未 commit → commit 但未验证 → clean state
   - **建议：** 应用到 Shirakami checkpoint

3. **日志模式识别**（read last 10-20 entries for context）
   - autoresearch 结果日志 TSV 格式
   - **建议：** Shirakami Worker 的相似搜索可以参考上次成功的查询方式

---

## 第二轮汇总表：参考项目借鉴

| 源项目 | 模式 | Shirakami 应用 | 优先级 | 预估工作量 |
|--------|------|-------------|--------|----------|
| GitNexus | DAG pipeline + 显式依赖 | Worker 派发前的循环检测（#4） | P1 | 0.5 天 |
| GitNexus | 强类型阶段输出 | CallNode source 字段（#5） | P1 | 0.5 天 |
| GitNexus | Binding lifecycle 管理 | Worker 结果 LRU cache（#9） | P3 | 1 天 |
| claude-code | 预算级联阈值 | Token 预算分层管理 | P2 | 1 天 |
| claude-code | 错误恢复栈 | 智能工具错误分类（#3） | P1 | 1 天 |
| claude-code | 消息/结果预算 | 工具结果优先级截断 | P1 | 1 天 |
| autoresearch | 原子修改 + 一句话验证 | Worker 迭代原子性约束 | P2 | 1 天 |
| autoresearch | 奔溃恢复三点 | Checkpoint 防损坏恢复（#1） | P0 | 0.5 天 |
| autoresearch | 日志模式识别 | Worker 历史查询提示 | P3 | 1.5 天 |

---

## 综合实施建议（第一 + 二轮）

### 快速赢（Quick Wins）— 即刻开始（1-2 周）

1. **Checkpoint 原子写 + 损坏自愈** ← autoresearch crash recovery（P0，0.5d）
2. **工具错误结构化分类** ← claude-code error hints（P1，1d）
3. **跨 repo 调用循环检测** ← GitNexus DAG（P1，0.5d）

### 质量改进（Quality）— 2-3 周

4. **消息历史滑动窗口** ← claude-code 五层压缩（P0，1d）
5. **CallNode 可信度来源字段** ← GitNexus 强类型（P1，0.5d）
6. **补充 Prometheus metrics**（P2，1d）

### 长期投资（Long-term）— 4+ 周

7. **OpenTelemetry 链路追踪**（P2，2d）
8. **配置热重载**（P2，1d）
9. **工具结果 LRU cache** ← GitNexus lifecycle（P3，1d）
10. **Worker 结果回写 Layer1** ← autoresearch 记忆（P3，2d）

**总工作量：** ~11 人·天（包括测试）

**建议执行顺序：** P0 items 首先（稳定性 critical） → P1 items（质量显著提升） → P2 items（可观测性 + 运维） → P3 items（长期投资）
