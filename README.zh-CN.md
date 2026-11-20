# remote-go

[English](README.md) · **简体中文**

一个用 Go 写的、类似 UU 远程的远程桌面原型。**没有任何服务器，也不需要注册账号**：两端都内嵌 [tailcat](https://github.com/tailscale/tailcat)。tailcat 是 Tailscale 的数据面（WireGuard、magicsock NAT 打洞、DERP 中继），去掉了控制面，所以不需要登录。连接只靠 UU 远程那样的「设备代码 + 验证码」。

```
 控制端 (Remote Go)                                         被控端
 ┌──────────────────────────┐   WireGuard (P2P UDP 直连)  ┌────────────────────────┐
 │ NSGo 原生界面             │ ═════════════════════════▶ │ tailcat 节点 :7900      │
 │ 远程画面: 原生视图硬件解码 │   打洞失败时经公共 DERP 中继 │ 截屏 → GPU H.264 → WS   │
 │ tailcat 客户端            │                             │ 键鼠注入                │
 └──────────────────────────┘                             └────────────────────────┘
        两端用设备代码、验证码和对应的中继配置算出同一个 tailcat 地址
```

## 连接原理

tailcat 的地址由「节点公钥 + 预共享密钥 + DERP 区域」完全确定（disco 密钥也由节点密钥推导），所以两端可以各自算出它（`internal/tunnel`）：

```
设备代码 = 8 位随机数字 + 1 位区域编号（1 纽约 · 2 旧金山 · 3 法兰克福 · 4 东京 → DERP 301–304，0 自建 DERP）
验证码   = 被控端当前的密码（8 位，随机或固定）
Argon2id(验证码, salt = "remote-go/tailcat/v1|" + 设备代码, t=3, m=64MiB, p=4) → 64 字节
         = WireGuard 节点私钥 (32) ‖ WireGuard 预共享密钥 (32)
```

- **被控端**用这把私钥启动 tailcat 服务器，挂在设备代码指定的 DERP 区域（默认首次启动时测速，也可在设置中指定），在隧道内监听 7900 端口。
- **控制端**用同样的输入算出地址，先 ping，再经隧道拨 WebSocket。magicsock 先走 DERP，随后尝试打洞升级为 P2P 直连；会话窗口的控制球菜单显示「P2P 直连」或「中继 XXX」。
- 代码不对时，算出的就是另一把不存在的公钥，结果和「对方不在线」一样。
- 更换验证码会让被控端换一把密钥重启，旧验证码和已有连接立即失效。

## 配置 tailcat 中继

桌面端打开 **设置 → 本机引导中继**，选择后点击 **应用**。

- **自动选择**会测速选择公共中继，并记住结果，让设备代码在重启后保持稳定。
- **公共区域**可选纽约、旧金山、法兰克福或东京。控制端根据设备代码末位找到对应区域。
- **自建**需在「自建 DERP 地址」填写运行 derper 的主机名或地址，例如 `derp.example.com:8443`。默认使用 TLS 端口 443，证书须受客户端信任并匹配主机名。设备代码末位为 `0`。桌面控制端在「设置」、安卓控制端在首页「自建 DERP 地址」保存相同地址即可连接。控制端可以保留自己的公共中继选择。

更换本机中继会重启被控服务，断开正在控制本机的连接。设备代码前八位保留，请把更新后的完整代码告诉对方。更换自建服务器地址时，控制端也需要更新保存的地址。末位为 1–9 的旧设备代码继续走各自的公共中继。

## 桌面客户端（类似 UU 远程）

可从 [GitHub Releases](https://github.com/i5shuyi/remote-go/releases/latest) 下载安装包，也可以在本地构建。

```bash
sh scripts/build.sh mac       # → bin/build-mac/<版本>/releases/*.dmg
sh scripts/build.sh windows   # → bin/build-windows/<版本>/releases/*-setup.exe（需要 makensis）
```

打开后不需要登录：

1. **被控端**的「本机设备」卡片显示 9 位**设备代码**和 8 位**验证码**，可以复制设备代码、更换验证码，或在设置里改成固定密码。左下角显示 tailcat 用的中继区域。
2. **控制端**在「连接设备」中输入对方的设备代码，点「连接」，再输入验证码，就会打开一个独立的远程会话窗口（可以同时连多台）。远程画面占满窗口，「•••」控制球默认位于右上角，可拖动；靠近任一边缘松手会吸附，拖到中间松手则停在原处。点击控制球可查看编码、帧率、码率、延迟和直连/中继状态，调整画质、帧率、分辨率，或使用全屏、Ctrl+Alt+Del 和断开连接。打开会话或收起菜单后即可直接使用键盘。
   在控制球菜单中手动勾选 **共享剪贴板**。此后复制的文字、截图和普通文件会双向同步。截图复制到剪贴板后，可在支持图片粘贴的远端聊天框中按 ⌘V 或 Ctrl+V。复制一个或多个文件后，可在远端 Finder 或资源管理器的目标文件夹粘贴。传输过程中按下粘贴快捷键，会等远端确认接收后再执行。
   每个会话默认关闭，重连要重新开启。仅观看会话禁用共享。与仅支持文字剪贴板的旧版连接时，会自动使用文字同步。
3. 在「最近连接」中，点击设备旁的「重新连接」即可重连。
4. 有人在控制本机时，顶部会出现提示条，可以一键断开。
5. 关闭主窗口不会退出，程序会留在菜单栏（Windows 是通知区域）继续允许远程控制，在那里可以重新打开窗口或退出。要真正退出，用托盘菜单的「退出」或 Cmd+Q。
6. 对方退出或断网后，会话窗口在 6 秒内（收不到心跳就判定）换成「远程会话已断开」页面，说明原因，可以一键重新连接。

界面用 [NSGo](https://github.com/nsgo-dev/nsgo) 绘制：macOS 上是 AppKit 原生视图，Windows 上是 Direct2D，没有 WebView。远程画面是注册给 NSGo 的原生组件（`internal/screen`，`core.Native`）：

- **macOS**：`NSView` + `AVSampleBufferDisplayLayer`。Annex-B 转成 AVCC 的 `CMSampleBuffer` 后交给 VideoToolbox 硬件解码，由系统合成器直接显示，不经过 CPU 拷贝。键鼠在视图里直接拿原生事件（物理键码，含左右修饰键、Command 快捷键）。
- **Windows**：NSGo 窗口里的子 `HWND`。Media Foundation H.264 解码器（MFT）输出 NV12，转成 BGRA 后用 GDI 缩放显示；键盘按扫描码转换，与布局无关。

会话客户端是 Go 写的（`internal/client`），通过 tailcat 隧道直接拨 WebSocket，不经过本地代理。

开发调试用的环境变量：

- `RG_DEV_CONNECT=<设备代码>/<验证码>`：启动时经 tailcat 连接。
- `RG_DEV_SESSION=127.0.0.1:7900/<验证码>`：不走隧道，直连本机测试被控端（`remote-go local`）。
- `RG_VERBOSE=1`：打印 tailcat 日志。
- `RG_SCREEN_DEBUG=1`：打印解码状态。
- `RG_DEV_SNAPSHOT=<目录>`（仅 macOS）：把窗口截图存成 PNG。

端到端测试（需要联网，会用到公共 DERP）：

```bash
RG_TAILCAT_E2E=1 go test ./internal/tunnel -run TestTailcat -v
RG_TAILCAT_HOST=1 go test ./internal/host -run TestServeTailcat -v -timeout 10m
```

第二条起一个测试画面的被控端，并打印设备代码和验证码，供桌面客户端连接。`RG_SCREEN_DEBUG=1` 打印解码状态，`RG_DEV_SNAPSHOT=<目录>`（仅 macOS）把窗口截图存成 PNG。

## 安卓客户端（仅控制端）

```bash
sh scripts/build.sh android
adb install -r bin/build-android/0.4.1/releases/remote-go-0.4.1-android-arm64.apk
```

构建 APK 需要 Python 3.9+、JDK 17+、Android SDK（platform 34 或更高）及带 d8 的 Build Tools 36+。可通过 `ANDROID_HOME` 指定 SDK 路径。

安卓版只用来控制电脑，不能被控。打开后输入电脑上显示的设备代码和验证码就能连接，最近连过的设备会记在列表里。

- **画面**：NSGo 的安卓运行时不能嵌入应用自己的原生视图，所以远程画面放在一个 WebView 里（`internal/mobile/player.html`）。Go 负责经 tailcat 收流，再把每帧 H.264 发给页面；页面用 WebCodecs 解码，Android System WebView 底层走 MediaCodec 硬件解码。页面跟不上时会丢掉差分帧，并请求关键帧重新同步。
- **操作**：控制球默认位于右上角，可拖动，靠近边缘松手后吸附。点按控制球展开工具栏，里面有键盘、操作模式、适应屏幕、Ctrl+Alt+Del、画质 / 帧率和断开。
  - 触控模式：轻点是左键，长按是右键，单指拖动是拖拽。
  - 触摸板模式：单指移动光标，轻点是左键，双指轻点是右键，轻点后按住拖动是拖拽。
  - 两种模式都是双指滑动滚动、双指张合缩放（最大 5 倍）。
- **共享剪贴板**：在会话面板中手动开启。手机复制文字、截图或文件后，回到远程会话即可同步到电脑。手机文件管理器需要将文件放入 Android 系统剪贴板；只在应用内执行的复制操作无法跨应用读取。
- **键盘**：输入框里打的字以 Unicode 文本发给被控端（新增的 `tx` 消息：macOS 用 `CGEventKeyboardSetUnicodeString`，Windows 用 `KEYEVENTF_UNICODE`），所以中文输入法不受对方键盘布局影响。上方一排是软键盘没有的 Esc、Tab、方向键、Del、Home/End、F5 等；Ctrl / Alt / Shift / ⌘(Win) 点一下会粘住，对下一个键生效，用来按快捷键。
- **网络**：Go 程序在安卓上以普通可执行文件运行，系统不让它读网卡列表。tailcat 引入了 tailscale 的 `androidbin` / `androiddns` 来补上网卡和 DNS。
- 会话中屏幕保持常亮，系统返回键依次收起键盘、收起工具栏，最后询问是否断开。断线后的处理与桌面版相同，可以一键重连。
- 不用安卓机也能调试界面：`RG_MOBILE=1 ./remote-go` 在桌面上打开手机尺寸的同一套界面，macOS 的 WKWebView 同样支持 WebCodecs。

## 在线更新与发布

从 0.3.0 起，桌面「设置」和安卓首页「默认设置」会显示当前版本，并提供「检查更新」。正式安装包默认每天检查一次。发现新版后，macOS 通过 Sparkle 更新应用，Windows 通过 WinSparkle 更新当前用户安装的程序，Android 在验证 APK 后打开系统安装界面。0.2.x 及更早的用户需要手动安装一次含更新器的版本。

从 0.4.0 起，桌面端可以在远控会话中更新主程序。被控端保留原隧道和验证码，控制端原窗口继续显示画面、接收键鼠输入，进行中的剪贴板传输也会继续。有会话时，新版主界面从菜单栏或通知区域启动，点击 **打开 Remote Go** 即可查看。新开的控制窗口使用新版，等被控连接和会话窗口全部关闭后，被控服务也会切到新版。从主程序托盘选择 **退出 Remote Go** 仍会结束所有会话。

从 0.3.0 首次升级请安排在会话结束后。macOS 若提示授权，请给会话进程授予屏幕录制和辅助功能权限。Android 更新仍使用系统安装界面。

每个系统和架构分别从 GitHub Releases 的 Latest 读取 `appcast-<目标>.xml`。清单链接到对应标签下的完整安装包，包使用 Ed25519 签名。

GitHub Actions 会在 PR、推送到 `main` 和手动触发时运行 Go 测试，并构建四个测试安装包。可在 **Build and release** 工作流的运行记录中下载，保留七天。测试 APK 使用临时签名。

发布时先提交 `nsgo.yaml` 的新版本号，再推送对应正式标签，例如 `v0.5.0`。标签工作流通过 Actions Secrets 中的 `NSGO_UPDATES_KEY` 和 `ANDROID_SIGNING_PEM` 沿用现有更新签名和 APK 签名身份。工作流验证签名、核对草稿中的 16 个附件后发布为 **Latest**，并从公开地址重新下载清单和安装包进行校验。发布任务失败后可重跑，以继续上传草稿或重新校验已发布版本。

也可在安装了 Go、Xcode 命令行工具、NSIS 和上述 Android 工具的 macOS 上运行 `sh scripts/build.sh release`。本地构建读取 `NSGO_UPDATES_KEY` 或 `~/.config/remote-go/updates.key` 的更新私钥，以及 `.nsgo/android/debug.pem` 的 APK 签名身份。两份私钥均应独立备份。

## 命令行用法

不带参数运行就是桌面客户端；带子命令时是无界面的命令行模式，适合服务器或无人值守的机器：

```bash
go build -o remote-go .
./remote-go host -p mypassword               # 被控端：打印设备代码和验证码
./remote-go view -p mypassword 868987922     # 控制端：经 tailcat 连接，在浏览器里打开控制界面（WebCodecs 解码）
./remote-go local                            # 不走隧道，只在本机 127.0.0.1 测试
./remote-go host -region 304                 # 选择东京公共中继
./remote-go host -derp derp.example.com:8443
./remote-go view -derp derp.example.com:8443 -p mypassword 123456780  # 换成被控端显示的代码和验证码
```

| 选项 | 说明 |
| --- | --- |
| `host -region 304` | 指定公共中继，省略则按固定设备代码或自动测速选择 |
| `host/view -derp 主机[:端口]` / `RG_DERP` | 自建 DERP，两端使用同一地址；固定设备代码时末位须与中继匹配 |
| `host -device 123456782` / `RG_DEVICE` | 固定设备代码（默认测速选最近的区域后随机生成） |
| `host -p xxxx` / `RG_PASSWORD` | 固定验证码（默认每次启动随机生成） |
| `host -view-only` | 仅观看，不接受键鼠控制 |
| `host -display 1` | 选择共享的显示器 |
| `host -name 办公室` | 显示给对方的设备名 |
| `-v` | 打印 tailcat 诊断日志 |
| `view -listen 127.0.0.1:0 -no-open` | 本地代理的监听地址；不自动打开浏览器 |

## 安全模型

1. **传输**：WireGuard（Noise IK + 预共享密钥）端到端加密，DERP 中继只能看到密文和公钥。被控端没有任何公网监听端口。
2. **寻址即认证**：不知道设备代码和验证码，就算不出节点公钥和预共享密钥，连握手都无法完成。DERP 不提供列出节点的功能。
3. **抗离线猜测**：每次尝试都要做一次 Argon2id（64 MiB 内存）。攻击者必须先拿到设备代码，才能针对它离线穷举；默认验证码 8 位 × 31 个字符 ≈ 2^39.6。想要更强，在设置里设置更长的固定密码即可。
4. **在线猜测**：每次猜错只是一次没人应答的握手，被控端感知不到，所以没法限速。安全性完全靠验证码的熵和 Argon2 的成本，**不要用短而常见的固定密码**。
5. 隧道内的 WebSocket 仍然校验验证码（常量时间比较，连续错误会锁定），作为第二层防护。
6. 默认使用 Tailscale 运营的公共 tailcat DERP，也可以配置自建中继。公共中继不保证可用性和带宽；打洞成功后画面不再经过中继。

## 平台支持

| | 画面传输 | 键鼠控制 |
| --- | --- | --- |
| macOS | ✅ **H.264 硬件编码**（ScreenCaptureKit + VideoToolbox，需「屏幕录制」权限） | ✅ CGEvent（需「辅助功能」权限） |
| Windows 10/11 (x64/ARM64) | ✅ **H.264 硬件编码**（DXGI 桌面复制 + D3D11 视频处理器 + Media Foundation） | ✅ SendInput |
| Linux | JPEG 逐帧（X11） | ❌ 暂未实现（仅观看） |
| Android | — 仅控制端（WebCodecs → MediaCodec 硬件解码） | — |

### macOS 硬件编码链路

```
ScreenCaptureKit ──NV12 IOSurface(显存, GPU 缩放)──▶ VideoToolbox H.264 (媒体引擎, 低延迟码控, 无 B 帧)
   ──Annex-B──▶ WebSocket ──▶ 控制端硬件解码（VideoToolbox / Media Foundation，CLI 的浏览器界面用 WebCodecs）
```

- 像素全程不经过 CPU，零拷贝；画面静止时 ScreenCaptureKit 不产生新帧，带宽接近 0。
- 控制端浏览器不支持 WebCodecs，或者被控端硬件编码启动失败（比如没有屏幕录制权限）时，自动回退到 JPEG。
- 解码出错或积压时，浏览器会请求关键帧来重新同步；调整画质、帧率或分辨率会重建编码会话。
- 码率由「画质」档位换算：1920×1080@15fps 的「标准」档约 2.5 Mbps。

### Windows 硬件编码链路

```
DXGI Desktop Duplication (BGRA 纹理) ──CopyResource──▶ D3D11 Video Processor (GPU 上 BGRA→NV12 + 缩放, BT.709)
   ──D3D11 纹理 (IMFDXGIDeviceManager, 零拷贝)──▶ Media Foundation 硬件 H.264 MFT (NVENC / Intel QSV / AMD AMF)
```

- 纯 Go 通过 syscall 调用 COM，不需要 cgo 或 mingw，`GOOS=windows go build` 就能交叉编译。
- COM 虚函数表下标由 [`internal/encoder/genvtbl`](internal/encoder/genvtbl) 从 mingw-w64 的 IDL 自动生成；struct 布局有编译期大小断言。
- 按顺序尝试系统里的每个硬件编码器；全部失败（没有 GPU、远程桌面会话、Windows N 版缺少媒体功能包）时回退到 JPEG。
- 码控用 CBR + 低延迟模式，不用 B 帧；收到关键帧请求时通过 `CODECAPI_AVEncVideoForceKeyFrame` 强制出 IDR，屏幕静止时会重新编码最后一帧。
- 在 Windows 真机上验证：`go test ./internal/encoder/ -run TestMediaFoundation -v`

## 已知限制 / 后续方向

- Windows 桌面复制的画面不含鼠标指针，目前控制端看到的是自己本地的指针；切换分辨率后会回退到 JPEG；旋转屏幕、HDR 屏幕还没处理。
- 传输还是 WebSocket over TCP，丢包时会有队头阻塞；可以换成 WebRTC（Pion）走 UDP。
- 原生客户端共享剪贴板支持纯文本（32 KiB）、图片（PNG 最大 32 MiB，最多 33,554,432 像素）和普通文件（单次最多 128 个，合计 1 GiB）。文件夹和符号链接不参与同步。文件全部接收后才能粘贴，接收端将文件暂存在应用缓存中。浏览器控制端暂不支持共享剪贴板。音频传输暂未支持。
- 安卓版的画面要经过 Go → Java → WebView 消息桥（base64 文本），比原生视图多几毫秒延迟和一些 CPU 开销；高码率（如 4K、60 帧、高清）可能跟不上，会自动丢帧并重新同步。安卓版还没在真机或模拟器上跑过，只在 macOS 上用同一套界面和 WebView 验证过，也暂时只打包了 arm64。
- Windows 控制端目前用 GDI 显示、CPU 做 NV12→BGRA，1080p 没问题，4K 高帧率会吃 CPU；可以换成 D3D11 交换链直接显示解码纹理。Windows 控制端也还拦不到 Win 键和 Alt+Tab（需要低级键盘钩子）。
- NSGo v0.2.0 的 API 还不稳定，升级时可能要改界面代码。`go tool nsgo build` 在 macOS 上关闭了 cgo，本项目用到了 cgo，所以用 `scripts/build.sh` 打包。
- Windows 上看不到 UAC 安全桌面或登录界面，要支持的话需要以 SYSTEM 服务运行。
- 「无服务器」指的是没有自己的服务器、没有账号：NAT 打洞的协调和打洞失败后的中继，使用选定的公共或自建 DERP（tailcat 目前是实验项目，API 和公共区域可能变化）。
- 双方都在对称 NAT 后时打不通，只能走中继，延迟和带宽受 DERP 限制。
