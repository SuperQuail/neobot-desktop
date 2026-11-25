package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// requireTerminalSupport 在当前平台没有高级终端时跳过相关用例。
func requireTerminalSupport(t *testing.T) {
	t.Helper()
	if !terminalSupported() {
		t.Skip("当前平台不支持高级终端")
	}
}

func TestTerminalTargetPriority(t *testing.T) {
	m := &Manager{
		root:      t.TempDir(),
		installed: map[string]Version{"1.0.0": {Name: "1.0.0"}, "1.1.0": {Name: "1.1.0"}},
		state:     diskState{Selected: "1.0.0"},
	}
	if target, err := m.terminalTargetLocked(""); err != nil || target != "1.0.0" {
		t.Fatalf("selected target = %q, %v; want 1.0.0", target, err)
	}
	m.bot = &botProcess{version: "1.1.0"}
	if target, err := m.terminalTargetLocked(""); err != nil || target != "1.1.0" {
		t.Fatalf("running target = %q, %v; want 1.1.0", target, err)
	}
	if target, err := m.terminalTargetLocked("1.0.0"); err != nil || target != "1.0.0" {
		t.Fatalf("explicit target = %q, %v; want 1.0.0", target, err)
	}
	if _, err := m.terminalTargetLocked("9.9.9"); err == nil {
		t.Fatal("accepted an uninstalled version")
	}
	m.bot = nil
	m.state.Selected = ""
	delete(m.installed, "1.0.0")
	if target, err := m.terminalTargetLocked(""); err != nil || target != "1.1.0" {
		t.Fatalf("single installed target = %q, %v; want 1.1.0", target, err)
	}
	delete(m.installed, "1.1.0")
	if _, err := m.terminalTargetLocked(""); err == nil {
		t.Fatal("accepted an empty installation")
	}
}

func TestTerminalInfoHasNoSecrets(t *testing.T) {
	requireTerminalSupport(t)
	m := versionsTestManager(t)
	dir := versionsInstall(t, m, "1.0.0")
	m.state.Selected = "1.0.0"
	info := m.terminalInfoLocked()
	if !info.Available || info.Version != "1.0.0" || info.Label != "1.0.0" {
		t.Fatalf("terminal info = %+v", info)
	}
	if info.Venv != dir || info.Python != venvPython(filepath.Join(dir)) {
		t.Fatalf("terminal paths = %+v", info)
	}
	if info.Data != filepath.Join(m.root, "data") {
		t.Fatalf("data dir = %q", info.Data)
	}
	if info.UV != uvExecutable(m.root) {
		t.Fatalf("uv path = %q", info.UV)
	}
	if strings.Contains(info.Venv+info.Data+info.UV, ".env") {
		t.Fatalf("terminal info leaked a secret file path: %+v", info)
	}
}

