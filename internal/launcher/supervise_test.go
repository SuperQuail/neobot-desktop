package launcher

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newSupervisorManager 造一个不需要完整 Open 的 Manager：常驻服务的状态只在
// m.state 和内存字段里，落盘的 settings.json 也只会写到 t.TempDir() 下。
func newSupervisorManager(root string) *Manager {
	return &Manager{root: root, state: diskState{}}
}

// supervisorStubDelays 把退避时间换成测试值，测试结束恢复，避免定时器落到后面的用例里。
func supervisorStubDelays(t *testing.T, delay time.Duration) {
	t.Helper()
	before := restartDelays
	restartDelays = [crashRetryLimit]time.Duration{delay, delay, delay}
	t.Cleanup(func() { restartDelays = before })
}

// supervisorReplaceAutostart 替换开机自启的实现，测试结束后恢复，避免真的写注册表。
func supervisorReplaceAutostart(t *testing.T, write func(bool) error) {
	t.Helper()
	before := setAutostart
	setAutostart = write
	t.Cleanup(func() { setAutostart = before })
}

func supervisorSettings(t *testing.T, root string) diskState {
	t.Helper()
	var saved diskState
	if err := readJSON(filepath.Join(root, "settings.json"), &saved); err != nil {
		t.Fatalf("读取启动器设置失败：%v", err)
	}
	return saved
}

func TestSupervisorFlagsPersist(t *testing.T) {
	root := t.TempDir()
	m := newSupervisorManager(root)
	var notified atomic.Int32
	m.changed = func() { notified.Add(1) }
	if err := m.SetAutoRestart(true); err != nil {
		t.Fatalf("开启崩溃后自动重启失败：%v", err)
	}
	if err := m.SetCloseToTray(true); err != nil {
		t.Fatalf("开启关闭窗口收进托盘失败：%v", err)
	}
	status := m.Snapshot().Supervisor
	if !status.AutoRestart || !status.CloseToTray {
		t.Fatalf("内存状态没有更新：%+v", status)
	}
	saved := supervisorSettings(t, root)
	if !saved.AutoRestart || !saved.CloseToTray {
		t.Fatalf("设置没有落盘：%+v", saved)
	}
	if notified.Load() < 2 {
		t.Fatalf("每次修改都要通知界面，实际只通知了 %d 次", notified.Load())
	}
	if err := m.SetAutoRestart(false); err != nil {
		t.Fatalf("关闭崩溃后自动重启失败：%v", err)
	}
	if supervisorSettings(t, root).AutoRestart {
		t.Fatal("关闭后设置应落盘为 false")
	}
}

