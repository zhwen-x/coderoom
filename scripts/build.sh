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

cat > "$OUT/server/读我.txt" <<'TXT'
CodeRoom 判题服务端（教师端）
=============================
1. 双击 coderoom-server.exe，窗口会显示教师端网址和管理口令。
2. 浏览器会自动打开教师端；管理口令请勿转发给学生。
3. 窗口中列出的 "IP:端口" 就是告知学生端的服务器地址。
4. 数据全部保存在同目录的 coderoom-data 文件夹，整个文件夹可直接拷贝备份。
5. 判题需要 g++/gcc/python。若本机未安装，请把便携工具链放到：
      toolchain\mingw64\bin\g++.exe
      toolchain\python\python.exe
   或在教师端「系统 - 判题环境」中手动指定路径。
   建议与机房 IDE 的编译器版本保持一致。
TXT

cat > "$OUT/client/读我.txt" <<'TXT'
CodeRoom 学生端
===============
1. 双击 coderoom-client.exe，程序会自动搜索局域网内的服务端。
2. 若搜索不到，请按教师提供的地址手动输入（例如 192.168.1.10:8080）。
3. 输入学号和姓名后进入，题目会在教师开考后自动出现。
4. 在机房 IDE 中写好并自测代码，然后复制粘贴到本程序的编辑器中，
   点「保存」存到本机，点「提交判题」上交服务端。
5. 代码会先保存在本机的 coderoom-client-data 目录，再自动上传，
   网络中断不会丢失。必要时可用「更多 - 导出本机答卷」兜底补交。
6. 本程序需要 Windows 10 1803 及以上（依赖系统自带的 Edge WebView2）。
TXT

echo
echo "完成。产物:"
find "$OUT" -type f | sed 's/^/  /'
du -sh "$OUT"/* | sed 's/^/  /'
