package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type botProcess struct {
	version     string
	data        string
	ownsPID     func(int) bool
	done        chan struct{}
	closeTree   func()
	ready       bool
	reportedURL string
	// expected 记录这次退出是不是用户主动停止（或启动器关闭）造成的，
	// 退出清理会把 m.stopping 复位，之后的自动重启判断只能依赖这个快照。
	expected bool
}

var (
	dashboardLine = regexp.MustCompile(`^(?:面板地址：|Dashboard URL:\s*)(http://\S+)`)
	dashboardLog  = regexp.MustCompile(`\|\s*plugin\.dashboard\s*\|\s*网页面板已启动\s*\|[^\r\n]*\blocal_url=(http://[^,\s]+)`)
)

func dashboardAddress(line string) string {
	line = strings.TrimSpace(printableLine(line))
	for _, pattern := range []*regexp.Regexp{dashboardLine, dashboardLog} {
		if match := pattern.FindStringSubmatch(line); len(match) == 2 && localURL(match[1], false) {
			return match[1]
		}
	}
	return ""
}

// Older releases ignore the desktop listener override. Seed their first-run
// config without changing any listener the user has already configured.
func seedDashboardConfig(data string) error {
	dir := filepath.Join(data, "plugins_data", "dashboard")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "config.toml"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.WriteString("host = \"127.0.0.1\"\n")
	return errors.Join(err, f.Close())
}

func (m *Manager) Start(confirmed bool) error {
	m.mu.Lock()
	if m.bot != nil || m.stopping || m.closing || m.job.State == "running" || m.preparing {
		m.mu.Unlock()
		return errors.New("请等待当前任务结束或先停止 NeoBot")
	}
	version := m.state.Selected
	selected, ok := m.installed[version]
	if !ok {
		m.mu.Unlock()
		return errors.New("请先安装并选择 NeoBot 版本")
	}
	if !isFile(m.binary(version)) || !isFile(venvPython(m.versionDir(version))) {
		m.mu.Unlock()
		return errors.New("当前版本安装不完整，请重新安装")
	}
	m.preparing, m.runtimeError = true, ""
	m.logRun++
	m.logs.reset(version, selected.Branch)
	m.logMessage("正在启动 NeoBot " + selected.Label())
	m.mu.Unlock()
	m.notify()
	go m.start(version, confirmed)
	return nil
}

func (m *Manager) start(version string, confirmed bool) {
	m.mu.Lock()
	defer func() { m.preparing = false; m.mu.Unlock(); m.notify() }()
	if m.closing {
		return
	}
	data, err := m.prepareData(version, confirmed)
	if err != nil {
		m.runtimeError = err.Error()
		m.logMessage("无法准备 NeoBot 数据，请查看上方的错误提示。")
		return
	}
	if err := seedDashboardConfig(data); err != nil {
		m.runtimeError = "无法初始化网页面板配置，请检查数据目录权限"
		m.logMessage(m.runtimeError)
		return
	}
	// 插件依赖同步可能要跑几分钟，必须在锁外：否则快照、停止和日志刷新都会被
	// 一起卡住。这一段没有 return，锁的配对仍由上面的 defer 负责。
	sources := m.state.Sources
	m.mu.Unlock()
	m.syncPluginDependencies(version, data, sources)
	m.mu.Lock()
	if m.closing {
		return
	}
	python := venvPython(m.versionDir(version))
	cmd := exec.Command(python, "-u", "-m", "neobot_app.cli")
	cmd.Dir = data
	cmd.Env = m.backendEnv(data, sources)
	hideWindow(cmd)
	prepareLaunch(cmd)
	reader, writer, err := os.Pipe()
	if err != nil {
		m.runtimeError = "无法读取本机启动状态"
		m.logMessage(m.runtimeError)
		return
	}
	cmd.Stdout, cmd.Stderr = writer, writer
	// 子进程必须拿到「非终端」的 stdin：Windows 的 NUL 设备会被 Python 的
	// sys.stdin.isatty() 判成终端，NeoBot 的插件依赖安装器于是向它提问，立刻
	// 读到 EOF 抛 EOFError——面板安装请求变成 500，启动装配期还会把进程带崩。
	// 给一个空 reader，子进程看到的是已经关闭的管道。
	cmd.Stdin = strings.NewReader("")
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		m.runtimeError = "无法启动 NeoBot，请检查安装是否完整"
		m.logMessage(m.runtimeError)
		return
	}
	_ = writer.Close()
	closeTree, ownsPID, err := trackProcessTree(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = reader.Close()
		m.runtimeError = "无法接管 NeoBot 进程，请重新打开桌面应用后重试"
		m.logMessage(m.runtimeError)
		return
	}
	p := &botProcess{version: version, data: data, ownsPID: ownsPID, closeTree: closeTree, done: make(chan struct{})}
	m.bot = p
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		m.watchOutput(p, reader)
	}()
	go func() {
		err := cmd.Wait()
		p.closeTree()
		// Drain final output before publishing the exit and permitting a new run.
		select {
		case <-outputDone:
		case <-time.After(time.Second):
			_ = reader.Close()
			<-outputDone
		}
		m.mu.Lock()
		if m.bot == p {
			if err != nil && !m.stopping && !m.closing {
				m.runtimeError = "NeoBot 意外退出；请查看下方日志"
			}
			if err == nil {
				m.logMessage("NeoBot 已退出。")
			} else {
				m.logMessage(fmt.Sprintf("NeoBot 已退出（退出码 %d）。", cmd.ProcessState.ExitCode()))
			}
			p.expected = p.expected || m.stopping || m.closing
			m.bot, m.stopping, m.setupSupported = nil, false, false
			m.setup, m.dashboardURL = SetupStatus{}, ""
		}
		close(p.done)
		m.mu.Unlock()
		m.notify()
		m.afterExit(p, err)
	}()
	go m.poll(p)
}

