package launcher

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// freeDiskSpace 返回给定路径所在卷对当前用户可用的剩余字节数。
func freeDiskSpace(path string) (int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, available uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &available); err != nil {
		return 0, err
	}
	return int64(free), nil
}

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

func replaceFile(source, target string) error {
	s, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	t, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(s, t, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func lockInstance(path string) (func(), error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, errors.New("NeoBot Desktop 已经在运行")
		}
		return nil, &os.PathError{Op: "锁定", Path: path, Err: err}
	}
	return func() { _ = windows.CloseHandle(h) }, nil
}

// prepareLaunch 让 NeoBot 以挂起状态启动，等它被放进 Job Object 后再恢复。
// 这样从第一个指令开始，进程及其后续子进程都受启动器控制。
func prepareLaunch(cmd *exec.Cmd) { cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED }

func trackProcessTree(pid int) (func(), func(int) bool, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, nil, err
	}
	var once sync.Once
	closeJob := func() { once.Do(func() { _ = windows.CloseHandle(job) }) }
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		closeJob()
		return nil, nil, err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		closeJob()
		return nil, nil, err
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		closeJob()
		return nil, nil, err
	}
	if err := resumeProcess(pid); err != nil {
		closeJob()
		return nil, nil, err
	}
	ownsPID := func(candidate int) bool {
		if candidate <= 0 {
			return false
		}
		// The virtualenv executable can delegate to a child interpreter.
		var list struct {
			assigned, count uint32
			pids            [128]uintptr
		}
		err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(&list)), uint32(unsafe.Sizeof(list)), nil)
		if err != nil {
			return false
		}
		for _, value := range list.pids[:min(int(list.count), len(list.pids))] {
			if value == uintptr(candidate) {
				return true
			}
		}
		return false
	}
	return closeJob, ownsPID, nil
}

// makeExecutable 在 Windows 上不需要：可执行性由 .exe 扩展名决定。
func makeExecutable(string) error { return nil }

// appDataBase 返回启动器数据根目录的上级目录。Windows 沿用 %LocalAppData%。
func appDataBase() (string, error) { return os.UserCacheDir() }

// openPath 用资源管理器打开目录。
func OpenPath(path string) error { return exec.Command("explorer", path).Start() }

// samePath 在 Windows 上忽略大小写和分隔符差异。
func samePath(a, b string) bool { return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) }

// terminalSupported 报告当前平台是否实现了高级终端。
func terminalSupported() bool { return true }

func resumeProcess(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{}
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(pid) {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return err
		}
		defer windows.CloseHandle(thread)
		_, err = windows.ResumeThread(thread)
		return err
	}
	return errors.New("找不到待启动的 NeoBot 线程")
}
