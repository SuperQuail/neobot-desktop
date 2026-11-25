package launcher

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

var snapshotID = regexp.MustCompile(`^[0-9]{1,20}$`)

func validDiskState(s diskState) bool {
	if s.DataVersion != "" && (!versionPattern.MatchString(s.DataVersion) || versionOf(s.DataVersion).Preview) {
		return false
	}
	for v, id := range s.Backups {
		if !versionPattern.MatchString(v) || !snapshotID.MatchString(id) {
			return false
		}
	}
	return true
}

type DataSwitch struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Restore bool   `json:"restore"`
}

func (m *Manager) SwitchInfo() DataSwitch {
	m.mu.Lock()
	defer m.mu.Unlock()
	return switchInfo(m.state.DataVersion, m.state.Selected, m.state.IsolateData)
}

func switchInfo(from, to string, isolate bool) DataSwitch {
	restore := !isolate && from != "" && to != "" && !isBranchID(to) &&
		!versionOf(to).Preview && compareVersions(from, to) > 0
	return DataSwitch{From: from, To: to, Restore: restore}
}

type dataJournal struct {
	To   string
	Swap string
}

func (m *Manager) prepareData(version string, confirmed bool) (string, error) {
	if err := m.recoverData(); err != nil {
		return "", err
	}
	if m.state.IsolateData {
		dir := filepath.Join(m.root, "isolated", version)
		return dir, os.MkdirAll(dir, 0700)
	}
	active := filepath.Join(m.root, "data")
	if err := os.MkdirAll(active, 0700); err != nil {
		return "", err
	}
	if isBranchID(version) || versionOf(version).Preview {
		return active, nil
	}
	info := switchInfo(m.state.DataVersion, version, false)
	if info.From == version {
		return active, nil
	}
	if info.Restore && !confirmed {
		return "", errors.New("切回旧版会恢复对应快照，请先确认")
	}
	if info.Restore && m.state.Backups[version] == "" {
		return "", errors.New("没有此版本的兼容快照，无法安全切回")
	}
	if err := os.MkdirAll(filepath.Join(m.root, "backups"), 0700); err != nil {
		return "", err
	}
	backup := ""
	if info.From != "" {
		backup = timeID()
		if err := copyData(active, filepath.Join(m.root, "backups", backup)); err != nil {
			return "", fmt.Errorf("备份失败，原数据未切换：%w", err)
		}
	}
	if info.Restore {
		swap := timeID()
		if err := copyData(filepath.Join(m.root, "backups", m.state.Backups[version]), m.swapDir(swap, "next")); err != nil {
			return "", fmt.Errorf("读取兼容快照失败：%w", err)
		}
		if err := writeJSON(m.journalPath(), dataJournal{To: version, Swap: swap}); err != nil {
			return "", err
		}
		if err := os.Rename(active, m.swapDir(swap, "old")); err != nil {
			return "", err
		}
		if err := os.Rename(m.swapDir(swap, "next"), active); err != nil {
			return "", errors.Join(err, m.recoverData())
		}
	}
	before := m.state
	m.state.DataVersion = version
	m.state.Backups = make(map[string]string, len(before.Backups)+1)
	for v, id := range before.Backups {
		m.state.Backups[v] = id
	}
	if backup != "" {
		m.state.Backups[info.From] = backup
	}
	if err := m.persist(); err != nil {
		m.state = before
		return "", errors.Join(err, m.recoverData())
	}
	return active, m.recoverData()
}

func (m *Manager) journalPath() string { return filepath.Join(m.root, "data-switch.json") }
func (m *Manager) swapDir(id, suffix string) string {
	return filepath.Join(m.root, "data-"+id+"-"+suffix)
}

func (m *Manager) recoverData() error {
	var j dataJournal
	err := readJSON(m.journalPath(), &j)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !versionPattern.MatchString(j.To) || !snapshotID.MatchString(j.Swap) {
		return errors.New("数据切换记录无效")
	}
	old, active := m.swapDir(j.Swap, "old"), filepath.Join(m.root, "data")
	if m.state.DataVersion != j.To {
		if _, err = os.Stat(old); err == nil {
			if _, err = os.Stat(active); err == nil {
				if err = os.Rename(active, m.swapDir(j.Swap, "rejected")); err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err = os.Rename(old, active); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if _, err = os.Stat(active); err != nil {
		return err
	}
	return os.Remove(m.journalPath())
}

// copyFileMode 返回写入副本时使用的权限：保留所有者的读写位和源文件的可执行位，
// 但不把数据目录里的私密文件复制成组/其他可读。
func copyFileMode(source os.FileMode) os.FileMode {
	mode := os.FileMode(0600)
	if source.Perm()&0111 != 0 {
		mode |= 0111
	}
	return mode
}

func copyData(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("数据目录不是普通文件夹")
	}
	if err = os.Mkdir(target, 0700); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, info fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil || rel == "." {
			return err
		}
		if rel == ".neobot" && info.IsDir() {
			return filepath.SkipDir
		}
		if info.Type()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Type().IsRegular()) {
			return fmt.Errorf("无法备份非普通文件：%s", rel)
		}
		dest := filepath.Join(target, rel)
		if info.IsDir() {
			return os.Mkdir(dest, 0700)
		}
		entryInfo, err := info.Info()
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, copyFileMode(entryInfo.Mode()))
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if err == nil {
			err = out.Sync()
		}
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		return err
	})
}
