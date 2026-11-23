@echo off
rem 放行 Windows 防火墙，让学生端能连上本机。只需运行一次。
rem
rem 存在的理由：防火墙默认拦截新程序的入站连接，而服务端看起来完全正常
rem （照常监听、照常显示局域网地址），只是没有一台学生机连得上。
rem
rem 资源管理器的「以管理员身份运行」无法传参数，所以用这个脚本代劳：
rem 它会自己申请提权，再带参数调用服务端。

chcp 65001 >nul
cd /d "%~dp0"

net session >nul 2>&1
if %errorlevel% neq 0 (
    echo 需要管理员权限，正在申请...
    powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -Verb RunAs"
    exit /b
)

echo 正在添加防火墙入站规则...
echo.
coderoom-server.exe -setup-firewall
echo.
pause
