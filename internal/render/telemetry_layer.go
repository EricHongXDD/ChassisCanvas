package render

import (
	"image"
	"image/color"
	"math"
	"strings"

	"casescreen/internal/theme"
)

type telemetryVisualMetric struct {
	key   string
	label string
	value string
	raw   float64
}

type telemetryVisualColors struct {
	label  color.NRGBA
	value  color.NRGBA
	accent color.NRGBA
	card   color.NRGBA
}

// drawTelemetry 为主机合成设备兑现与微雪预设同名的四种布局。
// 所有绘制先裁到图层矩形；极小图层允许省略文字，但不能越界或崩溃。
func (renderer *Renderer) drawTelemetry(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, state State) {
	bounds = bounds.Intersect(canvas.Bounds())
	if bounds.Empty() {
		return
	}
	canvas = clippedLayerCanvas(canvas, bounds)
	renderer.drawComponentPanel(canvas, bounds, layer, color.NRGBA{R: 0, G: 0, B: 0, A: 72})
	metrics := telemetryMetrics(state, layer)
	if len(metrics) == 0 {
		return
	}
	colors := telemetryColors(layer)
	style := strings.ToLower(strings.TrimSpace(optionString(layer.Options, "style", "compact")))
	switch style {
	case "cards":
		renderer.drawTelemetryCards(canvas, bounds, layer, metrics, colors, false)
	case "gauge":
		renderer.drawTelemetryGauges(canvas, bounds, layer, metrics, colors)
	case "digital":
		renderer.drawTelemetryCards(canvas, bounds, layer, metrics, colors, true)
	case "minimal":
		renderer.drawTelemetryMinimal(canvas, bounds, layer, metrics, colors)
	default:
		// compact 以及未知旧样式都沿用紧凑列表，旧主题不会因升级消失。
		renderer.drawTelemetryCompact(canvas, bounds, layer, metrics, colors)
	}
}

func telemetryMetrics(state State, layer theme.Layer) []telemetryVisualMetric {
	fields := optionStrings(layer.Options, "fields")
	if len(fields) == 0 {
		fields = []string{"time", "Cpu_Freq", "Memory_Load"}
	}
	showUnit := theme.OptionBool(layer.Options, "show_unit", true)
	result := make([]telemetryVisualMetric, 0, len(fields))
	for _, key := range fields {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		metric := resolveComponentMetric(state, layer, key)
		result = append(result, telemetryVisualMetric{
			key: key, label: componentMetricLabel(key), value: telemetryValueText(metric.text, showUnit), raw: metric.raw,
		})
	}
	return result
}

func telemetryValueText(value string, showUnit bool) string {
	value = strings.TrimSpace(value)
	if showUnit {
		return value
	}
	for _, suffix := range []string{"GB", "dB", "℃", "%"} {
		if strings.HasSuffix(value, suffix) {
			return strings.TrimSpace(strings.TrimSuffix(value, suffix))
		}
	}
	return value
}

func telemetryColors(layer theme.Layer) telemetryVisualColors {
	legacyFallback := color.NRGBA{R: 245, G: 248, B: 255, A: 255}
	labelFallback := color.NRGBA{R: 168, G: 196, B: 202, A: 255}
	valueFallback := color.NRGBA{R: 245, G: 251, B: 255, A: 255}
	accentFallback := color.NRGBA{R: 76, G: 224, B: 255, A: 255}
	style := strings.ToLower(strings.TrimSpace(optionString(layer.Options, "style", "compact")))
	if style != "cards" && style != "gauge" && style != "digital" && style != "minimal" {
		// compact 和未知旧样式继续使用原来的单色缺省值。
		labelFallback, valueFallback, accentFallback = legacyFallback, legacyFallback, legacyFallback
	}
	// 旧主题只有 color 时继续统一上色；新样式缺省值则与设备端保持一致。
	if strings.TrimSpace(optionString(layer.Options, "color", "")) != "" {
		legacy := optionColor(layer.Options, "color", valueFallback)
		labelFallback, valueFallback, accentFallback = legacy, legacy, legacy
	}
	return telemetryVisualColors{
		label:  componentOpacity(optionColor(layer.Options, "label_color", labelFallback), layer.Opacity),
		value:  componentOpacity(optionColor(layer.Options, "value_color", valueFallback), layer.Opacity),
		accent: componentOpacity(optionColor(layer.Options, "accent_color", accentFallback), layer.Opacity),
		card:   componentOpacity(optionColor(layer.Options, "card_color", color.NRGBA{R: 17, G: 44, B: 53, A: 217}), layer.Opacity),
	}
}

