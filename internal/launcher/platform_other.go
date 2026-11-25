//go:build !windows && !linux

package launcher

// 其他系统还没有实现安装与进程托管；保留签名让共享代码可以编译，调用时给出
// 明确的不支持提示。Linux 的实现见 platform_linux.go。

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func freeDiskSpace(string) (int64, error) {
	return 0, errors.New("暂不支持读取磁盘剩余空间")
}
func hideWindow(*exec.Cmd)                    {}
func replaceFile(source, target string) error { return os.Rename(source, target) }
func lockInstance(string) (func(), error)     { return nil, errors.New("当前系统暂不支持") }
func prepareLaunch(*exec.Cmd)                 {}
func trackProcessTree(int) (func(), func(int) bool, error) {
	return nil, nil, errors.New("当前系统暂不支持")
}
func makeExecutable(string) error { return nil }
func appDataBase() (string, error) {
	return os.UserCacheDir()
}
func OpenPath(string) error { return errors.New("当前系统暂不支持打开文件夹") }
func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
func terminalSupported() bool { return false }
