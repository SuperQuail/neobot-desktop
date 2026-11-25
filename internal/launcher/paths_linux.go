//go:build linux

package launcher

const (
	// venvBinDir 是 POSIX 虚拟环境里存放可执行文件的目录。
	venvBinDir = "bin"
	// exeSuffix 在 POSIX 系统上为空。
	exeSuffix = ""
)
