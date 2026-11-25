package launcher

// 体检：在后台把一组只读检查跑一遍，结果交给界面展示。
// 检查不会启动 NeoBot，也不会修改安装目录里的文件；单项失败不影响其它项。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// healthDoctorTimeout 是 doctor 自检的等待上限：它本来就只跑几秒。
	healthDoctorTimeout = 10 * time.Second
	// healthPipTimeout 比 doctor 宽松：uv 要读取整个环境的元数据。
	healthPipTimeout = 60 * time.Second
	// healthPluginTimeout 是插件依赖探测的等待上限：只做本地清单扫描，不需要等网络。
	healthPluginTimeout = 30 * time.Second
	// healthControlTimeout 是本机控制接口的等待上限。
	healthControlTimeout = 5 * time.Second
	// healthOutputLimit 限制命令输出进内存的量；详情和诊断包只用到开头一段。
	healthOutputLimit = 64 << 10
	// healthDetailLimit 是放进 Detail 的字符数上限。
	healthDetailLimit = 500
)

// HealthItem 是体检中的一项结果。
type HealthItem struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	State  string `json:"state"` // ok | warn | fail | skip
	Detail string `json:"detail"`
	Hint   string `json:"hint"`
}

// Health 是一次体检的完整结果。
type Health struct {
	Running bool         `json:"running"`
	At      int64        `json:"at"`
	Items   []HealthItem `json:"items"`
	Error   string       `json:"error"`
}

// healthTarget 是体检和诊断包共用的上下文：在锁内取一次，之后所有检查都在锁外跑。
type healthTarget struct {
	// version 是这次体检针对的版本：运行中的版本优先，否则是下次启动的版本。
	// 检查使用的解释器和数据目录必须来自同一个版本。
	version  string
	versions []Version
	dataDir  string
	running  bool
	bot      *botProcess
	usage    Usage
}

// healthTarget 汇总当前状态。锁只在这里取，检查过程一路不加锁。
func (m *Manager) healthTarget() healthTarget {
	m.mu.Lock()
	defer m.mu.Unlock()
	version := m.state.Selected
	if m.bot != nil {
		version = m.bot.version
	}
	target := healthTarget{
		version:  version,
		running:  m.bot != nil,
		bot:      m.bot,
		usage:    m.usage,
		versions: make([]Version, 0, len(m.installed)),
	}
	for _, item := range m.installed {
		target.versions = append(target.versions, item)
	}
	sort.Slice(target.versions, func(i, j int) bool {
		return compareVersions(target.versions[i].Name, target.versions[j].Name) < 0
	})
	target.dataDir = m.dataDir(version)
	return target
}

// RefreshHealth 异步体检，结果写入 Snapshot.Health 并触发 launcher:changed。
// 必须覆盖：已安装版本完整性（Python 解释器/NeoBot 启动程序）、uv pip check、
// 已启用插件的依赖、上游 python -m neobot_app.cli doctor（退出码 1 表示有依赖问题）、
// 本机控制接口可达性、数据目录可写、磁盘剩余空间。
// 体检不启动 NeoBot，也不得修改任何文件。
func (m *Manager) RefreshHealth() error {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return errors.New("启动器正在关闭，请稍后重试")
	}
	if m.healthBusy {
		m.mu.Unlock()
		return errors.New("体检正在进行，请等这一次结束")
	}
	m.healthBusy = true
	// 开始时清掉上一轮结果：界面上「进行中」和结果二选一显示，
	// 旧结果留着只会把它挤到下面去，还容易被当成这次的结果。
	m.health = Health{Running: true, At: time.Now().Unix()}
	m.mu.Unlock()
	m.notify()
	go m.runHealth()
	return nil
}

