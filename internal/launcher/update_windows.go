//go:build windows

package launcher

import (
	"os/exec"
	"path/filepath"
)

// Windows 提供固定的 NSIS 安装包与校验文件，支持应用内下载和静默安装。
var (
	updateAssetName    = "neobot-desktop-amd64-installer.exe"
	updateAssetSuffix  = "-installer.exe"
	updateChecksumName = "checksums.txt"
	updateAutoInstall  = true
)

// launchUpdateInstaller 启动下载好的 NSIS 安装包（/S 静默）。
//
// 不直接 exec：安装器要覆盖本进程正在运行的 neobot-desktop.exe，立刻启动多半会
// 因为文件被占用而装不上。这里让 cmd 等 3 秒再启动安装程序，前端在 LaunchUpdate
// 返回后立刻 Quit，等安装器真正开始复制时启动器已经退出。
func launchUpdateInstaller(path string) error {
	cmd := exec.Command("cmd", "/c", "timeout /t 3 /nobreak >nul & start \"\" \""+path+"\" /S")
	cmd.Dir = filepath.Dir(path)
	hideWindow(cmd)
	return cmd.Start()
}