// backendEnv 是后端进程的环境：在继承的环境上补齐桌面端自己的设置。
//
// 后端自己也会调 uv/pip 装插件依赖，用户选了镜像就不该让它们回落到 pypi.org。
// 环境变量压不过命令行参数，桌面端自己的安装仍以 --default-index 为准。
func (m *Manager) backendEnv(data string, sources Sources) []string {
	return cleanEnv(false, backendEnvExtra(m.root, data, sources)...)
}

// backendEnvExtra 是 backendEnv 里由桌面端追加的那部分，单独拆出来便于测试。
func backendEnvExtra(root, data string, sources Sources) []string {
	extra := []string{
		"PYTHONUTF8=1",
		"NEOBOT_DATA_DIR=" + data,
		"NEOBOT_ENV_FILE=" + filepath.Join(data, ".env"),
		"NEOBOT_DESKTOP_LOCAL_DASHBOARD=1",
		// 与桌面端共用 uv 缓存：同一个包不必下载两次。
		"UV_CACHE_DIR=" + filepath.Join(root, "cache"),
	}
	if index := sources.Index; index != "" {
		extra = append(extra, "UV_DEFAULT_INDEX="+index, "PIP_INDEX_URL="+index)
	}
	return extra
}

func (m *Manager) watchOutput(bot *botProcess, r *os.File) {
	defer r.Close()
	readOutputLines(r, func(line string) {
		if address := dashboardAddress(line); address != "" {
			m.mu.Lock()
			if m.bot == bot {
				bot.reportedURL = address
			}
			m.mu.Unlock()
		}
		m.logs.add(redactLogLine(line))
	})
}

func (m *Manager) logMessage(message string) {
	m.logs.add("[NeoBot Desktop] " + message)
}

func (m *Manager) poll(bot *botProcess) {
	first := time.Now()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		m.pollOnce(bot, first)
		select {
		case <-bot.done:
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) pollOnce(bot *botProcess, first time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d, err := readDiscovery(bot.data, bot.ownsPID)
	supported := false
	ready := false
	var status SetupStatus
	if err == nil {
		supported, err = d.supported(ctx)
		ready = err == nil
		if supported {
			ready = d.call(ctx, "/v1/setup/status", nil, &status) == nil
		}
	}
	m.mu.Lock()
	reported := bot.reportedURL
	m.mu.Unlock()
	dashboard := status.DashboardURL
	if !localURL(dashboard, false) {
		dashboard = ""
	}
	if !supported && reported != "" && verifyDashboard(ctx, reported) {
		dashboard, ready = reported, true
	}
	m.mu.Lock()
	if m.bot != bot {
		m.mu.Unlock()
		return
	}
	if status.InstanceID != "" {
		m.setup = status
	}
	if ready {
		bot.ready, m.setupSupported, m.dashboardURL = true, supported, dashboard
		if !m.stopping {
			m.runtimeError = ""
		}
	} else if !bot.ready && time.Since(first) > 75*time.Second {
		m.runtimeError = "NeoBot 仍未就绪，请检查日志；可停止后重试"
	}
	m.mu.Unlock()
	m.notify()
}

func verifyDashboard(ctx context.Context, address string) bool {
	if !localURL(address, false) {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(address, "/")+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := controlHTTP.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var body struct {
		OK     bool
		Plugin string
	}
	return resp.StatusCode == 200 && json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body) == nil &&
		body.OK && body.Plugin == "dashboard"
}

func (m *Manager) Stop(force bool) error {
	m.mu.Lock()
	p := m.bot
	if p == nil {
		m.mu.Unlock()
		return nil
	}
	if m.stopping && !force {
		m.mu.Unlock()
		return nil
	}
	m.stopping = true
	// 用户点了停止就先记下来：强制停止可能先超时复位 m.stopping，
	// 进程随后才真正退出，那时只有 p.expected 还能说明「这是主动停的」。
	p.expected = true
	m.mu.Unlock()
	m.notify()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	d, err := readDiscovery(p.data, p.ownsPID)
	if err == nil {
		err = d.call(ctx, "/v1/stop", map[string]any{}, nil)
	}
	if err != nil && force {
		p.closeTree()
	}
	if err != nil && !force {
		m.stopFailed(p, "此版本不支持正常停止，或控制接口未响应。可以选择「强制停止」。")
		return errors.New("无法正常停止 NeoBot")
	}
	select {
	case <-p.done:
		return nil
	case <-time.After(8 * time.Second):
		if force {
			p.closeTree()
			select {
			case <-p.done:
				return nil
			case <-time.After(4 * time.Second):
			}
		}
		m.stopFailed(p, "NeoBot 还未退出，暂时不能切换版本")
		return fmt.Errorf("等待 NeoBot 退出超时")
	}
}

func (m *Manager) stopFailed(p *botProcess, reason string) {
	m.mu.Lock()
	if m.bot == p {
		m.stopping, m.runtimeError = false, reason
		// 进程其实还活着：撤回「这是主动停止」的标记，之后它若崩溃要按崩溃处理。
		p.expected = false
	}
	m.mu.Unlock()
	m.notify()
}

func (m *Manager) setRuntimeError(reason string) {
	m.mu.Lock()
	m.runtimeError = reason
	m.mu.Unlock()
	m.notify()
}

func AppData() (string, error) {
	base, err := appDataBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "NeoBotDesktop"), nil
}
