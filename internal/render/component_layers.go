package render

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"
	"time"

	"casescreen/internal/theme"
)

type chartVisualRuntime struct {
	values      []float64
	capturedAt  time.Time
	initialized bool
}

type metricVisualValue struct {
	text string
	raw  float64
}

// drawTextLayer 绘制静态文字。动态数据绑定由专用遥测控件负责，避免把
// 用户设置的标题在主机合成设备上意外替换成另一段内容。
func (renderer *Renderer) drawTextLayer(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer) {
	if bounds.Empty() {
		return
	}
	canvas = clippedLayerCanvas(canvas, bounds)
	renderer.drawComponentPanel(canvas, bounds, layer, color.NRGBA{})
	value := optionString(layer.Options, "text", "自定义文本")
	foreground := componentColor(layer, "color", color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	renderer.drawComponentText(canvas, bounds, layer, value, optionFloat(layer.Options, "font_size", 32), foreground,
		optionString(layer.Options, "align", "center"), optionString(layer.Options, "vertical_align", "center"))
}

func (renderer *Renderer) drawClockLayer(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, capturedAt time.Time) {
	if bounds.Empty() {
		return
	}
	canvas = clippedLayerCanvas(canvas, bounds)
	if capturedAt.IsZero() {
		capturedAt = time.Now()
	}
	renderer.drawComponentPanel(canvas, bounds, layer, color.NRGBA{R: 4, G: 20, B: 27, A: 188})
	value := formatComponentClock(optionString(layer.Options, "format", "HH:mm:ss"), capturedAt)
	foreground := componentColor(layer, "color", color.NRGBA{R: 76, G: 224, B: 255, A: 255})
	renderer.drawComponentText(canvas, bounds, layer, value, optionFloat(layer.Options, "font_size", 44), foreground,
		optionString(layer.Options, "align", "center"), "center")
}

func formatComponentClock(format string, now time.Time) string {
	if format == "" {
		format = "HH:mm:ss"
	}
	if strings.Contains(format, "week") {
		week := [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[now.Weekday()]
		format = strings.ReplaceAll(format, "week", week)
	}
	return now.Format(strings.NewReplacer(
		"yyyy", "2006", "MM", "01", "dd", "02", "HH", "15", "mm", "04", "ss", "05",
	).Replace(format))
}

func (renderer *Renderer) drawProgressLayer(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, state State) {
	if bounds.Empty() {
		return
	}
	canvas = clippedLayerCanvas(canvas, bounds)
	renderer.drawComponentPanel(canvas, bounds, layer, color.NRGBA{R: 4, G: 20, B: 27, A: 188})
	key := optionString(layer.Options, "data_key", "Cpu_Freq")
	metric := resolveComponentMetric(state, layer, key)
	ratio := componentRangeRatio(layer, metric.raw)
	trackColor := componentColor(layer, "secondary_color", color.NRGBA{R: 24, G: 55, B: 64, A: 255})
	fillColor := componentColor(layer, "color", color.NRGBA{R: 76, G: 224, B: 255, A: 255})
	orientation := strings.ToLower(optionString(layer.Options, "orientation", "horizontal"))
	style := strings.ToLower(optionString(layer.Options, "style", "rounded"))
	margin := componentMargin(bounds)
	bar := image.Rect(bounds.Min.X+margin, bounds.Min.Y+bounds.Dy()/2, bounds.Max.X-margin,
		maxInt(bounds.Min.Y+bounds.Dy()/2+4, bounds.Max.Y-margin)).Intersect(bounds)
	if orientation == "vertical" {
		bar = image.Rect(bounds.Min.X+bounds.Dx()/3, bounds.Min.Y+margin,
			maxInt(bounds.Min.X+bounds.Dx()/3+5, bounds.Min.X+bounds.Dx()*2/3), bounds.Max.Y-margin).Intersect(bounds)
	}
	if bar.Empty() {
		return
	}
	radius := maxInt(0, minInt(optionInt(layer.Options, "corner_radius", 8), minInt(bar.Dx(), bar.Dy())/2))
	if style == "square" || style == "segmented" {
		radius = 0
	}
	trackFill := trackColor
	if style == "outline" {
		trackFill = componentColor(layer, "panel_color", color.NRGBA{R: 4, G: 20, B: 27, A: 188})
	}
	fillRoundedPanel(canvas, bar, radius, trackFill)
	active := bar
	if orientation == "vertical" {
		active.Min.Y = active.Max.Y - int(math.Round(float64(active.Dy())*ratio))
	} else if orientation == "reverse" {
		active.Min.X = active.Max.X - int(math.Round(float64(active.Dx())*ratio))
	} else {
		active.Max.X = active.Min.X + int(math.Round(float64(active.Dx())*ratio))
	}
	active = active.Intersect(bar)
	segments := maxInt(1, minInt(16, optionInt(layer.Options, "segments", 10)))
	switch style {
	case "segmented":
		for index := 0; index < segments; index++ {
			segment := image.Rect(bar.Min.X+index*bar.Dx()/segments, bar.Min.Y,
				bar.Min.X+(index+1)*bar.Dx()/segments-maxInt(1, bar.Dx()/maxInt(20, segments*8)), bar.Max.Y)
			if orientation == "vertical" {
				segment = image.Rect(bar.Min.X, bar.Min.Y+index*bar.Dy()/segments, bar.Max.X,
					bar.Min.Y+(index+1)*bar.Dy()/segments-maxInt(1, bar.Dy()/maxInt(20, segments*8)))
			}
			if visible := segment.Intersect(active); !visible.Empty() {
				fillRoundedPanel(canvas, visible, minInt(2, minInt(visible.Dx(), visible.Dy())/2), fillColor)
			}
		}
	case "dots":
		for index := 0; index < segments; index++ {
			x := bar.Min.X + (index*2+1)*bar.Dx()/(segments*2)
			y := (bar.Min.Y + bar.Max.Y) / 2
			dotRadius := maxInt(1, bar.Dy()/3)
			isActive := x >= active.Min.X && x < active.Max.X
			if orientation == "vertical" {
				x = (bar.Min.X + bar.Max.X) / 2
				y = bar.Min.Y + (index*2+1)*bar.Dy()/(segments*2)
				dotRadius = maxInt(1, bar.Dx()/3)
				isActive = y >= active.Min.Y && y < active.Max.Y
			}
			if isActive {
				fillCircle(canvas, x, y, dotRadius, fillColor)
			}
		}
	default:
		if !active.Empty() {
			fillRoundedPanel(canvas, active, minInt(radius, minInt(active.Dx(), active.Dy())/2), fillColor)
		}
	}
	if style == "neon" && !active.Empty() {
		highlight := active
		highlight.Max.Y = highlight.Min.Y + maxInt(1, highlight.Dy()/2)
		fillRoundedPanel(canvas, highlight, minInt(radius, minInt(highlight.Dx(), highlight.Dy())/2),
			componentOpacity(color.NRGBA{R: 255, G: 255, B: 255, A: 72}, layer.Opacity))
	}
	if style == "neon" || style == "outline" {
		borderWidth := 1
		if style == "outline" {
			borderWidth = 2
		}
		drawComponentRectBorder(canvas, bar, borderWidth, fillColor)
	}
	renderer.drawProgressLabels(canvas, bounds, bar, layer, key, metric.text, fillColor, margin, orientation)
}

func (renderer *Renderer) drawProgressLabels(canvas *image.RGBA, bounds, bar image.Rectangle, layer theme.Layer,
	key, value string, accent color.NRGBA, margin int, orientation string) {
	showLabel := theme.OptionBool(layer.Options, "show_label", true)
	showValue := theme.OptionBool(layer.Options, "show_value", true)
	if !showLabel && !showValue {
		return
	}
	textTop := bounds.Min.Y + maxInt(1, margin/2)
	textBottom := maxInt(textTop+1, bar.Min.Y-maxInt(1, margin/2))
	if orientation == "vertical" || textBottom > bounds.Max.Y-1 {
		textBottom = minInt(bounds.Max.Y-1, maxInt(textTop+1, bounds.Min.Y+bounds.Dy()/3))
	}
	textBounds := image.Rect(bounds.Min.X+maxInt(1, margin/2), textTop,
		bounds.Max.X-maxInt(1, margin/2), textBottom).Intersect(bounds)
	if textBounds.Empty() {
		return
	}
	size := math.Max(7, float64(minInt(bounds.Dx(), bounds.Dy()))*.15)
	labelColor := componentOpacity(color.NRGBA{R: 185, G: 210, B: 216, A: 255}, layer.Opacity)
	if showLabel && showValue {
		gap := maxInt(1, minInt(4, textBounds.Dx()/16))
		middle := textBounds.Min.X + maxInt(1, (textBounds.Dx()-gap)/2)
		renderer.drawComponentText(canvas, image.Rect(textBounds.Min.X, textBounds.Min.Y, middle, textBounds.Max.Y), layer,
			componentMetricLabel(key), size, labelColor, "left", "center")
		renderer.drawComponentText(canvas, image.Rect(minInt(textBounds.Max.X-1, middle+gap), textBounds.Min.Y,
			textBounds.Max.X, textBounds.Max.Y), layer, value, size, accent, "right", "center")
	} else if showLabel {
		renderer.drawComponentText(canvas, textBounds, layer, componentMetricLabel(key), size, labelColor, "left", "center")
	} else {
		renderer.drawComponentText(canvas, textBounds, layer, value, size, accent, "right", "center")
	}
}

func (renderer *Renderer) drawGaugeLayer(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, state State) {
	if bounds.Empty() {
		return
	}
	canvas = clippedLayerCanvas(canvas, bounds)
	renderer.drawComponentPanel(canvas, bounds, layer, color.NRGBA{R: 4, G: 20, B: 27, A: 188})
	key := optionString(layer.Options, "data_key", "Cpu_Freq")
	metric := resolveComponentMetric(state, layer, key)
	ratio := componentRangeRatio(layer, metric.raw)
	centerX, centerY := bounds.Min.X+bounds.Dx()/2, bounds.Min.Y+bounds.Dy()/2
	radius := maxInt(2, minInt(bounds.Dx(), bounds.Dy())/2-maxInt(3, minInt(bounds.Dx(), bounds.Dy())/12))
	stroke := maxInt(1, minInt(optionInt(layer.Options, "stroke_width", 8), maxInt(1, radius/2)))
	startAngle := optionFloat(layer.Options, "start_angle", -140)
	endAngle := optionFloat(layer.Options, "end_angle", 140)
	style := strings.ToLower(optionString(layer.Options, "style", "arc"))
	switch style {
	case "semicircle":
		startAngle, endAngle = 180, 360
	case "ring":
		startAngle, endAngle = -180, 180
	case "minimal":
		startAngle, endAngle = -180, 180
		stroke = maxInt(1, stroke/2)
	}
	track := componentColor(layer, "secondary_color", color.NRGBA{R: 24, G: 55, B: 64, A: 255})
	drawComponentArc(canvas, centerX, centerY, radius, startAngle, endAngle, stroke, track, 1)
	foreground := componentColor(layer, "color", color.NRGBA{R: 76, G: 224, B: 255, A: 255})
	if layer.Type == theme.LayerGradientGauge {
		startColor := componentColor(layer, "gradient_start", color.NRGBA{R: 76, G: 224, B: 255, A: 255})
		middleColor := componentColor(layer, "gradient_mid", color.NRGBA{R: 255, G: 242, B: 117, A: 255})
		endColor := componentColor(layer, "gradient_end", color.NRGBA{R: 255, G: 92, B: 104, A: 255})
		drawComponentGradientArc(canvas, centerX, centerY, radius, startAngle, endAngle, stroke, ratio, startColor, middleColor, endColor)
		foreground = startColor
		if optionString(layer.Options, "color", "") != "" {
			foreground = componentColor(layer, "color", startColor)
		}
	} else {
		drawComponentArc(canvas, centerX, centerY, radius, startAngle, endAngle, stroke, foreground, ratio)
	}
	if style == "dial" || style == "ticks" {
		markCount := 10
		if style == "dial" {
			markCount = 18
		}
		for index := 0; index <= markCount; index++ {
			angle := (startAngle + (endAngle-startAngle)*float64(index)/float64(markCount) - 90) * math.Pi / 180
			x0 := centerX + int(math.Round(math.Cos(angle)*float64(radius-stroke*2)))
			y0 := centerY + int(math.Round(math.Sin(angle)*float64(radius-stroke*2)))
			x1 := centerX + int(math.Round(math.Cos(angle)*float64(radius+stroke/2)))
			y1 := centerY + int(math.Round(math.Sin(angle)*float64(radius+stroke/2)))
			drawLine(canvas, x0, y0, x1, y1, maxInt(1, stroke/3), track)
		}
	}
	if style == "dial" {
		angle := (startAngle + (endAngle-startAngle)*ratio - 90) * math.Pi / 180
		x := centerX + int(math.Round(math.Cos(angle)*float64(maxInt(1, radius-stroke*2))))
		y := centerY + int(math.Round(math.Sin(angle)*float64(maxInt(1, radius-stroke*2))))
		drawLine(canvas, centerX, centerY, x, y, maxInt(2, stroke/3), foreground)
		fillCircle(canvas, centerX, centerY, maxInt(2, stroke/2), foreground)
	}
	renderer.drawGaugeLabels(canvas, bounds, layer, key, metric.text, foreground)
}

func (renderer *Renderer) drawGaugeLabels(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer,
	key, value string, foreground color.NRGBA) {
	showValue := theme.OptionBool(layer.Options, "show_value", true)
	showLabel := theme.OptionBool(layer.Options, "show_label", true)
	if !showValue && !showLabel {
		return
	}
	padding := maxInt(1, minInt(5, minInt(bounds.Dx(), bounds.Dy())/12))
	content := image.Rect(bounds.Min.X+padding, bounds.Min.Y+padding, bounds.Max.X-padding, bounds.Max.Y-padding)
	preferred := optionFloat(layer.Options, "font_size", 22)
	if showValue && showLabel {
		labelTop := maxInt(content.Min.Y+1, minInt(content.Max.Y-1, bounds.Min.Y+bounds.Dy()*2/3))
		valueBounds := image.Rect(content.Min.X, maxInt(content.Min.Y, bounds.Min.Y+bounds.Dy()/3), content.Max.X, labelTop)
		labelBounds := image.Rect(content.Min.X, labelTop, content.Max.X, content.Max.Y)
		renderer.drawComponentText(canvas, valueBounds, layer, value, preferred, foreground, "center", "center")
		renderer.drawComponentText(canvas, labelBounds, layer, componentMetricLabel(key), math.Max(6, preferred*.48), foreground, "center", "center")
	} else if showValue {
		renderer.drawComponentText(canvas, content, layer, value, preferred, foreground, "center", "center")
	} else {
		renderer.drawComponentText(canvas, content, layer, componentMetricLabel(key), math.Max(6, preferred*.48), foreground, "center", "center")
	}
}

func (renderer *Renderer) drawLineChartLayer(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, state State) {
	if bounds.Empty() {
		return
	}
	canvas = clippedLayerCanvas(canvas, bounds)
	renderer.drawComponentPanel(canvas, bounds, layer, color.NRGBA{R: 4, G: 20, B: 27, A: 188})
	margin := maxInt(3, minInt(bounds.Dx(), bounds.Dy())/12)
	chart := image.Rect(bounds.Min.X+margin, bounds.Min.Y+margin, bounds.Max.X-margin, bounds.Max.Y-margin).Intersect(bounds)
	if chart.Empty() {
		return
	}
	style := strings.ToLower(optionString(layer.Options, "style", "area"))
	gridColor := componentColor(layer, "grid_color", color.NRGBA{R: 82, G: 114, B: 126, A: 100})
	if theme.OptionBool(layer.Options, "show_grid", true) {
		for index := 1; index < 4; index++ {
			y := chart.Min.Y + index*chart.Dy()/4
			drawLine(canvas, chart.Min.X, y, chart.Max.X-1, y, 1, gridColor)
		}
		for index := 1; index < 6; index++ {
			x := chart.Min.X + index*chart.Dx()/6
			drawLine(canvas, x, chart.Min.Y, x, chart.Max.Y-1, 1, gridColor)
		}
	}
	key := optionString(layer.Options, "data_key", "Cpu_Freq")
	metric := resolveComponentMetric(state, layer, key)
	pointCount := maxInt(8, minInt(240, optionInt(layer.Options, "history_points", 48)))
	values := renderer.updateChartHistory(layer, key, metric.raw, state.Telemetry.CapturedAt, pointCount)
	if len(values) == 0 {
		return
	}
	points := make([]image.Point, pointCount)
	first := values[0]
	for index := range points {
		valueIndex := index - (pointCount - len(values))
		raw := first
		if valueIndex >= 0 {
			raw = values[valueIndex]
		}
		ratio := componentRangeRatio(layer, raw)
		x := chart.Min.X + int(math.Round(float64(maxInt(0, chart.Dx()-1))*float64(index)/float64(maxInt(1, pointCount-1))))
		y := chart.Max.Y - 1 - int(math.Round(float64(maxInt(0, chart.Dy()-1))*ratio))
		points[index] = image.Pt(x, y)
	}
	lineColor := componentColor(layer, "color", color.NRGBA{R: 76, G: 224, B: 255, A: 255})
	fillColor := componentColor(layer, "fill_color", color.NRGBA{R: 76, G: 224, B: 255, A: 56})
	stroke := maxInt(1, minInt(20, optionInt(layer.Options, "stroke_width", 3)))
	if style == "area" || style == "neon" {
		for index, point := range points {
			right := point.X + maxInt(1, chart.Dx()/pointCount+1)
			if index+1 < len(points) {
				right = maxInt(right, points[index+1].X+1)
			}
			fillRect(canvas, image.Rect(point.X, point.Y, minInt(chart.Max.X, right), chart.Max.Y), fillColor)
		}
	}
	switch style {
	case "dots":
		for _, point := range points {
			fillCircle(canvas, point.X, point.Y, stroke, lineColor)
		}
	case "step":
		for index := 1; index < len(points); index++ {
			previous, current := points[index-1], points[index]
			drawLine(canvas, previous.X, previous.Y, current.X, previous.Y, stroke, lineColor)
			drawLine(canvas, current.X, previous.Y, current.X, current.Y, stroke, lineColor)
		}
	case "spline":
		for index := 0; index+1 < len(points); index++ {
			firstPoint := points[maxInt(0, index-1)]
			second, third := points[index], points[index+1]
			fourth := points[minInt(len(points)-1, index+2)]
			previous := second
			for subdivision := 1; subdivision <= 4; subdivision++ {
				current := componentCatmullRomPoint(firstPoint, second, third, fourth, float64(subdivision)/4)
				current.X = maxInt(chart.Min.X, minInt(chart.Max.X-1, current.X))
				current.Y = maxInt(chart.Min.Y, minInt(chart.Max.Y-1, current.Y))
				drawLine(canvas, previous.X, previous.Y, current.X, current.Y, stroke, lineColor)
				previous = current
			}
		}
	default:
		for index := 1; index < len(points); index++ {
			previous, current := points[index-1], points[index]
			if style == "neon" {
				drawLine(canvas, previous.X, previous.Y, current.X, current.Y, stroke+3, withVisualAlpha(lineColor, .30))
			}
			drawLine(canvas, previous.X, previous.Y, current.X, current.Y, stroke, lineColor)
		}
	}
	if theme.OptionBool(layer.Options, "show_label", true) {
		labelHeight := maxInt(8, minInt(chart.Dy(), maxInt(10, chart.Dy()/4)))
		labelBounds := image.Rect(chart.Min.X+2, chart.Min.Y+1, chart.Max.X-2, minInt(chart.Max.Y, chart.Min.Y+1+labelHeight))
		renderer.drawComponentText(canvas, labelBounds, layer, componentMetricLabel(key)+"  "+metric.text,
			math.Max(7, float64(minInt(bounds.Dx(), bounds.Dy()))*.10), lineColor, "left", "center")
	}
}

func (renderer *Renderer) updateChartHistory(layer theme.Layer, dataKey string, value float64, capturedAt time.Time, limit int) []float64 {
	key := layer.ID + "\x00" + dataKey
	if layer.ID == "" {
		key = fmt.Sprintf("%s:%.4f:%.4f:%.4f:%.4f:%s", layer.Type, layer.Bounds.X, layer.Bounds.Y,
			layer.Bounds.Width, layer.Bounds.Height, dataKey)
	}
	runtime := renderer.charts[key]
	appendValue := !runtime.initialized
	if runtime.initialized {
		if !capturedAt.IsZero() {
			appendValue = !capturedAt.Equal(runtime.capturedAt)
		} else if len(runtime.values) > 0 {
			appendValue = math.Abs(runtime.values[len(runtime.values)-1]-value) > 1e-9
		}
	}
	if appendValue {
		runtime.values = append(runtime.values, value)
		runtime.capturedAt = capturedAt
		runtime.initialized = true
	}
	if len(runtime.values) > limit {
		runtime.values = append([]float64(nil), runtime.values[len(runtime.values)-limit:]...)
	}
	renderer.charts[key] = runtime
	return append([]float64(nil), runtime.values...)
}

func (renderer *Renderer) drawShapeLayer(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer) {
	if bounds.Empty() {
		return
	}
	canvas = clippedLayerCanvas(canvas, bounds)
	fill := componentColor(layer, "fill_color", color.NRGBA{R: 11, G: 32, B: 41, A: 204})
	stroke := componentColor(layer, "stroke_color", color.NRGBA{R: 76, G: 224, B: 255, A: 255})
	strokeWidth := maxInt(0, minInt(optionInt(layer.Options, "stroke_width", 2), minInt(bounds.Dx(), bounds.Dy())/2))
	shape := strings.ToLower(optionString(layer.Options, "shape", "rounded_rect"))
	if shape == "line" {
		drawLine(canvas, bounds.Min.X+minInt(2, bounds.Dx()/4), bounds.Min.Y+bounds.Dy()/2,
			bounds.Max.X-1-minInt(2, bounds.Dx()/4), bounds.Min.Y+bounds.Dy()/2, maxInt(1, strokeWidth), stroke)
		return
	}
	shapeBounds := bounds
	if shape == "circle" {
		diameter := minInt(bounds.Dx(), bounds.Dy())
		shapeBounds = image.Rect(bounds.Min.X+(bounds.Dx()-diameter)/2, bounds.Min.Y+(bounds.Dy()-diameter)/2,
			bounds.Min.X+(bounds.Dx()+diameter)/2, bounds.Min.Y+(bounds.Dy()+diameter)/2)
	}
	if shape == "circle" || shape == "ellipse" {
		if strokeWidth > 0 {
			fillComponentEllipse(canvas, shapeBounds, stroke)
		}
		inside := image.Rect(shapeBounds.Min.X+strokeWidth, shapeBounds.Min.Y+strokeWidth,
			shapeBounds.Max.X-strokeWidth, shapeBounds.Max.Y-strokeWidth)
		if strokeWidth == 0 {
			inside = shapeBounds
		}
		if !inside.Empty() {
			fillComponentEllipse(canvas, inside, fill)
		}
		return
	}
	radius := 0
	if shape == "rounded_rect" {
		radius = optionInt(layer.Options, "corner_radius", 14)
	} else if shape == "pill" {
		radius = minInt(bounds.Dx(), bounds.Dy()) / 2
	}
	radius = maxInt(0, minInt(radius, minInt(bounds.Dx(), bounds.Dy())/2))
	if strokeWidth > 0 {
		fillRoundedPanel(canvas, bounds, radius, stroke)
	}
	inside := image.Rect(bounds.Min.X+strokeWidth, bounds.Min.Y+strokeWidth,
		bounds.Max.X-strokeWidth, bounds.Max.Y-strokeWidth)
	if strokeWidth == 0 {
		inside = bounds
	}
	if !inside.Empty() {
		fillRoundedPanel(canvas, inside, maxInt(0, radius-strokeWidth), fill)
	}
}

func (renderer *Renderer) drawComponentPanel(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, fallback color.NRGBA) {
	panel := componentColor(layer, "panel_color", fallback)
	if panel.A == 0 {
		return
	}
	radius := maxInt(0, minInt(optionInt(layer.Options, "corner_radius", 8), minInt(bounds.Dx(), bounds.Dy())/2))
	fillRoundedPanel(canvas, bounds, radius, panel)
}

func (renderer *Renderer) drawComponentText(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, value string,
	preferredSize float64, foreground color.NRGBA, align, verticalAlign string) {
	bounds = bounds.Intersect(canvas.Bounds())
	if bounds.Empty() || value == "" || foreground.A == 0 {
		return
	}
	padding := maxInt(1, minInt(6, minInt(bounds.Dx(), bounds.Dy())/10))
	content := image.Rect(bounds.Min.X+padding, bounds.Min.Y+padding, bounds.Max.X-padding, bounds.Max.Y-padding)
	if content.Empty() {
		content = bounds
	}
	resourceID := componentFontResource(renderer, layer)
	face := renderer.fittedFace(resourceID, preferredSize, []string{value}, maxInt(1, content.Dx()), maxInt(1, content.Dy()))
	value = fitTextToWidth(face, value, maxInt(1, content.Dx()))
	x := alignedTextX(face, value, content.Min.X, content.Dx(), strings.ToLower(align))
	baseline := centeredTextBaseline(face, content.Min.Y+content.Dy()/2)
	switch strings.ToLower(verticalAlign) {
	case "top":
		baseline = content.Min.Y + face.Metrics().Ascent.Ceil()
	case "bottom":
		baseline = content.Max.Y - face.Metrics().Descent.Ceil()
	}
	drawLayerText(clippedLayerCanvas(canvas, bounds), face, x, baseline, value, foreground, theme.OptionBool(layer.Options, "shadow", false), layer)
}

func componentFontResource(renderer *Renderer, layer theme.Layer) string {
	if resourceID := optionString(layer.Options, "font_resource_id", ""); resourceID != "" {
		return resourceID
	}
	family := strings.ToLower(optionString(layer.Options, "font_family", "source-han"))
	weighted := systemFontResourceID(family, layerFontWeight(layer))
	if renderer.fonts[weighted] != nil {
		return weighted
	}
	if renderer.fonts[family] != nil {
		return family
	}
	return ""
}

func componentColor(layer theme.Layer, key string, fallback color.NRGBA) color.NRGBA {
	return componentOpacity(optionColor(layer.Options, key, fallback), layer.Opacity)
}

func componentOpacity(value color.NRGBA, opacity float64) color.NRGBA {
	value.A = multiplyAlpha(value.A, opacity)
	return value
}

func componentMargin(bounds image.Rectangle) int {
	return maxInt(2, minInt(maxInt(1, minInt(bounds.Dx(), bounds.Dy())/10), maxInt(2, minInt(bounds.Dx(), bounds.Dy())/3)))
}

func componentRangeRatio(layer theme.Layer, raw float64) float64 {
	minimum := optionFloat(layer.Options, "min_value", 0)
	maximum := optionFloat(layer.Options, "max_value", 100)
	if math.IsNaN(raw) || math.IsInf(raw, 0) || math.IsNaN(minimum) || math.IsInf(minimum, 0) ||
		math.IsNaN(maximum) || math.IsInf(maximum, 0) || maximum <= minimum {
		return 0
	}
	return clampVisualUnit((raw - minimum) / (maximum - minimum))
}

func resolveComponentMetric(state State, layer theme.Layer, key string) metricVisualValue {
	now := state.Telemetry.CapturedAt
	if now.IsZero() {
		now = time.Now()
	}
	fallback := defaultComponentMetric(key, now)
	if value := optionFloat(layer.Options, "default_value", math.NaN()); !math.IsNaN(value) && !math.IsInf(value, 0) {
		fallback = metricVisualValue{text: formatComponentMetric(key, value), raw: value}
	}
	if !componentTelemetryAvailable(state) {
		return fallback
	}
	snapshot := state.Telemetry
	normalizedKey := strings.ToLower(key)
	if snapshot.OmitHardwareMetrics && normalizedKey != "time" && normalizedKey != "date" && normalizedKey != "week" {
		return fallback
	}
	switch normalizedKey {
	case "cpu_freq", "cpu_percent":
		return metricVisualValue{text: fmt.Sprintf("%.0f%%", snapshot.CPUPercent), raw: snapshot.CPUPercent}
	case "gpu_load", "gpu_percent":
		if snapshot.GPUPercent != nil {
			return metricVisualValue{text: fmt.Sprintf("%.0f%%", *snapshot.GPUPercent), raw: *snapshot.GPUPercent}
		}
		return metricVisualValue{text: "--", raw: 0}
	case "memory_load", "memory_percent":
		return metricVisualValue{text: fmt.Sprintf("%.0f%%", snapshot.MemoryPercent), raw: snapshot.MemoryPercent}
	case "gpu_temp", "gpu_temperature":
		if snapshot.GPUTemperature != nil {
			return metricVisualValue{text: fmt.Sprintf("%.0f℃", *snapshot.GPUTemperature), raw: *snapshot.GPUTemperature}
		}
		return metricVisualValue{text: "--", raw: 0}
	case "gpu_hotspot_temp", "gpu_hotspot_temperature":
		if snapshot.GPUHotspotTemperature != nil {
			return metricVisualValue{text: fmt.Sprintf("%.0f℃", *snapshot.GPUHotspotTemperature), raw: *snapshot.GPUHotspotTemperature}
		}
		return metricVisualValue{text: "--", raw: 0}
	case "gpu_memory_temp", "gpu_memory_temperature":
		if snapshot.GPUMemoryTemperature != nil {
			return metricVisualValue{text: fmt.Sprintf("%.0f℃", *snapshot.GPUMemoryTemperature), raw: *snapshot.GPUMemoryTemperature}
		}
		return metricVisualValue{text: "--", raw: 0}
	case "memory_used":
		return metricVisualValue{text: fmt.Sprintf("%.1fGB", snapshot.MemoryUsedGiB), raw: snapshot.MemoryUsedGiB}
	case "memory_available":
		available := snapshot.MemoryTotalGiB - snapshot.MemoryUsedGiB
		return metricVisualValue{text: fmt.Sprintf("%.1fGB", available), raw: available}
	case "vol", "volume":
		return metricVisualValue{text: fmt.Sprintf("%d%%", snapshot.Volume), raw: float64(snapshot.Volume)}
	case "cur_db", "current_decibels":
		return metricVisualValue{text: fmt.Sprintf("%.0fdB", snapshot.CurrentDecibels), raw: snapshot.CurrentDecibels}
	case "avg_db", "average_decibels":
		return metricVisualValue{text: fmt.Sprintf("%.0fdB", snapshot.AverageDecibels), raw: snapshot.AverageDecibels}
	case "max_db", "maximum_decibels":
		return metricVisualValue{text: fmt.Sprintf("%.0fdB", snapshot.MaximumDecibels), raw: snapshot.MaximumDecibels}
	case "time":
		return metricVisualValue{text: now.Format("15:04:05"), raw: fallback.raw}
	case "date":
		return metricVisualValue{text: now.Format("2006-01-02"), raw: fallback.raw}
	case "week":
		return metricVisualValue{text: [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[now.Weekday()], raw: fallback.raw}
	case "cpu_model":
		if snapshot.CPUModel != "" {
			return metricVisualValue{text: snapshot.CPUModel, raw: fallback.raw}
		}
	case "gpu_model":
		if snapshot.GPUModel != "" {
			return metricVisualValue{text: snapshot.GPUModel, raw: fallback.raw}
		}
		return metricVisualValue{text: "--", raw: 0}
	}
	return fallback
}

func componentTelemetryAvailable(state State) bool {
	if state.TelemetryEnabled {
		return true
	}
	snapshot := state.Telemetry
	return !snapshot.CapturedAt.IsZero() || snapshot.CPUPercent != 0 || snapshot.MemoryPercent != 0 ||
		snapshot.MemoryUsedGiB != 0 || snapshot.MemoryTotalGiB != 0 || snapshot.CPUModel != "" || snapshot.GPUModel != "" ||
		snapshot.GPUPercent != nil || snapshot.GPUTemperature != nil || snapshot.GPUHotspotTemperature != nil ||
		snapshot.GPUMemoryTemperature != nil || snapshot.CurrentDecibels != 0 ||
		snapshot.AverageDecibels != 0 || snapshot.MaximumDecibels != 0 || snapshot.Volume != 0
}

func defaultComponentMetric(key string, now time.Time) metricVisualValue {
	switch strings.ToLower(key) {
	case "gpu_load", "gpu_percent":
		return metricVisualValue{text: "63%", raw: 63}
	case "memory_load", "memory_percent":
		return metricVisualValue{text: "58%", raw: 58}
	case "gpu_temp", "gpu_temperature", "gpu_hotspot_temp", "gpu_hotspot_temperature", "gpu_memory_temp", "gpu_memory_temperature":
		return metricVisualValue{text: "45℃", raw: 45}
	case "memory_used":
		return metricVisualValue{text: "18.6GB", raw: 18.6}
	case "memory_available":
		return metricVisualValue{text: "13.4GB", raw: 13.4}
	case "time":
		return metricVisualValue{text: now.Format("15:04:05"), raw: 72}
	case "date":
		return metricVisualValue{text: now.Format("2006-01-02"), raw: 72}
	case "week":
		return metricVisualValue{text: [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[now.Weekday()], raw: 50}
	case "cpu_model":
		return metricVisualValue{text: "AMD Ryzen", raw: 72}
	case "gpu_model":
		return metricVisualValue{text: "GeForce RTX", raw: 72}
	case "vol", "volume":
		return metricVisualValue{text: "72%", raw: 72}
	case "cur_db", "current_decibels":
		return metricVisualValue{text: "-18dB", raw: -18}
	case "avg_db", "average_decibels":
		return metricVisualValue{text: "-24dB", raw: -24}
	case "max_db", "maximum_decibels":
		return metricVisualValue{text: "-8dB", raw: -8}
	default:
		return metricVisualValue{text: "42%", raw: 42}
	}
}

func formatComponentMetric(key string, value float64) string {
	switch strings.ToLower(key) {
	case "gpu_temp", "gpu_temperature", "gpu_hotspot_temp", "gpu_hotspot_temperature", "gpu_memory_temp", "gpu_memory_temperature":
		return fmt.Sprintf("%.0f℃", value)
	case "memory_used", "memory_available":
		return fmt.Sprintf("%.1fGB", value)
	case "cur_db", "avg_db", "max_db", "current_decibels", "average_decibels", "maximum_decibels":
		return fmt.Sprintf("%.0fdB", value)
	default:
		return fmt.Sprintf("%.0f%%", value)
	}
}

func componentMetricLabel(key string) string {
	switch strings.ToLower(key) {
	case "cpu_freq", "cpu_percent":
		return "CPU 使用率"
	case "gpu_load", "gpu_percent":
		return "GPU 使用率"
	case "memory_load", "memory_percent":
		return "内存使用率"
	case "gpu_temp", "gpu_temperature":
		return "GPU 温度"
	case "gpu_hotspot_temp", "gpu_hotspot_temperature":
		return "GPU 热点温度"
	case "gpu_memory_temp", "gpu_memory_temperature":
		return "显存温度"
	case "memory_used":
		return "已用内存"
	case "memory_available":
		return "可用内存"
	case "time":
		return "时间"
	case "date":
		return "日期"
	case "week":
		return "星期"
	case "cpu_model":
		return "CPU 型号"
	case "gpu_model":
		return "GPU 型号"
	case "vol", "volume":
		return "音量"
	case "cur_db", "current_decibels":
		return "当前音量"
	case "avg_db", "average_decibels":
		return "平均音量"
	case "max_db", "maximum_decibels":
		return "峰值音量"
	default:
		return key
	}
}

func drawComponentArc(canvas *image.RGBA, centerX, centerY, radius int, startDegrees, endDegrees float64,
	width int, fill color.NRGBA, progress float64) {
	progress = clampVisualUnit(progress)
	if progress <= 0 || radius <= 0 || fill.A == 0 {
		return
	}
	endDegrees = startDegrees + (endDegrees-startDegrees)*progress
	steps := maxInt(8, minInt(720, int(math.Ceil(math.Abs(endDegrees-startDegrees)/2))))
	lastX, lastY := 0, 0
	for index := 0; index <= steps; index++ {
		ratio := float64(index) / float64(steps)
		angle := (startDegrees + (endDegrees-startDegrees)*ratio - 90) * math.Pi / 180
		x := centerX + int(math.Round(math.Cos(angle)*float64(radius)))
		y := centerY + int(math.Round(math.Sin(angle)*float64(radius)))
		if index > 0 {
			drawLine(canvas, lastX, lastY, x, y, width, fill)
		}
		lastX, lastY = x, y
	}
}

func drawComponentGradientArc(canvas *image.RGBA, centerX, centerY, radius int, startDegrees, endDegrees float64,
	width int, progress float64, start, middle, end color.NRGBA) {
	progress = clampVisualUnit(progress)
	if progress <= 0 {
		return
	}
	segments := 48
	active := maxInt(1, int(math.Ceil(float64(segments)*progress)))
	for index := 0; index < active; index++ {
		segmentStart := startDegrees + (endDegrees-startDegrees)*float64(index)/float64(segments)
		segmentEndRatio := math.Min(progress, float64(index+1)/float64(segments))
		segmentEnd := startDegrees + (endDegrees-startDegrees)*segmentEndRatio
		position := float64(index) / float64(segments-1)
		fill := interpolateVisualColor(start, middle, position*2)
		if position > .5 {
			fill = interpolateVisualColor(middle, end, (position-.5)*2)
		}
		drawComponentArc(canvas, centerX, centerY, radius, segmentStart, segmentEnd, width, fill, 1)
	}
}

func drawComponentRectBorder(canvas *image.RGBA, rectangle image.Rectangle, width int, fill color.NRGBA) {
	if rectangle.Empty() || fill.A == 0 {
		return
	}
	width = maxInt(1, minInt(width, minInt(rectangle.Dx(), rectangle.Dy())/2))
	fillRect(canvas, image.Rect(rectangle.Min.X, rectangle.Min.Y, rectangle.Max.X, rectangle.Min.Y+width), fill)
	fillRect(canvas, image.Rect(rectangle.Min.X, rectangle.Max.Y-width, rectangle.Max.X, rectangle.Max.Y), fill)
	fillRect(canvas, image.Rect(rectangle.Min.X, rectangle.Min.Y+width, rectangle.Min.X+width, rectangle.Max.Y-width), fill)
	fillRect(canvas, image.Rect(rectangle.Max.X-width, rectangle.Min.Y+width, rectangle.Max.X, rectangle.Max.Y-width), fill)
}

func fillComponentEllipse(canvas *image.RGBA, bounds image.Rectangle, fill color.NRGBA) {
	bounds = bounds.Intersect(canvas.Bounds())
	if bounds.Empty() || fill.A == 0 {
		return
	}
	radiusX := float64(bounds.Dx()) / 2
	radiusY := float64(bounds.Dy()) / 2
	centerX := float64(bounds.Min.X) + radiusX
	centerY := float64(bounds.Min.Y) + radiusY
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		normalizedY := (float64(y) + .5 - centerY) / radiusY
		span := radiusX * math.Sqrt(math.Max(0, 1-normalizedY*normalizedY))
		left := maxInt(bounds.Min.X, int(math.Ceil(centerX-span-.5)))
		right := minInt(bounds.Max.X, int(math.Floor(centerX+span-.5))+1)
		fillRect(canvas, image.Rect(left, y, right, y+1), fill)
	}
}

func componentCatmullRomPoint(first, second, third, fourth image.Point, ratio float64) image.Point {
	squared, cubed := ratio*ratio, ratio*ratio*ratio
	coordinate := func(p0, p1, p2, p3 int) int {
		value := .5 * (2*float64(p1) + (-float64(p0)+float64(p2))*ratio +
			(2*float64(p0)-5*float64(p1)+4*float64(p2)-float64(p3))*squared +
			(-float64(p0)+3*float64(p1)-3*float64(p2)+float64(p3))*cubed)
		return int(math.Round(value))
	}
	return image.Pt(coordinate(first.X, second.X, third.X, fourth.X), coordinate(first.Y, second.Y, third.Y, fourth.Y))
}
