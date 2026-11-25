//go:build !windows && !linux

package launcher

// 其他平台目前不支持安装版本；保留 POSIX 常量让共享代码可以编译。
const (
	uvArchiveFile   = "uv-x86_64-unknown-linux-gnu.tar.gz"
	uvArchiveEntry  = "uv-x86_64-unknown-linux-gnu/uv"
	uvArchiveSHA256 = "ec7a99cd05e0cd7f80243f135ce1361c76835cb0ee60055d14d20eba8eba1460"
	uvWheelEntry    = "uv-" + uvVersion + ".data/scripts/uv"
	uvWheelFile     = "uv-" + uvVersion + "-py3-none-manylinux_2_17_x86_64.manylinux2014_x86_64.whl"
)

var uvWheelSHA256 = "5badfd805fd88bf99b4b4f044f6e8f762f1892cab27477f4427bb473e93dd049"

func matchUVWheel(string) bool { return false }
