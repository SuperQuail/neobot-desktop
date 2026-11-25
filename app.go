package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/runtime"
	"neobot-desktop/internal/launcher"
)

type App struct {
	ctx         context.Context
	manager     *launcher.Manager
	initErr     error
	mu          sync.RWMutex
	startHidden bool
	quitting    bool
	closing     bool
}

func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) {
	launcher.SetCurrentVersion(appVersion())
	root, err := launcher.AppData()
	var manager *launcher.Manager
	if err == nil {
		manager, err = launcher.Open(root, func() {
			// 关闭流程里的事件会打到已经销毁的 WebView 上，直接丢掉。
			if a.isClosing() {
				return
			}
			runtime.EventsEmit(ctx, "launcher:changed")
			a.syncTray()
		})
	}
	a.mu.Lock()
	a.ctx, a.manager, a.initErr = ctx, manager, err
	hidden := a.startHidden
	a.mu.Unlock()
	if err != nil {
		return
	}
	// 首次配置向导要用 Snapshot.Recommended 决定是否显示「安装并启动推荐版本」，
	// 所以启动后必须跑一次更新检查（后端自带 1 小时节流）。
	go manager.CheckUpdate(false)
	// --startup（开机自启）时直接拉起上次选中的版本。必须在锁外、单独 goroutine 里做：
	// Start 会触发 changed 回调，回调里的 syncTray 又要读 a.mu，持锁同步调用会死锁。
	if hidden {
		go func() {
			if snapshot := manager.Snapshot(); snapshot.Selected != "" && snapshot.Running == "" {
				_ = manager.Start(false)
			}
		}()
	}
}

// windowContext 安全地取 Wails 上下文：startup 写、其它 goroutine 读，必须加锁。
func (a *App) windowContext() context.Context {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ctx
}

// isClosing 报告启动器是否已经在关闭流程里。
func (a *App) isClosing() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.closing
}

// domReady 在主窗口渲染完成后启动托盘。托盘不可用时整段逻辑自动跳过。
func (a *App) domReady(context.Context) {
	if !trayAvailable() {
		return
	}
	startTray(trayCommands{
		Show: func() {
			ctx := a.windowContext()
			if ctx == nil {
				return
			}
			runtime.WindowShow(ctx)
			runtime.WindowUnminimise(ctx)
		},
		Start: func() { a.background(func(m *launcher.Manager) error { return m.Start(false) }) },
		Stop:  func() { a.background(func(m *launcher.Manager) error { return m.Stop(false) }) },
		Dashboard: func() {
			_ = a.OpenDashboard()
		},
		OpenData: func() {
			_ = a.OpenFolder("data")
		},
		// 窗口打不开或最小化在托盘时，这里是唯一还能点到诊断的入口。
		Diagnose: func() {
			if ctx := a.windowContext(); ctx != nil {
				runtime.WindowShow(ctx)
				runtime.WindowUnminimise(ctx)
			}
			a.background(func(m *launcher.Manager) error { return m.RefreshHealth() })
		},
		Quit: a.Quit,
	})
	go func() {
		// 托盘在独立 goroutine 里初始化，这里短暂等待它真的挂上图标。
		for attempt := 0; attempt < 30; attempt++ {
			if trayInstalled() {
				if m, err := a.get(); err == nil {
					m.MarkTrayReady(true)
				}
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		// 托盘最终没起来（通知区域未就绪、被策略禁用等）：开机自启时窗口是隐藏的，
		// 这时必须把窗口放出来，否则用户既没有窗口也没有托盘，只能去任务管理器。
		if ctx := a.windowContext(); ctx != nil && a.startHidden {
			runtime.WindowShow(ctx)
		}
	}()
}

// background 把托盘菜单里的动作转成一次普通调用，失败只记录到运行错误。
func (a *App) background(work func(*launcher.Manager) error) {
	m, err := a.get()
	if err != nil {
		return
	}
	_ = work(m)
}

// beforeClose 决定关闭窗口时是收进托盘还是真的退出。
func (a *App) beforeClose(ctx context.Context) bool {
	a.mu.RLock()
	quitting, m := a.quitting, a.manager
	a.mu.RUnlock()
	// 必须用 trayInstalled：trayAvailable 在托盘初始化完成前就是 true，
	// 那种情况下把窗口藏起来会让应用彻底失联。
	if quitting || m == nil || !trayInstalled() {
		return false
	}
	if !m.Snapshot().Supervisor.CloseToTray {
		return false
	}
	runtime.WindowHide(ctx)
	return true
}

// secondInstanceLaunch 在用户重复打开启动器时把已有窗口拉到前台。
func (a *App) secondInstanceLaunch(options.SecondInstanceData) {
	a.mu.RLock()
	ctx := a.ctx
	a.mu.RUnlock()
	if ctx == nil {
		return
	}
	runtime.WindowShow(ctx)
	runtime.WindowUnminimise(ctx)
}

func (a *App) shutdown(_ context.Context) {
	stopTray()
	a.mu.Lock()
	a.closing = true
	m := a.manager
	a.manager = nil
	a.mu.Unlock()
	if m != nil {
		m.Close()
	}
}

// syncTray 把运行状态同步到托盘提示文本。
func (a *App) syncTray() {
	if !trayAvailable() {
		return
	}
	m, err := a.get()
	if err != nil {
		return
	}
	snapshot := m.Snapshot()
	running := snapshot.Running != ""
	label := "未运行"
	if running {
		label = "运行中 · " + snapshot.Running
	}
	updateTrayStatus(running, "NeoBot Desktop — "+label)
}

func (a *App) get() (*launcher.Manager, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.initErr != nil {
		return nil, a.initErr
	}
	if a.manager == nil {
		return nil, errors.New("启动器尚未就绪")
	}
	return a.manager, nil
}

// Quit 真正退出启动器（托盘「退出」和界面上的退出都走这里）。
func (a *App) Quit() {
	a.mu.Lock()
	a.quitting = true
	ctx := a.ctx
	a.mu.Unlock()
	stopTray()
	if ctx != nil {
		runtime.Quit(ctx)
	}
}

func (a *App) GetSnapshot() (launcher.Snapshot, error) {
	m, err := a.get()
	if err != nil {
		return launcher.Snapshot{}, err
	}
	return m.Snapshot(), nil
}

func (a *App) GetLogs(revision uint64) (launcher.LogSnapshot, error) {
	m, err := a.get()
	if err != nil {
		return launcher.LogSnapshot{}, err
	}
	return m.Logs(revision), nil
}

func (a *App) ListVersions() ([]launcher.Version, error) {
	m, err := a.get()
	if err != nil {
		return nil, err
	}
	return m.Available()
}

func (a *App) SaveSources(s launcher.Sources) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.SaveSources(s)
}

func (a *App) Install(version string, official, start bool) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.Install(version, official, start)
}

