#!/bin/bash
# 提交分析任务到 shirakami
#
# 用法（三选一）：
#
#   1. repo + branch 模式（推荐）— server 端自动 git fetch + three-dot diff：
#      SOURCE_REPO=vstation_network BRANCH=feature/fix-dfw bash submit.sh
#
#      多仓多分支（branches 模式）：
#      BRANCHES='[{"repo":"vstation_network","branch":"feature/fix-dfw"},{"repo":"cvm_api","branch":"feature/fix-dfw"}]' \
#        bash submit.sh
#
#   2. 从 GitLab MR 自动拉取 diff：
#      GITLAB_TOKEN=<your_token> MR_URL=https://gitlab.example.com/org/repo/-/merge_requests/304 bash submit.sh
#
#   3. 使用本地 patch 文件：
#      SOURCE_REPO=vstation_network PATCH_FILE=/path/to/your.patch bash submit.sh

set -euo pipefail

# ── --help ────────────────────────────────────────────────────────────────────
if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  grep '^#' "$0" | grep -v '^#!/' | sed 's/^# \{0,2\}//'
  exit 0
fi

API_BASE=${API_BASE:-"http://43.137.205.156:8080"}
API="${API_BASE}/api/v1/tasks"
SOURCE_REPO=${SOURCE_REPO:-"vstation_network"}

# ── 轮询并展示 e2e 结果 ────────────────────────────────────────────────────────
# 用法：_wait_and_show <task_id>
# 环境变量：
#   WAIT=0          — 跳过等待，仅打印 task_id 后退出
#   POLL_INTERVAL   — 轮询间隔秒数，默认 15
#   TIMEOUT         — 最长等待秒数，默认 1800（30 分钟）
_wait_and_show() {
  local task_id=$1
  local poll=${POLL_INTERVAL:-15}
  local timeout=${TIMEOUT:-1800}
  local deadline=$(( $(date +%s) + timeout ))

  if [[ "${WAIT:-1}" == "0" ]]; then
    return
  fi

  echo "⏳ 等待任务完成（每 ${poll}s 轮询，最长 ${timeout}s）..."
  local last_status=""
  while [[ $(date +%s) -lt $deadline ]]; do
    sleep "$poll"
    local resp
    resp=$(curl -s "${API}/${task_id}" 2>/dev/null || true)
    local status
    status=$(echo "$resp" | jq -r '.status // "unknown"')
    local progress
    progress=$(echo "$resp" | jq -r '.progress // ""')

    local ts
    ts=$(date '+%H:%M:%S')
    if [[ "$status" != "$last_status" || -n "$progress" ]]; then
      echo -n "[$ts] status=${status}"
      [[ -n "$progress" ]] && echo -n "  progress=${progress}"
      echo ""
      last_status=$status
    fi

    if [[ "$status" == "completed" ]]; then
      break
    fi
    if [[ "$status" == "failed" ]]; then
      local err
      err=$(echo "$resp" | jq -r '.error // .error_message // "unknown error"')
      echo "❌ 任务失败：$err" >&2
      exit 1
    fi
  done

  if [[ "$last_status" != "completed" ]]; then
    echo "⚠️  超时（${timeout}s），任务仍未完成，task_id=${task_id}" >&2
    exit 1
  fi

  echo ""
  echo "✅ 分析完成，获取 e2e 结果..."
  echo ""

  local e2e
  e2e=$(curl -s "${API}/${task_id}/e2e" 2>/dev/null)

  # 显示 impact_summary（e2e 场景正文）
  local impact
  impact=$(echo "$e2e" | jq -r '.impact_summary // ""')
  if [[ -n "$impact" ]]; then
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  E2E 测试场景（impact_summary）"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "$impact"
    echo ""
  fi

  # 显示 entry_points 汇总
  local ep_count
  ep_count=$(echo "$e2e" | jq '.entry_points | length')
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  echo "  Entry Points（共 ${ep_count} 个）"
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  echo "$e2e" | jq -r '.entry_points[]? | "  [\(.Repo)] \(.Function)  \(.File):\(.Line)"'
  echo ""

  # warnings
  local warnings
  warnings=$(echo "$e2e" | jq -r '.warnings[]? // empty')
  if [[ -n "$warnings" ]]; then
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  ⚠️  分析警告"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "$warnings" | sed 's/^/  /'
    echo ""
  fi
}

