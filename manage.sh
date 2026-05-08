#!/bin/bash
# Shirakami 管理脚本
#
# 用法：
#
#   ── 提交任务 ──────────────────────────────────────────────────────────────
#
#   方式 1：repo + branch（推荐，server 端自动 git fetch + three-dot diff）：
#     SOURCE_REPO=vstation_network BRANCH=feature/fix-dfw bash manage.sh submit
#
#     多仓多分支：
#     BRANCHES='[{"repo":"vstation_network","branch":"feature/fix-dfw"},{"repo":"cvm_api","branch":"feature/fix-dfw"}]' \
#       bash manage.sh submit
#
#   方式 2：从 GitLab MR 自动拉取 diff：
#     GITLAB_TOKEN=<token> MR_URL=https://gitlab.example.com/org/repo/-/merge_requests/304 \
#       bash manage.sh submit
#
#   方式 3：使用本地 patch 文件：
#     SOURCE_REPO=vstation_network PATCH_FILE=/path/to/your.patch bash manage.sh submit
#
#   提交后自动等待并展示结果（默认开启，WAIT=0 可跳过）：
#     BRANCH=feature/fix-dfw bash manage.sh submit
#     WAIT=0 BRANCH=feature/fix-dfw bash manage.sh submit   # 仅打印 task_id
#
#   ── 查询与监控 ────────────────────────────────────────────────────────────
#
#   查看任务队列（最近 20 条）：
#     bash manage.sh tasks
#
#   只看排队/运行中的任务：
#     bash manage.sh tasks pending
#     bash manage.sh tasks running
#
#   持续监控单个任务状态（支持完整 ID 或前缀）：
#     bash manage.sh watch <task_id_or_prefix>
#
#   查看任务详情：
#     bash manage.sh task <task_id>
#
#   ── 结果查看 ──────────────────────────────────────────────────────────────
#
#   查看任务结果（模式可选 e2e / chain / ut / all）：
#     bash manage.sh e2e   <task_id>   # E2E 测试场景 + 入口点
#     bash manage.sh chain <task_id>   # 调用链 + 入口点
#     bash manage.sh ut    <task_id>   # UT 建议
#     bash manage.sh all   <task_id>   # 完整结果（原始 JSON）
#
#   ── 缓存管理 ──────────────────────────────────────────────────────────────
#
#   清理全部缓存：
#     bash manage.sh cache clear
#
#   清理指定任务的缓存（下次相同 diff 会重新分析）：
#     bash manage.sh cache clear <task_id>

set -euo pipefail

API_BASE=${API_BASE:-"http://localhost:8080"}
API="${API_BASE}/api/v1/tasks"
CACHE_API="${API_BASE}/api/v1/cache"

_usage() {
  grep '^#' "$0" | grep -v '^#!/' | sed 's/^# \{0,2\}//'
  exit 0
}

# _wait_and_show <task_id>
# 轮询任务直到完成，然后打印 e2e 结果摘要。
# 环境变量：
#   WAIT=0          跳过等待，仅打印 task_id 后返回
#   POLL_INTERVAL   轮询间隔秒数（默认 15）
#   TIMEOUT         最长等待秒数（默认 1800，即 30 分钟）
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

  local impact
  impact=$(echo "$e2e" | jq -r '.impact_summary // ""')
  if [[ -n "$impact" ]]; then
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  E2E 测试场景（impact_summary）"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "$impact"
    echo ""
  fi

  local ep_count
  ep_count=$(echo "$e2e" | jq '.entry_points | length')
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  echo "  Entry Points（共 ${ep_count} 个）"
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  echo "$e2e" | jq -r '.entry_points[]? | "  [\(.Repo)] \(.Function)  \(.File):\(.Line)"'
  echo ""

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

CMD=${1:-""}

