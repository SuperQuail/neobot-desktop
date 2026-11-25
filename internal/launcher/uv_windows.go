//go:build windows

package launcher

import "strings"

// Both sources ship the same uv.exe; each is pinned to the digest of the
// container it downloads.
const (
	uvArchiveFile   = "uv-x86_64-pc-windows-msvc.zip"
	uvArchiveEntry  = "uv.exe"
	uvArchiveSHA256 = "ddbfcee1ac615a0499f6aa97b5ec8ebdf3ee4a7714a48055ec2ba0030e3cf810"
	uvWheelEntry    = "uv-" + uvVersion + ".data/scripts/uv.exe"
	uvWheelFile     = "uv-" + uvVersion + "-py3-none-win_amd64.whl"
)

// uvWheelSHA256 is the digest of the PyPI wheel. It is a variable so tests can
// install from a fixture wheel; production code never writes it.
var uvWheelSHA256 = "871e884c3329f69c12bb7426edb9e727745c3bb2db6d9e0538ce0e1bdfb64916"

// matchUVWheel 判断一个索引文件名是不是当前平台的 uv 发行包。Windows 只认
// win_amd64 wheel；win32、musllinux 等其它平台包都要跳过。
func matchUVWheel(name string) bool {
	return strings.HasPrefix(name, "uv-"+uvVersion+"-") && strings.HasSuffix(name, "-win_amd64.whl")
}