func TestTerminalEnvIsCleanAndScoped(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("VIRTUAL_ENV", "/old/venv")
	t.Setenv("UV_CACHE_DIR", "/old/cache")
	t.Setenv("NEOBOT_DATA_DIR", "/old/data")
	t.Setenv("PIP_INDEX_URL", "https://old.example/simple")
	t.Setenv("PROMPT", "old")
	root := t.TempDir()
	data := filepath.Join(root, "data")
	venv := filepath.Join(root, "versions", "1.0.0")
	index := "https://pypi.tuna.tsinghua.edu.cn/simple"
	mirror := "https://mirror.example/python"
	entries := terminalEnv(root, data, venv, Sources{Index: index, Python: mirror})
	values := envMap(entries)

	pathCount := 0
	for _, entry := range entries {
		if name, _, _ := strings.Cut(entry, "="); strings.EqualFold(name, "PATH") {
			pathCount++
		}
	}
	if pathCount != 1 {
		t.Fatalf("PATH appears %d times; want exactly one", pathCount)
	}
	if values["PATH"] == "" || !strings.HasPrefix(values["PATH"], venvBin(venv)) {
		t.Fatalf("PATH = %q; venv must come first", values["PATH"])
	}
	if !strings.Contains(values["PATH"], filepath.Join(root, "tools")) {
		t.Fatalf("PATH = %q; tools directory missing", values["PATH"])
	}
	want := map[string]string{
		"VIRTUAL_ENV":              venv,
		"PYTHONUTF8":               "1",
		"NEOBOT_DATA_DIR":          data,
		"NEOBOT_ENV_FILE":          filepath.Join(data, ".env"),
		"UV_CACHE_DIR":             filepath.Join(root, "cache"),
		"UV_PYTHON_INSTALL_DIR":    filepath.Join(root, "python"),
		"UV_PYTHON_PREFERENCE":     "only-managed",
		"UV_DEFAULT_INDEX":         index,
		"PIP_INDEX_URL":            index,
		"UV_PYTHON_INSTALL_MIRROR": mirror,
	}
	if runtime.GOOS == "windows" {
		want["NODEFAULTCURRENTDIRECTORYINEXEPATH"] = "1"
	} else {
		want["PS1"] = "[NeoBot 1.0.0] \\w \\$ "
		want["PROMPT"] = "[NeoBot 1.0.0] \\w \\$ "
	}
	for name, value := range want {
		if values[name] != value {
			t.Fatalf("%s = %q; want %q", name, values[name], value)
		}
	}
	for _, name := range []string{"VIRTUAL_ENV", "UV_CACHE_DIR", "NEOBOT_DATA_DIR", "PROMPT"} {
		if strings.Contains(values[name], "old") {
			t.Fatalf("%s leaked an inherited value: %q", name, values[name])
		}
	}
	if !strings.Contains(values["PROMPT"], "[NeoBot 1.0.0]") {
		t.Fatalf("PROMPT = %q", values["PROMPT"])
	}
	if _, ok := values["UV_NO_CONFIG"]; ok {
		t.Fatal("UV_NO_CONFIG must not be set in the user terminal")
	}
}

