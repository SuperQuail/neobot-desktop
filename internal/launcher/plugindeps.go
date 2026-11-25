package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// pluginProbeTimeout 只做一次本地扫描，不需要等网络。
	pluginProbeTimeout = time.Minute
	// pluginDepsTimeout 覆盖 akshare 这类会带入 pandas 的大依赖，但不允许无限等待。
	pluginDepsTimeout = 10 * time.Minute
)

// pluginDependencyProbe 只依赖标准库和 packaging：宿主自己的依赖解析要导入
// neobot_modloader（冷启动约 2 秒），这里不值得为一次扫描付这个代价。
const pluginDependencyProbe = `import importlib.metadata as metadata
import json
import sys
import tomllib
from pathlib import Path

try:
    from packaging.requirements import Requirement
except Exception:
    # 没有 packaging 时退化为「全部交给 uv 判断」：已满足的需求 uv 会直接跳过。
    Requirement = None


def needs_install(requirement):
    if Requirement is None:
        return True
    parsed = Requirement(requirement)
    if parsed.marker is not None and not parsed.marker.evaluate():
        return False
    try:
        installed = metadata.version(parsed.name)
    except metadata.PackageNotFoundError:
        return True
    try:
        return not parsed.specifier.contains(installed)
    except Exception:
        return True


plugin_dir = Path(sys.argv[1])
found = []
if plugin_dir.is_dir():
    for entry in sorted(plugin_dir.iterdir()):
        manifest = entry / "plugin.toml"
        if entry.name.startswith("_") or not manifest.is_file():
            continue
        try:
            with manifest.open("rb") as handle:
                declared = tomllib.load(handle)
        except Exception:
            continue
        requirements = declared.get("python_dependencies")
        if requirements is None:
            requirements = declared.get("pypi_dependencies")
        if requirements is None:
            requirements = declared.get("requirements")
        if not isinstance(requirements, list):
            continue
        missing = []
        for item in requirements:
            if not isinstance(item, str):
                continue
            requirement = item.strip()
            # 与宿主一致：拒绝 pip 选项和含空白的条目，它们会被 uv 当成参数解析。
            if not requirement or requirement.startswith("-") or any(char.isspace() for char in requirement):
                continue
            try:
                if needs_install(requirement):
                    missing.append(requirement)
            except Exception:
                continue
        if missing:
            found.append({"name": str(declared.get("name") or entry.name), "missing": missing})
print(json.dumps({"packaging": Requirement is not None, "plugins": found}, ensure_ascii=False))
`

type pluginDependency struct {
	Name    string   `json:"name"`
	Missing []string `json:"missing"`
}

// pluginDependencyReport 是一次插件依赖探测的结果。Packaging 为 false 时探测
// 脚本无法判断版本范围，此时 Plugins 里的需求不一定是真的缺失。
type pluginDependencyReport struct {
	Packaging bool               `json:"packaging"`
	Plugins   []pluginDependency `json:"plugins"`
}

// syncPluginDependencies 在启动 NeoBot 之前补齐第三方插件的 PyPI 依赖。
//
// 宿主的依赖安装是交互式的：它向 stdin 打印 [y/N] 再读一行。桌面端启动的后端
// 没有终端，这条路径只会跳过或抛 EOFError，所以依赖必须由桌面端自己装。
// 任何一步失败都只记日志：装不上依赖的插件会被宿主跳过，不能因此让 NeoBot 起不来。
func (m *Manager) syncPluginDependencies(version, data string, sources Sources) {
	defer func() {
		if recovered := recover(); recovered != nil {
			m.logMessage("检查插件依赖时出错，已跳过")
		}
	}()
	python := venvPython(m.versionDir(version))
	pluginDir := filepath.Join(data, "plugins")
	if !isFile(python) || !hasPluginManifests(pluginDir) {
		return
	}
	report, err := probePluginDependencies(python, pluginDir)
	if err != nil {
		m.logMessage("无法检查插件依赖：" + err.Error())
		return
	}
	requirements, names := pluginRequirements(report.Plugins, disabledPlugins(data))
	if len(requirements) == 0 {
		return
	}
	uv := uvExecutable(m.root)
	if !isFile(uv) {
		m.logMessage("缺少 uv，无法自动安装插件依赖；" + strings.Join(names, "、") + " 可能无法加载")
		return
	}
	index := sources.Index
	if index == "" {
		index = officialIndex
	}
	m.logMessage(fmt.Sprintf("正在安装插件依赖，可能需要几分钟：%s", strings.Join(names, "、")))
	ctx, cancel := context.WithTimeout(context.Background(), pluginDepsTimeout)
	defer cancel()
	// 插件声明的版本范围可能把 NeoBot 自己的依赖顶掉。只在安装前环境自洽时才
	// 比较安装后的结果，否则会把宿主本来就有的问题算到插件头上。
	consistent := runUV(ctx, uv, m.installEnv(), "pip", "check", "--python", python) == nil
	args := append([]string{"pip", "install", "--python", python, "--default-index", index}, requirements...)
	if err := runUV(ctx, uv, m.installEnv(), args...); err != nil {
		m.logMessage("插件依赖安装失败，插件可能无法加载：" + err.Error())
		return
	}
	if consistent {
		if err := runUV(ctx, uv, m.installEnv(), "pip", "check", "--python", python); err != nil {
			m.logMessage("插件依赖与 NeoBot 环境存在版本冲突，请检查插件声明的依赖范围")
			return
		}
	}
	m.logMessage("插件依赖已就绪")
}

