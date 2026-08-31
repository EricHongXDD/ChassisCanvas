package telemetry

import (
	"fmt"
	"strconv"
	"time"
)

const (
	// LyricsProgressLegacyKey 保留给已经写入设备的旧 Theme。
	LyricsProgressLegacyKey = "CaseScreen_LyricsProgress"
	// LyricsProgressFineKey 供新版驻留 Theme 使用。三字符键独立发送时约
	// 64 字节；有活跃频谱时只增加约 22 字节，可在 85% 线速预算内合包。
	LyricsProgressFineKey = "LFP"
	// LyricsVisibleKey 只在换句、暂停、取词完成或清空时发送。驻留 Theme
	// 用这个两字符标量同时隐藏歌词面板、文字与进度轨，不进入 20Hz 进度包。
	LyricsVisibleKey        = "LV"
	LyricsProgressLegacyMax = 100
	LyricsProgressFineMax   = 10000
)

// Snapshot 是一次采集结果，字段命保持原厂主题兼容。
type Snapshot struct {
	CapturedAt            time.Time `json:"captured_at"`
	CPUPercent            float64   `json:"cpu_percent"`
	MemoryPercent         float64   `json:"memory_percent"`
	MemoryUsedGiB         float64   `json:"memory_used_gib"`
	MemoryTotalGiB        float64   `json:"memory_total_gib"`
	CPUModel              string    `json:"cpu_model,omitempty"`
	GPUModel              string    `json:"gpu_model,omitempty"`
	GPUPercent            *float64  `json:"gpu_percent,omitempty"`
	GPUTemperature        *float64  `json:"gpu_temperature,omitempty"`
	GPUHotspotTemperature *float64  `json:"gpu_hotspot_temperature,omitempty"`
	GPUMemoryTemperature  *float64  `json:"gpu_memory_temperature,omitempty"`
	CurrentDecibels       float64   `json:"current_decibels"`
	AverageDecibels       float64   `json:"average_decibels"`
	MaximumDecibels       float64   `json:"maximum_decibels"`
	Volume                int       `json:"volume"`
	// OmitHardwareMetrics 用于采集失败或仅发送歌词/音频的设备包，避免把零值伪装成实测数据。
	OmitHardwareMetrics bool `json:"-"`
	// OmitClock 用于纯歌词/音频包，避免附带主题未使用且每秒变化的日期时间键。
	OmitClock     bool   `json:"-"`
	AudioSpectrum string `json:"-"`
	LyricsCurrent string `json:"lyrics_current,omitempty"`
	LyricsNext    string `json:"lyrics_next,omitempty"`
	// 行时间戳和播放状态属于歌词小包协议。当前原厂 Theme 只消费双行
	// 文本，但保留这些小字段供暂停语义和后续屏端消费者使用。
	LyricsCurrentAtMS    int64  `json:"-"`
	LyricsNextAtMS       int64  `json:"-"`
	LyricsCurrentAtKnown bool   `json:"-"`
	LyricsNextAtKnown    bool   `json:"-"`
	LyricsPlaying        bool   `json:"-"`
	LyricsMode           string `json:"-"`
	// LyricsProgress 是兼容旧 Theme 的 0..100 整数百分比；LyricsProgressFine
	// 是新版 Theme 使用的 0..10000 细进度。两者都由主机计算，屏端不需要
	// 重建播放器时钟；Fine 由频谱包或独立短包承载，旧键仅在整百分比跨界时发送。
	LyricsProgress          int  `json:"-"`
	LyricsProgressKnown     bool `json:"-"`
	LyricsProgressFine      int  `json:"-"`
	LyricsProgressFineKnown bool `json:"-"`
	// LyricsVisible 只随歌词状态事件发送；暂停时仍为 true。连续进度包不带
	// 这个字段，避免给日常 USB 通讯增加无意义的重复开销。
	LyricsVisible      bool `json:"-"`
	LyricsVisibleKnown bool `json:"-"`
	// LyricsClear 强制隐藏歌词层并清空两个驻留文本控件。map 中必须保留空
	// 字符串，因此不能复用普通歌词字段的 omitempty 发送条件。
	LyricsClear bool `json:"-"`
}

