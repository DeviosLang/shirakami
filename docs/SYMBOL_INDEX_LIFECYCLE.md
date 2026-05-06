# Shirakami 符号图索引（Symbol Graph Index / Layer B）完整生命周期调查

## 1. 索引存储位置和格式

### 1.1 数据库表结构
**位置**: PostgreSQL 数据库（migrations/002_symbol_graph.sql）

三张主要表：

#### `symbol_nodes` 表
- **存储内容**: 所有符号定义（函数、方法、类、接口等）
- **主要字段**:
  - `id` (TEXT, PRIMARY KEY): "{repo}:{file}:{qualified_name}#{arity}" 格式
  - `repo` (TEXT): 仓库名称
  - `file_path` (TEXT): 文件相对路径
  - `name` (TEXT): 限定符名称，如 "PaymentService.Handle"
  - `kind` (TEXT): 符号类型（function/method/class/interface/struct/constant）
  - `start_line`, `end_line` (INTEGER): 符号代码行数范围
  - `signature` (TEXT): 函数签名（参数列表）
  - `commit_hash` (TEXT): 索引时的 git HEAD
  - `indexed_at` (TIMESTAMPTZ): 索引时间戳

- **索引**:
  - `idx_symbol_repo_name` - 按仓库+符号名称查询
  - `idx_symbol_file` - 按仓库+文件查询
  - `idx_symbol_line_range` - Layer B 使用：给定(repo, file, line range)查询重叠符号

#### `symbol_edges` 表
- **存储内容**: 符号之间的关系（CALLS、IMPORTS、EXTENDS、IMPLEMENTS）
- **主要字段**:
  - `id` (TEXT, PRIMARY KEY): 边标识符
  - `source_id`, `target_id` (TEXT, FOREIGN KEY): 符号节点ID
  - `type` (TEXT): 边类型（CALLS/IMPORTS/EXTENDS/IMPLEMENTS）
  - `file_path` (TEXT): 关系发生的文件
  - `line` (INTEGER): 调用/导入发生的行号
  - `confidence` (REAL): 信心度 0-1（1.0=确定）
  - UNIQUE(source_id, target_id, type)

- **索引**:
  - `idx_edge_target` - 上游遍历（查找谁调用我）
  - `idx_edge_source` - 下游遍历（查找我调用谁）

#### `index_metadata` 表
- **存储内容**: 各仓库索引元数据（用于陈旧度检测）
- **主要字段**:
  - `repo` (TEXT, PRIMARY KEY)
  - `commit_hash` (TEXT): 上次索引时的 HEAD
  - `indexed_at` (TIMESTAMPTZ)
  - `total_files`, `total_symbols`, `total_edges` (INTEGER): 统计数据
  - `language` (TEXT): 检测到的主要语言
  - `duration_ms` (INTEGER): 索引耗时

### 1.2 内存表示
**位置**: `internal/index/graph.go` 中的 `InMemoryGraph` 结构体

```go
type InMemoryGraph struct {
    nodes    map[string]*SymbolNode      // ID → 节点
    inEdges  map[string][]SymbolEdge     // target_id → 边列表（谁调用我？）
    outEdges map[string][]SymbolEdge     // source_id → 边列表（我调用谁？）
}
```

- 在启动时从 PostgreSQL 加载到内存
- 支持微秒级 BFS 遍历
- 用于混合模式和确定性模式分析

---

## 2. 索引构建流程（完整调用链）

### 2.1 索引构建的入口点

**Command**: `shirakami index update` / `shirakami index rebuild`
**位置**: `cmd/analyze/index.go`

