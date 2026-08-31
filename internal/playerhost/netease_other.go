//go:build !windows && casescreen_legacy_netease_cdp

package playerhost

import (
	"errors"
	"io"
)

func newPlatformNeteaseDependencies() neteaseDependencies {
	unsupported := errors.New("网易云播放信息同步启动仅支持 Windows")
	return neteaseDependencies{
		locateExecutable: func() (string, error) { return "", unsupported },
		processRunning:   func() (bool, error) { return false, unsupported },
		launch: func(string) (io.ReadWriteCloser, error) {
			return nil, unsupported
		},
	}
}
