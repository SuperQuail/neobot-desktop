package launcher

// 诊断包：把一个可以反馈给开发者的 zip 写到 <root>/diagnostics/ 下。
// 只读取排查必需的文件：data/.env、data/.neobot/control.json、plugins_data/
// 和 settings.json 原文一律不进包。

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	// diagnosticsMaxLogFiles / diagnosticsLogLimit 限制启动器日志：最多 3 个、每个最多 1MB。
	diagnosticsMaxLogFiles = 3
	diagnosticsLogLimit    = 1 << 20
	// diagnosticsHostLines 是宿主日志只带的最后行数。
	diagnosticsHostLines = 2000
	// diagnosticsStateLimit 是 plugin_state.json 的上限，超过就不用它的内容。
	diagnosticsStateLimit = 1 << 20
	// diagnosticsManifestLimit 只用来读 plugin.toml 开头的 name/version。
	diagnosticsManifestLimit = 256 << 10
	// diagnosticsPrefix / diagnosticsKeepBundles 决定诊断包留在目录里的数量。
	// 诊断包只对「这一次出的问题」有意义，旧包按文件名排序还排在前面，
	// 找刚生成的那份得先翻过一堆旧包，所以导出成功后只留最新的一份。
	diagnosticsPrefix      = "neobot-desktop-diag-"
	diagnosticsKeepBundles = 1
)

// zipEntry 是诊断包里的一个文件。
type zipEntry struct {
	name string
	data []byte
}

// diagnosticsMeta 描述生成诊断包时的启动器与系统状态。
type diagnosticsMeta struct {
	ExportedAt   string            `json:"exported_at"`
	AppVersion   string            `json:"app_version"`
	GoVersion    string            `json:"go_version"`
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	Root         string            `json:"root"`
	DataDir      string            `json:"data_dir"`
	Installed    []Version         `json:"installed"`
	Selected     string            `json:"selected"`
	Running      string            `json:"running"`
	Starting     bool              `json:"starting"`
	Stopping     bool              `json:"stopping"`
	RuntimeError string            `json:"runtime_error"`
	Job          Job               `json:"job"`
	Sources      diagnosticsSource `json:"sources"`
}

// diagnosticsSource 是去掉路径和参数后的下载源：诊断包不需要暴露自定义源的完整地址。
type diagnosticsSource struct {
	Preset string `json:"preset"`
	UV     string `json:"uv"`
	Python string `json:"python"`
	Index  string `json:"index"`
}

// diagnosticsPlugins 汇总插件启停状态和清单里的名字、版本。
type diagnosticsPlugins struct {
	State   json.RawMessage     `json:"state,omitempty"`
	Note    string              `json:"note,omitempty"`
	Plugins []diagnosticsPlugin `json:"plugins"`
}

