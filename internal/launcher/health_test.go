package launcher

// 体检与诊断包的测试。这里只覆盖能离线稳定复现的部分：真正的 uv/pip/doctor
// 需要完整安装环境，靠人工验证。

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// healthTestWrite 写一个测试文件，自动建好上级目录。
func healthTestWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckInstalledFilesReportsMissingBinaries(t *testing.T) {
	root := t.TempDir()
	m := &Manager{root: root, installed: map[string]Version{
		"1.0.0": {Name: "1.0.0"},
		"1.1.0": {Name: "1.1.0"},
	}}
	for _, version := range []string{"1.0.0", "1.1.0"} {
		healthTestWrite(t, venvPython(filepath.Join(root, "versions", version)), "")
	}
	healthTestWrite(t, botExecutable(filepath.Join(root, "versions", "1.0.0")), "")

	state, detail, hint := m.checkInstalledFiles(m.healthTarget())
	if state != "fail" || !strings.Contains(detail, "1.1.0 的 "+filepath.Base(botExecutable(""))) || hint == "" {
		t.Fatalf("state = %q，detail = %q，hint = %q", state, detail, hint)
	}

	healthTestWrite(t, botExecutable(filepath.Join(root, "versions", "1.1.0")), "")
	if state, _, _ = m.checkInstalledFiles(m.healthTarget()); state != "ok" {
		t.Fatalf("补齐后 state = %q", state)
	}
}

func TestCheckInstalledFilesSkipsWithoutVersions(t *testing.T) {
	m := &Manager{root: t.TempDir()}
	state, detail, hint := m.checkInstalledFiles(m.healthTarget())
	if state != "skip" || detail == "" || hint == "" {
		t.Fatalf("state = %q，detail = %q，hint = %q", state, detail, hint)
	}
}

func TestHealthTargetUsesRunningVersionDataDir(t *testing.T) {
	root := t.TempDir()
	m := &Manager{
		root:      root,
		state:     diskState{Selected: "1.0.0", IsolateData: true},
		installed: map[string]Version{"2.0.0": {Name: "2.0.0"}},
		bot:       &botProcess{version: "2.0.0"},
	}
	target := m.healthTarget()
	if !target.running || target.bot == nil {
		t.Fatal("运行状态没有带进体检上下文")
	}
	if want := filepath.Join(root, "isolated", "2.0.0"); target.dataDir != want {
		t.Fatalf("dataDir = %q，期望 %q", target.dataDir, want)
	}
	if target.version != "2.0.0" {
		t.Fatalf("体检版本 = %q，期望运行中的 2.0.0", target.version)
	}
	if len(target.versions) != 1 || target.versions[0].Name != "2.0.0" {
		t.Fatalf("versions = %+v", target.versions)
	}
}

func TestHealthTargetUsesSelectedVersionWhenStopped(t *testing.T) {
	root := t.TempDir()
	m := &Manager{
		root:  root,
		state: diskState{Selected: "1.0.0", IsolateData: true},
	}
	target := m.healthTarget()
	if target.version != "1.0.0" {
		t.Fatalf("体检版本 = %q，期望选中的 1.0.0", target.version)
	}
	if want := filepath.Join(root, "isolated", "1.0.0"); target.dataDir != want {
		t.Fatalf("dataDir = %q，期望 %q", target.dataDir, want)
	}
}

func TestCheckDataDirProbesWithoutCreatingDirectory(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	m := &Manager{root: root}
	state, detail, _ := m.checkDataDir(healthTarget{dataDir: data})
	if state != "ok" || !strings.Contains(detail, "可写") {
		t.Fatalf("state = %q，detail = %q", state, detail)
	}
	if entries, err := os.ReadDir(data); err != nil || len(entries) != 0 {
		t.Fatalf("体检不应留下文件：%v, %v", entries, err)
	}

	// 数据目录还没创建时不能替用户建目录，只能在上级目录里试写。
	missing := filepath.Join(root, "isolated", "9.9.9")
	state, detail, _ = m.checkDataDir(healthTarget{dataDir: missing})
	if state != "ok" || !strings.Contains(detail, "还没有创建") {
		t.Fatalf("state = %q，detail = %q", state, detail)
	}
	if isDir(missing) {
		t.Fatal("体检不应创建数据目录")
	}
}

