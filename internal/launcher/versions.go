package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// versionRepairTimeout 限制一次修复的总时长。修复要重新下载并安装整棵依赖树，
// 慢网络下也要几分钟，但不能让任务永远挂着。
const versionRepairTimeout = 20 * time.Minute

// safeVersionDirName 给删除和修复加一道路径保险。版本名会直接拼进 versions/ 与
// isolated/ 下的路径，必须只是一个普通目录名。
func safeVersionDirName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 128 {
		return false
	}
	if strings.ContainsAny(name, "/:") || strings.ContainsRune(name, '\\') {
		return false
	}
	return filepath.Base(name) == name
}

// versionLeftoverDirs 找出 versions/ 下属于该版本、可以安全删除的残留目录：
// 更新中断留下的 "<name>.old-<纳秒>"、上次卸载中断留下的 "<name>.trash-<纳秒>"，
// 以及恢复失败时被改名成 "<name>.broken-<纳秒>" 的旧目录。
//
// 后缀必须像 timeID 一样是 16–20 位数字：分支安装 id 本身也可能带 ".old-" 字样，
// 太短的纯数字后缀不能当成残留。
func versionLeftoverDirs(dir string) []string {
	parent := filepath.Dir(dir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}
	prefix := filepath.Base(dir) + "."
	leftovers := make([]string, 0, 2)
	for _, entry := range entries {
		rest, ok := strings.CutPrefix(entry.Name(), prefix)
		if !ok {
			continue
		}
		for _, marker := range []string{"old-", "trash-", "broken-"} {
			suffix, matched := strings.CutPrefix(rest, marker)
			if matched && len(suffix) >= 16 && len(suffix) <= 20 && allDigits(suffix) {
				leftovers = append(leftovers, filepath.Join(parent, entry.Name()))
				break
			}
		}
	}
	return leftovers
}

// RemoveVersion 删除一个已安装版本。
//   - 正在运行、正在启动/停止、或安装任务进行中时拒绝。
//   - dropData 为真时同时删除该版本在 isolated/ 下的独立数据目录。
//   - 若该版本是下次启动的选中项，需要清空 Selected 并 persist。
//   - 版本目录要用 .trash-<纳秒> 先改名再删除：recoverVersionDirs 会把
//     "<name>.old-<19位纳秒>" 当成可恢复的旧安装，直接 RemoveAll 到一半会被误恢复。
//   - 备份快照（state.Backups）不要自动删，交给 CleanUsage("backups")。
func (m *Manager) RemoveVersion(version string, dropData bool) error {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return errors.New("启动器正在关闭，请重新打开后再试")
	}
	if m.job.State == "running" {
		m.mu.Unlock()
		return errors.New("请等当前任务结束后再卸载版本")
	}
	if m.preparing || m.stopping {
		m.mu.Unlock()
		return errors.New("NeoBot 正在启动或停止，请等操作结束后再卸载版本")
	}
	item, installed := m.installed[version]
	if !installed {
		m.mu.Unlock()
		return errors.New("这个版本没有安装")
	}
	if m.bot != nil && m.bot.version == version {
		m.mu.Unlock()
		return fmt.Errorf("版本 %s 正在运行，请先停止 NeoBot 再卸载", item.Label())
	}
	if !safeVersionDirName(version) {
		m.mu.Unlock()
		return errors.New("版本名称无效，无法定位它的安装目录")
	}
	dir := m.versionDir(version)
	// 残留目录要在改名之前记下来，否则刚建好的中转目录也会被列进来删两遍。
	leftovers := versionLeftoverDirs(dir)
	trash, moved := "", false
	if _, statErr := os.Lstat(dir); statErr == nil {
		// 先改成中转名再删：recoverVersionDirs 只认 ".old-"，".trash-" 不会被恢复。
		trash = dir + ".trash-" + timeID()
		if err := os.Rename(dir, trash); err != nil {
			m.mu.Unlock()
			return errors.New("无法删除版本目录：它可能正被其他程序占用，请关闭正在使用它的程序后重试")
		}
		moved = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		m.mu.Unlock()
		return errors.New("无法读取版本目录，请检查数据目录权限后重试")
	}
	// 目录可能已经被手动删掉了：那就只清版本记录，免得列表里留下删不掉的条目。
	delete(m.installed, version)
	problems := make([]string, 0, 2)
	if m.state.Selected == version {
		m.state.Selected = ""
		if err := m.persist(); err != nil {
			problems = append(problems, "无法保存启动设置，请检查磁盘空间")
		}
	}
	// 独立数据目录也先改名再删：下面是锁外清理，期间用户可能重新安装同一个版本，
	// 直接 RemoveAll 会把新版本刚建出来的数据目录一起删掉。
	isolatedTrash := ""
	if dropData {
		isolated := filepath.Join(m.root, "isolated", version)
		if isDir(isolated) {
			candidate := isolated + ".trash-" + timeID()
			if err := os.Rename(isolated, candidate); err != nil {
				problems = append(problems, "独立数据目录未能删除，请检查文件占用后重试")
			} else {
				isolatedTrash = candidate
			}
		}
	}
	m.mu.Unlock()
	m.notify()

	// 以下只是磁盘清理，不再持锁：删一个虚拟环境要好几百毫秒到几秒，界面还得能刷新。
	// 中转目录名唯一，版本也已从 installed 移除，不会被别的流程再碰到。
	if isolatedTrash != "" {
		if err := os.RemoveAll(isolatedTrash); err != nil {
			problems = append(problems, "独立数据目录未能完全删除，请检查文件占用后重试")
		}
	}
	if moved {
		if err := os.RemoveAll(trash); err != nil {
			problems = append(problems, "版本文件未能完全删除（已改名为 "+filepath.Base(trash)+"），请关闭占用它的程序后重试")
		}
	}
	// 残留的 "<name>.old-*" 必须一起删：否则下次开机会被 recoverVersionDirs
	// 当成可恢复的旧安装还原，已经卸载的版本又冒出来。
	for _, leftover := range leftovers {
		if err := os.RemoveAll(leftover); err != nil {
			problems = append(problems, "残留目录 "+filepath.Base(leftover)+" 未能删除，请关闭占用它的程序后重试")
		}
	}
	if len(problems) > 0 {
		// 版本目录已经没了，这时的报错只说「没做完的部分」，不要说成整个操作失败。
		return fmt.Errorf("版本已卸载，但%s", strings.Join(problems, "；"))
	}
	return nil
}

