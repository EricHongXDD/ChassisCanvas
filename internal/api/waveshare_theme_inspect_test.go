package api

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

func TestFilterWaveshareThemeWidgetsRemovesOldTelemetry(t *testing.T) {
	var source bytes.Buffer
	source.WriteString(waveshareThemeHeader)
	writeThemeUint32(&source, 0) // 全局 QVariantMap。
	writeThemeUint32(&source, 0) // 画布 QStringMap。
	writeThemeUint32(&source, 2)
	writeThemeStringMap(&source, map[string]string{"type": "8", "system_data_name": "Cpu使用率"})
	writeThemeStringMap(&source, map[string]string{"type": "100", "path": "/video/background.mp4"})
	source.WriteString("TAIL")

	filtered, err := filterWaveshareThemeWidgets(source.Bytes(), func(fields map[string]string) bool { return fields["type"] == "100" })
	if err != nil {
		t.Fatal(err)
	}
	countOffset := len(waveshareThemeHeader) + 8
	if got := binary.BigEndian.Uint32(filtered[countOffset : countOffset+4]); got != 1 {
		t.Fatalf("清理后控件数量=%d，期望 1", got)
	}
	if bytes.Contains(filtered, []byte{0x00, 'C', 0x00, 'p', 0x00, 'u'}) {
		t.Fatal("旧 CPU 指标仍保留在主题控件表中")
	}
	if !bytes.HasSuffix(filtered, []byte("TAIL")) {
		t.Fatal("主题尾部资源被改坏")
	}
}

func TestWaveshareWidgetFilterFollowsManifestLayers(t *testing.T) {
	backgroundOnly := themeManifestWithLayers("background_image")
	filter := waveshareWidgetFilter(backgroundOnly)
	if filter(map[string]string{"type": "8"}) || filter(map[string]string{"type": "108"}) || !filter(map[string]string{"type": "100"}) {
		t.Fatal("纯背景主题没有清除遥测和音频控件")
	}
	withTelemetry := themeManifestWithLayers("background_image", "telemetry")
	telemetryFilter := waveshareWidgetFilter(withTelemetry)
	if !telemetryFilter(map[string]string{"type": "8", "system_data_name": "Cpu使用率"}) {
		t.Fatal("遥测主题错误清除了遥测控件")
	}
	if telemetryFilter(map[string]string{"type": "8", "system_data_name": "显卡温度"}) {
		t.Fatal("没有添加的显卡温度指标不应保留")
	}
}

func TestFilterOriginalAstronautThemeLeavesOnlyBackground(t *testing.T) {
	path := filepath.Join("..", "..", ".tmp", "waveshare", "070-astronaut.Theme")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skip("本地未保留原始微雪主题样本")
	}
	filtered, err := filterWaveshareThemeWidgets(data, waveshareWidgetFilter(themeManifestWithLayers("background_image")))
	if err != nil {
		t.Fatal(err)
	}
	cursor := len(waveshareThemeHeader)
	if err := skipWaveshareVariantMap(filtered, &cursor); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readWaveshareStringMap(filtered, &cursor); err != nil {
		t.Fatal(err)
	}
	count, err := readWaveshareUint32(filtered, &cursor)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("原宇航员主题清理后仍有 %d 个控件", count)
	}
	fields, _, err := readWaveshareStringMap(filtered, &cursor)
	if err != nil {
		t.Fatal(err)
	}
	if fields["type"] != "100" {
		t.Fatalf("保留的不是背景视频控件：%+v", fields)
	}
}

func TestWaveshareThemeSourcePreservesOriginalWidgets(t *testing.T) {
	originalPath := filepath.Join("..", "..", ".tmp", "waveshare", "070-astronaut.Theme")
	original, err := os.ReadFile(originalPath)
	if err != nil {
		t.Skip("本地未保留原始微雪主题样本")
	}
	filtered, err := filterWaveshareThemeWidgets(original, waveshareWidgetFilter(themeManifestWithLayers("background_image")))
	if err != nil {
		t.Fatal(err)
	}
	if count, err := waveshareThemeWidgetCount(original); err != nil || count <= 1 {
		t.Fatalf("原始主题控件数量无效: count=%d err=%v", count, err)
	}
	if count, err := waveshareThemeWidgetCount(filtered); err != nil || count != 1 {
		t.Fatalf("过滤主题控件数量无效: count=%d err=%v", count, err)
	}
	dataDirectory := t.TempDir()
	sourcePath := filepath.Join(dataDirectory, "filtered.Theme")
	if err := os.WriteFile(sourcePath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	server := &Server{dataDirectory: dataDirectory}
	resource := theme.Resource{LocalPath: sourcePath, RemotePath: "/data/theme/1024x600/custom.Theme"}
	basePath, err := server.ensureWaveshareThemeSource(resource)
	if err != nil {
		t.Fatal(err)
	}
	if baseCount, err := waveshareThemeWidgetCount(mustReadFile(t, basePath)); err != nil || baseCount <= 1 {
		t.Fatalf("原始主题缓存未保留控件: count=%d err=%v", baseCount, err)
	}
	if err := os.WriteFile(sourcePath, filtered, 0o600); err != nil {
		t.Fatal(err)
	}
	reusedPath, err := server.ensureWaveshareThemeSource(resource)
	if err != nil {
		t.Fatal(err)
	}
	if reusedPath != basePath {
		t.Fatalf("主题缓存路径发生变化: first=%q second=%q", basePath, reusedPath)
	}
	if reusedCount, err := waveshareThemeWidgetCount(mustReadFile(t, reusedPath)); err != nil || reusedCount <= 1 {
		t.Fatalf("再次应用时错误复用了过滤主题: count=%d err=%v", reusedCount, err)
	}
	// 旧版本可能已经把损坏/单背景主题写进 base 缓存；启动时必须识别并恢复。
	if err := os.WriteFile(basePath, filtered, 0o600); err != nil {
		t.Fatal(err)
	}
	recoveredPath, err := server.ensureWaveshareThemeSource(resource)
	if err != nil {
		t.Fatal(err)
	}
	if !waveshareThemeHasRequiredTemplates(mustReadFile(t, recoveredPath)) {
		t.Fatal("无效基础主题缓存没有恢复为完整模板")
	}
}

func TestCompileWaveshareWidgetsUsesEditorGeometry(t *testing.T) {
	path := filepath.Join("..", "..", ".tmp", "waveshare", "070-astronaut.Theme")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skip("本地未保留原始微雪主题样本")
	}
	manifest := theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{
		{ID: "image", Type: theme.LayerImage, Bounds: theme.Rect{X: .3, Y: .23, Width: .32, Height: .55}, ZIndex: 0},
		{ID: "telemetry", Type: theme.LayerTelemetry, Bounds: theme.Rect{X: .053, Y: .046, Width: .42, Height: .2}, Opacity: 1, ZIndex: 2, Options: map[string]any{"style": "cards", "fields": []string{"time", "Cpu_Freq", "Gpu_Load", "Memory_Load"}}},
		{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{X: .522, Y: .352, Width: .42, Height: .2}, Opacity: 1, ZIndex: 3},
		{ID: "ring", Type: theme.LayerAudioRing, Bounds: theme.Rect{X: .211, Y: .525, Width: .28, Height: .36}, Opacity: 1, ZIndex: 4},
		{ID: "lyrics", Type: theme.LayerLyrics, Bounds: theme.Rect{X: .491, Y: .566, Width: .42, Height: .2}, Opacity: 1, ZIndex: 5},
	}}
	compiled, err := compileWaveshareThemeWidgets(data, manifest)
	if err != nil {
		t.Fatal(err)
	}
	table, err := parseWaveshareThemeWidgetTable(compiled)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	visibilityWidgets := 0
	lyricsProgressWidgets := 0
	var firstBar map[string]string
	var firstRingBand map[string]string
	for _, widget := range table.widgets {
		counts[widget.fields["type"]]++
		if widget.fields["itemName"] == "bars-band-00" {
			firstBar = widget.fields
		}
		if widget.fields["itemName"] == "ring-band-00-out" {
			firstRingBand = widget.fields
		}
		if widget.fields["type"] == "101" {
			t.Fatalf("编译结果仍包含原宇航员仪表控件: %+v", widget.fields)
		}
		if widget.fields["system_data_name"] == telemetry.LyricsVisibleKey {
			visibilityWidgets++
		}
		if widget.fields["system_data_name"] == telemetry.LyricsProgressFineKey {
			lyricsProgressWidgets++
		}
	}
	expectedProgressWidgets := theme.DefaultAudioBarsCount + theme.DefaultAudioRingCount*2 + visibilityWidgets + lyricsProgressWidgets
	if counts["100"] != 1 || counts["102"] != expectedProgressWidgets || counts["108"] != 0 || counts["107"] != 0 ||
		counts["8"] < 20 || visibilityWidgets < 3 || lyricsProgressWidgets != 1 {
		t.Fatalf("编译控件数量无效: %+v", counts)
	}
	if firstBar["x"] != "536" || firstBar["y"] != "331" || firstBar["w"] != "120" || firstBar["h"] != "4" || firstBar["rotate"] != "-90" || firstBar["system_data_name"] != theme.WaveshareAudioBandKey(manifest.Layers[2], 0) {
		t.Fatalf("条状频谱没有使用编辑器坐标或系统音频键: %+v", firstBar)
	}
	if firstRingBand["system_data_name"] != theme.WaveshareAudioBandKey(manifest.Layers[3], 0) || firstRingBand["rotate"] != "-90" {
		t.Fatalf("环状频谱没有绑定系统音频频带: %+v", firstRingBand)
	}
	foundCPU := false
	foundLyrics := false
	for _, widget := range table.widgets {
		if widget.fields["system_data_name"] == "Cpu使用率" {
			foundCPU = true
			x, xErr := strconv.Atoi(widget.fields["x"])
			y, yErr := strconv.Atoi(widget.fields["y"])
			if xErr != nil || yErr != nil || x < 54 || y < 28 {
				t.Fatalf("CPU 控件坐标异常: %+v", widget.fields)
			}
		}
		if widget.fields["system_data_name"] == "CaseScreen_LyricsCurrent" && widget.fields["text_str"] == "" {
			foundLyrics = true
		}
		if widget.fields["system_data_name"] == "CaseScreen_LyricsCurrent" && widget.fields["text_str"] != "" {
			t.Fatalf("歌词动态控件仍含等待占位: %+v", widget.fields)
		}
	}
	if !foundCPU || !foundLyrics {
		t.Fatalf("缺少编译后的动态组件: cpu=%v lyrics=%v", foundCPU, foundLyrics)
	}
}

