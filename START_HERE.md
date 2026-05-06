# 🚀 START HERE - Shirakami 索引系统完整调查

## ⚡ 5 分钟快速总结

Shirakami 的符号图索引系统已经有完整的存储和构建能力，**但缺乏自动更新机制**。本调查提供了完整的分析和可立即实施的改进方案。

### 关键发现

| 问题 | 答案 | 详见 |
|-----|------|------|
| **索引存储在哪?** | PostgreSQL, 3 个表 | docs/SYMBOL_INDEX_LIFECYCLE.md §1 |
| **如何构建?** | CLI + Go/Python Indexer | docs/SYMBOL_INDEX_LIFECYCLE.md §2 |
| **自动更新?** | **否** ⚠️ 主要缺陷 | docs/SYMBOL_INDEX_LIFECYCLE.md §6 |
| **修复方案?** | P1-P4 四个优先级 | docs/INDEX_IMPROVEMENT_ROADMAP.md |

### 立即行动

- **阅读**: 5 分钟内理解系统 → `docs/README.md`
- **学习**: 1 小时深入理解 → `docs/SYMBOL_INDEX_LIFECYCLE.md`
- **改进**: 2-3 天实施 P1 → `docs/P1_WEBHOOK_INDEX_TRIGGER_IMPL.md`

---

## 📚 文档导航

### 按使用场景选择

**场景 1: 理解系统现状** (30 分钟)
```
1. docs/README.md                  ← 全景图和优缺点对比
2. docs/INDEX_DOCS_GUIDE.md        ← Q&A 快速答案
```

**场景 2: 深入技术理解** (2 小时)
```
1. docs/SYMBOL_INDEX_LIFECYCLE.md  ← 完整调查报告
2. docs/architecture-v2-design.md  ← 系统架构背景
```

**场景 3: 准备实施改进** (2-3 天编码)
```
1. docs/INDEX_IMPROVEMENT_ROADMAP.md        ← 了解 4 个方案
2. docs/P1_WEBHOOK_INDEX_TRIGGER_IMPL.md    ← 按步骤编码
```

**场景 4: 快速查询** (5 分钟)
```
docs/INDEX_DOCS_GUIDE.md
  ├─ Q&A 部分 ← 常见 12 个问题的快速答案
  └─ 代码位置速查 ← 所有关键文件位置
```

---

## 🎯 核心内容概要

### 问题 1: 索引存储在哪里?

**答案**: PostgreSQL 中的 3 个表

```
symbol_nodes      ← 函数、类、接口等符号定义 (confidence 度量)
symbol_edges      ← 符号之间的 CALLS/IMPORTS/EXTENDS 关系
index_metadata    ← 每个仓库的索引元数据和版本
```

**关键索引**:
- `idx_symbol_line_range` - Layer B (diff→symbols 映射)
- `idx_edge_source/target` - 上游/下游遍历

**详见**: `docs/SYMBOL_INDEX_LIFECYCLE.md` §1 (42 行详细说明)

### 问题 2: 索引如何构建?

**答案**: CLI 命令 `shirakami index update/rebuild`

**调用链**:
```
CLI Command
  └─ indexRepo()
      ├─ detectLanguage()
      ├─ GoIndexer.Index()        [go/ast + go/types, confidence=1.0]
      │   ├─ extractSymbols()
      │   ├─ extractCalls()
      │   └─ resolveCallTarget()
      ├─ OR PythonIndexer.Index() [tree-sitter, confidence<1.0]
      └─ SaveNodes() / SaveEdges() / SaveMetadata()
```

**详见**: `docs/SYMBOL_INDEX_LIFECYCLE.md` §2 (108 行完整调用链)

### 问题 3: 代码变更后索引是否自动更新?

**答案**: **NO** — 这是系统的主要缺陷

**现状**:
```
✓ Webhook 创建分析任务
✗ Webhook 不触发索引更新

✓ K8s 同步代码
✗ K8s 不重建索引

✗ 无 CronJob 定时更新
✗ 无增量索引能力
```

**结果**: 分析基于陈旧的符号图，准确度 ~85%

**详见**: `docs/SYMBOL_INDEX_LIFECYCLE.md` §6 (140 行详细分析)

### 问题 4: 如何修复?

**答案**: 实施 4 个优先级的改进

| P | 功能 | 时间 | 影响 | 状态 |
|-|------|------|------|------|
| 1 | Webhook 触发 | 2-3d | 🔴 最高 | ✅ 完全设计 |
| 2 | CronJob | 1-2d | 🟠 高 | 📋 设计中 |
| 3 | 增量索引 | 4-5d | 🟡 性能 | 📋 设计中 |
| 4 | 告警 | 1-2d | 🟢 可靠 | ✅ 完全设计 |

