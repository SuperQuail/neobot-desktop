package launcher

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// userAgent identifies the launcher to mirrors. Some of them reject requests
	// carrying Go's default User-Agent ("Go-http-client/1.1") on their file paths,
	// so every download names the launcher instead.
	userAgent = "NeoBot-Desktop"
)

type userAgentTransport struct{ base http.RoundTripper }

func (t userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", userAgent)
	}
	return t.base.RoundTrip(req)
}

var downloads = &http.Client{
	Timeout: 2 * time.Minute,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) >= 10 {
			return errors.New("无效的下载重定向")
		}
		return nil
	},
	Transport: userAgentTransport{base: http.DefaultTransport},
}

func (m *Manager) Available() ([]Version, error) {
	m.mu.Lock()
	sources := m.state.Sources
	m.mu.Unlock()
	versions, err := availableAt(sources.Index)
	if shouldFallBack(sources, versions, err) {
		return availableAt(officialIndex)
	}
	return versions, err
}

// shouldFallBack reports whether a version list from these sources is unusable
// and the official index should be tried instead. An error always falls back;
// an empty list only does for the accelerated preset, because a custom index is
// the operator's own choice and should not be answered with pypi.org versions.
func shouldFallBack(sources Sources, versions []Version, err error) bool {
	if sources.Index == officialIndex {
		return false
	}
	return err != nil || (len(versions) == 0 && sources.Preset == "accelerated")
}

func availableAt(index string) ([]Version, error) {
	page := index + "/neobot-app/"
	req, err := http.NewRequest(http.MethodGet, page, nil)
	if err != nil {
		return nil, errors.New("软件包索引地址无效")
	}
	req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")
	resp, err := downloads.Do(req)
	if err != nil {
		return nil, errors.New("无法获取版本列表，请检查网络后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("无法读取 NeoBot 版本列表")
	}
	// Resolve links against the address that answered, not the one requested: a
	// mirror may redirect the project page to another prefix.
	files, err := parseSimpleFiles(io.LimitReader(resp.Body, 8<<20), resp.Header.Get("Content-Type"), resp.Request.URL.String())
	if err != nil {
		return nil, errors.New("无法解析 NeoBot 版本列表")
	}
	available := map[string]bool{}
	for _, file := range files {
		if file.Yanked {
			continue
		}
		parts := strings.Split(file.Filename, "-")
		if len(parts) < 2 || (parts[0] != "neobot_app" && parts[0] != "neobot.app") {
			continue
		}
		version := strings.TrimSuffix(strings.TrimSuffix(parts[1], ".tar.gz"), ".zip")
		if versionPattern.MatchString(version) {
			available[version] = true
		}
	}
	result := make([]Version, 0, len(available))
	for version := range available {
		result = append(result, versionOf(version))
	}
	sort.Slice(result, func(i, j int) bool { return compareVersions(result[i].Name, result[j].Name) > 0 })
	return result, nil
}

type simpleFile struct {
	Filename string
	URL      string
	Yanked   bool
}

// parseSimpleFiles reads a PEP 503 project page. Indexes answer with PEP 691
// JSON when asked, HTML otherwise; either form may use links relative to the
// project page.
func parseSimpleFiles(r io.Reader, contentType, page string) ([]simpleFile, error) {
	base, err := url.Parse(page)
	if err != nil {
		return nil, err
	}
	if strings.Contains(contentType, "json") {
		var list struct {
			Files []struct {
				Filename string `json:"filename"`
				URL      string `json:"url"`
				Yanked   any    `json:"yanked"`
			} `json:"files"`
		}
		if err := json.NewDecoder(r).Decode(&list); err != nil {
			return nil, err
		}
		files := make([]simpleFile, 0, len(list.Files))
		for _, f := range list.Files {
			yanked, _ := f.Yanked.(bool)
			_, hasReason := f.Yanked.(string)
			file := simpleFile{Filename: f.Filename, Yanked: yanked || hasReason}
			if u := resolveLink(base, f.URL); u != nil {
				file.URL = u.String()
			}
			files = append(files, file)
		}
		return files, nil
	}
	// PEP 503 indexes may return HTML when they have not implemented JSON.
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var files []simpleFile
	for _, match := range simpleLink.FindAllStringSubmatch(string(body), -1) {
		u := resolveLink(base, match[1])
		if u == nil {
			continue
		}
		files = append(files, simpleFile{
			Filename: filepath.Base(u.Path),
			URL:      u.String(),
			Yanked:   strings.Contains(strings.ToLower(match[0]), "data-yanked"),
		})
	}
	return files, nil
}

// resolveLink makes an index link absolute; mirrors may answer with paths
// relative to the project page. A PEP 503 link often carries a #sha256 fragment,
// which is not part of the download.
func resolveLink(base *url.URL, link string) *url.URL {
	if link == "" || strings.HasPrefix(link, "#") {
		return nil
	}
	u, err := url.Parse(link)
	if err != nil {
		return nil
	}
	u = base.ResolveReference(u)
	// Only web links can be downloaded; anything else would fail far from here.
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil
	}
	u.Fragment = ""
	return u
}

