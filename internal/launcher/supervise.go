package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// SupervisorStatus 是常驻服务的状态。
type SupervisorStatus struct {
	AutoRestart bool   `json:"auto_restart"`
	Autostart   bool   `json:"autostart"`
	CloseToTray bool   `json:"close_to_tray"`
	CrashCount  int    `json:"crash_count"`
	LastCrash   string `json:"last_crash"`
	TrayReady   bool   `json:"tray_ready"`
}

const (
	// autostartValueName 是 HKCU Run 项里的值名；关闭自启时按同一个名字删除。
	autostartValueName = "NeoBot Desktop"
	// crashPauseError 是连续崩溃后写进界面提示的文案。
	crashPauseError = "NeoBot 连续异常退出，已暂停自动重启"
	// crashLoopWindow 是「连续快速崩溃」的判定窗口：两次崩溃间隔超过它，
	// 就认为上一次之前已经稳定运行过，重新开始计数。
	crashLoopWindow = 2 * time.Minute
	// crashRetryLimit 是一轮崩溃里最多自动重启的次数，超过后暂停自动重启。
	crashRetryLimit = 3
)

// restartDelays 是第 1/2/3 次自动重启前的等待时间，逐次退避。
var restartDelays = [crashRetryLimit]time.Duration{time.Second, 5 * time.Second, 30 * time.Second}

// setAutostart 写入或清理开机自启项，按平台实现分别在 supervise_windows.go 与
// supervise_other.go 里；测试替换它就不用碰真实注册表。
var setAutostart = setAutostartEntry

// crashTimes 记录每个 Manager 最近一次崩溃的时间。崩溃时间只在内存里用，
// 不属于要持久化的设置，Manager 结构里没有对应字段，所以单独存一张表。
var crashTimes sync.Map // map[*Manager]time.Time

// SetAutoRestart 开启/关闭 NeoBot 意外退出后的自动重启。
// 重新打开等于用户要求再试一次，所以顺手清掉上一轮的崩溃计数和「已暂停」提示。
func (m *Manager) SetAutoRestart(enabled bool) error {
	m.mu.Lock()
	before := m.state.AutoRestart
	m.state.AutoRestart = enabled
	err := m.persist()
	if err != nil {
		m.state.AutoRestart = before
		err = errors.New("无法保存自动重启设置，请检查磁盘空间后重试")
	} else {
		if m.runtimeError == crashPauseError {
			m.runtimeError = ""
		}
		if enabled {
			m.crashCount = 0
			crashTimes.Delete(m)
		}
	}
	m.mu.Unlock()
	m.notify()
	return err
}

// SetCloseToTray 开启/关闭「关闭窗口 = 收进托盘」。关闭时关闭窗口即退出。
func (m *Manager) SetCloseToTray(enabled bool) error {
	m.mu.Lock()
	before := m.state.CloseToTray
	m.state.CloseToTray = enabled
	// 记下「用户自己设置过」，否则下次启动又会按默认值改回收进托盘。
	m.state.CloseToTraySet = true
	err := m.persist()
	if err != nil {
		m.state.CloseToTray = before
		err = errors.New("无法保存托盘设置，请检查磁盘空间后重试")
	}
	m.mu.Unlock()
	m.notify()
	return err
}

// SetAutostart 开启/关闭随 Windows 登录自动启动启动器（HKCU Run 项）。
// 先写系统启动项再落盘：注册表写不进去时，界面上的开关不该显示成已打开。
func (m *Manager) SetAutostart(enabled bool) error {
	m.mu.Lock()
	before := m.state.Autostart
	m.mu.Unlock()
	if err := setAutostart(enabled); err != nil {
		return fmt.Errorf("无法更新开机自启设置（%w）%s", err, autostartErrorHint())
	}
	m.mu.Lock()
	m.state.Autostart = enabled
	err := m.persist()
	if err != nil {
		m.state.Autostart = before
	}
	m.mu.Unlock()
	if err != nil {
		// 设置文件没写成，把启动项恢复原样，避免系统状态和界面不一致。
		_ = setAutostart(before)
		m.notify()
		return errors.New("无法保存开机自启设置，请检查磁盘空间后重试")
	}
	m.notify()
	return nil
}

