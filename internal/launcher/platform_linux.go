//go:build linux

package launcher

// Linux 平台实现。Windows 那套 Job Object、注册表、GetDiskFreeSpaceEx 在这里
// 分别换成进程组、XDG 目录和 statfs；业务层的调用方式保持不变。

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// freeDiskSpace 返回给定路径所在文件系统对当前用户可用的剩余字节数。
func freeDiskSpace(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * stat.Bsize, nil
}

func hideWindow(*exec.Cmd) {}

// replaceFile 用 rename 覆盖目标文件；同一文件系统内 rename 是原子的。
func replaceFile(source, target string) error { return os.Rename(source, target) }

// lockInstance 用 flock 保证同一个数据目录只被一个启动器使用。进程退出（包括
// 被强杀）时内核会自动释放锁，不需要额外的陈旧锁清理。
func lockInstance(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, &os.PathError{Op: "锁定", Path: path, Err: err}
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("NeoBot Desktop 已经在运行")
		}
		return nil, &os.PathError{Op: "锁定", Path: path, Err: err}
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			_ = file.Close()
		})
	}
	return release, nil
}

// prepareLaunch 让 NeoBot 成为独立进程组的组长，并在启动器进程死亡时由内核
// 给直接子进程发 SIGKILL。Setpgid 负责正常 Stop 时收掉整组，Pdeathsig 补上
// 启动器崩溃/被强杀时的兜底（对应 Windows 的 Job Object）。
//
// Pdeathsig 只覆盖直接子进程；如果 NeoBot 再启动 setsid 的孙进程，仍要依赖
// 正常停止流程。不要在这里 LockOSThread：锁住的 goroutine 退出会结束线程，
// 反而会提前触发 Pdeathsig。
func prepareLaunch(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
}

// trackProcessTree 接管一个刚启动的进程组：closeTree 杀掉整组，ownsPID 判断某个
// PID 是否仍在组里（本机控制接口靠它确认 control.json 属于自己启动的实例）。
func trackProcessTree(pid int) (func(), func(int) bool, error) {
	if pid <= 0 {
		return nil, nil, errors.New("无效的进程号")
	}
	group, err := processGroup(pid)
	if err != nil {
		return nil, nil, err
	}
	if group != pid {
		return nil, nil, errors.New("NeoBot 没有成为独立进程组的组长")
	}
	var once sync.Once
	closeTree := func() {
		once.Do(func() {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		})
	}
	ownsPID := func(candidate int) bool {
		if candidate <= 0 {
			return false
		}
		group, err := processGroup(candidate)
		return err == nil && group == pid
	}
	return closeTree, ownsPID, nil
}

// processGroup 从 /proc/<pid>/stat 读取进程组号。comm 字段可能包含空格和括号，
// 所以从最后一个 ')' 之后开始切字段：state、ppid、pgrp 依次是第 1、2、3 个。
func processGroup(pid int) (int, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return 0, errors.New("无法解析进程状态")
	}
	fields := bytes.Fields(data[end+1:])
	if len(fields) < 3 {
		return 0, errors.New("无法解析进程状态")
	}
	group, err := strconv.Atoi(string(fields[2]))
	if err != nil {
		return 0, errors.New("无法解析进程状态")
	}
	return group, nil
}

// makeExecutable 给解压出来的 uv 可执行位。Windows 上不需要这一步。
func makeExecutable(path string) error { return os.Chmod(path, 0755) }

// appDataBase 返回启动器数据根目录的上级目录。Linux 按 XDG 规范使用
// $XDG_DATA_HOME（默认 ~/.local/share），而不是会定期清理的 ~/.cache。
func appDataBase() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dir) {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share"), nil
}

// startDetached 启动一个不需要等待的进程，并在后台 Wait 回收，避免留下僵尸。
func startDetached(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// OpenPath 用桌面默认程序打开目录。
func OpenPath(path string) error {
	if _, err := exec.LookPath("xdg-open"); err == nil {
		return startDetached(exec.Command("xdg-open", path))
	}
	if _, err := exec.LookPath("gio"); err == nil {
		return startDetached(exec.Command("gio", "open", path))
	}
	return errors.New("没有找到可用的文件管理器，请安装 xdg-utils")
}

// samePath 在 Linux 上按大小写敏感比较。Windows 的实现仍然忽略大小写。
func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

// terminalSupported 报告当前平台是否实现了高级终端。
func terminalSupported() bool { return true }