func (renderer *Renderer) drawTelemetryCards(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer,
	metrics []telemetryVisualMetric, colors telemetryVisualColors, digital bool) {
	columns := minInt(4, len(metrics))
	columns = maxInt(1, minInt(columns, maxInt(1, bounds.Dx())))
	rows := maxInt(1, (len(metrics)+columns-1)/columns)
	rows = minInt(rows, maxInt(1, bounds.Dy()))
	gap := maxInt(0, minInt(4, minInt(bounds.Dx()/maxInt(1, columns*10), bounds.Dy()/maxInt(1, rows*8))))
	fontSize := math.Max(6, optionFloat(layer.Options, "font_size", 22))
	showLabel := theme.OptionBool(layer.Options, "show_label", true)
	showValue := theme.OptionBool(layer.Options, "show_value", true)
	for index, metric := range metrics {
		row, column := index/columns, index%columns
		if row >= rows {
			break
		}
		cell := telemetryGridCell(bounds, columns, rows, column, row)
		card := image.Rect(cell.Min.X+gap, cell.Min.Y+gap, cell.Max.X-gap, cell.Max.Y-gap).Intersect(bounds)
		if card.Empty() {
			card = cell.Intersect(bounds)
		}
		if card.Empty() {
			continue
		}
		drawTelemetryCard(canvas, card, layer, colors.card, colors.accent)
		renderer.drawTelemetryCardText(canvas, card, layer, metric, colors, fontSize, showLabel, showValue, digital)
	}
}

func telemetryGridCell(bounds image.Rectangle, columns, rows, column, row int) image.Rectangle {
	columns, rows = maxInt(1, columns), maxInt(1, rows)
	column, row = maxInt(0, minInt(columns-1, column)), maxInt(0, minInt(rows-1, row))
	left := bounds.Min.X + column*bounds.Dx()/columns
	right := bounds.Min.X + (column+1)*bounds.Dx()/columns
	top := bounds.Min.Y + row*bounds.Dy()/rows
	bottom := bounds.Min.Y + (row+1)*bounds.Dy()/rows
	return image.Rect(left, top, maxInt(left+1, right), maxInt(top+1, bottom)).Intersect(bounds)
}

func drawTelemetryCard(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, card, accent color.NRGBA) {
	if bounds.Empty() {
		return
	}
	shape := strings.ToLower(strings.TrimSpace(optionString(layer.Options, "card_shape", "rounded")))
	radius := maxInt(0, minInt(optionInt(layer.Options, "corner_radius", 10), minInt(bounds.Dx(), bounds.Dy())/2))
	switch shape {
	case "square":
		radius = 0
	case "pill":
		radius = minInt(bounds.Dx(), bounds.Dy()) / 2
	case "outline":
		stroke := maxInt(1, minInt(optionInt(layer.Options, "stroke_width", 2), maxInt(1, minInt(bounds.Dx(), bounds.Dy())/2)))
		fillRoundedPanel(canvas, bounds, radius, accent)
		inside := image.Rect(bounds.Min.X+stroke, bounds.Min.Y+stroke, bounds.Max.X-stroke, bounds.Max.Y-stroke)
		if !inside.Empty() {
			fillRoundedPanel(canvas, inside, maxInt(0, radius-stroke), card)
		}
		return
	}
	fillRoundedPanel(canvas, bounds, radius, card)
}

