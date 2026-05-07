#!/usr/bin/env python3
"""
tapd_analyze.py — 从 TAPD_GIT_INFOS 环境变量读取分支信息，
                  向 Shirakami 提交 chain+e2e 分析，
                  等待完成后把 e2e 结果写入 Markdown 文件。

用法：
    TAPD_GIT_INFOS='[...]' python3 scripts/tapd_analyze.py

可选环境变量：
    API_BASE   — Shirakami API 地址，默认 http://43.137.205.156:8080
    OUTPUT_DIR    — Markdown 输出目录，默认当前目录
    MODES         — 分析模式，默认 chain,e2e（逗号分隔）
    POLL_INTERVAL — 轮询间隔秒数，默认 15
    TIMEOUT       — 最长等待秒数，默认 1800（30 分钟）
    SHIRAKAMI_YAML — shirakami.yaml 路径（用于 repo url→name 映射）
    REPO_MAP      — 手动指定映射，格式：git路径=name,git路径=name
                    例：vstation/vstation=vstation_allinone,vstation/api=vstation_api
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

API_BASE      = os.environ.get("API_BASE", "http://43.137.205.156:8080").rstrip("/")
TASKS_URL     = f"{API_BASE}/api/v1/tasks"
OUTPUT_DIR    = pathlib.Path(os.environ.get("OUTPUT_DIR", "."))
MODES         = [m.strip() for m in os.environ.get("MODES", "chain,e2e").split(",")]
POLL_INTERVAL = int(os.environ.get("POLL_INTERVAL", "15"))
TIMEOUT       = int(os.environ.get("TIMEOUT", "1800"))

def http_json(url, payload=None):
    """GET（payload=None）或 POST（payload=dict）并返回 JSON。"""
    data = json.dumps(payload).encode() if payload is not None else None
    headers = {"Content-Type": "application/json"} if data else {}
    req = urllib.request.Request(url, data=data, headers=headers,
                                  method="POST" if data else "GET")
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read())


# shirakami repo name ← git.woa.com 路径 映射表
# key: "{namespace}/{repo_name}" (repo_url 去掉 scheme+host+.git)
REPO_URL_TO_NAME = {}

def _url_to_key(url):
    """http://git.woa.com/vstation/vstation.git → vstation/vstation"""
    for prefix in ["http://", "https://", "git@"]:
        if url.startswith(prefix):
            rest = url[len(prefix):]
            if "@" in rest:
                rest = rest.split("@", 1)[1]
            if "/" in rest:
                rest = rest.split("/", 1)[1]
            if rest.endswith(".git"):
                rest = rest[:-4]
            return rest
    return url

def _load_repo_map():
    """优先从 API_BASE/api/v1/repos 拉取映射；失败时尝试本地 shirakami.yaml。"""
    # 1. 从服务接口拉取（脚本运行在任何机器上都能工作）
    try:
        data = http_json("{}/api/v1/repos".format(API_BASE))
        repos = data.get("repos", [])
        for r in repos:
            key = _url_to_key(r.get("url", ""))
            if key and r.get("name"):
                REPO_URL_TO_NAME[key] = r["name"]
        if REPO_URL_TO_NAME:
            print("[info] 从 API 加载 repo 映射，共 {} 条".format(len(REPO_URL_TO_NAME)))
            return
    except Exception as e:
        print("[warn] 从 API 加载 repo 映射失败: {}，尝试本地文件".format(e), file=sys.stderr)

    # 2. fallback：本地 shirakami.yaml
    candidates = []
    if os.environ.get("SHIRAKAMI_YAML"):
        candidates.append(pathlib.Path(os.environ["SHIRAKAMI_YAML"]))
    candidates.append(pathlib.Path(__file__).parent.parent / "shirakami.yaml")
    candidates.append(pathlib.Path("shirakami.yaml"))

    yaml_path = next((p for p in candidates if p.exists()), None)
    if yaml_path is None:
        print("[warn] 未找到 shirakami.yaml，repo 映射为空，可用 REPO_MAP 环境变量手动指定", file=sys.stderr)
        return

    try:
        current_name = None
        with open(yaml_path) as f:
            for line in f:
                stripped = line.strip()
                if stripped.startswith("- name:"):
                    current_name = stripped.split(":", 1)[1].strip()
                elif stripped.startswith("url:") and current_name:
                    url = stripped.split(":", 1)[1].strip()
                    key = _url_to_key(url)
                    if key:
                        REPO_URL_TO_NAME[key] = current_name
                    current_name = None
        print("[info] 从 {} 加载 repo 映射，共 {} 条".format(yaml_path, len(REPO_URL_TO_NAME)))
    except Exception as e:
        print("[warn] 读取 shirakami.yaml 失败: {}".format(e), file=sys.stderr)

_load_repo_map()

# REPO_MAP 环境变量可覆盖（优先级最高）
# 格式：vstation/vstation=vstation_allinone,vstation/api=vstation_api
_repo_map_env = os.environ.get("REPO_MAP", "")
if _repo_map_env:
    for pair in _repo_map_env.split(","):
        pair = pair.strip()
        if "=" in pair:
            k, v = pair.split("=", 1)
            REPO_URL_TO_NAME[k.strip()] = v.strip()
    print("[info] REPO_MAP 环境变量已合并")


