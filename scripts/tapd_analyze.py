#!/usr/bin/env python3
"""
tapd_analyze.py — 从 TAPD_GIT_INFOS 环境变量读取分支信息，
                  向 Shirakami 提交 chain+e2e 分析，
                  等待完成后把 e2e 结果写入 Markdown 文件。

用法：
    TAPD_GIT_INFOS='[...]' python3 scripts/tapd_analyze.py

必需环境变量：
    API_BASE      — Shirakami API 地址（如 http://43.137.205.156:8080）

可选环境变量：
    OUTPUT_DIR    — Markdown 输出目录，默认当前目录
    MODES         — 分析模式，默认 chain,e2e（逗号分隔）
    POLL_INTERVAL — 轮询间隔秒数，默认 15
    TIMEOUT       — 最长等待秒数，默认 1800（30 分钟）
    REPO_MAP      — 手动覆盖映射，格式：namespace/repo=shirakami_name,...
                    例：vstation/api=vstation_api,art/api=cvm_api
                    优先级最高，覆盖服务端返回结果
"""

import json
import os
import sys
import time
import datetime
import pathlib
import urllib.request
import urllib.error

# ── 配置 ─────────────────────────────────────────────────────────────────────

API_BASE      = os.environ.get("API_BASE", "http://localhost:8080").rstrip("/")
TASKS_URL     = f"{API_BASE}/api/v1/tasks"
REGISTER_URL  = f"{API_BASE}/api/v1/repos/register"
OUTPUT_DIR    = pathlib.Path(os.environ.get("OUTPUT_DIR", "."))
MODES         = [m.strip() for m in os.environ.get("MODES", "chain,e2e").split(",")]
POLL_INTERVAL = int(os.environ.get("POLL_INTERVAL", "15"))
TIMEOUT       = int(os.environ.get("TIMEOUT", "1800"))


def http_json(url, payload=None, timeout=30):
    """GET（payload=None）或 POST（payload=dict）并返回解析后的 JSON。"""
    data = json.dumps(payload).encode() if payload is not None else None
    headers = {"Content-Type": "application/json"} if data else {}
    req = urllib.request.Request(
        url, data=data, headers=headers,
        method="POST" if data is not None else "GET",
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read())


# ── URL 规范化 ────────────────────────────────────────────────────────────────

def _normalize_url(url):
    """
    把各种 git URL 格式统一成 https://host/path 形式，方便服务端匹配。

    git@git.woa.com:vstation/api[.git]   → https://git.woa.com/vstation/api
    http://oauth2:token@git.woa.com/...  → https://git.woa.com/...
    https://git.woa.com/vstation/api.git → https://git.woa.com/vstation/api
    """
    url = url.strip()

    # SSH: git@host:path/to/repo[.git]
    if url.startswith("git@"):
        rest = url[len("git@"):]
        if ":" in rest:
            host, path = rest.split(":", 1)
        elif "/" in rest:
            # 无冒号时把第一段当 host
            host, path = rest.split("/", 1)
        else:
            return url  # 无法解析，原样返回
        if path.endswith(".git"):
            path = path[:-4]
        return f"https://{host}/{path}"

    # HTTP(S): 去掉 credentials，统一成 https
    for prefix in ("https://", "http://"):
        if url.startswith(prefix):
            rest = url[len(prefix):]
            if "@" in rest:                        # 去掉 user:pass@
                rest = rest.split("@", 1)[1]
            if rest.endswith(".git"):
                rest = rest[:-4]
            return f"https://{rest}"

    return url


# ── REPO_MAP 环境变量（最高优先级）───────────────────────────────────────────
# 格式：namespace/repo=shirakami_name,...
# 例：vstation/api=vstation_api,art/api=cvm_api
_REPO_MAP_OVERRIDE = {}
_repo_map_env = os.environ.get("REPO_MAP", "")
if _repo_map_env:
    for pair in _repo_map_env.split(","):
        pair = pair.strip()
        if "=" in pair:
            k, v = pair.split("=", 1)
            _REPO_MAP_OVERRIDE[k.strip()] = v.strip()
    print(f"[info] REPO_MAP 环境变量：{len(_REPO_MAP_OVERRIDE)} 条覆盖规则")