// runHealth 是体检主体：所有检查都在锁外执行，最后一次性写回结果。
func (m *Manager) runHealth() {
	target := m.healthTarget()
	checks := []healthCheck{
		{key: "env", title: "已安装版本", run: m.checkInstalledFiles},
		{key: "pipcheck", title: "依赖完整性", run: m.checkPipCheck},
		{key: "plugindeps", title: "插件依赖", run: m.checkPluginDependencies},
		{key: "doctor", title: "NeoBot 自检", run: m.checkDoctor},
		{key: "control", title: "本机控制接口", run: m.checkControl},
		{key: "data", title: "数据目录", run: m.checkDataDir},
		{key: "disk", title: "磁盘空间", run: m.checkDisk},
	}
	items := make([]HealthItem, 0, len(checks))
	for _, check := range checks {
		items = append(items, m.runHealthCheck(check, target))
	}
	m.mu.Lock()
	m.healthBusy = false
	m.health = Health{At: time.Now().Unix(), Items: items}
	closing := m.closing
	m.mu.Unlock()
	if closing {
		// 体检可能跑几十秒，结束时界面可能已经销毁，不要再发事件。
		return
	}
	m.notify()
}

// healthCheck 是一项检查：run 返回状态、详情和建议，由 runHealthCheck 包成 HealthItem。
type healthCheck struct {
	key   string
	title string
	run   func(healthTarget) (string, string, string)
}

// runHealthCheck 执行一项检查，并把它内部的 panic 收成一个失败项：
// 体检不能因为某一项算错就停在「进行中」。
func (m *Manager) runHealthCheck(check healthCheck, target healthTarget) (item HealthItem) {
	defer func() {
		if recovered := recover(); recovered != nil {
			item = HealthItem{
				Key: check.key, Title: check.title, State: "fail",
				Detail: "这项检查因为内部错误没有完成。",
				Hint:   "请重新体检；如果反复出现，请导出诊断包反馈。",
			}
		}
	}()
	state, detail, hint := check.run(target)
	return HealthItem{Key: check.key, Title: check.title, State: state, Detail: detail, Hint: hint}
}

// checkInstalledFiles 检查每个已安装版本的 Python 解释器与 NeoBot 启动程序是否还在。
func (m *Manager) checkInstalledFiles(target healthTarget) (string, string, string) {
	if len(target.versions) == 0 {
		return "skip", "还没有安装 NeoBot 版本。", "在「版本管理」里安装一个版本后再体检。"
	}
	pythonName := filepath.Base(venvPython(""))
	botName := filepath.Base(botExecutable(""))
	missing := make([]string, 0, 2)
	for _, version := range target.versions {
		if !isFile(venvPython(m.versionDir(version.Name))) {
			missing = append(missing, version.Name+" 的 "+pythonName)
		}
		if !isFile(m.binary(version.Name)) {
			missing = append(missing, version.Name+" 的 "+botName)
		}
	}
	if len(missing) == 0 {
		return "ok", fmt.Sprintf("%d 个已安装版本的文件都齐全。", len(target.versions)), ""
	}
	detail := "缺少文件：" + strings.Join(clipList(missing, 6), "、") + "。"
	return "fail", detail, "在「版本管理」里对相应版本点「修复安装」；修不好就重新安装这个版本。"
}

// checkPipCheck 用 uv 检查当前体检版本的依赖是否互相兼容。
func (m *Manager) checkPipCheck(target healthTarget) (string, string, string) {
	command := m.pipCheckCommand(target)
	if command.Skipped != "" {
		return "skip", command.Skipped, "安装一个完整版本后会自动准备 uv 和依赖，再体检即可。"
	}
	result := command.run()
	switch {
	case result.Failed:
		return "fail", "无法运行依赖检查，当前版本的 Python 环境可能已经损坏。", "在「版本管理」里点「修复安装」重新准备这个版本。"
	case result.TimedOut:
		return "warn", fmt.Sprintf("依赖检查超过 %d 秒没有结束，可能被杀毒软件拖慢。", int(healthPipTimeout/time.Second)), "稍后重新体检；如果一直超时，请导出诊断包反馈。"
	case result.ExitCode == 0:
		return "ok", "当前版本的依赖版本相互兼容。", ""
	default:
		return "fail", "依赖检查发现冲突：" + clipText(result.Output, healthDetailLimit), "在「版本管理」里点「修复安装」重新安装依赖后重试。"
	}
}

