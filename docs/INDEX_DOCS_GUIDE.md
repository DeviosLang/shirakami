# Shirakami 索引系统文档导航

## 快速索引

这个目录包含关于 Shirakami 符号图索引的完整文档。根据你的需求选择：

### 📚 调查和分析文档

#### 1. **SYMBOL_INDEX_LIFECYCLE.md** ⭐ 从这里开始
   - **内容**: 完整的符号图索引生命周期调查
   - **长度**: ~500 行
   - **涵盖**:
     - 索引存储位置（PostgreSQL 三表设计）
     - 索引构建流程（完整调用链）
     - 更新机制现状（无自动更新的原因）
     - Layer B 实现细节
     - 信息流图
   - **阅读场景**: 想理解索引系统的全貌、答疑生产问题
   - **关键发现**: 无完整的自动更新机制，webhook 和 K8s 都缺乏触发器

#### 2. **architecture-v2-design.md**
   - **内容**: 完整的 Shirakami 架构设计文档
   - **长度**: ~2000 行
   - **涵盖**:
     - 分层架构（Layer A/B/C/D）
     - 混合分析流程
     - 索引系统作为 Layer B 的角色
   - **阅读场景**: 需要理解索引在整个系统中的位置
   - **相关章节**: §2 Overview, §3.2 Layer B

### 🚀 改进和实现文档

#### 3. **INDEX_IMPROVEMENT_ROADMAP.md** ⭐ 如果要改进系统，读这个
   - **内容**: 按优先级排序的改进方案
   - **长度**: ~400 行
   - **包含**:
     - 4 个优先级的改进方案
     - 实现时间表
     - 监控指标定义
   - **优先级**:
     1. ✅ **P1** (2-3 天): Webhook 中自动触发索引更新
     2. ✅ **P2** (1-2 天): K8s CronJob 定时更新
     3. ✅ **P3** (4-5 天): 增量索引实现
     4. ✅ **P4** (1-2 天): 陈旧度告警改进

#### 4. **P1_WEBHOOK_INDEX_TRIGGER_IMPL.md** ⭐ 准备开始 P1 实现时读这个
   - **内容**: Priority 1 详细实现指南
   - **长度**: ~400 行
   - **包含**:
     - 完整的代码示例
     - 逐步的实现步骤
     - 测试策略
     - 部署检查清单
   - **即插即用**: 可直接按照步骤实现
   - **文件列表**:
     - `internal/index/updater.go` (新建)
     - `internal/storage/index_updater.go` (新建)
     - `internal/webhook/handler.go` (修改)
     - `cmd/server/main.go` (修改)

---

## 文档之间的关联

```
SYMBOL_INDEX_LIFECYCLE.md
    ↓ (发现问题)
INDEX_IMPROVEMENT_ROADMAP.md
    ├─ P1: 优先级最高，影响最大
    │   └─ P1_WEBHOOK_INDEX_TRIGGER_IMPL.md (详细实现)
    │       └─ 包含代码示例和测试用例
    │
    ├─ P2: K8s CronJob (待详细实现指南)
    ├─ P3: 增量索引 (待详细实现指南)
    └─ P4: 陈旧度告警 (待详细实现指南)
```

---

## 关键问题 Q&A

### Q: 索引存储在哪里？
**A**: PostgreSQL，三张表
- `symbol_nodes`: 符号定义（函数、类等）
- `symbol_edges`: 符号之间的调用关系
- `index_metadata`: 每个仓库的索引元数据

📖 详见: `SYMBOL_INDEX_LIFECYCLE.md` §1

### Q: 索引如何构建？
**A**: CLI 命令 `shirakami index update/rebuild`
- 调用链: CLI → GoIndexer/PythonIndexer → SaveNodes/Edges
- Go 使用 go/ast + go/types (confidence=1.0)
- Python 使用 tree-sitter (confidence<1.0)

📖 详见: `SYMBOL_INDEX_LIFECYCLE.md` §2

### Q: 代码变更后索引是否自动更新？
**A**: **否**，这是当前的主要问题
- Webhook 创建分析任务 ✓
- Webhook 不触发索引更新 ✗
- K8s initContainer 同步代码 ✓
- K8s initContainer 不重建索引 ✗
- 无 CronJob 定时更新 ✗