// uvWheelURL finds the current platform's wheel of the pinned uv release on a
// PEP 503 index. The wheel carries the same uv executable as the GitHub archive.
func uvWheelURL(ctx context.Context, index string) (string, error) {
	page := index + "/uv/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
	if err != nil {
		return "", errors.New("软件包索引地址无效")
	}
	req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")
	resp, err := downloads.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", errors.New("任务已取消或超时")
		}
		return "", errors.New("无法获取 uv 版本列表，请检查网络后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", errors.New("无法读取 uv 版本列表")
	}
	// The uv page lists every release for every platform and keeps growing (2.4 MB
	// when this was written), so it gets a wider cap than the neobot-app page. An
	// overflowing JSON answer fails to decode; only the HTML fallback is truncated
	// silently, which would hide the newest releases at the end.
	files, err := parseSimpleFiles(io.LimitReader(resp.Body, 32<<20), resp.Header.Get("Content-Type"), resp.Request.URL.String())
	if err != nil {
		if ctx.Err() != nil {
			return "", errors.New("任务已取消或超时")
		}
		return "", errors.New("无法解析 uv 版本列表")
	}
	// 平台匹配规则见 uv_<platform>.go：Windows 接受 win_amd64 wheel，
	// Linux 只接受固定版本的 manylinux wheel（排除 musllinux 等变体）。
	address := ""
	for _, file := range files {
		if file.Yanked || file.URL == "" || !matchUVWheel(file.Filename) {
			continue
		}
		// Some mirrors list the same file twice; only distinct addresses are
		// ambiguous.
		if file.URL == address {
			continue
		}
		if address != "" {
			return "", fmt.Errorf("下载源提供了多个 uv %s 发行包，请更换下载源后重试", uvVersion)
		}
		address = file.URL
	}
	if address == "" {
		return "", fmt.Errorf("下载源没有提供 uv %s，可在设置中切换为官方源后重试", uvVersion)
	}
	return address, nil
}