```
buildIndexUpdateCmd() / buildIndexRebuildCmd()
    └─ indexRepo(ctx, store, repo, workspaceDir, fullRebuild)  [lines 246-316]
        ├─ getGitHEAD(repoPath)                                [lines 238-244]
        ├─ store.GetMetadata(ctx, repo.Name)                   [检查陈旧度]
        ├─ [fullRebuild] store.DeleteByRepo(ctx, repo.Name)     [行 264-266]
        │
        ├─ detectLanguage(repoPath)                            [行 222-236]
        │
        ├─ Go 仓库:
        │   └─ NewGoIndexer(repo.Name, repoPath, head)
        │       └─ indexer.Index()                             [internal/index/indexer_go.go:40-88]
        │           ├─ packages.Load("./...")                  [行 51]
        │           ├─ extractSymbols()                        [行 79 for each file]
        │           │   ├─ funcDeclToNode()                    [行 117-156]
        │           │   └─ typeSpecToNode()                    [行 159-191]
        │           └─ extractCalls()                          [行 83]
        │               └─ findEnclosingFunc() + resolveCallTarget()
        │
        ├─ Python 仓库:
        │   └─ NewPythonIndexer(repo.Name, repoPath, head)
        │       └─ indexer.Index()                             [internal/index/indexer_python.go]
        │
        ├─ store.SaveNodes(ctx, result.Nodes)                  [行 289]
        │   └─ INSERT INTO symbol_nodes (upsert on conflict)   [store.go:62-91]
        │
        ├─ store.SaveEdges(ctx, result.Edges)                  [行 292]
        │   └─ INSERT INTO symbol_edges (upsert on conflict)   [store.go:93-121]
        │
        └─ store.SaveMetadata(ctx, meta)                       [行 309]
            └─ INSERT INTO index_metadata (upsert)             [store.go:123-142]
```

### 2.2 Go 索引器详细流程

**文件**: `internal/index/indexer_go.go`

```
GoIndexer.Index()
├─ packages.Load(cfg, "./...") 
│  └─ 使用 go/packages 加载整个项目，进行类型检查
│
├─ for each package:
│  ├─ for each file (skip vendor/, *_test.go):
│  │  └─ extractSymbols(file, pkg, relPath)
│  │      ├─ ast.Inspect() 遍历 AST
│  │      ├─ FuncDecl → funcDeclToNode()
│  │      │  ├─ 提取函数名、接收者类型（方法）
│  │      │  ├─ arity = countParams()
│  │      │  └─ 生成 ID: "{repo}:{file}:{name}#{arity}"
│  │      │
│  │      └─ GenDecl → TypeSpec → typeSpecToNode()
│  │         ├─ 识别类型类型（struct/interface/class）
│  │         └─ 生成 ID: "{repo}:{file}:{name}#0"
│  │
│  └─ extractCalls(pkg)
│     └─ 使用 types.Info 提取调用关系
│        ├─ ast.Inspect() 查找 CallExpr
│        ├─ findEnclosingFunc() 确定调用者
│        ├─ resolveCallTarget() 确定被调用者
│        └─ 创建 SymbolEdge(source_id, target_id, CALLS, confidence=1.0)
│
└─ return IndexResult{Nodes, Edges, Files}
```

### 2.3 关键特性

- **Go 仓库**: 使用编译器级别的 `go/ast` + `go/types`，置信度 = 1.0
- **Python 仓库**: 使用树状解析（tree-sitter），置信度可能 < 1.0
- **增量更新**: `index update` 检查 commit_hash，只在 HEAD 变化时重新索引
- **完全重建**: `index rebuild` 删除现有数据后重新索引

---

## 3. 代码变更后的索引更新流程

### 3.1 **自动更新机制的状态**

**结论**: 当前 **不存在完全的自动更新机制**。

### 3.2 手动更新方式（当前实现）

#### 方式 1: CLI 命令
```bash
# 增量更新（仅处理改变的文件）
shirakami index update [--repo <name>]

# 完全重建（删除 + 重新索引）
shirakami index rebuild [--repo <name>]

# 检查陈旧度
shirakami index check [--repo <name>]
```

#### 方式 2: 工作空间同步
```bash
# 同步所有仓库的代码（git pull/clone）
shirakami workspace sync

# 位置: cmd/analyze/main.go:645-677
```