func TestSupervisorFlagsRollBackWhenSaveFails(t *testing.T) {
	root := t.TempDir()
	// 用同名目录占住 settings.json，让写盘必然失败。
	if err := os.Mkdir(filepath.Join(root, "settings.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	m := newSupervisorManager(root)
	if err := m.SetAutoRestart(true); err == nil {
		t.Fatal("写盘失败时应该返回错误，而不是假装保存成功")
	}
	if err := m.SetCloseToTray(true); err == nil {
		t.Fatal("写盘失败时应该返回错误，而不是假装保存成功")
	}
	if status := m.Snapshot().Supervisor; status.AutoRestart || status.CloseToTray {
		t.Fatalf("写盘失败必须把开关回滚：%+v", status)
	}
}

func TestSetAutostartWritesRegistryAndRollsBack(t *testing.T) {
	root := t.TempDir()
	m := newSupervisorManager(root)
	var calls []bool
	record := func(enabled bool) error {
		calls = append(calls, enabled)
		return nil
	}
	supervisorReplaceAutostart(t, record)

	if err := m.SetAutostart(true); err != nil {
		t.Fatalf("开启开机自启失败：%v", err)
	}
	if status := m.Snapshot().Supervisor; !status.Autostart || !supervisorSettings(t, root).Autostart {
		t.Fatal("开机自启状态没有保存")
	}
	if len(calls) != 1 || !calls[0] {
		t.Fatalf("应该写一次启动项，实际 %v", calls)
	}

	// 注册表写不进去时，开关必须保持原样。
	setAutostart = func(bool) error { return errors.New("拒绝访问") }
	err := m.SetAutostart(false)
	if err == nil {
		t.Fatal("写注册表失败时应该返回错误")
	}
	if hint := autostartErrorHint(); hint != "" && !strings.Contains(err.Error(), hint) {
		t.Fatalf("错误提示要说清下一步：%q", err)
	}
	if !m.Snapshot().Supervisor.Autostart {
		t.Fatal("注册表写失败时不应改动开关")
	}

	// 落盘失败时开关回滚，启动项也要恢复原样。
	calls = nil
	setAutostart = record
	settings := filepath.Join(root, "settings.json")
	if err := os.Remove(settings); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(settings, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.SetAutostart(false); err == nil {
		t.Fatal("写盘失败时应该返回错误")
	}
	if !m.Snapshot().Supervisor.Autostart {
		t.Fatal("写盘失败时必须回滚开关")
	}
	if len(calls) != 2 || calls[0] || !calls[1] {
		t.Fatalf("写盘失败后应把启动项恢复成开启：%v", calls)
	}
}

func TestAutostartCommandQuotesExecutableAndStartupFlag(t *testing.T) {
	command, err := autostartCommand()
	if err != nil {
		t.Fatalf("生成开机启动命令失败：%v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(command, "\""+exe+"\"") {
		t.Fatalf("可执行文件路径必须带引号（否则含空格的目录会启动失败）：%q", command)
	}
	if !strings.HasSuffix(command, "--startup") {
		t.Fatalf("开机启动必须带 --startup，才能静默驻留托盘：%q", command)
	}
}

// supervisorExitError 跑一个必然以非零码退出的进程，拿到真实的 *exec.ExitError。
func supervisorExitError(t *testing.T) error {
	t.Helper()
	// 未知参数会让测试二进制在解析 flag 时直接退出，不会递归跑测试。
	err := exec.Command(os.Args[0], "-test.run=^$", "-supervise-test-unknown-flag").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Skipf("这个环境没能造出进程退出错误：%v", err)
	}
	return err
}

func TestAfterExitRecordsCrashWithExitCode(t *testing.T) {
	m := newSupervisorManager(t.TempDir())
	err := supervisorExitError(t)
	m.afterExit(nil, err)
	status := m.Snapshot().Supervisor
	if status.CrashCount != 1 {
		t.Fatalf("崩溃次数 = %d；期望 1", status.CrashCount)
	}
	if !strings.Contains(status.LastCrash, "退出码") {
		t.Fatalf("崩溃原因要写清退出码：%q", status.LastCrash)
	}
	if !strings.Contains(status.LastCrash, "日志") {
		t.Fatalf("崩溃原因要告诉用户下一步做什么：%q", status.LastCrash)
	}
}

func TestAfterExitIgnoresNormalExitAndStop(t *testing.T) {
	normal := newSupervisorManager(t.TempDir())
	normal.crashCount = 2
	normal.afterExit(nil, nil)
	if status := normal.Snapshot().Supervisor; status.CrashCount != 0 {
		t.Fatalf("正常退出后崩溃次数应清零，实际 %d", status.CrashCount)
	}

	closing := newSupervisorManager(t.TempDir())
	closing.closing = true
	closing.afterExit(nil, errors.New("boom"))
	if closing.Snapshot().Supervisor.CrashCount != 0 {
		t.Fatal("启动器正在关闭时不应记崩溃")
	}

	// 「用户主动停止」的信号随进程对象传递：退出清理会把 m.stopping 复位，
	// 所以 afterExit 只能看 p.expected。
	stopped := newSupervisorManager(t.TempDir())
	stopped.afterExit(&botProcess{expected: true}, errors.New("boom"))
	if stopped.Snapshot().Supervisor.CrashCount != 0 {
		t.Fatal("用户主动停止时不应记崩溃")
	}
}

func TestAfterExitResetsCountAfterQuietPeriod(t *testing.T) {
	m := newSupervisorManager(t.TempDir())
	t.Cleanup(func() { crashTimes.Delete(m) })
	m.crashCount = 2
	// 上次崩溃已经是三分钟前，不算连续崩溃，应该从 1 重新数。
	crashTimes.Store(m, time.Now().Add(-3*time.Minute))
	m.afterExit(nil, errors.New("boom"))
	if got := m.Snapshot().Supervisor.CrashCount; got != 1 {
		t.Fatalf("隔了很久的崩溃应重新计数，实际 %d", got)
	}
}

func TestCrashDecision(t *testing.T) {
	cases := []struct {
		name        string
		autoRestart bool
		job         Job
		preparing   bool
		crashCount  int
		wantRestart bool
		wantDelay   time.Duration
		wantPause   string
	}{
		{name: "未开启自动重启", crashCount: 1},
		{name: "有安装任务在跑", autoRestart: true, job: Job{State: "running"}, crashCount: 1},
		{name: "正在准备启动", autoRestart: true, preparing: true, crashCount: 1},
		{name: "第一次崩溃等一秒", autoRestart: true, crashCount: 1, wantRestart: true, wantDelay: time.Second},
		{name: "第二次崩溃等五秒", autoRestart: true, crashCount: 2, wantRestart: true, wantDelay: 5 * time.Second},
		{name: "第三次崩溃等三十秒", autoRestart: true, crashCount: 3, wantRestart: true, wantDelay: 30 * time.Second},
		{name: "退避用完则暂停", autoRestart: true, crashCount: 4, wantPause: crashPauseError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newSupervisorManager(t.TempDir())
			m.state.AutoRestart = c.autoRestart
			m.job = c.job
			m.preparing = c.preparing
			m.crashCount = c.crashCount
			delay, restart, pause := m.crashDecision()
			if restart != c.wantRestart || delay != c.wantDelay || pause != c.wantPause {
				t.Fatalf("crashDecision() = (%v, %v, %q)；期望 (%v, %v, %q)",
					delay, restart, pause, c.wantDelay, c.wantRestart, c.wantPause)
			}
		})
	}
}

func TestAfterExitPausesAutoRestartAfterRepeatedCrashes(t *testing.T) {
	supervisorStubDelays(t, time.Hour) // 定时器不会在测试期间真的跑起来
	m := newSupervisorManager(t.TempDir())
	m.state.AutoRestart = true
	t.Cleanup(func() { crashTimes.Delete(m) })
	for i := 1; i <= crashRetryLimit; i++ {
		m.afterExit(nil, errors.New("boom"))
		status := m.Snapshot()
		if status.Supervisor.CrashCount != i {
			t.Fatalf("第 %d 次崩溃后计数 = %d", i, status.Supervisor.CrashCount)
		}
		if status.RuntimeError != "" {
			t.Fatalf("第 %d 次崩溃后还不该暂停自动重启：%q", i, status.RuntimeError)
		}
	}
	m.afterExit(nil, errors.New("boom"))
	status := m.Snapshot()
	if status.Supervisor.CrashCount != crashRetryLimit+1 {
		t.Fatalf("崩溃计数 = %d；期望 %d", status.Supervisor.CrashCount, crashRetryLimit+1)
	}
	if status.RuntimeError != crashPauseError {
		t.Fatalf("暂停提示 = %q；期望 %q", status.RuntimeError, crashPauseError)
	}
}

func TestAutoRestartTriesToStartNeoBot(t *testing.T) {
	supervisorStubDelays(t, 5*time.Millisecond)
	m := newSupervisorManager(t.TempDir())
	m.state.AutoRestart = true
	t.Cleanup(func() { crashTimes.Delete(m) })
	m.afterExit(nil, errors.New("boom"))
	// 没有安装任何版本，Start 必然失败；失败要变成看得懂的提示，而不是静默什么都不做。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		message := m.runtimeError
		m.mu.Unlock()
		if strings.Contains(message, "自动重启失败") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("自动重启没有执行：%q", m.Snapshot().RuntimeError)
}

func TestAutoRestartSkipsWhileJobRunning(t *testing.T) {
	supervisorStubDelays(t, 5*time.Millisecond)
	m := newSupervisorManager(t.TempDir())
	m.state.AutoRestart = true
	m.job = Job{State: "running"}
	t.Cleanup(func() { crashTimes.Delete(m) })
	m.afterExit(nil, errors.New("boom"))
	time.Sleep(80 * time.Millisecond)
	if status := m.Snapshot(); status.RuntimeError != "" || status.Starting {
		t.Fatalf("有任务在跑时不应自动重启：%+v", status)
	}
}
