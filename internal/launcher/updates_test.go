package launcher

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ── 测试夹具 ─────────────────────────────────────────────────────

// updateTestVersion 替换当前启动器版本号，测试结束后恢复。
// 它会改动包级状态，所以相关测试不使用 t.Parallel。
func updateTestVersion(t *testing.T, version string) {
	t.Helper()
	previous := currentVersion
	currentVersion = version
	t.Cleanup(func() { currentVersion = previous })
}

// updateTestServer 起一个 TLS 服务器，并把可注入的地址与客户端都换成它。
// 用 TLS 而不是 http，是因为自更新只接受 https 地址（发布信息也来自 HTTPS）。
func updateTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	previousAPI, previousDownload := updateAPIHTTP, updateDownloadHTTP
	previousRelease, previousRecommended := githubLatestReleaseURL, recommendedJSONURL
	updateAPIHTTP, updateDownloadHTTP = server.Client(), server.Client()
	githubLatestReleaseURL = server.URL + "/releases/latest"
	recommendedJSONURL = server.URL + "/recommended.json"
	t.Cleanup(func() {
		updateAPIHTTP, updateDownloadHTTP = previousAPI, previousDownload
		githubLatestReleaseURL, recommendedJSONURL = previousRelease, previousRecommended
		server.Close()
	})
	return server
}

// updateTestWait 等异步检查结束。CheckUpdate 先占住 updateBusy 再起 goroutine，
// 所以这里不会漏掉刚发起的那一轮。
func updateTestWait(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		m.mu.Lock()
		busy := m.updateBusy
		m.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("更新检查没有在 10 秒内结束")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// updateTestCheck 同步跑一次检查并返回结果。
func updateTestCheck(t *testing.T, m *Manager) UpdateStatus {
	t.Helper()
	if err := m.CheckUpdate(true); err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	updateTestWait(t, m)
	return m.Snapshot().Update
}

// updateTestRelease 拼一段 GitHub 发布接口的响应。
func updateTestRelease(tag, body, assets string) string {
	return `{"tag_name":"` + tag + `","body":` + strconv.Quote(body) +
		`,"html_url":"https://github.com/SuperQuail/neobot-desktop/releases/tag/` + tag + `","assets":[` + assets + `]}`
}

// updateTestEmptyRecommended 是一份「暂不推荐」的 recommended.json。
const updateTestEmptyRecommended = `{"version":"","min_launcher":"","updated_at":""}`

// ── 版本比较 ─────────────────────────────────────────────────────

func TestNormalizeVersion(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
		ok    bool
	}{
		{"0.1.0", "0.1.0", true},
		{"v0.1.0", "0.1.0", true},
		{"  V1.2.3  ", "1.2.3", true},
		{"1.0.0-alpha.5", "1.0.0a5", true},
		{"1.0.0-beta.2", "1.0.0b2", true},
		{"1.0.0-rc.1", "1.0.0rc1", true},
		{"1.0.0-rc", "1.0.0rc0", true},
		{"1.2.3a1", "1.2.3a1", true},
		{"latest", "", false},
		{"1.0", "", false},
		{"2024.01.01", "", false},
	} {
		got, ok := normalizeVersion(test.input)
		if got != test.want || ok != test.ok {
			t.Errorf("normalizeVersion(%q) = %q, %v; 期望 %q, %v", test.input, got, ok, test.want, test.ok)
		}
	}
}

func TestUpdateDetectsNewerRelease(t *testing.T) {
	for _, test := range []struct {
		name      string
		current   string
		tag       string
		available bool
	}{
		{"预发布递增", "0.1.0-alpha.5", "v0.1.0-alpha.6", true},
		{"同版本", "0.1.0-alpha.5", "v0.1.0-alpha.5", false},
		{"正式版比预发布新", "0.1.0-alpha.5", "v0.1.0", true},
		{"预发布比正式版旧", "0.1.0", "v0.1.0-alpha.5", false},
		{"次版本递增", "0.1.0", "v0.2.0", true},
		{"预发布序号按数字比较", "0.1.0-alpha.9", "v0.1.0-alpha.10", true},
		{"PEP 440 预发布", "1.2.3a1", "v1.2.3a2", true},
		{"更高版本不算更新", "2.0.0", "v1.9.9", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			updateTestVersion(t, test.current)
			updateTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/releases/latest":
					_, _ = io.WriteString(w, updateTestRelease(test.tag, "", ""))
				case "/recommended.json":
					_, _ = io.WriteString(w, updateTestEmptyRecommended)
				default:
					http.NotFound(w, r)
				}
			})
			status := updateTestCheck(t, &Manager{root: t.TempDir()})
			if status.Error != "" {
				t.Fatalf("检查 %s → %s 报错：%s", test.current, test.tag, status.Error)
			}
			if status.Available != test.available {
				t.Fatalf("检查 %s → %s: available = %v; 期望 %v", test.current, test.tag, status.Available, test.available)
			}
			if want := strings.TrimPrefix(test.tag, "v"); status.Latest != want {
				t.Fatalf("Latest = %q; 期望 %q", status.Latest, want)
			}
			if status.Current != test.current {
				t.Fatalf("Current = %q; 期望 %q", status.Current, test.current)
			}
		})
	}
}

