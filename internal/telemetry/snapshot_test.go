package telemetry

import (
	"testing"
	"time"
)

func TestValuesCanOmitUnavailableHardwareMetrics(t *testing.T) {
	snapshot := Snapshot{
		CapturedAt:           time.Date(2026, 8, 26, 15, 4, 5, 0, time.Local),
		OmitHardwareMetrics:  true,
		LyricsCurrent:        "当前歌词",
		LyricsNext:           "下一句歌词",
		LyricsCurrentAtMS:    40000,
		LyricsNextAtMS:       45000,
		LyricsCurrentAtKnown: true,
		LyricsNextAtKnown:    true,
		LyricsPlaying:        true,
		LyricsMode:           "timed",
	}
	values := snapshot.Values()
	for _, key := range []string{
		"Cpu_Freq", "Memory_Load", "Memory_Used", "Memory_Available", "RAM",
		"Cpu_Model", "Gpu_Model", "Gpu_Load", "GPU_Temp", "GPU_Hotspot_Temp", "GPU_Memory_Temp", "vol", "cur_dB", "avg_dB", "max_dB",
	} {
		if _, exists := values[key]; exists {
			t.Errorf("采集不可用时不应发送 %s=%q", key, values[key])
		}
	}
	if values["time"] != "15:04:05" || values["CaseScreen_LyricsCurrent"] != "当前歌词" {
		t.Fatalf("真实时钟或歌词数据被误删: %#v", values)
	}
	for _, key := range []string{
		"source", "track_id", "position", "position_known", "playing", "lyric_mode",
		"CaseScreen_LyricsPositionMs",
	} {
		if _, exists := values[key]; exists {
			t.Errorf("主机字段或 Theme 未绑定字段不应下发到屏端: %s=%q", key, values[key])
		}
	}
	if values["CaseScreen_LyricsCurrentAtMs"] != "40000" ||
		values["CaseScreen_LyricsNextAtMs"] != "45000" ||
		values["CaseScreen_LyricsPlaying"] != "true" {
		t.Fatalf("歌词时间戳/播放状态小字段错误: %#v", values)
	}
}

func TestValuesPublishesEveryAvailableGPUTemperature(t *testing.T) {
	edge, hotspot, memory := 44.4, 54.2, 56.1
	values := (Snapshot{
		CapturedAt:            time.Now(),
		GPUTemperature:        &edge,
		GPUHotspotTemperature: &hotspot,
		GPUMemoryTemperature:  &memory,
	}).Values()
	if values["GPU_Temp"] != "44℃" || values["GPU_Hotspot_Temp"] != "54℃" || values["GPU_Memory_Temp"] != "56℃" {
		t.Fatalf("GPU 三路温度没有完整转换为小包字段: %#v", values)
	}
}

func TestValuesExplicitlyClearsUnavailableGPUMetrics(t *testing.T) {
	values := (Snapshot{CapturedAt: time.Now()}).Values()
	for _, key := range []string{
		"Gpu_Model", "Gpu_Load", "GPU_Temp", "GPU_Hotspot_Temp", "GPU_Memory_Temp",
	} {
		if value, exists := values[key]; !exists || value != "--" {
			t.Errorf("GPU 指标缺失时应显式清理 %s：got=%q exists=%v", key, value, exists)
		}
	}
}

func TestValuesNeverAdvertisesUncollectedAudioStatistics(t *testing.T) {
	values := (Snapshot{
		CapturedAt:      time.Now(),
		CurrentDecibels: 48,
		AverageDecibels: 41,
		MaximumDecibels: 73,
		Volume:          62,
	}).Values()
	for _, key := range []string{"vol", "cur_dB", "avg_dB", "max_dB"} {
		if _, exists := values[key]; exists {
			t.Errorf("未接入真实采集的数据不应下发: %s=%q", key, values[key])
		}
	}
}

