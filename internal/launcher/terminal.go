package launcher

// 高级终端：从诊断页直接进入某个已安装版本的 venv。
//
// 这里只负责解析目标版本、拼出环境变量并交给平台实现。Windows 上用系统
// cmd.exe 打开一个新控制台；Linux 上打开系统终端模拟器。终端拥有与当前
// 账户相同的权限，不是沙箱，因此首次使用必须由界面确认风险提示。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TerminalInfo 是诊断页「高级」区域展示的当前终端目标。
// Paths 只包含启动器自己的目录，不包含 .env 或控制接口凭据。
type TerminalInfo struct {
	Version   string `json:"version"`
	Label     string `json:"label"`
	Venv      string `json:"venv"`
	Python    string `json:"python"`
	Data      string `json:"data"`
	UV        string `json:"uv"`
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

// launchTerminal 由平台实现替换，测试里也用它避免真的打开窗口。
var launchTerminal = openTerminalWindow

// terminalTargetLocked 解析「打开终端」的目标版本。调用方持有 m.mu。
// 优先级：显式请求 > 正在运行的版本 > 下次启动版本 > 唯一已安装版本。
func (m *Manager) terminalTargetLocked(requested string) (string, error) {
	if requested != "" {
		if _, ok := m.installed[requested]; !ok {
			return "", errors.New("这个版本没有安装")
		}
		return requested, nil
	}
	if m.bot != nil {
		if _, ok := m.installed[m.bot.version]; ok {
			return m.bot.version, nil
		}
	}
	if m.state.Selected != "" {
		if _, ok := m.installed[m.state.Selected]; ok {
			return m.state.Selected, nil
		}
	}
	if len(m.installed) == 1 {
		for name := range m.installed {
			return name, nil
		}
	}
	return "", errors.New("请先安装并选择要维护的版本")
}

// terminalInfoLocked 生成界面展示用的路径信息。调用方持有 m.mu。
// 这里不检查文件是否存在，避免在快照里做磁盘 IO；打开终端时会再校验。
func (m *Manager) terminalInfoLocked() TerminalInfo {
	if !terminalSupported() {
		return TerminalInfo{Reason: "当前系统暂不支持高级终端"}
	}
	target, err := m.terminalTargetLocked("")
	if err != nil {
		return TerminalInfo{Reason: err.Error()}
	}
	item := m.installed[target]
	venv := m.versionDir(target)
	return TerminalInfo{
		Version:   target,
		Label:     item.Label(),
		Venv:      venv,
		Python:    venvPython(venv),
		Data:      m.dataDir(target),
		UV:        uvExecutable(m.root),
		Available: true,
	}
}

// OpenTerminal 打开目标版本的 venv 终端。
//
// acknowledged 表示界面已经展示并确认过风险提示；第一次打开时由前端在弹窗
// 确认后传 true，后端把确认状态写进 settings.json。没有确认过又传 false 的
// 调用会被拒绝，避免绕过首次提示。
func (m *Manager) OpenTerminal(version string, acknowledged bool) error {
	if !terminalSupported() {
		return errors.New("当前系统暂不支持高级终端")
	}
	m.mu.Lock()
	if m.root == "" {
		m.mu.Unlock()
		return errors.New("启动器数据目录尚未就绪")
	}
	if m.closing {
		m.mu.Unlock()
		return errors.New("启动器正在关闭，请重新打开后再试")
	}
	if m.job.State == "running" {
		m.mu.Unlock()
		return errors.New("请等当前安装或修复任务结束后再打开终端")
	}
	if m.preparing {
		m.mu.Unlock()
		return errors.New("NeoBot 正在启动，请等启动完成后再打开终端")
	}
	target, err := m.terminalTargetLocked(version)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	needNotify := false
	if !m.state.TerminalAcknowledged {
		if !acknowledged {
			m.mu.Unlock()
			return errors.New("请先确认终端使用提示")
		}
		m.state.TerminalAcknowledged = true
		if err := m.persist(); err != nil {
			m.state.TerminalAcknowledged = false
			m.mu.Unlock()
			return errors.New("无法保存终端使用提示，请检查磁盘空间后重试")
		}
		needNotify = true
	}
	data := m.dataDir(target)
	venv := m.versionDir(target)
	sources := m.state.Sources
	m.mu.Unlock()
	if needNotify {
		m.notify()
	}

	if err := os.MkdirAll(data, 0700); err != nil {
		return errors.New("无法准备数据目录，请检查磁盘空间或权限后重试")
	}
	if !isFile(venvPython(venv)) {
		return errors.New("当前版本环境不完整，请先修复安装")
	}
	// uv 的可执行文件是按版本命名的；生成一个固定的 uv 转发命令
	// （Windows 为 uv.cmd，Linux 为 uv 脚本），用户才能在终端里直接敲 uv。
	if err := m.writeUVShim(); err != nil {
		m.logMessage("无法创建 uv 命令：" + err.Error())
	}
	if err := launchTerminal(data, terminalEnv(m.root, data, venv, sources)); err != nil {
		return fmt.Errorf("无法打开终端：%w", err)
	}
	m.logMessage("已打开终端：" + target)
	return nil
}

// writeUVShim 在 tools/ 下维护 uv 命令，把 uv 转发到当前固定版本的可执行文件。
// Windows 生成 uv.cmd，Linux 生成可执行的 uv 包装脚本；版本升级后内容会不匹配，
// 下次打开终端时自动改写。
func (m *Manager) writeUVShim() error {
	exe := uvExecutable(m.root)
	if !isFile(exe) {
		return errors.New("缺少 uv，请重新安装或修复该版本")
	}
	name, content, mode, err := uvShim(exe)
	if err != nil {
		return err
	}
	shim := filepath.Join(m.root, "tools", name)
	if existing, err := os.ReadFile(shim); err == nil && string(existing) == string(content) {
		_ = os.Chmod(shim, mode)
		return nil
	}
	if err := os.WriteFile(shim, content, mode); err != nil {
		return err
	}
	return os.Chmod(shim, mode)
}

// terminalEnv 构造终端进程的环境。它沿用启动器安装时使用的缓存、Python
// 目录和下载源，同时清理掉可能把 uv/python 带到别的环境里的继承变量。
//
// 不设置 UV_NO_CONFIG：用户在终端里手动执行 uv 时，应保留他们自己的 uv 配置；
// 启动器自己的安装命令仍然通过 installEnv() 强制 UV_NO_CONFIG=1。
func terminalEnv(root, data, venv string, sources Sources) []string {
	env := cleanEnv(true)
	pathParts := []string{
		venvBin(venv),
		venv,
		filepath.Join(root, "tools"),
	}
	if inherited := os.Getenv("PATH"); inherited != "" {
		pathParts = append(pathParts, inherited)
	}
	path := strings.Join(pathParts, string(os.PathListSeparator))

	result := make([]string, 0, len(env)+12)
	for _, kv := range env {
		name := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
		switch {
		case name == "PATH", name == "PROMPT", name == "PS1", name == "PIP_INDEX_URL",
			name == "PIP_EXTRA_INDEX_URL", name == "PIP_CONFIG_FILE", strings.HasPrefix(name, "NEOBOT_"):
			// 这些值要么由下面统一重建，要么会把终端带到别的安装/索引上。
			continue
		}
		result = append(result, kv)
	}
	result = append(result,
		"PATH="+path,
		"VIRTUAL_ENV="+venv,
		"PYTHONUTF8=1",
		"PYTHONIOENCODING=utf-8",
		"NEOBOT_DATA_DIR="+data,
		"NEOBOT_ENV_FILE="+filepath.Join(data, ".env"),
		"UV_CACHE_DIR="+filepath.Join(root, "cache"),
		"UV_PYTHON_INSTALL_DIR="+filepath.Join(root, "python"),
		"UV_PYTHON_PREFERENCE=only-managed",
	)
	// 平台相关的 shell 提示符与环境变量由 terminal_<platform>.go 提供。
	result = append(result, terminalShellEnv(filepath.Base(venv))...)
	if sources.Index != "" {
		result = append(result, "UV_DEFAULT_INDEX="+sources.Index, "PIP_INDEX_URL="+sources.Index)
	}
	if sources.Python != "" {
		result = append(result, "UV_PYTHON_INSTALL_MIRROR="+sources.Python)
	}
	return result
}