// withWindowsUpdateAssets 在非 Windows 平台上把更新资产切回 Windows 的配置，
// 这样资产选择、下载与校验逻辑仍然能在 Linux CI 上覆盖；测试结束恢复。
func withWindowsUpdateAssets(t *testing.T) {
	t.Helper()
	previousName, previousSuffix := updateAssetName, updateAssetSuffix
	previousChecksum, previousAuto := updateChecksumName, updateAutoInstall
	updateAssetName, updateAssetSuffix = "neobot-desktop-amd64-installer.exe", "-installer.exe"
	updateChecksumName, updateAutoInstall = "checksums.txt", true
	t.Cleanup(func() {
		updateAssetName, updateAssetSuffix = previousName, previousSuffix
		updateChecksumName, updateAutoInstall = previousChecksum, previousAuto
	})
}

// ── 发布信息解析 ─────────────────────────────────────────────────

func TestUpdatePicksInstallerAssetsAndTruncatesNotes(t *testing.T) {
	withWindowsUpdateAssets(t)
	updateTestVersion(t, "1.0.0")
	var base string
	updateTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			assets := `{"name":"neobot-desktop-amd64.zip","browser_download_url":"` + base + `/download/neobot-desktop-amd64.zip"},` +
				`{"name":"checksums.txt","browser_download_url":"` + base + `/download/checksums.txt"},` +
				`{"name":"` + updateAssetName + `","browser_download_url":"` + base + `/download/` + updateAssetName + `"}`
			_, _ = io.WriteString(w, updateTestRelease("v1.1.0", strings.Repeat("更新说明", 1500), assets))
		case "/recommended.json":
			_, _ = io.WriteString(w, updateTestEmptyRecommended)
		default:
			http.NotFound(w, r)
		}
	})
	base = strings.TrimSuffix(strings.TrimSpace(githubLatestReleaseURL), "/releases/latest")

	status := updateTestCheck(t, &Manager{root: t.TempDir()})
	if status.Error != "" {
		t.Fatal(status.Error)
	}
	if want := base + "/download/" + updateAssetName; status.AssetURL != want {
		t.Fatalf("AssetURL = %q; 期望 %q", status.AssetURL, want)
	}
	if want := base + "/download/checksums.txt"; status.ChecksumsURL != want {
		t.Fatalf("ChecksumsURL = %q; 期望 %q", status.ChecksumsURL, want)
	}
	if !strings.HasSuffix(status.PageURL, "/releases/tag/v1.1.0") {
		t.Fatalf("PageURL = %q", status.PageURL)
	}
	if runes := []rune(status.Notes); len(runes) != updateNotesLimit+1 || !strings.HasSuffix(status.Notes, "…") {
		t.Fatalf("发布说明没有被截断到 %d 字：%d 字", updateNotesLimit, len(runes))
	}
}

func TestUpdateFallsBackToInstallerSuffix(t *testing.T) {
	withWindowsUpdateAssets(t)
	updateTestVersion(t, "1.0.0")
	var base string
	updateTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			assets := `{"name":"neobot-desktop-arm64-installer.exe","browser_download_url":"` + base + `/download/neobot-desktop-arm64-installer.exe"}`
			_, _ = io.WriteString(w, updateTestRelease("v1.1.0", "", assets))
		case "/recommended.json":
			_, _ = io.WriteString(w, updateTestEmptyRecommended)
		default:
			http.NotFound(w, r)
		}
	})
	base = strings.TrimSuffix(strings.TrimSpace(githubLatestReleaseURL), "/releases/latest")

	status := updateTestCheck(t, &Manager{root: t.TempDir()})
	if want := base + "/download/neobot-desktop-arm64-installer.exe"; status.AssetURL != want {
		t.Fatalf("AssetURL = %q; 期望 %q", status.AssetURL, want)
	}
}