// afterExit 在 NeoBot 进程退出后被调用，负责自动重启决策。
// 调用时 m.mu 未被持有；函数内部需要自己加锁。
func (m *Manager) afterExit(p *botProcess, err error) {
	m.mu.Lock()
	// 正常退出、启动器已关闭、用户点了停止：都不算崩溃，也不用重启。
	if err == nil || m.closing || (p != nil && p.expected) {
		m.crashCount = 0
		// 正常退出同样要把上一次的崩溃文案清掉，否则界面会一直挂着
		// 「最近有一次异常退出」，而这一轮其实是用户自己停的。
		m.lastCrash = ""
		if isCrashMessage(m.runtimeError) {
			m.runtimeError = ""
		}
		crashTimes.Delete(m)
		m.mu.Unlock()
		return
	}
	// 有安装/修复任务或正在启动时，进程退出多半是被任务流程牵连的，不计入崩溃次数：
	// 否则几次任务期退出就会累加到「连续异常退出」，把真正的崩溃重试也一起暂停。
	if m.job.State == "running" || m.preparing {
		m.lastCrash = "NeoBot 在任务进行中退出（" + exitCause(err) + "）"
		m.mu.Unlock()
		m.notify()
		return
	}
	now := time.Now()
	if last, ok := crashTimes.Load(m); ok && now.Sub(last.(time.Time)) > crashLoopWindow {
		// 上次崩溃已经过去很久，说明不是崩溃循环，重新开始计数。
		m.crashCount = 0
	}
	m.crashCount++
	crashTimes.Store(m, now)
	cause := exitCause(err)
	m.lastCrash = "NeoBot 意外退出（" + cause + "）；可查看日志排查原因"
	delay, restart, pause := m.crashDecision()
	if pause != "" {
		m.runtimeError = pause
	}
	m.mu.Unlock()

	label := ""
	if p != nil && p.version != "" {
		label = " " + p.version
	}
	m.logMessage("NeoBot" + label + " 异常退出（" + cause + "）。")
	if pause != "" {
		m.logMessage(pause + "；请查看日志排查原因后手动启动。")
	}
	if restart {
		m.logMessage(fmt.Sprintf("将在 %d 秒后自动重启。", int(delay/time.Second)))
		time.AfterFunc(delay, m.restartAfterCrash)
	}
	m.notify()
}

// crashDecision 决定这次崩溃后要不要自动重启、等多久、要不要暂停。调用方持有 m.mu。
// 返回的 pause 非空表示不再重启，并把它写进界面提示。
func (m *Manager) crashDecision() (delay time.Duration, restart bool, pause string) {
	switch {
	case !m.state.AutoRestart:
		// 用户没开自动重启：只记录崩溃，交给界面提示。
		return 0, false, ""
	case m.job.State == "running" || m.preparing || m.bot != nil:
		// 有安装任务或新进程在跑，自动重启会和它们抢同一个版本目录，交给用户手动启动。
		return 0, false, ""
	case m.crashCount > crashRetryLimit:
		// 退避重试已经用完还在崩，先停下，免得反复拉起、每次都把数据目录写坏。
		return 0, false, crashPauseError
	default:
		return restartDelays[m.crashCount-1], true, ""
	}
}

// restartAfterCrash 是自动重启定时器的回调。它跑在独立的 goroutine 里，
// 这里不能持锁调用 Start（Start 自己会加锁）。
func (m *Manager) restartAfterCrash() {
	m.mu.Lock()
	ready := !m.closing && !m.stopping && m.bot == nil && !m.preparing &&
		m.job.State != "running" && m.state.AutoRestart
	m.mu.Unlock()
	if !ready {
		return
	}
	if err := m.Start(false); err != nil {
		m.mu.Lock()
		if m.closing {
			// 启动器正在关闭：这次失败没有意义，也不要再往界面上写提示。
			m.mu.Unlock()
			return
		}
		m.runtimeError = "自动重启失败（" + err.Error() + "），请手动启动"
		message := m.runtimeError
		m.mu.Unlock()
		m.logMessage(message)
		m.notify()
		return
	}
	m.mu.Lock()
	// 只清崩溃留下的提示：Start 之后启动流程可能会写进更具体的错误，那些要保留。
	if isCrashMessage(m.runtimeError) {
		m.runtimeError = ""
	}
	m.mu.Unlock()
	m.notify()
}

// exitCause 把进程退出的错误翻译成一句简短的中文原因。
func exitCause(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
		if code := exitErr.ProcessState.ExitCode(); code >= 0 {
			return fmt.Sprintf("退出码 %d", code)
		}
		return "被系统强制结束"
	}
	if err != nil {
		return err.Error()
	}
	return "原因未知"
}

// isCrashMessage 判断界面提示是不是崩溃处理写下的；自动重启成功后只清理这一类。
func isCrashMessage(message string) bool {
	return strings.Contains(message, "意外退出") || strings.Contains(message, crashPauseError)
}

// autostartCommand 是写进 Run 项的命令行：带引号的可执行文件路径加 --startup。
// main.go 认这个参数后不弹主窗口，直接驻留托盘。路径加引号是为了兼容含空格的安装目录。
func autostartCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", errors.New("无法定位启动器所在位置，暂时不能设置开机自启")
	}
	if strings.ContainsAny(exe, `"`) {
		return "", errors.New("启动器路径中含有引号，无法设置开机自启，请换个目录重新安装")
	}
	return `"` + exe + `" --startup`, nil
}
