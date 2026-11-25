package launcher

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	branchArchiveLimit = int64(256 << 20)
	branchSourceLimit  = int64(2 << 30)
	branchFileLimit    = int64(512 << 20)
	branchMaxFiles     = 100000
)

var (
	branchIDPattern   = regexp.MustCompile(`^branch-[a-z0-9_][a-z0-9._-]{0,39}-[0-9a-f]{8}$`)
	branchSlugPattern = regexp.MustCompile(`[^a-z0-9._-]+`)
)

func validBranchName(name string) bool {
	if name == "" || len(name) > 200 || !utf8.ValidString(name) || name == "@" ||
		strings.ContainsAny(name, " ~^:?*[\\") || strings.ContainsFunc(name, unicode.IsControl) {
		return false
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.HasSuffix(name, ".") || strings.Contains(name, "..") || strings.Contains(name, "//") ||
		strings.Contains(name, "@{") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func branchID(name string) string {
	slug := strings.ToLower(name)
	slug = branchSlugPattern.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, ".-")
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	if len(slug) > 40 {
		slug = strings.TrimRight(slug[:40], ".-")
	}
	if slug == "" {
		slug = "ref"
	}
	sum := sha256.Sum256([]byte(name))
	return "branch-" + slug + "-" + hex.EncodeToString(sum[:4])
}

func isBranchID(name string) bool {
	return branchIDPattern.MatchString(name)
}

// checkBranchInstall checks the shared state before starting an installation. Caller holds m.mu.
func (m *Manager) checkBranchInstall(start bool) error {
	if m.closing || m.job.State == "running" {
		return errors.New("已有安装任务正在运行")
	}
	if m.preparing || m.stopping {
		return errors.New("请等待当前启动任务结束")
	}
	if start && m.bot != nil {
		return errors.New("请先停止正在运行的 NeoBot")
	}
	return nil
}

func (m *Manager) InstallBranch(repository, branch string, start, refresh bool) error {
	if !supported() {
		return errors.New("当前系统暂不支持安装 NeoBot")
	}
	repository, err := normalizeGitHubRepo(repository)
	if err != nil {
		return err
	}
	// Trim paste whitespace that cannot occur in a Git ref.
	branch = strings.Trim(branch, " \t\r\n\v\f")
	if branch == "" {
		m.mu.Lock()
		err = m.checkBranchInstall(start)
		m.mu.Unlock()
		if err != nil {
			return err
		}
		// Resolve without holding the manager lock, then recheck state below.
		branch, err = fetchGitHubDefaultBranch(context.Background(), repository)
		if err != nil {
			return err
		}
	} else if !validBranchName(branch) {
		return errors.New("请输入有效的 GitHub 分支名")
	}
	id := repositoryBranchID(repository, branch)
	m.mu.Lock()
	if err := m.checkBranchInstall(start); err != nil {
		m.mu.Unlock()
		return err
	}
	_, installed := m.installed[id]
	if installed && !refresh {
		m.mu.Unlock()
		if !start {
			return nil
		}
		if err := m.SelectVersion(id); err != nil {
			return err
		}
		return m.Start(false)
	}
	if m.bot != nil && m.bot.version == id {
		m.mu.Unlock()
		return errors.New("请先停止正在运行的分支版本")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	m.jobCancel = cancel
	done := make(chan struct{})
	m.jobDone = done
	sources := m.state.Sources
	m.job = Job{Version: id, Repository: repository, Branch: branch, Step: "uv", State: "running", Total: 4, StartAfter: start}
	m.mu.Unlock()
	m.notify()

	go func() {
		defer cancel()
		defer close(done)
		err := m.installBranch(ctx, repository, branch, id, refresh, sources)
		m.mu.Lock()
		if err == nil {
			m.installed[id] = Version{Name: id, Repository: repository, Branch: branch}
			if start || m.state.Selected == "" {
				before := m.state.Selected
				m.state.Selected = id
				if persistErr := m.persist(); persistErr != nil {
					m.state.Selected = before
					err = errors.New("无法保存启动设置，请检查磁盘空间")
				}
			}
		}
		m.jobCancel = nil
		if err != nil {
			m.job.State, m.job.Message = "failed", err.Error()
			// A failed refresh may have left target unusable. Do not keep
			// offering to start a half-installed version in this session.
			record, recordErr := readInstallRecord(m.versionDir(id))
			version, valid := record.branchVersion(id)
			if recordErr != nil || !valid || version.Repository != repository || version.Branch != branch ||
				!isFile(m.binary(id)) || !isFile(venvPython(m.versionDir(id))) {
				delete(m.installed, id)
				if m.state.Selected == id {
					m.state.Selected = ""
					_ = m.persist()
				}
			}
		} else {
			m.job.State, m.job.Completed, m.job.Message = "done", 4, "分支 "+branch+" 安装完成"
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

func (m *Manager) installBranch(ctx context.Context, repository, branch, id string, refresh bool, sources Sources) (err error) {
	for _, dir := range []string{"tools", "python", "cache", "tmp", "versions"} {
		if err = os.MkdirAll(filepath.Join(m.root, dir), 0700); err != nil {
			return errors.New("无法创建安装目录，请检查磁盘空间")
		}
	}
	m.setStep("uv", "正在准备 uv", 0)
	uv, err := m.ensureUV(ctx, sources)
	if err != nil {
		return fmt.Errorf("下载 uv 失败：%w", err)
	}
	env := m.uvEnv()
	m.setStep("python", "正在准备 Python 3.13", 1)
	if err = m.ensurePython(ctx, uv, env, sources); err != nil {
		return fmt.Errorf("安装 Python 失败：%w", err)
	}

	m.setStep("source", "正在下载分支 "+branch+" 的源码", 2)
	work, err := os.MkdirTemp(filepath.Join(m.root, "tmp"), "branch-"+id+"-")
	if err != nil {
		return errors.New("无法创建分支临时目录，请检查磁盘空间")
	}
	defer os.RemoveAll(work)
	archive := filepath.Join(work, "source.zip")
	if err = downloadBranchArchive(ctx, repository, branch, archive); err != nil {
		return err
	}
	source, err := extractBranchSource(ctx, archive, filepath.Join(work, "src"))
	if err != nil {
		return err
	}

	target := m.versionDir(id)
	if refresh && isDir(target) {
		old := target + ".old-" + timeID()
		if err = os.Rename(target, old); err != nil {
			return errors.New("无法准备分支更新，请确认该版本已停止且文件未被占用")
		}
		defer func() {
			if err == nil {
				return
			}
			_ = os.RemoveAll(target)
			if restoreErr := os.Rename(old, target); restoreErr != nil {
				err = errors.Join(err, errors.New("恢复旧分支版本失败，请重启后重试"))
			}
		}()
	} else {
		if isDir(target) {
			if err = os.RemoveAll(target); err != nil {
				return errors.New("无法清理旧的分支安装目录，请重启后重试")
			}
		}
		defer func() {
			if err != nil {
				_ = os.RemoveAll(target)
			}
		}()
	}

	m.setStep("neobot", "正在安装分支 "+branch, 3)
	if err = runBranchSync(ctx, uv, env, source, target); err != nil {
		return err
	}
	python := venvPython(target)
	if !isFile(python) {
		return errors.New("安装完成后没有找到分支版本的 Python")
	}
	if err = runUV(ctx, uv, env, "pip", "check", "--python", python); err != nil {
		return fmt.Errorf("检查分支依赖失败：%w", err)
	}
	if !isFile(m.binary(id)) {
		return errors.New("安装完成后没有找到分支版本的启动程序")
	}
	if err = writeInstallRecord(target, installRecord{Name: id, Repository: repository, Branch: branch}); err != nil {
		return errors.New("无法写入分支安装记录，请检查磁盘空间")
	}
	if refresh {
		matches, _ := filepath.Glob(target + ".old-*")
		for _, match := range matches {
			_ = os.RemoveAll(match)
		}
	}
	return nil
}

func branchSyncArgs(source string) []string {
	return []string{
		"sync", "--project", source, "--package", "neobot-app",
		"--no-dev", "--no-editable", "--python", "3.13", "--no-python-downloads",
	}
}

func branchSyncEnv(env []string, target string) []string {
	cmdEnv := make([]string, 0, len(env)+1)
	cmdEnv = append(cmdEnv, env...)
	cmdEnv = append(cmdEnv, "UV_PROJECT_ENVIRONMENT="+target)
	return cleanEnv(true, cmdEnv...)
}

func runBranchSync(ctx context.Context, uv string, env []string, source, target string) error {
	cmd := exec.CommandContext(ctx, uv, branchSyncArgs(source)...)
	cmd.Env = branchSyncEnv(env, target)
	hideWindow(cmd)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.New("任务已取消或超时")
		}
		return errors.New("同步分支依赖失败，请检查网络、磁盘空间或该分支的 uv.lock")
	}
	return nil
}

func downloadBranchArchive(ctx context.Context, repository, branch, target string) error {
	address := strings.TrimRight(githubArchiveBase, "/") + "/" + repository + "/archive/refs/heads/" + escapeBranchPath(branch) + ".zip"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return errors.New("无法连接 GitHub 下载源码")
	}
	request.Header.Set("User-Agent", "neobot-desktop")
	response, err := branchDownloadHTTP.Do(request)
	if err != nil {
		return errors.New("无法连接 GitHub 下载源码，请检查网络")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("找不到分支 %s，请检查分支名", branch)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub 返回 HTTP %d，无法下载分支源码", response.StatusCode)
	}
	if response.ContentLength > branchArchiveLimit {
		return errors.New("分支源码压缩包过大")
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".branch-*.zip")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, branchArchiveLimit+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return errors.New("下载分支源码失败")
	}
	if written > branchArchiveLimit {
		return errors.New("分支源码压缩包过大")
	}
	if err := os.Rename(temp, target); err != nil {
		return err
	}
	return nil
}

func escapeBranchPath(branch string) string {
	parts := strings.Split(branch, "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return strings.Join(parts, "/")
}

func extractBranchSource(ctx context.Context, archive, destination string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", errors.New("任务已取消或超时")
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return "", errors.New("分支源码压缩包无效")
	}
	defer reader.Close()
	if err := os.MkdirAll(destination, 0700); err != nil {
		return "", err
	}
	var total int64
	files := 0
	for _, file := range reader.File {
		if err := ctx.Err(); err != nil {
			return "", errors.New("任务已取消或超时")
		}
		if !safeZipName(file.Name) {
			return "", errors.New("分支源码压缩包包含非法路径")
		}
		if file.FileInfo().IsDir() {
			continue
		}
		if file.Mode()&os.ModeSymlink != 0 || !file.Mode().IsRegular() {
			return "", errors.New("分支源码压缩包包含非普通文件")
		}
		files++
		if files > branchMaxFiles {
			return "", errors.New("分支源码文件过多")
		}
		if file.UncompressedSize64 > uint64(branchFileLimit) {
			return "", errors.New("分支源码文件过大")
		}
		target := filepath.Join(destination, filepath.FromSlash(path.Clean(file.Name)))
		relative, err := filepath.Rel(destination, target)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", errors.New("分支源码压缩包包含非法路径")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return "", err
		}
		input, err := file.Open()
		if err != nil {
			return "", err
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, copyFileMode(file.Mode()))
		if err != nil {
			input.Close()
			return "", errors.New("解压分支源码失败")
		}
		written, copyErr := io.Copy(output, io.LimitReader(input, branchFileLimit+1))
		input.Close()
		closeErr := output.Close()
		if copyErr != nil || closeErr != nil {
			return "", errors.New("解压分支源码失败")
		}
		if written > branchFileLimit {
			return "", errors.New("分支源码文件过大")
		}
		total += written
		if total > branchSourceLimit {
			return "", errors.New("分支源码解压后过大")
		}
	}
	root, err := findWorkspaceRoot(destination)
	if err != nil {
		if errors.Is(err, errNotWorkspace) {
			return "", errNotWorkspace
		}
		return "", errors.New("解压分支源码失败")
	}
	return root, nil
}