// Values 转换为 PCMonitorLinux 直接消费的字符串键值。
func (snapshot Snapshot) Values() map[string]string {
	now := snapshot.CapturedAt
	values := make(map[string]string)
	if !snapshot.OmitClock {
		_, week := now.ISOWeek()
		values["year"] = now.Format("2006")
		values["month"] = now.Format("01")
		values["day"] = now.Format("02")
		values["hour"] = now.Format("15")
		values["minute"] = now.Format("04")
		values["second"] = now.Format("05")
		values["date"] = now.Format("2006-01-02")
		values["time"] = now.Format("15:04:05")
		values["week"] = chineseWeekday(now.Weekday())
		values["week_number"] = strconv.Itoa(week)
	}
	if !snapshot.OmitHardwareMetrics {
		values["Cpu_Freq"] = fmt.Sprintf("%.0f%%", snapshot.CPUPercent)
		values["Memory_Load"] = fmt.Sprintf("%.0f%%", snapshot.MemoryPercent)
		values["Memory_Used"] = fmt.Sprintf("%.1fGB", snapshot.MemoryUsedGiB)
		values["Memory_Available"] = fmt.Sprintf("%.1fGB", snapshot.MemoryTotalGiB-snapshot.MemoryUsedGiB)
		values["RAM"] = fmt.Sprintf("%.0fG", snapshot.MemoryTotalGiB)
		// GPU 监控目标切换后，新显卡可能不提供负载或某类温度。
		// 显式下发缺失值，避免增量协议保留上一块显卡的旧数据。
		values["Gpu_Model"] = "--"
		values["Gpu_Load"] = "--"
		values["GPU_Temp"] = "--"
		values["GPU_Hotspot_Temp"] = "--"
		values["GPU_Memory_Temp"] = "--"
	}
	if !snapshot.OmitHardwareMetrics && snapshot.CPUModel != "" {
		values["Cpu_Model"] = snapshot.CPUModel
	}
	if !snapshot.OmitHardwareMetrics && snapshot.GPUModel != "" {
		values["Gpu_Model"] = snapshot.GPUModel
	}
	if snapshot.AudioSpectrum != "" {
		// 原厂音频控件的 system_data_name 就是 audio mic；未知键会被旧固件安全忽略。
		values["audio mic"] = snapshot.AudioSpectrum
	}
	if snapshot.LyricsClear {
		values["CaseScreen_LyricsCurrent"] = ""
		values["CaseScreen_LyricsNext"] = ""
	} else {
		if snapshot.LyricsCurrent != "" {
			values["CaseScreen_LyricsCurrent"] = snapshot.LyricsCurrent
		}
		if snapshot.LyricsNext != "" {
			values["CaseScreen_LyricsNext"] = snapshot.LyricsNext
		}
	}
	if snapshot.LyricsClear {
		values[LyricsVisibleKey] = "0"
	} else if snapshot.LyricsVisibleKnown {
		if snapshot.LyricsVisible {
			values[LyricsVisibleKey] = "1"
		} else {
			values[LyricsVisibleKey] = "0"
		}
	}
	lyricsPacket := snapshot.LyricsClear || snapshot.LyricsCurrent != "" || snapshot.LyricsNext != ""
	if lyricsPacket {
		// 未知时间戳也显式发送空值，避免增量更新协议让屏端残留上一首歌
		// 的时间轴；清空包同样把 playing 复位为 false。
		values["CaseScreen_LyricsCurrentAtMs"] = ""
		values["CaseScreen_LyricsNextAtMs"] = ""
		values["CaseScreen_LyricsPlaying"] = strconv.FormatBool(snapshot.LyricsPlaying && !snapshot.LyricsClear)
		if snapshot.LyricsCurrentAtKnown && !snapshot.LyricsClear {
			values["CaseScreen_LyricsCurrentAtMs"] = strconv.FormatInt(max(0, snapshot.LyricsCurrentAtMS), 10)
		}
		if snapshot.LyricsNextAtKnown && !snapshot.LyricsClear {
			values["CaseScreen_LyricsNextAtMs"] = strconv.FormatInt(max(0, snapshot.LyricsNextAtMS), 10)
		}
	}
	if snapshot.LyricsClear {
		values[LyricsProgressLegacyKey] = "0"
		values[LyricsProgressFineKey] = "0"
	} else {
		if snapshot.LyricsProgressKnown {
			values[LyricsProgressLegacyKey] = strconv.Itoa(max(0, min(LyricsProgressLegacyMax, snapshot.LyricsProgress)))
		}
		if snapshot.LyricsProgressFineKnown {
			values[LyricsProgressFineKey] = strconv.Itoa(max(0, min(LyricsProgressFineMax, snapshot.LyricsProgressFine)))
		}
	}
	if !snapshot.OmitHardwareMetrics && snapshot.GPUPercent != nil {
		values["Gpu_Load"] = fmt.Sprintf("%.0f%%", *snapshot.GPUPercent)
	}
	if !snapshot.OmitHardwareMetrics && snapshot.GPUTemperature != nil {
		values["GPU_Temp"] = fmt.Sprintf("%.0f℃", *snapshot.GPUTemperature)
	}
	if !snapshot.OmitHardwareMetrics && snapshot.GPUHotspotTemperature != nil {
		values["GPU_Hotspot_Temp"] = fmt.Sprintf("%.0f℃", *snapshot.GPUHotspotTemperature)
	}
	if !snapshot.OmitHardwareMetrics && snapshot.GPUMemoryTemperature != nil {
		values["GPU_Memory_Temp"] = fmt.Sprintf("%.0f℃", *snapshot.GPUMemoryTemperature)
	}
	return values
}

func chineseWeekday(day time.Weekday) string {
	return [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[day]
}
