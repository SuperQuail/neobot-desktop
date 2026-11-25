//go:build !windows && !linux

package main

// 其他非 Windows 平台目前没有托盘实现：这里的签名与 tray_windows.go 保持一致，
// 调用方不用区分平台。

func startTray(trayCommands) {}

func stopTray() {}

func updateTrayStatus(bool, string) {}

// trayAvailable 在其他平台固定为 false：beforeClose 因此不会把窗口藏起来，
// 关闭窗口就是真的退出。
func trayAvailable() bool { return false }

// trayInstalled 在其他平台永远是 false。
func trayInstalled() bool { return false }
