package api

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	stddraw "image/draw"
	"math"
	"sort"
	"strconv"
	"strings"

	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/math/f64"
)

type wavesharePixelRect struct {
	x      int
	y      int
	width  int
	height int
}

type waveshareTelemetrySpec struct {
	key         string
	label       string
	systemName  string
	placeholder string
	color       string
}

const waveshareHostSpectrumBands = 48

var waveshareTelemetrySpecs = []waveshareTelemetrySpec{
	{key: "Cpu_Freq", label: "CPU", systemName: "Cpu使用率", placeholder: "42%", color: "r=76,g=221,b=255,a=255"},
	{key: "Gpu_Load", label: "GPU", systemName: "显卡利用率", placeholder: "63%", color: "r=255,g=186,b=94,a=255"},
	{key: "Memory_Load", label: "RAM", systemName: "内存利用率", placeholder: "58%", color: "r=140,g=241,b=170,a=255"},
	{key: "GPU_Temp", label: "GPU ℃", systemName: "显卡温度", placeholder: "45℃", color: "r=255,g=125,b=210,a=255"},
	{key: "GPU_Hotspot_Temp", label: "GPU 热点", systemName: "显卡热点温度", placeholder: "54℃", color: "r=255,g=92,b=104,a=255"},
	{key: "GPU_Memory_Temp", label: "显存 ℃", systemName: "显存温度", placeholder: "56℃", color: "r=255,g=186,b=94,a=255"},
	{key: "Memory_Used", label: "已用内存", systemName: "已用内存", placeholder: "18.6GB", color: "r=118,g=220,b=255,a=255"},
	{key: "Memory_Available", label: "可用内存", systemName: "可用内存", placeholder: "13.4GB", color: "r=140,g=241,b=170,a=255"},
	{key: "time", label: "时间", systemName: "时间", placeholder: "15:04:05", color: "r=111,g=231,b=240,a=255"},
	{key: "date", label: "日期", systemName: "日期", placeholder: "2026-08-26", color: "r=245,g=251,b=255,a=255"},
	{key: "week", label: "星期", systemName: "星期", placeholder: "周三", color: "r=245,g=251,b=255,a=255"},
	{key: "Cpu_Model", label: "CPU 型号", systemName: "Cpu型号", placeholder: "AMD Ryzen", color: "r=245,g=251,b=255,a=255"},
	{key: "Gpu_Model", label: "GPU 型号", systemName: "显卡型号", placeholder: "GeForce RTX", color: "r=245,g=251,b=255,a=255"},
	{key: "vol", label: "系统音量", systemName: "vol", placeholder: "62", color: "r=255,g=242,b=117,a=255"},
	{key: "cur_dB", label: "当前分贝", systemName: "cur_dB", placeholder: "48", color: "r=76,g=224,b=255,a=255"},
	{key: "avg_dB", label: "平均分贝", systemName: "avg_dB", placeholder: "41", color: "r=139,g=92,b=246,a=255"},
	{key: "max_dB", label: "峰值分贝", systemName: "max_dB", placeholder: "73", color: "r=255,g=92,b=104,a=255"},
}

// compileWaveshareThemeWidgets 把统一编辑器图层编译为 PCMonitorLinux 原生控件。
// 原厂主题只作为控件模板，不再沿用它的固定坐标、尺寸和层级。
func compileWaveshareThemeWidgets(data []byte, manifest theme.Manifest) ([]byte, error) {
	var err error
	data, err = rewriteWaveshareThemeCanvas(data, manifest.Width, manifest.Height)
	if err != nil {
		return nil, err
	}
	table, err := parseWaveshareThemeWidgetTable(data)
	if err != nil {
		return nil, err
	}
	background, ok := findWaveshareWidget(table.widgets, func(fields map[string]string) bool { return fields["type"] == "100" })
	if !ok {
		return nil, errors.New("微雪原始主题缺少背景视频控件")
	}
	textTemplate, ok := findWaveshareWidget(table.widgets, func(fields map[string]string) bool {
		return fields["type"] == "8" && strings.Contains(fields["text_customFont_path"], "庞门")
	})
	if !ok {
		textTemplate, ok = findWaveshareWidget(table.widgets, func(fields map[string]string) bool { return fields["type"] == "8" })
	}
	if !ok {
		return nil, errors.New("微雪原始主题缺少文本控件模板")
	}
	gaugeTemplate, hasGaugeTemplate := findWaveshareWidget(table.widgets, func(fields map[string]string) bool { return fields["type"] == "101" })
	progressTemplate, hasProgressTemplate := findWaveshareWidget(table.widgets, func(fields map[string]string) bool { return fields["type"] == "102" })

	compiled := make([]waveshareThemeWidget, 0, 20)
	background = cloneWaveshareThemeWidget(background)
	background.fields["x"] = "0"
	background.fields["y"] = "0"
	background.fields["w"] = strconv.Itoa(manifest.Width)
	background.fields["h"] = strconv.Itoa(manifest.Height)
	background.fields["maxWidth"] = strconv.Itoa(manifest.Width)
	background.fields["maxHeight"] = strconv.Itoa(manifest.Height)
	background.fields["z"] = "0"
	background.fields["rotate"] = "0"
	background.fields["scale"] = "1"
	background.fields["lock"] = "1"
	compiled = append(compiled, background)

	layers := append([]theme.Layer(nil), manifest.Layers...)
	sort.SliceStable(layers, func(left, right int) bool { return layers[left].ZIndex < layers[right].ZIndex })
	nextID := 1000
	for _, layer := range layers {
		if !theme.LayerVisible(layer) || layer.Opacity <= 0 {
			continue
		}
		// 编辑器把一个组件的全部子控件围绕图层中心旋转。PCMonitorLinux 则把
		// 每个原生控件围绕自己的左上角旋转。先以 0° 生成子控件，再统一旋转
		// 每个锚点并叠加角度，二者的几何变换才完全等价。
		compiledLayer := layer
		compiledLayer.Rotation = 0
		firstWidget := len(compiled)
		switch compiledLayer.Type {
		case theme.LayerTelemetry:
			widgets := compileWaveshareTelemetryLayer(textTemplate, gaugeTemplate, progressTemplate, hasGaugeTemplate, hasProgressTemplate, compiledLayer, manifest.Width, manifest.Height, &nextID)
			compiled = append(compiled, widgets...)
		case theme.LayerAudioBars:
			if hasProgressTemplate {
				compiled = append(compiled, compileWaveshareHostAudioBars(progressTemplate, compiledLayer, manifest.Width, manifest.Height, &nextID)...)
			}
		case theme.LayerAudioRing:
			if hasProgressTemplate {
				compiled = append(compiled, compileWaveshareHostAudioRing(progressTemplate, compiledLayer, manifest.Width, manifest.Height, &nextID)...)
			}
		case theme.LayerLyrics:
			widgets := compileWaveshareLyricsLayer(textTemplate, progressTemplate, hasProgressTemplate, compiledLayer, manifest.Width, manifest.Height, &nextID)
			compiled = append(compiled, widgets...)
		case theme.LayerText:
			compiled = append(compiled, compileWaveshareTextLayer(textTemplate, compiledLayer, manifest.Width, manifest.Height, &nextID)...)
		case theme.LayerClock:
			compiled = append(compiled, compileWaveshareClockLayer(textTemplate, compiledLayer, manifest.Width, manifest.Height, &nextID)...)
		case theme.LayerProgressBar:
			if hasProgressTemplate {
				compiled = append(compiled, compileWaveshareProgressLayer(textTemplate, progressTemplate, compiledLayer, manifest.Width, manifest.Height, &nextID)...)
			}
		case theme.LayerGauge:
			if hasGaugeTemplate {
				compiled = append(compiled, compileWaveshareGaugeLayer(textTemplate, gaugeTemplate, compiledLayer, manifest.Width, manifest.Height, &nextID)...)
			}
		case theme.LayerGradientGauge:
			compiled = append(compiled, compileWaveshareGradientGaugeLayer(textTemplate, compiledLayer, manifest.Width, manifest.Height, &nextID)...)
		case theme.LayerLineChart:
			compiled = append(compiled, compileWaveshareLineChartLayer(textTemplate, compiledLayer, manifest.Width, manifest.Height, &nextID)...)
		case theme.LayerShape:
			// 形状没有运行时数据，直接绘入 TF 驻留背景，避免额外 USB 传输。
		}
		if compiledLayer.Type != theme.LayerLyrics {
			// 旧主题未显式声明字重时保留单控件结构；新编辑器会始终写入
			// font_weight，所以用户选择的 medium/bold 仍会得到可见形态差异。
			if _, explicitWeight := compiledLayer.Options["font_weight"]; explicitWeight {
				weight := waveshareFontWeight(waveshareOptionString(compiledLayer.Options, "font_weight", "medium"))
				compiled = append(compiled, waveshareWeightTextCopies(compiled[firstWidget:], weight, &nextID)...)
			}
		}
		waveshareRotateLayerWidgets(compiled[firstWidget:], layer, manifest.Width, manifest.Height)
	}
	return rewriteWaveshareThemeWidgets(data, compiled)
}

// waveshareRotateLayerWidgets 把围绕图层中心的编辑器变换转换为屏端每个原生
// 控件的左上角锚点与自身旋转角。原厂主题实测的 QGraphicsItem 原点是左上角。
func waveshareRotateLayerWidgets(widgets []waveshareThemeWidget, layer theme.Layer, width, height int) {
	if len(widgets) == 0 || math.Abs(layer.Rotation) < 0.0001 {
		return
	}
	bounds := waveshareLayerPixelRect(layer, width, height)
	centerX := float64(bounds.x) + float64(bounds.width)/2
	centerY := float64(bounds.y) + float64(bounds.height)/2
	radians := layer.Rotation * math.Pi / 180
	cosine, sine := math.Cos(radians), math.Sin(radians)
	for index := range widgets {
		x, xErr := strconv.ParseFloat(widgets[index].fields["x"], 64)
		y, yErr := strconv.ParseFloat(widgets[index].fields["y"], 64)
		if xErr == nil && yErr == nil {
			deltaX, deltaY := x-centerX, y-centerY
			widgets[index].fields["x"] = strconv.Itoa(int(math.Round(centerX + cosine*deltaX - sine*deltaY)))
			widgets[index].fields["y"] = strconv.Itoa(int(math.Round(centerY + sine*deltaX + cosine*deltaY)))
		}
		rotation, _ := strconv.ParseFloat(widgets[index].fields["rotate"], 64)
		widgets[index].fields["rotate"] = strconv.Itoa(int(math.Round(rotation + layer.Rotation)))
	}
}

func compileWaveshareTelemetryLayer(textTemplate, gaugeTemplate, progressTemplate waveshareThemeWidget, hasGaugeTemplate, hasProgressTemplate bool, layer theme.Layer, width, height int, nextID *int) []waveshareThemeWidget {
	bounds := waveshareLayerPixelRect(layer, width, height)
	selected := make(map[string]bool)
	for _, field := range waveshareTelemetryFields(layer.Options) {
		selected[field] = true
	}
	metrics := make([]waveshareTelemetrySpec, 0, len(selected))
	for _, spec := range waveshareTelemetrySpecs {
		if selected[spec.key] {
			metrics = append(metrics, spec)
		}
	}
	if len(metrics) == 0 {
		metrics = append(metrics, waveshareTelemetrySpecs[0], waveshareTelemetrySpecs[2])
	}
	style := strings.ToLower(waveshareOptionString(layer.Options, "style", "cards"))
	z := max(1, layer.ZIndex*10)
	rotation := strconv.Itoa(int(math.Round(layer.Rotation)))
	widgets := make([]waveshareThemeWidget, 0, len(metrics)*3)
	fontSize := max(8, min(160, waveshareOptionInt(layer.Options, "font_size", 22)))
	labelColor := waveshareNativeColor(waveshareOptionString(layer.Options, "label_color", "A8C4CA"), layer.Opacity)
	valueColor := waveshareNativeColor(waveshareOptionString(layer.Options, "value_color", "F5FBFF"), layer.Opacity)
	accentColor := waveshareNativeColor(waveshareOptionString(layer.Options, "accent_color", "4CE0FF"), layer.Opacity)
	showLabel := waveshareOptionBool(layer.Options, "show_label", true)
	showValue := waveshareOptionBool(layer.Options, "show_value", true)
	showUnit := waveshareOptionBool(layer.Options, "show_unit", true)
	labelOptions := waveshareTelemetryLabelOptions(layer.Options)

	switch style {
	case "gauge":
		rowHeight := max(1, bounds.height/max(1, len(metrics)))
		for index, spec := range metrics {
			systemName := waveshareTelemetrySystemName(spec, showUnit)
			numericSystemName := waveshareNumericSystemName(spec)
			y := bounds.y + index*rowHeight
			rowBottom := bounds.y + bounds.height
			currentHeight := min(rowHeight, max(1, rowBottom-y))
			rowBounds := wavesharePixelRect{x: bounds.x, y: y, width: bounds.width, height: currentHeight}
			labelWidth := 0
			if showLabel {
				labelWidth = min(max(54, bounds.width/4), 150)
			}
			if hasProgressTemplate && waveshareTelemetryNumeric(spec.key) {
				barX := bounds.x + labelWidth + 5
				barWidth := max(8, bounds.width-labelWidth-12)
				barHeight := max(1, min(currentHeight/3, 18))
				widgets = append(widgets, waveshareProgressWidget(progressTemplate, nextID, layer.ID+"-"+spec.key, numericSystemName, barX, y+(currentHeight-barHeight)/2, barWidth, barHeight, 0, z+index*4+1, accentColor, waveshareNativeColor("183740", layer.Opacity*.75), 4, 0, 100))
			}
			labelText, valueText := "", ""
			if showLabel {
				labelText = spec.label
			}
			if showValue {
				valueText = waveshareTelemetryPlaceholder(spec, showUnit)
			}
			textBounds := waveshareInsetPixelRect(rowBounds, min(5, max(0, rowBounds.width/24)), 0, min(5, max(0, rowBounds.width/24)), 0)
			preferred := max(8, min(fontSize, currentHeight/2))
			minimumPairWidth := waveshareEstimatedRenderedTextWidth(labelText, waveshareMinimumTextSize) + waveshareEstimatedRenderedTextWidth(valueText, waveshareMinimumTextSize) + 4
			labelLayout, labelOK, valueLayout, valueOK := waveshareFitTextPair(textBounds, labelText, valueText, preferred, preferred, "left", "right", textBounds.width < minimumPairWidth)
			if labelOK {
				widgets = append(widgets, waveshareStyledTextWidget(textTemplate, nextID, labelLayout.text, "", false, labelLayout.x, labelLayout.y, labelLayout.size, labelColor, z+index*4, rotation, labelOptions))
			}
			if valueOK {
				widgets = append(widgets, waveshareStyledTextWidget(textTemplate, nextID, valueLayout.text, systemName, true, valueLayout.x, valueLayout.y, valueLayout.size, valueColor, z+index*4+2, rotation, layer.Options))
			}
		}
	case "dials":
		columns := min(4, max(1, len(metrics)))
		rows := (len(metrics) + columns - 1) / columns
		cellWidth := max(1, bounds.width/columns)
		cellHeight := max(1, bounds.height/max(1, rows))
		for index, spec := range metrics {
			systemName := waveshareTelemetrySystemName(spec, showUnit)
			numericSystemName := waveshareNumericSystemName(spec)
			column, row := index%columns, index/columns
			x, y := bounds.x+column*cellWidth, bounds.y+row*cellHeight
			if hasGaugeTemplate && waveshareTelemetryNumeric(spec.key) {
				diameter := max(2, min(cellWidth, cellHeight)-8)
				widgets = append(widgets, waveshareGaugeWidget(gaugeTemplate, nextID, layer.ID+"-"+spec.key, numericSystemName, x+(cellWidth-diameter)/2, y+(cellHeight-diameter)/2, diameter/2, max(2, diameter/12), z+index*4, accentColor, waveshareNativeColor("183740", layer.Opacity*.7), 0, 100))
			}
			labelText, valueText := "", ""
			if showLabel {
				labelText = spec.label
			}
			if showValue {
				valueText = waveshareTelemetryPlaceholder(spec, showUnit)
			}
			cellBounds := waveshareInsetPixelRect(wavesharePixelRect{x: x, y: y, width: cellWidth, height: cellHeight}, min(5, max(0, cellWidth/20)), 0, min(5, max(0, cellWidth/20)), 0)
			labelPreferred := max(8, min(fontSize-4, cellHeight/7))
			valuePreferred := max(8, min(fontSize, cellHeight/4))
			labelLayout, labelOK, valueLayout, valueOK := waveshareFitTextPair(cellBounds, labelText, valueText, labelPreferred, valuePreferred, "left", "right", true)
			if labelOK {
				widgets = append(widgets, waveshareStyledTextWidget(textTemplate, nextID, labelLayout.text, "", false, labelLayout.x, labelLayout.y, labelLayout.size, labelColor, z+index*4+1, rotation, labelOptions))
			}
			if valueOK {
				widgets = append(widgets, waveshareStyledTextWidget(textTemplate, nextID, valueLayout.text, systemName, true, valueLayout.x, valueLayout.y, valueLayout.size, valueColor, z+index*4+2, rotation, layer.Options))
			}
		}
	default:
		columns := len(metrics)
		switch style {
		case "list", "minimal":
			columns = 1
		case "tiles":
			columns = min(3, max(1, len(metrics)))
		case "digital":
			columns = min(4, max(1, len(metrics)))
		case "compact":
			columns = min(6, max(1, len(metrics)))
		default: // cards
			columns = min(4, max(1, len(metrics)))
		}
		rows := (len(metrics) + columns - 1) / columns
		cellWidth := max(1, bounds.width/columns)
		cellHeight := max(1, bounds.height/max(1, rows))
		for index, spec := range metrics {
			systemName := waveshareTelemetrySystemName(spec, showUnit)
			column, row := index%columns, index/columns
			cellBounds := wavesharePixelRect{x: bounds.x + column*cellWidth, y: bounds.y + row*cellHeight, width: cellWidth, height: cellHeight}
			contentBounds := waveshareInsetPixelRect(cellBounds, min(6, max(0, cellWidth/20)), 0, min(6, max(0, cellWidth/20)), 0)
			drawLabel := showLabel && style != "minimal"
			labelPreferredSize := max(9, min(fontSize-5, cellHeight/4))
			valueText := waveshareTelemetryPlaceholder(spec, showUnit)
			valuePreferredSize := max(12, min(fontSize, cellHeight/2))
			if style == "digital" {
				valuePreferredSize = max(valuePreferredSize, min(96, cellHeight*2/3))
			}
			labelText := ""
			if drawLabel {
				labelText = spec.label
			}
			if !showValue {
				valueText = ""
			}
			labelLayout, labelOK, valueLayout, valueOK := waveshareFitTextPair(contentBounds, labelText, valueText, labelPreferredSize, valuePreferredSize, "left", "left", drawLabel && showValue)
			if labelOK {
				widgets = append(widgets, waveshareStyledTextWidget(textTemplate, nextID, labelLayout.text, "", false, labelLayout.x, labelLayout.y, labelLayout.size, labelColor, z+index*3, rotation, labelOptions))
			}
			if valueOK {
				widgets = append(widgets, waveshareStyledTextWidget(textTemplate, nextID, valueLayout.text, systemName, true, valueLayout.x, valueLayout.y, valueLayout.size, valueColor, z+index*3+1, rotation, layer.Options))
			}
		}
	}
	return widgets
}

