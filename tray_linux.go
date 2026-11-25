//go:build linux

package main

// Linux 托盘：通过 StatusNotifierItem + dbusmenu 挂到桌面环境的通知区域。
// Wails v2 没有托盘 API，这里用 energye/systray（Linux 实现是纯 Go 的 DBus，
// 不启动第二个 GTK 主循环，可以和 Wails 共用进程）。
//
// 与 tray_windows.go 一样：初始化失败必须静默降级，trayAvailable/trayInstalled
// 要如实反映「图标是否真的挂上了」，否则关闭窗口会藏进一个不存在的托盘。

import (
	_ "embed"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/energye/systray"
	"github.com/godbus/dbus/v5"
)

//go:embed build/tray.png
var trayIconPNG []byte

var (
	trayMu         sync.Mutex // 保护下面的状态字段
	trayUiMu       sync.Mutex // 串行化 systray 的菜单/图标调用（库内部不保证线程安全）
	trayCmds       trayCommands
	trayStarted    bool
	trayStopped    bool
	trayBroken     bool
	trayRegistered bool
	trayMenuReady  bool
	trayRunning    bool
	trayLastTip    string
	trayStatusItem *systray.MenuItem
	trayStartItem  *systray.MenuItem
	trayStopItem   *systray.MenuItem
	trayEnd        func()
)

// trayAvailable 报告托盘是否还值得使用：初始化失败或已经停止后返回 false，
// 主窗口就不会把窗口藏进一个不存在的托盘。
func trayAvailable() bool {
	trayMu.Lock()
	defer trayMu.Unlock()
	return !trayBroken && !trayStopped
}

// trayInstalled 报告图标是否真的注册到了通知区域。trayAvailable 在初始化完成前
// 也为 true，判断「这台机器上有托盘」必须用这个函数。
func trayInstalled() bool {
	trayMu.Lock()
	defer trayMu.Unlock()
	return trayRegistered && trayMenuReady && !trayBroken && !trayStopped
}

// startTray 在独立 goroutine 里注册 StatusNotifierItem，函数立即返回。
// 任何一步失败都静默降级：托盘不可用时关闭窗口就真的退出。
func startTray(commands trayCommands) {
	trayMu.Lock()
	if trayStarted {
		trayMu.Unlock()
		return
	}
	trayStarted = true
	trayCmds = commands
	trayMu.Unlock()

	start, end := systray.RunWithExternalLoop(trayReady, trayExit)
	trayMu.Lock()
	trayEnd = end
	trayMu.Unlock()
	// systray 在连接不上会话总线时可能直接 panic；托盘不可用不能带走整个启动器。
	func() {
		defer func() {
			if recover() != nil {
				markTrayBroken()
			}
		}()
		start()
	}()
	go waitForTrayRegistration()
}

// waitForTrayRegistration 在启动后短暂轮询 StatusNotifierWatcher。库里注册失败
// 只打日志，不会返回错误，所以只能自己确认图标真的被通知区域接管。
func waitForTrayRegistration() {
	for attempt := 0; attempt < 30; attempt++ {
		trayMu.Lock()
		stopped := trayStopped
		trayMu.Unlock()
		if stopped {
			return
		}
		if trayWatcherRegistered() {
			trayMu.Lock()
			if !trayStopped {
				trayRegistered = true
			}
			trayMu.Unlock()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	trayMu.Lock()
	if !trayStopped {
		trayBroken = true
	}
	trayMu.Unlock()
}

// trayWatcherRegistered 查 StatusNotifierWatcher 是否已经接管了本进程的图标。
// 没有宿主（例如没装 AppIndicator 扩展的 GNOME）时返回 false，调用方降级。
func trayWatcherRegistered() bool {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}
	defer conn.Close()
	watcher := conn.Object("org.kde.StatusNotifierWatcher", "/StatusNotifierWatcher")

	// 宿主没有注册时图标不会显示；属性缺失的宿主则继续看条目列表。
	var hostValue dbus.Variant
	if err := watcher.Call("org.freedesktop.DBus.Properties.Get", 0,
		"org.kde.StatusNotifierWatcher", "IsStatusNotifierHostRegistered").Store(&hostValue); err == nil {
		if host, ok := hostValue.Value().(bool); ok && !host {
			return false
		}
	}

	// 库会用 org.kde.StatusNotifierItem-<pid>-1 申请一个总线名；拿它的唯一名去
	// 比对 watcher 条目，避免依赖 PID（PID 命名空间、沙箱里可能对不上）。
	wellKnown := "org.kde.StatusNotifierItem-" + strconv.Itoa(os.Getpid()) + "-1"
	var owner string
	if err := conn.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, wellKnown).Store(&owner); err != nil || owner == "" {
		return false
	}

	var itemsValue dbus.Variant
	if err := watcher.Call("org.freedesktop.DBus.Properties.Get", 0,
		"org.kde.StatusNotifierWatcher", "RegisteredStatusNotifierItems").Store(&itemsValue); err != nil {
		return false
	}
	var items []string
	switch value := itemsValue.Value().(type) {
	case []string:
		items = value
	case []interface{}:
		for _, item := range value {
			if text, ok := item.(string); ok {
				items = append(items, text)
			}
		}
	}
	for _, item := range items {
		if item == owner || item == wellKnown || strings.HasPrefix(item, owner+"/") || strings.HasPrefix(item, wellKnown+"/") {
			return true
		}
	}
	return false
}

