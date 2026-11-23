# 便携工具链（判题用）

服务端判题需要 C/C++ 编译器和 Python 解释器。把它们按下面的布局放进本目录，
服务端启动时会自动探测，教师机**无需安装任何东西**。

放进来的二进制不入 git（见 `.gitignore`），因为那是第三方产物，应随发行包分发；
本说明文件本身是跟踪的。

## 期望布局

```
toolchain/
  mingw64/
    bin/
      g++.exe
      gcc.exe
      ...（MinGW-w64 的其余内容原样保留）
  python/
    python.exe
    python3xx.zip
    ...（Python embeddable 包原样解压）
```

备用路径（两者任一即可，探测时都会尝试）：

```
toolchain/bin/g++.exe
toolchain/bin/gcc.exe
toolchain/python3/python3.exe
```

## 怎么获取

- **MinGW-w64**：取任一「portable / standalone」构建（例如 winlibs.com 或
  niXman/mingw-builds-binaries 的 release），解压后把 `mingw64` 整个目录放进来。
  选 **win32 线程 + seh 异常** 的 64 位构建即可；本项目不依赖任何新语言特性。
- **Python**：取 python.org 的 **embeddable package**（`python-3.x.x-embed-amd64.zip`），
  解压到 `toolchain/python/`。嵌入版自带标准库 zip，`python -m py_compile` 可直接用，
  无需 pip。

## 放好后怎么验证

启动服务端，横幅会直接写明：

```
  可判语言   C++ / C / Python
```

若有缺失会列出来，并提示去教师端「系统 → 判题环境」配置路径。
教师端该页面还会显示每种语言**实际使用的可执行文件路径**，可据此确认用的是
便携工具链而不是机器上碰巧装着的另一个编译器。

## 版本怎么选

机房使用 Visual Studio / Dev-C++ / PyCharm / IDLE，不涉及语言最新特性，
因此任意较新版本均可。判题使用的语言标准在教师端「系统」中设置
（默认 C++14 / C11）——标准选得比机房 IDE 更旧是安全的，更新则可能出现
「学生本地能编译、服务端编译错误」。
