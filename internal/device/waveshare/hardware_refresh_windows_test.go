//go:build windows

package waveshare_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"casescreen/internal/config"
	"casescreen/internal/device"
	waveshare "casescreen/internal/device/waveshare"
	"casescreen/internal/serialport"
	"casescreen/internal/theme"
)

// TestHardwareSpectrumRefreshRate 只在显式设置 CASESCREEN_WAVESHARE_REFRESH_LIVE=1
// 时占用真实 COM 口。运行前必须退出 CaseScreen；测试不会上传主题或媒体，
// 只让当前驻留 Theme 的频谱键运动十秒，最后发送一次全零复位。
func TestHardwareSpectrumRefreshRate(t *testing.T) {
	if os.Getenv("CASESCREEN_WAVESHARE_REFRESH_LIVE") != "1" {
		t.Skip("未启用微雪真机刷新率测试")
	}
	store, err := config.Open("")
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	for _, configured := range cfg.Devices {
		if configured.Kind != "waveshare" || !configured.Enabled {
			continue
		}
		t.Run(configured.ID, func(t *testing.T) {
			manifestPath := cfg.DeviceThemePaths[configured.ID]
			data, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatalf("读取当前主题: %v", err)
			}
			var manifest theme.Manifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatalf("解析当前主题: %v", err)
			}
			keys := hardwareSpectrumKeys(manifest)
			if len(keys) == 0 {
				t.Skip("当前主题没有频谱图层")
			}
			port, err := serialport.Open(configured.Port, serialport.DefaultOptions())
			if err != nil {
				t.Fatalf("打开 %s: %v", configured.Port, err)
			}
			driver := waveshare.New(configured.ID, configured.Name, [2]int{configured.Width, configured.Height}, port, nil)
			driver.ActivationDelay = 0
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := driver.Connect(ctx); err != nil {
				_ = port.Close()
				t.Fatalf("连接 %s: %v", configured.Port, err)
			}
			defer driver.Close()

			const frames = 90
			const cadence = 110 * time.Millisecond
			started := time.Now()
			next := started
			successes := 0
			dropped := 0
			ackTotal := time.Duration(0)
			for frame := 0; frame < frames; frame++ {
				next = next.Add(cadence)
				values := make(map[string]string, len(keys))
				for index, key := range keys {
					values[key] = fmt.Sprintf("%d", (frame*7+index*3)%100)
				}
				before := time.Now()
				err := driver.SendSpectrumValues(ctx, values)
				switch {
				case err == nil:
					successes++
					ackTotal += time.Since(before)
				case errors.Is(err, device.ErrRealtimeFrameDropped):
					dropped++
				default:
					t.Fatalf("第 %d 帧发送失败: %v", frame+1, err)
				}
				if delay := time.Until(next); delay > 0 {
					time.Sleep(delay)
				}
			}
			elapsed := time.Since(started)
			fps := float64(successes) / elapsed.Seconds()
			averageACK := time.Duration(0)
			if successes > 0 {
				averageACK = ackTotal / time.Duration(successes)
			}
			t.Logf("port=%s keys=%d success=%d dropped=%d elapsed=%v fps=%.2f avg_ack=%v",
				configured.Port, len(keys), successes, dropped, elapsed, fps, averageACK)
			if fps < 8 {
				t.Fatalf("真机有效刷新率低于 8 FPS: %.2f", fps)
			}

			time.Sleep(cadence)
			zero := make(map[string]string, len(keys))
			for _, key := range keys {
				zero[key] = "0"
			}
			if err := driver.SendSpectrumValues(context.Background(), zero); err != nil {
				t.Fatalf("测试后频谱清零失败: %v", err)
			}
		})
	}
}

func hardwareSpectrumKeys(manifest theme.Manifest) []string {
	set := make(map[string]bool)
	for _, layer := range manifest.Layers {
		if (layer.Type != theme.LayerAudioBars && layer.Type != theme.LayerAudioRing) ||
			!theme.LayerVisible(layer) || layer.Opacity <= 0 {
			continue
		}
		for _, index := range theme.WaveshareAudioBandIndexes(layer, manifest.Width, manifest.Height) {
			set[theme.WaveshareAudioBandKey(layer, index)] = true
			if theme.OptionBool(layer.Options, "peak_hold", false) {
				set[theme.WaveshareAudioPeakKey(layer, index)] = true
			}
		}
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	return keys
}
