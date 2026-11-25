# NeoBot Desktop

[NeoBot](https://github.com/SuperQuail/NeoBot) 的桌面启动器，用图形界面安装、启动和管理机器人。支持切换 NeoBot 版本、查看运行日志，并在兼容的版本中直接绑定 QQ 官方机器人、配置回复模型。

运行所需的 Python 和 uv 由启动器自动下载和管理。

## 下载与准备

当前支持 **Windows x64** 和 **Linux x64**。前往 [GitHub Releases](https://github.com/SuperQuail/neobot-desktop/releases)，按平台下载并安装。

开始前请准备：

- 可联网的电脑，首次安装需要下载运行环境和 NeoBot。
- 用于绑定 QQ 官方机器人的 QQ 账号，以及模型供应商提供的 API Key。

### Windows

下载 `neobot-desktop-amd64-installer.exe`，按发行说明安装后打开 NeoBot Desktop。

Windows 还需要 [Microsoft Edge WebView2 运行时](https://developer.microsoft.com/microsoft-edge/webview2/)：主流 Windows 电脑通常已自带，仅在启动时提示缺失时需要下载安装。

### Linux

下载 `neobot-desktop-linux-amd64.tar.gz`，解压后运行用户级安装脚本（不需要 root）：

```bash
tar -xzf neobot-desktop-linux-amd64.tar.gz
cd neobot-desktop-linux-amd64
./install.sh
```

默认安装到 `~/.local`，可从应用菜单启动，或在 `~/.local/bin` 位于 `PATH` 时运行 `neobot-desktop`。卸载时在同一目录运行 `./uninstall.sh`；版本与数据目录会保留。

Linux 运行需要：

- GTK 3 和 WebKit2GTK 4.1（Ubuntu 22.04+、Arch、Fedora 等较新发行版）
- `xdg-utils`（打开目录和网页）
- DBus 会话总线
- 中文字体，例如 Noto Sans CJK

Arch：

```bash
sudo pacman -S --needed gtk3 webkit2gtk-4.1 xdg-utils noto-fonts-cjk
```

Ubuntu / Debian：

```bash
sudo apt install libgtk-3-0 libwebkit2gtk-4.1-0 xdg-utils fonts-noto-cjk
```

Linux 版暂不支持应用内自动更新：有新版本时应用会提示到发布页下载。退出启动器后重新运行 `install.sh` 即可覆盖安装，数据目录不受影响。

## 第一次使用

### 安装并启动 NeoBot

1. 打开顶部的「版本」，在「可下载的稳定版」中选择版本，点击「下载」。
2. 等待环境准备完成，在「已安装」中确认启动版本。需要更换时，点击「设为启动版本」。
3. 回到「启动」，点击「启动 NeoBot」。启动进度和运行日志会显示在页面中。

如果「一键准备」提示没有推荐版本，按上面的步骤手动选择即可；出现「安装并启动推荐版本」时，也可以直接使用该入口。

### 连接 QQ 并配置模型

启动后，如果出现「继续快捷配置」，点击进入配置页面：

1. 在「绑定 QQ 官方机器人」中点击「扫码绑定」，用手机 QQ 扫码。已有机器人凭据时，也可以展开「手动配置 AppID / AppSecret」填写并保存。
2. 在「配置回复模型」中填写页面所示供应商对应的 API Key，点击「测试并保存」。更换供应商或模型时，展开「高级：自定义供应商和模型」，填写 HTTPS API 地址和模型名。
3. 如果提示配置需要重启，点击「重启当前版本」。
4. 确认 QQ 显示「已连接」、模型配置已生效后，在手机 QQ 中给机器人发一条消息，检查能否收到回复。

部分旧版 NeoBot 不支持桌面快捷配置。遇到「此版本请使用网页管理完成配置」时，点击「打开网页管理」，在浏览器中继续设置。已配置的网页面板仍沿用该版本自己的监听设置。

## 日常使用

| 页面 | 可以做什么 |
| --- | --- |
| 启动 | 启动或停止 NeoBot，查看 QQ 和模型状态，打开网页管理，查看和复制本次运行的日志。 |
| 版本 | 下载稳定版或预览版，保留多个已安装版本，选择下次启动的版本，安装或更新 GitHub 分支，修复或卸载已安装的版本。 |
| 诊断 | 查看 NeoBot 的完整日志，运行体检，导出诊断包。在「存储」中查看数据目录和磁盘占用，切换版本隔离，清理下载缓存、临时文件或历史快照。在「高级」中打开当前版本的 Python 环境终端。 |
| 设置 | 选择下载源，设置常驻运行方式，在「关于」中检查启动器更新；Windows 可下载并安装，Linux 会引导到发布页手动下载。 |

切换运行版本时，先在「启动」页停止 NeoBot，再选择其他已安装版本并启动。运行中更改版本选择，只会影响下次启动。

默认情况下**关闭窗口会把启动器收进系统托盘，NeoBot 继续在后台运行**；要彻底退出，请用托盘菜单里的「退出」。

在「设置」→「常驻运行」里可以调整三件事：关闭窗口时是否收进托盘、是否随系统登录自动启动、NeoBot 意外退出时是否自动重启。托盘不可用的环境（例如远程桌面缺少通知区域，或 GNOME 未启用 AppIndicator 扩展）会自动退回「关闭即退出」。

再次打开启动器会显示原来的窗口。窗口已最小化时会自动恢复。

## 版本与数据

默认情况下，各个版本共用数据目录。数据根目录位置：

- Windows：`%LocalAppData%\NeoBotDesktop`
- Linux：`~/.local/share/NeoBotDesktop`（遵循 `XDG_DATA_HOME`）

需要查找配置、插件或备份数据时，打开「诊断」页，在「存储」卡片的「数据目录」一行复制完整路径，或点击「打开」进入目录。手动备份前请先停止 NeoBot。

共用数据时，切换到较新的稳定版前会自动备份现有数据；切回旧稳定版需要恢复该版本的兼容快照。恢复后，新版期间新增的聊天、配置和插件数据不会出现在旧版中，当前数据会另存备份。没有兼容快照时，启动器会阻止降级。

### 试用预览版或 GitHub 分支

预览版在「版本」页的「预览版」折叠区中。点击页面右上角的「从 GitHub 安装」，可使用主仓库 `SuperQuail/NeoBot`，也可以在「自定义仓库」中填写公开 fork 的 GitHub 地址或 `owner/repo`。选择或输入分支名后安装，留空会使用该仓库的默认分支。

![GitHub 安装窗口默认选中主仓库，可切换到自定义仓库](docs/images/github-install.png)

不同仓库的同名分支可同时安装。要获取某个分支的最新代码，在「已安装」中找到对应版本并点击「更新」。如果它正在运行，先停止它。

分支代码可能包含尚未正式发布的改动，安装时需要能访问 GitHub。预览版和分支默认也会使用共用数据，启动时可能修改配置或数据库，这些修改会影响之后使用同一数据目录的正式版。试用前建议备份数据，或开启「版本隔离」。

### 让不同版本使用独立数据

停止 NeoBot 并等待当前任务结束后，在「诊断」页的「存储」卡片打开「版本隔离」开关，点击「开启」确认。

- **开启后**：每个版本使用独立目录。现有共用数据不会复制、移动或删除，新目录首次使用需要重新配置。
- **关闭后**：各版本重新使用共用目录；已有的独立数据会保留，但不再使用。

此前预览版迁移留下的独立数据，可以在开启版本隔离后继续使用。

### 高级：从终端维护 Python 环境

在「诊断」页的「高级」卡片中点击「打开终端」，会打开一个已经进入当前版本虚拟环境的系统终端（Windows 为 `cmd.exe`，Linux 为 `gnome-terminal`、`konsole`、`xterm` 等终端模拟器），工作目录是该版本的数据目录。终端里可以直接使用 `python`、`uv` 和 `python -m neobot_app.cli doctor`；`uv venv` 默认不安装 pip，安装包请用 `uv pip ...`。

Linux 会依次尝试 `$TERMINAL`、`xdg-terminal-exec`、`gnome-terminal`、`kgx`（GNOME Console）、`konsole`、`xfce4-terminal`、`kitty`、`alacritty`、`wezterm`、`foot`、`xterm`。没有可用终端时请先安装其中一个，或设置 `$TERMINAL`。默认使用 `bash`；如果系统没有 bash，则回退到 `$SHELL` 或 `/bin/sh`。

第一次打开需要确认一次风险提示，之后不再重复弹出。终端是普通系统终端，拥有当前账户权限，并不是沙箱：

- `data/.env` 和 `data/.neobot/control.json` 含凭据，请勿截图或粘贴到公开渠道。
- 修改 Python 环境前先停止 NeoBot；NeoBot 运行期间不要启动第二个实例。
- 启动器的「修复安装」或版本更新可能重置你对 venv 的手动修改；长期依赖建议通过插件声明，或另建独立环境。
- 不要手删 `versions/` 或 `settings.json`。

## 遇到问题

| 情况 | 可以尝试的操作 |
| --- | --- |
| 下载慢或安装失败 | 在「设置」→「下载源」切换「官方源」「国内加速」或「自定义」，再重试。发行版安装失败时也可点击「用官方源重试」。国内加速覆盖 Python、uv 和软件包，均使用国内镜像（分支安装仍按分支自带的 uv.lock）。 |
| QQ 二维码过期 | 重新生成二维码，再用手机 QQ 扫码。 |
| QQ 或模型配置已保存，但没有生效 | 查看页面是否提示重启，按提示点击「重启当前版本」。 |
| 机器人启动失败或没有回复 | 先在「启动」页查看 QQ、模型状态和本次运行日志。要翻看 NeoBot 的日志文件，在「诊断」页点击「查看完整日志」。要检查运行环境、依赖、本机接口和磁盘空间，在同页的「体检」卡片点击「开始体检」。 |
| Linux 托盘图标不显示 | Cinnamon、KDE 等原生支持；GNOME 需要安装并启用 `gnome-shell-extension-appindicator`。没有宿主时关闭窗口会直接退出。 |
| Linux 打不开终端 | 安装 `gnome-terminal`、`konsole` 或 `xterm` 之一，或设置 `$TERMINAL` 指向可用的终端模拟器。 |
| Linux 启动器更新 | 目前请到 [GitHub Releases](https://github.com/SuperQuail/neobot-desktop/releases) 下载新的 Linux tar.gz，重新运行 `install.sh`；数据目录会保留。 |
| Linux 提示缺少 WebKit 或无法启动 | 确认系统有 `libwebkit2gtk-4.1` 和 GTK 3；本版本需要 WebKit2GTK 4.1（Ubuntu 22.04+ 等较新发行版）。 |

仍有问题时，在「诊断」页的「体检」卡片点击「导出诊断包」，再点击「打开所在文件夹」。请将生成的 ZIP 文件和操作步骤一并提交到本仓库的 [Issues](https://github.com/SuperQuail/neobot-desktop/issues)。

诊断包包含启动器与系统版本、近期日志、体检结果、插件启停状态和磁盘占用。导出时会遮盖日志中能识别的凭据，并跳过 `.env`、本机控制接口和插件的配置文件。提交前可解压查看内容。

## 开发与构建

<details>
<summary>从源码构建或参与开发</summary>

准备 Go 1.26 或更新版本、Node.js 22.12 或更新版本以及 npm。其他环境要求见 [Wails v2 安装文档](https://v2.wails.io/docs/gettingstarted/installation/)。

### Windows

```powershell
go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.1
git clone https://github.com/SuperQuail/neobot-desktop.git
cd neobot-desktop
wails build -platform windows/amd64
# 生成的程序位于 build/bin/neobot-desktop.exe
```

### Linux

Arch 需要：

```bash
sudo pacman -S --needed base-devel pkgconf gtk3 webkit2gtk-4.1 nodejs npm xdg-utils noto-fonts-cjk
```

Ubuntu 22.04+ 需要：

```bash
sudo apt install build-essential pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev nodejs npm
```

Wails 需要显式使用 WebKit2GTK 4.1 的构建标签：

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.1
wails build -tags webkit2_41
# 生成的程序位于 build/bin/neobot-desktop
```

`wails doctor` 在 Arch 上可能误报 `libwebkit Not Found`，因为它只识别 `webkit2gtk-4.0`；只要能找到 `webkit2gtk-4.1` 的 pkg-config 文件，`-tags webkit2_41` 就能正常构建。

打 Linux 安装包：

```bash
./build/linux/package.sh
# 生成 build/bin/neobot-desktop-linux-amd64.tar.gz
```

完整构建后，可在仓库根目录运行：

```bash
# 启动开发环境（Linux 需要 -tags webkit2_41）
wails dev -tags webkit2_41

# 运行后端测试
go test ./...
```

浏览器调试使用 `http://127.0.0.1:34217`，支持 Go 调用。Vite 开发服务使用 `http://127.0.0.1:5187`，端口被占用时会报错。全新检出后，先完成一次 Wails 构建，再运行 Go 测试，以生成所需的前端嵌入文件。

### 发布与推荐版本

Linux 发布包在 Ubuntu 22.04 上构建（glibc 2.35、WebKit2GTK 4.1），以兼顾较新的发行版；本机 Arch 构建的程序只适合本机使用。

维护推荐版本时，先在未安装 Python 和 uv 的干净环境中验收 NeoBot 正式发行包，Windows 和 Linux 都确认可以安装、绑定 QQ、测试模型、收到首条回复并打开网页面板。验收通过后，在主分支的推荐清单中填写 `version`，有最低启动器版本要求时再填写 `min_launcher`。`version` 留空表示暂不推荐。[推荐版本清单](recommended.json)

</details>