def _apply_repo_map(norm_url, server_name):
    """REPO_MAP 优先：按 namespace/repo 后缀匹配。"""
    if not _REPO_MAP_OVERRIDE:
        return server_name
    for k, v in _REPO_MAP_OVERRIDE.items():
        if norm_url.rstrip("/").endswith("/" + k.lstrip("/")):
            return v
    return server_name


# ── 批量解析 repo URL → shirakami name ───────────────────────────────────────

def resolve_repo_names(repo_urls):
    """
    通过 POST /api/v1/repos/register 批量把 git URL 转为 shirakami repo name。

    返回 dict: normalized_url → name
      registered=true  → 使用服务端注册名（最准确）
      registered=false → 使用服务端从 URL path 推断的末段名

    优点：
      - 无需本地 shirakami.yaml
      - 服务端做 URL 匹配，结果权威
      - SSH URL 规范化为 https 后，与服务端配置的 URL 一致
    """
    if not repo_urls:
        return {}

    # 规范化并去重
    norm_map = {}  # normalized_url → original_url（保留一个代表）
    for u in repo_urls:
        if u:
            norm_map[_normalize_url(u)] = u

    try:
        data = http_json(REGISTER_URL, {"urls": list(norm_map)}, timeout=15)
    except Exception as e:
        print(f"[warn] /api/v1/repos/register 调用失败: {e}", file=sys.stderr)
        return {}

    result = {}
    for r in data.get("repos", []):
        url  = r.get("url", "")
        name = r.get("name", "")
        err  = r.get("error", "")
        if not url or not name or err:
            if err:
                print(f"[warn] repos/register 无法解析 {url}: {err}", file=sys.stderr)
            continue
        norm = _normalize_url(url)
        # 应用 REPO_MAP 覆盖
        name = _apply_repo_map(norm, name)
        result[norm] = name
        reg = "registered" if r.get("registered") else "inferred"
        print(f"  [repo-map] {norm} → {name}  ({reg})")

    return result


# ── 解析 TAPD_GIT_INFOS ───────────────────────────────────────────────────────

raw = os.environ.get("TAPD_GIT_INFOS", "")
if not raw:
    print("错误：环境变量 TAPD_GIT_INFOS 未设置或为空", file=sys.stderr)
    sys.exit(1)

try:
    tapd_data = json.loads(raw)
except json.JSONDecodeError as e:
    print(f"错误：TAPD_GIT_INFOS 不是合法 JSON: {e}", file=sys.stderr)
    sys.exit(1)

# 支持两种结构：
#   1. 直接是列表：[{"CodeBranchObjects": {...}}, ...]
#   2. 带 status/data 包装：[{"status":1, "data": [...]}]  或  {"status":1,"data":[...]}
entries = []

def _extract_entries(obj):
    if isinstance(obj, list):
        for item in obj:
            if "CodeBranchObjects" in item:
                entries.append(item["CodeBranchObjects"])
            elif "data" in item:
                _extract_entries(item["data"])
    elif isinstance(obj, dict):
        if "CodeBranchObjects" in obj:
            entries.append(obj["CodeBranchObjects"])
        elif "data" in obj:
            _extract_entries(obj["data"])

_extract_entries(tapd_data)

if not entries:
    print("错误：TAPD_GIT_INFOS 中未找到任何 CodeBranchObjects 条目", file=sys.stderr)
    sys.exit(1)

print(f"解析到 {len(entries)} 条分支记录")

# ── 批量解析 repo URL → shirakami name ───────────────────────────────────────

all_repo_urls = [e.get("repo_url", "") for e in entries if e.get("repo_url")]
print(f"向服务端查询 {len(set(all_repo_urls))} 个唯一 repo URL...")

url_to_name = resolve_repo_names(all_repo_urls)  # normalized_url → name

# ── 构建 branches 请求体（去重：同 repo name 只保留 created 最新的一条）───────

repo_latest = {}   # repo_name → {"branch": ..., "created": ..., "repo_url": ..., "entry": ...}
unresolvable = []