// waveshareTelemetryLabelOptions 避免数字字体缺少中文字形时把遥测标签显示为方框。
// 只替换 digital 遥测标签的字体，动态数值仍继续使用用户选择的数字字体。
func waveshareTelemetryLabelOptions(options map[string]any) map[string]any {
	if strings.ToLower(waveshareOptionString(options, "font_family", "source-han")) != "digital" {
		return options
	}
	labelOptions := make(map[string]any, len(options)+1)
	for key, value := range options {
		labelOptions[key] = value
	}
	labelOptions["font_family"] = "source-han"
	return labelOptions
}

func waveshareTelemetryNumeric(key string) bool {
	switch key {
	case "Cpu_Freq", "Gpu_Load", "Memory_Load", "GPU_Temp", "GPU_Hotspot_Temp", "GPU_Memory_Temp", "Memory_Used", "Memory_Available", "vol", "cur_dB", "avg_dB", "max_dB":
		return true
	default:
		return false
	}
}

func waveshareTelemetrySystemName(spec waveshareTelemetrySpec, showUnit bool) string {
	if showUnit {
		return spec.systemName
	}
	switch spec.key {
	case "Cpu_Freq", "Gpu_Load", "Memory_Load", "GPU_Temp", "GPU_Hotspot_Temp", "GPU_Memory_Temp", "Memory_Used", "Memory_Available":
		return spec.systemName + "_数值"
	default:
		return spec.systemName
	}
}

// waveshareNumericSystemName 为进度、仪表和曲线选择不含 %、℃、GB 的别名，
// 避免不同固件版本对带单位字符串的数值解析差异。
func waveshareNumericSystemName(spec waveshareTelemetrySpec) string {
	switch spec.key {
	case "Cpu_Freq", "Gpu_Load", "Memory_Load", "GPU_Temp", "GPU_Hotspot_Temp", "GPU_Memory_Temp", "Memory_Used", "Memory_Available":
		return spec.systemName + "_数值"
	default:
		return spec.systemName
	}
}

func waveshareTelemetryPlaceholder(spec waveshareTelemetrySpec, showUnit bool) string {
	if showUnit {
		return spec.placeholder
	}
	value := strings.TrimSpace(spec.placeholder)
	for _, suffix := range []string{"GB", "℃", "%"} {
		value = strings.TrimSuffix(value, suffix)
	}
	return value
}

func compileWaveshareAudioLayer(template waveshareThemeWidget, layer theme.Layer, width, height int, nextID *int) waveshareThemeWidget {
	bounds := waveshareLayerPixelRect(layer, width, height)
	widget := cloneWaveshareThemeWidget(template)
	widget.fields["id"] = strconv.Itoa(*nextID)
	*nextID++
	widget.fields["itemName"] = layer.ID
	widget.fields["x"] = strconv.Itoa(bounds.x)
	widget.fields["y"] = strconv.Itoa(bounds.y)
	widget.fields["z"] = strconv.Itoa(max(1, layer.ZIndex*10))
	widget.fields["rotate"] = strconv.Itoa(int(math.Round(layer.Rotation)))
	count := max(1, min(min(48, waveshareOptionInt(layer.Options, "count", theme.DefaultAudioBarsCount)), bounds.width))
	barWidth, gap := wavesharePackedLinearLayout(bounds.width, count, max(1, min(8, bounds.width/max(1, count*3))))
	widget.fields["rectWidthCount"] = strconv.Itoa(count)
	widget.fields["rectWidth"] = strconv.Itoa(barWidth)
	widget.fields["rectWidthInterval"] = strconv.Itoa(gap)
	widget.fields["rectHeight"] = strconv.Itoa(max(8, bounds.height))
	widget.fields["rectHeightCount"] = "1"
	widget.fields["rectHeightInterval"] = "0"
	startColor := waveshareNativeColor(waveshareOptionString(layer.Options, "gradient_start", waveshareOptionString(layer.Options, "color", "4CE0FF")), layer.Opacity)
	endColor := waveshareNativeColor(waveshareOptionString(layer.Options, "gradient_end", waveshareOptionString(layer.Options, "color", "8B5CF6")), layer.Opacity)
	widget.fields["SoundColumnStartColor"] = startColor
	widget.fields["SoundColumnEndColor"] = endColor
	widget.fields["system_data_flag"] = "1"
	widget.fields["system_data_name"] = "audio mic"
	return widget
}

// compileWaveshareHostAudioBars 把系统回放频谱拆成多个屏端原生进度条。
// 控件定义驻留在 TF 卡中，运行时每个频带只发送一个 0..100 的标量值。
func compileWaveshareHostAudioBars(template waveshareThemeWidget, layer theme.Layer, width, height int, nextID *int) []waveshareThemeWidget {
	bounds := waveshareLayerPixelRect(layer, width, height)
	requestedCount := max(6, min(96, waveshareOptionInt(layer.Options, "count", theme.DefaultAudioBarsCount)))
	direction := strings.ToLower(waveshareOptionString(layer.Options, "direction", "bottom"))
	style := strings.ToLower(waveshareOptionString(layer.Options, "style", "equalizer"))
	if style == "wave" {
		style = "waveform"
	}
	if direction == "fourway" || style == "burst" {
		return compileWaveshareHostAudioBurst(template, layer, width, height, nextID)
	}
	availableBands := bounds.width
	if direction == "horizontal" {
		availableBands = bounds.height
	}
	count := max(1, min(requestedCount, availableBands))
	requestedGap := max(0, min(24, waveshareOptionInt(layer.Options, "gap", max(1, bounds.width/max(1, count*4)))))
	columnSlot, gap := wavesharePackedLinearLayout(bounds.width, count, requestedGap)
	barWidth := max(1, min(columnSlot, waveshareOptionInt(layer.Options, "thickness", max(1, int(math.Round(float64(columnSlot)*.52))))))
	foreground := waveshareNativeColor(waveshareOptionString(layer.Options, "gradient_start", waveshareOptionString(layer.Options, "color", "4CE0FF")), layer.Opacity)
	gradient := waveshareNativeColor(waveshareOptionString(layer.Options, "gradient_end", waveshareOptionString(layer.Options, "color", "8B5CF6")), layer.Opacity)
	background := waveshareNativeColor(waveshareOptionString(layer.Options, "track_color", "00000000"), math.Min(layer.Opacity, .72))
	cornerRadius := waveshareAudioCornerRadius(layer.Options, barWidth)
	if style == "center" {
		direction = "center"
	}
	capacity := count
	if direction == "center" || style == "mirror" {
		capacity *= 2
	}
	widgets := make([]waveshareThemeWidget, 0, capacity)
	for index := 0; index < count; index++ {
		x := bounds.x + index*(columnSlot+gap) + (columnSlot-barWidth)/2
		band := index * waveshareHostSpectrumBands / count
		systemName := theme.WaveshareAudioBandKey(layer, band)
		if direction == "horizontal" {
			rowSlot, rowGap := wavesharePackedLinearLayout(bounds.height, count, requestedGap)
			rowHeight := max(1, min(rowSlot, waveshareOptionInt(layer.Options, "thickness", max(1, int(math.Round(float64(rowSlot)*.52))))))
			y := bounds.y + index*(rowSlot+rowGap) + (rowSlot-rowHeight)/2
			widget := waveshareScalarProgressWidget(template, nextID, fmt.Sprintf("%s-band-%02d", layer.ID, index), systemName, bounds.x, y, bounds.width, rowHeight, 0, max(1, layer.ZIndex*10), foreground, gradient, background, waveshareAudioCornerRadius(layer.Options, rowHeight))
			waveshareApplyAudioWidgetStyle(&widget, style, index)
			widgets = append(widgets, waveshareFinalizeAudioBandWidgets(widget, style, bounds, layer, band, nextID)...)
			continue
		}
		if direction == "center" {
			halfHeight := max(2, bounds.height/2)
			centerY := bounds.y + bounds.height/2
			first := waveshareScalarProgressWidget(template, nextID, fmt.Sprintf("%s-band-%02d-a", layer.ID, index), systemName, x, centerY, halfHeight, barWidth, -90, max(1, layer.ZIndex*10), foreground, gradient, background, cornerRadius)
			second := waveshareScalarProgressWidget(template, nextID, fmt.Sprintf("%s-band-%02d-b", layer.ID, index), systemName, x, centerY, halfHeight, barWidth, 90, max(1, layer.ZIndex*10), foreground, gradient, background, cornerRadius)
			waveshareApplyAudioWidgetStyle(&first, style, index)
			waveshareApplyAudioWidgetStyle(&second, style, index)
			widgets = append(widgets, waveshareFinalizeAudioBandWidgets(first, style, bounds, layer, band, nextID)...)
			widgets = append(widgets, waveshareFinalizeAudioBandWidgets(second, style, bounds, layer, band, nextID)...)
			continue
		}
		y := bounds.y + bounds.height
		rotation := -90
		if direction == "top" || direction == "down" {
			y = bounds.y
			rotation = 90
		}
		widget := waveshareScalarProgressWidget(template, nextID, fmt.Sprintf("%s-band-%02d", layer.ID, index), systemName, x, y, bounds.height, barWidth, rotation, max(1, layer.ZIndex*10), foreground, gradient, background, cornerRadius)
		waveshareApplyAudioWidgetStyle(&widget, style, index)
		widgets = append(widgets, waveshareFinalizeAudioBandWidgets(widget, style, bounds, layer, band, nextID)...)
		if style == "mirror" {
			reflectionHeight := max(2, bounds.height/3)
			reflection := waveshareScalarProgressWidget(template, nextID, fmt.Sprintf("%s-band-%02d-reflection", layer.ID, index), systemName, x, bounds.y, reflectionHeight, barWidth, 90, max(1, layer.ZIndex*10)+1, waveshareNativeColor(waveshareOptionString(layer.Options, "gradient_end", "8B5CF6"), layer.Opacity*.4), gradient, "r=0,g=0,b=0,a=0", cornerRadius)
			widgets = append(widgets, waveshareFinalizeAudioBandWidgets(reflection, style, bounds, layer, band, nextID)...)
		}
	}
	return widgets
}

// compileWaveshareHostAudioBurst 把条状频谱的“四向爆发”映射为围绕中心
// 向外生长的原生进度条。它复用同一组短频带键，不增加主机传输字段。
func compileWaveshareHostAudioBurst(template waveshareThemeWidget, layer theme.Layer, width, height int, nextID *int) []waveshareThemeWidget {
	options := make(map[string]any, len(layer.Options)+3)
	for key, value := range layer.Options {
		options[key] = value
	}
	options["style"] = "spokes"
	options["direction"] = "outward"
	if _, ok := options["inner_radius"]; !ok {
		options["inner_radius"] = .46
	}
	burst := layer
	burst.Options = options
	return compileWaveshareHostAudioRing(template, burst, width, height, nextID)
}

// waveshareFinalizeAudioBandWidgets 在普通频谱控件下方可选叠加一个峰值尾迹。
// 尾迹也是驻留在 TF 卡的原生进度控件，运行时只多传一个数值键。
func waveshareFinalizeAudioBandWidgets(widget waveshareThemeWidget, style string, bounds wavesharePixelRect, layer theme.Layer, band int, nextID *int) []waveshareThemeWidget {
	baseWidgets := waveshareFinalizeHostBarWidgets(widget, style, bounds, nextID)
	resolvedStyle := strings.ToLower(style)
	if resolvedStyle == "" {
		resolvedStyle = strings.ToLower(waveshareOptionString(layer.Options, "style", "radial"))
	}
	if resolvedStyle == "wave" {
		resolvedStyle = "waveform"
	}
	defaultGlow := resolvedStyle == "neon" || resolvedStyle == "halo" || resolvedStyle == "pulse" || resolvedStyle == "waveform"
	glowWidgets := waveshareAudioGlowWidgets(baseWidgets, bounds, layer, defaultGlow, nextID)
	if !waveshareOptionBool(layer.Options, "peak_hold", false) {
		return append(baseWidgets, glowWidgets...)
	}
	peakColor := waveshareNativeColor(waveshareOptionString(layer.Options, "peak_color", "FFF275"), layer.Opacity*.55)
	widgets := make([]waveshareThemeWidget, 0, len(baseWidgets)*2+len(glowWidgets))
	for index := range baseWidgets {
		base := baseWidgets[index]
		ghost := cloneWaveshareThemeWidget(base)
		ghost.fields["id"] = strconv.Itoa(*nextID)
		*nextID++
		ghost.fields["itemName"] = base.fields["itemName"] + "-peak"
		ghost.fields["system_data_name"] = theme.WaveshareAudioPeakKey(layer, band)
		ghost.fields["front_color"] = peakColor
		ghost.fields["lineargradient_color"] = peakColor
		ghost.fields["back_color"] = "r=0,g=0,b=0,a=0"
		ghost.fields["border_width"] = "0"
		ghost.fields["border_color"] = "r=0,g=0,b=0,a=0"
		waveshareNarrowAudioPeakTrace(&ghost, bounds)
		if z, err := strconv.Atoi(base.fields["z"]); err == nil {
			ghost.fields["z"] = strconv.Itoa(max(1, z))
			base.fields["z"] = strconv.Itoa(max(1, z+1))
		}
		widgets = append(widgets, ghost, base)
	}
	widgets = append(widgets, glowWidgets...)
	return widgets
}

// waveshareNarrowAudioPeakTrace 把从零到峰值的固件进度填充收窄为中心
// 轨迹。102 控件不能移动独立标记；窄轨迹既保留峰值位置，又不会在
// 当前能量回落后用整根同宽色柱冒充实时高度。
func waveshareNarrowAudioPeakTrace(widget *waveshareThemeWidget, bounds wavesharePixelRect) {
	thickness, thicknessErr := strconv.Atoi(widget.fields["h"])
	rotation, rotationErr := strconv.ParseFloat(widget.fields["rotate"], 64)
	x, xErr := strconv.Atoi(widget.fields["x"])
	y, yErr := strconv.Atoi(widget.fields["y"])
	if thicknessErr != nil || rotationErr != nil || xErr != nil || yErr != nil || thickness <= 1 {
		return
	}
	traceThickness := max(1, min(2, thickness/3))
	if traceThickness >= thickness {
		return
	}
	localOffset := float64(thickness-traceThickness) / 2
	radians := rotation * math.Pi / 180
	widget.fields["x"] = strconv.Itoa(int(math.Round(float64(x) - math.Sin(radians)*localOffset)))
	widget.fields["y"] = strconv.Itoa(int(math.Round(float64(y) + math.Cos(radians)*localOffset)))
	widget.fields["h"] = strconv.Itoa(traceThickness)
	widget.fields["corner_radius"] = strconv.Itoa(traceThickness / 2)
	waveshareClampRotatedWidgetToRect(widget, bounds)
}

// waveshareAudioGlowWidgets 用绑定同一频带键的半透明宽柱近似柔光。
// 控件本身随 .Theme 驻留，运行时不增加任何 USB 键值字段。
func waveshareAudioGlowWidgets(base []waveshareThemeWidget, bounds wavesharePixelRect, layer theme.Layer, fallback bool, nextID *int) []waveshareThemeWidget {
	if !waveshareOptionBool(layer.Options, "glow", fallback) || len(base) == 0 {
		return nil
	}
	radius := max(1, min(16, waveshareOptionInt(layer.Options, "glow_radius", 6)))
	glowStart := waveshareNativeColor(waveshareOptionString(layer.Options, "glow_color",
		waveshareOptionString(layer.Options, "gradient_start", waveshareOptionString(layer.Options, "color", "4CE0FF"))), layer.Opacity*.16)
	glowEnd := waveshareNativeColor(waveshareOptionString(layer.Options, "glow_color",
		waveshareOptionString(layer.Options, "gradient_end", waveshareOptionString(layer.Options, "color", "8B5CF6"))), layer.Opacity*.12)
	result := make([]waveshareThemeWidget, 0, len(base))
	for _, source := range base {
		glow := cloneWaveshareThemeWidget(source)
		glow.fields["id"] = strconv.Itoa(*nextID)
		*nextID++
		glow.fields["itemName"] = source.fields["itemName"] + "-glow"
		thickness, thicknessErr := strconv.Atoi(source.fields["h"])
		rotation, rotationErr := strconv.ParseFloat(source.fields["rotate"], 64)
		x, xErr := strconv.Atoi(source.fields["x"])
		y, yErr := strconv.Atoi(source.fields["y"])
		if thicknessErr == nil && rotationErr == nil && xErr == nil && yErr == nil {
			radians := rotation * math.Pi / 180
			// 进度控件沿本地 X 轴增长；加宽本地 Y 轴时反向平移半径，
			// 使柔光继续围绕原细柱的中心线。
			glow.fields["x"] = strconv.Itoa(int(math.Round(float64(x) + math.Sin(radians)*float64(radius))))
			glow.fields["y"] = strconv.Itoa(int(math.Round(float64(y) - math.Cos(radians)*float64(radius))))
			glow.fields["h"] = strconv.Itoa(max(1, thickness+radius*2))
			glow.fields["corner_radius"] = strconv.Itoa(max(1, (thickness+radius*2)/2))
		}
		glow.fields["front_color"] = glowStart
		glow.fields["lineargradient_color"] = glowEnd
		glow.fields["back_color"] = "r=0,g=0,b=0,a=0"
		glow.fields["border_width"] = "0"
		glow.fields["border_color"] = "r=0,g=0,b=0,a=0"
		if z, err := strconv.Atoi(source.fields["z"]); err == nil {
			glow.fields["z"] = strconv.Itoa(max(1, z-1))
		}
		waveshareClampRotatedWidgetToRect(&glow, bounds)
		result = append(result, glow)
	}
	return result
}

