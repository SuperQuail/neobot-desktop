package launcher

import (
	"bufio"
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
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// UpdateStatus 是启动器更新与推荐版本的状态。
type UpdateStatus struct {
	Current      string `json:"current"`
	Latest       string `json:"latest"`
	Available    bool   `json:"available"`
	Notes        string `json:"notes"`
	PageURL      string `json:"page_url"`
	AssetURL     string `json:"asset_url"`
	ChecksumsURL string `json:"checksums_url"`
	CheckedAt    int64  `json:"checked_at"`
	Checking     bool   `json:"checking"`
	Downloaded   string `json:"downloaded"`
	Error        string `json:"error"`
	Recommended  string `json:"recommended"`
	// AutoUpdate 表示当前平台支持应用内下载并安装更新。Linux 第一版只做手动下载。
	AutoUpdate bool `json:"auto_update"`
	// NoReleases 表示仓库还没有发布任何版本：这不是错误，只是暂时没得更新。
	NoReleases bool `json:"no_releases"`
}

// 自更新用到的接口地址。做成包级变量，测试才能换成 httptest 服务器。
var (
	// githubLatestReleaseURL 是启动器仓库的「最新发布」接口。
	githubLatestReleaseURL = "https://api.github.com/repos/SuperQuail/neobot-desktop/releases/latest"
	// recommendedJSONURL 指向主分支维护的推荐清单，版本留空表示暂不推荐。
	recommendedJSONURL = "https://raw.githubusercontent.com/SuperQuail/neobot-desktop/main/recommended.json"
)

const (
	// updateUserAgent 是自更新请求的 User-Agent；GitHub 拒绝没有 UA 的请求。
	updateUserAgent = "neobot-desktop"
	// updateCheckInterval 是自动检查的最小间隔；用户点「检查更新」时传 force，不受限制。
	updateCheckInterval = time.Hour
	// recommendedTTL 是推荐版本的缓存时长；过期后回落编译期常量，直到下一次检查成功。
	recommendedTTL = 6 * time.Hour
	// updateNotesLimit 限制发布说明的字符数：快照每次都会全量传给界面，不该塞进整篇 Markdown。
	updateNotesLimit = 2000
	// updateJSONLimit 限制 JSON 响应体大小。
	updateJSONLimit = int64(1 << 20)
	// updateChecksumLimit 限制 checksums.txt 的大小。
	updateChecksumLimit = int64(1 << 20)
)

// maxUpdateBytes 是安装包大小上限。测试可以调小，生产恒为 200 MB。
var maxUpdateBytes = int64(200 << 20)

// updateHTTPSOnly 只允许 https 重定向，挡住被降级到明文的下载。
func updateHTTPSOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" || req.URL.User != nil || len(via) >= 10 {
		return errors.New("无效的更新下载重定向")
	}
	return nil
}

var (
	// updateAPIHTTP 用于查询发布信息和校验文件，10 秒超时。
	updateAPIHTTP = &http.Client{Timeout: 10 * time.Second, CheckRedirect: updateHTTPSOnly}
	// updateDownloadHTTP 用于下载安装包，安装包可能上百 MB，给 10 分钟。
	updateDownloadHTTP = &http.Client{Timeout: 10 * time.Minute, CheckRedirect: updateHTTPSOnly}
)

// releaseTag 匹配发布标签去掉前缀 v 后的形式，例如 0.1.0、0.1.0-alpha.5、0.2.0rc.3。
// 发布流程用的是 SemVer 预发布写法（-alpha.N），versionPattern 只认 a/b/rcN，
// 所以比较前先由 normalizeVersion 统一。
var releaseTag = regexp.MustCompile(`^([0-9]{1,9})\.([0-9]{1,9})\.([0-9]{1,9})(?:[-.]?(alpha|beta|rc)[.-]?([0-9]{1,9})?)?$`)

// updateChannels 把发布标签的预发布通道映射成 versionPattern 的后缀字母。
var updateChannels = map[string]string{"alpha": "a", "beta": "b", "rc": "rc"}