// Repair 在保留数据的前提下修复一个已安装版本：
//   - 运行中拒绝（必须先停止）。
//   - 重建缺失的虚拟环境（venv 丢了就重建）并用 --force-reinstall 重装包，
//     最后跑一次 uv pip check；不触碰 data/ 目录。
//   - 走 m.job 展示进度，结束时把 job.State 置为 done 或 failed。
//
// 和 Install 一样是异步的：这里只做校验和任务登记，真正的修复在后台跑，
// 进度通过快照里的 job 字段送到界面。
func (m *Manager) Repair(version string) error {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return errors.New("启动器正在关闭，请重新打开后再试")
	}
	if m.job.State == "running" {
		m.mu.Unlock()
		return errors.New("请等当前任务结束后再修复版本")
	}
	if m.preparing || m.stopping {
		m.mu.Unlock()
		return errors.New("NeoBot 正在启动或停止，请等操作结束后再修复版本")
	}
	item, installed := m.installed[version]
	if !installed {
		m.mu.Unlock()
		return errors.New("这个版本没有安装")
	}
	if item.Branch != "" {
		// 分支版本是 uv sync 出来的工作区，发行包里没有对应的源码，修不了。
		m.mu.Unlock()
		return errors.New("分支版本请用「更新」重新安装")
	}
	if m.bot != nil {
		m.mu.Unlock()
		return errors.New("NeoBot 正在运行，请先停止再修复版本")
	}
	if !safeVersionDirName(version) {
		m.mu.Unlock()
		return errors.New("版本名称无效，无法定位它的安装目录")
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionRepairTimeout)
	m.jobCancel = cancel
	done := make(chan struct{})
	m.jobDone = done
	sources := m.state.Sources
	m.job = Job{Version: version, Step: "uv", State: "running", Total: 3}
	m.mu.Unlock()
	m.notify()

	go func() {
		defer cancel()
		defer close(done)
		err := m.repair(ctx, version, sources)
		m.mu.Lock()
		m.jobCancel = nil
		if err != nil {
			m.job.State, m.job.Message = "failed", err.Error()
		} else {
			m.job.State, m.job.Completed, m.job.Message = "done", 3, "NeoBot 修复完成"
		}
		m.mu.Unlock()
		m.notify()
	}()
	return nil
}

// repair 是 Repair 的后台部分：只重建该版本的虚拟环境并重装依赖，
// 不碰 data/ 下的任何内容。
func (m *Manager) repair(ctx context.Context, version string, sources Sources) error {
	dir := m.versionDir(version)
	python := venvPython(dir)
	m.setStep("uv", "正在检查 uv", 0)
	uv := uvExecutable(m.root)
	if !isFile(uv) {
		return errors.New("缺少 uv，请重新安装该版本")
	}
	env := m.installEnv()
	m.setStep("venv", "正在检查 Python 环境", 1)
	if !isFile(python) {
		m.setStep("venv", "正在重建 Python 环境", 1)
		// --allow-existing 让半个残缺的目录也能就地补齐。
		if err := runUV(ctx, uv, env, "venv", "--allow-existing", "--python", "3.13", "--no-python-downloads", dir); err != nil {
			return fmt.Errorf("修复 Python 环境失败：%w", err)
		}
	}
	m.setStep("neobot", "正在重新安装 NeoBot "+version, 2)
	if err := runUV(ctx, uv, env, "pip", "install", "--python", python,
		"--default-index", sources.Index, "--force-reinstall", "neobot-app=="+version); err != nil {
		return fmt.Errorf("重新安装 NeoBot 失败：%w", err)
	}
	if err := runUV(ctx, uv, env, "pip", "check", "--python", python); err != nil {
		return fmt.Errorf("检查 NeoBot 依赖失败：%w", err)
	}
	if !isFile(m.binary(version)) {
		return errors.New("修复后没有找到 NeoBot 启动程序，请更换下载源后重新安装该版本")
	}
	// 安装记录可能在上次失败的安装里丢了；补一份，下次开机才认得出这个目录。
	_ = writeInstallRecord(dir, installRecord{Name: version, Preview: versionOf(version).Preview})
	return nil
}