func TestCompileWaveshareDeviceMicrophoneMigratesToHostAudioWidgets(t *testing.T) {
	path := filepath.Join("..", "..", ".tmp", "waveshare", "070-astronaut.Theme")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skip("本地未保留原始微雪主题样本")
	}
	manifest := theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{
		{ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{X: .522, Y: .352, Width: .42, Height: .2}, Opacity: 1, ZIndex: 3, Options: map[string]any{"source": "device_mic"}},
		{ID: "ring", Type: theme.LayerAudioRing, Bounds: theme.Rect{X: .211, Y: .525, Width: .28, Height: .36}, Opacity: 1, ZIndex: 4, Options: map[string]any{"source": "device_mic"}},
	}}
	compiled, err := compileWaveshareThemeWidgets(data, manifest)
	if err != nil {
		t.Fatal(err)
	}
	table, err := parseWaveshareThemeWidgetTable(compiled)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	var firstBar map[string]string
	var firstRing map[string]string
	for _, widget := range table.widgets {
		counts[widget.fields["type"]]++
		if widget.fields["itemName"] == "bars-band-00" {
			firstBar = widget.fields
		}
		if widget.fields["itemName"] == "ring-band-00-out" {
			firstRing = widget.fields
		}
	}
	expectedSpectrumWidgets := theme.DefaultAudioBarsCount + theme.DefaultAudioRingCount*2
	if counts["102"] != expectedSpectrumWidgets || counts["108"] != 0 || counts["107"] != 0 {
		t.Fatalf("旧设备麦克风图层没有迁移为主机频带控件: %+v", counts)
	}
	if firstBar["system_data_name"] != theme.WaveshareAudioBandKey(manifest.Layers[0], 0) || firstRing["system_data_name"] != theme.WaveshareAudioBandKey(manifest.Layers[1], 0) {
		t.Fatalf("迁移后频谱没有绑定主机频带: bars=%+v ring=%+v", firstBar, firstRing)
	}
}

func TestCompileWaveshareExtendedResidentWidgets(t *testing.T) {
	manifest := theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{
		{ID: "text", Type: theme.LayerText, Bounds: theme.Rect{X: .02, Y: .02, Width: .25, Height: .1}, Opacity: 1, ZIndex: 1, Options: map[string]any{"text": "CaseScreen", "font_family": "digital", "font_size": 30.0}},
		{ID: "clock", Type: theme.LayerClock, Bounds: theme.Rect{X: .3, Y: .02, Width: .3, Height: .1}, Opacity: 1, ZIndex: 2, Options: map[string]any{"format": "HH:mm:ss"}},
		{ID: "progress", Type: theme.LayerProgressBar, Bounds: theme.Rect{X: .05, Y: .2, Width: .35, Height: .08}, Opacity: 1, ZIndex: 3, Options: map[string]any{"data_key": "Cpu_Freq", "show_label": false, "show_value": false}},
		{ID: "gauge", Type: theme.LayerGauge, Bounds: theme.Rect{X: .45, Y: .18, Width: .18, Height: .28}, Opacity: 1, ZIndex: 4, Options: map[string]any{"data_key": "Memory_Load", "style": "ring", "show_label": false, "show_value": false}},
		{ID: "gradient", Type: theme.LayerGradientGauge, Bounds: theme.Rect{X: .65, Y: .18, Width: .18, Height: .28}, Opacity: 1, ZIndex: 5, Options: map[string]any{"data_key": "Gpu_Load", "show_label": false, "show_value": false}},
		{ID: "chart", Type: theme.LayerLineChart, Bounds: theme.Rect{X: .05, Y: .55, Width: .55, Height: .25}, Opacity: 1, ZIndex: 6, Options: map[string]any{"data_key": "Cpu_Freq", "show_label": false}},
		{ID: "hidden", Type: theme.LayerProgressBar, Bounds: theme.Rect{X: .7, Y: .55, Width: .2, Height: .1}, Opacity: 1, ZIndex: 7, Options: map[string]any{"visible": false}},
	}}
	compiled, err := compileWaveshareThemeWidgets(requireWaveshareTestBaseTheme(t), manifest)
	if err != nil {
		t.Fatal(err)
	}
	table, err := parseWaveshareThemeWidgetTable(compiled)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	items := map[string]bool{}
	for _, widget := range table.widgets {
		counts[widget.fields["type"]]++
		items[widget.fields["itemName"]] = true
	}
	if counts["100"] != 1 || counts["101"] != 1 || counts["102"] != 1 || counts["106"] != 1 || counts["109"] != 1 || counts["8"] < 2 {
		t.Fatalf("扩展驻留控件类型不完整: %+v", counts)
	}
	if items["hidden"] {
		t.Fatal("隐藏图层仍被编译进屏端主题")
	}
}