func waveshareFinalizeHostBarWidgets(widget waveshareThemeWidget, style string, bounds wavesharePixelRect, nextID *int) []waveshareThemeWidget {
	if style != "blocks" && style != "dots" {
		waveshareClampRotatedWidgetToRect(&widget, bounds)
		return []waveshareThemeWidget{widget}
	}
	x, xErr := strconv.Atoi(widget.fields["x"])
	y, yErr := strconv.Atoi(widget.fields["y"])
	width, widthErr := strconv.Atoi(widget.fields["w"])
	height, heightErr := strconv.Atoi(widget.fields["h"])
	rotation, rotationErr := strconv.ParseFloat(widget.fields["rotate"], 64)
	if xErr != nil || yErr != nil || widthErr != nil || heightErr != nil || rotationErr != nil || width < 2 || height < 1 {
		waveshareClampRotatedWidgetToRect(&widget, bounds)
		return []waveshareThemeWidget{widget}
	}
	count := max(2, min(16, width/max(1, height+2)))
	if width < count {
		count = width
	}
	radians := rotation * math.Pi / 180
	cosine, sine := math.Cos(radians), math.Sin(radians)
	widgets := make([]waveshareThemeWidget, 0, count)
	segmentLength, gap := wavesharePackedLinearLayout(width, count, max(1, min(4, height/3)))
	for index := 0; index < count; index++ {
		localX := index * (segmentLength + gap)
		localY := 0
		itemWidth, itemHeight := segmentLength, height
		if style == "dots" {
			diameter := max(1, min(height, segmentLength))
			itemWidth, itemHeight = diameter, diameter
			localX = index * max(1, (width-diameter)/max(1, count-1))
			localY = max(0, (height-diameter)/2)
		}
		segment := cloneWaveshareThemeWidget(widget)
		if index > 0 {
			segment.fields["id"] = strconv.Itoa(*nextID)
			*nextID++
		}
		segment.fields["itemName"] = fmt.Sprintf("%s-segment-%02d", widget.fields["itemName"], index)
		segment.fields["x"] = strconv.Itoa(int(math.Round(float64(x) + float64(localX)*cosine - float64(localY)*sine)))
		segment.fields["y"] = strconv.Itoa(int(math.Round(float64(y) + float64(localX)*sine + float64(localY)*cosine)))
		segment.fields["w"] = strconv.Itoa(itemWidth)
		segment.fields["h"] = strconv.Itoa(itemHeight)
		segment.fields["system_data_min_value"] = waveshareFormatNumber(float64(index) * 100 / float64(count))
		segment.fields["system_data_max_value"] = waveshareFormatNumber(float64(index+1) * 100 / float64(count))
		if style == "dots" {
			segment.fields["corner_radius"] = strconv.Itoa(max(1, min(itemWidth, itemHeight)/2))
		}
		waveshareClampRotatedWidgetToRect(&segment, bounds)
		widgets = append(widgets, segment)
	}
	return widgets
}

// wavesharePackedLinearLayout 在给定像素长度内排列固定数量的项，避免高频带数和大间隔把控件推出画布。
func wavesharePackedLinearLayout(length, count, requestedGap int) (itemSize, gap int) {
	length = max(1, length)
	count = max(1, min(count, length))
	gap = max(0, requestedGap)
	if count > 1 {
		gap = min(gap, max(0, (length-count)/(count-1)))
	} else {
		gap = 0
	}
	return max(1, (length-gap*(count-1))/count), gap
}

// compileWaveshareHostAudioRing 用一圈可旋转的原生进度条组成真正由主机频带驱动的环形频谱。
func compileWaveshareHostAudioRing(template waveshareThemeWidget, layer theme.Layer, width, height int, nextID *int) []waveshareThemeWidget {
	bounds := waveshareLayerPixelRect(layer, width, height)
	style := strings.ToLower(waveshareOptionString(layer.Options, "style", "radial"))
	if style == "wave" {
		style = "waveform"
	}
	count := theme.WaveshareAudioRingCount(layer)
	outerRadius := max(12, min(bounds.width, bounds.height)/2-2)
	columnHeight := max(8, min(64, waveshareOptionInt(layer.Options, "bar_length", max(10, outerRadius/3))))
	innerRadiusValue := waveshareOptionFloat(layer.Options, "inner_radius", float64(outerRadius-columnHeight))
	innerRadius := int(math.Round(innerRadiusValue))
	if innerRadiusValue > 0 && innerRadiusValue <= 1 {
		innerRadius = int(math.Round(float64(outerRadius) * innerRadiusValue))
	}
	innerRadius = max(4, min(outerRadius-4, innerRadius))
	columnHeight = max(4, outerRadius-innerRadius)
	columnWidth := max(1, min(16, waveshareOptionInt(layer.Options, "thickness", 1)))
	switch style {
	case "donut":
		columnHeight = max(4, min(columnHeight, columnWidth*2))
		columnWidth = max(columnWidth, min(18, columnHeight))
	case "dots":
		columnHeight = columnWidth
	case "dashed":
		columnHeight = max(4, min(columnHeight, columnWidth*3))
	case "spokes":
		columnHeight = max(columnHeight, min(72, outerRadius/2))
	case "pulse":
		columnWidth = max(columnWidth, min(20, columnWidth*2))
	}
	foreground := waveshareNativeColor(waveshareOptionString(layer.Options, "gradient_start", waveshareOptionString(layer.Options, "color", "C363E2")), layer.Opacity)
	gradient := waveshareNativeColor(waveshareOptionString(layer.Options, "gradient_end", waveshareOptionString(layer.Options, "color", "7C5CFF")), layer.Opacity)
	background := waveshareNativeColor(waveshareOptionString(layer.Options, "track_color", "00000000"), math.Min(layer.Opacity, .55))
	cornerRadius := waveshareAudioCornerRadius(layer.Options, columnWidth)
	centerX := float64(bounds.x) + float64(bounds.width)/2
	centerY := float64(bounds.y) + float64(bounds.height)/2
	direction := strings.ToLower(waveshareOptionString(layer.Options, "direction", "bidirectional"))
	startAngle := waveshareOptionFloat(layer.Options, "start_angle", -90)
	capacity := count
	if direction == "bidirectional" {
		capacity *= 2
	}
	if style == "halo" {
		capacity *= 2
	}
	widgets := make([]waveshareThemeWidget, 0, capacity)
	for index := 0; index < count; index++ {
		fraction := float64(index) / float64(count)
		angle := startAngle + fraction*360
		radians := angle * math.Pi / 180
		band := index * waveshareHostSpectrumBands / count
		rotation := int(math.Round(angle))
		if style == "dashed" && index%2 != 0 {
			continue
		}
		systemName := theme.WaveshareAudioBandKey(layer, band)
		makeBand := func(itemName string, radius, length, angleOffset int) waveshareThemeWidget {
			widgetRadians := radians + float64(angleOffset)*math.Pi/180
			x, y := wavesharePolarWidgetAnchor(centerX, centerY, float64(radius), radians, widgetRadians, columnWidth)
			widget := waveshareScalarProgressWidget(template, nextID, itemName, systemName, x, y, length, columnWidth, rotation+angleOffset, max(1, layer.ZIndex*10), foreground, gradient, background, cornerRadius)
			waveshareApplyAudioWidgetStyle(&widget, style, index)
			waveshareClampRotatedWidgetToRect(&widget, bounds)
			return widget
		}
		switch direction {
		case "inward":
			widgets = append(widgets, waveshareFinalizeAudioBandWidgets(makeBand(fmt.Sprintf("%s-band-%02d", layer.ID, index), outerRadius, columnHeight, 180), "", bounds, layer, band, nextID)...)
		case "bidirectional":
			middleRadius := (innerRadius + outerRadius) / 2
			outwardLength := max(2, outerRadius-middleRadius)
			inwardLength := max(2, middleRadius-innerRadius)
			widgets = append(widgets, waveshareFinalizeAudioBandWidgets(makeBand(fmt.Sprintf("%s-band-%02d-out", layer.ID, index), middleRadius, outwardLength, 0), "", bounds, layer, band, nextID)...)
			widgets = append(widgets, waveshareFinalizeAudioBandWidgets(makeBand(fmt.Sprintf("%s-band-%02d-in", layer.ID, index), middleRadius, inwardLength, 180), "", bounds, layer, band, nextID)...)
		default:
			widgets = append(widgets, waveshareFinalizeAudioBandWidgets(makeBand(fmt.Sprintf("%s-band-%02d", layer.ID, index), innerRadius, columnHeight, 0), "", bounds, layer, band, nextID)...)
		}
		if style == "halo" {
			haloRadius := max(4, innerRadius-columnWidth*2)
			haloWidth := max(2, columnWidth/2)
			haloX, haloY := wavesharePolarWidgetAnchor(centerX, centerY, float64(haloRadius), radians, radians, haloWidth)
			halo := waveshareScalarProgressWidget(template, nextID, fmt.Sprintf("%s-halo-%02d", layer.ID, index), theme.WaveshareAudioBandKey(layer, band), haloX, haloY, max(4, columnHeight/2), max(2, columnWidth/2), rotation, max(1, layer.ZIndex*10)+1, gradient, foreground, "r=0,g=0,b=0,a=0", max(1, cornerRadius/2))
			widgets = append(widgets, waveshareFinalizeAudioBandWidgets(halo, "", bounds, layer, band, nextID)...)
		}
	}
	return widgets
}

// wavesharePolarWidgetAnchor 把条形控件的横向中线放到指定半径，避免旋转后的线宽只向一侧伸展。
func wavesharePolarWidgetAnchor(centerX, centerY, radius, radialRadians, widgetRadians float64, thickness int) (int, int) {
	halfThickness := float64(max(1, thickness)) / 2
	return int(math.Round(centerX + math.Cos(radialRadians)*radius + math.Sin(widgetRadians)*halfThickness)),
		int(math.Round(centerY + math.Sin(radialRadians)*radius - math.Cos(widgetRadians)*halfThickness))
}

func waveshareApplyAudioWidgetStyle(widget *waveshareThemeWidget, style string, _ int) {
	// 102 进度控件的背景和描边覆盖整个控件长度，而不是只覆盖当前
	// 进度。频谱若保留这些字段，静音时仍会出现满高柱框，低音量也
	// 只像在固定格子底部换色。动态频谱必须只让前景随真实能量出现。
	widget.fields["back_color"] = "r=0,g=0,b=0,a=0"
	widget.fields["border_width"] = "0"
	widget.fields["border_color"] = "r=0,g=0,b=0,a=0"
	switch style {
	case "wave", "waveform":
		// 小包协议只能更新进度值，不能每帧移动控件。用无轨道、细圆角柱
		// 近似连续波形，绝不再叠加与真实声音无关的固定正弦偏移。
		if height, err := strconv.Atoi(widget.fields["h"]); err == nil {
			widget.fields["corner_radius"] = strconv.Itoa(max(1, height/2))
		}
	case "blocks", "dashed":
		widget.fields["corner_radius"] = "0"
		widget.fields["lineargradient_flag"] = "0"
	case "dots", "donut":
		if height, err := strconv.Atoi(widget.fields["h"]); err == nil {
			widget.fields["corner_radius"] = strconv.Itoa(max(1, height/2))
		}
	case "neon", "pulse":
		// 霓虹由随能量增长的柔光副控件实现，不能用整高描边冒充光晕。
	case "outline":
		// 固件描边始终覆盖整个量程；退化为无渐变的实心活动段，才能
		// 保证零值完全不可见且低值只占真实高度。
		widget.fields["lineargradient_flag"] = "0"
	}
}

func waveshareScalarProgressWidget(template waveshareThemeWidget, nextID *int, itemName, systemName string, x, y, width, height, rotation, z int, foreground, gradient, background string, cornerRadius int) waveshareThemeWidget {
	widget := cloneWaveshareThemeWidget(template)
	widget.fields["id"] = strconv.Itoa(*nextID)
	*nextID++
	widget.fields["itemName"] = itemName
	widget.fields["x"] = strconv.Itoa(x)
	widget.fields["y"] = strconv.Itoa(y)
	widget.fields["w"] = strconv.Itoa(max(1, width))
	widget.fields["h"] = strconv.Itoa(max(1, height))
	widget.fields["z"] = strconv.Itoa(max(1, z))
	widget.fields["rotate"] = strconv.Itoa(rotation)
	widget.fields["scale"] = "1"
	widget.fields["lock"] = "1"
	widget.fields["front_color"] = foreground
	widget.fields["back_color"] = background
	widget.fields["border_color"] = "r=0,g=0,b=0,a=0"
	widget.fields["border_width"] = "0"
	widget.fields["corner_radius"] = strconv.Itoa(max(0, min(cornerRadius, min(max(1, width), max(1, height))/2)))
	widget.fields["lineargradient_flag"] = "1"
	widget.fields["lineargradient_color"] = gradient
	widget.fields["system_data_flag"] = "1"
	widget.fields["system_data_name"] = systemName
	widget.fields["system_data_min_value"] = "0"
	widget.fields["system_data_max_value"] = "100"
	delete(widget.fields, "system_data_type")
	return widget
}

// waveshareClampRotatedWidgetToRect 按 Qt 围绕控件左上角旋转的规则修正锚点。
// 这对 -90° 的自下向上进度条尤其重要，否则反向和顶部模式会多偏移一个条宽。
func waveshareClampRotatedWidgetToRect(widget *waveshareThemeWidget, bounds wavesharePixelRect) {
	x, xErr := strconv.Atoi(widget.fields["x"])
	y, yErr := strconv.Atoi(widget.fields["y"])
	width, widthErr := strconv.Atoi(widget.fields["w"])
	height, heightErr := strconv.Atoi(widget.fields["h"])
	rotation, rotationErr := strconv.ParseFloat(widget.fields["rotate"], 64)
	if xErr != nil || yErr != nil || widthErr != nil || heightErr != nil || rotationErr != nil || width <= 0 || height <= 0 {
		return
	}
	radians := rotation * math.Pi / 180
	cosine, sine := math.Cos(radians), math.Sin(radians)
	minX, maxX, minY, maxY := 0.0, 0.0, 0.0, 0.0
	for _, point := range [][2]float64{{0, 0}, {float64(width), 0}, {0, float64(height)}, {float64(width), float64(height)}} {
		rotatedX := point[0]*cosine - point[1]*sine
		rotatedY := point[0]*sine + point[1]*cosine
		minX, maxX = math.Min(minX, rotatedX), math.Max(maxX, rotatedX)
		minY, maxY = math.Min(minY, rotatedY), math.Max(maxY, rotatedY)
	}
	targetMinX, targetMinY := float64(bounds.x), float64(bounds.y)
	targetMaxX, targetMaxY := float64(bounds.x+bounds.width), float64(bounds.y+bounds.height)
	anchorX, anchorY := float64(x), float64(y)
	if anchorX+minX < targetMinX {
		anchorX += targetMinX - (anchorX + minX)
	}
	if anchorX+maxX > targetMaxX {
		anchorX -= anchorX + maxX - targetMaxX
	}
	if anchorY+minY < targetMinY {
		anchorY += targetMinY - (anchorY + minY)
	}
	if anchorY+maxY > targetMaxY {
		anchorY -= anchorY + maxY - targetMaxY
	}
	widget.fields["x"] = strconv.Itoa(int(math.Round(anchorX)))
	widget.fields["y"] = strconv.Itoa(int(math.Round(anchorY)))
}

func waveshareProgressWidget(template waveshareThemeWidget, nextID *int, itemName, systemName string, x, y, width, height, rotation, z int, foreground, background string, cornerRadius int, minimum, maximum float64) waveshareThemeWidget {
	widget := waveshareScalarProgressWidget(template, nextID, itemName, systemName, x, y, width, height, rotation, z, foreground, foreground, background, cornerRadius)
	widget.fields["lineargradient_flag"] = "0"
	widget.fields["system_data_min_value"] = waveshareFormatNumber(minimum)
	widget.fields["system_data_max_value"] = waveshareFormatNumber(maximum)
	return widget
}

