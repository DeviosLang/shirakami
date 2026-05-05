# Golden Cases — 来源说明与设计思路

本目录的 golden case 分为两类：**开源 case**（可提交 Git）和**私有 case**（`.gitignore` 排除，仅本地保存）。

---

## 一、为什么要用开源项目做 golden case？

私有项目的 case 存在两个问题：
1. **不能提交 GitHub** — 代码涉及内部业务逻辑，有泄漏风险。
2. **可读性差** — 新人看不懂 case 背景，无法独立判断 expected.json 是否合理。

使用知名开源项目（grpc-go、gin、prometheus…）的好处：
- 任何人都能 `git checkout v1.57.0` 还原原始代码，验证 diff 的正确性。
- 开源社区的代码质量高，调用关系清晰，适合作为"标准答案"。
- 可以直接引用 commit URL，来源可溯源。

---

## 二、开源 case 目录（可提交）

### `go-grpc-server-worker` ★★（medium）

| 属性 | 值 |
|------|-----|
| 上游项目 | [grpc/grpc-go](https://github.com/grpc/grpc-go) |
| 版本跨度 | v1.54.0 → v1.57.0 |
| 核心文件 | `server.go` |
| 关键 commit | [v1.57.0 tag](https://github.com/grpc/grpc-go/tree/v1.57.0) |

**改动内容**：
- `serverWorkerChannels []chan` → `serverWorkerChannel chan`（多 channel 合并为单 channel）
- 新增 `handleSingleStream(data serverWorkerData)` 方法（从 serverWorker 中抽出）
- 新增 `RecvBufferPool(SharedBufferPool) ServerOption`

**设计意图**：验证 **Go 新方法 + 重构（旧逻辑改写）** 场景下 DiffToSymbols 能否同时识别"新函数"和"被修改的现有函数"。调用链应能追溯到 `Serve()` 入口。

**补充新 case 时参考**：grpc-go 每个 minor 版本都有 server/client 层的重构，适合挖掘 v1.60+。

---

### `go-prometheus-counter-vec` ★★（medium）

| 属性 | 值 |
|------|-----|
| 上游项目 | [prometheus/client_golang](https://github.com/prometheus/client_golang) |
| 版本跨度 | v1.14.0 → v1.17.0 |
| 核心文件 | `prometheus/counter.go`, `prometheus/vec.go` |
| 关键 commit | [v1.17.0 tag](https://github.com/prometheus/client_golang/tree/v1.17.0) |

**改动内容**：
- 新增 `CounterVecOpts` 类型（原 `CounterOpts` 的超集）
- 新增 `NewCounterVec(CounterVecOpts)` 函数（v2 API）
- `counter.Write()` 签名新增 `createdTs *timestamppb.Timestamp` 参数
- 新增 `counter.createdTimestamp()` 辅助方法

**设计意图**：验证 **接口签名变更 + 跨文件传播** 场景。`counter.Write` 的签名改变会影响所有实现 `Collector` 接口的地方，测试 Impact 传播的深度。

**补充新 case 时参考**：prometheus/client_golang v1.18+ 引入了 Native Histograms，有大量接口变更，是很好的 medium-hard case 素材。

---

### `go-gin-context-json` ★（easy）

| 属性 | 值 |
|------|-----|
| 上游项目 | [gin-gonic/gin](https://github.com/gin-gonic/gin) |
| 版本跨度 | v1.8.x → v1.9.1 |
| 核心文件 | `context.go`, `gin.go` |
| 关键 commit | [d4b45d9](https://github.com/gin-gonic/gin/commit/d4b45d9) |

**改动内容**：
- `Context.Render()` 错误处理从 `debugPrint` 改为 `c.Abort() + panic`
- `Context.JSON()` 和 `IndentedJSON()` 增加 `WriteHeaderNow()` 预刷 header

**设计意图**：验证 **方法体改动（非新函数）** 的最基础场景。changed_functions 里没有新声明的函数，测 DiffToSymbols 能否仅靠行号覆盖匹配到已有的方法。HTTP 入口 `ServeHTTP` 的追踪是标准测试路径。

**补充新 case 时参考**：gin 的 `RouterGroup`、`HandlerChain` 相关改动适合构造中等难度 case。

---

### `py-fastapi-serialize-response` ★★（medium）

| 属性 | 值 |
|------|-----|
| 上游项目 | [tiangolo/fastapi](https://github.com/tiangolo/fastapi) |
| 版本跨度 | 0.95.x → 0.100.1 |
| 核心文件 | `fastapi/routing.py` |
| 关键 PR | [#9866](https://github.com/tiangolo/fastapi/pull/9866) (serialize include/exclude) |

**改动内容**：
- `serialize_response()` 新增 `exclude`、`exclude_unset`、`exclude_defaults`、`exclude_none` 参数
- `run_endpoint_function()` `is_coroutine` 参数加默认值，增加 `dependant` 校验逻辑

**设计意图**：验证 **Python 函数签名扩展** 场景。Python 的动态特性使得 Layer B 静态索引无效，此 case 仅测试 Layer A（diff hunk 行号覆盖）和 LLM 层的语义理解。`get_request_handler` 是标准 HTTP entry point。

**补充新 case 时参考**：fastapi 0.100+ 的 Response Model 重构、依赖注入系统的 async 支持，都有函数签名扩展。

---

### `py-celery-task-retry` ★★（medium）

| 属性 | 值 |
|------|-----|
| 上游项目 | [celery/celery](https://github.com/celery/celery) |
| 版本跨度 | v5.2.x → v5.3.x |
| 核心文件 | `celery/app/task.py` |
| 关键 PR | [#7734](https://github.com/celery/celery/pull/7734) (retry max_retries=0) |

**改动内容**：
- `Task.retry()` 新增 `max_retries=0` 无限重试语义处理
- `Task.retry()` 重构为通过 `signature_from_request` 构建 signature 再 `apply_async`
- `Task.apply_async()` 新增 `countdown` → `eta` 转换逻辑

**设计意图**：验证 **MQ 任务入口** 场景（`Task.__call__` 是 celery worker 消费者的 entry point）。Python 的 `retry → apply_async` 调用链是典型的"循环/递归触发"模式，适合测试 LLM 对 celery 架构的理解。

**补充新 case 时参考**：celery v5.4 引入了 `Task.run_eager_on_failure`，适合构造异常路径 case。

---

### `cross-grpc-microservices` ★★★（hard）

| 属性 | 值 |
|------|-----|
| 上游项目 | [GoogleCloudPlatform/microservices-demo](https://github.com/GoogleCloudPlatform/microservices-demo) |
| 版本/commit | main 分支，模拟 checkoutservice 改动 |
| 核心文件 | `src/checkoutservice/main.go` |
| 设计基础 | checkoutservice → cartservice → currencyservice 标准 gRPC 链 |

**改动内容**：
- `PlaceOrder()` 新增对 `currencyservice.Convert()` 的 gRPC 调用（商品价格多币种转换）
- 新增 `convertCurrency()` 和 `getCurrencySvcClient()` 辅助方法

**设计意图**：验证 **跨仓库 gRPC 调用** 场景。`cross_repo_calls` 字段应检测到 `checkoutservice → currencyservice` 的跨仓库关系。fixtures.sql 包含 `GRPC_CALLS` 类型边，测试 Layer B 对跨仓库 gRPC 调用的图查询能力。

**补充新 case 时参考**：microservices-demo 的 `productcatalogservice`、`recommendationservice` 间的调用链也是很好的素材。

---

## 三、私有 case 目录（不提交，仅本地）

以下目录被 `.gitignore` 排除，**不能提交 GitHub**：

| 目录 | 内容 | 原因 |
|------|------|------|
| `cases/compute-mr1681-encrypt-disk/` | vstation_compute MR1681 真实 diff | 内部代码 |
| `cases/cross-repo-dispatch/` | VMachineCreate → cvm_api 跨仓库链 | 内部代码 + 业务逻辑 |
| `cases/shallow-config-change/` | vstation_compute health_check 改动 | 含内部 repo 名 |
| `cases/single-file-utils-change/` | install_virtio_driver_v2 新方法 | 含内部 repo 名 |
| `cases/wide-impact-common-utils/` | execute() 高扇出改动 | 含内部 repo 名 |
| `cases/go-cache-invalidation/` | shirakami 自身 cache.go synthetic case | shirakami 是私有仓库 |

**私有 case 的本地同步方式**：通过团队内部的 Git 私有仓库或共享存储维护，不走本仓库。

> 注意：新建私有 case 时，目录命名请参考上表的前缀规则（`compute-`、`vstation-`、`cvm-`、`vsresource-`），`.gitignore` 已覆盖这些前缀。如有新的私有 repo 前缀，请同步更新 `.gitignore`。

---

## 四、如何新增 golden case

### 4.1 开源 case（推荐路径）

```bash
# 1. 确定目标项目和版本跨度（建议跨度覆盖 1-3 个 minor 版本）
#    好的 case：有新函数声明 OR 函数签名改变 OR 跨文件影响

# 2. 拉取两个版本的目标文件
curl -sL https://raw.githubusercontent.com/OWNER/REPO/TAG_NEW/path/to/file.go > new.go
curl -sL https://raw.githubusercontent.com/OWNER/REPO/TAG_OLD/path/to/file.go > old.go
diff -u old.go new.go > input.patch

# 3. 创建目录结构
mkdir -p tests/golden/cases/MY-CASE-NAME
cp input.patch tests/golden/cases/MY-CASE-NAME/
# 写 metadata.json / input.yaml / expected.json / fixtures.sql（可选）

# 4. 验证 Layer A 通过
go test ./tests/golden/... -short -run TestParseDiffHunks_GoldenCases/MY-CASE-NAME -v
```

### 4.2 人工审核 expected.json

`expected.json` 是人工审核的"标准答案"，不是 LLM 输出。写法：

1. 阅读 `input.patch`，手动识别哪些函数被新增/修改
2. 对照上游代码的实际调用关系，填写 `call_chain`
3. 判断 entry point（HTTP handler / gRPC handler / MQ consumer / CLI command）
4. 如有跨仓库调用，填写 `cross_repo_calls`

**字段规范**（与 `runner_test.go` 的 `ExpectedResult` 类型对齐）：

```jsonc
{
  "changed_functions": [
    { "name": "FuncName", "repo": "repo-name", "file": "path/to/file.go", "start_line": 123 }
  ],
  "call_chain": [
    // source/target 填函数名，type 必须是 CALLS / IMPORTS / EXTENDS / IMPLEMENTS
    { "source": "Caller", "target": "Callee", "type": "CALLS", "depth": 1 }
  ],
  "entry_points": [
    // protocol 必须是 HTTP / gRPC / MQ / Cron / CLI / JSON-RPC
    { "function": "Handler", "repo": "repo-name", "file": "path.go", "protocol": "HTTP" }
  ],
  "cross_repo_calls": [
    { "from_repo": "src-repo", "to_repo": "dst-repo", "function": "RemoteFunc", "confidence": 0.9 }
  ]
}
```

### 4.3 fixtures.sql（Layer B，可选）

只有 Go 仓库的 case 才需要写 fixtures.sql。Python 仓库跳过 Layer B（无 fixtures.sql 文件即自动跳过）。

symbol_nodes 主键格式：`"{repo}:{file_path}:{qualified_name}#{arity}"`

```sql
INSERT INTO symbol_nodes (id, repo, file_path, name, kind, start_line, end_line, signature, commit_hash) VALUES
  ('myrepo:pkg/foo.go:MyFunc#2', 'myrepo', 'pkg/foo.go', 'MyFunc', 'function', 10, 25,
   '(ctx context.Context, id string) error', 'abc1234');
```

---

## 五、难度分级与覆盖策略

| 难度 | 标准 | 现有 case 数 | 目标 |
|------|------|-------------|------|
| ★ easy | 单文件，无跨仓库，函数体改动 | 2 | 3 |
| ★★ medium | 多文件或接口变更，单仓库 | 4 | 6 |
| ★★★ hard | 跨仓库或动态 dispatch | 1 | 3 |

**建议下一批补充方向**：

1. **Go interface 变更传播**：如 `io.ReadWriteCloser` 相关的接口扩展（hard）
2. **Python 装饰器 entry point**：如 `@app.route` / `@celery.task` 的识别（medium）
3. **gRPC server streaming**：区别于 unary call，双向流的 entry point 识别（hard）
4. **跨仓库 HTTP REST 调用**：与 gRPC case 互补，测试 Contract Bridge 的 HTTP 路径（hard）