func (renderer *Renderer) drawTelemetryCardText(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer,
	metric telemetryVisualMetric, colors telemetryVisualColors, fontSize float64, showLabel, showValue, digital bool) {
	if bounds.Empty() || (!showLabel && !showValue) {
		return
	}
	labelLayer := telemetryLabelLayer(layer, digital)
	if showLabel && showValue {
		split := bounds.Min.Y + bounds.Dy()*2/5
		if digital {
			split = bounds.Min.Y + bounds.Dy()*2/5
			renderer.drawComponentText(canvas, image.Rect(bounds.Min.X, bounds.Min.Y, bounds.Max.X, split), labelLayer,
				metric.label, math.Max(6, fontSize*.42), colors.label, "left", "center")
			renderer.drawComponentText(canvas, image.Rect(bounds.Min.X, split, bounds.Max.X, bounds.Max.Y), layer,
				metric.value, fontSize, colors.value, "left", "center")
			return
		}
		renderer.drawComponentText(canvas, image.Rect(bounds.Min.X, bounds.Min.Y, bounds.Max.X, split), labelLayer,
			metric.label, math.Max(6, fontSize*.56), colors.label, "left", "center")
		renderer.drawComponentText(canvas, image.Rect(bounds.Min.X, split, bounds.Max.X, bounds.Max.Y), layer,
			metric.value, fontSize, colors.value, "left", "center")
		return
	}
	if showValue {
		renderer.drawComponentText(canvas, bounds, layer, metric.value, fontSize, colors.value, "center", "center")
		return
	}
	label := metric.label
	if digital {
		label = telemetryShortLabel(metric.key)
	}
	renderer.drawComponentText(canvas, bounds, labelLayer, label, math.Max(6, fontSize*.62), colors.label, "center", "center")
}

func telemetryLabelLayer(layer theme.Layer, digital bool) theme.Layer {
	if !digital || !strings.EqualFold(optionString(layer.Options, "font_family", "source-han"), "digital") {
		return layer
	}
	copyLayer := layer
	copyLayer.Options = make(map[string]any, len(layer.Options)+1)
	for key, value := range layer.Options {
		copyLayer.Options[key] = value
	}
	copyLayer.Options["font_family"] = "source-han"
	return copyLayer
}

func telemetryShortLabel(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "cpu_freq", "cpu_percent":
		return "CPU"
	case "gpu_load", "gpu_percent":
		return "GPU"
	case "memory_load", "memory_percent":
		return "RAM"
	case "gpu_temp", "gpu_temperature":
		return "GPU TEMP"
	case "gpu_hotspot_temp", "gpu_hotspot_temperature":
		return "GPU HOTSPOT"
	case "gpu_memory_temp", "gpu_memory_temperature":
		return "VRAM TEMP"
	case "memory_used":
		return "USED"
	case "memory_available":
		return "FREE"
	case "time":
		return "TIME"
	case "date":
		return "DATE"
	case "week":
		return "WEEK"
	case "vol", "volume":
		return "VOL"
	case "cur_db", "current_decibels":
		return "dB"
	case "avg_db", "average_decibels":
		return "AVG dB"
	case "max_db", "maximum_decibels":
		return "MAX dB"
	default:
		return strings.ToUpper(strings.TrimSpace(key))
	}
}

