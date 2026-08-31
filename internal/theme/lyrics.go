package theme

import "strings"

const (
	LyricsStylePlayer  = "player"
	LyricsStyleKaraoke = "karaoke"
	LyricsStyleClassic = "classic"
	LyricsStyleMinimal = "minimal"
)

// LyricsStyle 返回编辑器和两条实际输出链路共同认可的歌词样式。
// 未知值按播放器样式降级，避免预览、主机合成与屏端编译各自猜测。
func LyricsStyle(layer Layer) string {
	style := LyricsStylePlayer
	if layer.Options != nil {
		if value, ok := layer.Options["style"].(string); ok {
			style = strings.ToLower(strings.TrimSpace(value))
		}
	}
	switch style {
	case LyricsStylePlayer, LyricsStyleKaraoke, LyricsStyleClassic, LyricsStyleMinimal:
		return style
	default:
		return LyricsStylePlayer
	}
}

// LyricsShowsNext 表示样式是否承诺显示下一句。极简样式严格保持单行。
func LyricsShowsNext(layer Layer) bool {
	return LyricsStyle(layer) != LyricsStyleMinimal
}

// LyricsUsesKaraokeHighlight 只允许播放器和卡拉 OK 样式启用播放高亮。
// 微雪固件会把它降级为整句强调色，主机合成设备可以逐字裁切。
func LyricsUsesKaraokeHighlight(layer Layer) bool {
	style := LyricsStyle(layer)
	return (style == LyricsStylePlayer || style == LyricsStyleKaraoke) &&
		OptionBool(layer.Options, "karaoke_highlight", true)
}

// LyricsShowsProgress 表示主题是否真正需要消费细粒度歌词进度小字段。
// 极简样式即使继承旧主题的 true，也不得偷偷生成固定进度装饰。
func LyricsShowsProgress(layer Layer) bool {
	style := LyricsStyle(layer)
	if style == LyricsStyleMinimal {
		return false
	}
	defaultValue := style == LyricsStylePlayer || style == LyricsStyleKaraoke
	return OptionBool(layer.Options, "show_progress", defaultValue)
}

// LyricsUsesSmoothTransition 只在能兑现淡入动画的主机播放器类样式中生效。
func LyricsUsesSmoothTransition(layer Layer) bool {
	style := LyricsStyle(layer)
	return (style == LyricsStylePlayer || style == LyricsStyleKaraoke) &&
		OptionBool(layer.Options, "smooth_transition", true)
}
