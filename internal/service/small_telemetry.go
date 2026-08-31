package service

import (
	"strings"
	"time"

	"casescreen/internal/theme"
)

// waveshareLyricProgressInterval 以 0..10000 细进度驱动新版驻留主题。
// 50ms 可以把无频谱时的短句进度提高到 20Hz；活跃频谱只合入三字符
// LFP，以 9..10Hz 平滑刷新且不突破 85% 线速预算。旧 0..100 长键
// 只在跨整百分比时独立发送；数值未跨可见像素时去重。
const waveshareLyricProgressInterval = 50 * time.Millisecond

// manifestNeedsSmallTelemetry 判断屏内控件是否依赖每秒更新的键值数据。
// 音频与歌词使用各自的发送路径，不在这里重复计入。
func manifestNeedsSmallTelemetry(manifest theme.Manifest) bool {
	for _, layer := range manifest.Layers {
		if !theme.LayerVisible(layer) || layer.Opacity <= 0 {
			continue
		}
		switch layer.Type {
		case theme.LayerTelemetry, theme.LayerClock,
			theme.LayerProgressBar, theme.LayerGauge, theme.LayerGradientGauge, theme.LayerLineChart:
			return true
		case theme.LayerText:
			if strings.TrimSpace(layerDataKey(layer)) != "" {
				return true
			}
		}
	}
	return false
}

// manifestNeedsLyricProgress 只在确实画出动态进度轨时启用小步进更新。
// 极简样式和关闭进度轨的主题继续只在换句时发送歌词文本。
func manifestNeedsLyricProgress(manifest theme.Manifest) bool {
	for _, layer := range manifest.Layers {
		if layer.Type == theme.LayerLyrics && theme.LayerVisible(layer) && layer.Opacity > 0 && theme.LyricsShowsProgress(layer) {
			return true
		}
	}
	return false
}

func layerDataKey(layer theme.Layer) string {
	if layer.Options == nil {
		return ""
	}
	value, _ := layer.Options["data_key"].(string)
	return value
}
