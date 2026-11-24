#Requires -Version 5.1
<#
.SYNOPSIS
    在 Windows 上原生编译 CodeRoom 发行包。

.DESCRIPTION
    scripts/build.sh 在 macOS/Linux 上交叉编译；本脚本在 Windows 上原生编译。
    两者产物一致，但本脚本多一层价值：go test 会在 Windows 上真实执行，
    从而实际跑到 Job Object 判题沙箱的代码路径 —— 那是交叉编译验证不了的部分。

    只依赖 Windows 10 自带的 PowerShell 5.1，无需安装任何额外组件。

.PARAMETER Out
    产物目录，默认 dist。

.PARAMETER Version
    写入横幅的版本号，默认 0.1.0。

.PARAMETER SkipTests
    跳过 go vet 和 go test。仅用于反复调试构建本身时提速，
    正式出包不要用。

.PARAMETER Arch
    要构建的架构：both（默认）、amd64、386。

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File scripts\build.ps1

.EXAMPLE
    .\scripts\build.ps1 -SkipTests -Arch amd64
#>
[CmdletBinding()]
param(
    [string]$Out = 'dist',
    [string]$Version = '0.1.0',
    [switch]$SkipTests,
    [ValidateSet('both', 'amd64', '386')]
    [string]$Arch = 'both'
)

$ErrorActionPreference = 'Stop'

# 控制台按 UTF-8 输出，否则本脚本里的中文会是乱码。
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

$repo = Split-Path -Parent $PSScriptRoot
Set-Location -LiteralPath $repo

function Write-Step { param([string]$Text) Write-Host "`n$Text" -ForegroundColor Cyan }
function Write-Ok   { param([string]$Text) Write-Host "  $Text" -ForegroundColor Green }
function Write-Warn { param([string]$Text) Write-Host "  $Text" -ForegroundColor Yellow }

# PowerShell 不会因原生命令返回非零而中断，必须显式检查。
function Assert-LastExit {
    param([string]$What)
    if ($LASTEXITCODE -ne 0) {
        throw "$What 失败（退出码 $LASTEXITCODE）"
    }
}

# 写入发行包里的说明文件，并加上 UTF-8 BOM：较早版本的 Windows 记事本在没有
# BOM 时按 ANSI 代码页解读，中文会变成乱码。仓库里的源文件保持无 BOM。
function Copy-Note {
    param([string]$Source, [string]$Destination)
    $text = [System.IO.File]::ReadAllText($Source, [System.Text.Encoding]::UTF8)
    $utf8Bom = New-Object System.Text.UTF8Encoding($true)
    # .NET 的当前目录与 PowerShell 的不是一回事，这里必须用绝对路径。
    [System.IO.File]::WriteAllText($Destination, $text, $utf8Bom)
}

# ---------------------------------------------------------------- 环境检查

Write-Step 'CodeRoom Windows 原生构建'

$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) {
    # Go being installed but absent from PATH is at least as common as it not
    # being installed at all, so check the usual locations before telling
    # someone to download something they already have.
    $guesses = @('C:\dev\go\bin\go.exe', 'C:\Go\bin\go.exe',
                 "$env:LOCALAPPDATA\Programs\Go\bin\go.exe",
                 "$env:ProgramFiles\Go\bin\go.exe")
    $found = $guesses | Where-Object { Test-Path $_ } | Select-Object -First 1

    Write-Host '错误: PATH 中找不到 go 命令。' -ForegroundColor Red
    Write-Host ''
    if ($found) {
        Write-Host ("但在 {0} 找到了 Go。把它所在的目录加入 PATH 后重试：" -f $found)
        Write-Host ('  $env:PATH = "{0};" + $env:PATH' -f (Split-Path -Parent $found))
    } else {
        Write-Host '请先从 https://go.dev/dl/ 安装 Go，安装后"重新打开"命令行窗口再运行本脚本'
        Write-Host '（安装程序写入的 PATH 不会影响已经打开的窗口）。'
    }
    exit 1
}
& go version
Assert-LastExit 'go version'

