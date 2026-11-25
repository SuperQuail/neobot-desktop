package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func readInstallRecord(dir string) (installRecord, error) {
	var record installRecord
	err := readJSON(filepath.Join(dir, ".installed"), &record)
	return record, err
}

func writeInstallRecord(dir string, record installRecord) error {
	return writeJSON(filepath.Join(dir, ".installed"), record)
}

// Branch records written before repository selection refer to the upstream project.
func (record installRecord) branchVersion(name string) (Version, bool) {
	if record.Name != name || !validBranchName(record.Branch) {
		return Version{}, false
	}
	repo, err := normalizeGitHubRepo(record.Repository)
	if err != nil || repositoryBranchID(repo, record.Branch) != name {
		return Version{}, false
	}
	return Version{Name: name, Branch: record.Branch, Repository: repo}, true
}

func (m *Manager) scanInstalled() error {
	entries, err := os.ReadDir(filepath.Join(m.root, "versions"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if versionTrashName.MatchString(name) {
			// 卸载版本时的中转目录：删到一半会被留在磁盘上，绝不能当成已安装版本。
			continue
		}
		dir := filepath.Join(m.root, "versions", name)
		record, recordErr := readInstallRecord(dir)
		if recordErr == nil && record.Name == name && isFile(m.binary(name)) {
			if record.Branch != "" {
				if version, ok := record.branchVersion(name); ok {
					m.installed[name] = version
				}
				continue
			}
			if versionPattern.MatchString(name) {
				m.installed[name] = Version{Name: name, Preview: record.Preview || versionOf(name).Preview}
			}
			continue
		}
		// Releases installed before the record was introduced, or whose record
		// was lost, are still usable when the launcher executable is present.
		if versionPattern.MatchString(name) && isFile(m.binary(name)) {
			m.installed[name] = versionOf(name)
			_ = writeInstallRecord(dir, installRecord{Name: name, Preview: versionOf(name).Preview})
		}
	}
	return nil
}

func validInstallDir(dir, name string) bool {
	record, err := readInstallRecord(dir)
	return err == nil && record.Name == name && isFile(botExecutable(dir))
}

// recoverVersionDirs restores an update that was interrupted after the old
// installation had been renamed aside. Failures are reported but must never
// prevent the launcher from opening.
func (m *Manager) recoverVersionDirs() error {
	versions := filepath.Join(m.root, "versions")
	entries, err := os.ReadDir(versions)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// Newest first, so a valid recent backup wins over an older one.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	var firstErr error
	remember := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		index := strings.LastIndex(name, ".old-")
		if index <= 0 {
			continue
		}
		suffix := name[index+5:]
		// timeID is a 19-digit nanosecond timestamp; shorter digit suffixes can
		// legitimately belong to a branch slug such as "x.old-12345678".
		if len(suffix) < 16 || len(suffix) > 20 || !allDigits(suffix) {
			continue
		}
		base := name[:index]
		target := filepath.Join(versions, base)
		old := filepath.Join(versions, name)
		if !isDir(target) {
			_, statErr := os.Lstat(target)
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				remember(statErr)
				continue
			}
			if !validInstallDir(old, base) {
				remember(errors.New("旧版本目录缺少安装记录，已保留待人工处理"))
				continue
			}
			if statErr == nil {
				broken := target + ".broken-" + timeID()
				if renameErr := os.Rename(target, broken); renameErr != nil {
					remember(renameErr)
					continue
				}
			}
			if renameErr := os.Rename(old, target); renameErr != nil {
				remember(renameErr)
			}
			continue
		}
		record, recordErr := readInstallRecord(target)
		if recordErr != nil && !errors.Is(recordErr, os.ErrNotExist) {
			// A temporarily unreadable record must not destroy a probably valid target.
			remember(recordErr)
			continue
		}
		if recordErr == nil && record.Name == base && isFile(m.binary(base)) {
			if removeErr := os.RemoveAll(old); removeErr != nil {
				remember(removeErr)
			}
			continue
		}
		if !validInstallDir(old, base) {
			remember(errors.New("旧版本目录缺少安装记录，已保留待人工处理"))
			continue
		}
		if removeErr := os.RemoveAll(target); removeErr != nil {
			remember(removeErr)
			continue
		}
		if renameErr := os.Rename(old, target); renameErr != nil {
			remember(renameErr)
		}
	}
	return firstErr
}

// migratePreviewData keeps data created before version isolation became an
// option. The move is best effort and never blocks startup.
func (m *Manager) migratePreviewData() {
	legacy := filepath.Join(m.root, "preview")
	entries, err := os.ReadDir(legacy)
	if err != nil {
		return
	}
	isolated := filepath.Join(m.root, "isolated")
	if err := os.MkdirAll(isolated, 0700); err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		target := filepath.Join(isolated, entry.Name())
		if _, err := os.Stat(target); err == nil {
			continue
		}
		_ = os.Rename(filepath.Join(legacy, entry.Name()), target)
	}
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