for e in entries:
    repo_url   = e.get("repo_url", "")
    branch_ref = e.get("branch", "")
    branch     = branch_ref[len("refs/heads/"):] if branch_ref.startswith("refs/heads/") else branch_ref
    created    = e.get("created", "")

    if not repo_url or not branch:
        unresolvable.append(e)
        continue

    norm_url = _normalize_url(repo_url)
    repo     = url_to_name.get(norm_url)

    # REPO_MAP 直接覆盖（兜底，url_to_name 里已包含 REPO_MAP 应用，此处双保险）
    if not repo:
        repo = _apply_repo_map(norm_url, None)

    if not repo:
        print(f"  [skip] 无法解析 repo name: {repo_url}", file=sys.stderr)
        unresolvable.append(e)
        continue

    if repo not in repo_latest or created > repo_latest[repo]["created"]:
        if repo in repo_latest:
            old = repo_latest[repo]
            print(f"  [skip] repo={repo}  branch={old['branch']}  created={old['created']}  → 被更新记录替代")
        repo_latest[repo] = {"branch": branch, "created": created,
                              "repo_url": repo_url, "entry": e}
    else:
        print(f"  [skip] repo={repo}  branch={branch}  created={created}  → 非最新，忽略")

branches_payload = []
for repo_name, info in repo_latest.items():
    branches_payload.append({"repo": repo_name, "branch": info["branch"]})
    print(f"  → repo={repo_name}  branch={info['branch']}  created={info['created']}  (来自 {info['repo_url']})")

if unresolvable:
    print(f"[warn] {len(unresolvable)} 条记录因缺少 repo_url/branch 或无法解析 name 被跳过", file=sys.stderr)

if not branches_payload:
    print("错误：没有可提交的有效分支", file=sys.stderr)
    sys.exit(1)

# ── 提交分析任务 ──────────────────────────────────────────────────────────────

request_body = {"branches": branches_payload, "modes": MODES}
print(f"\n提交分析任务（modes={MODES}，共 {len(branches_payload)} 个仓库）...")

try:
    # 提交时服务器需要并行 fetch 所有仓库的 diff，耗时可能超过 30s，
    # 故单独指定更宽裕的超时（180s）。
    resp = http_json(TASKS_URL, request_body, timeout=180)
except urllib.error.HTTPError as e:
    body = e.read().decode(errors="replace")
    print(f"错误：提交失败 HTTP {e.code}: {body}", file=sys.stderr)
    sys.exit(1)

task_id = resp.get("id")
if not task_id:
    print(f"错误：服务器未返回 task_id，响应：{resp}", file=sys.stderr)
    sys.exit(1)

print(f"任务已提交：{task_id}")
print(f"TASK_ID={task_id}")  # CI/CD 可 grep 捕获
print(f"进度查询：curl -s {TASKS_URL}/{task_id} | python3 -m json.tool")

# ── 轮询等待完成 ──────────────────────────────────────────────────────────────

deadline = time.time() + TIMEOUT
last_status = ""

while time.time() < deadline:
    time.sleep(POLL_INTERVAL)
    try:
        task = http_json(f"{TASKS_URL}/{task_id}")
    except Exception as e:
        print(f"[warn] 轮询失败，重试: {e}", file=sys.stderr)
        continue

    status   = task.get("status", "")
    progress = task.get("progress", "")
    if status != last_status or progress:
        ts = datetime.datetime.now().strftime("%H:%M:%S")
        print(f"[{ts}] status={status}" + (f"  progress={progress}" if progress else ""))
        last_status = status

    if status == "completed":
        break
    if status == "failed":
        err = task.get("error", task.get("error_message", "unknown error"))
        print(f"错误：任务失败: {err}", file=sys.stderr)
        sys.exit(1)
else:
    print(f"错误：超时（{TIMEOUT}s），任务仍未完成", file=sys.stderr)
    sys.exit(1)

print("分析完成，正在获取 e2e 结果...")

# ── 获取 e2e 结果 ─────────────────────────────────────────────────────────────

try:
    e2e_data = http_json(f"{TASKS_URL}/{task_id}/e2e")
