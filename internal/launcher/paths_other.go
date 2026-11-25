//go:build !windows && !linux

package launcher

const (
	// 其他系统沿用 POSIX 布局；这些平台目前不支持安装版本。
	venvBinDir = "bin"
	exeSuffix  = ""
)