# ------------------------------------------------- 前端语法检查（可选）

# 教师端与学生端界面是内嵌的静态脚本，不参与 Go 编译。一个语法错误会被原样
# 打包进 exe，直到运行时整个控制台白屏才发现 —— 所以在这里拦掉。
Write-Step '检查内嵌界面脚本'
$node = Get-Command node -ErrorAction SilentlyContinue
if ($node) {
    $scripts = Get-ChildItem -Path (Join-Path $repo 'internal\*\web\*.js') -ErrorAction SilentlyContinue
    if (-not $scripts) {
        Write-Warn '未找到界面脚本，跳过'
    }
    foreach ($js in $scripts) {
        & node --check $js.FullName
        if ($LASTEXITCODE -ne 0) {
            throw "界面脚本语法错误: $($js.FullName)"
        }
        Write-Ok "ok $($js.FullName.Substring($repo.Length + 1))"
    }
} else {
    Write-Warn '未安装 node，跳过前端语法检查（不影响编译，但建议装上）'
}

# ---------------------------------------------------------------- 测试

if ($SkipTests) {
    Write-Step '已跳过 go vet 和 go test（-SkipTests）'
    Write-Warn '正式出包前请去掉 -SkipTests 重新构建'
} else {
    Write-Step '运行 go vet'
    & go vet ./...
    Assert-LastExit 'go vet'
    Write-Ok '通过'

    Write-Step '运行 go test'
    & go test ./...
    Assert-LastExit 'go test'
    Write-Ok '通过'
    Write-Ok '注意: 在 Windows 上运行时，判题相关测试会真实走到 Job Object 沙箱，'
    Write-Ok '      这是 macOS 交叉编译验证不到的代码路径。'
}

# ---------------------------------------------------------------- 编译

$outDir    = Join-Path $repo $Out
$serverDir = Join-Path $outDir 'server'
$clientDir = Join-Path $outDir 'client'

if (Test-Path -LiteralPath $outDir) {
    try {
        Remove-Item -LiteralPath $outDir -Recurse -Force
    } catch {
        # 反复构建-验证时最常见的失败：上一轮的 exe 还开着，目录删不掉。
        # 默认的 .NET 报错很难看懂，这里换成能直接照做的提示。
        throw ("无法清空 {0}\ : {1}`n请先关闭正在运行的 coderoom-server.exe / coderoom-client.exe 再重试。" -f $Out, $_.Exception.Message)
    }
}
New-Item -ItemType Directory -Force -Path $serverDir | Out-Null
New-Item -ItemType Directory -Force -Path $clientDir | Out-Null

function Build-One {
    param([string]$TargetArch, [string]$CmdName, [string]$DestDir, [string]$ExeName)
    $env:GOARCH = $TargetArch
    $dest = Join-Path $DestDir $ExeName
    Write-Host ("  windows/{0,-6} {1}" -f $TargetArch, $ExeName)
    # 直接调用而不做参数展开：PowerShell 5.1 展开含空格的参数（-ldflags "-s -w"）
    # 时行为不稳，这种写法是可靠的。
    & go build -trimpath -ldflags "-s -w" -o $dest "./cmd/$CmdName"
    Assert-LastExit "编译 $ExeName"
}

# 原来的值先存下来，结束时还原，免得污染调用者的会话。
$savedGoos   = $env:GOOS
$savedGoarch = $env:GOARCH
$savedCgo    = $env:CGO_ENABLED