func TestCompileWaveshareTextAlwaysUsesDeviceResidentFont(t *testing.T) {
	wanted := map[string]string{
		"pangmen":         "/usr/share/fonts/Dengb.ttf",
		"source-han":      "/usr/share/fonts/Source San Hans BLOD.ttf",
		"microsoft-yahei": "/usr/share/fonts/msyhbd.ttc",
		"simhei":          "/usr/share/fonts/simhei.ttf",
		"digital":         "/usr/share/fonts/LiquidCrystal-Bold.otf",
		"monospace":       "/usr/share/fonts/consolab.ttf",
		"rounded":         "/usr/share/fonts/YuGothM.ttc",
		"serif":           "/usr/share/fonts/simsunb.ttf",
	}
	manifest := theme.Manifest{Width: 1024, Height: 600}
	for family := range wanted {
		manifest.Layers = append(manifest.Layers, theme.Layer{
			ID: family, Type: theme.LayerText,
			Bounds: theme.Rect{X: .05, Y: .05, Width: .4, Height: .1}, Opacity: 1, ZIndex: 1,
			Options: map[string]any{"text": family, "font_family": family},
		})
	}
	table := compileWaveshareTable(t, manifest)
	paths := map[string]string{}
	for _, widget := range table.widgets {
		if widget.fields["type"] == "8" {
			if widget.fields["text_customFont_flag"] != "1" || widget.fields["text_customFont_path"] == "" {
				t.Fatalf("微雪文字没有使用 TF 内真实字体，屏端可能只显示背景: %+v", widget.fields)
			}
			paths[widget.fields["text_str"]] = widget.fields["text_customFont_path"]
		}
	}
	for family, want := range wanted {
		if paths[family] != want {
			t.Fatalf("字体 %s 没有落到屏内真实字体文件: got=%q want=%q", family, paths[family], want)
		}
	}
}

func TestCompileWaveshareRotatesWholeNativeComponentAroundLayerCenter(t *testing.T) {
	baseLayer := theme.Layer{
		ID: "telemetry", Type: theme.LayerTelemetry,
		Bounds: theme.Rect{X: .2, Y: .2, Width: .4, Height: .2}, Opacity: 1, ZIndex: 2,
		Options: map[string]any{"style": "list", "fields": []string{"Cpu_Freq", "Memory_Load"}},
	}
	baseTable := compileWaveshareTable(t, theme.Manifest{Width: 1000, Height: 600, Layers: []theme.Layer{baseLayer}})
	baseWidgets := widgetsWithItemPrefix(baseTable.widgets, "casescreen-")
	rotatedLayer := baseLayer
	rotatedLayer.Rotation = 90
	rotatedTable := compileWaveshareTable(t, theme.Manifest{Width: 1000, Height: 600, Layers: []theme.Layer{rotatedLayer}})
	rotatedWidgets := widgetsWithItemPrefix(rotatedTable.widgets, "casescreen-")
	if len(baseWidgets) == 0 || len(baseWidgets) != len(rotatedWidgets) {
		t.Fatalf("旋转前后原生子控件数量不一致: %d/%d", len(baseWidgets), len(rotatedWidgets))
	}
	centerX, centerY := 400.0, 180.0
	for index := range baseWidgets {
		baseX, _ := strconv.Atoi(baseWidgets[index].fields["x"])
		baseY, _ := strconv.Atoi(baseWidgets[index].fields["y"])
		wantX := int(math.Round(centerX - (float64(baseY) - centerY)))
		wantY := int(math.Round(centerY + (float64(baseX) - centerX)))
		if rotatedWidgets[index].fields["x"] != strconv.Itoa(wantX) || rotatedWidgets[index].fields["y"] != strconv.Itoa(wantY) || rotatedWidgets[index].fields["rotate"] != "90" {
			t.Fatalf("子控件未按图层中心旋转: base=%+v rotated=%+v want=(%d,%d,90)", baseWidgets[index].fields, rotatedWidgets[index].fields, wantX, wantY)
		}
	}
}

func TestCompileWaveshareTelemetryStylesUseNativeWidgets(t *testing.T) {
	for _, testCase := range []struct {
		style    string
		wantType string
	}{
		{style: "cards", wantType: "8"},
		{style: "list", wantType: "8"},
		{style: "compact", wantType: "8"},
		{style: "gauge", wantType: "102"},
		{style: "dials", wantType: "101"},
		{style: "tiles", wantType: "8"},
		{style: "digital", wantType: "8"},
		{style: "minimal", wantType: "8"},
	} {
		for _, resolution := range [][2]int{{1024, 600}, {480, 1920}} {
			name := testCase.style + "/" + strconv.Itoa(resolution[0]) + "x" + strconv.Itoa(resolution[1])
			t.Run(name, func(t *testing.T) {
				manifest := theme.Manifest{Width: resolution[0], Height: resolution[1], Layers: []theme.Layer{{
					ID: "telemetry", Type: theme.LayerTelemetry, Bounds: theme.Rect{X: .05, Y: .05, Width: .9, Height: .35}, Opacity: 1, ZIndex: 2,
					Options: map[string]any{"style": testCase.style, "fields": []string{"Cpu_Freq", "Gpu_Load", "Memory_Load", "time"}},
				}}}
				table := compileWaveshareTable(t, manifest)
				found := false
				for _, widget := range table.widgets {
					assertWaveshareProtocolFields(t, widget.fields)
					assertWaveshareWidgetInCanvas(t, widget.fields, resolution[0], resolution[1])
					if widget.fields["type"] == testCase.wantType {
						found = true
					}
				}
				if !found {
					t.Fatalf("样式 %s 没有生成原生类型 %s", testCase.style, testCase.wantType)
				}
			})
		}
	}
}

