#!/usr/bin/env bash
# docs/ 链接死链自查（AGENTS.md §6.1 白名单制的机械检查）。有 ❌ 退出码 1。
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
fail=0
while read -r f; do
  if git check-ignore -q "$f"; then echo "❌ 未入库（死链）: $f"; fail=1
  else echo "✅ $f"; fi
done < <(rg -o --no-filename '\]\((docs/[^)]+\.md)\)' -r '$1' AGENTS.md README.md DEVELOPMENT.md 2>/dev/null | sort -u)
exit $fail
