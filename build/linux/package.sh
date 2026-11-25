#!/usr/bin/env bash
# 把 build/bin/neobot-desktop 打成可直接分发的 Linux x64 tar.gz。
set -euo pipefail

root=$(cd -- "$(dirname -- "$0")/../.." && pwd)
name=neobot-desktop-linux-amd64
binary="$root/build/bin/neobot-desktop"
if [ ! -x "$binary" ]; then
    echo "找不到可执行文件：$binary；请先运行 wails build" >&2
    exit 1
fi

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
package="$stage/$name"
mkdir -p "$package"
install -m 0755 "$binary" "$package/neobot-desktop"
install -m 0755 "$root/build/linux/install.sh" "$root/build/linux/uninstall.sh" "$package/"
install -m 0644 "$root/build/linux/neobot-desktop.desktop.in" "$root/build/linux/README.txt" "$package/"
cp -a "$root/build/linux/icons" "$package/icons"

mkdir -p "$root/build/bin"
tar -C "$stage" -czf "$root/build/bin/$name.tar.gz" "$name"
echo "已生成 build/bin/$name.tar.gz"