### 3.3 K8s 部署中的同步流程

**文件**: `k8s/job.yaml`

```yaml
initContainers:
  - name: clone-repos
    image: alpine/git:latest
    command:
      - sh
      - -c
      - |
        # 对每个仓库：
        clone_or_pull() {
          NAME=$1; URL=$2
          DIR="${WS}/${NAME}"
          if [ -d "${DIR}/.git" ]; then
            echo "[${NAME}] pulling..."
            cd "${DIR}" && git pull --ff-only
          else
            echo "[${NAME}] cloning..."
            git clone --depth=50 "${URL}" "${DIR}"
          fi
        }
        # 并行克隆/拉取所有仓库 (&)
```

**当前流程**:
1. initContainer 阶段：`git clone/pull` 所有仓库
2. 主容器运行分析（无索引更新步骤）
3. **缺失**: 没有自动调用 `shirakami index rebuild` 的触发器

### 3.4 webhook 处理（仅处理分析任务）

**文件**: `internal/webhook/handler.go`

```go
// webhook 流程：
ParsedEvent (GitLab MR Hook / GitHub PR event)
    ├─ verifyGitLab() / verifyGitHub() [签名验证]
    ├─ parseGitLabEvent() / parseGitHubEvent() [提取 diff]
    ├─ store.CreateTask() [创建任务]
    ├─ Launch(taskID, diff, desc) [启动分析]
    └─ Commenter.PostComment() [回复评论]

// **重要**: webhook 处理器目前只触发分析任务，不触发索引更新
```

### 3.5 分析时的索引使用（Layer B）

**文件**: `cmd/analyze/main.go:321-358`

```go
// 分析过程中的索引加载
if indexMode != "off" && pool != nil {
    idxStore := index.NewStore(pool)
    nodes, _ := idxStore.LoadAllNodes(ctx, repoNames)
    edges, _ := idxStore.LoadAllEdges(ctx, repoNames)
    if len(nodes) > 0 {
        graph := index.NewInMemoryGraph()
        graph.Load(nodes, edges)
        orch.SetIndexGraph(&graphAdapter{graph: graph})
    }
}
```

### 3.6 orchestrator 中的索引应用

**文件**: `internal/agent/orchestrator.go`

```go
// 混合模式流程：
1. extractChangedFunctions(ctx, input) [行 614-xxx]
   └─ 使用 DiffToSymbols (Layer B) 映射 diff 到索引符号
      └─ store.FindSymbolsByLineRange(ctx, repo, file, startLine, endLine)
         [internal/index/store.go:225-249]

2. runGraphAnalysis(changedFunctions, sourceRepo) [行 1666-1749]
   ├─ 通过 resolver.Resolver.ImpactMany() 执行图遍历
   │  └─ 返回风险等级、入口点、跨仓库边等
   └─ 未覆盖的符号返回给 LLM 作为降级

3. indexMode 支持:
   - "off":          无索引，纯 LLM 模式
   - "shadow":       图分析与 LLM 对比（用于验证）
   - "hybrid":       图优先，LLM 处理未覆盖符号
   - "deterministic": 仅使用图，无 LLM 降级
```

---

## 4. 关键文件及行号列表

### 4.1 索引存储（Store 层）
| 文件 | 主要内容 | 行号 |
|------|--------|------|
| `internal/index/store.go` | Store 结构体和 DB 操作 | 51-250 |
|  | SaveNodes() | 62-91 |
|  | SaveEdges() | 93-121 |
|  | SaveMetadata() | 123-142 |
|  | FindSymbolsByLineRange() (Layer B) | 225-249 |
|  | LoadAllNodes() | 203-223 |
|  | LoadAllEdges() | 179-201 |

