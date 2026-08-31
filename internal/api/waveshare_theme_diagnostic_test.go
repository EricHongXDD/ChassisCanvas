package api

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestWaveshareCompiledThemeDiagnostic 供实机验收检查最终落盘的 Theme。
// 普通自动测试没有指定文件时跳过，不依赖用户目录或外接屏幕。
func TestWaveshareCompiledThemeDiagnostic(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("CASESCREEN_THEME_DIAGNOSTIC"))
	if path == "" {
		t.Skip("设置 CASESCREEN_THEME_DIAGNOSTIC 后检查实际编译主题")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	table, err := parseWaveshareThemeWidgetTable(data)
	if err != nil {
		t.Fatal(err)
	}
	typeCounts := make(map[string]int)
	audioWidgets := 0
	audioKeys := make(map[string]bool)
	lyricsKeys := map[string]int{
		"CaseScreen_LyricsCurrent": 0,
		"CaseScreen_LyricsNext":    0,
	}
	// 原厂 PCMonitorLinux 只把当前句和下一句绑定到动态文本控件；主机
	// 内部时间轴不能伪装成屏端已经实现的消费者。
	unboundTimelineKeys := map[string]int{
		"CaseScreen_LyricsPositionMs":  0,
		"CaseScreen_LyricsCurrentAtMs": 0,
		"CaseScreen_LyricsNextAtMs":    0,
		"CaseScreen_LyricsPlaying":     0,
	}
	for _, widget := range table.widgets {
		widgetType := widget.fields["type"]
		typeCounts[widgetType]++
		systemDataName := widget.fields["system_data_name"]
		if widgetType == "102" && (strings.HasPrefix(systemDataName, "A") || strings.HasPrefix(systemDataName, "P")) {
			audioWidgets++
			audioKeys[systemDataName] = true
		}
		if _, ok := lyricsKeys[systemDataName]; ok {
			lyricsKeys[systemDataName]++
			if widgetType != "8" || widget.fields["system_data_flag"] != "1" {
				t.Fatalf("实际歌词键没有绑定动态文本控件: key=%s type=%s flag=%s", systemDataName, widgetType, widget.fields["system_data_flag"])
			}
		}
		if _, ok := unboundTimelineKeys[systemDataName]; ok {
			unboundTimelineKeys[systemDataName]++
		}
		for fieldName, value := range widget.fields {
			lower := strings.ToLower(fieldName + "\n" + value)
			for _, forbidden := range []string{
				"http://", "https://", "ws://", "wss://", "acrcloud", "recognition",
				"microphone", "麦克风", "audio mic",
			} {
				if strings.Contains(lower, forbidden) {
					t.Fatalf("实际主题控件含屏端联网/识歌引用: field=%s value=%q", fieldName, value)
				}
			}
		}
	}
	if typeCounts["107"] != 0 || typeCounts["108"] != 0 {
		t.Fatalf("实际主题仍包含屏端麦克风控件: type107=%d type108=%d", typeCounts["107"], typeCounts["108"])
	}
	if audioWidgets == 0 {
		t.Fatal("实际主题没有主机频谱 type102 控件")
	}
	for key, count := range lyricsKeys {
		// 艺术歌词会为描边、阴影和柔光生成多个共享同一动态键的驻留文本控件。
		// 至少要有一个消费者，同时限制数量避免异常主题无限放大日常更新成本。
		if count < 1 || count > 32 {
			t.Fatalf("实际主题歌词动态键数量错误: key=%s count=%d", key, count)
		}
	}
	for key, count := range unboundTimelineKeys {
		if count != 0 {
			t.Fatalf("实际主题意外绑定了未实现的屏端歌词时间轴键: key=%s count=%d", key, count)
		}
	}
	if rawExpected := strings.TrimSpace(os.Getenv("CASESCREEN_THEME_EXPECT_AUDIO_WIDGETS")); rawExpected != "" {
		expected, parseErr := strconv.Atoi(rawExpected)
		if parseErr != nil || expected < 1 {
			t.Fatalf("CASESCREEN_THEME_EXPECT_AUDIO_WIDGETS 无效: %q", rawExpected)
		}
		if audioWidgets != expected {
			t.Fatalf("实际主机频谱控件数=%d，期望 %d", audioWidgets, expected)
		}
	}
	keys := make([]string, 0, len(audioKeys))
	for key := range audioKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	t.Logf("控件总数=%d type102=%d type107=%d type108=%d 主机频谱控件=%d 去重频带键=%d 歌词Current=%d 歌词Next=%d 屏端时间轴键=%d",
		len(table.widgets), typeCounts["102"], typeCounts["107"], typeCounts["108"], audioWidgets, len(keys),
		lyricsKeys["CaseScreen_LyricsCurrent"], lyricsKeys["CaseScreen_LyricsNext"],
		unboundTimelineKeys["CaseScreen_LyricsPositionMs"]+unboundTimelineKeys["CaseScreen_LyricsCurrentAtMs"]+
			unboundTimelineKeys["CaseScreen_LyricsNextAtMs"]+unboundTimelineKeys["CaseScreen_LyricsPlaying"])
}