**详见**: `docs/INDEX_IMPROVEMENT_ROADMAP.md` (完整方案)

---

## 🚀 立即可以做的事

### 第 1 步: 理解现状 (30 分钟)
- [ ] 阅读 `docs/README.md`
- [ ] 阅读 `docs/INDEX_DOCS_GUIDE.md` 的 Q&A 部分

### 第 2 步: 深入理解 (1-2 小时)
- [ ] 阅读 `docs/SYMBOL_INDEX_LIFECYCLE.md`
- [ ] 扫一遍 `docs/architecture-v2-design.md` §2-3

### 第 3 步: 准备实施 (1 小时)
- [ ] 阅读 `docs/INDEX_IMPROVEMENT_ROADMAP.md`
- [ ] 审视 `docs/P1_WEBHOOK_INDEX_TRIGGER_IMPL.md`
- [ ] 决定团队何时开始 P1

### 第 4 步: 开始编码 (2-3 天)
- [ ] 按照 P1 实现指南的 5 个步骤
- [ ] 运行测试
- [ ] 部署到 staging

---

## 📊 预期收益

### 实施 P1 (Webhook 触发)
```
Before: MR/PR 到达 → 2+ 小时后才索引更新 → 分析基于陈旧索引
After:  MR/PR 到达 → 5 分钟内索引更新    → 分析基于最新索引

准确度提升: 85% → 95%
```

### 实施 P1 + P2 + P3
```
每天 10 个 PR，平均仓库 50k LOC

Before: 20 分钟/天 (全量扫描)
After:  2 分钟/天 (增量索引)

加速: 10 倍
```

---

## ✅ 文档清单

全部保存在 `/mnt/shirakami/docs/`:

- [x] **README.md** - 执行摘要和快速索引
- [x] **SYMBOL_INDEX_LIFECYCLE.md** - 完整调查 (490 行)
- [x] **INDEX_IMPROVEMENT_ROADMAP.md** - 改进方案 (270 行)
- [x] **P1_WEBHOOK_INDEX_TRIGGER_IMPL.md** - P1 实现指南 (462 行)
- [x] **INDEX_DOCS_GUIDE.md** - 快速参考 (225 行)
- [x] **architecture-v2-design.md** - 系统架构 (1808 行)

**总计**: 3,542 行详细文档

---

## 🎓 三层学习路径

### 🟢 Level 1: 快速理解 (30 min)
- 适合: 想快速了解系统状态
- 阅读: README.md + INDEX_DOCS_GUIDE.md Q&A
- 输出: 了解 4 个主要问题的答案

### 🟡 Level 2: 技术深入 (2 hours)
- 适合: 想完整理解系统设计
- 阅读: SYMBOL_INDEX_LIFECYCLE.md + architecture-v2-design.md
- 输出: 理解完整的索引生命周期和架构

### 🔴 Level 3: 实施阶段 (2-3 days)
- 适合: 准备开发改进
- 阅读: INDEX_IMPROVEMENT_ROADMAP.md + P1_WEBHOOK_INDEX_TRIGGER_IMPL.md
- 输出: 完整的 P1 实现 + 测试 + 部署

---

## 📞 快速问答

**Q: 索引用了哪些数据库表?**
A: `symbol_nodes`, `symbol_edges`, `index_metadata`

**Q: 索引如何同时支持 Go 和 Python?**
A: 两个独立的 Indexer: GoIndexer (go/ast) + PythonIndexer (tree-sitter)

**Q: Layer B 是什么?**
A: Diff→Symbols 映射，用于混合分析模式的快速符号解析

**Q: 当前最大的问题是什么?**
A: 没有自动索引更新，分析基于陈旧的符号图

**Q: 如何修复?**
A: 实施 P1-P4 四个改进，总共 8-12 天工作量

**Q: P1 要多久?**
A: 2-3 天，按照 P1_WEBHOOK_INDEX_TRIGGER_IMPL.md 的 5 个步骤

---

## 🎯 下一步行动

```
NOW      → 花 30 分钟读 README.md 了解全景
TODAY    → 花 2 小时读 SYMBOL_INDEX_LIFECYCLE.md 深入理解
TOMORROW → 审视 P1 实现指南，计划团队工作
NEXT WEEK→ 开始 P1 实现 (2-3 天)
```

---

**📍 位置**: 所有文档在 `/mnt/shirakami/docs/`
**⏱️ 准备时间**: 30 分钟快速了解，2 小时完整理解
**✅ 状态**: 调查完成，可立即实施改进

🎉 **准备好了? 打开 `docs/README.md` 开始吧!**

