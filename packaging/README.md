# 发行包附带文件

这里是打进发行包的面向使用者的说明文件，由 `scripts/build.sh`（macOS/Linux
交叉编译）和 `scripts/build.ps1`（Windows 原生编译）共同引用。

放在这里而不是写死在两个构建脚本里，是为了避免两份文案各自漂移。

## 两类文件，编码要求相反

**说明文件**（`server-readme.txt`、`client-readme.txt`）打包时会在开头加上
UTF-8 BOM 再写入 `dist/`：较早版本的 Windows 记事本在没有 BOM 时按 ANSI
代码页解读，中文会变成乱码。仓库里的源文件保持无 BOM。

**批处理文件**（`setup-firewall.cmd` → 放行防火墙.cmd、`restore-guard.cmd`
→ 恢复考场管控.cmd）原样复制，**绝不能加 BOM**，而且必须是 CRLF 换行。
cmd.exe 会把开头的 BOM 当成命令的一部分而报错，LF 换行也会让它解析出错。
编辑这两个文件时请确认编辑器没有擅自改写这两项——`scripts/build.sh` 与
`build.ps1` 都是直接 `cp` / `Copy-Item`，不做任何转换。

这两个 .cmd 存在的理由相同：资源管理器的「以管理员身份运行」没法传参数，
而这两件事都需要参数（`-setup-firewall` / `-restore`）。脚本自己申请提权，
再带参数调用 exe。学生端平时隐藏控制台，双击 exe 只会打开界面，所以恢复
管控更加需要这样一个可双击的入口。
