//go:build linux

package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxAutostartWritesAndRemovesDesktopEntry(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	path, err := autostartPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(config, "autostart", autostartFileName); path != want {
		t.Fatalf("autostart path = %q; want %q", path, want)
	}
	if err := setAutostartEntry(true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[Desktop Entry]",
		"Type=Application",
		"Name=NeoBot Desktop",
		"Exec=" + desktopQuote(exe) + " --startup",
		"Terminal=false",
		"X-GNOME-Autostart-enabled=true",
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("desktop entry missing %q:\n%s", want, data)
		}
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("desktop entry mode = %v, %v; want 0644", info, err)
	}
	if err := setAutostartEntry(true); err != nil {
		t.Fatalf("second enable = %v; want idempotent", err)
	}
	if err := setAutostartEntry(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("autostart entry still exists: %v", err)
	}
	if err := setAutostartEntry(false); err != nil {
		t.Fatalf("second disable = %v; want idempotent", err)
	}
}

func TestDesktopQuoteEscapesSpecialCharacters(t *testing.T) {
	for _, test := range []struct{ in, want string }{
		{"/home/me/NeoBot Desktop/neobot-desktop", `"/home/me/NeoBot Desktop/neobot-desktop"`},
		{`/home/me/a"b`, `"/home/me/a\"b"`},
		{`/home/me/a%b`, `"/home/me/a%%b"`},
		{`/home/me/a$b`, `"/home/me/a\$b"`},
		{`/home/me/a\b`, `"/home/me/a\\b"`},
		{" /home/me/a`b", "\" /home/me/a\\`b\""},
	} {
		if got := desktopQuote(test.in); got != test.want {
			t.Fatalf("desktopQuote(%q) = %q; want %q", test.in, got, test.want)
		}
	}
}

func TestLinuxSetAutostartWritesAndPersists(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	m, err := Open(filepath.Join(t.TempDir(), "data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.SetAutostart(true); err != nil {
		t.Fatal(err)
	}
	if !m.Snapshot().Supervisor.Autostart {
		t.Fatal("autostart state was not reported")
	}
	path, err := autostartPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("autostart entry was not written: %v", err)
	}
	if err := m.SetAutostart(false); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Supervisor.Autostart {
		t.Fatal("autostart state was not cleared")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("autostart entry was not removed: %v", err)
	}
}