func TestWaveshareFitTelemetryTextSizeUsesCellWidthHeightAndTextLength(t *testing.T) {
	shortSize := waveshareFitTelemetryTextSize("42%", 96, 95, 31)
	longSize := waveshareFitTelemetryTextSize("2026-08-26", 96, 95, 31)
	if shortSize <= longSize {
		t.Fatalf("长文本没有获得更小字号: short=%d long=%d", shortSize, longSize)
	}
	for textValue, size := range map[string]int{"42%": shortSize, "2026-08-26": longSize} {
		if waveshareEstimatedTelemetryTextWidth(textValue, size) > 95 {
			t.Fatalf("文本 %q 自适应后仍超出单元格宽度: size=%d width=%d", textValue, size, waveshareEstimatedTelemetryTextWidth(textValue, size))
		}
		if waveshareTelemetryRenderedFontHeight(size) > 31 {
			t.Fatalf("文本 %q 自适应后仍超出单元格高度: size=%d height=%d", textValue, size, waveshareTelemetryRenderedFontHeight(size))
		}
	}
}

func TestCompileWaveshareDigitalTelemetryFitsSmallMultiFieldGrid(t *testing.T) {
	const (
		componentWidth  = 430
		componentHeight = 120
		columns         = 4
	)
	manifest := theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{{
		ID: "telemetry", Type: theme.LayerTelemetry,
		Bounds:  theme.Rect{X: 0, Y: 0, Width: float64(componentWidth) / 1024, Height: float64(componentHeight) / 600},
		Opacity: 1, ZIndex: 2,
		Options: map[string]any{
			"style":     "digital",
			"font_size": 96.0,
			"fields":    []string{"Cpu_Freq", "Gpu_Load", "Memory_Load", "GPU_Temp", "date"},
		},
	}}}
	widgets := widgetsWithItemPrefix(compileWaveshareTable(t, manifest).widgets, "casescreen-")
	if len(widgets) != 10 {
		t.Fatalf("数字遥测控件数=%d，期望 5 个标签和 5 个数值", len(widgets))
	}
	cellWidth, cellHeight := componentWidth/columns, componentHeight/2
	valueSizes := make(map[string]int)
	for widgetIndex, widget := range widgets {
		fields := widget.fields
		if fields["type"] != "8" {
			t.Fatalf("数字遥测生成了非文字控件: %+v", fields)
		}
		metricIndex := widgetIndex / 2
		cellRight := (metricIndex%columns+1)*cellWidth - 6
		cellBottom := (metricIndex/columns + 1) * cellHeight
		x, xErr := strconv.Atoi(fields["x"])
		y, yErr := strconv.Atoi(fields["y"])
		size, sizeErr := strconv.Atoi(fields["text_size"])
		if xErr != nil || yErr != nil || sizeErr != nil {
			t.Fatalf("数字遥测文字坐标或字号无效: %+v", fields)
		}
		if x+waveshareEstimatedTelemetryTextWidth(fields["text_str"], size) > cellRight {
			t.Fatalf("文字横向超出第 %d 个单元格: %+v", metricIndex, fields)
		}
		if y+waveshareTelemetryRenderedFontHeight(size) > cellBottom {
			t.Fatalf("文字纵向超出第 %d 个单元格: %+v", metricIndex, fields)
		}
		if fields["system_data_flag"] == "1" {
			valueSizes[fields["text_str"]] = size
		}
	}
	if valueSizes["42%"] <= valueSizes["2026-08-26"] {
		t.Fatalf("编译后的日期字号没有按文本长度缩小: %+v", valueSizes)
	}
}

func TestCompileWaveshareDigitalTelemetryUsesChineseFontForLabelsOnly(t *testing.T) {
	for _, style := range []string{"cards", "list", "compact", "gauge", "dials", "tiles", "digital"} {
		t.Run(style, func(t *testing.T) {
			manifest := theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{{
				ID: "telemetry", Type: theme.LayerTelemetry,
				Bounds:  theme.Rect{X: .05, Y: .05, Width: .5, Height: .25},
				Opacity: 1, ZIndex: 2,
				Options: map[string]any{
					"style":       style,
					"font_family": "digital",
					"fields":      []string{"Memory_Used"},
				},
			}}}
			widgets := widgetsWithItemPrefix(compileWaveshareTable(t, manifest).widgets, "casescreen-")
			var labelWidget, valueWidget *waveshareThemeWidget
			for index := range widgets {
				widget := &widgets[index]
				if widget.fields["type"] != "8" {
					continue
				}
				if widget.fields["system_data_flag"] == "1" {
					valueWidget = widget
				} else if widget.fields["text_str"] == "已用内存" {
					labelWidget = widget
				}
			}
			if labelWidget == nil || valueWidget == nil {
				t.Fatalf("样式 %s 没有同时生成标签和数值文字: %+v", style, widgets)
			}
			if labelWidget.fields["text_customFont_path"] != "/usr/share/fonts/Source San Hans BLOD.ttf" || labelWidget.fields["text_family"] != "Source Han Sans CN" {
				t.Fatalf("样式 %s 的中文标签没有回退到思源黑体: %+v", style, labelWidget.fields)
			}
			if valueWidget.fields["text_customFont_path"] != "/usr/share/fonts/LiquidCrystal-Bold.otf" || valueWidget.fields["text_family"] != "Liquid Crystal" {
				t.Fatalf("样式 %s 的动态数值没有保留数字字体: %+v", style, valueWidget.fields)
			}
		})
	}
}

func TestCompileWaveshareTelemetryPreservesNonDigitalFontForLabels(t *testing.T) {
	manifest := theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{{
		ID: "telemetry", Type: theme.LayerTelemetry,
		Bounds:  theme.Rect{X: .05, Y: .05, Width: .5, Height: .25},
		Opacity: 1, ZIndex: 2,
		Options: map[string]any{
			"style":       "cards",
			"font_family": "pangmen",
			"fields":      []string{"Memory_Used"},
		},
	}}}
	widgets := widgetsWithItemPrefix(compileWaveshareTable(t, manifest).widgets, "casescreen-")
	textWidgets := 0
	for _, widget := range widgets {
		if widget.fields["type"] != "8" {
			continue
		}
		textWidgets++
		if widget.fields["text_customFont_path"] != "/usr/share/fonts/Dengb.ttf" || widget.fields["text_family"] != "DengXian" {
			t.Fatalf("非数字字体的遥测文字被意外替换: %+v", widget.fields)
		}
	}
	if textWidgets != 2 {
		t.Fatalf("遥测文字控件数=%d，期望标签和值各一个", textWidgets)
	}
}

func TestCompileWaveshareVerticalCanvasUsesDeviceResolution(t *testing.T) {
	manifest := theme.Manifest{Width: 480, Height: 1920, Layers: []theme.Layer{{
		ID: "clock", Type: theme.LayerClock, Bounds: theme.Rect{X: .1, Y: .05, Width: .8, Height: .08}, Opacity: 1, ZIndex: 1,
	}}}
	compiled, err := compileWaveshareThemeWidgets(requireWaveshareTestBaseTheme(t), manifest)
	if err != nil {
		t.Fatal(err)
	}
	table, err := parseWaveshareThemeWidgetTable(compiled)
	if err != nil {
		t.Fatal(err)
	}
	if table.canvas["canvas_w"] != "480" || table.canvas["canvas_h"] != "1920" {
		t.Fatalf("8.8 英寸竖屏画布尺寸错误: %+v", table.canvas)
	}
	foundBackground := false
	for _, widget := range table.widgets {
		if widget.fields["type"] == "100" {
			foundBackground = true
			if widget.fields["w"] != "480" || widget.fields["h"] != "1920" || widget.fields["maxWidth"] != "480" || widget.fields["maxHeight"] != "1920" {
				t.Fatalf("8.8 英寸背景控件尺寸错误: %+v", widget.fields)
			}
		}
	}
	if !foundBackground {
		t.Fatal("8.8 英寸主题缺少背景控件")
	}
}