func (m *Manager) Install(version string, official, start bool) error {
	if !supported() {
		return errors.New("当前系统暂不支持安装 NeoBot")
	}
	if !versionPattern.MatchString(version) {
		return errors.New("请选择 NeoBot 发行版本")
	}
	m.mu.Lock()
	if m.closing || m.job.State == "running" {
		m.mu.Unlock()
		return errors.New("已有安装任务正在运行")
	}
	if start && (m.bot != nil || m.preparing) {
		m.mu.Unlock()
		return errors.New("请先停止正在运行的 NeoBot")
	}
	if _, installed := m.installed[version]; installed {
		m.mu.Unlock()
		if !start {
			return nil
		}
		if err := m.SelectVersion(version); err != nil {
			return err
		}
		return m.Start(false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	m.jobCancel = cancel
	done := make(chan struct{})
	m.jobDone = done
	sources := m.state.Sources
	if official {
		sources = defaultSources("official")
	}
	m.job = Job{Version: version, Step: "uv", State: "running", Total: 3, StartAfter: start}
	m.mu.Unlock()
	m.notify()

	go func() {
		defer cancel()
		defer close(done)
		err := m.install(ctx, version, sources)
		m.mu.Lock()
		if err == nil {
			m.installed[version] = versionOf(version)
			if start || m.state.Selected == "" {
				before := m.state.Selected
				m.state.Selected = version
				if err = m.persist(); err != nil {
					m.state.Selected = before
				}
			}
		}
		m.jobCancel = nil
		if err != nil {
			m.job.State, m.job.Message = "failed", err.Error()
		} else {
			m.job.State, m.job.Completed, m.job.Message = "done", 3, "NeoBot 安装完成"
		}
		m.mu.Unlock()
		m.notify()
		if err == nil && start {
			if err := m.Start(false); err != nil {
				m.setRuntimeError(err.Error())
			}
		}
	}()
	return nil
}

func (m *Manager) setStep(step, message string, done int) {
	m.mu.Lock()
	m.job.Step, m.job.Message, m.job.Completed = step, message, done
	m.mu.Unlock()
	m.notify()
}

func (m *Manager) install(ctx context.Context, version string, sources Sources) error {
	for _, dir := range []string{"tools", "python", "cache", "versions"} {
		if err := os.MkdirAll(filepath.Join(m.root, dir), 0700); err != nil {
			return err
		}
	}
	m.setStep("uv", "正在准备 uv", 0)
	uv, err := m.ensureUV(ctx, sources)
	if err != nil {
		return fmt.Errorf("下载 uv 失败：%w", err)
	}
	env := m.installEnv()
	m.setStep("python", "正在准备 Python 3.13", 1)
	if err := m.ensurePython(ctx, uv, env, sources); err != nil {
		return fmt.Errorf("安装 Python 失败：%w", err)
	}

	m.setStep("neobot", "正在安装 NeoBot "+version, 2)
	dir := m.versionDir(version)
	python := venvPython(dir)
	if !isFile(python) {
		if err := runUV(ctx, uv, env, "venv", "--allow-existing", "--python", "3.13", "--no-python-downloads", dir); err != nil {
			return fmt.Errorf("创建 NeoBot 环境失败：%w", err)
		}
	}
	if err := runUV(ctx, uv, env, "pip", "install", "--python", python, "--default-index", sources.Index, "neobot-app=="+version); err != nil {
		return fmt.Errorf("安装 NeoBot 失败：%w", err)
	}
	if err := runUV(ctx, uv, env, "pip", "check", "--python", python); err != nil {
		return fmt.Errorf("检查 NeoBot 依赖失败：%w", err)
	}
	if !isFile(m.binary(version)) {
		return errors.New("安装完成后没有找到 NeoBot 启动程序")
	}
	return writeInstallRecord(dir, installRecord{Name: version, Preview: versionOf(version).Preview})
}

func (m *Manager) uvEnv() []string {
	return []string{
		"UV_CACHE_DIR=" + filepath.Join(m.root, "cache"),
		"UV_PYTHON_INSTALL_DIR=" + filepath.Join(m.root, "python"),
		"UV_PYTHON_PREFERENCE=only-managed",
	}
}

func (m *Manager) installEnv() []string {
	return append(m.uvEnv(), "UV_NO_CONFIG=1")
}

func (m *Manager) ensureUV(ctx context.Context, sources Sources) (string, error) {
	uv := uvExecutable(m.root)
	// A killed install can leave a partial download behind, even when the
	// executable itself was already renamed into place.
	if entries, err := os.ReadDir(filepath.Join(m.root, "tools")); err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".uv-") {
				_ = os.Remove(filepath.Join(m.root, "tools", entry.Name()))
			}
		}
	}
	if isFile(uv) {
		return uv, nil
	}
	// The accelerated preset takes uv from the same index as the packages; every
	// other preset keeps the GitHub release archive.
	if sources.Preset == "accelerated" {
		address, err := uvWheelURL(ctx, sources.Index)
		if err != nil {
			return "", err
		}
		return uv, installUV(ctx, uv, address, uvWheelSHA256, uvWheelEntry)
	}
	address := sources.UV + "/" + uvVersion + "/" + uvArchiveFile
	return uv, installUV(ctx, uv, address, uvArchiveSHA256, uvArchiveEntry)
}

func (m *Manager) ensurePython(ctx context.Context, uv string, env []string, sources Sources) error {
	args := []string{"python", "install", "3.13", "--no-bin", "--no-registry"}
	if sources.Python != "" {
		args = append(args, "--mirror", sources.Python)
	}
	return runUV(ctx, uv, env, args...)
}