// hasPluginManifests 是探测前的快速判断：没有插件时不必启动解释器。
func hasPluginManifests(pluginDir string) bool {
	entries, err := os.ReadDir(pluginDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() && isFile(filepath.Join(pluginDir, entry.Name(), "plugin.toml")) {
			return true
		}
	}
	return false
}

// probePluginDependencies 让目标版本自己的解释器扫描插件目录，返回缺失依赖。
func probePluginDependencies(python, pluginDir string) (pluginDependencyReport, error) {
	return probePluginDependenciesWithin(python, pluginDir, pluginProbeTimeout)
}

// probePluginDependenciesWithin 是 probePluginDependencies 的带超时版本：体检可以
// 用更短的等待上限，避免把整个体检拖住太久。
func probePluginDependenciesWithin(python, pluginDir string, timeout time.Duration) (pluginDependencyReport, error) {
	if timeout <= 0 {
		timeout = pluginProbeTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", pluginDependencyProbe, pluginDir)
	cmd.Env = cleanEnv(false, "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
	// 与后端一致：子进程拿到的必须是管道而不是 NUL 设备。
	cmd.Stdin = strings.NewReader("")
	hideWindow(cmd)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return pluginDependencyReport{}, errors.New("检查插件依赖超时")
		}
		return pluginDependencyReport{}, errors.New("无法读取插件依赖清单（NeoBot 版本可能不兼容）")
	}
	return parsePluginDependencies(stdout.Bytes())
}

func parsePluginDependencies(raw []byte) (pluginDependencyReport, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return pluginDependencyReport{}, errors.New("插件依赖清单为空")
	}
	var report pluginDependencyReport
	if err := json.Unmarshal(trimmed, &report); err != nil {
		return pluginDependencyReport{}, errors.New("无法解析插件依赖清单")
	}
	return report, nil
}

// pluginRequirements 汇总待安装的需求串：跳过用户停用的插件，按插件名排序
// 保证日志和命令稳定，同一需求只出现一次。
func pluginRequirements(found []pluginDependency, disabled map[string]bool) ([]string, []string) {
	sorted := append([]pluginDependency(nil), found...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	seen := make(map[string]bool, len(sorted))
	requirements := make([]string, 0, len(sorted))
	names := make([]string, 0, len(sorted))
	for _, item := range sorted {
		if disabled[item.Name] || len(item.Missing) == 0 {
			continue
		}
		names = append(names, item.Name)
		for _, requirement := range item.Missing {
			if seen[requirement] {
				continue
			}
			seen[requirement] = true
			requirements = append(requirements, requirement)
		}
	}
	return requirements, names
}

// disabledPlugins 读取宿主的插件启停状态：用户停用的插件不再为它装依赖。
func disabledPlugins(data string) map[string]bool {
	var state struct {
		Plugins map[string]struct {
			Enabled *bool `json:"enabled"`
		} `json:"plugins"`
	}
	if err := readJSON(filepath.Join(data, "plugin_state.json"), &state); err != nil {
		return nil
	}
	disabled := make(map[string]bool, len(state.Plugins))
	for name, entry := range state.Plugins {
		if entry.Enabled != nil && !*entry.Enabled {
			disabled[name] = true
		}
	}
	return disabled
}
