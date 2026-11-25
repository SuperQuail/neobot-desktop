//go:build !windows && !linux

package launcher

import "errors"

// setAutostartEntry 只在 Windows 与 Linux 上有实现；其他系统请在系统自带的
// 启动项设置里手动添加。
func setAutostartEntry(bool) error {
	return errors.New("当前系统不支持自动写入开机自启，请在系统的启动项设置里手动添加")
}

func autostartErrorHint() string { return "" }