func (a *App) InstallBranch(repository, branch string, start, refresh bool) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.InstallBranch(repository, branch, start, refresh)
}

func (a *App) ListGitHubBranches(repository string, force bool) ([]launcher.Branch, error) {
	m, err := a.get()
	if err != nil {
		return nil, err
	}
	return m.ListGitHubBranches(repository, force)
}

func (a *App) SetVersionIsolation(enabled bool) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.SetVersionIsolation(enabled)
}

func (a *App) SelectVersion(version string) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.SelectVersion(version)
}

func (a *App) SwitchInfo() (launcher.DataSwitch, error) {
	m, err := a.get()
	if err != nil {
		return launcher.DataSwitch{}, err
	}
	return m.SwitchInfo(), nil
}

func (a *App) Start(confirmed bool) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.Start(confirmed)
}

func (a *App) Stop(force bool) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.Stop(force)
}

func (a *App) OpenDashboard() error {
	m, err := a.get()
	if err != nil {
		return err
	}
	address := m.Snapshot().DashboardURL
	if address == "" {
		return errors.New("网页面板尚未就绪")
	}
	ctx := a.windowContext()
	if ctx == nil {
		return errors.New("启动器正在关闭")
	}
	runtime.BrowserOpenURL(ctx, address)
	return nil
}

func (a *App) BindQQ(revision string, refresh bool) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.BindQQ(revision, refresh)
}

func (a *App) SaveQQ(revision, id, secret string) (launcher.SaveResult, error) {
	m, err := a.get()
	if err != nil {
		return launcher.SaveResult{}, err
	}
	return m.SaveQQ(revision, id, secret)
}

func (a *App) SaveModel(revision, envRevision, key, url, name string) (launcher.SaveResult, error) {
	m, err := a.get()
	if err != nil {
		return launcher.SaveResult{}, err
	}
	return m.SaveModel(revision, envRevision, key, url, name)
}

// ── 磁盘与版本生命周期 ───────────────────────────────────────────

func (a *App) RefreshUsage() error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.RefreshUsage()
}

func (a *App) CleanUsage(target string) (int64, error) {
	m, err := a.get()
	if err != nil {
		return 0, err
	}
	return m.CleanUsage(target)
}

func (a *App) RemoveVersion(version string, dropData bool) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.RemoveVersion(version, dropData)
}

func (a *App) Repair(version string) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.Repair(version)
}

// ── 体检与诊断 ───────────────────────────────────────────────────

func (a *App) RefreshHealth() error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.RefreshHealth()
}

func (a *App) ExportDiagnostics() (string, error) {
	m, err := a.get()
	if err != nil {
		return "", err
	}
	return m.ExportDiagnostics()
}

func (a *App) HostLogFiles() ([]launcher.HostLogFile, error) {
	m, err := a.get()
	if err != nil {
		return nil, err
	}
	return m.HostLogFiles()
}

func (a *App) ReadHostLog(name string, start, count int) (launcher.HostLogPage, error) {
	m, err := a.get()
	if err != nil {
		return launcher.HostLogPage{}, err
	}
	return m.ReadHostLog(name, start, count)
}

// ── 更新 ─────────────────────────────────────────────────────────

func (a *App) CheckUpdate(force bool) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.CheckUpdate(force)
}

func (a *App) DownloadUpdate() (string, error) {
	m, err := a.get()
	if err != nil {
		return "", err
	}
	return m.DownloadUpdate()
}

func (a *App) LaunchUpdate() error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.LaunchUpdate()
}

// ── 常驻服务 ─────────────────────────────────────────────────────

func (a *App) SetAutoRestart(enabled bool) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.SetAutoRestart(enabled)
}

func (a *App) SetAutostart(enabled bool) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.SetAutostart(enabled)
}

func (a *App) SetCloseToTray(enabled bool) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.SetCloseToTray(enabled)
}

// ── 通用 ─────────────────────────────────────────────────────────

// OpenTerminal 打开目标版本 venv 的系统终端。acknowledged 为 true 表示界面
// 已经让用户确认过首次风险提示。
func (a *App) OpenTerminal(version string, acknowledged bool) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	return m.OpenTerminal(version, acknowledged)
}

// OpenFolder 用资源管理器打开启动器自己的目录。
func (a *App) OpenFolder(kind string) error {
	m, err := a.get()
	if err != nil {
		return err
	}
	dir, err := m.Folder(kind)
	if err != nil {
		return err
	}
	return launcher.OpenPath(dir)
}
