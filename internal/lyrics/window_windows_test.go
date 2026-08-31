//go:build windows

package lyrics

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestWindowsDateTimeConversion(t *testing.T) {
	const unixEpochTicks = int64(116444736000000000)
	converted, ok := windowsDateTime(unixEpochTicks + 12_345_678)
	if !ok || !converted.Equal(time.Unix(1, 234567800).UTC()) {
		t.Fatalf("WinRT DateTime 转换错误: time=%s ok=%v", converted, ok)
	}
	if converted, ok = windowsDateTime(0); ok || !converted.IsZero() {
		t.Fatalf("无效 WinRT DateTime 没有失败关闭: time=%s ok=%v", converted, ok)
	}
}

func TestWindowSourceSmoke(t *testing.T) {
	if os.Getenv("CASESCREEN_WINDOW_SOURCE_SMOKE") != "1" {
		t.Skip("设置 CASESCREEN_WINDOW_SOURCE_SMOKE=1 后读取当前播放器现场状态")
	}
	track, err := NewNowPlayingSource().Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("当前播放器窗口: source=%q title=%q artist=%q", track.Source, track.Title, track.Artist)
}
