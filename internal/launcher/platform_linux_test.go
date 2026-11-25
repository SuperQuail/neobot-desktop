//go:build linux

package launcher

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestLockInstanceRejectsSecondHolder 验证同一个数据目录只能被一个启动器锁定，
// 释放后可以重新获取（进程退出时内核也会自动释放）。
func TestLockInstanceRejectsSecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop.lock")
	release, err := lockInstance(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockInstance(path); err == nil || !strings.Contains(err.Error(), "已经在运行") {
		t.Fatalf("second lock = %v; want the already-running error", err)
	}
	release()
	again, err := lockInstance(path)
	if err != nil {
		t.Fatalf("released lock could not be taken again: %v", err)
	}
	again()
}

// TestTrackProcessTreeKillsGroup 验证 Linux 用进程组替代 Job Object：
// 组内所有进程都能被判为「自己启动的」，强制停止会把整组收掉。
func TestTrackProcessTreeKillsGroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & wait")
	prepareLaunch(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	closeTree, ownsPID, err := trackProcessTree(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	if !ownsPID(cmd.Process.Pid) {
		t.Fatal("the launched process was not recognized as owned")
	}
	if ownsPID(os.Getpid()) {
		t.Fatal("the launcher itself was reported as owned")
	}
	closeTree()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process group was not terminated")
	}
}

func TestAppDataUsesXDGDataHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	got, err := AppData()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "NeoBotDesktop"); got != want {
		t.Fatalf("AppData = %q; want %q", got, want)
	}
}

func TestFreeDiskSpaceReportsAvailableBytes(t *testing.T) {
	free, err := freeDiskSpace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if free <= 0 {
		t.Fatalf("free disk space = %d; want a positive number", free)
	}
}

// TestSamePathIsCaseSensitive 验证本机控制接口的 data_dir 校验不会把大小写不同的
// 两个目录当成同一个（Windows 上则相反，见 platform_windows.go）。
func TestSamePathIsCaseSensitive(t *testing.T) {
	if samePath("/tmp/neobot", "/tmp/NeoBot") {
		t.Fatal("case-different paths were treated as the same")
	}
	if !samePath("/tmp/./neobot", "/tmp/neobot") {
		t.Fatal("cleaned paths were not treated as the same")
	}
}

func TestPrepareLaunchSetsDeathSignal(t *testing.T) {
	cmd := exec.Command("true")
	prepareLaunch(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("launch attrs = %+v; want Setpgid and Pdeathsig SIGKILL", cmd.SysProcAttr)
	}
}

// TestPdeathsigKillsChildWhenLauncherDies 用一个测试二进制子进程模拟启动器：
// 它启动 sleep 后挂起，父测试 SIGKILL 它，sleep 必须随 Pdeathsig 一起消失。
func TestPdeathsigKillsChildWhenLauncherDies(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	helper := exec.Command(os.Args[0], "-test.run=^TestPdeathsigHelper$")
	helper.Env = append(os.Environ(),
		"NEOBOT_PDEATHSIG_HELPER=1",
		"NEOBOT_PDEATHSIG_PID="+pidFile,
	)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	var childPID int
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if childPID > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			_ = helper.Process.Kill()
			_ = helper.Wait()
			t.Fatalf("helper did not publish a child pid: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 等一小段时间再确认子进程仍然存活：Pdeathsig 依赖创建子进程的线程，
	// 如果那个线程被 Go 运行时提前结束，这里会暴露出来。
	time.Sleep(300 * time.Millisecond)
	if !linuxProcessAlive(childPID) {
		_ = helper.Process.Kill()
		_ = helper.Wait()
		t.Fatalf("child %d died before the launcher helper", childPID)
	}
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	deadline = time.Now().Add(5 * time.Second)
	for {
		if !linuxProcessAlive(childPID) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
			t.Fatalf("child %d survived the launcher SIGKILL", childPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPdeathsigHelper 是上面的被启动器模拟进程；正常测试运行时直接返回。
func TestPdeathsigHelper(t *testing.T) {
	if os.Getenv("NEOBOT_PDEATHSIG_HELPER") != "1" {
		return
	}
	pidFile := os.Getenv("NEOBOT_PDEATHSIG_PID")
	child := exec.Command("sleep", "30")
	prepareLaunch(child)
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
		os.Exit(3)
	}
	// 用定时器阻塞，避免 Go 运行时把 select{} 判定为死锁后提前退出。
	time.Sleep(10 * time.Minute)
}

func linuxProcessAlive(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return false
	}
	fields := bytes.Fields(data[end+1:])
	return len(fields) > 0 && string(fields[0]) != "Z"
}
