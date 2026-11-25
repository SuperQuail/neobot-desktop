package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// versionsTestManager 造一个测试用 Manager：不发通知、不锁实例目录。
func versionsTestManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{root: t.TempDir(), installed: map[string]Version{}, state: diskState{Backups: map[string]string{}}}
}

// versionsWriteInstall 写一份最小可用的安装目录，scanInstalled/validInstallDir 都认。
func versionsWriteInstall(t *testing.T, dir, version string) {
	t.Helper()
	if err := os.MkdirAll(venvBin(dir), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{venvPython(dir), botExecutable(dir)} {
		if err := os.WriteFile(path, []byte("stub"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeInstallRecord(dir, installRecord{Name: version, Preview: versionOf(version).Preview}); err != nil {
		t.Fatal(err)
	}
}

// versionsInstall 在 root 下装好一个版本，并登记进 installed。
func versionsInstall(t *testing.T, m *Manager, version string) string {
	t.Helper()
	dir := m.versionDir(version)
	versionsWriteInstall(t, dir, version)
	m.installed[version] = versionOf(version)
	return dir
}

// versionsWaitJob 等 Repair 的后台任务结束。
func versionsWaitJob(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.Lock()
	done := m.jobDone
	m.mu.Unlock()
	if done == nil {
		t.Fatal("修复没有登记 jobDone")
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("修复任务没有在预期时间内结束")
	}
}

// versionsJob 读一份任务快照。
func versionsJob(t *testing.T, m *Manager) Job {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.job
}

func TestRemoveVersionDropsInstallAndLeftoverUpdate(t *testing.T) {
	m := versionsTestManager(t)
	m.state.Selected = "1.0.0"
	m.state.Backups["0.9.0"] = "1700000000"
	dir := versionsInstall(t, m, "1.0.0")
	// 一次中断的更新会把旧安装改名成 "<name>.old-<纳秒>"；卸载必须一起清掉，
	// 否则下次开机 recoverVersionDirs 会把它还原成刚删掉的版本。
	versionsWriteInstall(t, dir+".old-"+timeID(), "1.0.0")

	if err := m.RemoveVersion("1.0.0", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.installed["1.0.0"]; ok {
		t.Fatal("版本记录没有被移除")
	}
	if m.state.Selected != "" {
		t.Fatalf("选中项 = %q；卸载后应清空", m.state.Selected)
	}
	if got := m.state.Backups["0.9.0"]; got != "1700000000" {
		t.Fatalf("快照 = %q；卸载不应删除历史快照", got)
	}
	entries, err := os.ReadDir(filepath.Join(m.root, "versions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Fatalf("versions/ 下还剩 %q；卸载后不应留下任何目录", entry.Name())
	}
	var saved diskState
	if err := readJSON(filepath.Join(m.root, "settings.json"), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Selected != "" {
		t.Fatalf("落盘的选中项 = %q；应随卸载清空", saved.Selected)
	}
	if err := m.recoverVersionDirs(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("已卸载的版本被恢复出来了：%v", err)
	}
}

func TestRemoveVersionKeepsDataUnlessDropped(t *testing.T) {
	m := versionsTestManager(t)
	shared := filepath.Join(m.root, "data")
	isolated := filepath.Join(m.root, "isolated", "1.0.0")
	for _, dir := range []string{shared, isolated} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	versionsInstall(t, m, "1.0.0")
	if err := m.RemoveVersion("1.0.0", false); err != nil {
		t.Fatal(err)
	}
	if !isFile(filepath.Join(isolated, "config.toml")) {
		t.Fatal("默认卸载不应删除独立数据目录")
	}

	versionsInstall(t, m, "1.0.0")
	if err := m.RemoveVersion("1.0.0", true); err != nil {
		t.Fatal(err)
	}
	if isDir(isolated) {
		t.Fatal("要求删除数据时独立数据目录应被清除")
	}
	if !isFile(filepath.Join(shared, "config.toml")) {
		t.Fatal("共享 data/ 目录不属于卸载范围")
	}
}

func TestRemoveVersionRejections(t *testing.T) {
	m := versionsTestManager(t)
	dir := versionsInstall(t, m, "1.0.0")
	expect := func(want string, dropData bool) {
		t.Helper()
		err := m.RemoveVersion("1.0.0", dropData)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("错误 = %v；应说明 %q", err, want)
		}
	}

	if err := m.RemoveVersion("2.0.0", false); err == nil || !strings.Contains(err.Error(), "没有安装") {
		t.Fatalf("未安装的版本没有被拒绝：%v", err)
	}

	m.bot = &botProcess{version: "1.0.0"}
	expect("正在运行", false)
	m.bot = nil

	m.job.State = "running"
	expect("请等当前任务结束", false)
	m.job.State = ""

	m.preparing = true
	expect("正在启动或停止", false)
	m.preparing = false

	m.stopping = true
	expect("正在启动或停止", false)
	m.stopping = false

	m.closing = true
	expect("正在关闭", false)
	m.closing = false

	if !isDir(dir) {
		t.Fatal("被拒绝的卸载不该改动磁盘")
	}
	if _, ok := m.installed["1.0.0"]; !ok {
		t.Fatal("被拒绝的卸载不该删除版本记录")
	}
}

func TestRemoveVersionOnlyBlocksTheRunningVersion(t *testing.T) {
	m := versionsTestManager(t)
	removed := versionsInstall(t, m, "1.0.0")
	kept := versionsInstall(t, m, "2.0.0")
	m.bot = &botProcess{version: "2.0.0"}

	if err := m.RemoveVersion("1.0.0", false); err != nil {
		t.Fatalf("另一个版本在运行时仍应允许卸载：%v", err)
	}
	if isDir(removed) {
		t.Fatal("目标版本目录应被删除")
	}
	if !isDir(kept) {
		t.Fatal("正在运行的版本被误删")
	}
}

func TestRemoveVersionClearsRecordWhenDirectoryIsGone(t *testing.T) {
	m := versionsTestManager(t)
	m.installed["1.0.0"] = versionOf("1.0.0")
	m.state.Selected = "1.0.0"

	if err := m.RemoveVersion("1.0.0", false); err != nil {
		t.Fatalf("目录已不存在时应只清记录：%v", err)
	}
	if _, ok := m.installed["1.0.0"]; ok {
		t.Fatal("版本记录没有被移除")
	}
	if m.state.Selected != "" {
		t.Fatalf("选中项 = %q；应清空", m.state.Selected)
	}
}

func TestRemoveVersionReportsSaveFailure(t *testing.T) {
	m := versionsTestManager(t)
	versionsInstall(t, m, "1.0.0")
	m.state.Selected = "1.0.0"
	// 把 settings.json 占成一个目录：写设置一定失败，但卸载本身应照常完成。
	if err := os.Mkdir(filepath.Join(m.root, "settings.json"), 0700); err != nil {
		t.Fatal(err)
	}
	err := m.RemoveVersion("1.0.0", false)
	if err == nil || !strings.Contains(err.Error(), "版本已卸载") || !strings.Contains(err.Error(), "无法保存启动设置") {
		t.Fatalf("错误 = %v；应说明版本已删除但设置没保存", err)
	}
	if _, ok := m.installed["1.0.0"]; ok {
		t.Fatal("版本记录应已移除")
	}
	if m.state.Selected != "" {
		t.Fatalf("选中项 = %q；内存里也应清空", m.state.Selected)
	}
}

func TestRemoveVersionRemovesBranchInstall(t *testing.T) {
	m := versionsTestManager(t)
	id := repositoryBranchID(githubRepo, "dev")
	m.installed[id] = Version{Name: id, Branch: "dev", Repository: githubRepo}
	dir := m.versionDir(id)
	versionsWriteInstall(t, dir, id)

	if err := m.RemoveVersion(id, false); err != nil {
		t.Fatal(err)
	}
	if isDir(dir) {
		t.Fatal("分支版本的目录没有被删除")
	}
	if _, ok := m.installed[id]; ok {
		t.Fatal("分支版本的记录没有被移除")
	}
}

func TestVersionLeftoverDirsOnlyMatchesInterruptedNames(t *testing.T) {
	m := versionsTestManager(t)
	versions := filepath.Join(m.root, "versions")

	// 三种真正的残留：更新中断、卸载中断、恢复失败。
	wanted := []string{
		filepath.Join(versions, "1.0.0.old-"+timeID()),
		filepath.Join(versions, "1.0.0.trash-"+timeID()),
		filepath.Join(versions, "1.0.0.broken-"+timeID()),
	}
	ignored := []string{
		filepath.Join(versions, "1.0.0.old-12345678"),         // 太短，可能是分支 id 的一部分
		filepath.Join(versions, "1.0.0.old-"+timeID()+"x"),    // 数字后缀不完整
		filepath.Join(versions, "1.0.1.old-"+timeID()),        // 别的版本
		filepath.Join(versions, "1.0.0"),                      // 版本本体
		filepath.Join(versions, "branch-1.0.0.old-1234-abcd"), // 名字里带 ".old-" 的分支 id
	}
	for _, dir := range append(append([]string{}, wanted...), ignored...) {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}

	got := versionLeftoverDirs(filepath.Join(versions, "1.0.0"))
	sort.Strings(got)
	sort.Strings(wanted)
	if strings.Join(got, "|") != strings.Join(wanted, "|") {
		t.Fatalf("残留列表 = %v；应只包含 %v", got, wanted)
	}
}

func TestRepairRejections(t *testing.T) {
	m := versionsTestManager(t)
	versionsInstall(t, m, "1.0.0")
	branchID := repositoryBranchID(githubRepo, "dev")
	m.installed[branchID] = Version{Name: branchID, Branch: "dev", Repository: githubRepo}
	versionsWriteInstall(t, m.versionDir(branchID), branchID)

	if err := m.Repair("2.0.0"); err == nil || !strings.Contains(err.Error(), "没有安装") {
		t.Fatalf("未安装的版本没有被拒绝：%v", err)
	}
	if err := m.Repair(branchID); err == nil || !strings.Contains(err.Error(), "分支版本请用") {
		t.Fatalf("分支版本没有被拒绝：%v", err)
	}

	m.bot = &botProcess{version: branchID}
	if err := m.Repair("1.0.0"); err == nil || !strings.Contains(err.Error(), "请先停止") {
		t.Fatalf("运行中没有被拒绝：%v", err)
	}
	m.bot = nil

	m.job.State = "running"
	if err := m.Repair("1.0.0"); err == nil || !strings.Contains(err.Error(), "请等当前任务结束") {
		t.Fatalf("任务进行中没有被拒绝：%v", err)
	}
	m.job.State = ""

	m.preparing = true
	if err := m.Repair("1.0.0"); err == nil || !strings.Contains(err.Error(), "正在启动或停止") {
		t.Fatalf("启动中没有被拒绝：%v", err)
	}
	m.preparing = false

	m.stopping = true
	if err := m.Repair("1.0.0"); err == nil || !strings.Contains(err.Error(), "正在启动或停止") {
		t.Fatalf("停止中没有被拒绝：%v", err)
	}
	m.stopping = false

	m.closing = true
	if err := m.Repair("1.0.0"); err == nil || !strings.Contains(err.Error(), "正在关闭") {
		t.Fatalf("关闭中没有被拒绝：%v", err)
	}
	m.closing = false

	if job := versionsJob(t, m); job.State == "running" {
		t.Fatal("被拒绝的修复不应登记任务")
	}
}

func TestRepairFailsWithoutUV(t *testing.T) {
	m := versionsTestManager(t)
	dir := versionsInstall(t, m, "1.0.0")

	if err := m.Repair("1.0.0"); err != nil {
		t.Fatal(err)
	}
	if job := versionsJob(t, m); job.State != "running" || job.Total != 3 || job.Step != "uv" {
		t.Fatalf("任务 = %+v；应处于 3 步任务的第一步", job)
	}
	versionsWaitJob(t, m)

	job := versionsJob(t, m)
	if job.State != "failed" || !strings.Contains(job.Message, "缺少 uv") {
		t.Fatalf("任务 = %+v；缺少 uv 时应失败并说明原因", job)
	}
	m.mu.Lock()
	cancel := m.jobCancel
	m.mu.Unlock()
	if cancel != nil {
		t.Fatal("任务结束后应清空 jobCancel")
	}
	if !isDir(dir) {
		t.Fatal("修复失败不该删除版本目录")
	}
}

func TestRepairReportsWhichStepFailed(t *testing.T) {
	m := versionsTestManager(t)
	m.state.Sources = Sources{Preset: "accelerated", UV: officialUV, Index: mirrorIndex}
	dir := versionsInstall(t, m, "1.0.0")
	data := filepath.Join(m.root, "data")
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "config.toml"), []byte("user data"), 0600); err != nil {
		t.Fatal(err)
	}
	// 一个不是可执行文件的 uv：exec 会立刻失败，正好用来看走到了哪一步。
	if err := os.MkdirAll(filepath.Join(m.root, "tools"), 0700); err != nil {
		t.Fatal(err)
	}
	uv := uvExecutable(m.root)
	if err := os.WriteFile(uv, []byte("not an executable"), 0600); err != nil {
		t.Fatal(err)
	}

	// python.exe 还在：跳过 venv 重建，直接重装。
	if err := m.Repair("1.0.0"); err != nil {
		t.Fatal(err)
	}
	versionsWaitJob(t, m)
	job := versionsJob(t, m)
	if job.State != "failed" || !strings.Contains(job.Message, "重新安装 NeoBot 失败") {
		t.Fatalf("任务 = %+v；应停在重装依赖这一步", job)
	}
	if job.Step != "neobot" {
		t.Fatalf("失败时的步骤 = %q；应为 neobot", job.Step)
	}

	// python.exe 丢了：先重建环境，错误应指向环境重建。
	if err := os.Remove(venvPython(filepath.Join(dir))); err != nil {
		t.Fatal(err)
	}
	if err := m.Repair("1.0.0"); err != nil {
		t.Fatal(err)
	}
	versionsWaitJob(t, m)
	job = versionsJob(t, m)
	if job.State != "failed" || !strings.Contains(job.Message, "修复 Python 环境失败") {
		t.Fatalf("任务 = %+v；应停在重建虚拟环境这一步", job)
	}
	if job.Step != "venv" {
		t.Fatalf("失败时的步骤 = %q；应为 venv", job.Step)
	}
	if content, err := os.ReadFile(filepath.Join(data, "config.toml")); err != nil || string(content) != "user data" {
		t.Fatalf("data/ 被修复改动了：%q，%v", content, err)
	}
}
