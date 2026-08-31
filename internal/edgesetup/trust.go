package edgesetup

import (
	"context"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
)

// FileTLSProbe 检查本地 CA bundle 与系统时间。它不通过关闭证书校验来
// “修复”环境；任一条件缺失时平台授权保持关闭。
type FileTLSProbe struct {
	CABundlePath string
	Clock        ClockController
}

func (probe FileTLSProbe) Readiness(ctx context.Context) TLSReadiness {
	result := TLSReadiness{}
	path := filepath.Clean(strings.TrimSpace(probe.CABundlePath))
	if path != "" && path != "." {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() &&
			info.Size() > 0 && info.Size() <= 16<<20 {
			if data, readErr := os.ReadFile(path); readErr == nil {
				pool := x509.NewCertPool()
				result.CABundle = pool.AppendCertsFromPEM(data)
			}
		}
	}
	if probe.Clock != nil {
		result.Clock = probe.Clock.Status(ctx).Ready
	}
	result.Ready = result.CABundle && result.Clock
	switch {
	case !result.CABundle && !result.Clock:
		result.Reason = "缺少 CA bundle，且系统时间不可信"
	case !result.CABundle:
		result.Reason = "缺少 CA bundle"
	case !result.Clock:
		result.Reason = "系统时间不可信"
	}
	return result
}
