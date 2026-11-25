package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Intentionally empty until a published build passes the Windows end-to-end release gate.
	recommendedVersion = ""
	// Bumping uvVersion also requires updating uvWheelSHA256 and uvArchiveSHA256
	// in uv_<platform>.go, and confirming the mirror already carries that wheel
	// and that the wheel still stores uv at uvWheelEntry; otherwise accelerated
	// installs fail with "uv 下载不完整或校验失败" (stale digest) or
	// "下载源没有提供 uv <版本>" (missing wheel), and official installs fail with
	// the same digest error.
	uvVersion     = "0.12.9"
	officialUV    = "https://github.com/astral-sh/uv/releases/download"
	officialIndex = "https://pypi.org/simple"
	mirrorPython  = "https://mirror.nju.edu.cn/github-release/astral-sh/python-build-standalone"
	mirrorIndex   = "https://pypi.tuna.tsinghua.edu.cn/simple"
)

// versionTrashName 匹配卸载中转目录：<版本名>.trash-<16~20 位纳秒>。
var versionTrashName = regexp.MustCompile(`^.+\.trash-[0-9]{16,20}$`)

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(?:(a|b|rc)(0|[1-9][0-9]{0,8}))?$`)
var simpleLink = regexp.MustCompile(`(?i)<a\s+[^>]*?href\s*=\s*["']([^"']+)["'][^>]*>`)

type Sources struct {
	Preset string `json:"preset"`
	UV     string `json:"uv"`
	Python string `json:"python"`
	Index  string `json:"index"`
}

type Job struct {
	Step       string `json:"step"`
	State      string `json:"state"`
	Message    string `json:"message"`
	Version    string `json:"version"`
	Branch     string `json:"branch,omitempty"`
	Repository string `json:"repository,omitempty"`
	Completed  int    `json:"completed"`
	Total      int    `json:"total"`
	StartAfter bool   `json:"start_after"`
}

type Version struct {
	Name       string `json:"name"`
	Preview    bool   `json:"preview"`
	Branch     string `json:"branch,omitempty"`
	Repository string `json:"repository,omitempty"`
}

func (v Version) Label() string {
	if v.Branch == "" {
		return v.Name
	}
	if v.Repository == "" || v.Repository == githubRepo {
		return "分支 " + v.Branch
	}
	return "分支 " + v.Repository + "@" + v.Branch
}

type installRecord struct {
	Name       string `json:"name"`
	Preview    bool   `json:"preview"`
	Branch     string `json:"branch,omitempty"`
	Repository string `json:"repository,omitempty"`
}

type Snapshot struct {
	Installed      []Version        `json:"installed"`
	Selected       string           `json:"selected"`
	Running        string           `json:"running"`
	Starting       bool             `json:"starting"`
	Stopping       bool             `json:"stopping"`
	RuntimeError   string           `json:"runtime_error"`
	LogRun         uint64           `json:"log_run"`
	Recommended    string           `json:"recommended"`
	Sources        Sources          `json:"sources"`
	Job            Job              `json:"job"`
	Setup          SetupStatus      `json:"setup"`
	SetupSupported bool             `json:"setup_supported"`
	DashboardURL   string           `json:"dashboard_url"`
	IsolateData    bool             `json:"isolate_data"`
	DataDir        string           `json:"data_dir"`
	LauncherLogDir string           `json:"launcher_log_dir"`
	Usage          Usage            `json:"usage"`
	Health         Health           `json:"health"`
	Update         UpdateStatus     `json:"update"`
	Supervisor     SupervisorStatus `json:"supervisor"`
	Terminal       TerminalInfo     `json:"terminal"`
	// TerminalAcknowledged 记录用户是否已经确认过「高级」终端的使用提示。
	TerminalAcknowledged bool `json:"terminal_acknowledged"`
}

type diskState struct {
	Selected    string            `json:"selected"`
	Sources     Sources           `json:"sources"`
	DataVersion string            `json:"data_version"`
	Backups     map[string]string `json:"backups"`
	IsolateData bool              `json:"isolate_data"`
	AutoRestart bool              `json:"auto_restart"`
	Autostart   bool              `json:"autostart"`
	CloseToTray bool              `json:"close_to_tray"`
	// CloseToTraySet 区分「用户明确关掉」和「还没设置过」：没设置过时默认收进托盘。
	CloseToTraySet bool `json:"close_to_tray_set"`
	// TerminalAcknowledged 只影响第一次打开高级终端时的风险提示。
	TerminalAcknowledged bool `json:"terminal_acknowledged"`
}

type Manager struct {
	root           string
	mu             sync.Mutex
	changed        func()
	state          diskState
	installed      map[string]Version
	job            Job
	jobCancel      context.CancelFunc
	jobDone        chan struct{}
	bot            *botProcess
	logs           sessionLog
	logRun         uint64
	preparing      bool
	stopping       bool
	closing        bool
	runtimeError   string
	setup          SetupStatus
	setupSupported bool
	dashboardURL   string
	releaseLock    func()
	branchRepo     string
	branches       []Branch
	branchesAt     time.Time

	usage      Usage
	usageBusy  bool
	health     Health
	healthBusy bool
	update     UpdateStatus
	updateBusy bool
	crashCount int
	lastCrash  string
	trayReady  bool
	logState   *launcherLogState
}

func Open(root string, changed func()) (*Manager, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	release, err := lockInstance(filepath.Join(root, "desktop.lock"))
	if err != nil {
		return nil, err
	}
	m := &Manager{root: root, changed: changed, installed: map[string]Version{}, releaseLock: release}
	m.startLauncherLog()
	m.logs.sink = m.appendLauncherLog
	// 打开失败时要把已经创建的日志文件一起关掉，否则会漏一个文件句柄。
	abort := func() {
		m.closeLauncherLog()
		release()
	}
	// Decode into a zero value so a settings file that omits a source field does
	// not inherit a default it never asked for; validSources fills the preset.
	var state diskState
	err = readJSON(filepath.Join(root, "settings.json"), &state)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		abort()
		return nil, fmt.Errorf("读取启动器设置：%w", err)
	}
	state.Sources, err = validSources(state.Sources)
	if err != nil {
		abort()
		return nil, err
	}
	if state.Backups == nil {
		state.Backups = map[string]string{}
	}
	// 新装用户默认「关闭窗口 = 收进托盘」：这是常驻型工具该有的默认值。
	// 关掉的用户会被 CloseToTraySet 记住，不会每次启动又被改回来。
	if !state.CloseToTraySet && !state.CloseToTray {
		state.CloseToTray, state.CloseToTraySet = true, true
	}
	m.state = state
	if m.state.DataVersion != "" &&
		(!versionPattern.MatchString(m.state.DataVersion) || versionOf(m.state.DataVersion).Preview) {
		// Older builds could persist a branch id here. It only tracks shared
		// stable data, so dropping the marker is safer than refusing to start.
		m.state.DataVersion = ""
		_ = m.persist()
	}
	if !validDiskState(m.state) {
		abort()
		return nil, errors.New("启动器版本记录无效")
	}
	if err := m.recoverData(); err != nil {
		abort()
		return nil, err
	}
	if recoverErr := m.recoverVersionDirs(); recoverErr != nil {
		m.runtimeError = "部分版本目录恢复失败，请检查数据目录权限"
	}
	// A hard shutdown can leave a half-extracted branch source behind.
	_ = os.RemoveAll(filepath.Join(root, "tmp"))
	m.cleanVersionTrash()
	m.migratePreviewData()
	if scanErr := m.scanInstalled(); scanErr != nil {
		m.runtimeError = "无法读取已安装版本，请检查数据目录权限"
	} else if _, ok := m.installed[m.state.Selected]; !ok && m.state.Selected != "" {
		m.state.Selected = ""
		_ = m.persist()
	}
	return m, nil
}

func versionOf(name string) Version {
	parts := versionPattern.FindStringSubmatch(name)
	return Version{Name: name, Preview: len(parts) > 4 && parts[4] != ""}
}

func compareVersions(a, b string) int {
	x, y := versionPattern.FindStringSubmatch(a), versionPattern.FindStringSubmatch(b)
	if x == nil || y == nil {
		return strings.Compare(a, b)
	}
	for _, i := range []int{1, 2, 3, 4, 5} {
		if x[i] == y[i] {
			continue
		}
		if i == 4 {
			rank := map[string]int{"a": 0, "b": 1, "rc": 2, "": 3}
			return rank[x[i]] - rank[y[i]]
		}
		n, _ := strconv.Atoi(x[i])
		m, _ := strconv.Atoi(y[i])
		return n - m
	}
	return 0
}

// defaultSources fills in the preset for a download scheme. Only the explicit
// official preset stays on the upstream hosts; every other value, including the
// empty preset written by older builds, gets the accelerated mirrors.
func defaultSources(preset string) Sources {
	if preset == "official" {
		return Sources{Preset: "official", UV: officialUV, Index: officialIndex}
	}
	return Sources{Preset: "accelerated", UV: officialUV, Python: mirrorPython, Index: mirrorIndex}
}

func validSources(s Sources) (Sources, error) {
	switch s.Preset {
	case "official", "accelerated", "":
		return defaultSources(s.Preset), nil
	case "custom":
		fields := []*string{&s.UV, &s.Python, &s.Index}
		for i, field := range fields {
			*field = strings.TrimRight(strings.TrimSpace(*field), "/")
			if i == 1 && *field == "" {
				continue
			}
			if *field == "" {
				return Sources{}, errors.New("自定义下载源需要填写完整地址")
			}
			u, err := url.Parse(*field)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return Sources{}, errors.New("下载源需为不含账号、参数或片段的 HTTPS 地址")
			}
		}
		return s, nil
	default:
		return Sources{}, errors.New("未知下载方案")
	}
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{
		Installed: make([]Version, 0, len(m.installed)), Selected: m.state.Selected, Job: m.job,
		Recommended: m.recommended(), Sources: m.state.Sources, Setup: m.setup,
		SetupSupported: m.setupSupported, DashboardURL: m.dashboardURL, Stopping: m.stopping,
		RuntimeError: m.runtimeError, Starting: m.preparing, LogRun: m.logRun, IsolateData: m.state.IsolateData,
		Usage: m.usage, Health: m.health, Update: m.update,
		Supervisor: m.supervisorStatus(), LauncherLogDir: m.launcherLogDir(),
		Terminal: m.terminalInfoLocked(), TerminalAcknowledged: m.state.TerminalAcknowledged,
	}
	for _, v := range m.installed {
		s.Installed = append(s.Installed, v)
	}
	sort.Slice(s.Installed, func(i, j int) bool {
		a, b := s.Installed[i], s.Installed[j]
		if (a.Branch == "") != (b.Branch == "") {
			return a.Branch == ""
		}
		if a.Branch != "" {
			if a.Branch == b.Branch {
				return a.Repository < b.Repository
			}
			return a.Branch < b.Branch
		}
		return compareVersions(a.Name, b.Name) > 0
	})
	if m.bot != nil {
		s.Running = m.bot.version
		s.Starting = !m.bot.ready
	}
	inUse := s.Running
	if inUse == "" {
		inUse = m.state.Selected
	}
	s.DataDir = m.dataDir(inUse)
	return s
}

// supervisorStatus 汇总常驻服务状态。调用方持有 m.mu。
func (m *Manager) supervisorStatus() SupervisorStatus {
	return SupervisorStatus{
		AutoRestart: m.state.AutoRestart,
		Autostart:   m.state.Autostart,
		CloseToTray: m.state.CloseToTray,
		CrashCount:  m.crashCount,
		LastCrash:   m.lastCrash,
		TrayReady:   m.trayReady,
	}
}

// dataDir is where the given version keeps its data. An isolated version owns
// its own directory; every other version shares the active one. Callers hold
// the manager lock.
func (m *Manager) dataDir(version string) string {
	if !m.state.IsolateData {
		return filepath.Join(m.root, "data")
	}
	if version == "" {
		return filepath.Join(m.root, "isolated")
	}
	return filepath.Join(m.root, "isolated", version)
}

func (m *Manager) notify() {
	if m.changed != nil {
		m.changed()
	}
}

func (m *Manager) SaveSources(s Sources) error {
	normalized, err := validSources(s)
	if err != nil {
		return err
	}
	m.mu.Lock()
	before := m.state.Sources
	m.state.Sources = normalized
	if err = m.persist(); err != nil {
		m.state.Sources = before
	}
	m.mu.Unlock()
	m.notify()
	return err
}

func (m *Manager) SetVersionIsolation(enabled bool) error {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return errors.New("启动器正在关闭")
	}
	if m.job.State == "running" {
		m.mu.Unlock()
		return errors.New("请等待当前任务结束")
	}
	if m.bot != nil || m.preparing || m.stopping {
		m.mu.Unlock()
		return errors.New("请先停止 NeoBot")
	}
	before := m.state.IsolateData
	m.state.IsolateData = enabled
	err := m.persist()
	if err != nil {
		m.state.IsolateData = before
		err = errors.New("无法保存版本隔离设置，请检查磁盘空间")
	}
	m.mu.Unlock()
	m.notify()
	return err
}

func (m *Manager) SelectVersion(version string) error {
	m.mu.Lock()
	if m.preparing || m.closing {
		m.mu.Unlock()
		return errors.New("请等待当前启动任务结束")
	}
	if _, ok := m.installed[version]; !ok {
		m.mu.Unlock()
		return errors.New("请先安装这个版本")
	}
	before := m.state.Selected
	m.state.Selected = version
	err := m.persist()
	if err != nil {
		m.state.Selected = before
	}
	m.mu.Unlock()
	m.notify()
	return err
}

func (m *Manager) persist() error { return writeJSON(filepath.Join(m.root, "settings.json"), m.state) }
func (m *Manager) versionDir(version string) string {
	return filepath.Join(m.root, "versions", version)
}
func (m *Manager) binary(version string) string {
	return botExecutable(m.versionDir(version))
}
func (m *Manager) Close() {
	m.mu.Lock()
	m.closing = true
	if m.jobCancel != nil {
		m.jobCancel()
	}
	jobDone := m.jobDone
	m.mu.Unlock()
	if jobDone != nil {
		// 取消安装/修复不一定立刻生效（uv 要先把子进程收掉），退出最多等 5 秒，
		// 不能让窗口关了而进程无声挂在那里。
		select {
		case <-jobDone:
		case <-time.After(5 * time.Second):
		}
	}
	_ = m.Stop(true)
	m.closeLauncherLog()
	// crashTimes 以 *Manager 为键，不清理会跟着进程一直留着。
	crashTimes.Delete(m)
	m.releaseLock()
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func readJSON(path string, out any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewDecoder(file).Decode(out)
}

func writeJSON(path string, value any) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(value)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return replaceFile(file.Name(), path)
}

// supported 报告当前平台是否支持安装与运行 NeoBot。Windows 与 Linux x64 已实现；
// 其他平台仍然只保留只读界面。
func supported() bool {
	return (runtime.GOOS == "windows" || runtime.GOOS == "linux") && runtime.GOARCH == "amd64"
}
func timeID() string { return strconv.FormatInt(time.Now().UnixNano(), 10) }

// cleanVersionTrash 清理卸载版本时中断留下的中转目录。
// 卸载走 rename-then-delete，进程如果在中间被杀就会留下 <name>.trash-<纳秒>；
// 这些目录既不算已安装、也不会被磁盘统计覆盖，只能在启动时顺手清掉。
// .old-* 和 .broken-* 由 recoverVersionDirs 负责，这里绝不能碰。
func (m *Manager) cleanVersionTrash() {
	entries, err := os.ReadDir(filepath.Join(m.root, "versions"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !versionTrashName.MatchString(name) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(m.root, "versions", name))
	}
}

// MarkTrayReady 记录托盘是否可用，供界面显示「关闭窗口后仍会后台运行」。
func (m *Manager) MarkTrayReady(ready bool) {
	m.mu.Lock()
	m.trayReady = ready
	m.mu.Unlock()
	m.notify()
}

// Folder 返回启动器各类目录的绝对路径，供「打开文件夹」使用。
func (m *Manager) Folder(kind string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inUse := m.state.Selected
	if m.bot != nil {
		inUse = m.bot.version
	}
	switch kind {
	case "root":
		return m.root, nil
	case "data":
		return m.dataDir(inUse), nil
	case "versions":
		return filepath.Join(m.root, "versions"), nil
	case "version":
		if inUse == "" && len(m.installed) == 1 {
			for name := range m.installed {
				inUse = name
			}
		}
		if inUse == "" {
			return "", errors.New("还没有选择要维护的版本")
		}
		return m.versionDir(inUse), nil
	case "backups":
		return filepath.Join(m.root, "backups"), nil
	case "logs":
		return m.launcherLogDir(), nil
	case "host_logs":
		return filepath.Join(m.dataDir(inUse), "logs"), nil
	case "diagnostics":
		return filepath.Join(m.root, "diagnostics"), nil
	case "plugins":
		return filepath.Join(m.dataDir(inUse), "plugins"), nil
	default:
		return "", errors.New("未知的目录类型")
	}
}
