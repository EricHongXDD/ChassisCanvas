package lyrics

import "strings"

func parseWindowTitle(source, title string) NowPlaying {
	title = strings.TrimSpace(title)
	for _, suffix := range []string{
		" - 网易云音乐", " - QQ音乐", " - 酷狗音乐", " - 搜狗音乐", " - 酷我音乐",
		" — 网易云音乐", " — QQ音乐", " — 酷狗音乐", " — 搜狗音乐", " — 酷我音乐",
	} {
		title = strings.TrimSpace(strings.TrimSuffix(title, suffix))
	}
	lower := strings.ToLower(title)
	for _, generic := range []string{"网易云音乐", "qq音乐", "酷狗音乐", "搜狗音乐", "酷我音乐", "netease cloud music", "qqmusic", "kugou", "sogou music", "kuwo music"} {
		if lower == strings.ToLower(generic) {
			return NowPlaying{}
		}
	}
	if title == "" {
		return NowPlaying{}
	}
	separators := []string{" - ", " — ", " – "}
	for _, separator := range separators {
		if index := strings.Index(title, separator); index > 0 {
			return NowPlaying{
				Source: source,
				Title:  strings.TrimSpace(title[:index]),
				Artist: strings.TrimSpace(title[index+len(separator):]),
			}
		}
	}
	// 窗口标题只能证明歌曲身份，不能证明播放器是否正在播放。
	// 兜底来源保持暂停状态，直到 SMTC 能重新提供可信播放状态。
	return NowPlaying{Source: source, Title: title}
}