func TestHealthCommandsSkipWithoutSelectedVersion(t *testing.T) {
	m := &Manager{root: t.TempDir()}
	if command := m.doctorCommand(healthTarget{}); command.Skipped == "" {
		t.Fatal("没有选中版本时 doctor 应跳过")
	}
	if command := m.pipCheckCommand(healthTarget{version: "1.0.0"}); command.Skipped == "" {
		t.Fatal("缺少 python.exe 时 pip check 应跳过")
	}

	// 有 python.exe 但没有 uv：pip check 依然跳过，而 doctor 应该可以组装出来。
	root := t.TempDir()
	m = &Manager{root: root}
	healthTestWrite(t, venvPython(filepath.Join(root, "versions", "1.0.0")), "")
	if command := m.pipCheckCommand(healthTarget{version: "1.0.0"}); command.Skipped == "" {
		t.Fatal("没有 uv 时 pip check 应跳过")
	}
	command := m.doctorCommand(healthTarget{version: "1.0.0", dataDir: filepath.Join(root, "data")})
	if command.Skipped != "" || command.Path == "" {
		t.Fatalf("doctor 命令 = %+v", command)
	}
	// 数据目录不存在时，命令的工作目录要退回安装根目录，否则命令起不来。
	if command.Dir != root {
		t.Fatalf("dir = %q，期望 %q", command.Dir, root)
	}
	if !strings.Contains(strings.Join(command.Env, " "), "PYTHONUTF8=1") {
		t.Fatalf("env 里缺少 PYTHONUTF8：%v", command.Env)
	}
}

func TestPluginDependencyHealthReportsMissing(t *testing.T) {
	report := pluginDependencyReport{
		Packaging: true,
		Plugins: []pluginDependency{
			{Name: "demo", Missing: []string{"feedparser>=6.0,<7", "lxml>=5,<7"}},
			{Name: "disabled", Missing: []string{"akshare>=1.20,<2"}},
		},
	}
	state, detail, hint := pluginDependencyHealth(report, map[string]bool{"disabled": true})
	if state != "warn" || !strings.Contains(detail, "demo 缺少 feedparser>=6.0,<7、lxml>=5,<7") ||
		strings.Contains(detail, "akshare") || hint == "" {
		t.Fatalf("state = %q，detail = %q，hint = %q", state, detail, hint)
	}
}

func TestPluginDependencyHealthOKWhenSatisfied(t *testing.T) {
	state, detail, hint := pluginDependencyHealth(pluginDependencyReport{Packaging: true}, nil)
	if state != "ok" || detail == "" || hint != "" {
		t.Fatalf("state = %q，detail = %q，hint = %q", state, detail, hint)
	}
}

func TestPluginDependencyHealthWarnsWithoutPackaging(t *testing.T) {
	report := pluginDependencyReport{
		Packaging: false,
		Plugins:   []pluginDependency{{Name: "demo", Missing: []string{"feedparser>=6.0,<7"}}},
	}
	state, detail, hint := pluginDependencyHealth(report, nil)
	if state != "warn" || !strings.Contains(detail, "packaging") || hint == "" {
		t.Fatalf("state = %q，detail = %q，hint = %q", state, detail, hint)
	}
}

func TestPluginDependencyHealthIgnoresDisabledWithoutPackaging(t *testing.T) {
	report := pluginDependencyReport{
		Packaging: false,
		Plugins:   []pluginDependency{{Name: "off", Missing: []string{"feedparser>=6.0,<7"}}},
	}
	if state, detail, hint := pluginDependencyHealth(report, map[string]bool{"off": true}); state != "ok" || detail == "" || hint != "" {
		t.Fatalf("停用插件不该触发提示：state = %q，detail = %q，hint = %q", state, detail, hint)
	}
}

