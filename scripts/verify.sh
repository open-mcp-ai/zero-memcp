#!/usr/bin/env bash
# 1729 可检索性验收。
#
# 验收标准是「答案内容能被召回」，不是「某个特定文件名排第一」。
# 这一点很重要：记忆库会持续增长，同一个问题的答案可能存在于多篇
# 切片里（案卷、当事人的提问、施工侧的排查记录）。要求某个文件名
# 永远第一，等于要求记忆库停止生长。
#
# 分层是这个脚本的第二层含义：插件的真实查询路径是
#   review → record → thought
# 而不是把三层混在一起搜。混搜时，今天写的过程思考会把两周前的
# 结论案卷挤下去——这不是检索坏了，是「过程稀释结论」的必然结果。
# 分层就是为了让结论层先说话。

set -u
PORT="${PORT:-1729}"
URL="http://127.0.0.1:${PORT}/mcp"
TS="$(date +%Y%m%d%H%M%S)"

pass=0
fail=0

# c <cmd>：以 root 身份（无凭证）执行一条 term 命令。
c() {
  curl -s --max-time 60 -X POST "$URL" \
    -H 'Content-Type: application/json' \
    -d "$(python3 -c '
import json,sys
print(json.dumps({"jsonrpc":"2.0","id":1,"method":"tools/call",
  "params":{"name":"term","arguments":{"cmd":sys.argv[1]}}}))
' "$1")" |
    python3 -c '
import json,sys
try:
    d=json.load(sys.stdin)
    print(d["result"]["content"][0]["text"] if "result" in d else
          "ERR: "+str(d.get("error",{}).get("message","")))
except Exception as e:
    print("ERR:", e)
'
}

check() {
  local desc="$1"; local out="$2"; local pattern="$3"
  if echo "$out" | grep -q "$pattern"; then
    echo "✅ $desc"; pass=$((pass+1))
  else
    echo "❌ $desc"; fail=$((fail+1))
  fi
}

echo '━━━ 1. 分层查询：结论层优先（插件的真实路径）━━━'
OUT=$(c 'recall 端口为什么是1729 --scope dev/zero@main.record:review')
echo "$OUT" | grep -E '\.md \(' | head -3
echo '--- 第一名正文片段 ---'
echo "$OUT" | sed -n '2,5p'
echo
check "答案内容（哈代-拉马努金）出现在第一屏" "$OUT" "哈代"
check "第一名是 review 层的案卷" "$(echo "$OUT" | grep -E '\.md \(' | head -1)" "review/"

echo
echo '━━━ 2. 换问法（语义泛化提问，走 srecall 向量路）━━━'
OUT2=$(c 'srecall 1729这个数字有什么含义 --scope dev/zero@main.record:review')
echo "$OUT2" | grep -E '\.md \(' | head -3
check "语义泛化仍能召回哈代-拉马努金" "$OUT2" "哈代"

echo
echo '━━━ 3. 跨人格协同：纬读简的案卷 ━━━'
OUT3=$(c 'recall 端口 1729 --scope dev/zero@main.record:review')
check "纬能读到简的 review 层" "$OUT3" "review/"

echo
echo '━━━ 4. 向量语义路（跨语言证据）━━━'
OUT4=$(c 'srecall 端口为什么是1729')
echo "$OUT4" | grep -E '\.md \(' | head -3
check "srecall 使用混合检索（semantic / semantic+keyword）" "$OUT4" "semantic"

echo
echo '━━━ 5. BM25 纯关键词对照 ━━━'
OUT5=$(c 'recall 1729 --scope dev/zero')
check "BM25 仍正常命中答案文档" "$OUT5" "1729"

echo
echo "━━━ 结果: $pass 通过 / $fail 失败 ━━━"
[ "$fail" -eq 0 ]
