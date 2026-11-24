#!/usr/bin/env bash
# Cross-compile the Windows release from any host (developed on macOS).
#
# Pure-Go dependencies only (modernc.org/sqlite, jchv/go-webview2), so no CGO
# and no Windows toolchain is needed to produce the binaries.
set -euo pipefail

cd "$(dirname "$0")/.."
OUT=${OUT:-dist}
VERSION=${VERSION:-0.1.0}
LDFLAGS="-s -w"

rm -rf "$OUT"
mkdir -p "$OUT/server" "$OUT/client"

build() {
  local goos=$1 goarch=$2 cmd=$3 dest=$4 name=$5
  echo "  ${goos}/${goarch}  ${name}"
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
    go build -trimpath -ldflags "$LDFLAGS" -o "$dest/$name" "./cmd/$cmd"
}

# The UI scripts are embedded, not compiled, so a syntax error in them would
# ship silently and break the whole console at runtime. Catch it here.
if command -v node >/dev/null 2>&1; then
  echo "checking embedded UI scripts"
  for js in internal/*/web/*.js; do
    node --check "$js" || { echo "  语法错误: $js"; exit 1; }
    echo "  ok $js"
  done
else
  echo "warning: node 未安装，跳过前端语法检查"
fi

echo "running go vet and tests"
go vet ./...
go test ./...

echo "building CodeRoom v$VERSION -> $OUT/"
build windows amd64 coderoom-server "$OUT/server" coderoom-server.exe
build windows amd64 coderoom-client "$OUT/client" coderoom-client.exe

# 32-bit builds for older lab images.
build windows 386 coderoom-server "$OUT/server" coderoom-server-x86.exe
build windows 386 coderoom-client "$OUT/client" coderoom-client-x86.exe

# A populated toolchain/ is folded into the server package so the teacher's
# machine needs nothing installed. Absent, the server falls back to PATH and
# says so in its startup banner.
if [ -d toolchain ] && [ -n "$(ls -A toolchain 2>/dev/null | grep -v '^README.md$')" ]; then
  echo "  打包便携工具链 toolchain/ -> $OUT/server/toolchain/"
  mkdir -p "$OUT/server/toolchain"
  (cd toolchain && tar cf - --exclude README.md .) | (cd "$OUT/server/toolchain" && tar xf -)
else
  echo "  未发现便携工具链（toolchain/ 为空）；教师机需自行安装 gcc/python，"
  echo "  或参见 toolchain/README.md 放入便携版后重新构建"
fi

# The packaged notes live in packaging/ so this script and build.ps1 cannot
# drift apart. A UTF-8 BOM is prepended on the way out: older Windows Notepad
# reads a BOM-less file as the local ANSI codepage and mangles the Chinese.
copy_note() {
  printf '\xEF\xBB\xBF' > "$2"
  cat "$1" >> "$2"
}
copy_note packaging/server-readme.txt "$OUT/server/读我.txt"
# A double-clickable helper: Explorer's "run as administrator" cannot pass the
# -setup-firewall flag, so the teacher gets a script that elevates itself.
cp packaging/setup-firewall.cmd "$OUT/server/放行防火墙.cmd"
copy_note packaging/client-readme.txt "$OUT/client/读我.txt"
# The last-resort escape from a stuck exam lock. The client hides its console
# in normal use, so double-clicking the exe opens the UI rather than running a
# flag; this gives the teacher something they can double-click instead.
cp packaging/restore-guard.cmd "$OUT/client/恢复考场管控.cmd"

echo
echo "完成。产物:"
find "$OUT" -type f | sed 's/^/  /'
du -sh "$OUT"/* | sed 's/^/  /'