func TestCompileWaveshareAudioBarsStyleAndDirectionMatrix(t *testing.T) {
	styles := []string{"equalizer", "wave", "waveform", "mirror", "center", "blocks", "dots", "neon", "outline"}
	directions := []string{"bottom", "top", "center", "horizontal"}
	for _, resolution := range [][2]int{{1024, 600}, {480, 1920}} {
		for _, style := range styles {
			for _, direction := range directions {
				name := strconv.Itoa(resolution[0]) + "x" + strconv.Itoa(resolution[1]) + "/" + style + "/" + direction
				t.Run(name, func(t *testing.T) {
					manifest := theme.Manifest{Width: resolution[0], Height: resolution[1], Layers: []theme.Layer{{
						ID: "bars", Type: theme.LayerAudioBars, Bounds: theme.Rect{X: .1, Y: .1, Width: .8, Height: .25}, Opacity: 1, ZIndex: 2,
						Options: map[string]any{"source": "system", "style": style, "direction": direction, "count": 24, "gap": 3, "track_color": "FFFFFFFF"},
					}}}
					table := compileWaveshareTable(t, manifest)
					bands := widgetsWithItemPrefix(table.widgets, "bars-")
					if len(bands) == 0 {
						t.Fatal("条状频谱没有生成原生控件")
					}
					for _, widget := range bands {
						if widget.fields["type"] != "102" || !strings.HasPrefix(widget.fields["system_data_name"], theme.WaveshareAudioDataPrefix(manifest.Layers[0])) {
							t.Fatalf("条状频谱字段错误: %+v", widget.fields)
						}
						assertWaveshareAudioWidgetHasNoInactiveTrack(t, widget.fields)
						assertWaveshareProtocolFields(t, widget.fields)
						assertWaveshareRotatedRectInCanvas(t, widget.fields, resolution[0], resolution[1])
					}
					first := bands[0].fields
					switch style {
					case "equalizer":
						if first["lineargradient_flag"] != "1" {
							t.Fatalf("均衡器没有使用渐变: %+v", first)
						}
					case "mirror":
						if direction != "horizontal" && direction != "center" && !hasWidgetItemContaining(bands, "reflection") {
							t.Fatal("镜像样式没有编译反射条")
						}
					case "center":
						if !hasWidgetItemContaining(bands, "-a") || !hasWidgetItemContaining(bands, "-b") {
							t.Fatal("中心扩散没有生成双向条")
						}
					case "blocks":
						if first["lineargradient_flag"] != "0" {
							t.Fatalf("分段样式字段错误: %+v", first)
						}
					case "dots":
						if first["back_color"] != "r=0,g=0,b=0,a=0" || first["corner_radius"] == "0" {
							t.Fatalf("点阵样式字段错误: %+v", first)
						}
					case "outline":
						if first["lineargradient_flag"] != "0" {
							t.Fatalf("轮廓样式字段错误: %+v", first)
						}
					}
				})
			}
		}
	}
}

func TestCompileWaveshareAudioRingStyleAndDirectionMatrix(t *testing.T) {
	styles := []string{"radial", "waveform", "donut", "dots", "dashed", "halo", "spokes", "pulse"}
	directions := []string{"outward", "inward", "bidirectional"}
	for _, resolution := range [][2]int{{1024, 600}, {480, 1920}} {
		for _, style := range styles {
			for _, direction := range directions {
				name := strconv.Itoa(resolution[0]) + "x" + strconv.Itoa(resolution[1]) + "/" + style + "/" + direction
				t.Run(name, func(t *testing.T) {
					manifest := theme.Manifest{Width: resolution[0], Height: resolution[1], Layers: []theme.Layer{{
						ID: "ring", Type: theme.LayerAudioRing, Bounds: theme.Rect{X: .1, Y: .35, Width: .8, Height: .35}, Opacity: 1, ZIndex: 3,
						Options: map[string]any{"source": "system", "style": style, "direction": direction, "count": 24, "inner_radius": .52, "thickness": 5, "track_color": "FFFFFFFF"},
					}}}
					table := compileWaveshareTable(t, manifest)
					bands := widgetsWithItemPrefix(table.widgets, "ring-")
					if len(bands) == 0 {
						t.Fatal("环状频谱没有生成原生控件")
					}
					for _, widget := range bands {
						if widget.fields["type"] != "102" || !strings.HasPrefix(widget.fields["system_data_name"], theme.WaveshareAudioDataPrefix(manifest.Layers[0])) {
							t.Fatalf("环状频谱字段错误: %+v", widget.fields)
						}
						assertWaveshareAudioWidgetHasNoInactiveTrack(t, widget.fields)
						assertWaveshareProtocolFields(t, widget.fields)
						assertWaveshareRotatedRectInCanvas(t, widget.fields, resolution[0], resolution[1])
					}
					if direction == "bidirectional" && (!hasWidgetItemContaining(bands, "-out") || !hasWidgetItemContaining(bands, "-in")) {
						t.Fatal("双向环没有生成向内和向外两组控件")
					}
					if style == "halo" && !hasWidgetItemContaining(bands, "-halo-") {
						t.Fatal("双环样式没有生成内环")
					}
					if style == "dashed" && hasWidgetItemContaining(bands, "band-01") {
						t.Fatal("刻度样式没有留出间隔")
					}
				})
			}
		}
	}
}

func TestCompileWavesharePeakHoldUsesResidentGhostKeys(t *testing.T) {
	layer := theme.Layer{
		ID: "bars-peak", Type: theme.LayerAudioBars, Bounds: theme.Rect{X: .1, Y: .2, Width: .8, Height: .2}, Opacity: 1, ZIndex: 2,
		Options: map[string]any{"source": "system", "count": 12, "peak_hold": true, "peak_color": "FFF275"},
	}
	table := compileWaveshareTable(t, theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{layer}})
	foundBase := false
	foundPeak := false
	if got := theme.WaveshareAudioBandKey(layer, 0); got != "A00" {
		t.Fatalf("启用峰值保持后基础频带不应改用长键: %q", got)
	}
	if got := theme.WaveshareAudioPeakKey(layer, 0); got != "P00" {
		t.Fatalf("默认峰值保持应使用短键: %q", got)
	}
	for _, widget := range table.widgets {
		if widget.fields["type"] != "102" {
			continue
		}
		switch widget.fields["system_data_name"] {
		case theme.WaveshareAudioBandKey(layer, 0):
			foundBase = true
		case theme.WaveshareAudioPeakKey(layer, 0):
			foundPeak = true
			if !strings.Contains(widget.fields["itemName"], "-peak") {
				t.Fatalf("峰值尾迹控件标识错误: %+v", widget.fields)
			}
		}
	}
	if !foundBase || !foundPeak {
		t.Fatalf("峰值保持未编译为驻留原生控件: base=%v peak=%v", foundBase, foundPeak)
	}
	base := findWidgetNamed(t, table.widgets, "bars-peak-band-00")
	peak := findWidgetNamed(t, table.widgets, "bars-peak-band-00-peak")
	baseThickness, _ := strconv.Atoi(base.fields["h"])
	peakThickness, _ := strconv.Atoi(peak.fields["h"])
	if baseThickness > 1 && (peakThickness < 1 || peakThickness >= baseThickness) {
		t.Fatalf("峰值保持仍使用整根同宽色柱: base=%+v peak=%+v", base.fields, peak.fields)
	}
	assertWaveshareAudioWidgetHasNoInactiveTrack(t, peak.fields)
}

