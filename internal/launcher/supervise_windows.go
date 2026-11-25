//go:build windows

package launcher

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// autostartKeyPath 是当前用户的开机启动项，卸载程序也会清理这里的同名值。
const autostartKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// setAutostartEntry 按 enabled 写入或删除 HKCU 下的开机自启项。
// 只写当前用户、不写 HKLM，因此不需要管理员权限；普通用户被安全软件拦住时
// 会返回错误，由 SetAutostart 翻译成界面提示。
func setAutostartEntry(enabled bool) error {
	if !enabled {
		return removeAutostartEntry()
	}
	command, err := autostartCommand()
	if err != nil {
		return err
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, autostartKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	return key.SetStringValue(autostartValueName, command)
}

// removeAutostartEntry 删除开机自启项；本来就没有这个值时视为成功。
func removeAutostartEntry() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, autostartKeyPath, registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	defer key.Close()
	if err := key.DeleteValue(autostartValueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// autostartErrorHint 给 SetAutostart 的错误补一句平台相关的排查建议。
func autostartErrorHint() string {
	return "；多为安全软件拦截了注册表写入，请允许后重试"
}