func waveshareGaugeWidget(template waveshareThemeWidget, nextID *int, itemName, systemName string, x, y, radius, margin, z int, foreground, background string, minimum, maximum float64) waveshareThemeWidget {
	widget := cloneWaveshareThemeWidget(template)
	widget.fields["id"] = strconv.Itoa(*nextID)
	*nextID++
	widget.fields["itemName"] = itemName
	widget.fields["x"] = strconv.Itoa(x)
	widget.fields["y"] = strconv.Itoa(y)
	widget.fields["z"] = strconv.Itoa(max(1, z))
	widget.fields["rotate"] = "0"
	widget.fields["scale"] = "1"
	widget.fields["lock"] = "1"
	widget.fields["radius"] = strconv.Itoa(max(1, radius))
	widget.fields["margin"] = strconv.Itoa(max(1, min(margin, radius)))
	widget.fields["front_color"] = foreground
	widget.fields["back_color"] = background
	widget.fields["system_data_flag"] = "1"
	widget.fields["system_data_name"] = systemName
	widget.fields["system_data_min_value"] = waveshareFormatNumber(minimum)
	widget.fields["system_data_max_value"] = waveshareFormatNumber(maximum)
	delete(widget.fields, "system_data_type")
	return widget
}

func waveshareFormatNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func waveshareAudioCornerRadius(options map[string]any, thickness int) int {
	shape := strings.ToLower(waveshareOptionString(options, "bar_shape", "rounded"))
	switch shape {
	case "square":
		return 0
	case "pill", "capsule", "round", "dots":
		return max(1, thickness/2)
	case "line":
		return max(0, thickness/4)
	default:
		return max(1, min(6, thickness/3))
	}
}

// compileWaveshareAudioRingLayer 生成 PCMonitorLinux 的原生 107 号环状频谱控件。
// 微雪 V2 主题没有可直接复制的 107 模板，因此沿用 108 号音频控件的公共字段，
// 再按照原厂 PCMonitor V1 主题中的 107 字段表转换。
func compileWaveshareAudioRingLayer(template waveshareThemeWidget, layer theme.Layer, width, height int, nextID *int) waveshareThemeWidget {
	bounds := waveshareLayerPixelRect(layer, width, height)
	widget := cloneWaveshareThemeWidget(template)
	widget.fields["id"] = strconv.Itoa(*nextID)
	*nextID++
	widget.fields["itemName"] = layer.ID
	widget.fields["type"] = "107"
	widget.fields["z"] = strconv.Itoa(max(1, layer.ZIndex*10))
	widget.fields["rotate"] = strconv.Itoa(int(math.Round(layer.Rotation)))
	widget.fields["scale"] = "1"
	widget.fields["lock"] = "1"

	// 107 控件始终在一个以屏幕短边为边长的正方形中绘制。
	// 横屏时它的默认圆心是 (短边/2, 短边/2)，不能使用画布宽度的一半。
	// 这也与原厂 480×1920 竖屏主题通过 y=1400 下移圆环的坐标方式一致。
	centerX := bounds.x + bounds.width/2
	centerY := bounds.y + bounds.height/2
	baseCenter := min(width, height) / 2
	widget.fields["x"] = strconv.Itoa(centerX - baseCenter)
	widget.fields["y"] = strconv.Itoa(centerY - baseCenter)
	widget.fields["screen_w"] = strconv.Itoa(width)
	widget.fields["screen_h"] = strconv.Itoa(height)

	outerRadius := max(12, min(bounds.width, bounds.height)/2-3)
	columnHeight := max(8, min(48, outerRadius/3))
	roundWidth := max(2, min(6, outerRadius/20))
	radius := max(4, outerRadius-columnHeight-roundWidth)
	columnWidth := max(2, min(12, int(math.Round(float64(radius)*2*math.Pi/72))))
	widget.fields["radius"] = strconv.Itoa(radius)
	widget.fields["SoundColumnHeight"] = strconv.Itoa(columnHeight)
	widget.fields["SoundColumnWidth"] = strconv.Itoa(columnWidth)
	widget.fields["SoundColumnWidthInterval"] = "2"
	widget.fields["RoundWidth"] = strconv.Itoa(roundWidth)
	direction := strings.ToLower(waveshareOptionString(layer.Options, "direction", "outward"))
	switch direction {
	case "inward":
		widget.fields["DisplayDirection"] = "1"
	case "bidirectional":
		widget.fields["DisplayDirection"] = "2"
	default:
		widget.fields["DisplayDirection"] = "0"
	}
	widget.fields["Clockwise"] = "1"
	widget.fields["BoardSoundGain"] = "31"
	widget.fields["StartFrequency"] = "50"
	widget.fields["EndFrequency"] = "20000"
	widget.fields["path"] = ""

	startColor := waveshareNativeColor(waveshareOptionString(layer.Options, "gradient_start", waveshareOptionString(layer.Options, "color", "C363E2")), layer.Opacity)
	endColor := waveshareNativeColor(waveshareOptionString(layer.Options, "gradient_end", waveshareOptionString(layer.Options, "color", "7C5CFF")), layer.Opacity)
	transparent := "r=0,g=0,b=0,a=0"
	widget.fields["SoundColumnStartColor"] = startColor
	widget.fields["SoundColumnEndColor"] = endColor
	widget.fields["SoundRoundColor"] = startColor
	widget.fields["SoundSolidLineColor"] = transparent
	widget.fields["SoundDashedColor"] = transparent
	widget.fields["system_data_flag"] = "1"
	widget.fields["system_data_name"] = "audio mic"

	delete(widget.fields, "rectWidthInterval")
	delete(widget.fields, "rectWidthCount")
	delete(widget.fields, "rectWidth")
	delete(widget.fields, "rectHeightInterval")
	delete(widget.fields, "rectHeightCount")
	delete(widget.fields, "rectHeight")
	return widget
}

func compileWaveshareLyricsLayer(template, progressTemplate waveshareThemeWidget, hasProgressTemplate bool, layer theme.Layer, width, height int, nextID *int) []waveshareThemeWidget {
	bounds := waveshareLayerPixelRect(layer, width, height)
	// 每个逻辑层预留十个 z 槽位：动态面板位于文字下方，进度位于文字
	// 上方，同时不越过下一个逻辑层。这样 LV=0 时整组控件可一起消失。
	z := max(5, layer.ZIndex*10+5)
	current := waveshareOptionString(layer.Options, "placeholder_current", "正在等待歌词")
	next := waveshareOptionString(layer.Options, "placeholder_next", "播放歌曲后显示下一句")
	style := theme.LyricsStyle(layer)
	currentColorValue := waveshareOptionString(layer.Options, "current_color", waveshareOptionString(layer.Options, "color", "F7FCFF"))
	if theme.LyricsUsesKaraokeHighlight(layer) {
		// 原厂动态文字控件不能按字形裁切，屏端以强调色突出整句当前歌词；
		// 主机合成设备仍可使用逐字裁切，微雪的实际动态进度由下方标量轨兑现。
		currentColorValue = waveshareOptionString(layer.Options, "accent_color", "55E5FF")
	}
	currentColor := waveshareNativeColor(currentColorValue, layer.Opacity)
	nextColor := waveshareNativeColor(waveshareOptionString(layer.Options, "next_color", "CDD7E6"), layer.Opacity*.82)
	horizontalPadding := min(8, max(1, bounds.width/24))
	verticalPadding := min(6, max(0, bounds.height/30))
	content := waveshareInsetPixelRect(bounds, horizontalPadding, verticalPadding, horizontalPadding, verticalPadding)
	progressThickness := max(1, min(10, waveshareOptionInt(layer.Options, "progress_thickness", max(2, bounds.height/45))))
	minimumTextRows := 1
	if theme.LyricsShowsNext(layer) {
		minimumTextRows = 2
	}
	minimumTextHeight := waveshareRenderedTextHeight(waveshareMinimumTextSize)
	showProgress := hasProgressTemplate && theme.LyricsShowsProgress(layer) &&
		content.height >= minimumTextRows*minimumTextHeight+progressThickness+max(2, verticalPadding)
	if showProgress {
		reserved := progressThickness + max(2, verticalPadding)
		content.height = max(1, content.height-reserved)
	}
	currentRect, nextRect, showNext := waveshareLyricsTextRects(content, style)
	if currentRect.height < minimumTextHeight || (showNext && nextRect.height < minimumTextHeight) {
		// 极小图层优先保留 Current；不能为了形式上的双行把两行都挤到
		// 固件最小字号以下，导致实际屏幕完全没有歌词控件。
		currentRect = content
		nextRect = wavesharePixelRect{}
		showNext = false
	}
	widgets := make([]waveshareThemeWidget, 0, 40)
	if hasProgressTemplate {
		widgets = append(widgets, compileWaveshareLyricsVisibilityShell(progressTemplate, layer, bounds, z-4, nextID)...)
	}
	align := waveshareOptionString(layer.Options, "align", "center")
	currentSize := waveshareOptionInt(layer.Options, "font_size", theme.DefaultLyricsFontSize)
	switch style {
	case theme.LyricsStyleKaraoke:
		currentSize = int(math.Round(float64(currentSize) * 1.14))
	case theme.LyricsStyleClassic:
		currentSize = int(math.Round(float64(currentSize) * .92))
	case theme.LyricsStyleMinimal:
		currentSize = int(math.Round(float64(currentSize) * 1.06))
	}
	// 动态文字创建后字号不会随 QStringMap 内容改变。按最长状态提示预留
	// 字宽，避免“不可滚动”等中文提示在竖屏窄栏中超出图层边界。
	currentText, currentOK := waveshareFitDynamicLyricText(currentRect, current,
		"该首歌的歌词暂不支持滚动", currentSize, align)
	if currentOK {
		// 固件按 system_data_name 原样查 QMap，因此可以使用 CaseScreen 私有键，
		// 不再借用 CPU/GPU 型号字段，歌词与硬件型号可同时显示。
		currentWidgets := waveshareLyricTextWidgets(template, nextID, layer.ID+"-current", currentText.text,
			"CaseScreen_LyricsCurrent", currentText.x, currentText.y, currentText.size, currentColor, z, currentRect, layer, false)
		waveshareClearInitialLyricText(currentWidgets)
		widgets = append(widgets, currentWidgets...)
	}
	if showNext && nextRect.height >= waveshareRenderedTextHeight(waveshareMinimumTextSize) {
		nextText, nextOK := waveshareFitDynamicLyricText(nextRect, next,
			"请在原音乐播放器查看完整歌词", waveshareOptionInt(layer.Options, "next_font_size", theme.DefaultLyricsNextFontSize), align)
		if nextOK {
			nextWidgets := waveshareLyricTextWidgets(template, nextID, layer.ID+"-next", nextText.text,
				"CaseScreen_LyricsNext", nextText.x, nextText.y, nextText.size, nextColor, z+1, nextRect, layer, true)
			waveshareClearInitialLyricText(nextWidgets)
			widgets = append(widgets, nextWidgets...)
		}
	}
	if showProgress {
		widgets = append(widgets, compileWaveshareLyricsProgress(progressTemplate, layer, bounds, progressThickness, z+3, nextID)...)
	}
	return widgets
}

// waveshareClearInitialLyricText 保留占位样本计算出的字号和位置，但不把
// “正在等待歌词”写进驻留 Theme。首个状态小包到达前屏幕应保持空白。
func waveshareClearInitialLyricText(widgets []waveshareThemeWidget) {
	for index := range widgets {
		if widgets[index].fields["type"] == "8" {
			widgets[index].fields["text_str"] = ""
		}
	}
}

// compileWaveshareLyricsVisibilityShell 用 LV 这个 0/1 短键动态绘制歌词面板、
// 边框和样式装饰。原厂 Theme 没有组可见性属性，102 标量控件是已验证且
// 不增加每帧数据的等价实现：LV=0 完全透明，LV=1 填满目标矩形。
func compileWaveshareLyricsVisibilityShell(template waveshareThemeWidget, layer theme.Layer, bounds wavesharePixelRect, z int, nextID *int) []waveshareThemeWidget {
	transparent := "r=0,g=0,b=0,a=0"
	style := theme.LyricsStyle(layer)
	corner := max(0, waveshareOptionInt(layer.Options, "corner_radius", 14))
	panel := waveshareNativeColor(waveshareOptionString(layer.Options, "panel_color", "030A1292"), layer.Opacity)
	border := waveshareNativeColor(waveshareOptionString(layer.Options, "panel_border_color", "66E3FF3E"), layer.Opacity)
	accent := waveshareNativeColor(waveshareOptionString(layer.Options, "accent_color", "55E5FF"), layer.Opacity)
	widgets := make([]waveshareThemeWidget, 0, 5)
	appendRect := func(name string, rectangle wavesharePixelRect, fill string, widgetZ, radius int) {
		if rectangle.width <= 0 || rectangle.height <= 0 || strings.HasSuffix(fill, ",a=0") {
			return
		}
		widget := waveshareProgressWidget(template, nextID, layer.ID+name, telemetry.LyricsVisibleKey,
			rectangle.x, rectangle.y, rectangle.width, rectangle.height, 0, max(1, widgetZ), fill, transparent,
			max(0, radius), 0, 1)
		widget.fields["lineargradient_flag"] = "0"
		widget.fields["lineargradient_color"] = fill
		widgets = append(widgets, widget)
	}
	if style != theme.LyricsStyleMinimal && !strings.HasSuffix(border, ",a=0") {
		appendRect("-visible-border", bounds, border, z, corner)
		inner := waveshareInsetPixelRect(bounds, 1, 1, 1, 1)
		appendRect("-visible-panel", inner, panel, z+1, max(0, corner-1))
	} else {
		appendRect("-visible-panel", bounds, panel, z, corner)
	}
	if !strings.HasSuffix(accent, ",a=0") {
		switch style {
		case theme.LyricsStyleKaraoke:
			stripeY := bounds.y + max(2, bounds.height/8)
			appendRect("-visible-stripe", wavesharePixelRect{x: bounds.x + max(2, bounds.width/24), y: stripeY,
				width: max(2, bounds.width/4), height: 1}, accent, z+2, 0)
		case theme.LyricsStyleClassic:
			inset := max(2, bounds.width/24)
			appendRect("-visible-divider", wavesharePixelRect{x: bounds.x + inset, y: bounds.y + bounds.height/2,
				width: max(1, bounds.width-inset*2), height: 1}, waveshareScaleNativeColorAlpha(accent, .38), z+2, 0)
			markerWidth := max(1, min(3, bounds.width/80+1))
			markerY := bounds.y + max(2, bounds.height/10)
			appendRect("-visible-marker", wavesharePixelRect{x: bounds.x + inset, y: markerY,
				width: markerWidth, height: max(1, bounds.y+bounds.height-markerY-max(2, bounds.height/10))}, accent, z+2, markerWidth/2)
		}
	}
	return widgets
}

func waveshareFitDynamicLyricText(bounds wavesharePixelRect, placeholder, sizingSample string, preferredSize int, align string) (waveshareFittedText, bool) {
	sizingSize, _ := waveshareFitRenderedTextSize(sizingSample, preferredSize, bounds.width, bounds.height)
	if sizingSize <= 0 {
		return waveshareFittedText{}, false
	}
	return waveshareFitTextInRect(bounds, placeholder, min(preferredSize, sizingSize), align, "center")
}

// waveshareLyricsTextRects 为四种样式给出真实屏端布局。minimal 严格单行；
// karaoke 把当前句放大并下移，classic 使用等高双行，player 保留主次层次。
func waveshareLyricsTextRects(content wavesharePixelRect, style string) (wavesharePixelRect, wavesharePixelRect, bool) {
	if content.width <= 0 || content.height <= 0 || style == theme.LyricsStyleMinimal {
		return content, wavesharePixelRect{}, false
	}
	topInset := 0
	currentShare := .56
	switch style {
	case theme.LyricsStyleKaraoke:
		topInset = max(1, content.height/8)
		currentShare = .62
	case theme.LyricsStyleClassic:
		currentShare = .50
	}
	available := max(2, content.height-topInset)
	currentHeight := max(1, min(available-1, int(math.Round(float64(available)*currentShare))))
	current := wavesharePixelRect{x: content.x, y: content.y + topInset, width: content.width, height: currentHeight}
	next := wavesharePixelRect{x: content.x, y: current.y + current.height, width: content.width, height: max(1, content.y+content.height-(current.y+current.height))}
	return current, next, true
}

// compileWaveshareLyricsProgress 把主机计算的当前句进度绑定到原生 102 控件。
// player/classic 使用连续轨，karaoke 使用分段轨；新版 Theme 消费
// 0..10000 细进度键。主机仍按需发送旧 0..100 键，让已写入设备的旧
// Theme 保持兼容，但不让旧量程继续限制宽轨的平滑度。
func compileWaveshareLyricsProgress(template waveshareThemeWidget, layer theme.Layer, bounds wavesharePixelRect, thickness, z int, nextID *int) []waveshareThemeWidget {
	padding := min(8, max(1, bounds.width/24))
	x := bounds.x + padding
	width := max(1, bounds.width-padding*2)
	y := bounds.y + bounds.height - min(bounds.height, max(thickness+1, padding/2+thickness))
	foreground := waveshareNativeColor(waveshareOptionString(layer.Options, "accent_color", "55E5FF"), layer.Opacity)
	trackColor := waveshareNativeColor(waveshareOptionString(layer.Options, "progress_track_color", "84A2B434"), layer.Opacity)
	transparent := "r=0,g=0,b=0,a=0"
	style := theme.LyricsStyle(layer)
	// 底轨也必须受 LV 控制；真实进度控件的 back_color 保持透明，避免
	// LFP=0 时仍留下整条固定轨道。
	track := waveshareProgressWidget(template, nextID, layer.ID+"-lyrics-progress-track", telemetry.LyricsVisibleKey,
		x, y, width, thickness, 0, z, trackColor, transparent, max(0, thickness/2), 0, 1)
	track.fields["lineargradient_flag"] = "0"
	if style != theme.LyricsStyleKaraoke {
		return []waveshareThemeWidget{track, waveshareProgressWidget(template, nextID, layer.ID+"-lyrics-progress", telemetry.LyricsProgressFineKey,
			x, y, width, thickness, 0, z+1, foreground, transparent, max(0, thickness/2), 0, telemetry.LyricsProgressFineMax)}
	}
	segments := max(4, min(12, width/24))
	gap := max(1, min(3, width/max(1, segments*8)))
	widgets := make([]waveshareThemeWidget, 0, segments+1)
	widgets = append(widgets, track)
	for index := 0; index < segments; index++ {
		left := x + index*width/segments
		right := x + (index+1)*width/segments - gap
		if right <= left {
			continue
		}
		minimum := float64(index) * telemetry.LyricsProgressFineMax / float64(segments)
		maximum := float64(index+1) * telemetry.LyricsProgressFineMax / float64(segments)
		widgets = append(widgets, waveshareProgressWidget(template, nextID, fmt.Sprintf("%s-lyrics-progress-%02d", layer.ID, index), telemetry.LyricsProgressFineKey,
			left, y, right-left, thickness, 0, z+1, foreground, transparent, max(0, thickness/2), minimum, maximum))
	}
	return widgets
}

