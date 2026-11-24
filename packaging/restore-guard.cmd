@echo off
rem 解除本机残留的考场管控（禁 U 盘 / 断外网）。
rem
rem 正常情况下不需要运行：考试结束、交卷登出、关闭客户端、客户端被强杀、
rem 机器重启，都会自动恢复。这是以上全部失效时的最后一道手动兜底。
rem
rem 它不需要服务端、不需要登录，读取本机的恢复记录后把设置还原，然后退出。
rem 本机没有待恢复的管控时，会直接告诉你「没有待恢复的考场管控」。
rem
rem 学生端平时隐藏控制台窗口，双击 exe 只会打开界面，所以用这个脚本来跑
rem 带参数的恢复命令。

chcp 65001 >nul
cd /d "%~dp0"

net session >nul 2>&1
if %errorlevel% neq 0 (
    echo 需要管理员权限，正在申请...
    powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -Verb RunAs"
    exit /b
)

echo 正在检查并解除本机的考场管控...
echo.
coderoom-client.exe -restore
echo.
pause