MODES=${MODES:-'["chain","e2e"]'}

# ── 方式 1：repo + branch (input_branch 模式) ────────────────────────────────

if [[ -n "${BRANCH:-}" || -n "${BRANCHES:-}" ]]; then
  if [[ -n "${BRANCHES:-}" ]]; then
    # 多仓多分支模式
    echo "→ 提交到 Shirakami（branches 模式，modes=$MODES）"
    RESPONSE=$(curl -s -X POST "$API" \
      -H "Content-Type: application/json" \
      -d "$(jq -n \
        --argjson branches "${BRANCHES}" \
        --argjson modes "$MODES" \
        '{branches: $branches, modes: $modes}')")
  else
    # 单仓 branch 模式
    echo "→ 提交到 Shirakami（source_repo=$SOURCE_REPO，branch=${BRANCH}，modes=$MODES）"
    RESPONSE=$(curl -s -X POST "$API" \
      -H "Content-Type: application/json" \
      -d "$(jq -n \
        --arg repo "$SOURCE_REPO" \
        --arg branch "${BRANCH}" \
        --argjson modes "$MODES" \
        '{source_repo: $repo, input_branch: $branch, modes: $modes}')")
  fi

  TASK_ID=$(echo "$RESPONSE" | jq -r '.id // empty')
  if [[ -z "$TASK_ID" ]]; then
    echo "错误：提交失败，响应：$RESPONSE" >&2
    exit 1
  fi

  echo ""
  echo "✓ 任务已提交：$TASK_ID"
  echo ""
  echo "查询进度："
  echo "  curl -s ${API}/$TASK_ID | jq '{status, progress}'"
  echo ""
  echo "查看调用链："
  echo "  curl -s ${API}/$TASK_ID/chain | jq ."
  echo ""
  echo "查看 e2e 场景："
  echo "  curl -s ${API}/$TASK_ID/e2e | jq ."

  _wait_and_show "$TASK_ID"
  exit 0
fi

# ── 方式 2 & 3：需要本地 diff ────────────────────────────────────────────────

PATCH_TMP=/tmp/shirakami_submit_$$.patch

if [[ -n "${MR_URL:-}" ]]; then
  # 方式 2：从 GitLab MR URL 自动拉取
  # 需要设置 GITLAB_TOKEN 环境变量（个人 Access Token，read_api 权限）
  if [[ -z "${GITLAB_TOKEN:-}" ]]; then
    echo "错误：使用 MR_URL 时必须设置 GITLAB_TOKEN 环境变量" >&2
    exit 1
  fi

  # 从 URL 解析 host / namespace / project / MR 号
  # 支持格式：https://gitlab.example.com/org/project/-/merge_requests/304
  GITLAB_HOST=$(echo "$MR_URL" | grep -oP 'https?://[^/]+')
  MR_NUM=$(echo "$MR_URL" | grep -oP '\d+$')
  PROJECT_PATH=$(echo "$MR_URL" | grep -oP '(?<=\.com/)[^/]+/[^/]+(?=/-/)')
  PROJECT_ENCODED=$(echo "$PROJECT_PATH" | sed 's|/|%2F|g')

  echo "→ 拉取 MR diff：$GITLAB_HOST/$PROJECT_PATH!$MR_NUM"

  # 方式 A：直接用 .diff 后缀
  HTTP_CODE=$(curl -sf -o "$PATCH_TMP" -w "%{http_code}" \
    --header "PRIVATE-TOKEN: $GITLAB_TOKEN" \
    "${GITLAB_HOST}/${PROJECT_PATH}/-/merge_requests/${MR_NUM}.diff")

  if [[ "$HTTP_CODE" != "200" ]] || [[ ! -s "$PATCH_TMP" ]]; then
    echo "  .diff 方式失败（HTTP $HTTP_CODE），尝试 API 方式..." >&2
    # 方式 B：GitLab API /diffs
    curl -sf \
      --header "PRIVATE-TOKEN: $GITLAB_TOKEN" \
      "${GITLAB_HOST}/api/v4/projects/${PROJECT_ENCODED}/merge_requests/${MR_NUM}/diffs?per_page=100" \
      | jq -r '.[].diff // empty' > "$PATCH_TMP"
  fi

  if [[ ! -s "$PATCH_TMP" ]]; then
    echo "错误：获取 MR diff 失败，请检查 GITLAB_TOKEN 权限和 MR_URL 格式" >&2
    exit 1
  fi
  echo "  diff 大小：$(wc -c < "$PATCH_TMP") bytes，$(grep -c '^@@' "$PATCH_TMP" || true) 个 hunk"

