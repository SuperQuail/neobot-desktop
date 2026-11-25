//go:build !windows && !linux

package launcher

import "errors"

var (
	updateAssetName    = ""
	updateAssetSuffix  = ""
	updateChecksumName = ""
	updateAutoInstall  = false
)

func launchUpdateInstaller(string) error {
	return errors.New("当前系统请到发布页手动下载更新包")
}
