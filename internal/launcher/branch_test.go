package launcher

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBranchNameValidation(t *testing.T) {
	valid := []string{"main", "dev", "feat/at-args", "release/2.0", "a.b", "feat/x-y_z", "feat/中文", "release+test"}
	for _, name := range valid {
		if !validBranchName(name) {
			t.Fatalf("rejected valid branch %q", name)
		}
	}
	invalid := []string{
		"", "../x", "a..b", "/a", "a/", "a//b", "a/.b", "a/b.lock",
		"a\\b", "a b", "a~b", "-a", strings.Repeat("a", 201), "a@{b", "a:b", "@", "a^b", "a?b", "a*b", "a[b", "a\tb", "a\x7fb",
	}
	for _, name := range invalid {
		if validBranchName(name) {
			t.Fatalf("accepted invalid branch %q", name)
		}
	}
}

func TestBranchIDStableAndSafe(t *testing.T) {
	id := branchID("feat/QQ-args")
	if id != branchID("feat/QQ-args") || !isBranchID(id) {
		t.Fatalf("branch id is not stable or valid: %q", id)
	}
	if strings.ContainsAny(id, "/\\:") {
		t.Fatalf("branch id is not filesystem safe: %q", id)
	}
	if branchID("feat/x") == branchID("feat-x") {
		t.Fatal("different branches with the same slug collided")
	}
	if branchID("A") == branchID("a") {
		t.Fatal("case-sensitive branches collided")
	}
	if long := branchID(strings.Repeat("a", 200)); !isBranchID(long) {
		t.Fatalf("long branch id is invalid: %q", long)
	}
}