elif [[ -n "${PATCH_FILE:-}" ]]; then
  # 方式 3：使用本地文件
  if [[ ! -f "$PATCH_FILE" ]]; then
    echo "错误：文件不存在：$PATCH_FILE" >&2
    exit 1
  fi
  cp "$PATCH_FILE" "$PATCH_TMP"
  echo "→ 使用本地文件：$PATCH_FILE"

else
  echo "错误：未指定 diff 来源。请通过以下方式之一提供：" >&2
  echo ""
  echo "  方式 1（推荐）— repo + branch，server 自动拉取 diff：" >&2
  echo "    SOURCE_REPO=vstation_network BRANCH=feature/your-branch bash submit.sh" >&2
  echo ""
  echo "  方式 2 — GitLab MR URL：" >&2
  echo "    GITLAB_TOKEN=<token> MR_URL=https://gitlab.example.com/org/repo/-/merge_requests/123 bash submit.sh" >&2
  echo ""
  echo "  方式 3 — 本地 patch 文件：" >&2
  echo "    SOURCE_REPO=vstation_network PATCH_FILE=/path/to/your.patch bash submit.sh" >&2
  exit 1
fi

# ── diff 摘要 ────────────────────────────────────────────────────────────────
HUNK_COUNT=$(grep -c '^@@' "$PATCH_TMP" || true)
FILE_COUNT=$(grep -c '^diff --git' "$PATCH_TMP" || true)
FUNC_COUNT=$(grep -oP '^@@[^@]*@@\s*\K\S.*' "$PATCH_TMP" | grep -cv '^\s*$' || true)

echo "  文件数：$FILE_COUNT，hunk 数：$HUNK_COUNT，识别到函数名的 hunk：$FUNC_COUNT/$HUNK_COUNT"

if [[ "$FUNC_COUNT" -eq 0 && "$HUNK_COUNT" -gt 0 ]]; then
  echo "  ⚠️  警告：所有 @@ 行均未带函数名，分析将无法识别变更函数！"
  echo "     git 自动生成的 diff 会在 @@ 行末尾附加函数名，例如："
  echo "     @@ -209,10 +209,10 @@ def get_data(self):"
fi

echo "  @@ 行预览："
grep '^@@' "$PATCH_TMP" | sed 's/^/    /'
echo ""

echo "→ 提交到 Shirakami（source_repo=$SOURCE_REPO，modes=$MODES）"

RESPONSE=$(curl -s -X POST "$API" \
  -H "Content-Type: application/json" \
  --data-binary "$(jq -n \
    --rawfile diff "$PATCH_TMP" \
    --arg repo "$SOURCE_REPO" \
    --argjson modes "$MODES" \
    '{input_diff: $diff, source_repo: $repo, modes: $modes}')")

rm -f "$PATCH_TMP"

TASK_ID=$(echo "$RESPONSE" | jq -r '.id // empty')
if [[ -z "$TASK_ID" ]]; then
  echo "错误：提交失败，响应：$RESPONSE" >&2
  exit 1
fi

echo ""
echo "✓ 任务已提交：$TASK_ID"
echo ""
echo "查询进度："
echo "  curl -s ${API}/$TASK_ID | jq '{status, progress}'"
echo ""
echo "查看调用链："
echo "  curl -s ${API}/$TASK_ID/chain | jq ."
echo ""
echo "查看 e2e 场景："
echo "  curl -s ${API}/$TASK_ID/e2e | jq ."

_wait_and_show "$TASK_ID"
