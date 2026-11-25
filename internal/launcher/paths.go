package launcher

// 虚拟环境与工具的可执行文件布局按平台不同：Windows 用 Scripts\ 和 .exe，
// Linux 用 bin/ 且没有扩展名。安装、启动、修复、体检等所有路径都必须走这里，
// 不能再把平台目录名散落在业务代码里。

import "path/filepath"

// venvPython 返回某个版本虚拟环境里的 Python 解释器。
func venvPython(dir string) string {
	return filepath.Join(dir, venvBinDir, "python"+exeSuffix)
}

// venvBin 返回某个版本虚拟环境里存放可执行文件的目录。
func venvBin(dir string) string {
	return filepath.Join(dir, venvBinDir)
}

// botExecutable 返回某个版本安装的 NeoBot 启动程序。
func botExecutable(dir string) string {
	return filepath.Join(dir, venvBinDir, "neobot"+exeSuffix)
}

// uvExecutable 返回启动器自己管理的 uv 可执行文件。
func uvExecutable(root string) string {
	return filepath.Join(root, "tools", "uv-"+uvVersion+exeSuffix)
}
