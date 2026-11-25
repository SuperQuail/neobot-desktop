//go:build linux

package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTerminalArgsForKnownEmulators(t *testing.T) {
	for _, test := range []struct {
		name string
		want []string
	}{
		{"xdg-terminal-exec", []string{"--dir=/data", "/usr/bin/bash", "-i"}},
		{"gnome-terminal", []string{"--working-directory=/data", "--", "/usr/bin/bash", "-i"}},
		{"kgx", []string{"--working-directory=/data", "-e", "/usr/bin/bash", "-i"}},
		{"konsole", []string{"--workdir", "/data", "-e", "/usr/bin/bash", "-i"}},
		{"xfce4-terminal", []string{"--working-directory=/data", "-x", "/usr/bin/bash", "-i"}},
		{"kitty", []string{"--directory", "/data", "/usr/bin/bash", "-i"}},
		{"alacritty", []string{"--working-directory", "/data", "-e", "/usr/bin/bash", "-i"}},
		{"wezterm", []string{"start", "--cwd", "/data", "--", "/usr/bin/bash", "-i"}},
		{"foot", []string{"--working-directory=/data", "/usr/bin/bash", "-i"}},
		{"xterm", []string{"-e", "/usr/bin/bash", "-i"}},
		{"unknown-term", []string{"-e", "/usr/bin/bash", "-i"}},
	} {
		got := terminalArgs("/usr/bin/"+test.name, "/data", "/usr/bin/bash", []string{"-i"})
		if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
			t.Fatalf("%s args = %q; want %q", test.name, got, test.want)
		}
	}
}

func TestFindTerminalPrefersGNOMEConsoleOverKonsole(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"kgx", "konsole"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TERMINAL", "")
	t.Setenv("PATH", dir)
	path, args, err := findTerminal("/data")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "kgx" {
		t.Fatalf("terminal = %q; want kgx before konsole", path)
	}
	if len(args) < 2 || args[0] != "--working-directory=/data" || args[1] != "-e" {
		t.Fatalf("kgx args = %q; want --working-directory and -e", args)
	}
}

func TestFindTerminalHonorsTERMINAL(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "my-terminal")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMINAL", script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	path, args, err := findTerminal("/data")
	if err != nil {
		t.Fatal(err)
	}
	if path != script {
		t.Fatalf("terminal = %q; want %q", path, script)
	}
	if len(args) < 2 || args[0] != "-e" {
		t.Fatalf("args = %q; want -e <shell>", args)
	}
	if !strings.Contains(strings.Join(args, " "), filepath.Base(loginShellPath())) {
		t.Fatalf("args = %q; want the login shell %q", args, loginShellPath())
	}
}

func loginShellPath() string {
	path, _ := loginShell()
	return path
}

func TestOpenTerminalWindowLaunchesFakeTerminal(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "terminal.out")
	script := filepath.Join(dir, "fake-terminal")
	content := "#!/bin/sh\nprintf 'cwd=%s\\nmark=%s\\n' \"$(pwd)\" \"$NEOBOT_TEST_MARK\" > \"$FAKE_TERM_OUT\"\n"
	if err := os.WriteFile(script, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMINAL", script)
	env := append(os.Environ(), "FAKE_TERM_OUT="+out, "NEOBOT_TEST_MARK=probe", "PS1=[NeoBot 1.0.0]")
	if err := openTerminalWindow(data, env); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		dataBytes, err := os.ReadFile(out)
		if err == nil {
			text := string(dataBytes)
			if !strings.Contains(text, "cwd="+data) || !strings.Contains(text, "mark=probe") {
				t.Fatalf("terminal did not inherit cwd/env:\n%s", text)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake terminal did not run: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTerminalShellEnvSetsLinuxPrompt(t *testing.T) {
	t.Setenv("TERM", "")
	values := envMap(terminalShellEnv("1.2.3"))
	if values["PS1"] != "[NeoBot 1.2.3] \\w \\$ " || values["PROMPT"] != values["PS1"] {
		t.Fatalf("prompt env = %+v", values)
	}
	if values["TERM"] != "xterm-256color" {
		t.Fatalf("TERM = %q; want a fallback", values["TERM"])
	}
}

func TestUVShimIsExecutableShellScript(t *testing.T) {
	name, content, mode, err := uvShim("/opt/neobot/tools/uv-0.12.9")
	if err != nil {
		t.Fatal(err)
	}
	if name != "uv" || mode.Perm() != 0755 {
		t.Fatalf("uv shim = %q, %v; want executable uv", name, mode)
	}
	if !strings.HasPrefix(string(content), "#!/bin/sh\n") || !strings.Contains(string(content), "uv-0.12.9") {
		t.Fatalf("uv shim content = %q", content)
	}
}

func TestUVShimForwardsArguments(t *testing.T) {
	root := t.TempDir()
	tools := filepath.Join(root, "tools")
	if err := os.MkdirAll(tools, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uvExecutable(root), []byte("#!/bin/sh\nprintf '%s' \"$*\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{root: root}
	if err := m.writeUVShim(); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(filepath.Join(tools, "uv"), "pip", "check").Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "pip check" {
		t.Fatalf("uv shim output = %q; want %q", out, "pip check")
	}
}