func TestCompileWaveshareAssignedCustomAudioUsesShortResidentKeys(t *testing.T) {
	manifest := theme.AssignWaveshareAudioPrefixes(theme.Manifest{Width: 1024, Height: 600, Layers: []theme.Layer{{
		ID: "bars-custom", Type: theme.LayerAudioBars,
		Bounds: theme.Rect{X: .1, Y: .2, Width: .8, Height: .2}, Opacity: 1, ZIndex: 2,
		Options: map[string]any{"source": "system", "count": 12, "noise_gate": .025, "bass_boost": 1.22},
	}}})
	layer := manifest.Layers[0]
	key := theme.WaveshareAudioBandKey(layer, 0)
	if len(key) != 3 {
		t.Fatalf("已分配的自定义音频仍使用长键: %q", key)
	}
	table := compileWaveshareTable(t, manifest)
	found := false
	for _, widget := range table.widgets {
		if widget.fields["type"] == "102" && widget.fields["system_data_name"] == key {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("编译后的驻留控件没有使用短键 %q", key)
	}
}

func TestCompileWaveshareExtendedWidgetProtocolAndDynamicKeys(t *testing.T) {
	manifest := theme.Manifest{Width: 480, Height: 1920, Layers: []theme.Layer{
		{ID: "text", Type: theme.LayerText, Bounds: theme.Rect{X: .05, Y: .02, Width: .9, Height: .06}, Opacity: 1, ZIndex: 1, Options: map[string]any{"text": "42%", "data_key": "Cpu_Freq", "shadow": true, "font_size": 120}},
		{ID: "clock", Type: theme.LayerClock, Bounds: theme.Rect{X: .05, Y: .1, Width: .9, Height: .06}, Opacity: 1, ZIndex: 2, Options: map[string]any{"format": "HH:mm", "font_size": 48}},
		{ID: "progress", Type: theme.LayerProgressBar, Bounds: theme.Rect{X: .1, Y: .2, Width: .8, Height: .05}, Opacity: 1, ZIndex: 3, Options: map[string]any{"style": "segmented", "orientation": "vertical", "segments": 4, "show_label": false, "show_value": false}},
		{ID: "gauge", Type: theme.LayerGauge, Bounds: theme.Rect{X: .1, Y: .3, Width: .8, Height: .15}, Opacity: 1, Rotation: 30, ZIndex: 4, Options: map[string]any{"style": "arc", "show_label": false, "show_value": false, "start_angle": -135, "end_angle": 135}},
		{ID: "gradient", Type: theme.LayerGradientGauge, Bounds: theme.Rect{X: .1, Y: .5, Width: .8, Height: .15}, Opacity: 1, Rotation: 20, ZIndex: 5, Options: map[string]any{"style": "ticks", "show_label": false, "show_value": false, "start_angle": -135, "end_angle": 135}},
		{ID: "chart", Type: theme.LayerLineChart, Bounds: theme.Rect{X: .1, Y: .7, Width: .8, Height: .15}, Opacity: 1, ZIndex: 6, Options: map[string]any{"style": "spline", "show_label": true}},
		{ID: "fallback", Type: theme.LayerGauge, Bounds: theme.Rect{X: .1, Y: .87, Width: .8, Height: .06}, Opacity: 1, ZIndex: 7, Options: map[string]any{"data_key": "Cpu_Model", "show_label": false, "show_value": false}},
		{ID: "transparent", Type: theme.LayerProgressBar, Bounds: theme.Rect{X: .1, Y: .95, Width: .3, Height: .03}, Opacity: 0, ZIndex: 8, Options: map[string]any{"show_label": false, "show_value": false}},
	}}
	table := compileWaveshareTable(t, manifest)
	for _, widget := range table.widgets {
		assertWaveshareProtocolFields(t, widget.fields)
	}
	if !hasWidgetNamed(table.widgets, "text-shadow") || !hasWidgetNamed(table.widgets, "text") {
		t.Fatal("文本阴影没有编译为屏端驻留文本")
	}
	clockNames := map[string]bool{}
	for _, widget := range widgetsWithItemPrefix(table.widgets, "clock-") {
		clockNames[widget.fields["system_data_name"]] = true
	}
	if !clockNames["hour"] || !clockNames["minute"] || clockNames["second"] {
		t.Fatalf("HH:mm 时钟动态键错误: %+v", clockNames)
	}
	for index, want := range [][2]string{{"0", "25"}, {"25", "50"}, {"50", "75"}, {"75", "100"}} {
		widget := findWidgetNamed(t, table.widgets, "progress-"+formatTwoDigits(index))
		if widget.fields["system_data_name"] != "Cpu使用率_数值" || widget.fields["system_data_min_value"] != want[0] || widget.fields["system_data_max_value"] != want[1] {
			t.Fatalf("分段进度阈值或动态键错误: %+v", widget.fields)
		}
		assertWaveshareRotatedRectInCanvas(t, widget.fields, manifest.Width, manifest.Height)
	}
	for _, itemName := range []string{"gauge", "gradient"} {
		widget := findWidgetNamed(t, table.widgets, itemName)
		if widget.fields["angleMinValue"] != "225" || widget.fields["angleMaxValue"] != "315" || widget.fields["system_data_name"] != "Cpu使用率_数值" && itemName == "gauge" {
			t.Fatalf("仪表角度或动态键错误: %+v", widget.fields)
		}
	}
	chart := findWidgetNamed(t, table.widgets, "chart")
	if chart.fields["type"] != "106" || chart.fields["area_flag"] != "0" || chart.fields["grid_line_flag"] != "0" || chart.fields["system_data_name"] != "Cpu使用率_数值" {
		t.Fatalf("平滑曲线的原生降级或动态键错误: %+v", chart.fields)
	}
	fallback := findWidgetNamed(t, table.widgets, "fallback")
	if fallback.fields["type"] != "8" || fallback.fields["system_data_flag"] != "1" || fallback.fields["system_data_name"] != "Cpu型号" {
		t.Fatalf("非数值键应降级为可更新文本: %+v", fallback.fields)
	}
	if hasWidgetNamed(table.widgets, "transparent") {
		t.Fatal("透明度为 0 的图层仍被编译进屏端主题")
	}
}

func TestCompileWaveshareDeviceMicrophoneRingMigratesToHostDirectionAndGradient(t *testing.T) {
	manifest := theme.Manifest{Width: 480, Height: 1920, Layers: []theme.Layer{{
		ID: "ring", Type: theme.LayerAudioRing, Bounds: theme.Rect{X: 0, Y: .6, Width: 1, Height: .25}, Opacity: 1, ZIndex: 2,
		Options: map[string]any{"source": "device_mic", "direction": "bidirectional", "gradient_start": "5500FF", "gradient_end": "00FFFF"},
	}}}
	table := compileWaveshareTable(t, manifest)
	widget := findWidgetNamed(t, table.widgets, "ring-band-00-out")
	if widget.fields["type"] != "102" || widget.fields["system_data_name"] != theme.WaveshareAudioBandKey(manifest.Layers[0], 0) {
		t.Fatalf("旧环状麦克风图层没有迁移为主机频带: %+v", widget.fields)
	}
	if widget.fields["front_color"] == widget.fields["lineargradient_color"] {
		t.Fatalf("迁移后环状频谱丢失渐变: %+v", widget.fields)
	}
	for _, compiled := range table.widgets {
		if compiled.fields["type"] == "107" || compiled.fields["type"] == "108" {
			t.Fatalf("迁移后仍有屏端麦克风控件: %+v", compiled.fields)
		}
	}
	assertWaveshareProtocolFields(t, widget.fields)
}

func TestCompileWaveshareProgressGaugeAndChartStyleMatrices(t *testing.T) {
	resolutions := [][2]int{{1024, 600}, {480, 1920}}
	for _, resolution := range resolutions {
		resolutionName := strconv.Itoa(resolution[0]) + "x" + strconv.Itoa(resolution[1])
		for _, style := range []string{"rounded", "square", "segmented", "dots", "neon", "outline"} {
			for _, orientation := range []string{"horizontal", "vertical", "reverse"} {
				t.Run("progress/"+resolutionName+"/"+style+"/"+orientation, func(t *testing.T) {
					manifest := theme.Manifest{Width: resolution[0], Height: resolution[1], Layers: []theme.Layer{{
						ID: "progress", Type: theme.LayerProgressBar, Bounds: theme.Rect{X: .1, Y: .1, Width: .8, Height: .12}, Opacity: 1, ZIndex: 2,
						Options: map[string]any{"style": style, "orientation": orientation, "show_label": false, "show_value": false},
					}}}
					widgets := widgetsWithItemPrefix(compileWaveshareTable(t, manifest).widgets, "progress")
					if len(widgets) == 0 {
						t.Fatal("进度条没有生成原生控件")
					}
					for _, widget := range widgets {
						if widget.fields["type"] != "102" || widget.fields["system_data_name"] != "Cpu使用率_数值" {
							t.Fatalf("进度条动态键错误: %+v", widget.fields)
						}
						assertWaveshareProtocolFields(t, widget.fields)
						assertWaveshareRotatedRectInCanvas(t, widget.fields, resolution[0], resolution[1])
					}
				})
			}
		}
		for _, gradient := range []bool{false, true} {
			for _, style := range []string{"arc", "dial", "semicircle", "ring", "ticks", "minimal"} {
				prefix := "gauge"
				layerType := theme.LayerGauge
				if gradient {
					prefix = "gradient"
					layerType = theme.LayerGradientGauge
				}
				t.Run(prefix+"/"+resolutionName+"/"+style, func(t *testing.T) {
					manifest := theme.Manifest{Width: resolution[0], Height: resolution[1], Layers: []theme.Layer{{
						ID: prefix, Type: layerType, Bounds: theme.Rect{X: .1, Y: .32, Width: .8, Height: .25}, Opacity: 1, ZIndex: 3,
						Options: map[string]any{"style": style, "show_label": false, "show_value": false},
					}}}
					widget := findWidgetNamed(t, compileWaveshareTable(t, manifest).widgets, prefix)
					wantType := "109"
					if !gradient && (style == "ring" || style == "minimal") {
						wantType = "101"
					}
					if widget.fields["type"] != wantType {
						t.Fatalf("仪表样式 %s 原生类型=%s，期望 %s", style, widget.fields["type"], wantType)
					}
					assertWaveshareProtocolFields(t, widget.fields)
					assertWaveshareWidgetInCanvas(t, widget.fields, resolution[0], resolution[1])
				})
			}
		}
		for _, testCase := range []struct {
			style    string
			areaFlag string
			gridFlag string
		}{
			{style: "line", areaFlag: "0", gridFlag: "1"},
			{style: "area", areaFlag: "1", gridFlag: "1"},
			{style: "step", areaFlag: "0", gridFlag: "1"},
			{style: "spline", areaFlag: "0", gridFlag: "0"},
			{style: "dots", areaFlag: "0", gridFlag: "0"},
			{style: "neon", areaFlag: "1", gridFlag: "0"},
		} {
			t.Run("chart/"+resolutionName+"/"+testCase.style, func(t *testing.T) {
				manifest := theme.Manifest{Width: resolution[0], Height: resolution[1], Layers: []theme.Layer{{
					ID: "chart", Type: theme.LayerLineChart, Bounds: theme.Rect{X: .1, Y: .68, Width: .8, Height: .2}, Opacity: 1, ZIndex: 4,
					Options: map[string]any{"style": testCase.style, "show_label": false},
				}}}
				widget := findWidgetNamed(t, compileWaveshareTable(t, manifest).widgets, "chart")
				if widget.fields["type"] != "106" || widget.fields["area_flag"] != testCase.areaFlag || widget.fields["grid_line_flag"] != testCase.gridFlag {
					t.Fatalf("曲线样式 %s 的原生降级字段错误: %+v", testCase.style, widget.fields)
				}
				assertWaveshareProtocolFields(t, widget.fields)
				assertWaveshareWidgetInCanvas(t, widget.fields, resolution[0], resolution[1])
			})
		}
	}
}

func compileWaveshareTable(t *testing.T, manifest theme.Manifest) waveshareThemeWidgetTable {
	t.Helper()
	compiled, err := compileWaveshareThemeWidgets(requireWaveshareTestBaseTheme(t), manifest)
	if err != nil {
		t.Fatal(err)
	}
	table, err := parseWaveshareThemeWidgetTable(compiled)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func widgetsWithItemPrefix(widgets []waveshareThemeWidget, prefix string) []waveshareThemeWidget {
	result := make([]waveshareThemeWidget, 0)
	for _, widget := range widgets {
		if strings.HasPrefix(widget.fields["itemName"], prefix) {
			result = append(result, widget)
		}
	}
	return result
}

func hasWidgetItemContaining(widgets []waveshareThemeWidget, value string) bool {
	for _, widget := range widgets {
		if strings.Contains(widget.fields["itemName"], value) {
			return true
		}
	}
	return false
}

func hasWidgetNamed(widgets []waveshareThemeWidget, itemName string) bool {
	for _, widget := range widgets {
		if widget.fields["itemName"] == itemName {
			return true
		}
	}
	return false
}

func findWidgetNamed(t *testing.T, widgets []waveshareThemeWidget, itemName string) waveshareThemeWidget {
	t.Helper()
	for _, widget := range widgets {
		if widget.fields["itemName"] == itemName {
			return widget
		}
	}
	t.Fatalf("未找到控件 %q", itemName)
	return waveshareThemeWidget{}
}

func formatTwoDigits(value int) string {
	if value < 10 {
		return "0" + strconv.Itoa(value)
	}
	return strconv.Itoa(value)
}

func assertWaveshareProtocolFields(t *testing.T, fields map[string]string) {
	t.Helper()
	required := map[string][]string{
		"8":   {"id", "x", "y", "z", "rotate", "scale", "text_str", "text_size", "front_color", "system_data_flag", "system_data_name"},
		"100": {"id", "x", "y", "w", "h", "z", "rotate", "scale"},
		"101": {"id", "x", "y", "z", "rotate", "scale", "radius", "margin", "front_color", "back_color", "system_data_flag", "system_data_name", "system_data_min_value", "system_data_max_value"},
		"102": {"id", "x", "y", "w", "h", "z", "rotate", "scale", "front_color", "back_color", "corner_radius", "border_width", "border_color", "lineargradient_flag", "lineargradient_color", "system_data_flag", "system_data_name", "system_data_min_value", "system_data_max_value"},
		"106": {"id", "x", "y", "w", "h", "z", "rotate", "scale", "series_color", "area_flag", "area_color", "grid_line_flag", "grid_line_color", "system_data_flag", "system_data_name", "system_data_max_value"},
		"107": {"id", "x", "y", "z", "rotate", "scale", "radius", "screen_w", "screen_h", "SoundColumnHeight", "SoundColumnWidth", "DisplayDirection", "Clockwise", "system_data_flag", "system_data_name"},
		"108": {"id", "x", "y", "z", "rotate", "scale", "rectWidth", "rectWidthCount", "rectWidthInterval", "rectHeight", "rectHeightCount", "system_data_flag", "system_data_name"},
		"109": {"id", "x", "y", "z", "rotate", "scale", "radius", "arcRadius", "arcCircularInterval", "angleMinValue", "angleMaxValue", "gradientColor1", "gradientColor2", "gradientColor3", "system_data_flag", "system_data_name", "system_data_min_value", "system_data_max_value"},
	}
	for _, key := range required[fields["type"]] {
		if _, ok := fields[key]; !ok {
			t.Fatalf("控件类型 %s 缺少原厂字段 %q: %+v", fields["type"], key, fields)
		}
	}
	if fields["system_data_flag"] == "1" && strings.TrimSpace(fields["system_data_name"]) == "" {
		t.Fatalf("动态控件缺少 system_data_name: %+v", fields)
	}
}

func assertWaveshareAudioWidgetHasNoInactiveTrack(t *testing.T, fields map[string]string) {
	t.Helper()
	const transparent = "r=0,g=0,b=0,a=0"
	if fields["back_color"] != transparent || fields["border_width"] != "0" || fields["border_color"] != transparent {
		t.Fatalf("频谱控件仍会绘制与能量无关的整段轨道或边框: %+v", fields)
	}
}

func assertWaveshareRotatedRectInCanvas(t *testing.T, fields map[string]string, canvasWidth, canvasHeight int) {
	t.Helper()
	parse := func(key string) float64 {
		value, err := strconv.ParseFloat(fields[key], 64)
		if err != nil {
			t.Fatalf("控件字段 %s=%q 不是数字: %+v", key, fields[key], fields)
		}
		return value
	}
	x, y, width, height := parse("x"), parse("y"), parse("w"), parse("h")
	radians := parse("rotate") * math.Pi / 180
	cosine, sine := math.Cos(radians), math.Sin(radians)
	minimumX, maximumX, minimumY, maximumY := x, x, y, y
	for _, point := range [][2]float64{{0, 0}, {width, 0}, {0, height}, {width, height}} {
		rotatedX := x + point[0]*cosine - point[1]*sine
		rotatedY := y + point[0]*sine + point[1]*cosine
		minimumX, maximumX = math.Min(minimumX, rotatedX), math.Max(maximumX, rotatedX)
		minimumY, maximumY = math.Min(minimumY, rotatedY), math.Max(maximumY, rotatedY)
	}
	const tolerance = 1.01
	if minimumX < -tolerance || minimumY < -tolerance || maximumX > float64(canvasWidth)+tolerance || maximumY > float64(canvasHeight)+tolerance {
		t.Fatalf("旋转后控件超出 %dx%d 画布: x=%.2f..%.2f y=%.2f..%.2f fields=%+v", canvasWidth, canvasHeight, minimumX, maximumX, minimumY, maximumY, fields)
	}
}

func assertWaveshareWidgetInCanvas(t *testing.T, fields map[string]string, canvasWidth, canvasHeight int) {
	t.Helper()
	parse := func(key string) int {
		value, err := strconv.Atoi(fields[key])
		if err != nil {
			t.Fatalf("控件字段 %s=%q 不是整数: %+v", key, fields[key], fields)
		}
		return value
	}
	x, y := parse("x"), parse("y")
	if x < 0 || x >= canvasWidth || y < 0 || y >= canvasHeight {
		t.Fatalf("控件锚点超出 %dx%d 画布: %+v", canvasWidth, canvasHeight, fields)
	}
	switch fields["type"] {
	case "100", "106":
		if x+parse("w") > canvasWidth || y+parse("h") > canvasHeight {
			t.Fatalf("控件尺寸超出 %dx%d 画布: %+v", canvasWidth, canvasHeight, fields)
		}
	case "101", "109":
		if fields["rotate"] == "0" {
			diameter := parse("radius") * 2
			if x+diameter > canvasWidth || y+diameter > canvasHeight {
				t.Fatalf("圆形控件超出 %dx%d 画布: %+v", canvasWidth, canvasHeight, fields)
			}
		}
	case "102":
		assertWaveshareRotatedRectInCanvas(t, fields, canvasWidth, canvasHeight)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func themeManifestWithLayers(types ...string) theme.Manifest {
	manifest := theme.Manifest{}
	for index, layerType := range types {
		manifest.Layers = append(manifest.Layers, theme.Layer{ID: string(rune('a' + index)), Type: theme.LayerType(layerType)})
	}
	return manifest
}

func writeThemeStringMap(output *bytes.Buffer, fields map[string]string) {
	writeThemeUint32(output, uint32(len(fields)))
	for key, value := range fields {
		writeThemeQString(output, key)
		writeThemeQString(output, value)
	}
}

func writeThemeQString(output *bytes.Buffer, value string) {
	units := utf16.Encode([]rune(value))
	writeThemeUint32(output, uint32(len(units)*2))
	for _, unit := range units {
		var data [2]byte
		binary.BigEndian.PutUint16(data[:], unit)
		output.Write(data[:])
	}
}

func writeThemeUint32(output *bytes.Buffer, value uint32) {
	var data [4]byte
	binary.BigEndian.PutUint32(data[:], value)
	output.Write(data[:])
}