def repo_name_from_url(repo_url):
    """从 repo_url 推断 shirakami repo name，先查映射表，找不到则取路径最后一段。"""
    key = _url_to_key(repo_url)
    if not key:
        return None
    if key in REPO_URL_TO_NAME:
        return REPO_URL_TO_NAME[key]
    # fallback: 取最后一段（git 路径末段通常就是 repo 名）
    return key.split("/")[-1]


# ── 解析 TAPD_GIT_INFOS ───────────────────────────────────────────────────────

raw = os.environ.get("TAPD_GIT_INFOS", "")
if not raw:
    print("错误：环境变量 TAPD_GIT_INFOS 未设置或为空", file=sys.stderr)
    sys.exit(1)

try:
    tapd_data = json.loads(raw)
except json.JSONDecodeError as e:
    print("错误：TAPD_GIT_INFOS 不是合法 JSON: {}".format(e), file=sys.stderr)
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
            elif "status" in item and "data" in item:
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

# ── 构建 branches 请求体（去重：同 repo 只保留最新一条）────────────────────────

# 按 repo_url 去重，保留 created 最新的那条
latest = {}
for e in entries:
    url = e.get("repo_url", "")
    created = e.get("created", "")
    if url not in latest or created > latest[url]["created"]:
        latest[url] = e

branches_payload = []
skipped = []
for e in latest.values():
    repo_url = e.get("repo_url", "")
    branch_ref = e.get("branch", "")
    # refs/heads/feature/xxx → feature/xxx
    branch = branch_ref[len("refs/heads/"):] if branch_ref.startswith("refs/heads/") else branch_ref
    repo = repo_name_from_url(repo_url)
    if not repo or not branch:
        skipped.append(e)
        continue
    branches_payload.append({"repo": repo, "branch": branch})
    print(f"  → repo={repo}  branch={branch}  (来自 {repo_url})")

if skipped:
    print(f"[warn] 以下 {len(skipped)} 条记录因缺少 repo_url 或 branch 被跳过:", file=sys.stderr)
    for s in skipped:
        print(f"       {s}", file=sys.stderr)

if not branches_payload:
    print("错误：没有可提交的有效分支", file=sys.stderr)
    sys.exit(1)

# ── 提交分析任务 ──────────────────────────────────────────────────────────────

request_body = {"branches": branches_payload, "modes": MODES}
print(f"\n提交分析任务（modes={MODES}，共 {len(branches_payload)} 个仓库）...")

try:
    resp = http_json(TASKS_URL, request_body)
except urllib.error.HTTPError as e:
    body = e.read().decode(errors="replace")
    print(f"错误：提交失败 HTTP {e.code}: {body}", file=sys.stderr)
    sys.exit(1)

task_id = resp.get("id")
if not task_id:
    print(f"错误：服务器未返回 task_id，响应：{resp}", file=sys.stderr)
    sys.exit(1)

print(f"任务已提交：{task_id}")
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

    status = task.get("status", "")
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
ts_str = datetime.datetime.now().strftime("%Y%m%d_%H%M%S")
out_file = OUTPUT_DIR / "e2e.md"

# 从 TAPD 数据提取 story ID（去重）
story_ids = list({e.get("object_id", "") for e in entries if e.get("object_id")})
story_label = ", ".join(story_ids) if story_ids else "—"

lines = [
    f"# E2E 测试场景分析",
    f"",
    f"- **任务 ID**：`{task_id}`",
    f"- **TAPD Story**：{story_label}",
    f"- **分析分支**：",
]
for b in branches_payload:
    lines.append(f"  - `{b['repo']}` → `{b['branch']}`")
lines += [
    f"- **生成时间**：{datetime.datetime.now().strftime('%Y-%m-%d %H:%M:%S')}",
    f"",
]

# e2e_data 可能是列表（entry points 数组）或带 entry_points 字段的对象
entry_points = []
if isinstance(e2e_data, list):
    entry_points = e2e_data
elif isinstance(e2e_data, dict):
    entry_points = e2e_data.get("entry_points", e2e_data.get("data", []))

if not entry_points:
    lines += [
        "## 结果",
        "",
        "> ⚠️ 未找到 E2E 入口点，可能原因：diff 为空、变更函数无外部调用链、或分支尚未推送到远端。",
        "",
    ]
    # 附上原始响应供调试
    lines += [
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
        # 兼容不同字段名
        func     = ep.get("function") or ep.get("func") or ep.get("name") or "—"
        file_    = ep.get("file") or ep.get("path") or "—"
        repo     = ep.get("repo") or "—"
        line     = ep.get("line") or ep.get("line_number") or ""
        loc      = f"{file_}:{line}" if line else file_

        scenarios = ep.get("scenarios") or ep.get("test_scenarios") or []

        lines += [
            f"### {idx}. `{func}`",
            f"",
            f"- **仓库**：`{repo}`",
            f"- **位置**：`{loc}`",
            f"",
        ]
        if scenarios:
            lines += ["**测试场景：**", ""]
            for s in scenarios:
                # scenario 可能是字符串或对象
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

# warnings
warnings = e2e_data.get("warnings") if isinstance(e2e_data, dict) else None
if warnings:
    lines += ["## ⚠️ 分析警告", ""]
    for w in (warnings if isinstance(warnings, list) else [warnings]):
        lines.append(f"- {w}")
    lines.append("")

out_file.write_text("\n".join(lines), encoding="utf-8")
print(f"\n✓ 已写入：{out_file}")
print(f"  入口点数量：{len(entry_points)}")