// checkPluginDependencies 检查已启用插件声明的 Python 依赖是否装在当前版本的
// 环境里。只读：缺失时不在体检里安装，交给启动流程自动补装。
func (m *Manager) checkPluginDependencies(target healthTarget) (string, string, string) {
	python, skipped := m.healthPython(target)
	if skipped != "" {
		return "skip", skipped, "在「版本管理」里安装或修复这个版本后再体检。"
	}
	pluginDir := filepath.Join(target.dataDir, "plugins")
	if !hasPluginManifests(pluginDir) {
		return "skip", "数据目录里还没有安装插件。", "安装插件后再体检。"
	}
	report, err := probePluginDependenciesWithin(python, pluginDir, healthPluginTimeout)
	if err != nil {
		return "warn", "无法检查插件依赖：" + err.Error(), "在「版本管理」里点「修复安装」（分支版本请用「更新」），或检查插件清单是否兼容当前版本。"
	}
	return pluginDependencyHealth(report, disabledPlugins(target.dataDir))
}

// pluginDependencyHealth 把探测结果转成体检项。缺依赖时用 warn：启动 NeoBot
// 时会自动补装；只有探测本身或版本判断不可用时才需要用户额外处理。
func pluginDependencyHealth(report pluginDependencyReport, disabled map[string]bool) (string, string, string) {
	missing := make([]string, 0, len(report.Plugins))
	for _, item := range report.Plugins {
		if disabled[item.Name] || len(item.Missing) == 0 {
			continue
		}
		missing = append(missing, item.Name+" 缺少 "+strings.Join(item.Missing, "、"))
	}
	if len(missing) == 0 {
		return "ok", "已启用插件的依赖都已满足。", ""
	}
	if !report.Packaging {
		// 没有 packaging 时探测脚本把声明过的需求都当成缺失，不能把它们当成真实结果。
		return "warn", "当前环境缺少 packaging，无法判断插件依赖的版本范围。", "启动一次 NeoBot，或在「版本管理」里点「修复安装」后重新体检。"
	}
	detail := "以下插件依赖未满足：" + strings.Join(missing, "；") + "。"
	return "warn", clipText(detail, healthDetailLimit), "启动 NeoBot 时会自动尝试安装；失败时请检查下载源、网络或插件声明的依赖范围。"
}

// checkDoctor 运行上游自检命令。退出码 1 表示它报告了依赖或配置问题。
func (m *Manager) checkDoctor(target healthTarget) (string, string, string) {
	command := m.doctorCommand(target)
	if command.Skipped != "" {
		return "skip", command.Skipped, "安装一个完整版本后再体检。"
	}
	result := command.run()
	switch {
	case result.Failed:
		return "fail", "无法运行 NeoBot 自检，当前版本可能已经损坏。", "在「版本管理」里点「修复安装」，或重新安装这个版本。"
	case result.TimedOut:
		return "warn", fmt.Sprintf("NeoBot 自检超过 %d 秒没有结束。", int(healthDoctorTimeout/time.Second)), "重新体检一次；一直超时请导出诊断包反馈。"
	case result.ExitCode == 0:
		return "ok", "NeoBot 自检通过。", ""
	case result.ExitCode == 1:
		detail := clipText(result.Output, healthDetailLimit)
		if detail == "" {
			detail = "NeoBot 自检报告了问题，但没有给出说明。"
		}
		return "warn", detail, "按上面的提示处理；涉及依赖时可以在「版本管理」里点「修复安装」。"
	default:
		detail := clipText(result.Output, healthDetailLimit)
		if detail == "" {
			detail = "（没有输出）"
		}
		return "fail", fmt.Sprintf("NeoBot 自检异常退出（退出码 %d）：%s", result.ExitCode, detail), "导出诊断包后反馈；也可以先尝试「修复安装」。"
	}
}

