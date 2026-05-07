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

## 第二轮分析：借鉴参考仓库（待补充）

> 参考仓库：/mnt/GitNexus、/mnt/claude-code、/mnt/autoresearch  
> 分析结果将在探索后补充到本文档。