func TestUpdateRejectsInsecureAssetURL(t *testing.T) {
	withWindowsUpdateAssets(t)
	updateTestVersion(t, "1.0.0")
	updateTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			assets := `{"name":"` + updateAssetName + `","browser_download_url":"http://example.com/` + updateAssetName + `"}`
			_, _ = io.WriteString(w, updateTestRelease("v1.1.0", "", assets))
		case "/recommended.json":
			_, _ = io.WriteString(w, updateTestEmptyRecommended)
		default:
			http.NotFound(w, r)
		}
	})
	status := updateTestCheck(t, &Manager{root: t.TempDir()})
	if status.AssetURL != "" {
		t.Fatalf("接受了非 HTTPS 的安装包地址：%q", status.AssetURL)
	}
}

func TestUpdateReportsReadableErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		want string
	}{
		{"限流", http.StatusForbidden, "GitHub 接口限流"},
		{"请求过多", http.StatusTooManyRequests, "GitHub 接口限流"},
		// 「还没有发布版本」不是失败：按「没有更新」返回，界面上不该显示成错误。
		{"服务异常", http.StatusInternalServerError, "HTTP 500"},
	} {
		t.Run(test.name, func(t *testing.T) {
			updateTestVersion(t, "1.0.0")
			updateTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/releases/latest":
					w.WriteHeader(test.code)
				case "/recommended.json":
					_, _ = io.WriteString(w, `{"version":"1.2.0"}`)
				default:
					http.NotFound(w, r)
				}
			})
			m := &Manager{root: t.TempDir(), update: UpdateStatus{Latest: "9.9.9", Available: true}}
			status := updateTestCheck(t, m)
			if !strings.Contains(status.Error, test.want) {
				t.Fatalf("Error = %q; 期望包含 %q", status.Error, test.want)
			}
			if status.Latest != "9.9.9" || !status.Available {
				t.Fatalf("上一轮结果被失败抹掉了：%+v", status)
			}
			if status.Checking || status.CheckedAt == 0 {
				t.Fatalf("检查状态没有收尾：%+v", status)
			}
			// 推荐版本独立于发布接口，失败时也应当刷新。
			if status.Recommended != "1.2.0" {
				t.Fatalf("Recommended = %q; 期望 1.2.0", status.Recommended)
			}
		})
	}
}

func TestUpdateReportsUnparsableTag(t *testing.T) {
	updateTestVersion(t, "1.0.0")
	updateTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			_, _ = io.WriteString(w, updateTestRelease("nightly", "", ""))
		case "/recommended.json":
			_, _ = io.WriteString(w, updateTestEmptyRecommended)
		default:
			http.NotFound(w, r)
		}
	})
	status := updateTestCheck(t, &Manager{root: t.TempDir()})
	if !strings.Contains(status.Error, "无法识别 GitHub 上的版本号") {
		t.Fatalf("Error = %q", status.Error)
	}
}

// ── 自动检查节流 ─────────────────────────────────────────────────

func TestUpdateSkipsAutomaticCheckWithinAnHour(t *testing.T) {
	updateTestVersion(t, "1.0.0")
	var requests atomic.Int32
	updateTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			requests.Add(1)
			_, _ = io.WriteString(w, updateTestRelease("v1.1.0", "", ""))
		case "/recommended.json":
			_, _ = io.WriteString(w, updateTestEmptyRecommended)
		default:
			http.NotFound(w, r)
		}
	})
	m := &Manager{root: t.TempDir()}
	if err := m.CheckUpdate(false); err != nil {
		t.Fatal(err)
	}
	updateTestWait(t, m)
	if err := m.CheckUpdate(false); err != nil {
		t.Fatal(err)
	}
	updateTestWait(t, m)
	if got := requests.Load(); got != 1 {
		t.Fatalf("自动检查重复请求了 GitHub：%d 次", got)
	}
	// 用户点「检查更新」时传 force，必须绕过节流。
	if err := m.CheckUpdate(true); err != nil {
		t.Fatal(err)
	}
	updateTestWait(t, m)
	if got := requests.Load(); got != 2 {
		t.Fatalf("手动检查没有生效：%d 次请求", got)
	}
}

