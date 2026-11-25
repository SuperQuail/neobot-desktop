NeoBot Desktop（Linux x64）
==========================

运行要求
--------
- GTK 3
- WebKit2GTK 4.1（运行时库 libwebkit2gtk-4.1-0）
- xdg-utils（打开目录/网页）
- DBus 会话总线（单实例与托盘）
- 中文字体，例如 Noto Sans CJK
- 高级终端需要系统里安装 gnome-terminal、kgx（GNOME Console）、konsole、xterm 之一，或设置 $TERMINAL

安装（用户级，不需要 root）
--------------------------
  ./install.sh

默认安装到 ~/.local：
  ~/.local/opt/neobot-desktop/neobot-desktop
  ~/.local/bin/neobot-desktop
  ~/.local/share/applications/neobot-desktop.desktop
  ~/.local/share/icons/hicolor/<尺寸>/apps/neobot-desktop.png

可通过 PREFIX 指定其他前缀：
  PREFIX="$HOME/.local" ./install.sh

卸载：
  ./uninstall.sh
数据目录（默认 ~/.local/share/NeoBotDesktop）不会被删除。

更新
----
Linux 版暂不支持应用内更新。请到 GitHub Releases 下载新的 tar.gz，退出启动器后
重新运行 install.sh；版本与数据目录会保留。

托盘
----
Cinnamon、KDE 等桌面原生支持状态栏图标；GNOME 需要安装并启用
gnome-shell-extension-appindicator，否则关闭窗口会直接退出。

更多说明见项目主页与 README。
