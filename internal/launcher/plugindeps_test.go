package launcher

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPluginRequirementsSkipsDisabledAndDuplicates(t *testing.T) {
	found := []pluginDependency{
		{Name: "b_plugin", Missing: []string{"feedparser>=6.0,<7", "pydantic>=2,<3"}},
		{Name: "a_plugin", Missing: []string{"akshare>=1.20.34,<2", "pydantic>=2,<3"}},
		{Name: "off_plugin", Missing: []string{"lxml>=5,<7"}},
	}
	requirements, names := pluginRequirements(found, map[string]bool{"off_plugin": true})
	if got, want := strings.Join(names, ","), "a_plugin,b_plugin"; got != want {
		t.Fatalf("plugin names = %q; want %q", got, want)
	}
	want := []string{"akshare>=1.20.34,<2", "pydantic>=2,<3", "feedparser>=6.0,<7"}
	if !reflect.DeepEqual(requirements, want) {
		t.Fatalf("requirements = %q; want %q", requirements, want)
	}
}

func TestPluginRequirementsEmptyWhenAllDisabled(t *testing.T) {
	found := []pluginDependency{{Name: "off", Missing: []string{"lxml>=5,<7"}}}
	requirements, names := pluginRequirements(found, map[string]bool{"off": true})
	if len(requirements) != 0 || len(names) != 0 {
		t.Fatalf("disabled plugin must not be installed: %q %q", requirements, names)
	}
}

func TestParsePluginDependencies(t *testing.T) {
	report, err := parsePluginDependencies([]byte(`{"packaging":true,"plugins":[{"name":"x","missing":["a>=1"]}]}`))
	if err != nil || !report.Packaging || len(report.Plugins) != 1 ||
		report.Plugins[0].Name != "x" || report.Plugins[0].Missing[0] != "a>=1" {
		t.Fatalf("parse = %+v, %v", report, err)
	}
	report, err = parsePluginDependencies([]byte(`{"packaging":false,"plugins":[]}`))
	if err != nil || report.Packaging || len(report.Plugins) != 0 {
		t.Fatalf("packaging 不可用时应保留标志：%+v, %v", report, err)
	}
	for _, raw := range []string{"", "   ", "not json", `{"plugins":1}`} {
		if _, err := parsePluginDependencies([]byte(raw)); err == nil {
			t.Fatalf("expected an error for %q", raw)
		}
	}
}

func TestDisabledPluginsReadsHostState(t *testing.T) {
	data := t.TempDir()
	state := `{"version":1,"plugins":{"off":{"enabled":false},"on":{"enabled":true}}}`
	if err := os.WriteFile(filepath.Join(data, "plugin_state.json"), []byte(state), 0600); err != nil {
		t.Fatal(err)
	}
	disabled := disabledPlugins(data)
	if !disabled["off"] || disabled["on"] {
		t.Fatalf("disabled = %v", disabled)
	}
	if got := disabledPlugins(t.TempDir()); len(got) != 0 {
		t.Fatalf("missing state file must disable nothing: %v", got)
	}
}

func TestSyncPluginDependenciesSkipsWithoutInstall(t *testing.T) {
	root := t.TempDir()
	m := &Manager{root: root, state: diskState{Backups: map[string]string{}}}
	data := filepath.Join(root, "data")
	// 没有解释器：什么都不做，也不写日志。
	m.syncPluginDependencies("1.0.0", data, Sources{Index: officialIndex})
	if lines := m.Logs(0).Lines; len(lines) != 0 {
		t.Fatalf("unexpected log lines: %q", lines)
	}
	// 有解释器但没有插件目录：同样什么都不做。
	python := venvPython(m.versionDir("1.0.0"))
	if err := os.MkdirAll(filepath.Dir(python), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(python, nil, 0600); err != nil {
		t.Fatal(err)
	}
	m.syncPluginDependencies("1.0.0", data, Sources{Index: officialIndex})
	if lines := m.Logs(0).Lines; len(lines) != 0 {
		t.Fatalf("unexpected log lines: %q", lines)
	}
}

func TestHasPluginManifests(t *testing.T) {
	dir := t.TempDir()
	if hasPluginManifests(dir) {
		t.Fatal("empty directory must not look like it holds plugins")
	}
	if err := os.MkdirAll(filepath.Join(dir, "demo"), 0700); err != nil {
		t.Fatal(err)
	}
	if hasPluginManifests(dir) {
		t.Fatal("a directory without plugin.toml is not a plugin")
	}
	if err := os.WriteFile(filepath.Join(dir, "demo", "plugin.toml"), []byte("name = \"demo\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if !hasPluginManifests(dir) {
		t.Fatal("plugin.toml must be detected")
	}
	if hasPluginManifests(filepath.Join(dir, "missing")) {
		t.Fatal("missing directory must not hold plugins")
	}
}

func TestSyncPluginDependenciesReportsProbeFailure(t *testing.T) {
	root := t.TempDir()
	m := &Manager{root: root, state: diskState{Backups: map[string]string{}}}
	data := filepath.Join(root, "data")
	python := venvPython(m.versionDir("1.0.0"))
	if err := os.MkdirAll(filepath.Dir(python), 0700); err != nil {
		t.Fatal(err)
	}
	// 空文件不是可执行程序：探测必然失败，但启动流程不能因此中断。
	if err := os.WriteFile(python, nil, 0700); err != nil {
		t.Fatal(err)
	}
	pluginDir := filepath.Join(data, "plugins", "demo")
	if err := os.MkdirAll(pluginDir, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := "name = \"demo\"\npython_dependencies = [\"feedparser>=6.0,<7\"]\n"
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.toml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	m.syncPluginDependencies("1.0.0", data, Sources{Index: officialIndex})
	lines := m.Logs(0).Lines
	if len(lines) != 1 || !strings.Contains(lines[0], "无法检查插件依赖") {
		t.Fatalf("log lines = %q", lines)
	}
}

// TestPluginDependencyProbeRunsAgainstHost 需要真实的 NeoBot 环境，默认跳过：
//
//	$env:NEOBOT_TEST_PYTHON = "$env:LOCALAPPDATA\NeoBotDesktop\versions\<版本>\Scripts\python.exe"
//	$env:NEOBOT_TEST_PLUGIN_DIR = "$env:LOCALAPPDATA\NeoBotDesktop\data\plugins"
//	go test ./internal/launcher -run TestPluginDependencyProbeRunsAgainstHost -v
func TestPluginDependencyProbeRunsAgainstHost(t *testing.T) {
	python := os.Getenv("NEOBOT_TEST_PYTHON")
	pluginDir := os.Getenv("NEOBOT_TEST_PLUGIN_DIR")
	if python == "" || pluginDir == "" {
		t.Skip("set NEOBOT_TEST_PYTHON and NEOBOT_TEST_PLUGIN_DIR to run against a real installation")
	}
	report, err := probePluginDependencies(python, pluginDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("probe = %+v", report)
}