// normalizeVersion 把版本号归一化成 versionPattern 能按顺序比较的形式：
// 0.1.0-alpha.5 变成 0.1.0a5。识别不了时返回 false，调用方当作没有版本可比较。
func normalizeVersion(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	value = strings.TrimPrefix(strings.TrimPrefix(value, "v"), "V")
	if versionPattern.MatchString(value) {
		return value, true
	}
	parts := releaseTag.FindStringSubmatch(value)
	if parts == nil {
		return "", false
	}
	suffix := ""
	if channel, ok := updateChannels[parts[4]]; ok {
		number := parts[5]
		if number == "" {
			number = "0"
		}
		suffix = channel + number
	}
	normalized := parts[1] + "." + parts[2] + "." + parts[3] + suffix
	if !versionPattern.MatchString(normalized) {
		return "", false
	}
	return normalized, true
}

// updateRequest 构造一次自更新请求，补齐 GitHub 要求的请求头。
func updateRequest(ctx context.Context, address string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", updateUserAgent)
	return req, nil
}

// updateRelease 是 GitHub「最新发布」里用得到的字段。
type updateRelease struct {
	TagName string `json:"tag_name"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// fetchUpdateRelease 查询 GitHub 上启动器的最新发布。错误文案直接面向用户。
func fetchUpdateRelease(ctx context.Context) (updateRelease, error) {
	var release updateRelease
	req, err := updateRequest(ctx, githubLatestReleaseURL)
	if err != nil {
		return release, errors.New("更新地址无效，请检查网络设置后重试")
	}
	resp, err := updateAPIHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return release, errors.New("检查更新超时，请稍后重试")
		}
		return release, errors.New("无法连接 GitHub，请检查网络后重试")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return release, errors.New("GitHub 接口限流，请稍后再试")
	case resp.StatusCode == http.StatusNotFound:
		// 仓库还没发布任何版本：这是正常状态，不是失败。按「没有更新」返回，
		// 否则界面上每次检查都会显示成「上次检查未完成」。
		return release, nil
	case resp.StatusCode != http.StatusOK:
		return release, fmt.Errorf("GitHub 返回 HTTP %d，请稍后再试", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, updateJSONLimit)).Decode(&release); err != nil {
		if ctx.Err() != nil {
			return release, errors.New("检查更新超时，请稍后重试")
		}
		return release, errors.New("无法解析 GitHub 发布信息，请稍后再试")
	}
	return release, nil
}

// fetchRecommendedVersion 拉取 recommended.json 并校验。任何失败都回落到编译期常量，
// 不向界面报错：推荐版本只是锦上添花，不该因为网络问题让设置页出现红字。
func fetchRecommendedVersion(ctx context.Context, current string) string {
	req, err := updateRequest(ctx, recommendedJSONURL)
	if err != nil {
		return recommendedVersion
	}
	resp, err := updateAPIHTTP.Do(req)
	if err != nil {
		return recommendedVersion
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return recommendedVersion
	}
	var file struct {
		Version     string `json:"version"`
		MinLauncher string `json:"min_launcher"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, updateJSONLimit)).Decode(&file); err != nil {
		return recommendedVersion
	}
	return recommendedFor(file.Version, file.MinLauncher, current)
}

// recommendedFor 校验推荐清单里的版本：
//   - version 必须是正式的 NeoBot 版本号（versionPattern），预发布版本不推荐；
//   - min_launcher 填了时必须不高于当前启动器版本，否则这台机器装不上，视为不推荐。
func recommendedFor(version, minLauncher, current string) string {
	version = strings.TrimSpace(version)
	if !versionPattern.MatchString(version) || versionOf(version).Preview {
		return recommendedVersion
	}
	if minLauncher = strings.TrimSpace(minLauncher); minLauncher != "" {
		want, ok := normalizeVersion(minLauncher)
		if !ok {
			return recommendedVersion
		}
		have, ok := normalizeVersion(current)
		if !ok || compareVersions(want, have) > 0 {
			return recommendedVersion
		}
	}
	return version
}

