package api

import (
	"bytes"
	"encoding/binary"
	"os"
	"strconv"
	"testing"

	"casescreen/internal/telemetry"
)

func TestStandaloneLyricsThemeHasOnlyBackgroundAndDynamicLyrics(t *testing.T) {
	for _, size := range []struct {
		width  int
		height int
	}{{1024, 600}, {1920, 480}, {480, 1920}} {
		manifest, err := waveshareStandaloneLyricsManifest(size.width, size.height)
		if err != nil {
			t.Fatalf("%dx%d 清单失败: %v", size.width, size.height, err)
		}
		compiled, err := compileWaveshareThemeWidgets(requireWaveshareTestBaseTheme(t), manifest)
		if err != nil {
			t.Fatalf("%dx%d 编译失败: %v", size.width, size.height, err)
		}
		if err := validateWaveshareStandaloneLyricsTheme(compiled, size.width, size.height); err != nil {
			t.Fatalf("%dx%d 解析校验失败: %v", size.width, size.height, err)
		}
	}
}

func TestStandaloneLyricsValidatorRejectsVisibilityResidue(t *testing.T) {
	manifest, err := waveshareStandaloneLyricsManifest(1024, 600)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compileWaveshareThemeWidgets(requireWaveshareTestBaseTheme(t), manifest)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		key    string
		field  string
		value  string
		widget string
	}{
		{name: "初始歌词", key: "CaseScreen_LyricsCurrent", field: "text_str", value: "正在等待歌词", widget: "8"},
		{name: "可见性底色", key: telemetry.LyricsVisibleKey, field: "back_color", value: "r=1,g=1,b=1,a=255", widget: "102"},
		{name: "可见性边框", key: telemetry.LyricsVisibleKey, field: "border_width", value: "1", widget: "102"},
		{name: "可见性量程", key: telemetry.LyricsVisibleKey, field: "system_data_max_value", value: "100", widget: "102"},
		{name: "进度底色", key: telemetry.LyricsProgressFineKey, field: "back_color", value: "r=1,g=1,b=1,a=255", widget: "102"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			table, parseErr := parseWaveshareThemeWidgetTable(compiled)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			changed := false
			for index := range table.widgets {
				widget := &table.widgets[index]
				if widget.fields["type"] == test.widget && widget.fields["system_data_name"] == test.key {
					widget.fields[test.field] = test.value
					changed = true
					break
				}
			}
			if !changed {
				t.Fatalf("没有找到待篡改控件 %s/%s", test.widget, test.key)
			}
			tampered, rewriteErr := rewriteWaveshareThemeWidgets(compiled, table.widgets)
			if rewriteErr != nil {
				t.Fatal(rewriteErr)
			}
			if validateErr := validateWaveshareStandaloneLyricsTheme(tampered, 1024, 600); validateErr == nil {
				t.Fatalf("校验器放过了%s残留", test.name)
			}
		})
	}
}

func TestStandaloneLyricsCompilerPreservesReplacedVideo(t *testing.T) {
	video := testWaveshareMP4([]byte("dark-frame"))
	replaced, err := replaceWaveshareEmbeddedVideoData(requireWaveshareTestBaseTheme(t), video)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := waveshareStandaloneLyricsManifest(1024, 600)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compileWaveshareThemeWidgets(replaced, manifest)
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := extractWaveshareEmbeddedVideo(compiled)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(embedded, video) {
		t.Fatal("歌词控件编译覆盖了已经替换的深色背景")
	}
}

func TestStandaloneLyricsPortraitLayoutIsReadable(t *testing.T) {
	const width, height = 480, 1920
	manifest, err := waveshareStandaloneLyricsManifest(width, height)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compileWaveshareThemeWidgets(requireWaveshareTestBaseTheme(t), manifest)
	if err != nil {
		t.Fatal(err)
	}
	table, err := parseWaveshareThemeWidgetTable(compiled)
	if err != nil {
		t.Fatal(err)
	}
	widgets := map[string]map[string]string{}
	for _, widget := range table.widgets {
		key := widget.fields["system_data_name"]
		if key == "CaseScreen_LyricsCurrent" || key == "CaseScreen_LyricsNext" {
			widgets[key] = widget.fields
		}
	}
	current := readablePortraitText(t, widgets["CaseScreen_LyricsCurrent"], width, height)
	next := readablePortraitText(t, widgets["CaseScreen_LyricsNext"], width, height)
	if current.y < height*2/3 {
		t.Fatalf("竖屏当前歌词没有位于下方阅读区: y=%d", current.y)
	}
	if current.y+current.height > next.y {
		t.Fatalf("竖屏两行歌词重叠: current=%+v next=%+v", current, next)
	}
	if current.size < 18 || next.size < 16 {
		t.Fatalf("竖屏歌词字号过小: current=%d next=%d", current.size, next.size)
	}
}

type portraitTextMetrics struct {
	y      int
	height int
	size   int
}

func readablePortraitText(t *testing.T, fields map[string]string, width, height int) portraitTextMetrics {
	t.Helper()
	if fields == nil || fields["system_data_flag"] != "1" {
		t.Fatalf("竖屏歌词动态控件缺失: %+v", fields)
	}
	x, xErr := strconv.Atoi(fields["x"])
	y, yErr := strconv.Atoi(fields["y"])
	size, sizeErr := strconv.Atoi(fields["text_size"])
	if xErr != nil || yErr != nil || sizeErr != nil {
		t.Fatalf("竖屏歌词坐标或字号无效: %+v", fields)
	}
	textWidth := waveshareEstimatedRenderedTextWidth(fields["text_str"], size)
	textHeight := waveshareRenderedTextHeight(size)
	if x < 0 || y < 0 || x+textWidth > width || y+textHeight > height {
		t.Fatalf("竖屏歌词超出画布: x=%d y=%d w=%d h=%d", x, y, textWidth, textHeight)
	}
	return portraitTextMetrics{y: y, height: textHeight, size: size}
}

func TestStandaloneLyricsManifestRejectsInvalidSize(t *testing.T) {
	if _, err := waveshareStandaloneLyricsManifest(0, 600); err == nil {
		t.Fatal("无效分辨率未被拒绝")
	}
}

func TestBuildStandaloneLyricsThemeWithFFmpeg(t *testing.T) {
	ffmpegPath := os.Getenv("CASESCREEN_TEST_FFMPEG")
	if ffmpegPath == "" {
		t.Skip("未配置 CASESCREEN_TEST_FFMPEG")
	}
	for _, size := range []struct {
		width  int
		height int
	}{{1024, 600}, {480, 1920}} {
		data, err := BuildWaveshareStandaloneLyricsTheme(t.Context(), ffmpegPath, size.width, size.height)
		if err != nil {
			t.Fatalf("%dx%d 实际构建失败: %v", size.width, size.height, err)
		}
		if err := validateWaveshareStandaloneLyricsTheme(data, size.width, size.height); err != nil {
			t.Fatalf("%dx%d 实际构建校验失败: %v", size.width, size.height, err)
		}
	}
}

func testWaveshareMP4(payload []byte) []byte {
	var output bytes.Buffer
	writeBox := func(boxType string, body []byte) {
		_ = binary.Write(&output, binary.BigEndian, uint32(8+len(body)))
		output.WriteString(boxType)
		output.Write(body)
	}
	writeBox("ftyp", []byte("isom\x00\x00\x02\x00isom"))
	writeBox("moov", nil)
	writeBox("mdat", payload)
	return output.Bytes()
}
