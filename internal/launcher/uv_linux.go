//go:build linux

package launcher

// Linux x64 的 uv 发行包。官方源是 GitHub Release 的 .tar.gz，压缩包里可执行
// 文件位于顶层目录；加速源用 PyPI 的 manylinux wheel，可执行文件在
// uv-<版本>.data/scripts/ 下（POSIX 的 scripts，而不是 Windows 的 Scripts）。
// 两个来源的内容不同，哈希必须分别固定。
const (
	uvArchiveFile   = "uv-x86_64-unknown-linux-gnu.tar.gz"
	uvArchiveEntry  = "uv-x86_64-unknown-linux-gnu/uv"
	uvArchiveSHA256 = "ec7a99cd05e0cd7f80243f135ce1361c76835cb0ee60055d14d20eba8eba1460"
	uvWheelEntry    = "uv-" + uvVersion + ".data/scripts/uv"
	uvWheelFile     = "uv-" + uvVersion + "-py3-none-manylinux_2_17_x86_64.manylinux2014_x86_64.whl"
)

// uvWheelSHA256 is the digest of the PyPI wheel. It is a variable so tests can
// install from a fixture wheel; production code never writes it.
var uvWheelSHA256 = "5badfd805fd88bf99b4b4f044f6e8f762f1892cab27477f4427bb473e93dd049"

// matchUVWheel 只接受与当前固定的 manylinux wheel 完全同名的文件。PyPI 上
// 同时有 musllinux_1_1_x86_64 变体，按前缀匹配会得到两个候选，因此这里用精确
// 文件名；版本和哈希都已经钉死，不需要容忍标签变化。
func matchUVWheel(name string) bool {
	return name == uvWheelFile
}
