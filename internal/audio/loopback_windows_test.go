//go:build windows

package audio

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/moutend/go-wca/pkg/wca"
)

func TestLoopbackUsesConsoleRoleForDefaultEndpointChanges(t *testing.T) {
	loopback := NewLoopback()
	if loopback.role() != wca.EConsole {
		t.Fatalf("系统回放应跟随默认 Console 输出，实际角色 %d", loopback.role())
	}
	if !isRenderEndpointChange(wca.ERender, wca.EConsole, loopback.role()) {
		t.Fatal("默认系统播放端点变更应触发重连")
	}
	if isRenderEndpointChange(wca.ERender, wca.EMultimedia, loopback.role()) || isRenderEndpointChange(wca.ECapture, wca.EConsole, loopback.role()) {
		t.Fatal("其他角色或录音端点变更不应触发当前回放重连")
	}
}

func TestWASAPIPacketDiscontinuityFlagsResetAnalysis(t *testing.T) {
	if !wasapiPacketDiscontinuous(wca.AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY) ||
		!wasapiPacketDiscontinuous(wca.AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR) {
		t.Fatal("WASAPI 数据或时间戳断流标记应重置分析状态")
	}
	if wasapiPacketDiscontinuous(wca.AUDCLNT_BUFFERFLAGS_SILENT) || wasapiPacketDiscontinuous(0) {
		t.Fatal("普通静音包不应被误判为断流")
	}
}

func TestLoopbackSmoke(t *testing.T) {
	expectSignal := os.Getenv("CASESCREEN_AUDIO_EXPECT_SIGNAL") == "1"
	if os.Getenv("CASESCREEN_AUDIO_SMOKE") != "1" && !expectSignal {
		t.Skip("设置 CASESCREEN_AUDIO_SMOKE=1 后执行真机回环采集")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	samples := 0
	peak := 0.0
	loopback := NewLoopback()
	err := loopback.Capture(ctx, func(frame PCMFrame) error {
		samples += len(frame.Samples)
		for _, sample := range frame.Samples {
			peak = math.Max(peak, math.Abs(sample))
		}
		// 至少观察一段连续音频；只收满一个 4096 样本窗口会把歌曲间隙误判为静音。
		if samples >= 4096 && peak > 1e-3 {
			cancel()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if samples == 0 {
		t.Fatal("5 秒内没有收到默认播放设备的回环样本")
	}
	if expectSignal && peak <= 1e-3 {
		t.Fatalf("已要求验证真实播放信号，但峰值仅 %.9f；请确认测试期间默认输出正在播放声音", peak)
	}
	t.Logf("采集到 %d 个样本，峰值 %.9f，端点 %+v", samples, peak, loopback.SourceStatus())
}

func TestLoopbackEndpointRolesSmoke(t *testing.T) {
	if os.Getenv("CASESCREEN_AUDIO_ROLE_SMOKE") != "1" {
		t.Skip("设置 CASESCREEN_AUDIO_ROLE_SMOKE=1 后比较 Console/Multimedia 默认端点")
	}
	for _, test := range []struct {
		name string
		role wca.ERole
	}{{name: "console", role: wca.EConsole}, {name: "multimedia", role: wca.EMultimedia}} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			loopback := NewLoopback()
			loopback.endpointRole = test.role
			samples := 0
			peak := 0.0
			if err := loopback.Capture(ctx, func(frame PCMFrame) error {
				samples += len(frame.Samples)
				for _, sample := range frame.Samples {
					peak = math.Max(peak, math.Abs(sample))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: samples=%d peak=%.9f", test.name, samples, peak)
		})
	}
}