func TestValuesSendsLyricEventWithVisibilityKey(t *testing.T) {
	values := (Snapshot{
		OmitHardwareMetrics:  true,
		OmitClock:            true,
		LyricsCurrent:        "最后一句",
		LyricsNext:           "播放歌曲后显示下一句",
		LyricsCurrentAtMS:    120000,
		LyricsNextAtMS:       0,
		LyricsCurrentAtKnown: true,
		LyricsPlaying:        true,
		LyricsMode:           "timed",
		LyricsVisible:        true,
		LyricsVisibleKnown:   true,
	}).Values()
	if values["CaseScreen_LyricsCurrent"] != "最后一句" || values["CaseScreen_LyricsNext"] != "播放歌曲后显示下一句" {
		t.Fatalf("歌词文字没有保留: %#v", values)
	}
	if len(values) != 6 || values["CaseScreen_LyricsCurrentAtMs"] != "120000" ||
		values["CaseScreen_LyricsNextAtMs"] != "" || values["CaseScreen_LyricsPlaying"] != "true" ||
		values[LyricsVisibleKey] != "1" {
		t.Fatalf("歌词状态事件协议错误: %#v", values)
	}
}

func TestValuesWaitingPlaceholderClearsUnknownTimestamps(t *testing.T) {
	values := (Snapshot{
		OmitHardwareMetrics: true,
		OmitClock:           true,
		LyricsCurrent:       "正在等待歌词",
		LyricsCurrentAtMS:   0,
		LyricsPlaying:       true,
		LyricsMode:          "timed",
	}).Values()
	if values["CaseScreen_LyricsCurrentAtMs"] != "" || values["CaseScreen_LyricsNextAtMs"] != "" ||
		values["CaseScreen_LyricsPlaying"] != "true" {
		t.Fatalf("等待占位包没有清空未知时间戳并保留播放状态: %#v", values)
	}
}

func TestValuesExplicitLyricClearResetsLayerAndProgress(t *testing.T) {
	values := (Snapshot{
		OmitHardwareMetrics:  true,
		OmitClock:            true,
		LyricsCurrent:        "不应保留的当前歌词",
		LyricsNext:           "不应保留的下一句",
		LyricsCurrentAtMS:    40000,
		LyricsNextAtMS:       45000,
		LyricsCurrentAtKnown: true,
		LyricsNextAtKnown:    true,
		LyricsPlaying:        true,
		LyricsMode:           "timed",
		LyricsClear:          true,
	}).Values()

	current, hasCurrent := values["CaseScreen_LyricsCurrent"]
	next, hasNext := values["CaseScreen_LyricsNext"]
	if !hasCurrent || current != "" || !hasNext || next != "" {
		t.Fatalf("显式清空必须保留两个空字符串键: %#v", values)
	}
	if len(values) != 8 || values["CaseScreen_LyricsCurrentAtMs"] != "" ||
		values["CaseScreen_LyricsNextAtMs"] != "" || values["CaseScreen_LyricsPlaying"] != "false" ||
		values[LyricsProgressLegacyKey] != "0" || values[LyricsProgressFineKey] != "0" || values[LyricsVisibleKey] != "0" {
		t.Fatalf("显式清空没有复位歌词与动态进度协议: %#v", values)
	}
}

func TestValuesLyricProgressPacketCanSelectFineOrLegacyScalar(t *testing.T) {
	tests := []struct {
		name     string
		snapshot Snapshot
		key      string
		want     string
	}{
		{
			name: "新版细进度",
			snapshot: Snapshot{OmitHardwareMetrics: true, OmitClock: true,
				LyricsProgressFine: 12000, LyricsProgressFineKnown: true},
			key: LyricsProgressFineKey, want: "10000",
		},
		{
			name: "旧主题百分比",
			snapshot: Snapshot{OmitHardwareMetrics: true, OmitClock: true,
				LyricsProgress: 137, LyricsProgressKnown: true},
			key: LyricsProgressLegacyKey, want: "100",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := test.snapshot.Values()
			if len(values) != 1 || values[test.key] != test.want {
				t.Fatalf("歌词进度单键包限幅错误: %#v", values)
			}
		})
	}
}
