//go:build linux

package launcher

// Linux 高级终端：找到桌面环境里的终端模拟器，在目标版本的数据目录里打开
// 一个已经进入 venv 的交互式 shell。默认使用 bash；zsh/fish 的提示符和启动
// 参数可以在 shellArgs 里继续扩展。

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// openTerminalWindow 用系统终端打开一个新窗口。终端脱离启动器的会话，
// 这样启动器退出后终端仍可用。
func openTerminalWindow(dir string, env []string) error {
	path, args, err := findTerminal(dir)
	if err != nil {
		return err
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return startDetached(cmd)
}

// findTerminal 按「用户指定 → XDG 规范 → 常见终端」的顺序查找。返回的
// 参数已经包含工作目录和要执行的 shell。
func findTerminal(dir string) (string, []string, error) {
	shell, shellArgs := loginShell()
	if value := strings.TrimSpace(os.Getenv("TERMINAL")); value != "" {
		fields := strings.Fields(value)
		if path, err := exec.LookPath(fields[0]); err == nil {
			args := append([]string{}, fields[1:]...)
			args = append(args, terminalArgs(path, dir, shell, shellArgs)...)
			return path, args, nil
		}
	}
	for _, name := range []string{
		"xdg-terminal-exec",
		"gnome-terminal",
		"kgx", // GNOME Console（gnome-console 包）
		"konsole",
		"xfce4-terminal",
		"kitty",
		"alacritty",
		"wezterm",
		"foot",
		"xterm",
	} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		return path, terminalArgs(path, dir, shell, shellArgs), nil
	}
	return "", nil, errors.New("没有找到可用的终端模拟器，请安装 gnome-terminal、kgx（GNOME Console）、konsole 或 xterm，或设置 $TERMINAL")
}

// terminalArgs 按终端模拟器拼参数。不同终端的工作目录和命令参数并不统一。
func terminalArgs(path, dir, shell string, shellArgs []string) []string {
	command := append([]string{shell}, shellArgs...)
	switch filepath.Base(path) {
	case "xdg-terminal-exec":
		return append([]string{"--dir=" + dir}, command...)
	case "gnome-terminal":
		return append([]string{"--working-directory=" + dir, "--"}, command...)
	case "kgx":
		// GNOME Console 接受 -e COMMAND [ARGUMENT...]，也支持 --working-directory。
		return append([]string{"--working-directory=" + dir, "-e"}, command...)
	case "konsole":
		return append([]string{"--workdir", dir, "-e"}, command...)
	case "xfce4-terminal":
		return append([]string{"--working-directory=" + dir, "-x"}, command...)
	case "kitty":
		return append([]string{"--directory", dir}, command...)
	case "alacritty":
		return append([]string{"--working-directory", dir, "-e"}, command...)
	case "wezterm":
		return append([]string{"start", "--cwd", dir, "--"}, command...)
	case "foot":
		return append([]string{"--working-directory=" + dir}, command...)
	default:
		// xterm、urxvt 以及 $TERMINAL 指到的未知终端：用最通用的 -e。
		return append([]string{"-e"}, command...)
	}
}

// loginShell 默认优先 bash；找不到时回退到 $SHELL，最后 /bin/sh。
// 目前 bash/zsh/fish 都用 -i 启动；以后需要按 shell 加 --rcfile 等参数时
// 只改 shellArgs 即可。
func loginShell() (string, []string) {
	if path, err := exec.LookPath("bash"); err == nil {
		return path, shellArgs(path)
	}
	if shell := os.Getenv("SHELL"); filepath.IsAbs(shell) {
		if info, err := os.Stat(shell); err == nil && !info.IsDir() {
			return shell, shellArgs(shell)
		}
	}
	return "/bin/sh", shellArgs("/bin/sh")
}

func shellArgs(string) []string {
	// bash/zsh/fish 都用 -i 进入交互模式；以后需要加 --rcfile 等参数时在这里
	// 按 shell 分支即可。
	return []string{"-i"}
}

// terminalShellEnv 返回 Linux shell 的提示符，并在没有 TERM 时补一个可用值。
// fish 会忽略 PS1/PROMPT，但不会因此报错。
func terminalShellEnv(version string) []string {
	prompt := "[NeoBot " + version + "] \\w \\$ "
	env := []string{"PS1=" + prompt, "PROMPT=" + prompt}
	if os.Getenv("TERM") == "" {
		env = append(env, "TERM=xterm-256color")
	}
	return env
}

// uvShim 生成 Linux 的 uv 包装脚本：用脚本所在目录定位固定版本的 uv。
func uvShim(exe string) (string, []byte, os.FileMode, error) {
	content := "#!/bin/sh\nexec \"$(dirname \"$0\")/" + filepath.Base(exe) + "\" \"$@\"\n"
	return "uv", []byte(content), 0755, nil
}