except urllib.error.HTTPError as e:
    body = e.read().decode(errors="replace")
    print(f"错误：获取 e2e 结果失败 HTTP {e.code}: {body}", file=sys.stderr)
    sys.exit(1)

# ── 生成 Markdown ─────────────────────────────────────────────────────────────

OUTPUT_DIR.mkdir(parents=True, exist_ok=True)
out_file = OUTPUT_DIR / "e2e.md"

# 从 TAPD 数据提取 story ID（去重）
story_ids = list({e.get("object_id", "") for e in entries if e.get("object_id")})
story_label = ", ".join(story_ids) if story_ids else "—"

lines = [
    "# E2E 测试场景分析",
    "",
    f"- **任务 ID**：`{task_id}`",
    f"- **TAPD Story**：{story_label}",
    "- **分析分支**：",
]
for b in branches_payload:
    lines.append(f"  - `{b['repo']}` → `{b['branch']}`")
lines += [
    f"- **生成时间**：{datetime.datetime.now().strftime('%Y-%m-%d %H:%M:%S')}",
    "",
]

entry_points = []
if isinstance(e2e_data, list):
    entry_points = e2e_data
elif isinstance(e2e_data, dict):
    entry_points = e2e_data.get("entry_points", e2e_data.get("data", []))

# impact_summary holds the LLM-generated scenario table (Markdown string).
# It lives at the top level of the e2e response, NOT inside each entry_point.
impact_summary = e2e_data.get("impact_summary", "") if isinstance(e2e_data, dict) else ""

if impact_summary:
    lines += [
        "## E2E 测试场景",
        "",
        impact_summary.strip(),
        "",
    ]

if not entry_points:
    lines += [
        "## 结果",
        "",
        "> ⚠️ 未找到 E2E 入口点，可能原因：diff 为空、变更函数无外部调用链、或分支尚未推送到远端。",
        "",
        "## 原始响应",
        "",
        "```json",
        json.dumps(e2e_data, ensure_ascii=False, indent=2),
        "```",
    ]
else:
    lines += [
        f"## E2E 入口点（共 {len(entry_points)} 个）",
        "",
    ]
    for idx, ep in enumerate(entry_points, 1):
        # API returns PascalCase keys (Function/File/Line/Repo); fall back to
        # lowercase variants for forward compatibility.
        func      = ep.get("Function") or ep.get("function") or ep.get("func") or ep.get("name") or "—"
        file_     = ep.get("File") or ep.get("file") or ep.get("path") or "—"
        repo      = ep.get("Repo") or ep.get("repo") or "—"
        line      = ep.get("Line") or ep.get("line") or ep.get("line_number") or ""
        loc       = f"{file_}:{line}" if line else file_
        scenarios = ep.get("Scenarios") or ep.get("scenarios") or ep.get("test_scenarios") or []

        lines += [
            f"### {idx}. `{func}`",
            "",
            f"- **仓库**：`{repo}`",
            f"- **位置**：`{loc}`",
            "",
        ]
        if scenarios:
            lines += ["**测试场景：**", ""]
            for s in scenarios:
                if isinstance(s, str):
                    lines.append(f"- {s}")
                elif isinstance(s, dict):
                    name  = s.get("name") or s.get("title") or s.get("scenario") or str(s)
                    steps = s.get("steps") or s.get("description") or ""
                    lines.append(f"- **{name}**")
                    if steps:
                        if isinstance(steps, list):
                            for step in steps:
                                lines.append(f"  - {step}")
                        else:
                            lines.append(f"  {steps}")
            lines.append("")
        else:
            lines += ["> 暂无测试场景", ""]

warnings = e2e_data.get("warnings") if isinstance(e2e_data, dict) else None
if warnings:
    lines += ["## ⚠️ 分析警告", ""]
    for w in (warnings if isinstance(warnings, list) else [warnings]):
        lines.append(f"- {w}")
    lines.append("")

out_file.write_text("\n".join(lines), encoding="utf-8")
print(f"\n✓ 已写入：{out_file}")
print(f"  Task ID：{task_id}")
print(f"  入口点数量：{len(entry_points)}")
