//go:build linux

package launcher

import "errors"

// Linux 第一版只做手动更新：发布页提供 tar.gz，应用内不下载安装包。
// 空资产名让发布信息里的 Windows 安装包不会被选中。
var (
	updateAssetName    = ""
	updateAssetSuffix  = ""
	updateChecksumName = ""
	updateAutoInstall  = false
)

func launchUpdateInstaller(string) error {
	return errors.New("当前系统请到发布页手动下载更新包")
}
