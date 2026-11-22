# 发行包附带文件

这里是打进发行包的面向使用者的说明文件，由 `scripts/build.sh`（macOS/Linux
交叉编译）和 `scripts/build.ps1`（Windows 原生编译）共同引用。

放在这里而不是写死在两个构建脚本里，是为了避免两份文案各自漂移。

打包时会在文件开头加上 UTF-8 BOM 再写入 `dist/`：较早版本的 Windows 记事本
在没有 BOM 时按 ANSI 代码页解读，中文会变成乱码。仓库里的源文件保持无 BOM。