// waveshareLyricTextWidgets 用多个绑定同一小数据键的原生文字控件叠出描边、阴影和柔光。
// 运行时仍只传当前句/下一句文本，不增加 USB 数据字段。
func waveshareLyricTextWidgets(template waveshareThemeWidget, nextID *int, itemName, textValue, systemName string,
	x, y, size int, foreground string, z int, textBounds wavesharePixelRect, layer theme.Layer, secondary bool) []waveshareThemeWidget {
	style := theme.LyricsStyle(layer)
	fontOptions := waveshareLyricsFontOptions(layer.Options)
	outlineWidth := max(0, min(3, waveshareOptionInt(layer.Options, "outline_width", 2)))
	glowRadius := max(0, min(5, waveshareOptionInt(layer.Options, "glow_radius", 4)))
	shadowEnabled := waveshareOptionBool(layer.Options, "shadow", style != theme.LyricsStyleMinimal)
	if secondary || style == theme.LyricsStyleMinimal {
		outlineWidth = min(outlineWidth, 1)
		glowRadius = 0
	}
	widgets := make([]waveshareThemeWidget, 0, 24)
	textWidth := waveshareEstimatedRenderedTextWidth(textValue, size)
	textHeight := waveshareRenderedTextHeight(size)
	appendEffect := func(suffix string, offsetX, offsetY int, effectColor string, effectZ int) {
		effectX := max(textBounds.x, min(textBounds.x+max(0, textBounds.width-textWidth), x+offsetX))
		effectY := max(textBounds.y, min(textBounds.y+max(0, textBounds.height-textHeight), y+offsetY))
		widget := waveshareStyledTextWidget(template, nextID, textValue, systemName, true, effectX, effectY, size, effectColor, max(1, effectZ), "0", fontOptions)
		widget.fields["itemName"] = itemName + suffix
		widgets = append(widgets, widget)
	}
	if glowRadius > 0 {
		glowColor := waveshareNativeColor(waveshareOptionString(layer.Options, "glow_color", "53DAFF42"), layer.Opacity*.42)
		for _, offset := range [][2]int{{-glowRadius, 0}, {glowRadius, 0}, {0, -glowRadius}, {0, glowRadius}} {
			appendEffect(fmt.Sprintf("-glow-%d-%d", offset[0], offset[1]), offset[0], offset[1], glowColor, z-2)
		}
	}
	if shadowEnabled {
		shadowX := max(-8, min(8, waveshareOptionInt(layer.Options, "shadow_offset_x", 2)))
		shadowY := max(-8, min(8, waveshareOptionInt(layer.Options, "shadow_offset_y", 3)))
		shadowBlur := max(0, min(3, waveshareOptionInt(layer.Options, "shadow_blur", 3)))
		shadowColor := waveshareNativeColor(waveshareOptionString(layer.Options, "shadow_color", "000000B4"), layer.Opacity*.78)
		appendEffect("-shadow", shadowX, shadowY, shadowColor, z-1)
		if shadowBlur > 0 {
			// 原生文字没有模糊半径；用共享同一动态键的稀疏邻域近似柔化，
			// 不增加日常小包字段，也不会烘焙一条永远不动的假阴影。
			for index, offset := range [][2]int{{-shadowBlur, 0}, {shadowBlur, 0}, {0, -shadowBlur}, {0, shadowBlur}} {
				appendEffect(fmt.Sprintf("-shadow-blur-%02d", index), shadowX+offset[0], shadowY+offset[1], shadowColor, z-2)
			}
		}
	}
	if outlineWidth > 0 {
		outlineColor := waveshareNativeColor(waveshareOptionString(layer.Options, "outline_color", "02070EE1"), layer.Opacity)
		offsets := [][2]int{{-outlineWidth, 0}, {outlineWidth, 0}, {0, -outlineWidth}, {0, outlineWidth}}
		if outlineWidth > 1 {
			offsets = append(offsets, [2]int{-outlineWidth, -outlineWidth}, [2]int{outlineWidth, -outlineWidth}, [2]int{-outlineWidth, outlineWidth}, [2]int{outlineWidth, outlineWidth})
		}
		for index, offset := range offsets {
			appendEffect(fmt.Sprintf("-outline-%02d", index), offset[0], offset[1], outlineColor, z-1)
		}
	}
	switch waveshareFontWeight(waveshareOptionString(fontOptions, "font_weight", "bold")) {
	case "medium":
		// 中粗只叠加一层低透明度副本，与常规及粗体都保持稳定差异。
		appendEffect("-medium", 1, 0, waveshareScaleNativeColorAlpha(foreground, .42), z)
	case "bold":
		// 原厂 Qt 文字控件对于只有一张粗体字脸的字库不一定会再次加粗。
		// 用共享同一动态数据键的 1px 同色副本做形态膨胀；运行时仍只传一个歌词字符串。
		for index, offset := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
			appendEffect(fmt.Sprintf("-bold-%02d", index), offset[0], offset[1], foreground, z)
		}
	}
	widget := waveshareStyledTextWidget(template, nextID, textValue, systemName, true, x, y, size, foreground, z, "0", fontOptions)
	widget.fields["itemName"] = itemName
	widgets = append(widgets, widget)
	return widgets
}

func compileWaveshareTextLayer(template waveshareThemeWidget, layer theme.Layer, width, height int, nextID *int) []waveshareThemeWidget {
	bounds := waveshareLayerPixelRect(layer, width, height)
	textValue := waveshareOptionString(layer.Options, "text", "自定义文本")
	dataKey := waveshareOptionString(layer.Options, "data_key", "")
	dynamic := dataKey != ""
	systemName := dataKey
	if dynamic {
		spec := waveshareTelemetrySpecForKey(dataKey)
		systemName = spec.systemName
		if textValue == "" || textValue == "自定义文本" {
			textValue = spec.placeholder
		}
	}
	preferredSize := max(8, min(220, waveshareOptionInt(layer.Options, "font_size", 32)))
	textBounds := bounds
	shadowEnabled := waveshareOptionBool(layer.Options, "shadow", false)
	shadowOffset := 0
	if shadowEnabled {
		// 阴影向右下偏移 2 像素，先为它预留空间，避免主文字虽然在框内、阴影却越界。
		shadowOffset = min(2, max(0, min(bounds.width, bounds.height)-1))
		textBounds = waveshareInsetPixelRect(bounds, 0, 0, shadowOffset, shadowOffset)
	}
	fitted, ok := waveshareFitTextInRect(textBounds, textValue, preferredSize, waveshareOptionString(layer.Options, "align", "center"), "center")
	if !ok {
		return nil
	}
	colorValue := waveshareNativeColor(waveshareOptionString(layer.Options, "color", "FFFFFF"), layer.Opacity)
	z := max(1, layer.ZIndex*10)
	rotation := strconv.Itoa(int(math.Round(layer.Rotation)))
	widget := waveshareStyledTextWidget(template, nextID, fitted.text, systemName, dynamic, fitted.x, fitted.y, fitted.size, colorValue, z, rotation, layer.Options)
	widget.fields["itemName"] = layer.ID
	if shadowEnabled {
		shadow := waveshareStyledTextWidget(template, nextID, fitted.text, systemName, dynamic,
			fitted.x+shadowOffset, fitted.y+shadowOffset, fitted.size,
			waveshareNativeColor("000000", layer.Opacity*.55), max(1, z-1), rotation, layer.Options)
		shadow.fields["itemName"] = layer.ID + "-shadow"
		return []waveshareThemeWidget{shadow, widget}
	}
	return []waveshareThemeWidget{widget}
}

func compileWaveshareClockLayer(template waveshareThemeWidget, layer theme.Layer, width, height int, nextID *int) []waveshareThemeWidget {
	bounds := waveshareLayerPixelRect(layer, width, height)
	format := strings.TrimSpace(waveshareOptionString(layer.Options, "format", "HH:mm:ss"))
	preferredSize := max(8, min(220, waveshareOptionInt(layer.Options, "font_size", 44)))
	textColor := waveshareNativeColor(waveshareOptionString(layer.Options, "color", "4CE0FF"), layer.Opacity)
	z := max(1, layer.ZIndex*10)
	rotation := strconv.Itoa(int(math.Round(layer.Rotation)))
	align := waveshareOptionString(layer.Options, "align", "center")
	// 原厂的“时间”键固定含秒，HH:mm 若直接绑定该键，首次更新就会变回 HH:mm:ss。
	// 将非原子格式拆成年、月、日、时、分、秒的小型动态文本，才能忠实保持编辑器格式。
	if format == "HH:mm:ss" || format == "yyyy-MM-dd" || format == "week" {
		placeholder, systemName := "15:04:05", "时间"
		if format == "yyyy-MM-dd" {
			placeholder, systemName = "2026-08-26", "日期"
		} else if format == "week" {
			placeholder, systemName = "周三", "星期"
		}
		if fitted, ok := waveshareFitTextInRect(bounds, placeholder, preferredSize, align, "center"); ok && fitted.text == placeholder {
			widget := waveshareStyledTextWidget(template, nextID, placeholder, systemName, true, fitted.x, fitted.y, fitted.size, textColor, z, rotation, layer.Options)
			widget.fields["itemName"] = layer.ID + "-0"
			return []waveshareThemeWidget{widget}
		}
	}
	tokens := waveshareClockTokens(format)
	size, complete := waveshareFitClockTokenSize(tokens, preferredSize, bounds.width, bounds.height)
	if size == 0 {
		return nil
	}
	if !complete {
		// 极窄时不能截断动态 token，否则占位文案看似在框内，数据刷新后仍会溢出。
		// 选择第一个能完整放下的动态 token，保留有效绑定并隐藏放不下的次要部分。
		compact := make([]waveshareClockToken, 0, 1)
		for _, token := range tokens {
			if token.systemName != "" && waveshareEstimatedRenderedTextWidth(token.placeholder, size) <= bounds.width {
				compact = append(compact, token)
				break
			}
		}
		if len(compact) == 0 {
			return nil
		}
		tokens = compact
	}
	totalWidth := waveshareClockTokensRenderedWidth(tokens, size)
	x := waveshareAlignedBlockX(bounds, totalWidth, align)
	y := bounds.y + max(0, (bounds.height-waveshareRenderedTextHeight(size))/2)
	widgets := make([]waveshareThemeWidget, 0, len(tokens))
	for index, token := range tokens {
		widget := waveshareStyledTextWidget(template, nextID, token.placeholder, token.systemName, token.systemName != "", x, y, size, textColor, z+index, rotation, layer.Options)
		widget.fields["itemName"] = fmt.Sprintf("%s-%d", layer.ID, index)
		widgets = append(widgets, widget)
		x += waveshareEstimatedRenderedTextWidth(token.placeholder, size)
	}
	return widgets
}

type waveshareClockToken struct {
	placeholder string
	systemName  string
}

