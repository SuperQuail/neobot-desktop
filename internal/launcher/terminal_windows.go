//go:build windows

package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// openTerminalWindow 用系统 cmd.exe 打开一个新控制台，并进入已经准备好的环境。
//
// 这里用 `cmd /c start` 而不是直接给 cmd.exe 设置 CREATE_NEW_CONSOLE：Wails
// 是 GUI 进程，os/exec 默认会把子进程的标准句柄接到 NUL；交给 start 创建
// 新控制台，Windows 会把标准句柄正确接到新窗口上。Windows 11 的「默认终端
// 应用」设置成 Windows Terminal 时，start 创建的控制台也会自动由它承载。
func openTerminalWindow(dir string, env []string) error {
	shell := comspec()
	cmd := exec.Command(shell, "/d", "/c", "start", "", shell, "/d", "/k", "chcp 65001>nul")
	cmd.Dir = dir
	cmd.Env = env
	// 不调用 hideWindow：终端窗口必须可见。
	return cmd.Run()
}

// comspec 返回系统自带的命令解释器。优先 ComSpec，其次 SystemRoot 下的
// cmd.exe；ComSpec 指向不存在的路径时也要能回退，避免完全打不开终端。
func comspec() string {
	if value := strings.Trim(strings.TrimSpace(os.Getenv("ComSpec")), `"`); value != "" && isFile(value) {
		return value
	}
	if root := os.Getenv("SystemRoot"); root != "" {
		if candidate := filepath.Join(root, "System32", "cmd.exe"); isFile(candidate) {
			return candidate
		}
	}
	return "cmd.exe"
}

// terminalShellEnv 返回 Windows 控制台需要的提示符与搜索行为。
func terminalShellEnv(version string) []string {
	return []string{
		// 数据目录里可能放着 .env、插件文件或用户脚本；不让 cmd 先搜索
		// 当前目录，避免同名程序遮蔽 venv/tools 里的 python 和 uv。
		"NoDefaultCurrentDirectoryInExePath=1",
		"PROMPT=[NeoBot " + version + "] $P$G",
	}
}

// uvShim 生成 Windows 的 uv.cmd 转发脚本。
func uvShim(exe string) (string, []byte, os.FileMode, error) {
	content := "@echo off\r\n\"%~dp0" + filepath.Base(exe) + "\" %*\r\n"
	return "uv.cmd", []byte(content), 0600, nil
}