func TestWriteUVShimTracksVersionedBinary(t *testing.T) {
	requireTerminalSupport(t)
	m := versionsTestManager(t)
	if err := m.writeUVShim(); err == nil {
		t.Fatal("wrote a shim without uv")
	}
	tools := filepath.Join(m.root, "tools")
	if err := os.MkdirAll(tools, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uvExecutable(m.root), []byte("stub"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.writeUVShim(); err != nil {
		t.Fatal(err)
	}
	shimName, wantContent, mode, err := uvShim(uvExecutable(m.root))
	if err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(tools, shimName)
	content, err := os.ReadFile(shim)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(wantContent) || !strings.Contains(string(content), filepath.Base(uvExecutable(m.root))) {
		t.Fatalf("shim = %q", content)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(shim); err != nil || info.Mode().Perm() != mode.Perm() {
			t.Fatalf("shim mode = %v, %v; want %v", info, err, mode)
		}
	}
}

func TestOpenTerminalRequiresAcknowledgmentAndPersists(t *testing.T) {
	requireTerminalSupport(t)
	m := versionsTestManager(t)
	versionsInstall(t, m, "1.0.0")
	m.state.Selected = "1.0.0"
	tools := filepath.Join(m.root, "tools")
	if err := os.MkdirAll(tools, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uvExecutable(m.root), []byte("stub"), 0600); err != nil {
		t.Fatal(err)
	}

	calls := 0
	var gotDir string
	var gotEnv []string
	previous := launchTerminal
	launchTerminal = func(dir string, env []string) error {
		calls++
		gotDir, gotEnv = dir, env
		return nil
	}
	defer func() { launchTerminal = previous }()

	if err := m.OpenTerminal("1.0.0", false); err == nil {
		t.Fatal("opened without acknowledging the warning")
	}
	if calls != 0 || m.state.TerminalAcknowledged {
		t.Fatalf("calls = %d, acknowledged = %v", calls, m.state.TerminalAcknowledged)
	}
	if err := m.OpenTerminal("1.0.0", true); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !m.state.TerminalAcknowledged {
		t.Fatalf("calls = %d, acknowledged = %v", calls, m.state.TerminalAcknowledged)
	}
	data := filepath.Join(m.root, "data")
	if gotDir != data {
		t.Fatalf("terminal dir = %q; want %q", gotDir, data)
	}
	values := envMap(gotEnv)
	if values["VIRTUAL_ENV"] != filepath.Join(m.root, "versions", "1.0.0") || values["NEOBOT_DATA_DIR"] != data {
		t.Fatalf("terminal env = %+v", values)
	}
	var saved diskState
	if err := readJSON(filepath.Join(m.root, "settings.json"), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.TerminalAcknowledged {
		t.Fatal("acknowledgement was not persisted")
	}
	if err := m.OpenTerminal("1.0.0", false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d; want 2", calls)
	}
}

func TestOpenTerminalRejectsBusyAndIncompleteStates(t *testing.T) {
	requireTerminalSupport(t)
	m := versionsTestManager(t)
	versionsInstall(t, m, "1.0.0")
	m.state.Selected = "1.0.0"
	previous := launchTerminal
	launchTerminal = func(string, []string) error {
		t.Fatal("launched in an unavailable state")
		return nil
	}
	defer func() { launchTerminal = previous }()

	m.job.State = "running"
	if err := m.OpenTerminal("1.0.0", true); err == nil {
		t.Fatal("opened during an install task")
	}
	m.job.State = ""
	m.preparing = true
	if err := m.OpenTerminal("1.0.0", true); err == nil {
		t.Fatal("opened while NeoBot was starting")
	}
	m.preparing = false
	m.closing = true
	if err := m.OpenTerminal("1.0.0", true); err == nil {
		t.Fatal("opened while the launcher was closing")
	}
	m.closing = false
	if err := os.Remove(venvPython(m.versionDir("1.0.0"))); err != nil {
		t.Fatal(err)
	}
	if err := m.OpenTerminal("1.0.0", true); err == nil || !strings.Contains(err.Error(), "环境不完整") {
		t.Fatalf("missing python error = %v", err)
	}
}

func TestOpenTerminalReportsMissingUVWithoutBlocking(t *testing.T) {
	requireTerminalSupport(t)
	m := versionsTestManager(t)
	versionsInstall(t, m, "1.0.0")
	m.state.Selected = "1.0.0"
	previous := launchTerminal
	launchTerminal = func(string, []string) error { return nil }
	defer func() { launchTerminal = previous }()
	if err := m.OpenTerminal("1.0.0", true); err != nil {
		t.Fatalf("missing uv should not prevent opening the shell: %v", err)
	}
}

func TestOpenTerminalLaunchErrorIsReported(t *testing.T) {
	requireTerminalSupport(t)
	m := versionsTestManager(t)
	versionsInstall(t, m, "1.0.0")
	m.state.Selected = "1.0.0"
	previous := launchTerminal
	launchTerminal = func(string, []string) error { return errors.New("boom") }
	defer func() { launchTerminal = previous }()
	err := m.OpenTerminal("1.0.0", true)
	if err == nil || !strings.Contains(err.Error(), "无法打开终端") {
		t.Fatalf("launch error = %v", err)
	}
}

func TestFolderVersionUsesTerminalTarget(t *testing.T) {
	m := versionsTestManager(t)
	versionsInstall(t, m, "1.0.0")
	dir, err := m.Folder("version")
	if err != nil || dir != m.versionDir("1.0.0") {
		t.Fatalf("single install folder = %q, %v", dir, err)
	}
	m.installed["1.1.0"] = versionOf("1.1.0")
	if _, err := m.Folder("version"); err == nil {
		t.Fatal("accepted an ambiguous version folder")
	}
	m.state.Selected = "1.1.0"
	if dir, err := m.Folder("version"); err != nil || dir != m.versionDir("1.1.0") {
		t.Fatalf("selected folder = %q, %v", dir, err)
	}
}