### 4.2 Go 索引器
| 文件 | 主要内容 | 行号 |
|------|--------|------|
| `internal/index/indexer_go.go` | GoIndexer 结构体 | 16-29 |
|  | Index() 入口 | 40-88 |
|  | extractSymbols() | 91-114 |
|  | funcDeclToNode() | 117-156 |
|  | typeSpecToNode() | 159-191 |
|  | extractCalls() | 194-249 |
|  | findEnclosingFunc() | 252-272 |
|  | resolveCallTarget() | 275-310 |

### 4.3 Python 索引器
| 文件 | 主要内容 | 行号 |
|------|--------|------|
| `internal/index/indexer_python.go` | PythonIndexer 结构体 + Index() | 1-250+ |

### 4.4 图数据结构
| 文件 | 主要内容 | 行号 |
|------|--------|------|
| `internal/index/graph.go` | InMemoryGraph 结构体 | 5-28 |
|  | Load() | 32-40 |
|  | Impact() (BFS 遍历) | 88-161 |
|  | FindNodesByName() 等查询 | 59-79 |

### 4.5 diff 到符号映射（Layer B）
| 文件 | 主要内容 | 行号 |
|------|--------|------|
| `internal/index/diff_to_symbols.go` | DiffToSymbols() | 39-70 |

### 4.6 CLI 命令
| 文件 | 主要内容 | 行号 |
|------|--------|------|
| `cmd/analyze/index.go` | buildIndexUpdateCmd() | 38-77 |
|  | buildIndexRebuildCmd() | 83-121 |
|  | buildIndexCheckCmd() | 127-177 |
|  | indexRepo() (主要逻辑) | 246-316 |
|  | getIndexableRepos() | 204-220 |
|  | detectLanguage() | 222-236 |
|  | getGitHEAD() | 238-244 |

### 4.7 Workspace 同步
| 文件 | 主要内容 | 行号 |
|------|--------|------|
| `internal/workspace/sync.go` | SyncAll() | 36-66 |
|  | syncRepo() (git pull/clone) | 69-97 |
|  | currentCommit() | 100-107 |
| `cmd/analyze/main.go` | buildWorkspaceSyncCmd() | 645-677 |

### 4.8 Webhook 处理
| 文件 | 主要内容 | 行号 |
|------|--------|------|
| `internal/webhook/handler.go` | Handler.ServeHTTP() | 87-162 |
|  | parseGitLabEvent() | 275-318 |
|  | parseGitHubEvent() | 350-387 |
|  | **注意**: 无索引更新逻辑 | - |

### 4.9 Orchestrator 中的索引使用
| 文件 | 主要内容 | 行号 |
|------|--------|------|
| `internal/agent/orchestrator.go` | SetIndexGraph() | 195-199 |
|  | SetIndexMode() | 201-204 |
|  | SetResolver() | 220-228 |
|  | extractChangedFunctions() (Layer B 使用) | 614-xxx |
|  | runGraphAnalysis() | 1666-1749 |
|  | runGraphAnalysisViaResolver() | 1754-1850+ |

### 4.10 数据库迁移
| 文件 | 主要内容 | 行号 |
|------|--------|------|
| `migrations/002_symbol_graph.sql` | symbol_nodes 表定义 | 4-23 |
|  | symbol_edges 表定义 | 25-41 |
|  | index_metadata 表定义 | 43-53 |

### 4.11 K8s 部署
| 文件 | 主要内容 | 行号 |
|------|--------|------|
| `k8s/job.yaml` | initContainer clone-repos | 275-400+ |
|  | 主容器 analyze 命令 | 450-500+ |

---

## 5. 陈旧度检测机制

### 5.1 检测方式

**在 analyze 前**:
```go
// cmd/analyze/index.go:150-172
store.GetMetadata(ctx, repo.Name)  // 获取 indexed_at 和 commit_hash
meta.CommitHash != head            // 对比 git HEAD
→ 状态为 "STALE" 则需要手动 update
```

**命令**:
```bash
shirakami index check [--repo <name>]  # 只读，显示状态
```

### 5.2 自动 vs 手动