try {
    $env:GOOS = 'windows'
    # 纯 Go 依赖（modernc.org/sqlite、jchv/go-webview2），不需要 C 工具链。
    $env:CGO_ENABLED = '0'

    Write-Step "编译 CodeRoom v$Version -> $Out\"

    if ($Arch -eq 'both' -or $Arch -eq 'amd64') {
        Build-One 'amd64' 'coderoom-server' $serverDir 'coderoom-server.exe'
        Build-One 'amd64' 'coderoom-client' $clientDir 'coderoom-client.exe'
    }
    if ($Arch -eq 'both' -or $Arch -eq '386') {
        # 32 位，给较老的机房镜像用。
        Build-One '386' 'coderoom-server' $serverDir 'coderoom-server-x86.exe'
        Build-One '386' 'coderoom-client' $clientDir 'coderoom-client-x86.exe'
    }
}
finally {
    $env:GOOS = $savedGoos
    $env:GOARCH = $savedGoarch
    $env:CGO_ENABLED = $savedCgo
}

# ---------------------------------------------------------- 便携工具链

Write-Step '打包便携工具链'
$toolchainSrc = Join-Path $repo 'toolchain'
$payload = @()
if (Test-Path -LiteralPath $toolchainSrc) {
    # README.md 是布局说明，不属于工具链本身。
    $payload = @(Get-ChildItem -LiteralPath $toolchainSrc -Force |
                 Where-Object { $_.Name -ne 'README.md' })
}
if ($payload.Count -gt 0) {
    $dest = Join-Path $serverDir 'toolchain'
    New-Item -ItemType Directory -Force -Path $dest | Out-Null
    foreach ($item in $payload) {
        # 逐项复制而不是 Copy-Item -Recurse -Exclude：后者在 PowerShell 中
        # 对嵌套目录的排除行为并不可靠。
        Copy-Item -LiteralPath $item.FullName -Destination $dest -Recurse -Force
    }
    Write-Ok "已打包 toolchain\ -> $Out\server\toolchain\"
} else {
    Write-Warn '未发现便携工具链（toolchain\ 为空）'
    Write-Warn '教师机需自行安装 gcc/python，或参见 toolchain\README.md 放入便携版后重新构建'
}

# ------------------------------------------------------------ 说明文件

Copy-Note (Join-Path $repo 'packaging\server-readme.txt') (Join-Path $serverDir '读我.txt')
# A double-clickable helper: Explorer's "run as administrator" cannot pass the
# -setup-firewall flag, so the teacher gets a script that elevates itself.
# Copied verbatim - it is a batch file and must keep its CRLF line endings and
# stay free of a BOM, which Copy-Note would add.
Copy-Item -LiteralPath (Join-Path $repo 'packaging\setup-firewall.cmd') `
          -Destination (Join-Path $serverDir '放行防火墙.cmd') -Force
Copy-Note (Join-Path $repo 'packaging\client-readme.txt') (Join-Path $clientDir '读我.txt')
# The last-resort escape from a stuck exam lock. The client hides its console
# in normal use, so double-clicking the exe opens the UI rather than running a
# flag; this gives the teacher something they can double-click instead.
Copy-Item -LiteralPath (Join-Path $repo 'packaging\restore-guard.cmd') `
          -Destination (Join-Path $clientDir '恢复考场管控.cmd') -Force

# ---------------------------------------------------------------- 汇总

Write-Step '完成。产物:'
$files = Get-ChildItem -LiteralPath $outDir -Recurse -File | Sort-Object FullName
foreach ($f in $files) {
    $rel = $f.FullName.Substring($outDir.Length + 1)
    Write-Host ("  {0,10:N0} 字节  {1}" -f $f.Length, $rel)
}
$total = ($files | Measure-Object -Property Length -Sum).Sum
Write-Host ("`n  合计 {0:N1} MB" -f ($total / 1MB))

Write-Host ''
Write-Host '下一步：' -ForegroundColor Cyan
Write-Host "  1. 运行 $Out\server\coderoom-server.exe，横幅会显示教师端网址与管理口令"
Write-Host "  2. 运行 $Out\client\coderoom-client.exe，确认 WebView2 窗口能正常打开"
Write-Host '  3. 按 README「Windows 真机验证清单」逐项确认判题沙箱是否按预期工作'