// checkControl 确认运行中实例的本机控制接口真的能应答。
func (m *Manager) checkControl(target healthTarget) (string, string, string) {
	if !target.running || target.bot == nil {
		return "skip", "NeoBot 未运行。", "启动 NeoBot 后可以检查本机控制接口。"
	}
	d, err := readDiscovery(target.bot.data, target.bot.ownsPID)
	if err != nil {
		return "fail", "本机控制接口还不可用：" + err.Error(), "等启动完成后再体检；如果一直这样，可以在首页强制停止后重新启动。"
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthControlTimeout)
	defer cancel()
	var status struct {
		InstanceID string `json:"instance_id"`
	}
	if err = d.call(ctx, "/v1/status", nil, &status); err != nil {
		return "fail", "本机控制接口没有响应：" + err.Error(), "可以在首页「强制停止」后重新启动 NeoBot。"
	}
	return "ok", "本机控制接口可以正常响应。", ""
}

// checkDataDir 在数据目录里写一个临时文件再删掉，确认真的可写。
func (m *Manager) checkDataDir(target healthTarget) (string, string, string) {
	probe := target.dataDir
	note := ""
	if probe == "" {
		// 还没有选版本时 dataDir 是空串，filepath.Dir("") 是 "."，
		// 拿它去试写会落到进程当前目录（可能是 Program Files）。
		return "skip", "还没有确定数据目录，请先安装并选择一个版本。", ""
	}
	if !isDir(probe) {
		// 数据目录要等第一次启动才创建；体检不建目录，改在最近的已存在上级目录里试写。
		probe = nearestExistingDir(filepath.Dir(probe))
		if probe == "" {
			probe = m.root
		}
		note = "数据目录还没有创建，已在 " + probe + " 里验证写入。"
	}
	file, err := os.CreateTemp(probe, ".neobot-health-*.tmp")
	if err != nil {
		return "fail", "无法在 " + probe + " 里创建临时文件，数据目录不可写。", "检查磁盘剩余空间、目录权限，或安全软件是否拦截了写入。"
	}
	name := file.Name()
	_, writeErr := file.WriteString("health\n")
	closeErr := file.Close()
	_ = os.Remove(name)
	if writeErr != nil || closeErr != nil {
		return "fail", "写入测试文件失败：" + probe, "检查磁盘剩余空间或安全软件限制后再试。"
	}
	if note == "" {
		note = "数据目录可写：" + target.dataDir
	}
	return "ok", note, ""
}

// checkDisk 确认安装目录所在的磁盘可以访问。
//
// 标准库没有跨平台的「剩余空间」接口：Windows 用 GetDiskFreeSpaceEx，Linux 用
// Statfs，两者都在 platform_*.go 里。这里只负责组织提示文案；读取失败时降级为
// 只验证磁盘可访问，并给出启动器自身占用的空间。
func (m *Manager) checkDisk(target healthTarget) (string, string, string) {
	mount := m.root
	if volume := filepath.VolumeName(m.root); volume != "" {
		mount = volume + string(os.PathSeparator)
	}
	if _, err := os.Stat(mount); err != nil {
		return "fail", "无法访问安装目录所在的磁盘（" + mount + "），安装和运行都会失败。", "确认磁盘已经连接、没有被其它程序独占后再体检。"
	}
	detail := "安装目录所在的磁盘可以访问"
	if target.usage.Total > 0 {
		detail += "，启动器已占用 " + byteSizeText(target.usage.Total)
	}
	free, err := freeDiskSpace(mount)
	if err != nil {
		return "skip", detail + "；读取剩余空间失败。", "安装新版本大约需要 600 MB 空闲空间，可以在「导出诊断包」的 system.txt 里看磁盘明细。"
	}
	detail += "，剩余 " + byteSizeText(free)
	// 新装一个版本要下 Python、uv 和整包依赖，1 GB 以下很容易装到一半失败。
	if free < int64(1)<<30 {
		return "warn", detail + "。", "安装新版本大约需要 600 MB 空闲空间，建议先在「磁盘占用」里清理缓存或卸载不用的版本。"
	}
	return "ok", detail + "。", ""
}