// ── recommended.json ────────────────────────────────────────────

func TestRecommendedVersionFromRecommendedJSON(t *testing.T) {
	const current = "0.1.0-alpha.5"
	for _, test := range []struct {
		name string
		file string
		want string
	}{
		{"推荐正式版", `{"version":"1.2.0","min_launcher":"0.1.0-alpha.4"}`, "1.2.0"},
		{"min_launcher 与当前相同", `{"version":"1.2.0","min_launcher":"0.1.0-alpha.5"}`, "1.2.0"},
		{"min_launcher 留空", `{"version":"1.2.0"}`, "1.2.0"},
		{"预发布不推荐", `{"version":"1.3.0a1","min_launcher":""}`, recommendedVersion},
		// 当前是 0.1.0-alpha.5，0.1.0 正式版更高，说明这台机器装不上推荐的版本。
		{"启动器版本不够", `{"version":"1.2.0","min_launcher":"0.1.0"}`, recommendedVersion},
		{"min_launcher 更高", `{"version":"1.2.0","min_launcher":"0.2.0"}`, recommendedVersion},
		{"空清单", updateTestEmptyRecommended, recommendedVersion},
		{"版本号不合法", `{"version":"latest"}`, recommendedVersion},
		{"无法解析", `not json`, recommendedVersion},
	} {
		t.Run(test.name, func(t *testing.T) {
			updateTestVersion(t, current)
			updateTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/releases/latest":
					_, _ = io.WriteString(w, updateTestRelease("v0.1.0-alpha.6", "", ""))
				case "/recommended.json":
					_, _ = io.WriteString(w, test.file)
				default:
					http.NotFound(w, r)
				}
			})
			m := &Manager{root: t.TempDir()}
			status := updateTestCheck(t, m)
			if status.Recommended != test.want {
				t.Fatalf("Recommended = %q; 期望 %q", status.Recommended, test.want)
			}
			// Snapshot 会把它透出给「一键准备」使用。
			if got := m.Snapshot().Recommended; got != test.want {
				t.Fatalf("Snapshot.Recommended = %q; 期望 %q", got, test.want)
			}
		})
	}
}

func TestRecommendedFallsBackWhenListUnavailable(t *testing.T) {
	updateTestVersion(t, "1.0.0")
	updateTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			_, _ = io.WriteString(w, updateTestRelease("v1.1.0", "", ""))
		case "/recommended.json":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	m := &Manager{root: t.TempDir()}
	status := updateTestCheck(t, m)
	if status.Error != "" {
		t.Fatalf("recommended.json 失败不该影响更新检查：%s", status.Error)
	}
	if !status.Available {
		t.Fatal("发布信息有效时应当发现新版本")
	}
	if got := m.Snapshot().Recommended; got != recommendedVersion {
		t.Fatalf("Snapshot.Recommended = %q; 期望回落 %q", got, recommendedVersion)
	}
}

func TestRecommendedUsesSixHourCache(t *testing.T) {
	m := &Manager{root: t.TempDir(), update: UpdateStatus{Recommended: "1.2.0", CheckedAt: time.Now().Unix()}}
	if got := m.Snapshot().Recommended; got != "1.2.0" {
		t.Fatalf("新鲜缓存 = %q; 期望 1.2.0", got)
	}
	m.mu.Lock()
	m.update.CheckedAt = time.Now().Add(-7 * time.Hour).Unix()
	m.mu.Unlock()
	if got := m.Snapshot().Recommended; got != recommendedVersion {
		t.Fatalf("过期缓存 = %q; 期望回落 %q", got, recommendedVersion)
	}
	m.mu.Lock()
	m.update.CheckedAt = time.Now().Unix()
	m.update.Recommended = ""
	m.mu.Unlock()
	if got := m.Snapshot().Recommended; got != recommendedVersion {
		t.Fatalf("空推荐 = %q; 期望回落 %q", got, recommendedVersion)
	}
}

// ── 下载与校验 ───────────────────────────────────────────────────

