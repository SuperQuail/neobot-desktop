//go:build linux

package launcher

// Linux 开机自启走 XDG Autostart：在 $XDG_CONFIG_HOME/autostart 下维护一个
// .desktop 文件，登录桌面时由会话管理器启动。只写当前用户，不需要 root。

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const autostartFileName = "neobot-desktop.desktop"

// setAutostartEntry 按 enabled 写入或删除 XDG 自启项。
func setAutostartEntry(enabled bool) error {
	path, err := autostartPath()
	if err != nil {
		return err
	}
	if !enabled {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return errors.New("无法定位启动器所在位置，暂时不能设置开机自启")
	}
	if strings.ContainsAny(exe, "\r\n\x00") {
		return errors.New("启动器路径包含无法写入自启项的字符")
	}
	content := "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=NeoBot Desktop\n" +
		"Comment=NeoBot 桌面启动器\n" +
		"Exec=" + desktopQuote(exe) + " --startup\n" +
		"Terminal=false\n" +
		"X-GNOME-Autostart-enabled=true\n"
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".neobot-autostart-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.WriteString(content); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temp.Name(), 0644); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

// autostartPath 返回当前用户的自启文件路径。os.UserConfigDir 已经实现了
// XDG_CONFIG_HOME（绝对路径）与 ~/.config 的回退。
func autostartPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "autostart", autostartFileName), nil
}

// desktopQuote 按 Desktop Entry 规范给单个参数加引号：反斜杠、双引号、反引号
// 和美元符号需要转义；% 是字段码前缀，必须写成 %%。
func desktopQuote(value string) string {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"`", "\\`",
		`$`, `\$`,
		`%`, `%%`,
	).Replace(value)
	return `"` + escaped + `"`
}

// autostartErrorHint 给 SetAutostart 的错误补一句平台相关的排查建议。
func autostartErrorHint() string {
	return "；请检查 ~/.config/autostart 目录权限后重试"
}