// pipCheckCommand 组装 uv pip check：和安装阶段一样隐藏窗口，环境变量也保持一致。
func (m *Manager) pipCheckCommand(target healthTarget) stagedCommand {
	python, skipped := m.healthPython(target)
	if skipped != "" {
		return stagedCommand{Skipped: skipped}
	}
	uv := uvExecutable(m.root)
	if !isFile(uv) {
		return stagedCommand{Skipped: "找不到 uv，无法检查依赖。"}
	}
	return stagedCommand{
		Path: uv, Args: []string{"pip", "check", "--python", python},
		Env: cleanEnv(true, m.installEnv()...), Timeout: healthPipTimeout,
	}
}

// doctorCommand 组装上游的 doctor 自检命令。
func (m *Manager) doctorCommand(target healthTarget) stagedCommand {
	python, skipped := m.healthPython(target)
	if skipped != "" {
		return stagedCommand{Skipped: skipped}
	}
	dir := target.dataDir
	if !isDir(dir) {
		// 命令必须在已存在的目录里启动，否则会直接失败；数据目录没建时退回安装根目录。
		dir = m.root
	}
	return stagedCommand{
		Path: python, Args: []string{"-m", "neobot_app.cli", "doctor"}, Dir: dir,
		Env: cleanEnv(true, append(m.installEnv(), "PYTHONUTF8=1")...), Timeout: healthDoctorTimeout,
	}
}

// healthPython 返回当前体检版本的 Python 解释器；条件不满足时给出跳过原因。
func (m *Manager) healthPython(target healthTarget) (string, string) {
	if target.version == "" {
		return "", "还没有选择要运行的版本。"
	}
	python := venvPython(m.versionDir(target.version))
	if !isFile(python) {
		return "", "当前版本缺少 " + filepath.Base(python) + "，无法在这个版本上运行检查。"
	}
	return python, ""
}

// stagedCommand 是一条准备执行的检查命令。Skipped 非空表示条件不满足、命令不会执行：
// 体检把它显示成「跳过」，诊断包把原因写进对应的 txt。
type stagedCommand struct {
	Path    string
	Args    []string
	Dir     string
	Env     []string
	Timeout time.Duration
	Skipped string
}

// commandResult 是一条命令的执行结果。
type commandResult struct {
	Output   string
	ExitCode int
	TimedOut bool
	Failed   bool
}

// run 执行命令并收集输出。隐藏窗口、给空管道 stdin：和启动后端时一致，
// 否则 Python 会把 NUL 设备误判成终端，一直等输入直到超时。
func (c stagedCommand) run() commandResult {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = healthDoctorTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdin = strings.NewReader("")
	hideWindow(cmd)
	out := &cappedBuffer{limit: healthOutputLimit}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	result := commandResult{Output: out.String()}
	if err == nil {
		return result
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	} else {
		result.ExitCode = -1
		result.Failed = true
	}
	result.TimedOut = ctx.Err() != nil
	return result
}

// cappedBuffer 只保留前 limit 字节：检查命令的输出只用于展示摘要，不能无上限占内存。
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) < room {
			b.buf.Write(p)
		} else {
			b.buf.Write(p[:room])
		}
	}
	// 全部吃掉：返回短写会让子进程以为输出失败。
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

// clipText 把命令输出压到 limit 个字符：去掉 ANSI 转义和回车，保留换行方便阅读。
func clipText(value string, limit int) string {
	value = strings.TrimSpace(strings.ReplaceAll(terminalEscape.ReplaceAllString(value, ""), "\r", ""))
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

// clipList 只保留前几项，避免 Detail 太长把界面撑开。
func clipList(items []string, limit int) []string {
	if len(items) <= limit {
		return items
	}
	clipped := append(make([]string, 0, limit+1), items[:limit]...)
	return append(clipped, fmt.Sprintf("等 %d 处", len(items)))
}

// nearestExistingDir 找到 path 及其上级里第一个存在的目录。
func nearestExistingDir(path string) string {
	for path != "" {
		if isDir(path) {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return ""
		}
		path = parent
	}
	return ""
}

// byteSizeText 把字节数变成人读的大小，体检详情和诊断包共用。
func byteSizeText(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PB", value/unit)
}