// updateTestDownloadServer 起一个提供安装包与校验文件的服务器，返回安装包内容与地址前缀。
func updateTestDownloadServer(t *testing.T, content, checksums []byte) (installerURL, checksumsURL string) {
	t.Helper()
	var base string
	updateTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/" + updateAssetName:
			_, _ = w.Write(content)
		case "/download/checksums.txt":
			if checksums == nil {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(checksums)
		default:
			http.NotFound(w, r)
		}
	})
	base = strings.TrimSuffix(strings.TrimSpace(githubLatestReleaseURL), "/releases/latest")
	return base + "/download/" + updateAssetName, base + "/download/checksums.txt"
}

func TestDownloadUpdateWritesAndVerifiesInstaller(t *testing.T) {
	withWindowsUpdateAssets(t)
	content := []byte("NSIS installer fixture")
	sum := sha256.Sum256(content)
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + updateAssetName + "\n" +
		hex.EncodeToString(sum[:]) + " *neobot-desktop-amd64.zip\n")
	installerURL, checksumsURL := updateTestDownloadServer(t, content, checksums)

	root := t.TempDir()
	m := &Manager{root: root, update: UpdateStatus{AssetURL: installerURL, ChecksumsURL: checksumsURL}}
	path, err := m.DownloadUpdate()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "updates", updateAssetName)
	if path != want {
		t.Fatalf("安装包路径 = %q; 期望 %q", path, want)
	}
	if data, readErr := os.ReadFile(path); readErr != nil || string(data) != string(content) {
		t.Fatalf("安装包内容 = %q, %v", data, readErr)
	}
	if got := m.Snapshot().Update.Downloaded; got != path {
		t.Fatalf("Downloaded = %q; 期望 %q", got, path)
	}
	// 只留下安装包本身，.part 临时文件必须清干净。
	entries, err := os.ReadDir(filepath.Join(root, "updates"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != updateAssetName {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("更新目录里的文件 = %v", names)
	}
	// 已经下载过就直接复用，不该重新下载。
	if again, err := m.DownloadUpdate(); err != nil || again != path {
		t.Fatalf("重复下载 = %q, %v; 期望复用 %q", again, err, path)
	}
}

func TestDownloadUpdateRejectsBadChecksum(t *testing.T) {
	withWindowsUpdateAssets(t)
	content := []byte("NSIS installer fixture")
	checksums := []byte(strings.Repeat("0", 64) + "  " + updateAssetName + "\n")
	installerURL, checksumsURL := updateTestDownloadServer(t, content, checksums)

	root := t.TempDir()
	m := &Manager{root: root, update: UpdateStatus{AssetURL: installerURL, ChecksumsURL: checksumsURL}}
	if _, err := m.DownloadUpdate(); err == nil || !strings.Contains(err.Error(), "安装包校验失败") {
		t.Fatalf("校验不匹配没有被拦下：%v", err)
	}
	if isFile(filepath.Join(root, "updates", updateAssetName)) {
		t.Fatal("校验失败的安装包被留在了磁盘上")
	}
	if got := m.Snapshot().Update.Downloaded; got != "" {
		t.Fatalf("Downloaded = %q; 校验失败时不该记下路径", got)
	}
}

func TestDownloadUpdateRejectsChecksumsWithoutInstaller(t *testing.T) {
	withWindowsUpdateAssets(t)
	content := []byte("NSIS installer fixture")
	checksums := []byte(strings.Repeat("a", 64) + "  neobot-desktop-amd64.zip\n")
	installerURL, checksumsURL := updateTestDownloadServer(t, content, checksums)

	if _, err := (&Manager{root: t.TempDir(), update: UpdateStatus{AssetURL: installerURL, ChecksumsURL: checksumsURL}}).DownloadUpdate(); err == nil ||
		!strings.Contains(err.Error(), "校验文件里没有这个安装包") {
		t.Fatalf("缺少校验记录没有被拦下：%v", err)
	}
}

func TestDownloadUpdateRejectsOversizedInstaller(t *testing.T) {
	withWindowsUpdateAssets(t)
	installerURL, _ := updateTestDownloadServer(t, []byte("too large"), nil)
	previous := maxUpdateBytes
	maxUpdateBytes = 4
	t.Cleanup(func() { maxUpdateBytes = previous })

	root := t.TempDir()
	if _, err := (&Manager{root: root, update: UpdateStatus{AssetURL: installerURL}}).DownloadUpdate(); err == nil ||
		!strings.Contains(err.Error(), "手动下载") {
		t.Fatalf("超大安装包没有被拦下：%v", err)
	}
	if isFile(filepath.Join(root, "updates", updateAssetName)) {
		t.Fatal("超大安装包被留在了磁盘上")
	}
}

func TestDownloadUpdateNeedsAssetURL(t *testing.T) {
	m := &Manager{root: t.TempDir()}
	if _, err := m.DownloadUpdate(); err == nil || !strings.Contains(err.Error(), "请先检查更新") {
		t.Fatalf("没有安装包地址时的错误 = %v", err)
	}
}

func TestDownloadUpdateReportsHTTPFailure(t *testing.T) {
	withWindowsUpdateAssets(t)
	var base string
	updateTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	base = strings.TrimSuffix(strings.TrimSpace(githubLatestReleaseURL), "/releases/latest")
	m := &Manager{root: t.TempDir(), update: UpdateStatus{AssetURL: base + "/download/" + updateAssetName}}
	if _, err := m.DownloadUpdate(); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("下载失败的错误 = %v", err)
	}
}