func TestCheckPluginDependenciesSkipsWithoutVersionOrPlugins(t *testing.T) {
	m := &Manager{root: t.TempDir()}
	if state, _, _ := m.checkPluginDependencies(m.healthTarget()); state != "skip" {
		t.Fatalf("没有版本时 state = %q，应为 skip", state)
	}

	m.state.Selected = "1.0.0"
	healthTestWrite(t, venvPython(filepath.Join(m.root, "versions", "1.0.0")), "")
	state, detail, _ := m.checkPluginDependencies(m.healthTarget())
	if state != "skip" || !strings.Contains(detail, "插件") {
		t.Fatalf("没有插件时 state = %q，detail = %q", state, detail)
	}
}

func TestCheckPluginDependenciesReportsProbeFailureReadOnly(t *testing.T) {
	root := t.TempDir()
	m := &Manager{root: root, state: diskState{Selected: "1.0.0"}}
	healthTestWrite(t, venvPython(filepath.Join(root, "versions", "1.0.0")), "")
	manifest := filepath.Join(root, "data", "plugins", "demo", "plugin.toml")
	healthTestWrite(t, manifest, "name = \"demo\"\npython_dependencies = [\"feedparser>=6.0,<7\"]\n")

	state, detail, hint := m.checkPluginDependencies(m.healthTarget())
	if state != "warn" || !strings.Contains(detail, "无法检查插件依赖") || hint == "" {
		t.Fatalf("state = %q，detail = %q，hint = %q", state, detail, hint)
	}
	// 体检必须只读：探测失败也不能动插件目录。
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("插件清单被体检改动了：%v", err)
	}
	if entries, err := os.ReadDir(filepath.Dir(manifest)); err != nil || len(entries) != 1 {
		t.Fatalf("插件目录内容 = %v, %v", entries, err)
	}
}

func TestHealthCommandsUseRunningVersion(t *testing.T) {
	root := t.TempDir()
	m := &Manager{root: root, state: diskState{Selected: "1.0.0"}}
	selectedPython := venvPython(filepath.Join(root, "versions", "1.0.0"))
	runningPython := venvPython(filepath.Join(root, "versions", "2.0.0"))
	healthTestWrite(t, selectedPython, "")
	healthTestWrite(t, runningPython, "")
	healthTestWrite(t, uvExecutable(root), "")
	m.bot = &botProcess{version: "2.0.0"}

	target := m.healthTarget()
	pip := m.pipCheckCommand(target)
	if pip.Skipped != "" || !strings.Contains(strings.Join(pip.Args, " "), runningPython) {
		t.Fatalf("pip check 应使用运行中的 2.0.0：%+v", pip)
	}
	doctor := m.doctorCommand(target)
	if doctor.Skipped != "" || !strings.EqualFold(doctor.Path, runningPython) {
		t.Fatalf("doctor 应使用运行中的 2.0.0：%+v", doctor)
	}
	if strings.Contains(strings.Join(pip.Args, " "), selectedPython) {
		t.Fatalf("pip check 不应使用选中的 1.0.0：%+v", pip)
	}
}

func TestRefreshHealthRejectsConcurrentRun(t *testing.T) {
	m := &Manager{root: t.TempDir()}
	m.healthBusy = true
	if err := m.RefreshHealth(); err == nil {
		t.Fatal("体检进行中时再次体检应该报错")
	}
	m.healthBusy = false
	m.closing = true
	if err := m.RefreshHealth(); err == nil {
		t.Fatal("关闭中的启动器不应该开始体检")
	}
}