func TestBranchInstallRecordScanning(t *testing.T) {
	root := t.TempDir()
	m := Manager{root: root, installed: map[string]Version{}}
	makeInstall := func(name string, record installRecord) {
		dir := filepath.Join(root, "versions", name)
		if err := os.MkdirAll(venvBin(filepath.Join(dir)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(botExecutable(filepath.Join(dir)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeInstallRecord(dir, record); err != nil {
			t.Fatal(err)
		}
	}
	const ref = "feat/x"
	branch := branchID(ref)
	makeInstall(branch, installRecord{Name: branch, Branch: ref}) // Legacy upstream record.
	fork := repositoryBranchID("fork/neobot", ref)
	makeInstall(fork, installRecord{Name: fork, Repository: "fork/neobot", Branch: ref})
	mismatch := repositoryBranchID("other/neobot", ref)
	makeInstall(mismatch, installRecord{Name: mismatch, Repository: "fork/neobot", Branch: ref})
	makeInstall("1.2.3", installRecord{Name: "1.2.3", Preview: true})
	makeInstall(branchID("bad"), installRecord{Name: branchID("bad"), Branch: "other"})
	if err := m.scanInstalled(); err != nil {
		t.Fatal(err)
	}
	for id, repo := range map[string]string{branch: githubRepo, fork: "fork/neobot"} {
		if got := m.installed[id]; got != (Version{Name: id, Branch: ref, Repository: repo}) {
			t.Fatalf("scanned %s = %+v; want %s at %s", id, got, repo, ref)
		}
	}
	if version, ok := m.installed["1.2.3"]; !ok || !version.Preview {
		t.Fatalf("release install was not scanned: %+v", version)
	}
	for _, id := range []string{branchID("bad"), mismatch} {
		if _, ok := m.installed[id]; ok {
			t.Fatalf("accepted mismatched branch or repository for %s", id)
		}
	}
}

func TestBranchUsesSharedDataAndIsolation(t *testing.T) {
	root := t.TempDir()
	m := Manager{root: root, state: diskState{DataVersion: "1.0.0", Backups: map[string]string{}}}
	active, err := m.prepareData("1.0.0", false)
	if err != nil {
		t.Fatal(err)
	}
	branch := branchID("dev")
	shared, err := m.prepareData(branch, false)
	if err != nil || shared != active {
		t.Fatalf("branch should share active data: %s, %v", shared, err)
	}
	if m.state.DataVersion != "1.0.0" || len(m.state.Backups) != 0 {
		t.Fatal("branch changed stable data tracking")
	}
	m.state.IsolateData = true
	isolated, err := m.prepareData(branch, false)
	want := filepath.Join(root, "isolated", branch)
	if err != nil || isolated != want || !isDir(want) || m.state.DataVersion != "1.0.0" {
		t.Fatalf("branch isolation path: %s, %v", isolated, err)
	}
	stableIsolated, err := m.prepareData("1.0.0", false)
	if err != nil || stableIsolated != filepath.Join(root, "isolated", "1.0.0") || !isDir(stableIsolated) {
		t.Fatalf("stable isolation path: %s, %v", stableIsolated, err)
	}
	if switchInfo("1.0.0", branch, false).Restore || switchInfo("1.0.0", "0.9.0", true).Restore {
		t.Fatal("isolation or branch target requested a data restore")
	}
	if !switchInfo("1.0.0", "0.9.0", false).Restore {
		t.Fatal("stable downgrade should still request a snapshot")
	}
}

func TestPreviewDataMigratesToIsolated(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "preview", "2.0.0a1")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "config.toml"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	m := Manager{root: root}
	m.migratePreviewData()
	target := filepath.Join(root, "isolated", "2.0.0a1", "config.toml")
	if value, err := os.ReadFile(target); err != nil || string(value) != "keep" {
		t.Fatalf("preview data was not migrated: %q, %v", value, err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatal("legacy preview directory was not moved")
	}
}

func TestVersionIsolationPersistsAndRejectsRunning(t *testing.T) {
	root := t.TempDir()
	m := Manager{root: root, state: diskState{Backups: map[string]string{}}}
	if err := m.SetVersionIsolation(true); err != nil {
		t.Fatal(err)
	}
	var stored diskState
	if err := readJSON(filepath.Join(root, "settings.json"), &stored); err != nil || !stored.IsolateData {
		t.Fatalf("isolation setting was not persisted: %+v, %v", stored, err)
	}
	m.mu.Lock()
	m.bot = &botProcess{}
	m.mu.Unlock()
	if err := m.SetVersionIsolation(false); err == nil {
		t.Fatal("isolation changed while a process was running")
	}
	if !m.state.IsolateData {
		t.Fatal("failed isolation change leaked into memory")
	}
	m.mu.Lock()
	m.bot = nil
	m.job.State = "running"
	m.mu.Unlock()
	if err := m.SetVersionIsolation(false); err == nil {
		t.Fatal("isolation changed while an install was running")
	}
}

func TestExtractBranchSource(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "source.zip")
	createZip(t, archive, map[string]string{
		"NeoBot-dev/pyproject.toml":              "[project]\nname='neobot'\n",
		"NeoBot-dev/app/pyproject.toml":          "[project]\nname='neobot-app'\n",
		"NeoBot-dev/packages/foo/pyproject.toml": "[project]\n",
	})
	workspace, err := extractBranchSource(context.Background(), archive, filepath.Join(root, "src"))
	if err != nil || workspace != filepath.Join(root, "src", "NeoBot-dev") {
		t.Fatalf("workspace = %q, %v", workspace, err)
	}
	if !isFile(filepath.Join(workspace, "app", "pyproject.toml")) {
		t.Fatal("workspace files were not extracted")
	}
}

func TestExtractBranchSourceKeepsExecutableBit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有可执行位")
	}
	root := t.TempDir()
	archive := filepath.Join(root, "source.zip")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for _, entry := range []struct {
		name string
		mode os.FileMode
		body string
	}{
		{"NeoBot-dev/pyproject.toml", 0644, "[project]\nname='neobot'\n"},
		{"NeoBot-dev/app/pyproject.toml", 0755, "[project]\nname='neobot-app'\n"},
		{"NeoBot-dev/packages/keep/pyproject.toml", 0644, "[project]\n"},
		{"NeoBot-dev/packages/keep/run.sh", 0755, "#!/bin/sh\n"},
	} {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		header.SetMode(entry.mode)
		part, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	workspace, err := extractBranchSource(context.Background(), archive, filepath.Join(root, "src"))
	if err != nil {
		t.Fatal(err)
	}
	for path, wantExec := range map[string]bool{
		filepath.Join(workspace, "app", "pyproject.toml"):              true,
		filepath.Join(workspace, "packages", "keep", "run.sh"):         true,
		filepath.Join(workspace, "packages", "keep", "pyproject.toml"): false,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm()&0111 != 0; got != wantExec {
			t.Fatalf("%s executable = %v; want %v (mode %v)", path, got, wantExec, info.Mode())
		}
	}
}

func TestExtractBranchSourceRejectsUnsafeEntries(t *testing.T) {
	for _, name := range []string{"../evil.txt", "/evil.txt", "NeoBot-dev\\..\\evil.txt"} {
		root := t.TempDir()
		archive := filepath.Join(root, "source.zip")
		createZip(t, archive, map[string]string{name: "bad"})
		if _, err := extractBranchSource(context.Background(), archive, filepath.Join(root, "src")); err == nil {
			t.Fatalf("accepted unsafe archive entry %q", name)
		}
	}
	root := t.TempDir()
	archive := filepath.Join(root, "source.zip")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	header := &zip.FileHeader{Name: "NeoBot-dev/link", Method: zip.Store}
	header.SetMode(os.ModeSymlink | 0777)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = entry.Write([]byte("target")); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBranchSource(context.Background(), archive, filepath.Join(root, "src")); err == nil {
		t.Fatal("accepted a symlink entry")
	}
}

func TestDownloadBranchArchive(t *testing.T) {
	old := githubArchiveBase
	defer func() { githubArchiveBase = old }()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("NeoBot-dev/pyproject.toml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = entry.Write([]byte("fixture")); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/contributor/neobot/archive/refs/heads/Feat/中文+#1.zip":
			if r.RequestURI != "/contributor/neobot/archive/refs/heads/Feat/%E4%B8%AD%E6%96%87+%231.zip" {
				t.Errorf("unexpected escaped archive path %s", r.RequestURI)
			}
			_, _ = w.Write(archive.Bytes())
		case "/contributor/neobot/archive/refs/heads/large.zip":
			w.Header().Set("Content-Length", strconv.FormatInt(branchArchiveLimit+1, 10))
			w.WriteHeader(http.StatusOK)
		case "/contributor/neobot/archive/refs/heads/missing.zip":
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected archive path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	githubArchiveBase = server.URL
	target := filepath.Join(t.TempDir(), "source.zip")
	if err := downloadBranchArchive(context.Background(), "contributor/neobot", "missing", target); err == nil || !strings.Contains(err.Error(), "找不到分支") {
		t.Fatalf("missing branch error: %v", err)
	}
	if err := downloadBranchArchive(context.Background(), "contributor/neobot", "large", target); err == nil || !strings.Contains(err.Error(), "过大") {
		t.Fatalf("oversized archive error: %v", err)
	}
	if err := downloadBranchArchive(context.Background(), "contributor/neobot", "Feat/中文+#1", target); err != nil {
		t.Fatal(err)
	}
	reader, err := zip.OpenReader(target)
	if err != nil {
		t.Fatalf("downloaded archive is invalid: %v", err)
	}
	reader.Close()
}

func TestFetchGitHubBranches(t *testing.T) {
	old := githubAPIBase
	defer func() { githubAPIBase = old }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/contributor/neobot/branches" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("unexpected list URL %s", r.URL)
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", `</repos/contributor/neobot/branches?page=2>; rel="next"`)
			_, _ = w.Write([]byte(`[{"name":"dev","commit":{"sha":"abc"}},{"name":"../bad"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"name":"main","commit":{"sha":"def"}}]`))
	}))
	defer server.Close()
	githubAPIBase = server.URL
	branches, err := fetchGitHubBranches(context.Background(), "contributor/neobot")
	if err != nil || len(branches) != 2 || branches[0].Name != "main" || branches[1].Commit != "abc" {
		t.Fatalf("branches = %+v, %v", branches, err)
	}
	for _, branch := range branches {
		if branch.Name == "../bad" {
			t.Fatal("invalid branch was listed")
		}
	}
}

