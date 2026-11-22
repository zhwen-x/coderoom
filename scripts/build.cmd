@echo off
rem CodeRoom Windows 构建入口（双击即可运行）。
rem
rem 真正的逻辑在同目录的 build.ps1。这个包装的存在理由只有两个：
rem   1. 绕过 PowerShell 对未签名脚本的默认执行策略，省去手工改策略；
rem   2. 结束后 pause，双击运行时窗口不会一闪而过、看不到错误。
rem
rem 在已有终端里构建时，直接调 build.ps1 更顺手：
rem   powershell -ExecutionPolicy Bypass -File scripts\build.ps1

chcp 65001 >nul
setlocal

powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0build.ps1" %*
set EXITCODE=%ERRORLEVEL%

if not "%EXITCODE%"=="0" (
    echo.
    echo [失败] 构建未完成，请查看上方的错误信息。
)

echo.
pause
exit /b %EXITCODE%