// TestLinuxUpdateDoesNotOfferWindowsInstaller 验证 Linux 不会把 Windows 安装包
// 当成可下载资产；更新卡片应引导用户到发布页手动下载。
func TestLinuxUpdateDoesNotOfferWindowsInstaller(t *testing.T) {
	if updateAutoInstall {
		t.Skip("仅验证非自动更新平台")
	}
	updateTestVersion(t, "1.0.0")
	var base string
	updateTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			assets := `{"name":"neobot-desktop-amd64-installer.exe","browser_download_url":"` + base + `/download/neobot-desktop-amd64-installer.exe"},` +
				`{"name":"checksums.txt","browser_download_url":"` + base + `/download/checksums.txt"}`
			_, _ = io.WriteString(w, updateTestRelease("v1.1.0", "", assets))
		case "/recommended.json":
			_, _ = io.WriteString(w, updateTestEmptyRecommended)
		default:
			http.NotFound(w, r)
		}
	})
	base = strings.TrimSuffix(strings.TrimSpace(githubLatestReleaseURL), "/releases/latest")

	status := updateTestCheck(t, &Manager{root: t.TempDir()})
	if status.Error != "" {
		t.Fatal(status.Error)
	}
	if !status.Available {
		t.Fatal("newer release should still be reported as available")
	}
	if status.AutoUpdate || status.AssetURL != "" || status.ChecksumsURL != "" {
		t.Fatalf("Linux offered an installer: auto=%v asset=%q checksums=%q", status.AutoUpdate, status.AssetURL, status.ChecksumsURL)
	}
}

// ── 启动安装包 ───────────────────────────────────────────────────

func TestLaunchUpdateNeedsDownloadedInstaller(t *testing.T) {
	withWindowsUpdateAssets(t)
	m := &Manager{root: t.TempDir()}
	if err := m.LaunchUpdate(); err == nil || !strings.Contains(err.Error(), "请先下载") {
		t.Fatalf("没有安装包时的错误 = %v", err)
	}
	missing := filepath.Join(m.root, "updates", updateAssetName)
	m.update.Downloaded = missing
	if err := m.LaunchUpdate(); err == nil || !strings.Contains(err.Error(), "重新下载") {
		t.Fatalf("安装包丢失时的错误 = %v", err)
	}
	if got := m.Snapshot().Update.Downloaded; got != "" {
		t.Fatalf("失效的安装包路径没有被清掉：%q", got)
	}
	// 文件在但不是可执行安装包：要把 exec 的原始错误换成用户看得懂的提示。
	if err := os.MkdirAll(filepath.Dir(missing), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(missing, []byte("not an installer"), 0600); err != nil {
		t.Fatal(err)
	}
	m.update.Downloaded = missing
	// 启动安装包这一步在测试里替换掉：真的起一个 cmd 会留下后台进程。
	original := updateLauncher
	updateLauncher = func(string) error { return errors.New("launch failed") }
	t.Cleanup(func() { updateLauncher = original })
	if err := m.LaunchUpdate(); err == nil || !strings.Contains(err.Error(), "手动运行") {
		t.Fatalf("无法启动安装包时的错误 = %v", err)
	}
}