// checkLatestUpdate 组装一次完整的检查结果：最新发布与推荐版本。
// 发布接口失败时返回错误，但仍会带上推荐版本（两者互不影响）。
func checkLatestUpdate(ctx context.Context, current string) (UpdateStatus, error) {
	status := UpdateStatus{Current: current, AutoUpdate: updateAutoInstall, Recommended: fetchRecommendedVersion(ctx, current)}
	release, err := fetchUpdateRelease(ctx)
	if err != nil {
		return status, err
	}
	// 仓库还没发布任何版本时，/releases/latest 返回 404，fetchUpdateRelease 按「没有更新」
	// 返回一个空的 release：这里必须当成正常状态，而不是「版本号无法识别」报错。
	if strings.TrimSpace(release.TagName) == "" {
		return status, nil
	}
	latest, ok := normalizeVersion(release.TagName)
	if !ok {
		return status, fmt.Errorf("无法识别 GitHub 上的版本号「%s」，请到发布页查看", strings.TrimSpace(release.TagName))
	}
	// 界面显示发布标签的原始写法，比较用归一化后的形式。
	status.Latest = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(release.TagName), "v"), "V")
	status.PageURL = release.HTMLURL
	status.Notes = updateNotes(release.Body)
	if cur, valid := normalizeVersion(current); valid {
		status.Available = compareVersions(latest, cur) > 0
	}
	for _, asset := range release.Assets {
		name := strings.ToLower(strings.TrimSpace(asset.Name))
		switch {
		case updateAssetName != "" && name == updateAssetName:
			status.AssetURL = asset.URL // 正式资产名优先，出现多个安装包时不会被兜底规则抢走
		case updateChecksumName != "" && name == updateChecksumName:
			status.ChecksumsURL = asset.URL
		case updateAssetSuffix != "" && strings.HasSuffix(name, updateAssetSuffix) && status.AssetURL == "":
			status.AssetURL = asset.URL
		}
	}
	// 只接受 https 地址：发布信息来自 HTTPS 的 GitHub 接口，不该被降级下载。
	if !updateSecureURL(status.AssetURL) {
		status.AssetURL = ""
	}
	if !updateSecureURL(status.ChecksumsURL) {
		status.ChecksumsURL = ""
	}
	return status, nil
}

// updateSecureURL 判断下载地址是不是可接受的 HTTPS 地址。
func updateSecureURL(address string) bool {
	u, err := url.Parse(strings.TrimSpace(address))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return false
	}
	return true
}

// updateNotes 截断发布说明，避免快照里塞进整篇 Markdown。
func updateNotes(body string) string {
	notes := strings.TrimSpace(body)
	runes := []rune(notes)
	if len(runes) <= updateNotesLimit {
		return notes
	}
	return string(runes[:updateNotesLimit]) + "…"
}

// recommended 返回当前推荐的 NeoBot 版本；远端没有数据时回落到编译期常量。
// 调用方必须持有 m.mu（Snapshot 在持锁状态下调用它）。这里只读缓存，
// 不发网络请求也不再加锁，否则 Snapshot 会被网络或死锁拖慢。
func (m *Manager) recommended() string {
	if m.update.Recommended == "" || m.update.CheckedAt == 0 {
		return recommendedVersion
	}
	if time.Since(time.Unix(m.update.CheckedAt, 0)) >= recommendedTTL {
		return recommendedVersion
	}
	return m.update.Recommended
}

// CheckUpdate 异步检查启动器自身是否有新版本，并刷新推荐版本。
// 数据来源：GitHub Releases（SuperQuail/neobot-desktop）与 recommended.json。
// 失败只写 UpdateStatus.Error，不得阻塞界面。
func (m *Manager) CheckUpdate(force bool) error {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return errors.New("启动器正在关闭")
	}
	if m.updateBusy {
		// 已经在检查：重复点击不报错，等这一轮结果就行。
		m.mu.Unlock()
		return nil
	}
	if !force && m.update.CheckedAt != 0 && time.Since(time.Unix(m.update.CheckedAt, 0)) < updateCheckInterval {
		m.mu.Unlock()
		return nil
	}
	current := CurrentVersion()
	m.updateBusy, m.update.Checking = true, true
	m.update.Current = current
	m.mu.Unlock()
	m.notify()
	go m.runUpdateCheck(current)
	return nil
}