// trayReady 由 systray 在 DBus 注册阶段调用，负责建菜单。它不能阻塞。
func trayReady() {
	defer func() {
		if recover() != nil {
			markTrayBroken()
		}
	}()
	trayMu.Lock()
	stopped := trayStopped
	trayMu.Unlock()
	if stopped {
		return
	}
	// systray 的 MenuItem 没有对 title/disabled 做内部加锁；这里和 updateTrayStatus
	// 共用一把锁，避免 notify 的多个 goroutine 并发改菜单。
	trayUiMu.Lock()
	defer trayUiMu.Unlock()
	// 等锁期间可能已经被 stopTray 关闭。
	trayMu.Lock()
	stopped = trayStopped || trayBroken
	trayMu.Unlock()
	if stopped {
		return
	}
	systray.SetIcon(trayIconPNG)
	systray.SetTitle("NeoBot Desktop")

	trayMu.Lock()
	commands := trayCmds
	running, tooltip := trayRunning, trayLastTip
	trayMu.Unlock()

	status := systray.AddMenuItem("NeoBot Desktop", "NeoBot Desktop 运行状态")
	status.Disable()
	systray.AddSeparator()

	show := systray.AddMenuItem("显示主窗口", "把启动器窗口带到前台")
	startItem := systray.AddMenuItem("启动 NeoBot", "启动当前选中的版本")
	stopItem := systray.AddMenuItem("停止 NeoBot", "停止正在运行的版本")
	systray.AddSeparator()
	dashboard := systray.AddMenuItem("打开网页管理", "在浏览器中打开网页面板")
	openData := systray.AddMenuItem("打开数据目录", "打开当前版本的数据目录")
	systray.AddSeparator()
	diagnose := systray.AddMenuItem("运行体检", "检查当前版本的运行环境")
	systray.AddSeparator()
	quit := systray.AddMenuItem("退出", "退出 NeoBot Desktop")

	show.Click(func() { runTrayCommand(commands.Show) })
	startItem.Click(func() { runTrayCommand(commands.Start) })
	stopItem.Click(func() { runTrayCommand(commands.Stop) })
	dashboard.Click(func() { runTrayCommand(commands.Dashboard) })
	openData.Click(func() { runTrayCommand(commands.OpenData) })
	diagnose.Click(func() { runTrayCommand(commands.Diagnose) })
	quit.Click(func() { runTrayCommand(commands.Quit) })
	systray.SetOnClick(func(systray.IMenu) { runTrayCommand(commands.Show) })
	systray.SetOnDClick(func(systray.IMenu) { runTrayCommand(commands.Show) })

	trayMu.Lock()
	trayStatusItem, trayStartItem, trayStopItem = status, startItem, stopItem
	trayMenuReady = true
	trayMu.Unlock()
	applyTrayRunning(running)
	if tooltip != "" {
		applyTrayTooltip(tooltip)
	}
}

// trayExit 在托盘连接结束时清掉菜单状态，防止关闭流程再碰已经释放的对象。
func trayExit() {
	trayMu.Lock()
	trayMenuReady = false
	trayStatusItem, trayStartItem, trayStopItem = nil, nil, nil
	trayMu.Unlock()
}

// runTrayCommand 把菜单动作放到新的 goroutine 里：停止 NeoBot、打开浏览器都可能
// 阻塞好几秒，不能卡住 DBus 回调。
func runTrayCommand(command func()) {
	if command != nil {
		go command()
	}
}

// updateTrayStatus 更新 tooltip、状态行和启动/停止菜单项的可用状态。
// 没有宿主显示 tooltip 时，状态行是唯一的运行状态提示。
func updateTrayStatus(running bool, tooltip string) {
	trayMu.Lock()
	trayRunning, trayLastTip = running, tooltip
	ready := trayMenuReady && !trayBroken && !trayStopped
	trayMu.Unlock()
	if !ready {
		return
	}
	trayUiMu.Lock()
	defer trayUiMu.Unlock()
	// 等锁期间托盘可能已经被停止或标记为不可用。
	trayMu.Lock()
	active := trayMenuReady && !trayBroken && !trayStopped
	trayMu.Unlock()
	if !active {
		return
	}
	applyTrayRunning(running)
	applyTrayTooltip(tooltip)
}

func applyTrayRunning(running bool) {
	trayMu.Lock()
	startItem, stopItem := trayStartItem, trayStopItem
	trayMu.Unlock()
	if startItem == nil || stopItem == nil {
		return
	}
	if running {
		startItem.Disable()
		stopItem.Enable()
	} else {
		startItem.Enable()
		stopItem.Disable()
	}
}

func applyTrayTooltip(tooltip string) {
	trayMu.Lock()
	status := trayStatusItem
	trayMu.Unlock()
	systray.SetTooltip(tooltip)
	if status != nil {
		status.SetTitle(tooltip)
	}
}

// stopTray 移除托盘图标并结束 DBus 连接，可重复调用。
func stopTray() {
	trayMu.Lock()
	if trayStopped {
		trayMu.Unlock()
		return
	}
	trayStopped = true
	trayMenuReady = false
	end := trayEnd
	trayEnd = nil
	trayMu.Unlock()
	if end == nil {
		return
	}
	trayUiMu.Lock()
	defer trayUiMu.Unlock()
	defer func() { _ = recover() }()
	end()
}

// markTrayBroken 把托盘标记成不可用。
func markTrayBroken() {
	trayMu.Lock()
	trayBroken, trayRegistered, trayMenuReady = true, false, false
	trayMu.Unlock()
}