func safeZipName(name string) bool {
	if name == "" || strings.ContainsAny(name, "\\:") || strings.HasPrefix(name, "/") {
		return false
	}
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" {
		return false
	}
	for _, part := range strings.Split(trimmed, "/") {
		if part == "" || part == "." || part == ".." || unsafeZipPart(part) {
			return false
		}
	}
	cleaned := path.Clean(trimmed)
	return cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../") && !strings.HasPrefix(cleaned, "/")
}

func unsafeZipPart(part string) bool {
	if part == "" || strings.TrimSpace(part) != part {
		return true
	}
	if strings.TrimRight(part, ".") != part {
		return true
	}
	base := part
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	switch strings.ToUpper(base) {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$", "CLOCK$":
		return true
	}
	upper := strings.ToUpper(base)
	return len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) &&
		upper[3] >= '1' && upper[3] <= '9'
}

var errNotWorkspace = errors.New("下载的源码不是可安装的 NeoBot 工作区")

func findWorkspaceRoot(destination string) (string, error) {
	var candidates []string
	if isFile(filepath.Join(destination, "pyproject.toml")) {
		candidates = append(candidates, destination)
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "__MACOSX" {
			continue
		}
		root := filepath.Join(destination, entry.Name())
		if isFile(filepath.Join(root, "pyproject.toml")) {
			candidates = append(candidates, root)
		}
	}
	for _, root := range candidates {
		if isFile(filepath.Join(root, "app", "pyproject.toml")) && isDir(filepath.Join(root, "packages")) {
			return root, nil
		}
	}
	return "", errNotWorkspace
}
