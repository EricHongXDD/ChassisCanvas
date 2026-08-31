//go:build windows

package lyrics

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestNeteaseRawMemoryEvidenceLive 只用于真机定位官方客户端的只读字段。
// 它不依赖媒体会话，不改变播放器状态，也不读取或发送音频。
func TestNeteaseRawMemoryEvidenceLive(t *testing.T) {
	if os.Getenv("CASESCREEN_NETEASE_RAW_MEMORY_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_NETEASE_RAW_MEMORY_LIVE=1 后读取网易云原生播放字段")
	}
	pids, err := neteaseMusicProcessIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(pids) == 0 {
		t.Fatal("没有运行中的 cloudmusic.exe")
	}
	valid := 0
	for _, pid := range pids {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		probe, probeErr := openNeteaseProcessProbe(ctx, pid)
		if probeErr != nil {
			cancel()
			t.Logf("pid=%d 定位失败: %v", pid, probeErr)
			continue
		}
		evidence, readErr := probe.readEvidence(ctx)
		probe.close()
		cancel()
		if readErr != nil {
			t.Logf("pid=%d 没有有效播放证据: %v", pid, readErr)
			continue
		}
		valid++
		t.Logf("pid=%d id=%s status=%d playing=%v position=%s duration=%s sampled_at=%s",
			pid, evidence.TrackID, evidence.Status, evidence.playing(), evidence.Position,
			evidence.Duration, evidence.SampledAt.Format(time.RFC3339Nano))
	}
	if valid != 1 {
		t.Fatalf("有效网易云播放进程数量=%d，期望唯一一个", valid)
	}
}