func cleanEnv(clearUV bool, extra ...string) []string {
	env := make([]string, 0)
	for _, kv := range os.Environ() {
		name := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
		if clearUV && (strings.HasPrefix(name, "UV_") || name == "VIRTUAL_ENV" || name == "PYTHONHOME" || name == "PYTHONPATH") {
			continue
		}
		if !clearUV && (strings.HasPrefix(name, "NEOBOT_") || name == "PYTHONHOME" || name == "PYTHONPATH") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

func runUV(ctx context.Context, uv string, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, uv, args...)
	cmd.Env = cleanEnv(true, env...)
	hideWindow(cmd)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.New("任务已取消或超时")
		}
		// uv output can contain credentials inherited through proxy configuration.
		return errors.New("命令执行失败，请检查下载源、网络或磁盘空间后重试")
	}
	return nil
}

func getDownload(ctx context.Context, address string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	resp, err := downloads.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("任务已取消或超时")
		}
		return nil, errors.New("无法连接下载源")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("下载源返回 HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func installUV(ctx context.Context, target, address, expectedSHA256, entry string) error {
	resp, err := getDownload(ctx, address)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	tmp, err := os.CreateTemp(filepath.Dir(target), ".uv-*")
	if err != nil {
		return errors.New("无法创建 uv 临时文件，请检查磁盘空间")
	}
	defer os.Remove(tmp.Name())
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(resp.Body, 64<<20))
	closeErr := tmp.Close()
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("任务已取消或超时")
		}
		return errors.New("无法完成 uv 下载，请检查网络或磁盘空间后重试")
	}
	if closeErr != nil {
		return errors.New("无法写入 uv 临时文件，请检查磁盘空间")
	}
	if n == 64<<20 || expectedSHA256 != hex.EncodeToString(hash.Sum(nil)) {
		return errors.New("uv 下载不完整或校验失败")
	}
	return extractUV(tmp.Name(), target, entry)
}

// uvMaxBytes 是单个 uv 可执行文件的大小上限。
const uvMaxBytes = 96 << 20

// extractUV 从 uv 发行包里取出指定的可执行文件。官方源是 .tar.gz，加速源是
// wheel（zip）；这里按文件头魔数判断格式，不依赖下载地址的后缀。
func extractUV(archive, target, entry string) error {
	file, err := os.Open(archive)
	if err != nil {
		return errors.New("uv 压缩包无效")
	}
	defer file.Close()
	var magic [2]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil {
		return errors.New("uv 压缩包无效")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return errors.New("uv 压缩包无效")
	}
	if magic[0] == 0x1f && magic[1] == 0x8b {
		return extractUVTarGz(file, target, entry)
	}
	return extractUVZip(archive, target, entry)
}

func extractUVZip(archive, target, entry string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return errors.New("uv 压缩包无效")
	}
	defer r.Close()
	for _, file := range r.File {
		// Only the pinned entry is extracted; anything else, including paths that
		// try to escape the target directory, is skipped.
		if file.Name != entry || !file.Mode().IsRegular() {
			continue
		}
		if file.UncompressedSize64 > uvMaxBytes {
			return errors.New("uv 文件过大")
		}
		in, err := file.Open()
		if err != nil {
			return errors.New("无法读取 uv 压缩包")
		}
		defer in.Close()
		return writeUVFile(in, target)
	}
	return errors.New("压缩包缺少 uv 可执行文件")
}

func extractUVTarGz(reader io.Reader, target, entry string) error {
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return errors.New("uv 压缩包无效")
	}
	defer gz.Close()
	tarReader := tar.NewReader(gz)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.New("uv 压缩包无效")
		}
		if header.Name != entry {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return errors.New("uv 压缩包里的 uv 不是普通文件")
		}
		if header.Size > uvMaxBytes {
			return errors.New("uv 文件过大")
		}
		return writeUVFile(tarReader, target)
	}
	return errors.New("压缩包缺少 uv 可执行文件")
}

// writeUVFile 把唯一一个可执行文件写到目标位置，并补上可执行位。
func writeUVFile(source io.Reader, target string) error {
	out, err := os.CreateTemp(filepath.Dir(target), ".uv-*")
	if err != nil {
		return errors.New("无法创建 uv 临时文件，请检查磁盘空间")
	}
	defer os.Remove(out.Name())
	written, err := io.Copy(out, io.LimitReader(source, uvMaxBytes+1))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.New("无法写入 uv 文件，请检查磁盘空间")
	}
	if written > uvMaxBytes {
		return errors.New("uv 文件过大")
	}
	if err := os.Rename(out.Name(), target); err != nil {
		return errors.New("无法保存 uv 可执行文件")
	}
	return makeExecutable(target)
}
