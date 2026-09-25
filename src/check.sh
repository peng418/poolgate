#!/usr/bin/env sh
# 校验入口：vet + 竞态测试 + 构建。每次提交前跑一遍。
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
GO="${GO:-/tmp/go125/go/bin/go}"
export GOFLAGS=-mod=mod
export GOPATH=/tmp/go
export GOMODCACHE=/tmp/gomodcache
export GOTOOLCHAIN=local

cd "$HERE"

echo "== gofmt =="
UNFORMATTED=$(find "$HERE" -name '*.go' -not -path '*/build/*' | xargs "$GO"fmt -l 2>/dev/null || true)
if [ -n "$UNFORMATTED" ]; then
  echo "以下文件未格式化："
  echo "$UNFORMATTED"
  exit 1
fi

echo "== vet =="
"$GO" vet ./...

echo "== test (race) =="
"$GO" test -race -count=1 ./...

echo "== build =="
"$GO" build ./...

echo
echo "全部通过。"