func waveshareClockTokens(format string) []waveshareClockToken {
	dynamic := []struct {
		format      string
		placeholder string
		systemName  string
	}{
		{format: "yyyy", placeholder: "2026", systemName: "year"},
		{format: "week", placeholder: "周三", systemName: "week"},
		{format: "MM", placeholder: "08", systemName: "month"},
		{format: "dd", placeholder: "26", systemName: "day"},
		{format: "HH", placeholder: "15", systemName: "hour"},
		{format: "mm", placeholder: "04", systemName: "minute"},
		{format: "ss", placeholder: "05", systemName: "second"},
	}
	tokens := make([]waveshareClockToken, 0, 9)
	for len(format) > 0 {
		matched := false
		for _, candidate := range dynamic {
			if strings.HasPrefix(format, candidate.format) {
				tokens = append(tokens, waveshareClockToken{placeholder: candidate.placeholder, systemName: candidate.systemName})
				format = format[len(candidate.format):]
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		runes := []rune(format)
		literal := string(runes[0])
		format = string(runes[1:])
		if len(tokens) > 0 && tokens[len(tokens)-1].systemName == "" {
			tokens[len(tokens)-1].placeholder += literal
		} else {
			tokens = append(tokens, waveshareClockToken{placeholder: literal})
		}
	}
	if len(tokens) == 0 {
		return []waveshareClockToken{{placeholder: "15:04:05", systemName: "时间"}}
	}
	return tokens
}

func waveshareClockTokensRenderedWidth(tokens []waveshareClockToken, size int) int {
	total := 0
	for _, token := range tokens {
		total += waveshareEstimatedRenderedTextWidth(token.placeholder, size)
	}
	return total
}

func waveshareFitClockTokenSize(tokens []waveshareClockToken, preferredSize, availableWidth, availableHeight int) (int, bool) {
	if availableHeight < waveshareRenderedTextHeight(waveshareMinimumTextSize) {
		return 0, false
	}
	size := min(max(waveshareMinimumTextSize, preferredSize), max(waveshareMinimumTextSize, availableHeight*3/4))
	for size > waveshareMinimumTextSize && waveshareClockTokensRenderedWidth(tokens, size) > availableWidth {
		size--
	}
	return size, waveshareClockTokensRenderedWidth(tokens, size) <= availableWidth
}

func waveshareEstimatedTextWidth(textValue string, size int) int {
	return max(max(1, size/3), len([]rune(textValue))*size*3/5)
}

type waveshareFittedText struct {
	text string
	x    int
	y    int
	size int
}

const waveshareMinimumTextSize = 8

// waveshareRenderedTextHeight 按微雪固件的字号语义估算实际像素高度。
// 固件把 text_size 解释为点数，在屏上约为 4/3 像素，不能直接用字号当高度。
func waveshareRenderedTextHeight(size int) int {
	return max(1, (max(1, size)*4+2)/3)
}

// waveshareEstimatedRenderedTextWidth 对 ASCII 与全角字符分别估宽。这里宁可
// 多留少量空白，也不能让缩小后的组件仍把文字画到图层边界外。
func waveshareEstimatedRenderedTextWidth(textValue string, size int) int {
	renderedHeight := waveshareRenderedTextHeight(size)
	units := 0
	for _, value := range []rune(textValue) {
		if value <= 0x7f {
			units += 6
		} else {
			units += 10
		}
	}
	return max(max(1, renderedHeight/3), (max(1, units)*renderedHeight+9)/10)
}

// waveshareFitRenderedTextSize 同时按可用宽高限制字号。返回 false 表示固件
// 支持的最小字号仍放不下完整文本，调用方可进一步采用省略显示或隐藏次要标签。
func waveshareFitRenderedTextSize(textValue string, preferredSize, availableWidth, availableHeight int) (int, bool) {
	availableWidth = max(1, availableWidth)
	availableHeight = max(1, availableHeight)
	if availableHeight < waveshareRenderedTextHeight(waveshareMinimumTextSize) {
		return 0, false
	}
	size := min(max(waveshareMinimumTextSize, preferredSize), max(waveshareMinimumTextSize, availableHeight*3/4))
	for size > waveshareMinimumTextSize && waveshareEstimatedRenderedTextWidth(textValue, size) > availableWidth {
		size--
	}
	return size, waveshareEstimatedRenderedTextWidth(textValue, size) <= availableWidth
}

// waveshareEllipsizeText 只改写编译结果中的占位文案，不修改图层 Options，
// 动态控件的 system_data_name 也保持不变。极窄区域至少显示一个省略标记。
func waveshareEllipsizeText(textValue string, size, availableWidth int) (string, bool) {
	if waveshareEstimatedRenderedTextWidth(textValue, size) <= availableWidth {
		return textValue, true
	}
	marker := "…"
	if waveshareEstimatedRenderedTextWidth(marker, size) > availableWidth {
		marker = "."
	}
	if waveshareEstimatedRenderedTextWidth(marker, size) > availableWidth {
		return "", false
	}
	runes := []rune(textValue)
	for count := len(runes) - 1; count >= 1; count-- {
		candidate := string(runes[:count]) + marker
		if waveshareEstimatedRenderedTextWidth(candidate, size) <= availableWidth {
			return candidate, true
		}
	}
	return marker, true
}

// waveshareFitTextInRect 在一个像素矩形内完成字号、占位文案与对齐计算。
// 只有当估算后的四边均不会越界时才返回控件布局。
func waveshareFitTextInRect(bounds wavesharePixelRect, textValue string, preferredSize int, align, verticalAlign string) (waveshareFittedText, bool) {
	size, complete := waveshareFitRenderedTextSize(textValue, preferredSize, bounds.width, bounds.height)
	if size == 0 {
		return waveshareFittedText{}, false
	}
	displayText := textValue
	if !complete {
		var ok bool
		displayText, ok = waveshareEllipsizeText(textValue, size, bounds.width)
		if !ok {
			return waveshareFittedText{}, false
		}
	}
	textWidth := waveshareEstimatedRenderedTextWidth(displayText, size)
	textHeight := waveshareRenderedTextHeight(size)
	x := bounds.x
	switch strings.ToLower(strings.TrimSpace(align)) {
	case "right":
		x += max(0, bounds.width-textWidth)
	case "center":
		x += max(0, (bounds.width-textWidth)/2)
	}
	y := bounds.y
	switch strings.ToLower(strings.TrimSpace(verticalAlign)) {
	case "bottom":
		y += max(0, bounds.height-textHeight)
	case "center":
		y += max(0, (bounds.height-textHeight)/2)
	}
	if x < bounds.x || y < bounds.y || x+textWidth > bounds.x+bounds.width || y+textHeight > bounds.y+bounds.height {
		return waveshareFittedText{}, false
	}
	return waveshareFittedText{text: displayText, x: x, y: y, size: size}, true
}

func waveshareInsetPixelRect(bounds wavesharePixelRect, left, top, right, bottom int) wavesharePixelRect {
	left = min(max(0, left), max(0, bounds.width-1))
	right = min(max(0, right), max(0, bounds.width-left-1))
	top = min(max(0, top), max(0, bounds.height-1))
	bottom = min(max(0, bottom), max(0, bounds.height-top-1))
	return wavesharePixelRect{
		x: bounds.x + left, y: bounds.y + top,
		width: max(1, bounds.width-left-right), height: max(1, bounds.height-top-bottom),
	}
}

func waveshareSplitTextRows(bounds wavesharePixelRect, count int) []wavesharePixelRect {
	if count <= 1 {
		return []wavesharePixelRect{bounds}
	}
	gap := 1
	if bounds.height >= count*waveshareRenderedTextHeight(waveshareMinimumTextSize)+count-1 {
		gap = min(3, max(1, bounds.height/20))
	}
	contentHeight := max(1, bounds.height-gap*(count-1))
	rows := make([]wavesharePixelRect, 0, count)
	y := bounds.y
	for index := 0; index < count; index++ {
		rowHeight := contentHeight / count
		if index < contentHeight%count {
			rowHeight++
		}
		rows = append(rows, wavesharePixelRect{x: bounds.x, y: y, width: bounds.width, height: max(1, rowHeight)})
		y += rowHeight + gap
	}
	return rows
}

func waveshareSplitTextColumns(bounds wavesharePixelRect, count int) []wavesharePixelRect {
	if count <= 1 {
		return []wavesharePixelRect{bounds}
	}
	gap := min(4, max(1, bounds.width/40))
	contentWidth := max(1, bounds.width-gap*(count-1))
	columns := make([]wavesharePixelRect, 0, count)
	x := bounds.x
	for index := 0; index < count; index++ {
		columnWidth := contentWidth / count
		if index < contentWidth%count {
			columnWidth++
		}
		columns = append(columns, wavesharePixelRect{x: x, y: bounds.y, width: max(1, columnWidth), height: bounds.height})
		x += columnWidth + gap
	}
	return columns
}

// waveshareFitTextPair 为“标签 + 动态值”提供一致的窄尺寸降级。空间充足时
// 同时保留两者；不足时优先保留动态值，避免两个最小字号文本互相覆盖。
func waveshareFitTextPair(bounds wavesharePixelRect, label, value string, labelPreferred, valuePreferred int, labelAlign, valueAlign string, stacked bool) (waveshareFittedText, bool, waveshareFittedText, bool) {
	if label == "" && value == "" {
		return waveshareFittedText{}, false, waveshareFittedText{}, false
	}
	if label == "" {
		valueText, valueOK := waveshareFitTextInRect(bounds, value, valuePreferred, valueAlign, "center")
		return waveshareFittedText{}, false, valueText, valueOK
	}
	if value == "" {
		labelText, labelOK := waveshareFitTextInRect(bounds, label, labelPreferred, labelAlign, "center")
		return labelText, labelOK, waveshareFittedText{}, false
	}
	minimumHeight := waveshareRenderedTextHeight(waveshareMinimumTextSize)
	if stacked && bounds.height >= minimumHeight*2+1 {
		rows := waveshareSplitTextRows(bounds, 2)
		labelText, labelOK := waveshareFitTextInRect(rows[0], label, labelPreferred, labelAlign, "center")
		valueText, valueOK := waveshareFitTextInRect(rows[1], value, valuePreferred, valueAlign, "center")
		if labelOK && valueOK {
			return labelText, true, valueText, true
		}
	}
	if !stacked || bounds.height < minimumHeight*2+1 {
		columns := waveshareSplitTextColumns(bounds, 2)
		labelText, labelOK := waveshareFitTextInRect(columns[0], label, labelPreferred, labelAlign, "center")
		valueText, valueOK := waveshareFitTextInRect(columns[1], value, valuePreferred, valueAlign, "center")
		if labelOK && valueOK {
			return labelText, true, valueText, true
		}
	}
	valueText, valueOK := waveshareFitTextInRect(bounds, value, valuePreferred, valueAlign, "center")
	if valueOK {
		return waveshareFittedText{}, false, valueText, true
	}
	labelText, labelOK := waveshareFitTextInRect(bounds, label, labelPreferred, labelAlign, "center")
	return labelText, labelOK, waveshareFittedText{}, false
}

// waveshareFitTelemetryTextSize 把遥测字号限制在单元格的可用区域内。
// 微雪固件把主题里的 text_size 当作点数，实际像素高度约为点数的 4/3；
// 同时中文、温度符号等宽字符比 ASCII 数字更宽，因此宽高都必须留出余量。
func waveshareFitTelemetryTextSize(textValue string, preferredSize, availableWidth, availableHeight int) int {
	size, _ := waveshareFitRenderedTextSize(textValue, preferredSize, availableWidth, availableHeight)
	return max(waveshareMinimumTextSize, size)
}

func waveshareTelemetryRenderedFontHeight(size int) int {
	return waveshareRenderedTextHeight(size)
}

func waveshareEstimatedTelemetryTextWidth(textValue string, size int) int {
	return waveshareEstimatedRenderedTextWidth(textValue, size)
}

func compileWaveshareProgressLayer(textTemplate, progressTemplate waveshareThemeWidget, layer theme.Layer, width, height int, nextID *int) []waveshareThemeWidget {
	bounds := waveshareLayerPixelRect(layer, width, height)
	spec := waveshareTelemetrySpecForKey(waveshareOptionString(layer.Options, "data_key", "Cpu_Freq"))
	if !waveshareTelemetryNumeric(spec.key) {
		return compileWaveshareScalarTextFallback(textTemplate, layer, spec, bounds, nextID)
	}
	numericSystemName := waveshareNumericSystemName(spec)
	minimum := waveshareOptionFloat(layer.Options, "min_value", 0)
	maximum := waveshareOptionFloat(layer.Options, "max_value", 100)
	if maximum <= minimum {
		maximum = minimum + 1
	}
	foreground := waveshareNativeColor(waveshareOptionString(layer.Options, "color", "4CE0FF"), layer.Opacity)
	background := waveshareNativeColor(waveshareOptionString(layer.Options, "secondary_color", "183740"), layer.Opacity*.78)
	corner := max(0, waveshareOptionInt(layer.Options, "corner_radius", 8))
	style := strings.ToLower(waveshareOptionString(layer.Options, "style", "rounded"))
	if style == "square" {
		corner = 0
	} else if style == "dots" {
		corner = max(1, min(bounds.width, bounds.height)/2)
	}
	orientation := strings.ToLower(waveshareOptionString(layer.Options, "orientation", "horizontal"))
	x, y, widgetWidth, widgetHeight, rotation := bounds.x, bounds.y, bounds.width, bounds.height, int(math.Round(layer.Rotation))
	if orientation == "vertical" {
		y = bounds.y + bounds.height
		widgetWidth, widgetHeight, rotation = bounds.height, bounds.width, rotation-90
	} else if orientation == "reverse" {
		x = bounds.x + bounds.width
		y = bounds.y + bounds.height
		rotation += 180
	}
	z := max(1, layer.ZIndex*10)
	widgets := make([]waveshareThemeWidget, 0, 14)
	if style == "segmented" || style == "dots" {
		axisLength := bounds.width
		if orientation == "vertical" {
			axisLength = bounds.height
		}
		segments := max(1, min(min(16, waveshareOptionInt(layer.Options, "segments", 10)), axisLength))
		segmentLength, gap := wavesharePackedLinearLayout(axisLength, segments, max(1, min(6, axisLength/max(1, segments*8))))
		valueSpan := (maximum - minimum) / float64(segments)
		for index := 0; index < segments; index++ {
			segmentCorner := 0
			if style == "dots" {
				thickness := bounds.height
				if orientation == "vertical" {
					thickness = bounds.width
				}
				segmentCorner = max(1, min(segmentLength, thickness)/2)
			}
			segmentX, segmentY := bounds.x+index*(segmentLength+gap), bounds.y
			segmentWidth, segmentHeight, segmentRotation := segmentLength, bounds.height, int(math.Round(layer.Rotation))
			switch orientation {
			case "vertical":
				segmentX = bounds.x
				segmentY = bounds.y + bounds.height - index*(segmentLength+gap)
				segmentWidth, segmentHeight, segmentRotation = segmentLength, bounds.width, int(math.Round(layer.Rotation))-90
			case "reverse":
				segmentX = bounds.x + bounds.width - index*(segmentLength+gap)
				segmentY = bounds.y + bounds.height
				segmentWidth, segmentHeight, segmentRotation = segmentLength, bounds.height, int(math.Round(layer.Rotation))+180
			}
			segmentMinimum := minimum + valueSpan*float64(index)
			segmentMaximum := minimum + valueSpan*float64(index+1)
			widget := waveshareProgressWidget(progressTemplate, nextID, fmt.Sprintf("%s-%02d", layer.ID, index), numericSystemName, segmentX, segmentY, segmentWidth, segmentHeight, segmentRotation, z, foreground, background, segmentCorner, segmentMinimum, segmentMaximum)
			waveshareClampRotatedWidgetToRect(&widget, bounds)
			widgets = append(widgets, widget)
		}
	} else {
		widget := waveshareProgressWidget(progressTemplate, nextID, layer.ID, numericSystemName, x, y, widgetWidth, widgetHeight, rotation, z, foreground, background, corner, minimum, maximum)
		switch style {
		case "neon":
			widget.fields["lineargradient_flag"] = "1"
			widget.fields["lineargradient_color"] = waveshareNativeColor("FFFFFF", layer.Opacity)
			widget.fields["border_width"] = "1"
			widget.fields["border_color"] = foreground
		case "outline":
			widget.fields["back_color"] = "r=0,g=0,b=0,a=0"
			widget.fields["border_width"] = "2"
			widget.fields["border_color"] = foreground
		}
		waveshareClampRotatedWidgetToRect(&widget, bounds)
		widgets = append(widgets, widget)
	}
	textRotation := strconv.Itoa(int(math.Round(layer.Rotation)))
	showLabel := waveshareOptionBool(layer.Options, "show_label", true)
	showValue := waveshareOptionBool(layer.Options, "show_value", true)
	labelText, valueText := "", ""
	if showLabel {
		labelText = spec.label
	}
	if showValue {
		valueText = spec.placeholder
	}
	horizontalPadding := min(5, max(1, bounds.width/24))
	verticalPadding := 0
	if bounds.height >= 40 {
		verticalPadding = min(3, bounds.height/20)
	}
	textBounds := waveshareInsetPixelRect(bounds, horizontalPadding, verticalPadding, horizontalPadding, verticalPadding)
	preferredSize := waveshareOptionInt(layer.Options, "font_size", max(9, min(18, bounds.height/3)))
	minimumPairWidth := waveshareEstimatedRenderedTextWidth(labelText, waveshareMinimumTextSize) + waveshareEstimatedRenderedTextWidth(valueText, waveshareMinimumTextSize) + 4
	stacked := showLabel && showValue && textBounds.width < minimumPairWidth
	labelLayout, labelOK, valueLayout, valueOK := waveshareFitTextPair(textBounds, labelText, valueText, preferredSize, preferredSize, "left", "right", stacked)
	if labelOK {
		widget := waveshareStyledTextWidget(textTemplate, nextID, labelLayout.text, "", false, labelLayout.x, labelLayout.y, labelLayout.size, foreground, z+1, textRotation, layer.Options)
		widget.fields["itemName"] = layer.ID + "-label"
		widgets = append(widgets, widget)
	}
	if valueOK {
		widget := waveshareStyledTextWidget(textTemplate, nextID, valueLayout.text, spec.systemName, true, valueLayout.x, valueLayout.y, valueLayout.size, foreground, z+2, textRotation, layer.Options)
		widget.fields["itemName"] = layer.ID + "-value"
		widgets = append(widgets, widget)
	}
	return widgets
}

func compileWaveshareGaugeLayer(textTemplate, gaugeTemplate waveshareThemeWidget, layer theme.Layer, width, height int, nextID *int) []waveshareThemeWidget {
	bounds := waveshareLayerPixelRect(layer, width, height)
	spec := waveshareTelemetrySpecForKey(waveshareOptionString(layer.Options, "data_key", "Cpu_Freq"))
	if !waveshareTelemetryNumeric(spec.key) {
		return compileWaveshareScalarTextFallback(textTemplate, layer, spec, bounds, nextID)
	}
	minimum := waveshareOptionFloat(layer.Options, "min_value", 0)
	maximum := waveshareOptionFloat(layer.Options, "max_value", 100)
	if maximum <= minimum {
		maximum = minimum + 1
	}
	diameter := max(2, min(bounds.width, bounds.height))
	radius := max(1, diameter/2)
	margin := max(1, waveshareOptionInt(layer.Options, "stroke_width", 8))
	foreground := waveshareNativeColor(waveshareOptionString(layer.Options, "color", "4CE0FF"), layer.Opacity)
	background := waveshareNativeColor(waveshareOptionString(layer.Options, "secondary_color", "183740"), layer.Opacity*.78)
	x := bounds.x + (bounds.width-diameter)/2
	y := bounds.y + (bounds.height-diameter)/2
	z := max(1, layer.ZIndex*10)
	style := strings.ToLower(waveshareOptionString(layer.Options, "style", "arc"))
	widgets := make([]waveshareThemeWidget, 0, 3)
	if style == "ring" || style == "minimal" {
		if style == "minimal" {
			margin = max(1, margin/2)
		}
		margin = min(margin, radius)
		widgets = append(widgets, waveshareGaugeWidget(gaugeTemplate, nextID, layer.ID, waveshareNumericSystemName(spec), x, y, radius, margin, z, foreground, background, minimum, maximum))
	} else {
		startAngle := waveshareOptionFloat(layer.Options, "start_angle", -140)
		endAngle := waveshareOptionFloat(layer.Options, "end_angle", 140)
		interval := 4
		switch style {
		case "semicircle":
			startAngle, endAngle = 180, 360
		case "dial":
			interval = 2
		case "ticks":
			interval = 12
		}
		widgets = append(widgets, waveshareArcGaugeWidget(nextID, layer.ID, waveshareNumericSystemName(spec), x, y, radius, min(margin, radius), z, foreground, foreground, foreground, startAngle, endAngle, interval, minimum, maximum))
	}
	widgets[0].fields["rotate"] = strconv.Itoa(int(math.Round(layer.Rotation)))
	return append(widgets, compileWaveshareGaugeLabels(textTemplate, layer, spec, bounds, z+1, nextID)...)
}

func compileWaveshareGradientGaugeLayer(textTemplate waveshareThemeWidget, layer theme.Layer, width, height int, nextID *int) []waveshareThemeWidget {
	bounds := waveshareLayerPixelRect(layer, width, height)
	spec := waveshareTelemetrySpecForKey(waveshareOptionString(layer.Options, "data_key", "Gpu_Load"))
	if !waveshareTelemetryNumeric(spec.key) {
		return compileWaveshareScalarTextFallback(textTemplate, layer, spec, bounds, nextID)
	}
	minimum := waveshareOptionFloat(layer.Options, "min_value", 0)
	maximum := waveshareOptionFloat(layer.Options, "max_value", 100)
	if maximum <= minimum {
		maximum = minimum + 1
	}
	radius := max(1, min(bounds.width, bounds.height)/2)
	style := strings.ToLower(waveshareOptionString(layer.Options, "style", "arc"))
	startAngle := waveshareOptionFloat(layer.Options, "start_angle", -140)
	endAngle := waveshareOptionFloat(layer.Options, "end_angle", 140)
	interval := 4
	strokeWidth := min(radius, max(1, waveshareOptionInt(layer.Options, "stroke_width", 9)))
	switch style {
	case "semicircle":
		startAngle, endAngle = 180, 360
	case "ring":
		startAngle, endAngle, interval = -180, 180, 0
	case "minimal":
		interval = 0
		strokeWidth = max(1, strokeWidth/2)
	case "dial":
		interval = 2
	case "ticks":
		interval = 12
	}
	widget := waveshareArcGaugeWidget(nextID, layer.ID, waveshareNumericSystemName(spec),
		bounds.x+(bounds.width-radius*2)/2, bounds.y+(bounds.height-radius*2)/2,
		radius, strokeWidth, max(1, layer.ZIndex*10),
		waveshareNativeColor(waveshareOptionString(layer.Options, "gradient_start", "4CE0FF"), layer.Opacity),
		waveshareNativeColor(waveshareOptionString(layer.Options, "gradient_mid", "FFF275"), layer.Opacity),
		waveshareNativeColor(waveshareOptionString(layer.Options, "gradient_end", "FF5C68"), layer.Opacity),
		startAngle, endAngle, interval, minimum, maximum)
	widget.fields["rotate"] = strconv.Itoa(int(math.Round(layer.Rotation)))
	z := max(1, layer.ZIndex*10)
	widgets := []waveshareThemeWidget{widget}
	return append(widgets, compileWaveshareGaugeLabels(textTemplate, layer, spec, bounds, z+1, nextID)...)
}

func waveshareArcGaugeWidget(nextID *int, itemName, systemName string, x, y, radius, strokeWidth, z int, firstColor, middleColor, lastColor string, startAngle, endAngle float64, interval int, minimum, maximum float64) waveshareThemeWidget {
	angleMinimum, angleMaximum := waveshareFirmwareArcAngles(startAngle, endAngle)
	widget := waveshareThemeWidget{fields: map[string]string{
		"id": strconv.Itoa(*nextID), "itemName": itemName, "type": "109",
		"x": strconv.Itoa(x), "y": strconv.Itoa(y), "z": strconv.Itoa(max(1, z)), "rotate": "0", "scale": "1", "lock": "1",
		"radius": strconv.Itoa(max(1, radius)), "arcRadius": strconv.Itoa(max(1, min(strokeWidth, radius))), "arcCircularInterval": strconv.Itoa(max(0, interval)),
		"angleMinValue": waveshareFormatNumber(angleMinimum), "angleMaxValue": waveshareFormatNumber(angleMaximum),
		"gradientColor1": firstColor, "gradientColor2": middleColor, "gradientColor3": lastColor,
		"system_data_flag": "1", "system_data_name": systemName,
		"system_data_min_value": waveshareFormatNumber(minimum), "system_data_max_value": waveshareFormatNumber(maximum),
	}}
	*nextID++
	return widget
}

// waveshareFirmwareArcAngles 把编辑器的“0° 朝上”圆弧端点转成固件使用的 Qt 角度。
// 原厂 ±135° 仪表会写成 angleMinValue=225、angleMaxValue=315。
func waveshareFirmwareArcAngles(startAngle, endAngle float64) (float64, float64) {
	first := waveshareNormalizedAngle(90 - startAngle)
	second := waveshareNormalizedAngle(90 - endAngle)
	if first > second {
		first, second = second, first
	}
	return first, second
}

func compileWaveshareGaugeLabels(textTemplate waveshareThemeWidget, layer theme.Layer, spec waveshareTelemetrySpec, bounds wavesharePixelRect, z int, nextID *int) []waveshareThemeWidget {
	widgets := make([]waveshareThemeWidget, 0, 2)
	fontSize := max(10, min(96, waveshareOptionInt(layer.Options, "font_size", 22)))
	foreground := waveshareNativeColor(waveshareOptionString(layer.Options, "color", waveshareOptionString(layer.Options, "gradient_start", "F5FBFF")), layer.Opacity)
	rotation := strconv.Itoa(int(math.Round(layer.Rotation)))
	labelText, valueText := "", ""
	if waveshareOptionBool(layer.Options, "show_label", true) {
		labelText = spec.label
	}
	if waveshareOptionBool(layer.Options, "show_value", true) {
		valueText = spec.placeholder
	}
	padding := min(3, max(0, bounds.width/40))
	textBounds := waveshareInsetPixelRect(bounds, padding, 0, padding, 0)
	labelLayout, labelOK, valueLayout, valueOK := waveshareFitTextPair(textBounds, labelText, valueText, max(9, fontSize-5), fontSize, "center", "center", true)
	if labelOK {
		widget := waveshareStyledTextWidget(textTemplate, nextID, labelLayout.text, "", false, labelLayout.x, labelLayout.y, labelLayout.size, foreground, z, rotation, layer.Options)
		widget.fields["itemName"] = layer.ID + "-label"
		widgets = append(widgets, widget)
	}
	if valueOK {
		widget := waveshareStyledTextWidget(textTemplate, nextID, valueLayout.text, spec.systemName, true, valueLayout.x, valueLayout.y, valueLayout.size, foreground, z+1, rotation, layer.Options)
		widget.fields["itemName"] = layer.ID + "-value"
		widgets = append(widgets, widget)
	}
	return widgets
}

// compileWaveshareScalarTextFallback 在用户把型号、日期等非数值键绑到进度/仪表/曲线时，
// 降级为可更新的驻留文本，避免固件把字符串当数字解析后永远显示 0。
func compileWaveshareScalarTextFallback(textTemplate waveshareThemeWidget, layer theme.Layer, spec waveshareTelemetrySpec, bounds wavesharePixelRect, nextID *int) []waveshareThemeWidget {
	z := max(1, layer.ZIndex*10)
	fontSize := max(10, min(96, waveshareOptionInt(layer.Options, "font_size", min(32, bounds.height/3))))
	colorValue := waveshareNativeColor(waveshareOptionString(layer.Options, "color", waveshareOptionString(layer.Options, "gradient_start", "4CE0FF")), layer.Opacity)
	rotation := strconv.Itoa(int(math.Round(layer.Rotation)))
	widgets := make([]waveshareThemeWidget, 0, 2)
	showLabel := waveshareOptionBool(layer.Options, "show_label", true)
	showValue := waveshareOptionBool(layer.Options, "show_value", true) || !showLabel
	labelText, valueText := "", ""
	if showLabel {
		labelText = spec.label
	}
	if showValue {
		valueText = spec.placeholder
	}
	padding := min(3, max(0, bounds.width/40))
	textBounds := waveshareInsetPixelRect(bounds, padding, 0, padding, 0)
	labelLayout, labelOK, valueLayout, valueOK := waveshareFitTextPair(textBounds, labelText, valueText, max(9, fontSize-4), fontSize, "center", "center", true)
	if labelOK {
		label := waveshareStyledTextWidget(textTemplate, nextID, labelLayout.text, "", false, labelLayout.x, labelLayout.y, labelLayout.size, colorValue, z, rotation, layer.Options)
		label.fields["itemName"] = layer.ID + "-label"
		widgets = append(widgets, label)
	}
	if valueOK {
		value := waveshareStyledTextWidget(textTemplate, nextID, valueLayout.text, spec.systemName, true, valueLayout.x, valueLayout.y, valueLayout.size, colorValue, z+1, rotation, layer.Options)
		value.fields["itemName"] = layer.ID
		widgets = append(widgets, value)
	}
	return widgets
}

func compileWaveshareLineChartLayer(textTemplate waveshareThemeWidget, layer theme.Layer, width, height int, nextID *int) []waveshareThemeWidget {
	bounds := waveshareLayerPixelRect(layer, width, height)
	spec := waveshareTelemetrySpecForKey(waveshareOptionString(layer.Options, "data_key", "Cpu_Freq"))
	if !waveshareTelemetryNumeric(spec.key) {
		return compileWaveshareScalarTextFallback(textTemplate, layer, spec, bounds, nextID)
	}
	style := strings.ToLower(waveshareOptionString(layer.Options, "style", "area"))
	lineColor := waveshareNativeColor(waveshareOptionString(layer.Options, "color", "4CE0FF"), layer.Opacity)
	fillColor := waveshareNativeColor(waveshareOptionString(layer.Options, "fill_color", "4CE0FF38"), layer.Opacity)
	gridColor := waveshareNativeColor(waveshareOptionString(layer.Options, "grid_color", "52727E66"), layer.Opacity)
	areaEnabled := style == "area" || style == "neon"
	gridEnabled := waveshareOptionBool(layer.Options, "show_grid", true)
	switch style {
	case "spline":
		gridEnabled = false
	case "dots":
		gridEnabled = false
		fillColor = "r=0,g=0,b=0,a=0"
	case "neon":
		gridEnabled = false
		fillColor = waveshareNativeColor(waveshareOptionString(layer.Options, "fill_color", "4CE0FF66"), layer.Opacity)
	}
	minimum := waveshareOptionFloat(layer.Options, "min_value", 0)
	maximum := waveshareOptionFloat(layer.Options, "max_value", 100)
	if maximum <= minimum {
		maximum = minimum + 1
	}
	widget := waveshareThemeWidget{fields: map[string]string{
		"id": strconv.Itoa(*nextID), "itemName": layer.ID, "type": "106",
		"x": strconv.Itoa(bounds.x), "y": strconv.Itoa(bounds.y), "w": strconv.Itoa(bounds.width), "h": strconv.Itoa(bounds.height),
		"z": strconv.Itoa(max(1, layer.ZIndex*10)), "rotate": strconv.Itoa(int(math.Round(layer.Rotation))), "scale": "1", "lock": "1",
		"series_color": lineColor, "area_flag": map[bool]string{true: "1", false: "0"}[areaEnabled], "area_color": fillColor,
		"grid_line_flag": map[bool]string{true: "1", false: "0"}[gridEnabled], "grid_line_color": gridColor,
		"system_data_flag": "1", "system_data_name": waveshareNumericSystemName(spec),
		"system_data_min_value": waveshareFormatNumber(minimum),
		"system_data_max_value": waveshareFormatNumber(maximum),
	}}
	*nextID++
	widgets := []waveshareThemeWidget{widget}
	if waveshareOptionBool(layer.Options, "show_label", true) {
		fontSize := waveshareOptionInt(layer.Options, "font_size", max(10, min(20, bounds.height/6)))
		rotation := strconv.Itoa(int(math.Round(layer.Rotation)))
		horizontalPadding := min(6, max(1, bounds.width/24))
		textBounds := waveshareInsetPixelRect(bounds, horizontalPadding, 0, horizontalPadding, 0)
		minimumPairWidth := waveshareEstimatedRenderedTextWidth(spec.label, waveshareMinimumTextSize) + waveshareEstimatedRenderedTextWidth(spec.placeholder, waveshareMinimumTextSize) + 4
		stacked := textBounds.width < minimumPairWidth
		labelLayout, labelOK, valueLayout, valueOK := waveshareFitTextPair(textBounds, spec.label, spec.placeholder, fontSize, fontSize, "left", "right", stacked)
		if labelOK {
			label := waveshareStyledTextWidget(textTemplate, nextID, labelLayout.text, "", false, labelLayout.x, labelLayout.y, labelLayout.size, lineColor, max(1, layer.ZIndex*10)+1, rotation, layer.Options)
			label.fields["itemName"] = layer.ID + "-label"
			widgets = append(widgets, label)
		}
		if valueOK {
			value := waveshareStyledTextWidget(textTemplate, nextID, valueLayout.text, spec.systemName, true, valueLayout.x, valueLayout.y, valueLayout.size, lineColor, max(1, layer.ZIndex*10)+2, rotation, layer.Options)
			value.fields["itemName"] = layer.ID + "-value"
			widgets = append(widgets, value)
		}
	}
	return widgets
}

func waveshareTelemetrySpecForKey(key string) waveshareTelemetrySpec {
	for _, spec := range waveshareTelemetrySpecs {
		if spec.key == key {
			return spec
		}
	}
	return waveshareTelemetrySpec{key: key, label: key, systemName: key, placeholder: "0", color: "r=76,g=224,b=255,a=255"}
}

func waveshareNormalizedAngle(value float64) float64 {
	value = math.Mod(value, 360)
	if value < 0 {
		value += 360
	}
	return value
}

func waveshareAlignedTextX(bounds wavesharePixelRect, textValue string, size int, align string) int {
	estimatedWidth := max(size, waveshareEstimatedTextWidth(textValue, size))
	return waveshareAlignedBlockX(bounds, estimatedWidth, align)
}

func waveshareAlignedBlockX(bounds wavesharePixelRect, estimatedWidth int, align string) int {
	maximumX := bounds.x + max(0, bounds.width-1)
	switch strings.ToLower(strings.TrimSpace(align)) {
	case "right":
		return min(maximumX, bounds.x+max(0, bounds.width-estimatedWidth-min(4, max(0, bounds.width-1))))
	case "center":
		return min(maximumX, bounds.x+max(0, (bounds.width-estimatedWidth)/2))
	default:
		return min(maximumX, bounds.x+min(4, max(0, bounds.width-1)))
	}
}

func waveshareStyledTextWidget(template waveshareThemeWidget, nextID *int, textValue, systemName string, dynamic bool, x, y, size int, colorValue string, z int, rotation string, options map[string]any) waveshareThemeWidget {
	widget := waveshareTextWidget(template, nextID, textValue, systemName, dynamic, x, y, size, colorValue, z, rotation)
	waveshareApplyFontOptions(&widget, options)
	return widget
}

func waveshareApplyFontOptions(widget *waveshareThemeWidget, options map[string]any) {
	family := strings.ToLower(waveshareOptionString(options, "font_family", "source-han"))
	weight := waveshareFontWeight(waveshareOptionString(options, "font_weight", "medium"))
	fontPath, fontFamily, bold := waveshareResidentFont(family, weight)
	widget.fields["text_customFont_flag"] = "1"
	widget.fields["text_customFont_path"] = fontPath
	widget.fields["text_family"] = fontFamily
	if bold {
		widget.fields["text_IsBold"] = "1"
	} else {
		widget.fields["text_IsBold"] = "0"
	}
	if weight == "light" {
		// 少数字体族在原厂 TF 系统中只有常规/粗体。将主笔画透明度
		// 有界降低，作为不会破坏字形的细体后备，并保证与 regular 可区分。
		widget.fields["front_color"] = waveshareScaleNativeColorAlpha(widget.fields["front_color"], .68)
	}
}

// waveshareResidentFont 把编辑器的逻辑字体与字重落到两张原厂镜像共同
// 具备的真实文件。已逐项只读核对 7 英寸和 8.8 英寸 rootfs；因此无需为
// 字体再修改镜像，也不会在应用主题时上传几十 MB 字库。
func waveshareResidentFont(family, weight string) (fontPath, fontFamily string, synthesizeBold bool) {
	// 两张原厂镜像都在 TF 系统的 /usr/share/fonts 内预装了这些字体。必须启用
	// customFont 并给出屏内绝对路径；若回退到 Windows 字体族名，主题虽然能
	// 载入，文字却可能整体不可见。不能使用原厂主题的 /fonts 路径：7 英寸卡
	// 有庞门/LED，而 8.8 英寸卡只有 Oswald，两者并不通用。
	fontPath = "/usr/share/fonts/Source San Hans BLOD.ttf"
	fontFamily = "Source Han Sans CN"
	switch family {
	case "pangmen":
		switch weight {
		case "light":
			fontPath, fontFamily = "/usr/share/fonts/Dengl.ttf", "DengXian Light"
		case "regular":
			fontPath, fontFamily = "/usr/share/fonts/Deng.ttf", "DengXian"
		default:
			fontPath, fontFamily = "/usr/share/fonts/Dengb.ttf", "DengXian"
		}
	case "source-han":
		// 两张原厂镜像都只有这张 Source Han 粗体字脸。四档继续
		// 共用真实驻留字脸，light 由 waveshareApplyFontOptions 有界降低
		// 覆盖率，medium/bold 由共享数据键的 1px 副本分档；不得回退
		// 微软雅黑，否则字体族选择在 Light/Regular 下会失效。
		fontPath, fontFamily = "/usr/share/fonts/Source San Hans BLOD.ttf", "Source Han Sans CN"
	case "microsoft-yahei":
		switch weight {
		case "light":
			fontPath, fontFamily = "/usr/share/fonts/msyhl.ttc", "Microsoft YaHei Light"
		case "regular":
			fontPath, fontFamily = "/usr/share/fonts/msyh.ttc", "Microsoft YaHei"
		default:
			fontPath, fontFamily = "/usr/share/fonts/msyhbd.ttc", "Microsoft YaHei"
		}
	case "simhei":
		fontPath, fontFamily = "/usr/share/fonts/simhei.ttf", "SimHei"
	case "digital":
		switch weight {
		case "light":
			fontPath = "/usr/share/fonts/LiquidCrystal-Light.otf"
		case "regular":
			fontPath = "/usr/share/fonts/LiquidCrystal-Normal.otf"
		case "medium":
			fontPath = "/usr/share/fonts/LiquidCrystal-Bold.otf"
		case "bold":
			fontPath = "/usr/share/fonts/LiquidCrystal-ExBold.otf"
		}
		fontFamily = "Liquid Crystal"
	case "monospace":
		switch weight {
		case "light":
			fontPath, fontFamily = "/usr/share/fonts/CascadiaMono.ttf", "Cascadia Mono"
		case "regular":
			fontPath, fontFamily = "/usr/share/fonts/consola.ttf", "Consolas"
		default:
			fontPath, fontFamily = "/usr/share/fonts/consolab.ttf", "Consolas"
		}
	case "rounded":
		switch weight {
		case "light":
			fontPath = "/usr/share/fonts/YuGothL.ttc"
			fontFamily = "Yu Gothic Light"
		case "regular":
			fontPath = "/usr/share/fonts/YuGothR.ttc"
			fontFamily = "Yu Gothic"
		case "medium":
			fontPath = "/usr/share/fonts/YuGothM.ttc"
			fontFamily = "Yu Gothic Medium"
		case "bold":
			fontPath = "/usr/share/fonts/YuGothB.ttc"
			fontFamily = "Yu Gothic"
		}
	case "serif":
		if weight == "medium" || weight == "bold" {
			fontPath, fontFamily = "/usr/share/fonts/simsunb.ttf", "SimSun"
		} else {
			fontPath, fontFamily = "/usr/share/fonts/simsun.ttc", "SimSun"
		}
	}
	// 真实中粗字脸不重复请求 Qt 合成；粗体额外开启 Qt 标志，
	// medium/bold 的进一步可见差异由 waveshareWeightTextCopies 保底。
	return fontPath, fontFamily, weight == "bold"
}

// waveshareWeightTextCopies 为非歌词文字生成有界 1px 形态膨胀。medium
// 只加一层半透明右移副本，bold 增加横纵两层；动态副本共享原
// system_data_name，日常仍只发送一份小型键值数据。
func waveshareWeightTextCopies(widgets []waveshareThemeWidget, weight string, nextID *int) []waveshareThemeWidget {
	type copyPass struct {
		suffix     string
		offsetX    int
		offsetY    int
		alphaScale float64
	}
	var passes []copyPass
	switch waveshareFontWeight(weight) {
	case "medium":
		passes = []copyPass{{suffix: "-medium", offsetX: 1, alphaScale: .42}}
	case "bold":
		passes = []copyPass{{suffix: "-bold-x", offsetX: 1, alphaScale: .82}, {suffix: "-bold-y", offsetY: 1, alphaScale: .54}}
	default:
		return nil
	}
	result := make([]waveshareThemeWidget, 0, len(widgets)*len(passes))
	for _, widget := range widgets {
		if widget.fields["type"] != "8" || strings.HasSuffix(widget.fields["itemName"], "-shadow") {
			continue
		}
		for _, pass := range passes {
			copyWidget := cloneWaveshareThemeWidget(widget)
			copyWidget.fields["id"] = strconv.Itoa(*nextID)
			*nextID++
			copyWidget.fields["itemName"] = widget.fields["itemName"] + pass.suffix
			x, _ := strconv.Atoi(widget.fields["x"])
			y, _ := strconv.Atoi(widget.fields["y"])
			copyWidget.fields["x"] = strconv.Itoa(x + pass.offsetX)
			copyWidget.fields["y"] = strconv.Itoa(y + pass.offsetY)
			copyWidget.fields["front_color"] = waveshareScaleNativeColorAlpha(copyWidget.fields["front_color"], pass.alphaScale)
			result = append(result, copyWidget)
		}
	}
	return result
}

func waveshareFontWeight(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "light", "300":
		return "light"
	case "regular", "normal", "400":
		return "regular"
	case "bold", "700", "800", "900":
		return "bold"
	default:
		return "medium"
	}
}

// waveshareLyricsFontOptions 保证动态歌词一定使用覆盖中文、日文和韩文的
// 屏内字体。数码和等宽字体可用于英文遥测，但不能依赖 Qt 在运行时为中文
// 临时寻找 fallback；原厂精简系统的 fallback 顺序不稳定，常表现为缺字或
// 笔画过细。这里不修改用户清单，只为歌词控件复制一份安全编译选项。
func waveshareLyricsFontOptions(options map[string]any) map[string]any {
	result := make(map[string]any, len(options)+2)
	for key, value := range options {
		result[key] = value
	}
	family := strings.ToLower(waveshareOptionString(options, "font_family", "source-han"))
	if family == "digital" || family == "monospace" {
		result["font_family"] = "source-han"
	}
	if strings.TrimSpace(waveshareOptionString(options, "font_weight", "")) == "" {
		result["font_weight"] = "bold"
	}
	return result
}

func waveshareTextWidget(template waveshareThemeWidget, nextID *int, text, systemName string, dynamic bool, x, y, size int, color string, z int, rotation string) waveshareThemeWidget {
	widget := cloneWaveshareThemeWidget(template)
	widget.fields["id"] = strconv.Itoa(*nextID)
	*nextID++
	widget.fields["itemName"] = fmt.Sprintf("casescreen-%d", *nextID)
	widget.fields["x"] = strconv.Itoa(max(0, x))
	widget.fields["y"] = strconv.Itoa(max(0, y))
	widget.fields["z"] = strconv.Itoa(max(1, z))
	widget.fields["rotate"] = rotation
	widget.fields["scale"] = "1"
	widget.fields["lock"] = "1"
	widget.fields["text_size"] = strconv.Itoa(max(8, min(220, size)))
	widget.fields["text_str"] = text
	widget.fields["front_color"] = color
	if dynamic {
		widget.fields["system_data_flag"] = "1"
		widget.fields["system_data_name"] = systemName
	} else {
		widget.fields["system_data_flag"] = "0"
		widget.fields["system_data_name"] = ""
	}
	delete(widget.fields, "system_data_type")
	return widget
}

func findWaveshareWidget(widgets []waveshareThemeWidget, match func(map[string]string) bool) (waveshareThemeWidget, bool) {
	for _, widget := range widgets {
		if match(widget.fields) {
			return widget, true
		}
	}
	return waveshareThemeWidget{}, false
}

func waveshareLayerPixelRect(layer theme.Layer, width, height int) wavesharePixelRect {
	width, height = max(1, width), max(1, height)
	x := max(0, min(width-1, int(math.Round(layer.Bounds.X*float64(width)))))
	y := max(0, min(height-1, int(math.Round(layer.Bounds.Y*float64(height)))))
	return wavesharePixelRect{
		x:      x,
		y:      y,
		width:  max(1, min(width-x, int(math.Round(layer.Bounds.Width*float64(width))))),
		height: max(1, min(height-y, int(math.Round(layer.Bounds.Height*float64(height))))),
	}
}

func waveshareOptionString(options map[string]any, key, fallback string) string {
	if options != nil {
		if value, ok := options[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return fallback
}

func waveshareOptionInt(options map[string]any, key string, fallback int) int {
	if options == nil {
		return fallback
	}
	switch value := options[key].(type) {
	case int:
		return value
	case float64:
		return int(math.Round(value))
	case string:
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return fallback
}

func waveshareOptionFloat(options map[string]any, key string, fallback float64) float64 {
	if options == nil {
		return fallback
	}
	switch value := options[key].(type) {
	case float64:
		return value
	case float32:
		return float64(value)
	case int:
		return float64(value)
	case string:
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed
		}
	}
	return fallback
}

func waveshareOptionBool(options map[string]any, key string, fallback bool) bool {
	if options == nil {
		return fallback
	}
	switch value := options[key].(type) {
	case bool:
		return value
	case string:
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	case float64:
		return value != 0
	case int:
		return value != 0
	}
	return fallback
}

func waveshareNativeColor(value string, opacity float64) string {
	value = strings.TrimPrefix(strings.TrimSpace(value), "#")
	alphaValue := 255
	if len(value) == 8 {
		if parsed, err := strconv.ParseUint(value[6:8], 16, 8); err == nil {
			alphaValue = int(parsed)
		}
		value = value[:6]
	}
	if len(value) != 6 {
		value = "4CE0FF"
	}
	parse := func(text string) int {
		result, err := strconv.ParseUint(text, 16, 8)
		if err != nil {
			return 255
		}
		return int(result)
	}
	alpha := int(math.Round(math.Max(0, math.Min(1, opacity)) * float64(alphaValue)))
	return fmt.Sprintf("r=%d,g=%d,b=%d,a=%d", parse(value[0:2]), parse(value[2:4]), parse(value[4:6]), alpha)
}

func waveshareScaleNativeColorAlpha(value string, scale float64) string {
	var red, green, blue, alpha int
	if _, err := fmt.Sscanf(value, "r=%d,g=%d,b=%d,a=%d", &red, &green, &blue, &alpha); err != nil {
		return value
	}
	scale = math.Max(0, math.Min(1, scale))
	alpha = int(math.Round(float64(max(0, min(255, alpha))) * scale))
	return fmt.Sprintf("r=%d,g=%d,b=%d,a=%d", max(0, min(255, red)), max(0, min(255, green)), max(0, min(255, blue)), alpha)
}

// drawWaveshareComponentShells 把编辑器中的面板与微雪不具备原生等价物的装饰绘入驻留背景。
// 数值文本和条状频谱仍由屏端原生控件覆盖，因此日常更新只需要小型数据包。
func drawWaveshareComponentShells(canvas *image.RGBA, manifest theme.Manifest) {
	layers := append([]theme.Layer(nil), manifest.Layers...)
	sort.SliceStable(layers, func(left, right int) bool { return layers[left].ZIndex < layers[right].ZIndex })
	for _, layer := range layers {
		if !theme.LayerVisible(layer) || layer.Opacity <= 0 {
			continue
		}
		bounds := waveshareLayerPixelRect(layer, manifest.Width, manifest.Height)
		rectangle := image.Rect(bounds.x, bounds.y, bounds.x+bounds.width, bounds.y+bounds.height).Intersect(canvas.Bounds())
		if rectangle.Empty() {
			continue
		}
		drawTarget := canvas
		var rotatedLayer *image.RGBA
		if math.Abs(layer.Rotation) >= 0.0001 {
			rotatedLayer = image.NewRGBA(canvas.Bounds())
			drawTarget = rotatedLayer
		}
		switch layer.Type {
		case theme.LayerTelemetry:
			waveshareFillRoundedRect(drawTarget, rectangle, max(0, waveshareOptionInt(layer.Options, "corner_radius", 8)), wavesharePreviewColor(waveshareOptionString(layer.Options, "panel_color", "04141BDC"), layer.Opacity))
			style := strings.ToLower(waveshareOptionString(layer.Options, "style", "cards"))
			if style == "cards" || style == "tiles" || style == "digital" {
				gap := 4
				count := max(1, len(waveshareTelemetryFields(layer.Options)))
				columns := min(4, count)
				if style == "tiles" {
					columns = min(3, count)
				}
				rows := (count + columns - 1) / columns
				cardWidth := max(12, (bounds.width-gap*(columns-1))/columns)
				cardHeight := max(12, (bounds.height-gap*(rows-1))/rows)
				cardFill := wavesharePreviewColor(waveshareOptionString(layer.Options, "card_color", "112C35D9"), layer.Opacity)
				corner := max(0, waveshareOptionInt(layer.Options, "corner_radius", 10))
				cardShape := strings.ToLower(waveshareOptionString(layer.Options, "card_shape", "rounded"))
				for index := 0; index < count; index++ {
					column, row := index%columns, index/columns
					x, y := bounds.x+column*(cardWidth+gap), bounds.y+row*(cardHeight+gap)
					card := image.Rect(x, y, min(rectangle.Max.X, x+cardWidth), min(rectangle.Max.Y, y+cardHeight))
					switch cardShape {
					case "square":
						waveshareFillRect(drawTarget, card, cardFill)
					case "pill":
						waveshareFillRoundedRect(drawTarget, card, card.Dy()/2, cardFill)
					case "outline":
						strokeWidth := max(1, waveshareOptionInt(layer.Options, "stroke_width", 2))
						waveshareFillRoundedRect(drawTarget, card, corner, wavesharePreviewColor(waveshareOptionString(layer.Options, "accent_color", "4CE0FF"), layer.Opacity))
						inner := image.Rect(card.Min.X+strokeWidth, card.Min.Y+strokeWidth, card.Max.X-strokeWidth, card.Max.Y-strokeWidth)
						if !inner.Empty() {
							waveshareFillRoundedRect(drawTarget, inner, max(0, corner-strokeWidth), cardFill)
						}
					default:
						waveshareFillRoundedRect(drawTarget, card, corner, cardFill)
					}
				}
			}
		case theme.LayerAudioBars:
			waveshareFillRoundedRect(drawTarget, rectangle, max(0, waveshareOptionInt(layer.Options, "corner_radius", 8)), wavesharePreviewColor(waveshareOptionString(layer.Options, "panel_color", "05151BD2"), layer.Opacity))
			// 系统音频频带由屏端驻留的标量控件实时绘制，不烘焙假波形。
		case theme.LayerAudioRing:
			waveshareFillRoundedRect(drawTarget, rectangle, max(0, waveshareOptionInt(layer.Options, "corner_radius", 8)), wavesharePreviewColor(waveshareOptionString(layer.Options, "panel_color", "061218DC"), layer.Opacity))
			// 环状频谱由屏端驻留标量控件实时绘制，背景只保留承载面板。
		case theme.LayerLyrics:
			// 歌词面板、边框、样式线和进度底轨都由 LV 绑定的 102 控件
			// 动态绘制。不能烘焙进背景，否则等待歌词时无法隐藏整个图层。
		case theme.LayerText, theme.LayerClock, theme.LayerProgressBar, theme.LayerGauge, theme.LayerGradientGauge, theme.LayerLineChart:
			panel := wavesharePreviewColor(waveshareOptionString(layer.Options, "panel_color", "00000000"), layer.Opacity)
			waveshareFillRoundedRect(drawTarget, rectangle, max(0, waveshareOptionInt(layer.Options, "corner_radius", 8)), panel)
		case theme.LayerShape:
			drawWaveshareShapeShell(drawTarget, rectangle, layer)
		}
		if rotatedLayer != nil {
			centerX := float64(bounds.x) + float64(bounds.width)/2
			centerY := float64(bounds.y) + float64(bounds.height)/2
			radians := layer.Rotation * math.Pi / 180
			cosine, sine := math.Cos(radians), math.Sin(radians)
			transform := f64.Aff3{
				cosine, -sine, centerX - cosine*centerX + sine*centerY,
				sine, cosine, centerY - sine*centerX - cosine*centerY,
			}
			xdraw.CatmullRom.Transform(canvas, transform, rotatedLayer, rotatedLayer.Bounds(), stddraw.Over, nil)
		}
	}
}

func wavesharePreviewColor(value string, opacity float64) color.NRGBA {
	value = strings.TrimPrefix(strings.TrimSpace(value), "#")
	alphaValue := 255
	if len(value) == 8 {
		if parsed, err := strconv.ParseUint(value[6:8], 16, 8); err == nil {
			alphaValue = int(parsed)
		}
		value = value[:6]
	}
	if len(value) != 6 {
		value = "5AAE73"
	}
	parse := func(text string) uint8 {
		result, err := strconv.ParseUint(text, 16, 8)
		if err != nil {
			return 255
		}
		return uint8(result)
	}
	return color.NRGBA{
		R: parse(value[0:2]), G: parse(value[2:4]), B: parse(value[4:6]),
		A: uint8(math.Round(math.Max(0, math.Min(1, opacity)) * float64(alphaValue))),
	}
}

func drawWaveshareShapeShell(canvas *image.RGBA, rectangle image.Rectangle, layer theme.Layer) {
	fill := wavesharePreviewColor(waveshareOptionString(layer.Options, "fill_color", "0B2029CC"), layer.Opacity)
	stroke := wavesharePreviewColor(waveshareOptionString(layer.Options, "stroke_color", "4CE0FF"), layer.Opacity)
	strokeWidth := max(0, waveshareOptionInt(layer.Options, "stroke_width", 2))
	shape := strings.ToLower(waveshareOptionString(layer.Options, "shape", "rounded_rect"))
	drawFill := func(target image.Rectangle, inset int, value color.NRGBA) {
		switch shape {
		case "circle", "ellipse":
			waveshareFillEllipse(canvas, target, value)
		case "line":
			drawWaveshareLine(canvas, target.Min.X, target.Min.Y+target.Dy()/2, target.Max.X-1, target.Min.Y+target.Dy()/2, value)
		case "pill":
			waveshareFillRoundedRect(canvas, target, max(1, target.Dy()/2-inset), value)
		case "rect":
			waveshareFillRect(canvas, target, value)
		default:
			waveshareFillRoundedRect(canvas, target, max(0, waveshareOptionInt(layer.Options, "corner_radius", 14)-inset), value)
		}
	}
	if strokeWidth > 0 && stroke.A > 0 {
		drawFill(rectangle, 0, stroke)
		inner := image.Rect(rectangle.Min.X+strokeWidth, rectangle.Min.Y+strokeWidth, rectangle.Max.X-strokeWidth, rectangle.Max.Y-strokeWidth)
		if !inner.Empty() {
			drawFill(inner, strokeWidth, fill)
		}
		return
	}
	drawFill(rectangle, 0, fill)
}

func waveshareFillRoundedRect(canvas *image.RGBA, rectangle image.Rectangle, radius int, fill color.NRGBA) {
	if rectangle.Empty() || fill.A == 0 {
		return
	}
	radius = max(0, min(radius, min(rectangle.Dx(), rectangle.Dy())/2))
	if radius == 0 {
		waveshareFillRect(canvas, rectangle, fill)
		return
	}
	waveshareFillRect(canvas, image.Rect(rectangle.Min.X+radius, rectangle.Min.Y, rectangle.Max.X-radius, rectangle.Max.Y), fill)
	waveshareFillRect(canvas, image.Rect(rectangle.Min.X, rectangle.Min.Y+radius, rectangle.Max.X, rectangle.Max.Y-radius), fill)
	for _, corner := range []image.Rectangle{
		image.Rect(rectangle.Min.X, rectangle.Min.Y, rectangle.Min.X+radius*2, rectangle.Min.Y+radius*2),
		image.Rect(rectangle.Max.X-radius*2, rectangle.Min.Y, rectangle.Max.X, rectangle.Min.Y+radius*2),
		image.Rect(rectangle.Min.X, rectangle.Max.Y-radius*2, rectangle.Min.X+radius*2, rectangle.Max.Y),
		image.Rect(rectangle.Max.X-radius*2, rectangle.Max.Y-radius*2, rectangle.Max.X, rectangle.Max.Y),
	} {
		waveshareFillEllipse(canvas, corner, fill)
	}
}

func waveshareFillEllipse(canvas *image.RGBA, rectangle image.Rectangle, fill color.NRGBA) {
	rectangle = rectangle.Intersect(canvas.Bounds())
	if rectangle.Empty() || fill.A == 0 {
		return
	}
	mask := image.NewAlpha(image.Rect(0, 0, rectangle.Dx(), rectangle.Dy()))
	rx := float64(rectangle.Dx()) / 2
	ry := float64(rectangle.Dy()) / 2
	for y := 0; y < rectangle.Dy(); y++ {
		for x := 0; x < rectangle.Dx(); x++ {
			dx := (float64(x) + .5 - rx) / rx
			dy := (float64(y) + .5 - ry) / ry
			if dx*dx+dy*dy <= 1 {
				mask.SetAlpha(x, y, color.Alpha{A: 255})
			}
		}
	}
	stddraw.DrawMask(canvas, rectangle, image.NewUniform(fill), image.Point{}, mask, image.Point{}, stddraw.Over)
}

func waveshareFillRect(canvas *image.RGBA, rectangle image.Rectangle, fill color.NRGBA) {
	if rectangle.Empty() || fill.A == 0 {
		return
	}
	stddraw.Draw(canvas, rectangle, image.NewUniform(fill), image.Point{}, stddraw.Over)
}

func drawWaveshareLine(canvas *image.RGBA, x0, y0, x1, y1 int, stroke color.NRGBA) {
	dx := int(math.Abs(float64(x1 - x0)))
	sx := -1
	if x0 < x1 {
		sx = 1
	}
	dy := -int(math.Abs(float64(y1 - y0)))
	sy := -1
	if y0 < y1 {
		sy = 1
	}
	err := dx + dy
	for {
		waveshareFillRect(canvas, image.Rect(x0-1, y0-1, x0+2, y0+2).Intersect(canvas.Bounds()), stroke)
		if x0 == x1 && y0 == y1 {
			break
		}
		twice := 2 * err
		if twice >= dy {
			err += dy
			x0 += sx
		}
		if twice <= dx {
			err += dx
			y0 += sy
		}
	}
}