// runUpdateCheck 是 CheckUpdate 的后台部分：网络请求不持锁，结果一次性写回 m.update。
func (m *Manager) runUpdateCheck(current string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	status, err := checkLatestUpdate(ctx, current)

	m.mu.Lock()
	m.updateBusy, m.update.Checking = false, false
	m.update.Current = current
	// 无论成败都记录这次尝试的时间：失败后一小时内不再自动重试，免得反复打接口；
	// 推荐版本也用这个时间做 6 小时缓存，失败时正好回落到常量。
	m.update.CheckedAt = time.Now().Unix()
	m.update.Recommended = status.Recommended
	if err != nil {
		// 失败只覆盖错误信息：上一轮检查到的新版本仍然有效，不该被网络抖动抹掉。
		m.update.Error = err.Error()
	} else {
		kept := ""
		if m.update.AssetURL == status.AssetURL && isFile(m.update.Downloaded) {
			kept = m.update.Downloaded // 安装包没变就不用让用户重新下载
		}
		m.update.Latest = status.Latest
		m.update.Available = status.Available
		m.update.Notes = status.Notes
		m.update.PageURL = status.PageURL
		m.update.AssetURL = status.AssetURL
		m.update.ChecksumsURL = status.ChecksumsURL
		m.update.AutoUpdate = status.AutoUpdate
		m.update.Downloaded = kept
		m.update.Error = ""
	}
	m.mu.Unlock()
	m.notify()
}

// DownloadUpdate 下载安装包到 <root>/updates/ 并校验 SHA256，返回本地路径。
func (m *Manager) DownloadUpdate() (string, error) {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return "", errors.New("启动器正在关闭")
	}
	if m.updateBusy {
		m.mu.Unlock()
		return "", errors.New("更新任务正在进行，请稍候再试")
	}
	asset, checksums := m.update.AssetURL, m.update.ChecksumsURL
	if asset == "" {
		m.mu.Unlock()
		return "", errors.New("还没有可下载的安装包，请先检查更新")
	}
	if existing := m.update.Downloaded; existing != "" && isFile(existing) {
		m.mu.Unlock()
		return existing, nil
	}
	name, err := updateAssetFileName(asset)
	if err != nil {
		m.mu.Unlock()
		return "", err
	}
	m.updateBusy = true
	m.update.Error = ""
	m.mu.Unlock()
	m.notify()

	path, err := m.downloadUpdate(asset, checksums, name)
	m.mu.Lock()
	m.updateBusy = false
	if err != nil {
		m.update.Error = err.Error()
	} else {
		m.update.Downloaded, m.update.Error = path, ""
	}
	m.mu.Unlock()
	m.notify()
	return path, err
}

// downloadUpdate 把安装包写进 <root>/updates/：先写 .part 再改名，
// 中途失败不会留下一个看起来能用的半个安装包。
func (m *Manager) downloadUpdate(asset, checksums, name string) (string, error) {
	dir := filepath.Join(m.root, "updates")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", errors.New("无法创建更新目录，请检查磁盘权限")
	}
	// 上次中断可能留下 .part 临时文件，里面的内容不是可用安装包，直接清掉。
	if entries, err := os.ReadDir(dir); err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
	}
	req, err := updateRequest(context.Background(), asset)
	if err != nil {
		return "", errors.New("安装包地址无效，请重新检查更新")
	}
	resp, err := updateDownloadHTTP.Do(req)
	if err != nil {
		return "", errors.New("无法下载安装包，请检查网络后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载安装包失败（HTTP %d），请稍后再试", resp.StatusCode)
	}
	part := filepath.Join(dir, "."+name+".part-"+timeID())
	file, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", errors.New("无法保存安装包，请检查磁盘空间")
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(resp.Body, maxUpdateBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(part)
		return "", errors.New("下载安装包中断，请检查网络后重试")
	}
	if closeErr != nil {
		_ = os.Remove(part)
		return "", errors.New("无法保存安装包，请检查磁盘空间")
	}
	if written > maxUpdateBytes {
		_ = os.Remove(part)
		return "", errors.New("安装包超过 200 MB，请到 GitHub Releases 手动下载")
	}
	// 拿不到校验文件就不自动安装：静默执行一个未经校验的安装包，等于把更新的
	// 信任链交给下载路径本身。宁可让用户去发布页手动下载。
	if checksums == "" {
		_ = os.Remove(part)
		return "", errors.New("发布方没有提供安装包校验文件，已取消自动安装；请到 GitHub Releases 手动下载")
	}
	if err := verifyUpdateChecksum(checksums, name, hex.EncodeToString(hash.Sum(nil))); err != nil {
		_ = os.Remove(part)
		return "", err
	}
	target := filepath.Join(dir, name)
	if err := replaceFile(part, target); err != nil {
		_ = os.Remove(part)
		return "", errors.New("无法保存安装包，请检查是否有其他程序占用文件")
	}
	return target, nil
}