func TestFetchGitHubBranchesRateLimited(t *testing.T) {
	old := githubAPIBase
	defer func() { githubAPIBase = old }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	githubAPIBase = server.URL
	if _, err := fetchGitHubBranches(context.Background(), githubRepo); err == nil || !strings.Contains(err.Error(), "限流") {
		t.Fatalf("rate limit error: %v", err)
	}
}

func TestBranchSyncArgs(t *testing.T) {
	args := strings.Join(branchSyncArgs("C:/source"), " ")
	for _, want := range []string{
		"sync", "--project C:/source", "--package neobot-app",
		"--no-dev", "--no-editable", "--python 3.13", "--no-python-downloads",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("sync args missing %q: %s", want, args)
		}
	}
}

func TestBranchSyncEnv(t *testing.T) {
	root := t.TempDir()
	m := Manager{root: root}
	target := filepath.Join(root, "versions", branchID("dev"))
	env := branchSyncEnv(m.uvEnv(), target)
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"UV_CACHE_DIR=" + filepath.Join(root, "cache"),
		"UV_PYTHON_INSTALL_DIR=" + filepath.Join(root, "python"),
		"UV_PYTHON_PREFERENCE=only-managed",
		"UV_PROJECT_ENVIRONMENT=" + target,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("sync env missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "UV_NO_CONFIG") {
		t.Fatal("project config must not be disabled for workspace sync")
	}
}

func createZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestRecoverVersionDirs(t *testing.T) {
	root := t.TempDir()
	versions := filepath.Join(root, "versions")
	if err := os.MkdirAll(versions, 0700); err != nil {
		t.Fatal(err)
	}
	m := Manager{root: root, installed: map[string]Version{}}
	makeInstallAt := func(dir, name string) {
		t.Helper()
		if err := os.MkdirAll(venvBin(filepath.Join(dir)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(botExecutable(filepath.Join(dir)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeInstallRecord(dir, installRecord{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	makeInstall := func(name string) string {
		dir := filepath.Join(versions, name)
		makeInstallAt(dir, name)
		return dir
	}
	const oldStamp = "1234567890123456700"
	const newStamp = "1234567890123456800"

	keep := branchID("keep")
	makeInstall(keep)
	keepOld := filepath.Join(versions, keep+".old-"+oldStamp)
	makeInstallAt(keepOld, keep)
	if err := m.recoverVersionDirs(); err != nil {
		t.Fatal(err)
	}
	if isDir(keepOld) || !isFile(m.binary(keep)) {
		t.Fatal("valid new install did not clean up the old directory")
	}

	restore := branchID("restore")
	restoreDir := filepath.Join(versions, restore)
	restoreOld := filepath.Join(versions, restore+".old-"+oldStamp)
	makeInstallAt(restoreOld, restore)
	if err := m.recoverVersionDirs(); err != nil {
		t.Fatal(err)
	}
	if !isFile(botExecutable(filepath.Join(restoreDir))) || isDir(restoreOld) {
		t.Fatal("missing install was not restored from the old directory")
	}

	partial := branchID("partial")
	partialTarget := filepath.Join(versions, partial)
	if err := os.MkdirAll(partialTarget, 0700); err != nil {
		t.Fatal(err)
	}
	partialOld := filepath.Join(versions, partial+".old-"+oldStamp)
	makeInstallAt(partialOld, partial)
	if err := os.WriteFile(filepath.Join(partialOld, "marker"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.recoverVersionDirs(); err != nil {
		t.Fatal(err)
	}
	if value, err := os.ReadFile(filepath.Join(partialTarget, "marker")); err != nil || string(value) != "old" {
		t.Fatalf("invalid new install was not replaced by the old one: %q, %v", value, err)
	}

	fileTarget := filepath.Join(versions, branchID("file-target"))
	if err := os.WriteFile(fileTarget, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	fileOld := filepath.Join(versions, branchID("file-target")+".old-"+oldStamp)
	makeInstallAt(fileOld, branchID("file-target"))
	if err := m.recoverVersionDirs(); err != nil {
		t.Fatal(err)
	}
	broken, _ := filepath.Glob(fileTarget + ".broken-*")
	if !isFile(botExecutable(filepath.Join(fileTarget))) || len(broken) != 1 {
		t.Fatalf("file target was not recovered: binary=%v broken=%v", isFile(botExecutable(filepath.Join(fileTarget))), broken)
	}

	multi := branchID("multi")
	multiOld := filepath.Join(versions, multi+".old-"+oldStamp)
	multiNew := filepath.Join(versions, multi+".old-"+newStamp)
	makeInstallAt(multiOld, multi)
	makeInstallAt(multiNew, multi)
	if err := os.WriteFile(filepath.Join(multiOld, "marker"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(multiNew, "marker"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.recoverVersionDirs(); err != nil {
		t.Fatal(err)
	}
	if value, err := os.ReadFile(filepath.Join(versions, multi, "marker")); err != nil || string(value) != "new" {
		t.Fatalf("newest old install was not chosen: %q, %v", value, err)
	}
	if isDir(multiOld) || isDir(multiNew) {
		t.Fatal("old duplicates were not removed")
	}

	corrupt := branchID("corrupt")
	corruptTarget := filepath.Join(versions, corrupt)
	if err := os.MkdirAll(venvBin(filepath.Join(corruptTarget)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(botExecutable(filepath.Join(corruptTarget)), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptTarget, ".installed"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	corruptOld := filepath.Join(versions, corrupt+".old-"+oldStamp)
	makeInstallAt(corruptOld, corrupt)
	if err := m.recoverVersionDirs(); err == nil {
		t.Fatal("unreadable install record was not reported")
	}
	if !isDir(corruptTarget) || !isDir(corruptOld) {
		t.Fatal("a temporarily unreadable target was destroyed")
	}

	branchLike := filepath.Join(versions, "branch-x.old-12345678")
	if err := os.MkdirAll(branchLike, 0700); err != nil {
		t.Fatal(err)
	}
	_ = m.recoverVersionDirs()
	if !isDir(branchLike) {
		t.Fatal("a branch id containing .old- was treated as a backup directory")
	}
}
func TestListGitHubBranchesCaches(t *testing.T) {
	old := githubAPIBase
	defer func() { githubAPIBase = old }()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`[{"name":"main","commit":{"sha":"abc"}}]`))
	}))
	defer server.Close()
	githubAPIBase = server.URL
	m := &Manager{}
	first, err := m.ListGitHubBranches("", false)
	if err != nil || len(first) != 1 || first[0].Name != "main" || first[0].Commit != "abc" {
		t.Fatalf("first list = %+v, %v", first, err)
	}
	second, err := m.ListGitHubBranches("", false)
	if err != nil || len(second) != 1 || requests.Load() != 1 {
		t.Fatalf("cache was not used: %+v, %v, requests=%d", second, err, requests.Load())
	}
	if _, err := m.ListGitHubBranches("", true); err != nil || requests.Load() != 2 {
		t.Fatalf("forced refresh = %v, requests=%d", err, requests.Load())
	}
}
func TestBranchIDsAreRecognized(t *testing.T) {
	for _, name := range []string{"main", "_dev", "__", "_a/b", "a_b", "_"} {
		if !validBranchName(name) {
			t.Fatalf("valid branch %q was rejected", name)
		}
		id := branchID(name)
		if !isBranchID(id) {
			t.Fatalf("branch %q generated an unrecognized id %q", name, id)
		}
	}
}

func TestUnderscoreBranchDoesNotTouchDataVersion(t *testing.T) {
	root := t.TempDir()
	m := Manager{root: root, state: diskState{DataVersion: "1.0.0", Backups: map[string]string{}}}
	id := branchID("_dev")
	shared, err := m.prepareData(id, false)
	if err != nil || shared != filepath.Join(root, "data") {
		t.Fatalf("underscore branch data = %q, %v", shared, err)
	}
	if m.state.DataVersion != "1.0.0" {
		t.Fatalf("underscore branch wrote data version %q", m.state.DataVersion)
	}
	if switchInfo("1.0.0", id, false).Restore {
		t.Fatal("underscore branch requested a stable data restore")
	}
}

func TestOpenDropsInvalidDataVersion(t *testing.T) {
	if !supported() {
		t.Skip("实例锁仅在支持的平台实现")
	}
	root := t.TempDir()
	if err := writeJSON(filepath.Join(root, "settings.json"), diskState{
		DataVersion: "branch-_dev-0123456789",
		Backups:     map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	m, err := Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.state.DataVersion != "" {
		t.Fatalf("invalid data version was kept: %q", m.state.DataVersion)
	}
	var stored diskState
	if err := readJSON(filepath.Join(root, "settings.json"), &stored); err != nil || stored.DataVersion != "" {
		t.Fatalf("settings were not repaired: %+v, %v", stored, err)
	}
}
func TestInstalledReleaseWithoutRecord(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "versions", "1.2.3")
	if err := os.MkdirAll(venvBin(filepath.Join(dir)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(botExecutable(filepath.Join(dir)), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	m := Manager{root: root, installed: map[string]Version{}}
	if err := m.scanInstalled(); err != nil {
		t.Fatal(err)
	}
	if version, ok := m.installed["1.2.3"]; !ok || version.Preview {
		t.Fatalf("release without a record was not recovered: %+v", version)
	}
	if !isFile(filepath.Join(dir, ".installed")) {
		t.Fatal("missing install record was not repaired")
	}
}
func TestStartRejectsIncompleteInstall(t *testing.T) {
	root := t.TempDir()
	m := Manager{root: root, installed: map[string]Version{"1.0.0": {Name: "1.0.0"}},
		state: diskState{Selected: "1.0.0"}}
	if err := m.Start(false); err == nil || !strings.Contains(err.Error(), "不完整") {
		t.Fatalf("incomplete install was not rejected: %v", err)
	}
}
func TestSafeZipNameWindowsReserved(t *testing.T) {
	for _, name := range []string{
		"NeoBot-dev/CON", "NeoBot-dev/aux.txt", "NeoBot-dev/COM1", "NeoBot-dev/LPT9",
		"NeoBot-dev/x.", "NeoBot-dev/x ", "NeoBot-dev/a//b", "NeoBot-dev/.",
	} {
		if safeZipName(name) {
			t.Fatalf("accepted unsafe zip name %q", name)
		}
	}
	for _, name := range []string{"NeoBot-dev/", "NeoBot-dev/console.md", "NeoBot-dev/.gitignore"} {
		if !safeZipName(name) {
			t.Fatalf("rejected safe zip name %q", name)
		}
	}
}

func TestExtractBranchSourceCanceled(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "source.zip")
	createZip(t, archive, map[string]string{"NeoBot-dev/pyproject.toml": "[project]\n"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := extractBranchSource(ctx, archive, filepath.Join(root, "src")); err == nil {
		t.Fatal("canceled extraction was not stopped")
	}
}
func TestFetchGitHubBranchesTooManyRequests(t *testing.T) {
	old := githubAPIBase
	defer func() { githubAPIBase = old }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	githubAPIBase = server.URL
	if _, err := fetchGitHubBranches(context.Background(), githubRepo); err == nil || !strings.Contains(err.Error(), "限流") {
		t.Fatalf("429 was not mapped to a rate-limit error: %v", err)
	}
}
