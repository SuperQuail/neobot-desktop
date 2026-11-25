//go:build !windows && !linux

package launcher

import (
	"errors"
	"os"
)

func openTerminalWindow(string, []string) error {
	return errors.New("当前系统暂不支持高级终端")
}

func terminalShellEnv(string) []string { return nil }

func uvShim(string) (string, []byte, os.FileMode, error) {
	return "", nil, 0, errors.New("当前系统暂不支持高级终端")
}
