#!/usr/bin/env sh
# 构建 PoolGate 二进制。
#
# Go 工具链不在 PATH 里（本机位置 /tmp/go125/go/bin/go），因此显式指定。
# 用法： ./build.sh [版本]   →  build/poolgate
set -eu

VERSION="${1:-dev}"
HERE=$(cd "$(dirname "$0")" && pwd)
OUT="$HERE/build"
ROOT="$HERE"

GO="${GO:-/tmp/go125/go/bin/go}"
export GOFLAGS=-mod=mod
export GOPATH=/tmp/go
export GOMODCACHE=/tmp/gomodcache
export GOTOOLCHAIN=local

mkdir -p "$OUT"

# 前端资源自检。飞牛应用网关会把 /app/poolgate 前缀剥掉再转发，浏览器侧
# 必须自己带上前缀 —— 一旦 index.html 里出现根绝对路径，页面会「不报错但
# 全是未渲染的 {{ }}」，很难查。构建期直接挡掉。
echo "== 前端资源自检 =="
INDEX="$ROOT/internal/webui/dist/index.html"
[ -f "$INDEX" ] || { echo "缺 $INDEX"; exit 1; }
if grep -nE '(src|href)="/' "$INDEX"; then
  echo "!! index.html 含根绝对路径（网关前缀下会 404）：改用 ./ 相对路径"
  exit 1
fi
if grep -qE '"[[:space:]]*/api/' "$INDEX"; then
  echo "!! index.html 含双引号包裹的 \"/api/ 字面量：网关会再补一次前缀，导致路径重复"
  exit 1
fi
grep -q "vue.global.prod.js" "$INDEX" || { echo "!! index.html 未引用 vue.global.prod.js"; exit 1; }
# 插值里不能出现尖括号：HTML 解析器会把 {{ x || '<conf>/creds/' }} 里的 <conf>
# 当成元素开始标签，模板被切碎后 Vue 报「Invalid end tag」，那一段渲染不出来。
# （0.3.5 实测过：`paths.creds_dir` 为空时才会显形，属于「平时看不见」的坏味道。）
if grep -nE '\{\{[^}]*<' "$INDEX"; then
  echo "!! 插值表达式里含 < ：会被 HTML 解析器当标签切碎，请改用别的写法（如「配置目录/creds/」）"
  exit 1
fi

# 前端脚本语法自检。Vue 的模板在 HTML 里，脚本一有语法错就整页不挂载，
# 症状是「白屏 + 未渲染的 {{ }} + 遮罩挡住点击」，比 404 更难查。
# 这里把 <script> 抽出来交给 node --check，构建期就挡掉。
if command -v node >/dev/null 2>&1; then
  node - "$INDEX" <<'NODE'
const fs=require("fs");
const html=fs.readFileSync(process.argv[2],"utf8");
const m=html.match(/<script>\n([\s\S]*?)\n<\/script>/);
if(!m){console.error("!! 未找到内联 <script> 块");process.exit(1);}
fs.writeFileSync("/tmp/pg-index-check.js",m[1]);
NODE
  node --check /tmp/pg-index-check.js || { echo "!! 前端脚本有语法错误（见上）"; exit 1; }
  rm -f /tmp/pg-index-check.js
  echo "  内联脚本语法通过"
else
  echo "  跳过脚本语法自检（无 node）"
fi
echo "  通过（相对路径 + 单引号接口路径）"

# -s -w 去掉符号表与调试信息，单文件交付体积更小。
cd "$ROOT"
"$GO" build \
  -ldflags "-s -w -X main.Version=$VERSION" \
  -o "$OUT/poolgate" \
  ./cmd/poolgate

echo "已构建 $OUT/poolgate (版本 $VERSION)"
