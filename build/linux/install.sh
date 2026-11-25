#!/usr/bin/env bash
# NeoBot Desktop 用户级安装脚本：不写系统目录，不需要 root。
set -euo pipefail

here=$(cd -- "$(dirname -- "$0")" && pwd)
prefix=${PREFIX:-$HOME/.local}
case "$prefix" in
    /*) ;;
    *) prefix="$PWD/$prefix" ;;
esac

if [ ! -f "$here/neobot-desktop" ]; then
    echo "找不到 neobot-desktop，请在解压后的目录中运行 install.sh" >&2
    exit 1
fi
case "$(uname -m)" in
    x86_64 | amd64) ;;
    *)
        echo "当前安装包只支持 x86_64（检测到 $(uname -m)）" >&2
        exit 1
        ;;
esac

app_dir="$prefix/opt/neobot-desktop"
bin_dir="$prefix/bin"
applications="$prefix/share/applications"
icons="$prefix/share/icons/hicolor"

install -d -m 0755 "$app_dir" "$bin_dir" "$applications"
# 先写到临时文件再 rename：启动器正在运行时直接覆盖可执行文件会得到 ETXTBSY。
install -m 0755 "$here/neobot-desktop" "$app_dir/.neobot-desktop.new"
mv -f "$app_dir/.neobot-desktop.new" "$app_dir/neobot-desktop"
ln -sfn "$app_dir/neobot-desktop" "$bin_dir/neobot-desktop"

exec_path="$app_dir/neobot-desktop"
# Desktop Entry 的引号内需要转义反斜杠、双引号、反引号和美元符号；% 是字段码前缀。
exec_path=${exec_path//\\/\\\\}
exec_path=${exec_path//\"/\\\"}
exec_path=${exec_path//\`/\\\`}
exec_path=${exec_path//\$/\\\$}
exec_path=${exec_path//%/%%}
while IFS= read -r line; do
    case "$line" in
        Exec=@EXEC@) printf 'Exec="%s"\n' "$exec_path" ;;
        *) printf '%s\n' "$line" ;;
    esac
done < "$here/neobot-desktop.desktop.in" > "$applications/neobot-desktop.desktop"
chmod 0644 "$applications/neobot-desktop.desktop"

for icon in "$here"/icons/hicolor/*/apps/neobot-desktop.png; do
    [ -f "$icon" ] || continue
    size=$(basename "$(dirname "$(dirname "$icon")")")
    install -d -m 0755 "$icons/$size/apps"
    install -m 0644 "$icon" "$icons/$size/apps/neobot-desktop.png"
done

if command -v update-desktop-database >/dev/null 2>&1; then
    update-desktop-database "$applications" >/dev/null 2>&1 || true
fi
if command -v gtk-update-icon-cache >/dev/null 2>&1; then
    gtk-update-icon-cache -f -t "$icons" >/dev/null 2>&1 || true
fi

echo "NeoBot Desktop 已安装到 $app_dir"
echo "可从应用菜单启动，或运行：$bin_dir/neobot-desktop"
case ":$PATH:" in
    *":$bin_dir:"*) ;;
    *) echo "提示：$bin_dir 不在 PATH 中，请在 shell 配置里加入。" ;;
esac