// diagnosticsPlugin 是 <data_dir>/plugins/<目录>/plugin.toml 里的关键字段。
type diagnosticsPlugin struct {
	Dir     string `json:"dir"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// ExportDiagnostics 生成一个诊断压缩包并返回它的绝对路径。
// 内容：meta.json（启动器/系统/已装版本/最近错误）、启动器日志、
// 宿主 neobot.log 尾部、doctor 输出、pip check 输出、插件状态、磁盘信息。
// 绝对不允许打包：data/.env、data/.neobot/control.json、plugins_data/*/config.toml。
func (m *Manager) ExportDiagnostics() (string, error) {
	snapshot := m.Snapshot()
	entries := m.diagnosticsEntries(snapshot, m.healthTarget())

	dir := filepath.Join(m.root, "diagnostics")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", errors.New("无法创建诊断目录，请检查磁盘空间或目录权限后重试")
	}
	final := filepath.Join(dir, diagnosticsPrefix+time.Now().Format("20060102-150405")+".zip")
	file, err := os.CreateTemp(dir, ".diag-*.zip")
	if err != nil {
		return "", errors.New("无法创建诊断包，请检查磁盘空间后重试")
	}
	temp := file.Name()
	saved := false
	defer func() {
		// 中途失败（或还没 rename）就返回时，不能把半成品留在目录里。
		if !saved {
			_ = os.Remove(temp)
		}
	}()
	writer := zip.NewWriter(file)
	for _, entry := range entries {
		part, err := writer.CreateHeader(&zip.FileHeader{Name: entry.name, Method: zip.Deflate, Modified: time.Now()})
		if err == nil {
			_, err = part.Write(entry.data)
		}
		if err != nil {
			_ = writer.Close()
			_ = file.Close()
			return "", errors.New("无法写入诊断包，请检查磁盘剩余空间后重试")
		}
	}
	if err := writer.Close(); err != nil {
		_ = file.Close()
		return "", errors.New("无法写入诊断包，请检查磁盘剩余空间后重试")
	}
	if err := file.Close(); err != nil {
		return "", errors.New("无法保存诊断包，请检查磁盘剩余空间后重试")
	}
	if err := replaceFile(temp, final); err != nil {
		return "", errors.New("无法保存诊断包，请检查磁盘剩余空间后重试")
	}
	saved = true
	pruneDiagnosticsBundles(dir, final)
	m.logMessage("已导出诊断包：" + filepath.Base(final))
	if absolute, err := filepath.Abs(final); err == nil {
		return absolute, nil
	}
	return final, nil
}

// pruneDiagnosticsBundles 只保留最新几份诊断包，把上一次导出的旧包删掉。
// 清理失败一律放过：目录里多留一个旧包，也比让刚导出的诊断包因为清理出错而丢掉好。
func pruneDiagnosticsBundles(dir, keepPath string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type bundleFile struct {
		name string
		when int64
	}
	files := make([]bundleFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), diagnosticsPrefix) || !strings.HasSuffix(entry.Name(), ".zip") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, bundleFile{name: entry.Name(), when: info.ModTime().UnixNano()})
	}
	if len(files) <= diagnosticsKeepBundles {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].when > files[j].when })
	keep := filepath.Base(keepPath)
	for _, file := range files[diagnosticsKeepBundles:] {
		if file.name == keep {
			continue
		}
		_ = os.Remove(filepath.Join(dir, file.name))
	}
}

// diagnosticsEntries 收集要打包的内容。每一项都是尽力而为：读不到就留一条说明，
// 不让单个文件毁掉整包。
func (m *Manager) diagnosticsEntries(snapshot Snapshot, target healthTarget) []zipEntry {
	entries := []zipEntry{
		{name: "meta.json", data: diagnosticsJSON(m.diagnosticsMeta(snapshot))},
		{name: "system.txt", data: []byte(m.diagnosticsSystem(snapshot, target))},
	}
	entries = append(entries, m.diagnosticsLauncherLogs()...)
	entries = append(entries, m.diagnosticsHostLog())
	entries = append(entries,
		diagnosticsCommandEntry("doctor.txt", "python -m neobot_app.cli doctor", m.doctorCommand(target)),
		diagnosticsCommandEntry("pip-check.txt", "uv pip check --python <当前版本>/"+filepath.ToSlash(filepath.Join(venvBinDir, "python"+exeSuffix)), m.pipCheckCommand(target)),
		zipEntry{name: "plugins.json", data: diagnosticsJSON(m.diagnosticsPlugins(snapshot))},
	)
	return entries
}

func (m *Manager) diagnosticsMeta(snapshot Snapshot) diagnosticsMeta {
	return diagnosticsMeta{
		ExportedAt:   time.Now().Format(time.RFC3339),
		AppVersion:   CurrentVersion(),
		GoVersion:    runtime.Version(),
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Root:         m.root,
		DataDir:      snapshot.DataDir,
		Installed:    snapshot.Installed,
		Selected:     snapshot.Selected,
		Running:      snapshot.Running,
		Starting:     snapshot.Starting,
		Stopping:     snapshot.Stopping,
		RuntimeError: snapshot.RuntimeError,
		Job:          snapshot.Job,
		Sources: diagnosticsSource{
			Preset: snapshot.Sources.Preset,
			UV:     sourceEndpoint(snapshot.Sources.UV),
			Python: sourceEndpoint(snapshot.Sources.Python),
			Index:  sourceEndpoint(snapshot.Sources.Index),
		},
	}
}

// sourceEndpoint 只保留下载源的 scheme+host：路径和参数可能带账号信息，日志里不需要。
func sourceEndpoint(value string) string {
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// diagnosticsSystem 生成人读的系统信息：时间、时区、目录、磁盘占用。
func (m *Manager) diagnosticsSystem(snapshot Snapshot, target healthTarget) string {
	var b strings.Builder
	now := time.Now()
	zoneName, offset := now.Zone()
	fmt.Fprintf(&b, "导出时间：%s\n", now.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "时区：%s（%s，UTC%+d:%02d）\n", now.Location().String(), zoneName, offset/3600, absInt(offset%3600)/60)
	fmt.Fprintf(&b, "操作系统：%s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "Go 版本：%s\n", runtime.Version())
	fmt.Fprintf(&b, "启动器版本：%s\n", CurrentVersion())
	fmt.Fprintf(&b, "安装目录：%s\n", m.root)
	fmt.Fprintf(&b, "数据目录：%s\n", snapshot.DataDir)
	fmt.Fprintf(&b, "已安装版本：%d 个；选中：%s；运行中：%s\n",
		len(snapshot.Installed), textOr(snapshot.Selected, "无"), textOr(snapshot.Running, "无"))
	if snapshot.RuntimeError != "" {
		fmt.Fprintf(&b, "最近错误：%s\n", clipText(snapshot.RuntimeError, healthDetailLimit))
	}
	if target.usage.Error != "" {
		fmt.Fprintf(&b, "磁盘统计错误：%s\n", clipText(target.usage.Error, healthDetailLimit))
	}
	b.WriteString("\n磁盘占用：\n")
	if target.usage.Total > 0 {
		fmt.Fprintf(&b, "  合计：%s\n", byteSizeText(target.usage.Total))
		for _, item := range []struct {
			label string
			size  int64
		}{
			{"已安装版本", usageVersionsBytes(target.usage)},
			{"Python 运行时", target.usage.Python},
			{"uv 缓存", target.usage.Cache},
			{"备份快照", target.usage.Backups},
			{"数据目录", target.usage.Data},
			{"独立数据目录", target.usage.Isolated},
			{"日志", target.usage.Logs},
			{"临时文件", target.usage.Temp},
			{"下载工具", target.usage.Tools},
		} {
			if item.size > 0 {
				fmt.Fprintf(&b, "  %s：%s\n", item.label, byteSizeText(item.size))
			}
		}
		for _, version := range target.usage.Versions {
			fmt.Fprintf(&b, "    - %s：%s\n", textOr(version.Label, version.Name), byteSizeText(version.Bytes))
		}
	} else {
		// 还没扫过磁盘占用：现算一次安装根目录的大小（只用于诊断，慢一点没关系）。
		fmt.Fprintf(&b, "  合计：%s（导出时现算）\n", byteSizeText(diagnosticsDirSize(m.root)))
	}
	return b.String()
}

// diagnosticsLauncherLogs 打包最近的启动器日志，最多 3 个，每个最多 1MB。
func (m *Manager) diagnosticsLauncherLogs() []zipEntry {
	dir := m.launcherLogDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []zipEntry{{name: "launcher-logs/README.txt", data: []byte("无法读取启动器日志目录。\n")}}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasPrefix(name, launcherLogPrefix) && strings.HasSuffix(name, launcherLogSuffix) {
			names = append(names, name)
		}
	}
	// 文件名里是纳秒时间戳：按名字倒序就是从新到旧。
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if len(names) > diagnosticsMaxLogFiles {
		names = names[:diagnosticsMaxLogFiles]
	}
	result := make([]zipEntry, 0, len(names))
	for _, name := range names {
		if data, ok := readFileTail(filepath.Join(dir, name), diagnosticsLogLimit); ok {
			result = append(result, zipEntry{name: "launcher-logs/" + name, data: data})
		}
	}
	return result
}

// diagnosticsHostLog 打包宿主日志最后一个文件的尾部。
func (m *Manager) diagnosticsHostLog() zipEntry {
	notFound := func(message string) zipEntry {
		return zipEntry{name: "neobot-log.txt", data: []byte(message + "\n")}
	}
	files, err := m.HostLogFiles()
	if err != nil {
		return notFound("无法读取 NeoBot 日志目录。")
	}
	if len(files) == 0 {
		return notFound("还没有 NeoBot 日志。")
	}
	page, err := m.ReadHostLog(files[0].Name, -1, diagnosticsHostLines)
	if err != nil {
		return notFound("无法读取日志文件 " + files[0].Name + "。")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# 文件：%s（共 %d 行，这里只保留最后 %d 行）\n", page.File, page.Total, len(page.Lines))
	for _, line := range page.Lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return zipEntry{name: "neobot-log.txt", data: []byte(b.String())}
}

// diagnosticsCommandEntry 把体检用的命令输出写成文本。
func diagnosticsCommandEntry(name, title string, command stagedCommand) zipEntry {
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n", title)
	if command.Skipped != "" {
		fmt.Fprintf(&b, "未执行：%s\n", command.Skipped)
		return zipEntry{name: name, data: []byte(b.String())}
	}
	result := command.run()
	switch {
	case result.TimedOut:
		fmt.Fprintf(&b, "超时：超过 %s 没有结束。\n", command.Timeout)
	case result.Failed:
		b.WriteString("无法启动命令，当前版本的安装可能不完整。\n")
	default:
		fmt.Fprintf(&b, "退出码：%d\n", result.ExitCode)
	}
	b.WriteString("\n")
	b.WriteString(result.Output)
	if !strings.HasSuffix(result.Output, "\n") {
		b.WriteString("\n")
	}
	return zipEntry{name: name, data: []byte(b.String())}
}

// diagnosticsPlugins 读取插件启停状态原文和每个插件的名字、版本。
func (m *Manager) diagnosticsPlugins(snapshot Snapshot) diagnosticsPlugins {
	result := diagnosticsPlugins{Plugins: []diagnosticsPlugin{}}
	if data, err := readFileHead(filepath.Join(snapshot.DataDir, "plugin_state.json"), diagnosticsStateLimit); err == nil {
		if json.Valid(data) {
			result.State = json.RawMessage(data)
		} else {
			result.Note = "plugin_state.json 不是有效的 JSON，已跳过内容。"
		}
	}
	pluginDir := filepath.Join(snapshot.DataDir, "plugins")
	entries, err := os.ReadDir(pluginDir)
	if err != nil {
		return result
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifest, err := readFileHead(filepath.Join(pluginDir, entry.Name(), "plugin.toml"), diagnosticsManifestLimit)
		if err != nil {
			continue
		}
		text := string(manifest)
		result.Plugins = append(result.Plugins, diagnosticsPlugin{
			Dir: entry.Name(), Name: tomlStringValue(text, "name"), Version: tomlStringValue(text, "version"),
		})
	}
	sort.Slice(result.Plugins, func(i, j int) bool { return result.Plugins[i].Dir < result.Plugins[j].Dir })
	return result
}

// tomlStringValue 从 plugin.toml 里取一个字符串字段。
// 诊断包只展示插件名和版本，不值得为它引入 TOML 解析依赖；取不到就留空。
func tomlStringValue(manifest, key string) string {
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		rest, found := strings.CutPrefix(line, key)
		if !found {
			continue
		}
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "=") {
			continue
		}
		value := strings.TrimSpace(rest[1:])
		if len(value) < 2 {
			continue
		}
		quote := value[0]
		if quote != '"' && quote != '\'' {
			continue
		}
		if end := strings.IndexByte(value[1:], quote); end >= 0 {
			return strings.TrimSpace(value[1 : 1+end])
		}
	}
	return ""
}

// diagnosticsJSON 把结构体编码成带缩进的 JSON，方便用户直接打开看。
func diagnosticsJSON(value any) []byte {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return []byte("{}\n")
	}
	return append(data, '\n')
}

// readFileHead 读取文件开头最多 limit 字节；文件更大时只保留开头（清单足够用）。
func readFileHead(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit))
}

// readFileTail 读取文件最多 limit 字节；文件更大时只保留末尾（新的日志更有用）。
func readFileTail(path string, limit int64) ([]byte, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	if info.Size() > limit {
		if _, err := file.Seek(info.Size()-limit, io.SeekStart); err != nil {
			return nil, false
		}
		data, err := io.ReadAll(io.LimitReader(file, limit))
		if err != nil {
			return nil, false
		}
		header := fmt.Sprintf("# 日志较大，仅保留最后 %s（原文件 %d 字节）\n", byteSizeText(limit), info.Size())
		return append([]byte(header), data...), true
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return nil, false
	}
	return data, true
}

// diagnosticsDirSize 统计目录总大小；读不到的条目直接跳过，只用于诊断展示。
func diagnosticsDirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// usageVersionsBytes 汇总已安装版本占用的空间。
func usageVersionsBytes(usage Usage) int64 {
	var total int64
	for _, version := range usage.Versions {
		total += version.Bytes
	}
	return total
}

// textOr 在值为空时返回 fallback，用于拼可读文本。
func textOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// absInt 返回绝对值，用于格式化时区偏移的分钟数。
func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