// verifyUpdateChecksum 用 checksums.txt 里「<sha256>  <文件名>」那一行核对安装包。
// 清单里有这个文件就必须对得上；清单拿不到或者没记录，宁可让用户手动下载。
func verifyUpdateChecksum(address, name, want string) error {
	req, err := updateRequest(context.Background(), address)
	if err != nil {
		return errors.New("校验文件地址无效，请到 GitHub Releases 手动下载")
	}
	resp, err := updateAPIHTTP.Do(req)
	if err != nil {
		return errors.New("无法获取安装包校验文件，请重试或到 GitHub Releases 手动下载")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("无法获取安装包校验文件，请重试或到 GitHub Releases 手动下载")
	}
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, updateChecksumLimit))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		if !strings.EqualFold(fields[0], want) {
			return errors.New("安装包校验失败，下载可能不完整；请重试或到 GitHub Releases 手动下载")
		}
		return nil
	}
	if err := scanner.Err(); err != nil {
		return errors.New("无法读取安装包校验文件，请重试")
	}
	return errors.New("校验文件里没有这个安装包，请到 GitHub Releases 手动下载")
}

// updateAssetFileName 从下载地址里取安装包文件名，同时挡掉带路径分隔符或不符合命名约定的地址。
func updateAssetFileName(address string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(address))
	if err != nil {
		return "", errors.New("安装包地址无效，请重新检查更新")
	}
	name := u.Path
	if index := strings.LastIndexAny(name, `/\`); index >= 0 {
		name = name[index+1:]
	}
	if updateAssetSuffix == "" || !strings.HasSuffix(strings.ToLower(name), updateAssetSuffix) || strings.ContainsAny(name, `/:*?"<>|`) {
		return "", errors.New("安装包地址无效，请重新检查更新")
	}
	return filepath.Base(name), nil
}

// updateLauncher 启动下载好的安装包；具体实现按平台放在 update_<platform>.go。
// 抽成变量便于测试替换。
var updateLauncher = launchUpdateInstaller

// LaunchUpdate 启动已下载的安装包。静默安装前必须先停止 NeoBot。
func (m *Manager) LaunchUpdate() error {
	m.mu.Lock()
	path := m.update.Downloaded
	m.mu.Unlock()
	if path == "" {
		return errors.New("还没有下载安装包，请先下载更新")
	}
	if !isFile(path) {
		m.mu.Lock()
		if m.update.Downloaded == path {
			m.update.Downloaded = ""
		}
		m.mu.Unlock()
		m.notify()
		return errors.New("安装包已不存在，请重新下载更新")
	}
	// 安装程序要替换程序文件，NeoBot 必须先退出，否则文件被占用会装不上。
	// Stop 在没运行时直接返回 nil，这里不必先判断。
	if err := m.Stop(true); err != nil {
		return errors.New("请先手动停止 NeoBot，然后再安装更新")
	}
	if err := updateLauncher(path); err != nil {
		return errors.New("无法启动安装包，请到更新目录手动运行")
	}
	// 这里不退出启动器：安装程序覆盖文件前需要本进程先结束，
	// 由界面确认安装包已经跑起来后再调用 Quit()。
	return nil
}