func TestRefreshHealthCollectsEveryCheck(t *testing.T) {
	m := &Manager{root: t.TempDir()}
	if err := m.RefreshHealth(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		m.mu.Lock()
		health, busy := m.health, m.healthBusy
		m.mu.Unlock()
		if !busy {
			want := []string{"env", "pipcheck", "plugindeps", "doctor", "control", "data", "disk"}
			if len(health.Items) != len(want) {
				t.Fatalf("体检项 = %+v", health.Items)
			}
			for index, item := range health.Items {
				if item.Key != want[index] || item.Title == "" {
					t.Fatalf("第 %d 项 = %+v", index, item)
				}
				switch item.State {
				case "ok", "warn", "fail", "skip":
				default:
					t.Fatalf("第 %d 项状态非法：%q", index, item.State)
				}
			}
			if health.Running || health.At == 0 {
				t.Fatalf("结束状态不对：%+v", health)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("体检没有在 30 秒内结束")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSourceEndpointDropsPaths(t *testing.T) {
	cases := map[string]string{
		"https://mirror.example.com/github-release/astral-sh?token=abc": "https://mirror.example.com",
		"https://pypi.example.com:8443/simple":                          "https://pypi.example.com:8443",
		"":                                                              "",
		"not a url":                                                     "",
		"/only/path":                                                    "",
	}
	for value, want := range cases {
		if got := sourceEndpoint(value); got != want {
			t.Fatalf("sourceEndpoint(%q) = %q，期望 %q", value, got, want)
		}
	}
}

func TestTomlStringValueReadsManifest(t *testing.T) {
	manifest := "name2 = \"other\"\nname = \"demo\"\nversion = '1.2.3'\n[dependencies]\nname = \"ignored\"\n"
	if got := tomlStringValue(manifest, "name"); got != "demo" {
		t.Fatalf("name = %q", got)
	}
	if got := tomlStringValue(manifest, "version"); got != "1.2.3" {
		t.Fatalf("version = %q", got)
	}
	if got := tomlStringValue("description = \"no name\"\n", "name"); got != "" {
		t.Fatalf("缺失字段应为空，得到 %q", got)
	}
}

func TestExportDiagnosticsPacksOnlySafeFiles(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	// 这些文件里都有密钥或账号信息，绝不允许进诊断包。
	healthTestWrite(t, filepath.Join(data, ".env"), "API_KEY=secret\n")
	healthTestWrite(t, filepath.Join(data, ".neobot", "control.json"), "{\"token\":\"secret\"}\n")
	healthTestWrite(t, filepath.Join(data, "plugins_data", "dashboard", "config.toml"), "host = \"secret\"\n")
	healthTestWrite(t, filepath.Join(root, "settings.json"), "{\"selected\":\"1.0.0\",\"note\":\"secret\"}\n")

	// 允许打包的内容。
	healthTestWrite(t, filepath.Join(data, "logs", "neobot.log"), "host log line\n")
	healthTestWrite(t, filepath.Join(data, "plugin_state.json"), "{\"plugins\":{\"demo\":{\"enabled\":true}}}\n")
	healthTestWrite(t, filepath.Join(data, "plugins", "demo", "plugin.toml"), "name = \"demo\"\nversion = \"1.2.3\"\n")
	healthTestWrite(t, filepath.Join(root, "logs", "launcher-1700000000000000001.log"), "launcher line\n")

	m := &Manager{
		root:      root,
		state:     diskState{Selected: "1.0.0", Backups: map[string]string{}, Sources: Sources{Preset: "custom", UV: "https://uv.example.com/releases/download", Python: "https://py.example.com/mirror", Index: "https://pypi.example.com/simple"}},
		installed: map[string]Version{"1.0.0": {Name: "1.0.0"}},
	}
	out, err := m.ExportDiagnostics()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(out) || !strings.HasPrefix(filepath.Base(out), "neobot-desktop-diag-") ||
		!strings.HasSuffix(out, ".zip") {
		t.Fatalf("诊断包路径 = %q", out)
	}
	if filepath.Dir(out) != filepath.Join(root, "diagnostics") {
		t.Fatalf("诊断包目录 = %q", filepath.Dir(out))
	}
	// 只能留下最终的 zip，临时文件必须清掉。
	if entries, err := os.ReadDir(filepath.Dir(out)); err != nil || len(entries) != 1 {
		t.Fatalf("诊断目录内容 = %v, %v", entries, err)
	}

	reader, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	packed := map[string]string{}
	for _, file := range reader.File {
		body, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil {
			t.Fatal(err)
		}
		packed[file.Name] = string(content)
	}
	for _, want := range []string{
		"meta.json", "system.txt", "doctor.txt", "pip-check.txt", "plugins.json", "neobot-log.txt",
		"launcher-logs/launcher-1700000000000000001.log",
	} {
		if _, ok := packed[want]; !ok {
			t.Fatalf("诊断包缺少 %s，实际有 %v", want, mapKeysOf(packed))
		}
	}
	for name, content := range packed {
		for _, forbidden := range []string{".env", "control.json", "plugins_data", "settings.json"} {
			if strings.Contains(name, forbidden) {
				t.Fatalf("%s 不该进诊断包", name)
			}
		}
		if strings.Contains(content, "secret") {
			t.Fatalf("%s 泄露了敏感内容", name)
		}
	}

	var meta struct {
		AppVersion string `json:"app_version"`
		GoVersion  string `json:"go_version"`
		Sources    struct {
			Preset string `json:"preset"`
			UV     string `json:"uv"`
			Python string `json:"python"`
			Index  string `json:"index"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(packed["meta.json"]), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.AppVersion == "" || meta.GoVersion == "" {
		t.Fatalf("meta.json = %+v", meta)
	}
	// 自定义下载源只留 scheme+host。
	if meta.Sources.Preset != "custom" || meta.Sources.UV != "https://uv.example.com" ||
		meta.Sources.Python != "https://py.example.com" || meta.Sources.Index != "https://pypi.example.com" {
		t.Fatalf("sources = %+v", meta.Sources)
	}

	var plugins struct {
		State   json.RawMessage `json:"state"`
		Plugins []struct {
			Dir     string `json:"dir"`
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(packed["plugins.json"]), &plugins); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(plugins.State) || len(plugins.Plugins) != 1 ||
		plugins.Plugins[0].Dir != "demo" || plugins.Plugins[0].Name != "demo" || plugins.Plugins[0].Version != "1.2.3" {
		t.Fatalf("plugins.json = %s", packed["plugins.json"])
	}
	if !strings.Contains(packed["neobot-log.txt"], "host log line") {
		t.Fatalf("neobot-log.txt = %q", packed["neobot-log.txt"])
	}
	if !strings.Contains(packed["doctor.txt"], "未执行") {
		t.Fatalf("doctor.txt = %q", packed["doctor.txt"])
	}
}

// 反复导出只留最新诊断包：旧包按文件名排序排在前面，最新的一份在最后，
// 不清理就得先翻过一堆旧包才能找到刚生成的那份。
func TestPruneDiagnosticsBundlesKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	names := make([]string, 5)
	for i := range names {
		names[i] = fmt.Sprintf("neobot-desktop-diag-2026010%d-000000.zip", i+1)
		path := filepath.Join(dir, names[i])
		healthTestWrite(t, path, "bundle")
		stamp := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatalf("调整修改时间失败：%v", err)
		}
	}
	// 导出中途留下的临时文件和别的东西都不归它管。
	healthTestWrite(t, filepath.Join(dir, ".diag-123.zip"), "temp")
	healthTestWrite(t, filepath.Join(dir, "notes.txt"), "note")

	pruneDiagnosticsBundles(dir, filepath.Join(dir, names[len(names)-1]))

	for i, name := range names {
		_, err := os.Stat(filepath.Join(dir, name))
		if exists, want := err == nil, i >= len(names)-diagnosticsKeepBundles; exists != want {
			t.Fatalf("%s 存在 = %v，应为 %v", name, exists, want)
		}
	}
	for _, name := range []string{".diag-123.zip", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s 不应被清理：%v", name, err)
		}
	}
}

// mapKeysOf 只在断言失败时用来打印诊断包内容。
func mapKeysOf(packed map[string]string) []string {
	keys := make([]string, 0, len(packed))
	for key := range packed {
		keys = append(keys, key)
	}
	return keys
}