| 操作 | 是否自动 | 触发方式 |
|------|---------|---------|
| 符号提取 | 否 | 手动运行 `shirakami index update/rebuild` |
| 代码同步 | 否 | 手动运行 `shirakami workspace sync` |
| 分析时加载 | 是 | `shirakami analyze` 时自动从 DB 加载 |
| webhook 触发分析 | 是 | webhook 事件自动触发分析 |
| webhook 触发索引更新 | **否** | **不存在** |

---

## 6. 缺口与改进建议

### 6.1 当前缺口

1. **无 webhook 触发索引更新**: 新 commit/push 到仓库时，索引不会自动更新
   - 分析任务会检测 index 陈旧，但会回退到 LLM 模式（性能下降）

2. **无 git hook**: 本地 push 后无触发机制

3. **无 CronJob**: K8s 中无定时任务更新索引

4. **无增量文件检测**: `index update` 不使用 git diff，而是全量扫描

### 6.2 可能的改进方案

1. **在 webhook 中添加索引更新**:
   ```go
   webhook.Handler.ServeHTTP()
   ├─ 创建分析任务 (现有)
   └─ 触发索引更新 (需要添加)
   ```

2. **添加 K8s CronJob** 定时更新索引

3. **实现增量索引**: 使用 `git diff HEAD~1..HEAD` 只索引变化的文件

4. **改进 Layer B**: 
   - 使用 git diff 获取精确改变行号
   - 加速 symbol 查询

---

## 7. 信息流图

```
┌─ 仓库代码变更 (git commit/push)
│
├─ webhook 事件 (GitLab MR / GitHub PR)
│  └─ internal/webhook/handler.go:87-162
│     ├─ 签名验证
│     ├─ 创建分析任务 ✓
│     └─ 触发索引更新 ✗ (缺失)
│
├─ shirakami analyze --config ...
│  └─ cmd/analyze/main.go:139-450
│     ├─ 检查 index 陈旧度 [STALE → 警告]
│     ├─ 加载 index graph (if mode != off)
│     │  └─ internal/agent/orchestrator.go:321-358
│     ├─ extractChangedFunctions() [Layer B]
│     │  └─ DiffToSymbols() [使用 idx_symbol_line_range]
│     ├─ runGraphAnalysis() [确定性]
│     │  └─ resolve.Resolver.ImpactMany()
│     └─ LLM fallback [未覆盖符号]
│
├─ shirakami index update [--repo]  (手动)
│  └─ cmd/analyze/index.go:246-316
│     ├─ 检查陈旧度 [currentCommit vs indexed_at]
│     ├─ detectLanguage()
│     ├─ GoIndexer.Index() 或 PythonIndexer.Index()
│     ├─ SaveNodes() → symbol_nodes ✓
│     ├─ SaveEdges() → symbol_edges ✓
│     └─ SaveMetadata() → index_metadata ✓
│
└─ shirakami index check (只读)
   └─ 显示陈旧状态
```

---

## 总结

### 存储位置
- PostgreSQL: `symbol_nodes`, `symbol_edges`, `index_metadata` 表
- 内存: `InMemoryGraph` (启动时加载)

### 构建流程
- 入口: `cmd/analyze/index.go` 中的 CLI 命令
- Go: `go/ast` + `go/types` (confidence=1.0)
- Python: 树状解析 (confidence < 1.0)
- 调用链: Index() → SaveNodes/Edges/Metadata()

### 更新机制
- **无自动更新**（当前）
- 手动方式: `shirakami index update/rebuild`
- Workspace 同步: `shirakami workspace sync`
- 缺失: webhook 和 CronJob 触发器

### Layer B (diff→符号映射)
- 使用: `DiffToSymbols()` → `FindSymbolsByLineRange()`
- 索引支持: `idx_symbol_line_range` (repo, file_path, start_line, end_line)
- 用途: 混合模式的快速符号解析

### 改进空间
1. 添加 webhook 触发索引更新
2. 实现增量索引 (git diff)
3. 添加 K8s CronJob
4. 改进 Layer B 的查询效率