case "$CMD" in
  # ── submit（提交分析任务）──────────────────────────────────────────────────
  submit)
    MODES=${MODES:-'["chain","e2e"]'}
    SOURCE_REPO=${SOURCE_REPO:-"vstation_network"}

    # ── 方式 1：branch 模式 ──────────────────────────────────────────────────
    if [[ -n "${BRANCH:-}" || -n "${BRANCHES:-}" ]]; then
      if [[ -n "${BRANCHES:-}" ]]; then
        echo "→ 提交到 Shirakami（branches 模式，modes=$MODES）"
        RESPONSE=$(curl -s -X POST "$API" \
          -H "Content-Type: application/json" \
          -d "$(jq -n \
            --argjson branches "${BRANCHES}" \
            --argjson modes "$MODES" \
            '{branches: $branches, modes: $modes}')")
      else
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
      echo "查询进度：  bash manage.sh watch $TASK_ID"
      echo "查看结果：  bash manage.sh e2e   $TASK_ID"
      _wait_and_show "$TASK_ID"
      exit 0
    fi

    # ── 方式 2 & 3：需要本地 diff ────────────────────────────────────────────
    PATCH_TMP=/tmp/shirakami_submit_$$.patch

    if [[ -n "${MR_URL:-}" ]]; then
      # 方式 2：从 GitLab MR URL 自动拉取
      if [[ -z "${GITLAB_TOKEN:-}" ]]; then
        echo "错误：使用 MR_URL 时必须设置 GITLAB_TOKEN 环境变量" >&2
        exit 1
      fi

      GITLAB_HOST=$(echo "$MR_URL" | grep -oP 'https?://[^/]+')
      MR_NUM=$(echo "$MR_URL" | grep -oP '\d+$')
      PROJECT_PATH=$(echo "$MR_URL" | grep -oP '(?<=\.com/)[^/]+/[^/]+(?=/-/)')
      PROJECT_ENCODED=$(echo "$PROJECT_PATH" | sed 's|/|%2F|g')

      echo "→ 拉取 MR diff：$GITLAB_HOST/$PROJECT_PATH!$MR_NUM"

      HTTP_CODE=$(curl -sf -o "$PATCH_TMP" -w "%{http_code}" \
        --header "PRIVATE-TOKEN: $GITLAB_TOKEN" \
        "${GITLAB_HOST}/${PROJECT_PATH}/-/merge_requests/${MR_NUM}.diff")

      if [[ "$HTTP_CODE" != "200" ]] || [[ ! -s "$PATCH_TMP" ]]; then
        echo "  .diff 方式失败（HTTP $HTTP_CODE），尝试 API 方式..." >&2
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
      echo "错误：未指定 diff 来源。请选择以下方式之一：" >&2
      echo "" >&2
      echo "  方式 1（推荐）— repo + branch：" >&2
      echo "    SOURCE_REPO=vstation_network BRANCH=feature/your-branch bash manage.sh submit" >&2
      echo "" >&2
      echo "  方式 2 — GitLab MR URL：" >&2
      echo "    GITLAB_TOKEN=<token> MR_URL=https://gitlab.example.com/org/repo/-/merge_requests/123 bash manage.sh submit" >&2
      echo "" >&2
      echo "  方式 3 — 本地 patch 文件：" >&2
      echo "    SOURCE_REPO=vstation_network PATCH_FILE=/path/to/your.patch bash manage.sh submit" >&2
      exit 1
    fi

    # diff 摘要
    HUNK_COUNT=$(grep -c '^@@' "$PATCH_TMP" || true)
    FILE_COUNT=$(grep -c '^diff --git' "$PATCH_TMP" || true)
    FUNC_COUNT=$(grep -oP '^@@[^@]*@@\s*\K\S.*' "$PATCH_TMP" | grep -cv '^\s*$' || true)
    echo "  文件数：$FILE_COUNT，hunk 数：$HUNK_COUNT，识别到函数名的 hunk：$FUNC_COUNT/$HUNK_COUNT"

    if [[ "$FUNC_COUNT" -eq 0 && "$HUNK_COUNT" -gt 0 ]]; then
      echo "  ⚠️  警告：所有 @@ 行均未带函数名，分析将无法识别变更函数！"
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
    echo "查询进度：  bash manage.sh watch $TASK_ID"
    echo "查看结果：  bash manage.sh e2e   $TASK_ID"
    _wait_and_show "$TASK_ID"
    ;;

  # ── tasks ──────────────────────────────────────────────────────────────────
  tasks)
    FILTER=${2:-""}
    echo "→ 查询任务列表..."
    RESP=$(curl -s "$API")
    if [[ -z "$RESP" || "$RESP" == "null" ]]; then
      echo "（无任务或服务不可达）"
      exit 0
    fi

    if [[ -n "$FILTER" ]]; then
      echo "$RESP" | jq -r --arg f "$FILTER" '
        ["ID", "状态", "进度", "类型", "仓库", "创建时间"],
        (
          .[]
          | select(.status == $f)
          | [
              .id,
              .status,
              (.progress // "—"),
              (.input_type // "—"),
              (.source_repo // "—"),
              (.created_at // "—" | split("T")[0])
            ]
        )
        | @tsv' | column -t -s $'\t'
    else
      echo "$RESP" | jq -r '
        ["ID", "状态", "进度", "类型", "仓库", "创建时间"],
        (
          .[]
          | [
              .id,
              .status,
              (.progress // "—"),
              (.input_type // "—"),
              (.source_repo // "—"),
              (.created_at // "—" | split("T")[0])
            ]
        )
        | @tsv' | column -t -s $'\t'
    fi

    echo ""
    echo "$RESP" | jq -r '
      "共 \(length) 条  " +
      "pending=\(map(select(.status=="pending")) | length)  " +
      "running=\(map(select(.status=="running")) | length)  " +
      "completed=\(map(select(.status=="completed")) | length)  " +
      "failed=\(map(select(.status=="failed")) | length)"'
    ;;

  # ── task（单条详情）─────────────────────────────────────────────────────────
  task)
    TASK_ID=${2:-""}
    if [[ -z "$TASK_ID" ]]; then
      echo "用法：bash manage.sh task <task_id>" >&2
      exit 1
    fi
    curl -s "${API}/${TASK_ID}" | jq .
    ;;

  # ── watch（持续轮询单个任务）────────────────────────────────────────────────
  watch)
    PREFIX=${2:-""}
    if [[ -z "$PREFIX" ]]; then
      echo "用法：bash manage.sh watch <task_id_or_prefix>" >&2
      exit 1
    fi
    POLL=${POLL_INTERVAL:-5}

    TASK_ID="$PREFIX"
    if [[ ${#PREFIX} -lt 36 ]]; then
      MATCHED=$(curl -s "$API" | jq -r --arg p "$PREFIX" '.[] | select(.id | startswith($p)) | .id' | head -1)
      if [[ -z "$MATCHED" ]]; then
        echo "错误：找不到前缀为 ${PREFIX} 的任务" >&2
        exit 1
      fi
      TASK_ID="$MATCHED"
      echo "→ 匹配到任务：$TASK_ID"
    fi

    echo "⏳ 持续监控任务（每 ${POLL}s 刷新，Ctrl-C 退出）..."
    echo ""
    last_status=""
    while true; do
      RESP=$(curl -s "${API}/${TASK_ID}" 2>/dev/null || true)
      if [[ -z "$RESP" ]]; then
        echo "[$(date '+%H:%M:%S')] 请求失败，重试..."
        sleep "$POLL"
        continue
      fi
      STATUS=$(echo "$RESP" | jq -r '.status // "unknown"')
      PROGRESS=$(echo "$RESP" | jq -r '.progress // ""')
      TS=$(date '+%H:%M:%S')

      if [[ "$STATUS" != "$last_status" || -n "$PROGRESS" ]]; then
        echo -n "[$TS] status=${STATUS}"
        [[ -n "$PROGRESS" ]] && echo -n "  progress=${PROGRESS}"
        echo ""
        last_status=$STATUS
      fi

      if [[ "$STATUS" == "completed" || "$STATUS" == "failed" ]]; then
        echo ""
        if [[ "$STATUS" == "completed" ]]; then
          echo "✅ 完成。查看结果："
          echo "  bash manage.sh e2e   $TASK_ID"
          echo "  bash manage.sh chain $TASK_ID"
          echo "  bash manage.sh ut    $TASK_ID"
        else
          ERR=$(echo "$RESP" | jq -r '.error // .error_message // "unknown error"')
          echo "❌ 失败：$ERR"
        fi
        break
      fi

      sleep "$POLL"
    done
    ;;

  # ── 结果视图：e2e / chain / ut / all ────────────────────────────────────────
  e2e|chain|ut|all)
    TASK_ID=${2:-""}
    if [[ -z "$TASK_ID" ]]; then
      echo "用法：bash manage.sh ${CMD} <task_id>" >&2
      exit 1
    fi

    if [[ "$CMD" == "all" ]]; then
      URL="${API}/${TASK_ID}"
    else
      URL="${API}/${TASK_ID}/${CMD}"
    fi

    RESP=$(curl -s "$URL")

    if [[ "$CMD" == "e2e" ]]; then
      IMPACT=$(echo "$RESP" | jq -r '.impact_summary // ""')
      if [[ -n "$IMPACT" ]]; then
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "  E2E 测试场景（impact_summary）"
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "$IMPACT"
        echo ""
      fi

      EP_COUNT=$(echo "$RESP" | jq '.entry_points | length')
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "  Entry Points（共 ${EP_COUNT} 个）"
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "$RESP" | jq -r '.entry_points[]? | "  [\(.Repo)] \(.Function)  \(.File):\(.Line)"'
      echo ""

      WARNINGS=$(echo "$RESP" | jq -r '.warnings[]? // empty')
      if [[ -n "$WARNINGS" ]]; then
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "  ⚠️  分析警告"
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "$WARNINGS" | sed 's/^/  /'
        echo ""
      fi

    elif [[ "$CMD" == "chain" ]]; then
      EP_COUNT=$(echo "$RESP" | jq '.entry_points | length')
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "  Entry Points（共 ${EP_COUNT} 个）"
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "$RESP" | jq -r '.entry_points[]? | "  [\(.Repo)] \(.Function)  \(.File):\(.Line)"'
      echo ""

      NODE_COUNT=$(echo "$RESP" | jq '.call_chain | length')
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "  调用链节点（共 ${NODE_COUNT} 个）"
      echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
      echo "$RESP" | jq -r '.call_chain[]? | "  [\(.Repo)] \(.Function)  \(.File):\(.Line)"'
      echo ""

    elif [[ "$CMD" == "ut" ]]; then
      UT=$(echo "$RESP" | jq -r '.ut_suggestions // ""')
      if [[ -n "$UT" && "$UT" != "null" ]]; then
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "  UT 建议"
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "$UT"
        echo ""
      else
        echo "（暂无 UT 建议）"
      fi

      FA_COUNT=$(echo "$RESP" | jq '.function_analyses | length // 0')
      if [[ "$FA_COUNT" -gt 0 ]]; then
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "  函数分析（共 ${FA_COUNT} 个）"
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "$RESP" | jq -r '.function_analyses[]? | "  \(.entry_function // .function // "—")  scenarios=\(.entry_scenarios | length)"'
        echo ""
      fi

    else
      echo "$RESP" | jq .
    fi
    ;;

  # ── cache ───────────────────────────────────────────────────────────────────
  cache)
    SUBCMD=${2:-""}
    TASK_ID=${3:-""}

    if [[ "$SUBCMD" != "clear" ]]; then
      echo "用法：" >&2
      echo "  bash manage.sh cache clear            # 清理全部缓存" >&2
      echo "  bash manage.sh cache clear <task_id>  # 清理指定任务缓存" >&2
      exit 1
    fi

    if [[ -n "$TASK_ID" ]]; then
      echo "→ 清理任务 ${TASK_ID} 的缓存..."
      RESP=$(curl -s -X DELETE "${API}/${TASK_ID}/cache")
      DELETED=$(echo "$RESP" | jq -r '.deleted // 0')
      MSG=$(echo "$RESP" | jq -r '.message // ""')
      if [[ -n "$MSG" ]]; then
        echo "  $MSG"
      else
        echo "  ✓ 已清理（deleted=${DELETED}）"
      fi
    else
      echo "→ 清理全部缓存..."
      RESP=$(curl -s -X DELETE "$CACHE_API")
      DELETED=$(echo "$RESP" | jq -r '.deleted // 0')
      echo "  ✓ 已清理 ${DELETED} 条缓存记录"
    fi
    ;;

  # ── help ────────────────────────────────────────────────────────────────────
  help|--help|-h|"")
    _usage
    ;;

  *)
    echo "未知命令：$CMD" >&2
    echo "运行 bash manage.sh help 查看用法" >&2
    exit 1
    ;;
esac
