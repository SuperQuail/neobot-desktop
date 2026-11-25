#!/usr/bin/env bash
# NeoBot Desktop 用户级卸载脚本：保留版本与数据目录，方便以后重装。
set -euo pipefail

prefix=${PREFIX:-$HOME/.local}
case "$prefix" in
    /*) ;;
    *) prefix="$PWD/$prefix" ;;
esac

rm -f "$prefix/bin/neobot-desktop"
rm -rf "$prefix/opt/neobot-desktop"
rm -f "$prefix/share/applications/neobot-desktop.desktop"
for icon in "$prefix"/share/icons/hicolor/*/apps/neobot-desktop.png; do
    if [ -f "$icon" ]; then
        rm -f "$icon"
    fi
done
rm -f "${XDG_CONFIG_HOME:-$HOME/.config}/autostart/neobot-desktop.desktop"

if command -v update-desktop-database >/dev/null 2>&1; then
    update-desktop-database "$prefix/share/applications" >/dev/null 2>&1 || true
fi

echo "NeoBot Desktop 已卸载。版本与数据目录（默认 ~/.local/share/NeoBotDesktop）已保留。"
