# Shirakami 改进方案

> 文档版本：2026-05-08  
> 背景任务：task `341785f0`（`cvm_api`，46 分钟，0 个 entry points）

---

## 目录

1. [背景与问题根因](#背景与问题根因)
2. [跨仓分析流程说明](#跨仓分析流程说明)
3. [改进 1：Layer A+ 磁盘回退扫描](#改进-1layer-a-磁盘回退扫描) ✅
4. [改进 2：夜间定时索引（FIFO，04:00 CST）](#改进-2夜间定时索引) ✅
5. [改进 3：indexMode 改为 hybrid](#改进-3indexmode-改为-hybrid) ✅
6. [改进 4：diff snippet 注入 Worker prompt](#改进-4diff-snippet-注入-worker-prompt) ✅
7. [改进 5：全局变量改动的 FILE_CHANGED_VAR sentinel](#改进-5全局变量改动的-file_changed_var-sentinel) ✅
8. [改进 6：多子类继承父类场景的分析能力](#改进-6多子类继承父类场景的分析能力)（P0 ✅ P1 ✅ P2 ✅）
9. [改进 7：Worker Prompt 重构——步骤驱动 → 目标+约束](#改进-7worker-prompt-重构步骤驱动--目标约束) ✅
10. [长期记忆：模块上下游关系积累](#长期记忆模块上下游关系积累)
11. [改动文件汇总](#改动文件汇总)
12. [验证方案](#验证方案)

---

## 背景与问题根因

### Layer A+ 的工作原理

`extractChangedFunctions` 是一个四层漏斗：

```
输入: unified diff
  └─ Layer A   ParseDiffHunks()        纯文本解析 @@-行 → DiffHunk{File, StartLine, FuncContext}
  └─ Layer A+  hunk.FuncContext != ""   使用 @@ 行末的函数名（git 内置 diff driver 提供）
  └─ Layer B   DiffToSymbols()          DB 索引匹配（indexMode != "off" 时启用）
  └─ LLM C    fallback                  LLM 从 diff 文本直接推断函数名
  └─ ensureDiffFileCoverage()           兜底：生成 FILE_CHANGED:repo/path.py 哨兵
```

**`FuncContext`** 来自 `git diff` 的 `@@` 行末尾部分，例如：

```
@@ -209,10 +209,10 @@ def _check_disable(self, region, channel):
                                         ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
                                         FuncContext
```

这需要 `.git/info/attributes` 中配置语言 diff driver（如 `*.py diff=python`）。
若仓库缺少该配置，**所有** `@@` 行尾部均为空，Layer A+ 全部 `continue`。

### `FuncContext == ""` 与全局变量

全局变量（如 `white_set = {...}`）本就不在任何函数内：
- `FuncContext == ""` — 正确（git 不会填充）
- 磁盘扫描返回 `""` — 正确（向上 100 行找不到 `def`/`func`）
- 后续走 `FILE_CHANGED_VAR` 路径 — 行为符合预期

两个场景（无 `.gitattributes` 的仓库 vs 全局变量）都落到 `FuncContext == ""`，
但原因和处理方式完全不同，改进方案对两者各有针对。

### 复现案例

task `341785f0`：`cvm_api` 仓库无 `.gitattributes` → 所有 `@@` 的 `FuncContext` 均为空
→ 46 分钟只产出 0 个 entry points（`FILE_CHANGED` 哨兵触发整文件盲搜）。

---

## 跨仓分析流程说明

```
输入: branches = [{repo: "art_api", branch: "feature/xxx"}, {repo: "cvm_api", branch: "feature/yyy"}]

Round 0（初始 diff 解析）
  ├─ 对每个分支 git diff HEAD → 解析 DiffHunk
  └─ extractChangedFunctions → initial ChangedFunctions per repo

Round 1（第一轮 Workers）
  ├─ Triage → P0/P1/P2 分级
  ├─ Workers（最多 6 个并发）
  │   ├─ art_api Worker: 追踪 art_api 内调用链 → cross_repo_calls: [{target: "cvm_api", func: "dispatch"}]
  │   └─ cvm_api Worker: 追踪 cvm_api 内调用链 → cross_repo_calls: [{target: "vstation_network", func: "handler"}]
  └─ 收集 nextPending:
        cvm_api   += ["dispatch"]        ← 来自 art_api Worker
        vstation_network += ["handler"]  ← 来自 cvm_api Worker

Round 2（第二轮 Workers）
  ├─ cvm_api Worker: 追踪 ["dispatch"] 在 cvm_api 的调用链
  └─ vstation_network Worker: 追踪 ["handler"] ...

... 重复，最多 10 轮（deep mode）/ 3 轮（fast mode）

终止条件：
  - nextPending 为空（无新的跨仓调用）
  - 到达 role="entry" 仓库（入口层，如 vstation_network 是网络层）
  - 达到最大轮数
```

**关键设计**：
- Workers 在同一轮内并发执行（相互独立）
- `cross_repo_calls` 的去重和聚合在 Orchestrator 层完成
- `role="entry"` 配置在 `shirakami.yaml` 的 repos 列表中，entry 仓库的 Worker 会生成最终的 `entry_points`

---

## 改进 1：Layer A+ 磁盘回退扫描 ✅

### 问题

`FuncContext == ""` 时 Layer A+ 直接 `continue`，整文件落到 `FILE_CHANGED` 哨兵（5-6 分钟/文件）。

### 方案

对 `FuncContext == ""` 的 hunk，先尝试从磁盘文件的 `StartLine` 向上扫描 100 行找函数定义。
找到则精确定位函数；找不到再走原有兜底路径（全局变量会在此静默降级到改进5）。

### 改动 1a：`internal/tool/gitdiff.go`

新增 `sourceFuncDefREs` 和导出函数 `ResolveFuncAtLine()`：

```go
// sourceFuncDefREs 匹配磁盘源文件中的函数/方法定义（无 diff +/- 前缀）。
var sourceFuncDefREs = []*regexp.Regexp{
    // Python
    regexp.MustCompile(`^(?:async\s+)?def\s+(\w+)\s*\(`),
    // Go
    regexp.MustCompile(`^func\s+(?:\([^)]+\)\s+)?(\w+)\s*\(`),
    // JS/TS 普通函数
    regexp.MustCompile(`^(?:async\s+)?function\s+(\w+)\s*\(`),
    // JS/TS 箭头函数赋值
    regexp.MustCompile(`^(?:export\s+)?(?:const|let|var)\s+(\w+)\s*=\s*(?:async\s*)?\(`),
    // Java / C++ / C# 方法（宽松匹配）
    regexp.MustCompile(`^\s*(?:public|private|protected|static|virtual|override|inline|\s)*\w[\w<>*&\[\]]*\s+(\w+)\s*\(`),
}

// ResolveFuncAtLine 从 lines（0-indexed 切片）中，从 startLine（1-based）向上扫描
// 最多 maxScan 行，返回最近的函数/方法名。找不到时返回 ""。
func ResolveFuncAtLine(lines []string, startLine, maxScan int) string {
    if startLine < 1 || len(lines) == 0 {
        return ""
    }
    idx := startLine - 1
    if idx >= len(lines) {
        idx = len(lines) - 1
    }
    limit := idx - maxScan
    if limit < 0 {
        limit = 0
    }
    for i := idx; i >= limit; i-- {
        line := strings.TrimSpace(lines[i])
        for _, re := range sourceFuncDefREs {
            if m := re.FindStringSubmatch(line); len(m) > 1 {
                return m[1]
            }
        }
    }
    return ""
}
```

### 改动 1b：`internal/agent/orchestrator.go`

**a. Layer A+ 循环**（约 line 749，`if h.FuncContext == ""` 处）：

```go
if h.FuncContext == "" {
    // 新增：行号锚点 → 磁盘扫描回退
    if h.StartLine > 0 {
        funcName := o.resolveFuncNameFromDisk(h.File, h.StartLine, input.SourceRepo)
        if funcName != "" {
            filePath := h.File
            if input.SourceRepo != "" && !strings.HasPrefix(filePath, input.SourceRepo+"/") {
                filePath = input.SourceRepo + "/" + filePath
            }
            qualified := filePath + "::" + funcName
            if !seenCtx[qualified] {
                seenCtx[qualified] = true
                hunkContextFunctions = append(hunkContextFunctions, qualified)
            }
            if existing, ok := hunkLineHints[qualified]; !ok || h.StartLine < existing {
                hunkLineHints[qualified] = h.StartLine
            }
            log.Debugw("extract.layerA+.disk_resolved",
                "file", h.File, "start_line", h.StartLine, "func", funcName)
            continue
        }
    }
    continue // 磁盘也找不到（全局变量等），让 Layer B / LLM C / FILE_CHANGED_VAR 兜底
}
```

**b. 新增私有方法** `resolveFuncNameFromDisk()`（orchestrator.go 末尾附近）：

```go
// resolveFuncNameFromDisk 从工作区的磁盘文件中，从 startLine 向上扫描
// 最多 100 行，返回最近的函数/方法名。找不到时返回 ""。
func (o *Orchestrator) resolveFuncNameFromDisk(file string, startLine int, sourceRepo string) string {
    repoName := sourceRepo
    relPath := file

    // 尝试从 file 路径中分离 repoName/relPath
    if idx := strings.Index(file, "/"); idx > 0 {
        candidate := file[:idx]
        if o.repoExists(candidate) {
            repoName = candidate
            relPath = file[idx+1:]
        }
    }

    repoDir := o.repoPath(repoName)
    if repoDir == "" {
        return ""
    }

    data, err := os.ReadFile(filepath.Join(repoDir, relPath))
    if err != nil {
        return ""
    }
    return tool.ResolveFuncAtLine(strings.Split(string(data), "\n"), startLine, 100)
}
```

### 行为变化

| 场景 | 改动前 | 改动后 |
|------|--------|--------|
| `@@` 无函数名，repo 有磁盘文件 | FILE_CHANGED 盲搜（~5 min） | 精确函数名 + lineHint |
| `@@` 无函数名，磁盘无文件 | FILE_CHANGED | 同前（静默降级） |
| 全局变量改动 | FILE_CHANGED 整文件搜 | 磁盘返回 ""，走改进5 FILE_CHANGED_VAR |
| `@@` 有函数名 | 完全不变 | 完全不变 |

---

## 改进 2：夜间定时索引 ✅

### 方案

**不引入优先级机制**——直接复用现有的 `semaphore` FIFO。  
凌晨 04:00 CST 通常无用户任务，自然能抢到 semaphore。  
Pod 重启后 goroutine 随之消亡，重启后重新计算下一个 04:00，无需持久化。

### 改动：`cmd/server/main.go`

在 `runServer()` 初始化完成后启动 goroutine：

```go
go s.scheduleNightlyIndex(ctx)
```

```go
func (s *apiServer) scheduleNightlyIndex(ctx context.Context) {
    for {
        now := time.Now().In(time.FixedZone("CST", 8*3600))
        next := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
        if !next.After(now) {
            next = next.Add(24 * time.Hour)
        }
        select {
        case <-ctx.Done():
            return
        case <-time.After(next.Sub(now)):
        }
        s.runNightlyIndex(ctx)
    }
}

func (s *apiServer) runNightlyIndex(ctx context.Context) {
    log := logger.S()
    // 与 runAnalysis 完全一致：FIFO 等待 semaphore
    s.queueCounter.Add(1)
    s.semaphore <- struct{}{}
    s.queueCounter.Add(-1)
    defer func() { <-s.semaphore }()

    log.Infow("nightly_index.started")
    for _, r := range s.cfg.Workspace.Repos {
        repoDir := filepath.Join(s.cfg.Workspace.Dir, r.Name)
        if err := s.rebuildRepoIndex(ctx, repoDir, r.Name); err != nil {
            log.Warnw("nightly_index.repo_failed", "repo", r.Name, "err", err)
        } else {
            log.Infow("nightly_index.repo_done", "repo", r.Name)
        }
    }
    log.Infow("nightly_index.completed")
}
```

`rebuildRepoIndex` 复用 `cmd/analyze/index.go` 中 `indexRepo()` 逻辑（或直接 inline）。

---

## 改进 3：indexMode 改为 hybrid ✅

### 操作

无需改动 Go 代码。已在 `config/shirakami.example.yaml` 中将 `index_mode` 默认值改为 `hybrid`
并补充选项说明，使新部署开箱即用 Layer B 索引路径：

```yaml
# 可选值：off / shadow / hybrid / deterministic
index_mode: hybrid
```

或通过环境变量：

```bash
SHIRAKAMI_INDEX_MODE=hybrid
```

`"hybrid"` 满足 orchestrator.go 约 line 857 的条件（非 `""` 且非 `"off"`），激活 Layer B。

**前提**：需先有索引数据（改进 2 首次运行后，或手动执行 `shirakami workspace sync --index`）。

---

## 改进 4：diff snippet 注入 Worker prompt ✅

### 问题

Worker 只知道"哪些函数被改了 + 行号"，不知道"改了什么"。
LLM 生成场景时缺乏对改动逻辑的理解（例如无法知道 `white_set` 新增了 `Region`/`Channel` 字段）。

### 方案

在 `DiffHunk` 新增 `RawLines` 字段存储 hunk body（`+`/`-` 行，最多 50 行），随函数名一起传给 Worker。

### 改动 4a：`internal/tool/gitdiff.go`

`DiffHunk` 新增字段：

```go
type DiffHunk struct {
    File        string
    StartLine   int
    EndLine     int
    FuncContext string
    RawLines    string // hunk body（+/- 行，最多 50 行，含前缀）
    GlobalVar   string // 见改进5
}
```

`ParseDiffHunks` 在解析 hunk body 时收集 `+`/`-` 行，截断到 50 行后赋值给 `h.RawLines`。

### 改动 4b：`internal/agent/orchestrator.go`

新增 `hunkDiffSnippets := make(map[string]string)`。  
Layer A+ 中对每个解析出的 qualified 名：

```go
if h.RawLines != "" {
    if _, already := hunkDiffSnippets[qualified]; !already {
        hunkDiffSnippets[qualified] = h.RawLines
    }
}
```

`WorkerTask` 结构体新增字段：

```go
DiffSnippets map[string]string // funcQualified → diff snippet
```

### 改动 4c：`internal/agent/worker.go`

prompt 构建时，对有 snippet 的函数在函数名后追加：

```
- cvm_api/logic.py::_check_disable (changed around line 215)
  diff:
  -    white_set = {"purchaseSource"}
  +    white_set = {"purchaseSource", "Region", "Channel"}
```

---

## 改进 5：全局变量改动的 FILE_CHANGED_VAR sentinel ✅

### 问题

`white_set = {...}` 是模块级赋值，不在任何函数内：
- 当前结果：`FILE_CHANGED:art_api/path.py` → 整文件盲搜（LLM 不知道要找什么）

### 方案

对磁盘扫描也找不到函数名的 hunk，检测是否为全局变量赋值，生成更精确的 sentinel：

```
FILE_CHANGED_VAR:art_api/logic.py::white_set
```

Worker 收到该 sentinel 后，将搜索目标定向为：

```
[trace all call sites that reference variable 'white_set' in art_api/logic.py]
```

### 改动 5a：`internal/tool/gitdiff.go`

新增全局变量检测 regex，在 `ParseDiffHunks` 中填充 `h.GlobalVar`：

```go
// globalVarDefRE 匹配 diff 中的全局变量赋值行（允许有 + 前缀）
var globalVarDefRE = regexp.MustCompile(`^\+?([A-Za-z_]\w*)\s*=\s*[\{\[\(\"'0-9]`)

// 在解析 hunk body 时，若 FuncContext == "" 则尝试提取全局变量名
for _, bodyLine := range hunkBody {
    if m := globalVarDefRE.FindStringSubmatch(strings.TrimSpace(bodyLine)); len(m) > 1 {
        h.GlobalVar = m[1]
        break
    }
}
```

### 改动 5b：`internal/agent/orchestrator.go`

Layer A+ 磁盘扫描找不到函数名后（`funcName == ""`），检查 `h.GlobalVar`：

```go
if h.GlobalVar != "" {
    filePath := h.File
    if input.SourceRepo != "" && !strings.HasPrefix(filePath, input.SourceRepo+"/") {
        filePath = input.SourceRepo + "/" + filePath
    }
    sentinel := "FILE_CHANGED_VAR:" + filePath + "::" + h.GlobalVar
    if !seenCtx[sentinel] {
        seenCtx[sentinel] = true
        hunkContextFunctions = append(hunkContextFunctions, sentinel)
    }
    continue
}
```

### 改动 5c：`internal/agent/worker.go`

约 line 361 的 sentinel 处理逻辑，增加 `FILE_CHANGED_VAR:` 前缀处理：

```go
case strings.HasPrefix(fn, "FILE_CHANGED_VAR:"):
    // 格式：FILE_CHANGED_VAR:repo/path::varName
    parts := strings.SplitN(strings.TrimPrefix(fn, "FILE_CHANGED_VAR:"), "::", 2)
    if len(parts) == 2 {
        filePath, varName := parts[0], parts[1]
        resolved = append(resolved,
            fmt.Sprintf("[trace all call sites that reference variable '%s' in %s]", varName, filePath))
    }
```

---

## 改进 6：多子类继承父类场景的分析能力（P0 ✅ P1 ✅ P2 ✅）

参考 MR https://git.woa.com/vstation/network/-/merge_requests/304 — 多个子类继承同一父类，
父类某方法被修改（或子类 override 了父类方法）：

```python
class BaseHandler:
    def process(self, req):   # 父类方法被改动
        ...

class AHandler(BaseHandler):
    def process(self, req):   # 子类1 override
        ...

class BHandler(BaseHandler):
    def process(self, req):   # 子类2 override
        ...

def dispatch(handler: BaseHandler, req):
    handler.process(req)      # 多态调用，实际执行哪个子类取决于运行时
```

### 当前能力差距

| 场景 | 当前能力 |
|------|---------|
| 改动父类方法，搜其调用方 | ✅ ripgrep 找 `process(` → 找到 `dispatch` |
| 调用方用父类类型引用，实际走子类 | ❌ 无法自动找到所有子类实现 |
| diff 改动子类 override 方法 | ✅ 能找直接调用；但通过父类类型的调用会漏 |
| 找出父类所有子类实现 | ❌ 无 "find all implementations" 机制 |
| LSP `textDocument/implementation` | ❌ 当前只暴露 `incomingCalls`/`outgoingCalls` |
| systemPromptTmpl 继承指引 | ❌ 完全没有 |

### 方案（三层）

#### P0：系统 prompt 增加继承/多态追踪指引（零代码，立刻收益）✅

**改动 `internal/agent/prompt.go`** 的 `systemPromptTmpl`，在 "Step 2" 之前增加：

```
### Inheritance & Polymorphism (IMPORTANT)

When you encounter a method that may be overridden by subclasses:
  1. Search for all subclasses: ripgrep({"pattern": "class \\w+\\(ClassName\\)", "repo": "<repo>"})
  2. Check if each subclass also defines this method (override).
  3. If the caller uses a base-class type reference (e.g. handler: BaseHandler), it may invoke
     ANY subclass implementation at runtime — include all overrides in the call chain.
  4. Use lsp_call_hierarchy with operation="findImplementations" to find all concrete
     implementations of an abstract/interface method (most accurate).

When you see a diff that modifies a method inside a class:
  - Check if this class is inherited elsewhere: ripgrep({"pattern": "class \\w+\\(<ClassName>\\)"})
  - Check if subclasses override the same method: ripgrep({"pattern": "def <method_name>\\("})
  - Treat each subclass override as an ADDITIONAL changed function to trace.
```

#### P1：LSP Tool 增加 `findImplementations` 操作（语义级精度）✅

**改动 `internal/tool/lsp.go`**：

1. operation 校验放开 `findImplementations`：

```go
if inp.Operation != "incomingCalls" && inp.Operation != "outgoingCalls" && inp.Operation != "findImplementations" {
    return "", fmt.Errorf("lsp_call_hierarchy: operation must be incomingCalls, outgoingCalls, or findImplementations")
}
```

2. 增加处理分支：

```go
case "findImplementations":
    implParams := map[string]interface{}{
        "textDocument": map[string]interface{}{"uri": fileURI},
        "position": map[string]interface{}{
            "line":      inp.Line - 1,
            "character": inp.Character - 1,
        },
    }
    implResult, err := t.sendRequest(ctx, "textDocument/implementation", implParams)
    if err != nil {
        return "No implementations found (LSP error).", nil
    }
    // 解析 Location[] 或 LocationLink[] 返回
    // 格式化输出：repo/file:line — funcName
    return formatImplementations(implResult), nil
```

3. Schema 描述中新增 operation 选项：

```json
"findImplementations" — find all concrete implementations of an abstract/interface method
```

#### P2：`gitdiff.go` 识别继承 diff 上下文 ✅

**改动 `internal/tool/gitdiff.go`**：

扩展 `classContextRE` 同时捕获父类名：

```go
// 原：只捕获类名
var classContextRE = regexp.MustCompile(`^\s*class\s+(\w+)`)

// 新：同时捕获父类（可选）
// class Child(Parent): → groups[1]=Child, groups[2]=Parent（可能为空）
var classContextRE = regexp.MustCompile(`^\s*class\s+(\w+)(?:\s*\(\s*(\w+))?`)
```

`DiffHunk` 新增字段：

```go
type DiffHunk struct {
    ...
    ClassName   string // 变更所在类（来自 @@ context）
    ParentClass string // 该类的父类名（若 @@ context 包含继承语法）
}
```

**改动 `internal/agent/orchestrator.go`**：

在生成 WorkerTask 时，如果 `h.ParentClass != ""`，追加 `ExtraPrompt`：

```go
if h.ParentClass != "" {
    task.ExtraPrompt += fmt.Sprintf(
        "\nNOTE: %s inherits from %s. Check if %s has other subclasses that override %s.",
        h.ClassName, h.ParentClass, h.ParentClass, funcName,
    )
}
```

---

## 改进 7：Worker Prompt 重构——步骤驱动 → 目标+约束 ✅

### 问题本质

当前 `worker.go` 的 prompt 是**步骤驱动**（procedural）：

```
STEP 1 — 对每个函数，用 ripgrep 找调用者（a/b/c/d/e/f 六小步）
STEP 1b — 强制探测 entry-role 仓库（MANDATORY）
STEP 2 — 输出 JSON
```

这等于把执行路径写死了：
- diff 里直接能看出影响链时，模型也要走一遍 STEP 1 的 ripgrep
- 函数名高度通用（`run`/`process`）时，模型只能按规则转换关键词，无法自主判断
- 遇到多态/继承场景，prompt 没有指引，模型靠自己猜

对比 Claude Code 的 `/security-review` 命令：只给工具 + 目标 + 约束，不规定步骤。
模型根据每次 diff 内容自主决定路径（是否需要 Read、Grep 几层、是否搜子类）。

### 改动原则

| 保留（代码保证，Orchestrator 侧） | 改为方向性（模型自主，Worker prompt） |
|----------------------------------|--------------------------------------|
| 输入哪些函数名 + LineHints | 用哪个工具、以何种顺序 |
| ContractHints / ImportContext 注入 | 是否需要读文件、grep 几层 |
| cross_repo_calls JSON schema | 具体搜什么关键词 |
| entry_points JSON schema | 遇到多态时怎么探索 |
| `ensureDiffFileCoverage` 兜底 | 对 diff 自明的改动，是否直接输出 |
| 最大 hop 轮次（10轮） | |

### 改动 7a：重写 `worker.go` 主 prompt（核心）✅

**文件**：`internal/agent/worker.go`，约 line 432–523 的 `fmt.Sprintf`

**改后结构**（目标 + 约束）：

```
## 你的任务

追踪以下函数的完整调用链，找出所有最终被 entry-role 仓库（HTTP handler / gRPC endpoint）
调用到的入口点。

## 工具
- ripgrep: 在代码中搜索调用方
- file_reader: 读取文件内容以理解上下文
- lsp_call_hierarchy: 精确获取函数的调用层级
  （operation: incomingCalls / outgoingCalls / findImplementations）

## 约束（必须遵守）

### 搜索约束
- 每次 ripgrep 都必须真实执行，不得凭记忆推断
- ripgrep 返回 0 结果时，依次尝试：
    snake_case ↔ CamelCase 转换 → 关键词截断 → 去掉 repo 限制
- callers > 20 → wide_impact=true，停止展开

### 输出完整性约束
- cross_repo_calls.to_repo 必须来自 ripgrep 结果的文件路径首段，禁止猜测
- cross_repo_calls.caller_function 必须是 ripgrep 结果中的真实函数名，若无法确定留空
- nodes[].file 和 entry_points[].file 必须来自 ripgrep 的真实路径，若不确定留 ""

### 跨仓约束
- 发现跨仓调用时，记录 cross_repo_calls（供 Orchestrator 调度下一轮 Worker）
- Entry-role 仓库（见列表）到达即停止，记录为 entry_point

### 继承与多态
- 发现某个类方法被修改时，检查是否存在子类：
    ripgrep({"pattern": "class \\w+\\(ClassName\\)", "repo": "<repo>"})
- 有子类 override 同名方法时，将每个 override 视为额外的被改动函数追踪
- 调用方通过父类类型引用时（多态调用），所有子类实现都可能被触达——全部记录

## 已知上下文（优先使用，减少不必要搜索）
{contractHintsSection}
{importContextSection}
{newFunctionsSection}

## 输出格式（最终输出唯一一个 JSON 块）
```

**关键变化对比**：

| 当前（步骤驱动） | 改后（目标+约束） |
|----------------|----------------|
| STEP 1 规定 a→b→c→d→e→f 顺序 | 给出目标，顺序由模型决定 |
| STEP 1b "MANDATORY" 强制执行 | 变为约束：entry-role 仓库到达即停 |
| 无继承/多态指引 | 新增多态约束节（同改进6-P0） |
| diff 自明时也要走完所有步骤 | 模型可直接输出，减少无效工具调用 |

### 改动 7b：`prompt.go` systemPromptTmpl 同步更新 ✅

**文件**：`internal/agent/prompt.go`

`Step 1 / Step 2 / Step 3` 是给 Orchestrator AgentLoop 层的系统 prompt，同步改为方向性描述，
并新增"Inheritance & Polymorphism"节（与改进 6-P0 合并）：

```
### Inheritance & Polymorphism（新增）

When a modified function belongs to a class:
- Search for subclasses: ripgrep({"pattern": "class \\w+\\(ClassName\\)"})
- Check if subclasses override the same method
- If callers reference the base-class type, all subclass implementations may execute at runtime
- Use lsp_call_hierarchy operation="findImplementations" for interface/abstract method implementations
```

---

## 长期记忆：模块上下游关系积累

### 背景

当前 Layer1（memory）只存储函数级语义摘要（`repo_name, symbol, file_path, summary, commit_hash`），
不存储跨仓调用关系。经过大量分析后，系统积累了丰富的跨仓调用知识，但每次分析都从零开始。

### 方案：`module_relationships` 表

每当 Orchestrator 发现一条 `cross_repo_calls`（`from_repo/func → to_repo/func`），
将其 upsert 到新表，累计 `seen_count` 和 `confidence`：

```sql
CREATE TABLE module_relationships (
    id            BIGSERIAL PRIMARY KEY,
    from_repo     VARCHAR(255) NOT NULL,
    from_func     VARCHAR(512),
    to_repo       VARCHAR(255) NOT NULL,
    to_func       VARCHAR(512),
    confidence    FLOAT NOT NULL DEFAULT 1.0,
    seen_count    INT NOT NULL DEFAULT 1,
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (from_repo, from_func, to_repo, to_func)
);

-- 每次新发现时 upsert：
INSERT INTO module_relationships (from_repo, from_func, to_repo, to_func)
VALUES ($1, $2, $3, $4)
ON CONFLICT (from_repo, from_func, to_repo, to_func)
DO UPDATE SET
    seen_count   = module_relationships.seen_count + 1,
    confidence   = LEAST(module_relationships.confidence + 0.1, 1.0),
    last_seen_at = now();
```

### 用法

在 `runWorkerBatch()` 构建 `WorkerTask` 时，查询当前 `from_repo` 的高置信度关系：

```go
// 查询 from_repo 的已知跨仓调用关系（confidence >= 0.7）
rows := pool.Query(ctx, `
    SELECT to_repo, to_func, seen_count
    FROM module_relationships
    WHERE from_repo = $1 AND confidence >= 0.7
    ORDER BY seen_count DESC LIMIT 20
`, repoName)

// 注入到 WorkerTask.ContractHints 或 ExtraPrompt
task.ContractHints += "\nKnown downstream repos from " + repoName + ":\n"
for _, row := range rows {
    task.ContractHints += fmt.Sprintf("  - %s::%s (seen %d times)\n", row.ToRepo, row.ToFunc, row.SeenCount)
}
```

### 价值

- 首次分析一个新功能时，高置信度关系可作为"先验知识"注入 Worker，减少从零探索的步骤
- 随着分析任务积累，关系置信度提升，形成正向飞轮
- 低置信度关系（`seen_count=1`）不干扰分析（仅记录，不注入）

---

## 改动文件汇总

| 改进 | 文件 | 操作 | 状态 |
|------|------|------|------|
| 1 | `internal/tool/gitdiff.go` | 新增 `sourceFuncDefREs`；新增导出函数 `ResolveFuncAtLine()` | ✅ |
| 1 | `internal/agent/orchestrator.go` | Layer A+ 增加磁盘扫描分支；新增私有方法 `resolveFuncNameFromDisk()` | ✅ |
| 2 | `cmd/server/main.go` | 新增 `scheduleNightlyIndex()`、`runNightlyIndex()`；`runServer()` 中启动 goroutine | ✅ |
| 3 | `config/shirakami.example.yaml` | 新增 `index_mode: hybrid` 字段及选项注释 | ✅ |
| 4 | `internal/tool/gitdiff.go` | `DiffHunk` 新增 `RawLines` 字段；`ParseDiffHunks` 填充 body | ✅ |
| 4 | `internal/agent/orchestrator.go` | 新增 `hunkDiffSnippets` map；`WorkerTask` 新增 `DiffSnippets` 字段 | ✅ |
| 4 | `internal/agent/worker.go` | prompt 构建时注入 diff snippet | ✅ |
| 5 | `internal/tool/gitdiff.go` | `DiffHunk` 新增 `GlobalVar` 字段；新增 `globalVarDefRE`；填充逻辑 | ✅ |
| 5 | `internal/agent/orchestrator.go` | 磁盘扫描无结果 + `GlobalVar != ""` 时生成 `FILE_CHANGED_VAR` sentinel | ✅ |
| 5 | `internal/agent/worker.go` | 新增 `FILE_CHANGED_VAR:` 前缀处理分支 | ✅ |
| 6-P0 | `internal/agent/prompt.go` | `systemPromptTmpl` 增加"Inheritance & Polymorphism"指引节 | ✅ |
| 6-P1 | `internal/tool/lsp.go` | 增加 `findImplementations` operation；调用 `textDocument/implementation` | ✅ |
| 6-P2 | `internal/tool/gitdiff.go` | `classContextRE` 扩展捕获父类名；`DiffHunk` 增加 `ClassName`、`ParentClass` 字段 | ✅ |
| 6-P2 | `internal/agent/orchestrator.go` | `ParentClass != ""` 时注入额外搜索提示到 WorkerTask.ExtraPrompt | ✅ |
| 7 | `internal/agent/worker.go` | 重写 line 432–523 主 prompt：步骤驱动 → 目标+约束；新增多态指引节 | ✅ |
| 7 | `internal/agent/prompt.go` | `systemPromptTmpl` Step 1/2/3 改为方向性；新增"Inheritance & Polymorphism"节（与改进6-P0合并） | ✅ |
| 8 | `migrations/006_module_relationships.sql` | `module_relationships` 表 DDL + 索引 | ✅ |
| 8 | `internal/memory/layer1.go` | `ModuleRelationship` struct；`UpsertRelationship`/`UpsertRelationshipAsync`；`GetHighConfidenceRelationships`；`FormatRelationshipHints`；`SearchRelationships` | ✅ |
| 8 | `internal/agent/orchestrator.go` | `saveRelationshipsAsync`（write-back）；`contractHintsForRepo`（dynamic hint injection） | ✅ |

---

## 验证方案

### 编译验证（P0，无需运行时环境）

```bash
# 编译所有包
docker run --rm -v /mnt/shirakami:/src -w /src golang:1.25-alpine go build ./...

# 静态检查
docker run --rm -v /mnt/shirakami:/src -w /src golang:1.25-alpine go vet ./...
```

### 单元测试（改进1）

```bash
# 验证 ResolveFuncAtLine 的各语言定义识别
docker run --rm -v /mnt/shirakami:/src -w /src golang:1.25-alpine \
  go test ./internal/tool/... -v -run TestResolveFuncAtLine

# 验证全局变量 GlobalVar 检测
docker run --rm -v /mnt/shirakami:/src -w /src golang:1.25-alpine \
  go test ./internal/tool/... -v -run TestParseGlobalVar
```

### 全量测试

```bash
docker run --rm -v /mnt/shirakami:/src -w /src golang:1.25-alpine go test ./...
```

### 改进1 端到端验证

重跑 task `341785f0` 同款分支：

```bash
# 期望日志出现（磁盘扫描成功）：
#   extract.layerA+.disk_resolved  file=cvm_api/xxx.py  func=_get_disable_primary_link
# 期望分析时间 < 10 min（vs 原来 46 min）
# 不应出现：FILE_CHANGED 哨兵（磁盘扫描成功的文件）
```

### 改进2 验证

```bash
# 手动触发（或等待 04:00 CST）
# 期望日志：
#   nightly_index.started
#   nightly_index.repo_done  repo=art_api
#   nightly_index.repo_done  repo=cvm_api
#   ...
#   nightly_index.completed
```

### 改进5 验证

用包含全局变量改动的 diff 提交任务：

```bash
# 期望 Worker prompt 含：
#   [trace all call sites that reference variable 'white_set' in art_api/logic.py]
# 不应出现：FILE_CHANGED 整文件泛搜
```

### 改进6-P0 验证

分析 MR https://git.woa.com/vstation/network/-/merge_requests/304 同款 diff：

```bash
# 期望 Worker 工具调用出现：
#   ripgrep({"pattern": "class \\w+\\(BaseHandler\\)", "repo": "vstation_network"})
# 期望 entry_points 覆盖所有子类的 override 方法
```