📖 详见: `SYMBOL_INDEX_LIFECYCLE.md` §6

### Q: 如何修复缺失的自动更新？
**A**: 按优先级实现四个改进

| 优先级 | 方案 | 工作量 | 影响 |
|-------|------|--------|------|
| P1 | Webhook 触发 | 2-3天 | 最高 |
| P2 | CronJob | 1-2天 | 高 |
| P3 | 增量索引 | 4-5天 | 性能 |
| P4 | 告警 | 1-2天 | 可靠性 |

📖 详见: `INDEX_IMPROVEMENT_ROADMAP.md`

### Q: P1 如何实现？
**A**: 5 个步骤
1. 定义 IndexUpdater 接口
2. 在 storage 层实现
3. 更新 webhook Config
4. 修改 webhook 处理函数
5. 在服务器启动时连接

📖 详见: `P1_WEBHOOK_INDEX_TRIGGER_IMPL.md`

### Q: Layer B 是什么？
**A**: Diff to Symbols 映射
- 将 git diff 中的改变行号映射到修改的符号
- 使用 `DiffToSymbols()` 函数
- 数据库索引: `idx_symbol_line_range`
- 用途: 混合分析模式的快速符号解析

📖 详见: `SYMBOL_INDEX_LIFECYCLE.md` §5

---

## 代码位置速查

### 索引存储
```
migrations/002_symbol_graph.sql  ← 表定义
internal/index/store.go          ← SaveNodes/Edges/Metadata
```

### 索引构建
```
cmd/analyze/index.go                 ← CLI 入口
internal/index/indexer_go.go         ← Go indexer
internal/index/indexer_python.go     ← Python indexer
```

### 索引图遍历
```
internal/index/graph.go          ← InMemoryGraph, Impact() BFS
internal/resolve/resolve.go      ← Resolver, 风险评估
internal/index/diff_to_symbols.go  ← Layer B 实现
```

### Webhook 处理
```
internal/webhook/handler.go      ← 当前缺乏索引触发器
cmd/server/main.go               ← 需要添加 index updater
```

---

## 快速开始路径

### 如果你想...

**理解系统**: 阅读顺序
1. `SYMBOL_INDEX_LIFECYCLE.md` (30分钟)
2. `architecture-v2-design.md` §2-3 (30分钟)

**改进系统**: 阅读顺序
1. `SYMBOL_INDEX_LIFECYCLE.md` (30分钟) - 了解现状
2. `INDEX_IMPROVEMENT_ROADMAP.md` (20分钟) - 了解方案
3. `P1_WEBHOOK_INDEX_TRIGGER_IMPL.md` (1小时) - 准备编码
4. 开始 P1 实现！

**修复 Bug**: 
1. 查阅 `SYMBOL_INDEX_LIFECYCLE.md` 找相关代码位置
2. 跟踪调用链，找到症状根源
3. 使用 `INDEX_IMPROVEMENT_ROADMAP.md` 评估影响

**部署到生产**:
1. 完成 P1 实现
2. 按 `P1_WEBHOOK_INDEX_TRIGGER_IMPL.md` 的检查清单验证
3. 监控关键指标 (index staleness, update latency)

---

## 更新历史

| 日期 | 文档 | 内容 |
|------|------|------|
| 2026-05-06 | SYMBOL_INDEX_LIFECYCLE.md | 初版，完整生命周期调查 |
| 2026-05-06 | INDEX_IMPROVEMENT_ROADMAP.md | 改进方案和优先级 |
| 2026-05-06 | P1_WEBHOOK_INDEX_TRIGGER_IMPL.md | P1 详细实现指南 |
| 2026-05-06 | INDEX_DOCS_GUIDE.md | 本文档，导航和 Q&A |

---

## 相关资源

- 🏗️ `architecture-v2-design.md` - 完整架构
- 📊 `internal/index/graph.go` - InMemoryGraph 实现
- 🐍 `internal/index/indexer_python.go` - Python 索引器
- 🔧 `cmd/analyze/index.go` - CLI 命令实现
- ☸️ `k8s/job.yaml` - K8s 部署配置

---

## 反馈和问题

如果文档有不清楚的地方或发现错误，请：

1. 检查相关源代码文件
2. 如与源码不符，更新本文档
3. 记录到 issue 系统