func (renderer *Renderer) drawTelemetryGauges(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer,
	metrics []telemetryVisualMetric, colors telemetryVisualColors) {
	rows := minInt(len(metrics), maxInt(1, bounds.Dy()))
	fontSize := math.Max(6, optionFloat(layer.Options, "font_size", 22))
	showLabel := theme.OptionBool(layer.Options, "show_label", true)
	showValue := theme.OptionBool(layer.Options, "show_value", true)
	for index := 0; index < rows; index++ {
		metric := metrics[index]
		row := telemetryGridCell(bounds, 1, rows, 0, index)
		if row.Empty() {
			continue
		}
		padding := maxInt(1, minInt(6, minInt(row.Dx(), row.Dy())/8))
		content := image.Rect(row.Min.X+padding, row.Min.Y, row.Max.X-padding, row.Max.Y).Intersect(row)
		if content.Empty() {
			content = row
		}
		labelWidth, valueWidth := 0, 0
		if showLabel {
			labelWidth = minInt(maxInt(1, content.Dx()/3), maxInt(1, content.Dx()-1))
			renderer.drawComponentText(canvas, image.Rect(content.Min.X, content.Min.Y, content.Min.X+labelWidth, content.Max.Y),
				layer, telemetryShortLabel(metric.key), math.Max(6, fontSize*.58), colors.label, "left", "center")
		}
		if showValue {
			valueWidth = minInt(maxInt(1, content.Dx()/4), maxInt(1, content.Dx()-labelWidth))
			renderer.drawComponentText(canvas, image.Rect(content.Max.X-valueWidth, content.Min.Y, content.Max.X, content.Max.Y),
				layer, metric.value, math.Max(6, fontSize*.68), colors.value, "right", "center")
		}
		bar := image.Rect(content.Min.X+labelWidth, content.Min.Y+content.Dy()*3/5,
			content.Max.X-valueWidth, content.Min.Y+content.Dy()*3/5+maxInt(1, minInt(8, content.Dy()/5))).Intersect(content)
		if bar.Empty() {
			continue
		}
		radius := minInt(bar.Dx(), bar.Dy()) / 2
		track := componentOpacity(color.NRGBA{R: 24, G: 55, B: 64, A: 191}, layer.Opacity)
		fillRoundedPanel(canvas, bar, radius, track)
		ratio := componentRangeRatio(layer, metric.raw)
		filled := bar.Min.X + int(math.Round(float64(bar.Dx())*ratio))
		if filled > bar.Min.X {
			fillRoundedPanel(canvas, image.Rect(bar.Min.X, bar.Min.Y, minInt(bar.Max.X, filled), bar.Max.Y), radius, colors.accent)
		}
	}
}

func (renderer *Renderer) drawTelemetryMinimal(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer,
	metrics []telemetryVisualMetric, colors telemetryVisualColors) {
	rows := minInt(len(metrics), maxInt(1, bounds.Dy()))
	fontSize := math.Max(6, optionFloat(layer.Options, "font_size", 18))
	showValue := theme.OptionBool(layer.Options, "show_value", true)
	// 微雪的极简样式只保留数值；主机合成目标沿用同一语义，避免同一预设跨屏变形。
	if !showValue {
		return
	}
	for index := 0; index < rows; index++ {
		metric := metrics[index]
		row := telemetryGridCell(bounds, 1, rows, 0, index)
		renderer.drawComponentText(canvas, row, layer, metric.value, fontSize, colors.value, "left", "center")
	}
}

func (renderer *Renderer) drawTelemetryCompact(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer,
	metrics []telemetryVisualMetric, colors telemetryVisualColors) {
	rows := minInt(len(metrics), maxInt(1, bounds.Dy()))
	fontSize := math.Max(6, optionFloat(layer.Options, "font_size", math.Max(12, float64(bounds.Dy())/float64(maxInt(1, rows))*.44)))
	showLabel := theme.OptionBool(layer.Options, "show_label", true)
	showValue := theme.OptionBool(layer.Options, "show_value", true)
	for index := 0; index < rows; index++ {
		metric := metrics[index]
		row := telemetryGridCell(bounds, 1, rows, 0, index)
		if showLabel && showValue {
			split := row.Min.X + row.Dx()*2/5
			renderer.drawComponentText(canvas, image.Rect(row.Min.X, row.Min.Y, split, row.Max.Y), layer,
				telemetryShortLabel(metric.key), fontSize, colors.label, "left", "center")
			renderer.drawComponentText(canvas, image.Rect(split, row.Min.Y, row.Max.X, row.Max.Y), layer,
				metric.value, fontSize, colors.value, "left", "center")
		} else if showValue {
			renderer.drawComponentText(canvas, row, layer, metric.value, fontSize, colors.value, "left", "center")
		} else if showLabel {
			renderer.drawComponentText(canvas, row, layer, telemetryShortLabel(metric.key), fontSize, colors.label, "left", "center")
		}
	}
}
