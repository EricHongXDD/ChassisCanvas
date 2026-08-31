package render

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"
	"time"

	"casescreen/internal/audio"
	"casescreen/internal/theme"
	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

type lyricTextPaint struct {
	fill         color.NRGBA
	outline      color.NRGBA
	outlineWidth int
	shadow       color.NRGBA
	shadowX      int
	shadowY      int
	shadowBlur   int
	glow         color.NRGBA
	glowRadius   int
	fontWeight   string
}

type audioVisualRuntime struct {
	previous  []float64
	peaks     []float64
	heldUntil []time.Time
	updatedAt time.Time
}

// audioVisualFrame 为主机合成设备保留平滑与峰值状态。微雪采用同一算法，
// 但状态由小包服务持有；两条链路在遇到全零输入时都会立刻清空
// 历史波峰，最终绘制阶段再独立加入低幅静态基线。
func (renderer *Renderer) audioVisualFrame(layer theme.Layer, bounds image.Rectangle, frame audio.SpectrumFrame) ([]float64, []float64) {
	count := visualAudioBandCount(layer, bounds)
	key := layer.ID
	if key == "" {
		key = fmt.Sprintf("%s:%.4f:%.4f:%.4f:%.4f", layer.Type, layer.Bounds.X, layer.Bounds.Y, layer.Bounds.Width, layer.Bounds.Height)
	}
	runtime := renderer.audio[key]
	values := theme.TransformAudioBands(frame.Bars, count, layer, runtime.previous)
	if visualBandsSilent(values) {
		delete(renderer.audio, key)
		return values, nil
	}
	capturedAt := frame.CapturedAt
	if capturedAt.IsZero() {
		if runtime.updatedAt.IsZero() {
			capturedAt = time.Unix(0, 0)
		} else {
			capturedAt = runtime.updatedAt.Add(100 * time.Millisecond)
		}
	}
	if capturedAt.Before(runtime.updatedAt) {
		capturedAt = runtime.updatedAt
	}
	peaks, heldUntil := updateHostVisualPeaks(values, runtime, layer, capturedAt)
	renderer.audio[key] = audioVisualRuntime{previous: values, peaks: peaks, heldUntil: heldUntil, updatedAt: capturedAt}
	return values, peaks
}

func visualAudioBandCount(layer theme.Layer, bounds image.Rectangle) int {
	if layer.Type == theme.LayerAudioRing || (layer.Type == theme.LayerAudioBars && theme.AudioBarsUseRadialLayout(layer)) {
		return theme.WaveshareAudioRingCount(layer)
	}
	count := maxInt(6, minInt(96, optionInt(layer.Options, "count", theme.DefaultAudioBarsCount)))
	if strings.EqualFold(optionString(layer.Options, "direction", "bottom"), "horizontal") {
		return minInt(count, maxInt(1, bounds.Dy()))
	}
	return minInt(count, maxInt(1, bounds.Dx()))
}

func updateHostVisualPeaks(values []float64, runtime audioVisualRuntime, layer theme.Layer, capturedAt time.Time) ([]float64, []time.Time) {
	if !theme.OptionBool(layer.Options, "peak_hold", false) {
		return nil, nil
	}
	duration := time.Duration(math.Max(0, math.Min(5000, theme.OptionFloat(layer.Options, "peak_hold_ms", 450)))) * time.Millisecond
	decay := math.Max(.05, math.Min(10, theme.OptionFloat(layer.Options, "peak_decay", 1.8)))
	elapsed := .1
	if !runtime.updatedAt.IsZero() && capturedAt.After(runtime.updatedAt) {
		elapsed = math.Min(1, capturedAt.Sub(runtime.updatedAt).Seconds())
	}
	peaks := make([]float64, len(values))
	heldUntil := make([]time.Time, len(values))
	for index, value := range values {
		peak := value
		if index < len(runtime.peaks) {
			peak = runtime.peaks[index]
		}
		if index < len(runtime.heldUntil) {
			heldUntil[index] = runtime.heldUntil[index]
		}
		if value >= peak {
			peak = value
			heldUntil[index] = capturedAt.Add(duration)
		} else if !capturedAt.Before(heldUntil[index]) {
			peak = math.Max(value, peak-decay*elapsed)
		}
		peaks[index] = clampVisualUnit(peak)
	}
	return peaks, heldUntil
}

// drawArtisticLyrics 绘制具有当前/下一句层次、描边、柔光和卡拉 OK 渐进高亮的歌词。
// 它只使用已有的当前句进度，不额外制造播放时间或歌词状态。
func (renderer *Renderer) drawArtisticLyrics(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, lyrics LyricState) {
	// Current 为空表示播放器尚未提供可显示歌词。必须在绘制面板、边框和
	// 进度轨之前返回；Playing=false 不能作为隐藏条件，因为暂停时应保留画面。
	if bounds.Empty() || strings.TrimSpace(lyrics.Current) == "" {
		return
	}
	if lyrics.Next == "" {
		lyrics.Next = optionString(layer.Options, "placeholder_next", "播放歌曲后显示下一句")
	}
	style := theme.LyricsStyle(layer)
	radius := maxInt(2, minInt(bounds.Dy()/3, optionInt(layer.Options, "corner_radius", maxInt(8, bounds.Dy()/9))))
	panelFallback := color.NRGBA{R: 3, G: 10, B: 18, A: 146}
	if style == theme.LyricsStyleMinimal {
		panelFallback = color.NRGBA{}
	}
	panel := optionColor(layer.Options, "panel_color", panelFallback)
	panel.A = multiplyAlpha(panel.A, layer.Opacity)
	border := optionColor(layer.Options, "panel_border_color", color.NRGBA{R: 102, G: 227, B: 255, A: 62})
	border.A = multiplyAlpha(border.A, layer.Opacity)
	if border.A > 0 && style != theme.LyricsStyleMinimal {
		fillRoundedPanel(canvas, bounds, radius, border)
		inner := insetVisualRect(bounds, 1, 1, 1, 1)
		if !inner.Empty() {
			fillRoundedPanel(canvas, inner, maxInt(1, radius-1), panel)
		}
	} else if panel.A > 0 {
		fillRoundedPanel(canvas, bounds, radius, panel)
	}

	paddingX := maxInt(4, minInt(bounds.Dx()/8, optionInt(layer.Options, "padding_x", maxInt(10, bounds.Dx()/28))))
	paddingY := maxInt(2, minInt(bounds.Dy()/5, optionInt(layer.Options, "padding_y", maxInt(4, bounds.Dy()/18))))
	textWidth := maxInt(1, bounds.Dx()-paddingX*2)
	resourceID := renderer.lyricFontResource(layer)
	preferredSize := optionFloat(layer.Options, "font_size", theme.DefaultLyricsFontSize)
	preferredNextSize := optionFloat(layer.Options, "next_font_size", theme.DefaultLyricsNextFontSize)
	switch style {
	case theme.LyricsStyleKaraoke:
		preferredSize *= 1.14
	case theme.LyricsStyleClassic:
		preferredSize *= .92
	case theme.LyricsStyleMinimal:
		preferredSize *= 1.06
	}
	currentFace := renderer.fittedFace(resourceID, preferredSize, []string{lyrics.Current}, textWidth, maxInt(1, int(float64(bounds.Dy())*.55)))
	nextFace := renderer.fittedFace(resourceID, preferredNextSize, []string{lyrics.Next}, textWidth, maxInt(1, int(float64(bounds.Dy())*.28)))
	showNext := theme.LyricsShowsNext(layer) && strings.TrimSpace(lyrics.Next) != "" && fontHeight(currentFace)+fontHeight(nextFace)+paddingY*2+4 <= bounds.Dy()
	currentText := fitTextToWidth(currentFace, lyrics.Current, textWidth)
	nextText := fitTextToWidth(nextFace, lyrics.Next, textWidth)
	align := strings.ToLower(optionString(layer.Options, "align", "center"))
	currentX := alignedTextX(currentFace, currentText, bounds.Min.X+paddingX, textWidth, align)
	nextX := alignedTextX(nextFace, nextText, bounds.Min.X+paddingX, textWidth, align)

	currentCenterY := bounds.Min.Y + bounds.Dy()/2
	nextCenterY := bounds.Min.Y + bounds.Dy()*73/100
	if showNext {
		switch style {
		case theme.LyricsStyleKaraoke:
			currentCenterY = bounds.Min.Y + bounds.Dy()*46/100
			nextCenterY = bounds.Min.Y + bounds.Dy()*76/100
		case theme.LyricsStyleClassic:
			currentCenterY = bounds.Min.Y + bounds.Dy()*30/100
			nextCenterY = bounds.Min.Y + bounds.Dy()*70/100
		default:
			currentCenterY = bounds.Min.Y + bounds.Dy()*37/100
			nextCenterY = bounds.Min.Y + bounds.Dy()*70/100
		}
	}
	currentBaseline := centeredTextBaseline(currentFace, currentCenterY)
	nextBaseline := centeredTextBaseline(nextFace, nextCenterY)
	progress := clampVisualUnit(lyrics.Progress)
	transitionAlpha := 1.0
	if theme.LyricsUsesSmoothTransition(layer) && lyrics.PositionKnown {
		transitionWindow := math.Max(.01, math.Min(.35, optionFloat(layer.Options, "transition_fraction", .08)))
		transitionAlpha = smoothVisualStep(clampVisualUnit(progress / transitionWindow))
		entryOffset := optionInt(layer.Options, "transition_offset", maxInt(2, bounds.Dy()/18))
		currentBaseline += int(math.Round(float64(entryOffset) * (1 - transitionAlpha)))
		transitionAlpha = .28 + transitionAlpha*.72
	}

	primary := optionColor(layer.Options, "current_color", optionColor(layer.Options, "color", color.NRGBA{R: 247, G: 252, B: 255, A: 255}))
	secondary := optionColor(layer.Options, "next_color", color.NRGBA{R: 184, G: 200, B: 218, A: 164})
	accent := optionColor(layer.Options, "accent_color", color.NRGBA{R: 85, G: 229, B: 255, A: 255})
	primary.A = multiplyAlpha(primary.A, layer.Opacity*transitionAlpha)
	secondary.A = multiplyAlpha(secondary.A, layer.Opacity)
	accent.A = multiplyAlpha(accent.A, layer.Opacity*transitionAlpha)
	currentPaint := lyricPaintFromOptions(layer, primary, style, transitionAlpha)
	nextPaint := lyricPaintFromOptions(layer, secondary, "next", 1)
	currentBaseline, currentPaint = fitLyricBaselineAndEffects(bounds, currentFace, currentBaseline, currentPaint)
	if showNext {
		nextBaseline, nextPaint = fitLyricBaselineAndEffects(bounds, nextFace, nextBaseline, nextPaint)
	}
	textCanvas := clippedLayerCanvas(canvas, bounds)
	decoration := accent
	decoration.A = multiplyAlpha(decoration.A, .38)
	switch style {
	case theme.LyricsStyleKaraoke:
		badgeText := "卡拉 OK · 正在唱"
		badgeSize := math.Max(6, preferredNextSize*.58)
		badgeFace := renderer.fittedFace(resourceID, badgeSize, []string{badgeText}, textWidth, maxInt(1, bounds.Dy()/5))
		badgeText = fitTextToWidth(badgeFace, badgeText, textWidth)
		badgeX := alignedTextX(badgeFace, badgeText, bounds.Min.X+paddingX, textWidth, align)
		badgeBaseline := bounds.Min.Y + paddingY + badgeFace.Metrics().Ascent.Ceil()
		badgeColor := accent
		badgeColor.A = multiplyAlpha(badgeColor.A, .76)
		badgePaint := lyricPaintFromOptions(layer, badgeColor, "next", 1)
		badgeBaseline, badgePaint = fitLyricBaselineAndEffects(bounds, badgeFace, badgeBaseline, badgePaint)
		drawLyricStyledText(textCanvas, badgeFace, badgeX, badgeBaseline, badgeText, badgePaint)
		stripeY := bounds.Min.Y + maxInt(2, bounds.Dy()/5)
		fillRoundedPanel(canvas, image.Rect(bounds.Min.X+paddingX, stripeY, bounds.Max.X-paddingX, stripeY+1), 0, badgeColor)
	case theme.LyricsStyleClassic:
		dividerY := bounds.Min.Y + bounds.Dy()/2
		fillRoundedPanel(canvas, image.Rect(bounds.Min.X+paddingX, dividerY, bounds.Max.X-paddingX, dividerY+1), 0, decoration)
		markerWidth := maxInt(1, minInt(3, bounds.Dx()/80+1))
		fillRoundedPanel(canvas, image.Rect(bounds.Min.X+paddingX, bounds.Min.Y+paddingY,
			bounds.Min.X+paddingX+markerWidth, bounds.Max.Y-paddingY), markerWidth/2, accent)
	}
	drawLyricStyledText(textCanvas, currentFace, currentX, currentBaseline, currentText, currentPaint)
	if showNext {
		drawLyricStyledText(textCanvas, nextFace, nextX, nextBaseline, nextText, nextPaint)
	}

	highlight := theme.LyricsUsesKaraokeHighlight(layer)
	if highlight && progress > 0 && currentText != "" {
		textAdvance := font.MeasureString(currentFace, currentText).Ceil()
		reveal := int(math.Round(float64(textAdvance) * smoothVisualStep(progress)))
		clipBounds := image.Rect(currentX-maxInt(8, currentPaint.glowRadius+currentPaint.outlineWidth), bounds.Min.Y,
			minInt(bounds.Max.X, currentX+reveal+maxInt(2, currentPaint.glowRadius)), bounds.Max.Y).Intersect(textCanvas.Bounds())
		if !clipBounds.Empty() {
			highlightCanvas := textCanvas.SubImage(clipBounds).(*image.RGBA)
			highlightPaint := currentPaint
			highlightPaint.fill = accent
			highlightPaint.glow = optionColor(layer.Options, "glow_color", color.NRGBA{R: accent.R, G: accent.G, B: accent.B, A: 92})
			highlightPaint.glow.A = multiplyAlpha(highlightPaint.glow.A, layer.Opacity*transitionAlpha)
			drawLyricStyledText(highlightCanvas, currentFace, currentX, currentBaseline, currentText, highlightPaint)
		}
	}

	showProgress := theme.LyricsShowsProgress(layer)
	if showProgress {
		trackHeight := maxInt(1, minInt(5, optionInt(layer.Options, "progress_thickness", maxInt(2, bounds.Dy()/45))))
		left := bounds.Min.X + paddingX
		right := bounds.Max.X - paddingX
		y := bounds.Max.Y - paddingY - trackHeight
		track := optionColor(layer.Options, "progress_track_color", color.NRGBA{R: 132, G: 162, B: 180, A: 52})
		track.A = multiplyAlpha(track.A, layer.Opacity)
		if style == theme.LyricsStyleKaraoke {
			segments := maxInt(4, minInt(12, (right-left)/24))
			gap := maxInt(1, minInt(3, (right-left)/maxInt(1, segments*8)))
			for index := 0; index < segments; index++ {
				segment := image.Rect(left+index*(right-left)/segments, y, left+(index+1)*(right-left)/segments-gap, y+trackHeight)
				if segment.Empty() {
					continue
				}
				fill := track
				if lyrics.PositionKnown && float64(index+1)/float64(segments) <= progress+.001 {
					fill = accent
				}
				fillRoundedPanel(canvas, segment, trackHeight/2, fill)
			}
		} else {
			if style == theme.LyricsStyleClassic {
				trackHeight = 1
				y = bounds.Max.Y - paddingY - trackHeight
			}
			fillRoundedPanel(canvas, image.Rect(left, y, right, y+trackHeight), trackHeight/2, track)
			if lyrics.PositionKnown && progress > 0 {
				progressRight := left + int(math.Round(float64(right-left)*progress))
				fillRoundedPanel(canvas, image.Rect(left, y, maxInt(left+1, progressRight), y+trackHeight), trackHeight/2, accent)
			}
		}
	}
}

// fitLyricBaselineAndEffects 在小高度图层中先移动基线，再按可用留白收窄柔光、
// 描边和阴影，保证当前句主体与装饰都不会被图层边缘裁掉。
func fitLyricBaselineAndEffects(bounds image.Rectangle, face font.Face, baseline int, paint lyricTextPaint) (int, lyricTextPaint) {
	metrics := face.Metrics()
	ascent, descent := metrics.Ascent.Ceil(), metrics.Descent.Ceil()
	textHeight := maxInt(1, ascent+descent)
	const edgeReserve = 2
	availableEffects := maxInt(0, (bounds.Dy()-textHeight-edgeReserve*2)/2)
	paint.outlineWidth = minInt(paint.outlineWidth, availableEffects)
	paint.glowRadius = minInt(paint.glowRadius, availableEffects)
	paint.shadowBlur = minInt(paint.shadowBlur, availableEffects)
	paint.shadowY = maxInt(-availableEffects, minInt(availableEffects, paint.shadowY))
	topEffect := maxInt(paint.outlineWidth, paint.glowRadius)
	bottomEffect := topEffect
	if paint.shadow.A > 0 {
		topEffect = maxInt(topEffect, paint.shadowBlur+maxInt(0, -paint.shadowY))
		bottomEffect = maxInt(bottomEffect, paint.shadowBlur+maxInt(0, paint.shadowY))
	}
	minimum := bounds.Min.Y + edgeReserve + ascent + topEffect
	maximum := bounds.Max.Y - edgeReserve - descent - bottomEffect
	if minimum <= maximum {
		baseline = maxInt(minimum, minInt(maximum, baseline))
	} else {
		baseline = centeredTextBaseline(face, bounds.Min.Y+bounds.Dy()/2)
	}
	return baseline, paint
}

func (renderer *Renderer) lyricFontResource(layer theme.Layer) string {
	return componentFontResource(renderer, layer)
}

func lyricPaintFromOptions(layer theme.Layer, fill color.NRGBA, style string, alpha float64) lyricTextPaint {
	defaultOutlineWidth := 1
	defaultGlowRadius := 2
	defaultShadowBlur := 2
	if style == "player" || style == "karaoke" {
		defaultOutlineWidth = 2
		defaultGlowRadius = 4
		defaultShadowBlur = 3
	} else if style == "minimal" || style == "next" {
		defaultOutlineWidth = 0
		defaultGlowRadius = 0
	}
	outline := optionColor(layer.Options, "outline_color", color.NRGBA{R: 2, G: 7, B: 14, A: 225})
	shadow := optionColor(layer.Options, "shadow_color", color.NRGBA{R: 0, G: 0, B: 0, A: 180})
	glow := optionColor(layer.Options, "glow_color", color.NRGBA{R: 83, G: 218, B: 255, A: 66})
	if style == "next" {
		shadow.A /= 2
		glow.A = 0
	}
	outline.A = multiplyAlpha(outline.A, layer.Opacity*alpha)
	shadow.A = multiplyAlpha(shadow.A, layer.Opacity*alpha)
	glow.A = multiplyAlpha(glow.A, layer.Opacity*alpha)
	shadowEnabled := theme.OptionBool(layer.Options, "shadow", style != "minimal")
	if !shadowEnabled {
		shadow.A = 0
	}
	return lyricTextPaint{
		fill: fill, outline: outline,
		outlineWidth: maxInt(0, minInt(8, optionInt(layer.Options, "outline_width", defaultOutlineWidth))),
		shadow:       shadow, shadowX: optionInt(layer.Options, "shadow_offset_x", 2), shadowY: optionInt(layer.Options, "shadow_offset_y", 3),
		shadowBlur: maxInt(0, minInt(8, optionInt(layer.Options, "shadow_blur", defaultShadowBlur))),
		glow:       glow, glowRadius: maxInt(0, minInt(12, optionInt(layer.Options, "glow_radius", defaultGlowRadius))),
		fontWeight: layerFontWeight(layer),
	}
}

func drawLyricStyledText(canvas *image.RGBA, face font.Face, x, baseline int, textValue string, paint lyricTextPaint) {
	if canvas == nil || textValue == "" || paint.fill.A == 0 {
		return
	}
	if paint.glow.A > 0 && paint.glowRadius > 0 {
		drawTextDisc(canvas, face, x, baseline, textValue, paint.glow, paint.glowRadius, 2)
	}
	if paint.shadow.A > 0 {
		if paint.shadowBlur > 0 {
			drawTextDisc(canvas, face, x+paint.shadowX, baseline+paint.shadowY, textValue, paint.shadow, paint.shadowBlur, 1)
		} else {
			drawGlyphText(canvas, face, x+paint.shadowX, baseline+paint.shadowY, textValue, paint.shadow)
		}
	}
	if paint.outline.A > 0 && paint.outlineWidth > 0 {
		drawTextDisc(canvas, face, x, baseline, textValue, paint.outline, paint.outlineWidth, 1)
	}
	fill := paint.fill
	if paint.fontWeight == "light" {
		// 用户字体可能只有一张字脸；透明度细化是 light 的安全后备。
		// 若系统存在真实 light 字脸，字体选择仍会优先使用它。
		fill = withVisualAlpha(fill, .68)
	}
	drawGlyphText(canvas, face, x, baseline, textValue, fill)
	if paint.fontWeight == "bold" {
		drawGlyphText(canvas, face, x+1, baseline, textValue, withVisualAlpha(paint.fill, .72))
		drawGlyphText(canvas, face, x, baseline+1, textValue, withVisualAlpha(paint.fill, .46))
	} else if paint.fontWeight == "medium" {
		drawGlyphText(canvas, face, x+1, baseline, textValue, withVisualAlpha(paint.fill, .34))
	}
}

func drawTextDisc(canvas *image.RGBA, face font.Face, x, baseline int, textValue string, fill color.NRGBA, radius, step int) {
	if radius <= 0 {
		drawGlyphText(canvas, face, x, baseline, textValue, fill)
		return
	}
	step = maxInt(1, step)
	points := 0
	for y := -radius; y <= radius; y += step {
		for x := -radius; x <= radius; x += step {
			if x*x+y*y <= radius*radius {
				points++
			}
		}
	}
	// 多次 Over 合成会累积透明度，先按采样点数量衰减以形成柔和边缘。
	soft := withVisualAlpha(fill, math.Min(1, 3/math.Sqrt(float64(maxInt(1, points)))))
	for offsetY := -radius; offsetY <= radius; offsetY += step {
		for offsetX := -radius; offsetX <= radius; offsetX += step {
			if offsetX*offsetX+offsetY*offsetY <= radius*radius {
				drawGlyphText(canvas, face, x+offsetX, baseline+offsetY, textValue, soft)
			}
		}
	}
}

func drawGlyphText(canvas *image.RGBA, face font.Face, x, baseline int, textValue string, fill color.NRGBA) {
	if fill.A == 0 {
		return
	}
	drawer := font.Drawer{Dst: canvas, Src: image.NewUniform(fill), Face: face, Dot: fixed.P(x, baseline)}
	drawer.DrawString(textValue)
}

func alignedTextX(face font.Face, textValue string, left, width int, align string) int {
	advance := font.MeasureString(face, textValue).Ceil()
	switch align {
	case "left":
		return left
	case "right":
		return left + maxInt(0, width-advance)
	default:
		return left + maxInt(0, (width-advance)/2)
	}
}

func centeredTextBaseline(face font.Face, centerY int) int {
	metrics := face.Metrics()
	return centerY + (metrics.Ascent.Ceil()-metrics.Descent.Ceil())/2
}

func drawVisualBars(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, input []float64) {
	if bounds.Empty() {
		return
	}
	values := theme.TransformAudioBands(input, visualAudioBandCount(layer, bounds), layer, nil)
	drawVisualBarsPrepared(canvas, bounds, layer, values, nil)
}

func drawVisualBarsPrepared(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, values, peaks []float64) {
	if bounds.Empty() {
		return
	}
	panel := optionColor(layer.Options, "panel_color", color.NRGBA{})
	panel.A = multiplyAlpha(panel.A, layer.Opacity)
	if panel.A > 0 {
		fillRoundedPanel(canvas, bounds, maxInt(2, optionInt(layer.Options, "corner_radius", 8)), panel)
	}
	// 峰值与平滑在上游仍以全零清空；只在绘制时留下静态基线。
	values = theme.AudioBandsWithIdleBaseline(values)
	direction := strings.ToLower(optionString(layer.Options, "direction", "bottom"))
	style := strings.ToLower(optionString(layer.Options, "style", "equalizer"))
	count := len(values)
	if count == 0 {
		return
	}
	start := optionColor(layer.Options, "gradient_start", optionColor(layer.Options, "color", color.NRGBA{R: 76, G: 224, B: 255, A: 235}))
	end := optionColor(layer.Options, "gradient_end", color.NRGBA{R: 139, G: 92, B: 246, A: 235})
	start.A = multiplyAlpha(start.A, layer.Opacity)
	end.A = multiplyAlpha(end.A, layer.Opacity)
	peakColor := optionColor(layer.Options, "peak_color", color.NRGBA{R: 255, G: 242, B: 117, A: 225})
	peakColor.A = multiplyAlpha(peakColor.A, layer.Opacity)
	if direction == "fourway" || style == "burst" {
		drawSpectrumBurst(canvas, bounds, layer, values, peaks, start, end, peakColor)
		return
	}
	if style == "wave" || style == "waveform" {
		drawSpectrumWaveform(canvas, bounds, layer, values, peaks, start, end, peakColor)
		return
	}
	gap := maxInt(0, minInt(24, optionInt(layer.Options, "gap", 3)))
	length := bounds.Dx()
	if direction == "horizontal" {
		length = bounds.Dy()
	}
	cell, actualGap := packedVisualLayout(length, count, gap)
	thickness := optionInt(layer.Options, "thickness", maxInt(1, int(math.Round(float64(cell)*.52))))
	thickness = maxInt(1, minInt(cell, thickness))
	shape := strings.ToLower(optionString(layer.Options, "bar_shape", "rounded"))
	glow := theme.OptionBool(layer.Options, "glow", style == "neon")
	glowRadius := maxInt(1, minInt(16, optionInt(layer.Options, "glow_radius", 6)))
	for index, value := range values {
		if value <= 0 {
			continue
		}
		fill := interpolateVisualColor(start, end, float64(index)/float64(maxInt(1, count-1)))
		if direction == "horizontal" {
			y := bounds.Min.Y + index*(cell+actualGap) + (cell-thickness)/2
			barLength := maxInt(1, int(math.Round(float64(bounds.Dx())*value)))
			rectangle := image.Rect(bounds.Min.X, y, minInt(bounds.Max.X, bounds.Min.X+barLength), y+thickness)
			drawSpectrumBarShape(canvas, rectangle, shape, style, fill, glow, glowRadius)
			drawBarsPeakMarker(canvas, bounds, rectangle, direction, valueAtVisual(peaks, index), thickness, peakColor)
			continue
		}
		x := bounds.Min.X + index*(cell+actualGap) + (cell-thickness)/2
		barHeight := maxInt(1, int(math.Round(float64(bounds.Dy())*value)))
		var rectangle image.Rectangle
		switch direction {
		case "top", "down":
			rectangle = image.Rect(x, bounds.Min.Y, x+thickness, minInt(bounds.Max.Y, bounds.Min.Y+barHeight))
		case "center":
			center := bounds.Min.Y + bounds.Dy()/2
			half := maxInt(1, barHeight/2)
			rectangle = image.Rect(x, maxInt(bounds.Min.Y, center-half), x+thickness, minInt(bounds.Max.Y, center+half))
		default:
			rectangle = image.Rect(x, maxInt(bounds.Min.Y, bounds.Max.Y-barHeight), x+thickness, bounds.Max.Y)
		}
		drawSpectrumBarShape(canvas, rectangle, shape, style, fill, glow, glowRadius)
		drawBarsPeakMarker(canvas, bounds, rectangle, direction, valueAtVisual(peaks, index), thickness, peakColor)
		if style == "mirror" && direction != "center" {
			reflectionHeight := maxInt(1, barHeight/3)
			reflection := image.Rect(x, bounds.Min.Y, x+thickness, minInt(bounds.Max.Y, bounds.Min.Y+reflectionHeight))
			drawSpectrumBarShape(canvas, reflection, shape, "equalizer", withVisualAlpha(fill, .28), false, 0)
		}
	}
}

func drawSpectrumBurst(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, values, peaks []float64, start, end, peakColor color.NRGBA) {
	centerX := float64(bounds.Min.X+bounds.Max.X) / 2
	centerY := float64(bounds.Min.Y+bounds.Max.Y) / 2
	outer := float64(minInt(bounds.Dx(), bounds.Dy())) * .48
	inner := outer * math.Max(.02, math.Min(.90, optionFloat(layer.Options, "inner_radius", .46)))
	thickness := maxInt(1, minInt(30, optionInt(layer.Options, "thickness", 3)))
	glow := theme.OptionBool(layer.Options, "glow", true)
	glowRadius := maxInt(1, minInt(16, optionInt(layer.Options, "glow_radius", 6)))
	for index, value := range values {
		if value <= 0 {
			continue
		}
		angle := -math.Pi/2 + 2*math.Pi*float64(index)/float64(len(values))
		endRadius := inner + (outer-inner)*value
		fill := interpolateVisualColor(start, end, float64(index)/float64(maxInt(1, len(values)-1)))
		x0, y0 := int(math.Round(centerX+math.Cos(angle)*inner)), int(math.Round(centerY+math.Sin(angle)*inner))
		x1, y1 := int(math.Round(centerX+math.Cos(angle)*endRadius)), int(math.Round(centerY+math.Sin(angle)*endRadius))
		if glow {
			drawSpectrumGlowLine(canvas, x0, y0, x1, y1, thickness, glowRadius, fill)
		}
		drawLine(canvas, x0, y0, x1, y1, thickness, fill)
		peak := valueAtVisual(peaks, index)
		if peak > 0 {
			peakRadius := inner + (outer-inner)*peak
			px, py := int(math.Round(centerX+math.Cos(angle)*peakRadius)), int(math.Round(centerY+math.Sin(angle)*peakRadius))
			fillCircle(canvas, px, py, maxInt(1, thickness/2+1), peakColor)
		}
	}
}

func drawBarsPeakMarker(canvas *image.RGBA, bounds, bar image.Rectangle, direction string, peak float64, thickness int, fill color.NRGBA) {
	if peak <= 0 || fill.A == 0 || bar.Empty() {
		return
	}
	marker := maxInt(1, minInt(3, thickness))
	switch direction {
	case "horizontal":
		x := bounds.Min.X + int(math.Round(float64(bounds.Dx())*peak))
		y := (bar.Min.Y + bar.Max.Y) / 2
		fillRoundedPanel(canvas, image.Rect(x-marker/2, y-thickness/2, x+(marker+1)/2, y+(thickness+1)/2), marker/2, fill)
	case "top", "down":
		y := bounds.Min.Y + int(math.Round(float64(bounds.Dy())*peak))
		fillRoundedPanel(canvas, image.Rect(bar.Min.X, y-marker/2, bar.Max.X, y+(marker+1)/2), marker/2, fill)
	case "center":
		center := bounds.Min.Y + bounds.Dy()/2
		half := int(math.Round(float64(bounds.Dy()) * peak / 2))
		for _, y := range []int{center - half, center + half} {
			fillRoundedPanel(canvas, image.Rect(bar.Min.X, y-marker/2, bar.Max.X, y+(marker+1)/2), marker/2, fill)
		}
	default:
		y := bounds.Max.Y - int(math.Round(float64(bounds.Dy())*peak))
		fillRoundedPanel(canvas, image.Rect(bar.Min.X, y-marker/2, bar.Max.X, y+(marker+1)/2), marker/2, fill)
	}
}

func valueAtVisual(values []float64, index int) float64 {
	if index < 0 || index >= len(values) {
		return 0
	}
	return clampVisualUnit(values[index])
}

func drawVisualRing(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, input []float64) {
	if bounds.Empty() {
		return
	}
	values := theme.TransformAudioBands(input, visualAudioBandCount(layer, bounds), layer, nil)
	drawVisualRingPrepared(canvas, bounds, layer, values, nil)
}

func drawVisualRingPrepared(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, values, peaks []float64) {
	if bounds.Empty() {
		return
	}
	panel := optionColor(layer.Options, "panel_color", color.NRGBA{})
	panel.A = multiplyAlpha(panel.A, layer.Opacity)
	if panel.A > 0 {
		fillRoundedPanel(canvas, bounds, maxInt(2, optionInt(layer.Options, "corner_radius", 8)), panel)
	}
	// 无声时绘制低幅圆环，但不延续上一帧的峰值。
	values = theme.AudioBandsWithIdleBaseline(values)
	if len(values) == 0 {
		return
	}
	start := optionColor(layer.Options, "gradient_start", optionColor(layer.Options, "color", color.NRGBA{R: 255, G: 125, B: 210, A: 235}))
	end := optionColor(layer.Options, "gradient_end", color.NRGBA{R: 139, G: 92, B: 246, A: 235})
	start.A = multiplyAlpha(start.A, layer.Opacity)
	end.A = multiplyAlpha(end.A, layer.Opacity)
	peakColor := optionColor(layer.Options, "peak_color", color.NRGBA{R: 255, G: 242, B: 117, A: 225})
	peakColor.A = multiplyAlpha(peakColor.A, layer.Opacity)
	centerX := float64(bounds.Min.X+bounds.Max.X) / 2
	centerY := float64(bounds.Min.Y+bounds.Max.Y) / 2
	outer := float64(minInt(bounds.Dx(), bounds.Dy()))/2 - 2
	inner := outer * math.Max(.08, math.Min(.90, optionFloat(layer.Options, "inner_radius", .52)))
	maximumLength := math.Max(1, outer-inner)
	thickness := maxInt(1, minInt(30, optionInt(layer.Options, "thickness", 3)))
	style := strings.ToLower(optionString(layer.Options, "style", "radial"))
	direction := strings.ToLower(optionString(layer.Options, "direction", "outward"))
	startAngle := optionFloat(layer.Options, "start_angle", -90) * math.Pi / 180
	if style == "wave" || style == "waveform" {
		drawSpectrumRingWaveform(canvas, bounds, layer, values, peaks, start, end, peakColor, centerX, centerY, inner, outer, startAngle, direction)
		return
	}
	glow := theme.OptionBool(layer.Options, "glow", style == "halo" || style == "pulse" || style == "neon")
	glowRadius := maxInt(1, minInt(16, optionInt(layer.Options, "glow_radius", 6)))
	for index, value := range values {
		if value <= 0 || (style == "dashed" && index%2 != 0) {
			continue
		}
		angle := startAngle + 2*math.Pi*float64(index)/float64(len(values))
		length := maximumLength * value
		startRadius, endRadius := inner, inner+length
		switch direction {
		case "inward":
			startRadius, endRadius = outer, outer-length
		case "bidirectional":
			middle := (inner + outer) / 2
			startRadius, endRadius = middle-length/2, middle+length/2
		}
		if style == "spokes" {
			startRadius = math.Max(2, inner*.30)
		}
		fill := interpolateVisualColor(start, end, float64(index)/float64(maxInt(1, len(values)-1)))
		x0, y0 := int(math.Round(centerX+math.Cos(angle)*startRadius)), int(math.Round(centerY+math.Sin(angle)*startRadius))
		x1, y1 := int(math.Round(centerX+math.Cos(angle)*endRadius)), int(math.Round(centerY+math.Sin(angle)*endRadius))
		lineThickness := thickness
		if style == "pulse" {
			lineThickness = maxInt(1, int(math.Round(float64(thickness)*(.45+value*.85))))
		}
		if style == "dots" {
			fillCircle(canvas, x1, y1, maxInt(1, lineThickness/2+1), withVisualAlpha(fill, .16))
			fillCircle(canvas, x1, y1, maxInt(1, lineThickness/2), fill)
			drawRingPeakMarker(canvas, centerX, centerY, inner, outer, startAngle, index, len(values), direction, valueAtVisual(peaks, index), lineThickness, peakColor)
			continue
		}
		if glow {
			drawSpectrumGlowLine(canvas, x0, y0, x1, y1, lineThickness, glowRadius, fill)
		}
		drawLine(canvas, x0, y0, x1, y1, lineThickness, fill)
		if style == "halo" {
			haloEnd := math.Max(2, inner-float64(thickness)*2) + length*.35
			hx0 := int(math.Round(centerX + math.Cos(angle)*math.Max(2, inner-float64(thickness)*2)))
			hy0 := int(math.Round(centerY + math.Sin(angle)*math.Max(2, inner-float64(thickness)*2)))
			hx1 := int(math.Round(centerX + math.Cos(angle)*haloEnd))
			hy1 := int(math.Round(centerY + math.Sin(angle)*haloEnd))
			drawLine(canvas, hx0, hy0, hx1, hy1, maxInt(1, lineThickness/2), withVisualAlpha(fill, .62))
		}
		drawRingPeakMarker(canvas, centerX, centerY, inner, outer, startAngle, index, len(values), direction, valueAtVisual(peaks, index), lineThickness, peakColor)
	}
}

func drawRingPeakMarker(canvas *image.RGBA, centerX, centerY, inner, outer, startAngle float64, index, count int, direction string, peak float64, thickness int, fill color.NRGBA) {
	if peak <= 0 || fill.A == 0 || count <= 0 {
		return
	}
	angle := startAngle + 2*math.Pi*float64(index)/float64(count)
	length := math.Max(1, outer-inner) * peak
	radii := []float64{inner + length}
	switch direction {
	case "inward":
		radii[0] = outer - length
	case "bidirectional":
		middle := (inner + outer) / 2
		radii = []float64{middle - length/2, middle + length/2}
	}
	for _, radius := range radii {
		x := int(math.Round(centerX + math.Cos(angle)*radius))
		y := int(math.Round(centerY + math.Sin(angle)*radius))
		fillCircle(canvas, x, y, maxInt(1, minInt(4, thickness/2+1)), fill)
	}
}

// drawSpectrumWaveform 把真实频带顶点连成连续谱线。center 会绘制上下两条
// 对称包络；它与柱状图共用频带值，不使用固定正弦或随机动画。
func drawSpectrumWaveform(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, values, peaks []float64, start, end, peakColor color.NRGBA) {
	if len(values) < 2 || bounds.Empty() {
		return
	}
	direction := strings.ToLower(optionString(layer.Options, "direction", "bottom"))
	thickness := maxInt(1, minInt(12, optionInt(layer.Options, "thickness", 2)))
	glow := theme.OptionBool(layer.Options, "glow", true)
	glowRadius := maxInt(1, minInt(16, optionInt(layer.Options, "glow_radius", 6)))
	makePoints := func(source []float64, mirror bool) []image.Point {
		points := make([]image.Point, len(source))
		for index, value := range source {
			value = clampVisualUnit(value)
			ratio := float64(index) / float64(maxInt(1, len(source)-1))
			switch direction {
			case "horizontal":
				x := bounds.Min.X + int(math.Round(float64(maxInt(0, bounds.Dx()-1))*value))
				y := bounds.Min.Y + int(math.Round(float64(maxInt(0, bounds.Dy()-1))*ratio))
				points[index] = image.Pt(x, y)
			case "top", "down":
				x := bounds.Min.X + int(math.Round(float64(maxInt(0, bounds.Dx()-1))*ratio))
				y := bounds.Min.Y + int(math.Round(float64(maxInt(0, bounds.Dy()-1))*value))
				points[index] = image.Pt(x, y)
			case "center":
				x := bounds.Min.X + int(math.Round(float64(maxInt(0, bounds.Dx()-1))*ratio))
				extent := int(math.Round(float64(maxInt(1, bounds.Dy()-1)) * value / 2))
				y := bounds.Min.Y + bounds.Dy()/2 - extent
				if mirror {
					y = bounds.Min.Y + bounds.Dy()/2 + extent
				}
				points[index] = image.Pt(x, y)
			default:
				x := bounds.Min.X + int(math.Round(float64(maxInt(0, bounds.Dx()-1))*ratio))
				y := bounds.Max.Y - 1 - int(math.Round(float64(maxInt(0, bounds.Dy()-1))*value))
				points[index] = image.Pt(x, y)
			}
		}
		return points
	}
	drawEnvelope := func(source []float64, fillStart, fillEnd color.NRGBA, lineWidth int, alpha float64) {
		if visualBandsSilent(source) {
			return
		}
		paths := [][]image.Point{makePoints(source, false)}
		if direction == "center" {
			paths = append(paths, makePoints(source, true))
		}
		for _, points := range paths {
			drawSpectrumPolyline(canvas, points, false, lineWidth, glow, glowRadius, fillStart, fillEnd, alpha)
		}
	}
	drawEnvelope(peaks, peakColor, peakColor, maxInt(1, thickness/2), .45)
	drawEnvelope(values, start, end, thickness, 1)
}

// drawSpectrumRingWaveform 绘制闭合的径向锯齿线。bidirectional 生成内外
// 两条真实包络，效果对应参考图中的圆环和向外爆发形态。
func drawSpectrumRingWaveform(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, values, peaks []float64,
	start, end, peakColor color.NRGBA, centerX, centerY, inner, outer, startAngle float64, direction string) {
	if len(values) < 3 || bounds.Empty() {
		return
	}
	thickness := maxInt(1, minInt(12, optionInt(layer.Options, "thickness", 2)))
	glow := theme.OptionBool(layer.Options, "glow", true)
	glowRadius := maxInt(1, minInt(16, optionInt(layer.Options, "glow_radius", 6)))
	makePoints := func(source []float64, inwardHalf bool) []image.Point {
		points := make([]image.Point, len(source))
		span := math.Max(1, outer-inner)
		for index, value := range source {
			value = clampVisualUnit(value)
			angle := startAngle + 2*math.Pi*float64(index)/float64(len(source))
			radius := inner + span*value
			switch direction {
			case "inward":
				radius = outer - span*value
			case "bidirectional":
				middle := (inner + outer) / 2
				radius = middle + span*value/2
				if inwardHalf {
					radius = middle - span*value/2
				}
			}
			points[index] = image.Pt(
				int(math.Round(centerX+math.Cos(angle)*radius)),
				int(math.Round(centerY+math.Sin(angle)*radius)),
			)
		}
		return points
	}
	drawEnvelope := func(source []float64, fillStart, fillEnd color.NRGBA, lineWidth int, alpha float64) {
		if visualBandsSilent(source) {
			return
		}
		drawSpectrumPolyline(canvas, makePoints(source, false), true, lineWidth, glow, glowRadius, fillStart, fillEnd, alpha)
		if direction == "bidirectional" {
			drawSpectrumPolyline(canvas, makePoints(source, true), true, lineWidth, glow, glowRadius, fillStart, fillEnd, alpha)
		}
	}
	drawEnvelope(peaks, peakColor, peakColor, maxInt(1, thickness/2), .42)
	drawEnvelope(values, start, end, thickness, 1)
}

func drawSpectrumPolyline(canvas *image.RGBA, points []image.Point, closed bool, thickness int, glow bool, glowRadius int,
	start, end color.NRGBA, alpha float64) {
	if len(points) < 2 {
		return
	}
	segmentCount := len(points) - 1
	if closed {
		segmentCount = len(points)
	}
	for index := 0; index < segmentCount; index++ {
		left := points[index]
		right := points[(index+1)%len(points)]
		fill := interpolateVisualColor(start, end, float64(index)/float64(maxInt(1, segmentCount-1)))
		fill = withVisualAlpha(fill, alpha)
		if glow {
			drawSpectrumGlowLine(canvas, left.X, left.Y, right.X, right.Y, thickness, glowRadius, fill)
		}
		drawLine(canvas, left.X, left.Y, right.X, right.Y, thickness, fill)
	}
}

func drawSpectrumGlowLine(canvas *image.RGBA, x0, y0, x1, y1, thickness, radius int, fill color.NRGBA) {
	radius = maxInt(1, minInt(16, radius))
	drawLine(canvas, x0, y0, x1, y1, thickness+radius*2, withVisualAlpha(fill, .045))
	drawLine(canvas, x0, y0, x1, y1, thickness+radius, withVisualAlpha(fill, .09))
}

func drawSpectrumBarShape(canvas *image.RGBA, rectangle image.Rectangle, shape, style string, fill color.NRGBA, glow bool, glowRadius int) {
	rectangle = rectangle.Intersect(canvas.Bounds())
	if rectangle.Empty() || fill.A == 0 {
		return
	}
	if glow {
		glowRadius = maxInt(1, minInt(16, glowRadius))
		outer := image.Rect(rectangle.Min.X-glowRadius, rectangle.Min.Y-glowRadius, rectangle.Max.X+glowRadius, rectangle.Max.Y+glowRadius)
		middleRadius := maxInt(1, glowRadius/2)
		middle := image.Rect(rectangle.Min.X-middleRadius, rectangle.Min.Y-middleRadius, rectangle.Max.X+middleRadius, rectangle.Max.Y+middleRadius)
		fillRoundedPanel(canvas, outer, maxInt(1, minInt(outer.Dx(), outer.Dy())/2), withVisualAlpha(fill, .045))
		fillRoundedPanel(canvas, middle, maxInt(1, minInt(middle.Dx(), middle.Dy())/2), withVisualAlpha(fill, .10))
	}
	if style == "blocks" {
		vertical := rectangle.Dy() >= rectangle.Dx()
		length := rectangle.Dy()
		if !vertical {
			length = rectangle.Dx()
		}
		segments := maxInt(2, minInt(18, length/maxInt(3, minInt(rectangle.Dx(), rectangle.Dy())+2)))
		for segment := 0; segment < segments; segment++ {
			if vertical {
				y0 := rectangle.Min.Y + segment*rectangle.Dy()/segments
				y1 := rectangle.Min.Y + (segment+1)*rectangle.Dy()/segments - 1
				fillRect(canvas, image.Rect(rectangle.Min.X, y0, rectangle.Max.X, maxInt(y0+1, y1)), fill)
			} else {
				x0 := rectangle.Min.X + segment*rectangle.Dx()/segments
				x1 := rectangle.Min.X + (segment+1)*rectangle.Dx()/segments - 1
				fillRect(canvas, image.Rect(x0, rectangle.Min.Y, maxInt(x0+1, x1), rectangle.Max.Y), fill)
			}
		}
		return
	}
	if shape == "dots" || style == "dots" {
		diameter := maxInt(2, minInt(rectangle.Dx(), rectangle.Dy()))
		radius := maxInt(1, diameter/2)
		if rectangle.Dy() >= rectangle.Dx() {
			for y := rectangle.Max.Y - radius; y >= rectangle.Min.Y+radius-1; y -= maxInt(2, diameter+2) {
				fillCircle(canvas, rectangle.Min.X+rectangle.Dx()/2, y, radius, fill)
			}
		} else {
			for x := rectangle.Min.X + radius; x <= rectangle.Max.X-radius+1; x += maxInt(2, diameter+2) {
				fillCircle(canvas, x, rectangle.Min.Y+rectangle.Dy()/2, radius, fill)
			}
		}
		return
	}
	if style == "outline" {
		stroke := maxInt(1, minInt(2, minInt(rectangle.Dx(), rectangle.Dy())/2))
		fillRect(canvas, image.Rect(rectangle.Min.X, rectangle.Min.Y, rectangle.Max.X, rectangle.Min.Y+stroke), fill)
		fillRect(canvas, image.Rect(rectangle.Min.X, rectangle.Max.Y-stroke, rectangle.Max.X, rectangle.Max.Y), fill)
		fillRect(canvas, image.Rect(rectangle.Min.X, rectangle.Min.Y, rectangle.Min.X+stroke, rectangle.Max.Y), fill)
		fillRect(canvas, image.Rect(rectangle.Max.X-stroke, rectangle.Min.Y, rectangle.Max.X, rectangle.Max.Y), fill)
		return
	}
	radius := 0
	if shape == "rounded" {
		radius = minInt(5, minInt(rectangle.Dx(), rectangle.Dy())/3)
	} else if shape == "capsule" || shape == "pill" || shape == "line" {
		radius = minInt(rectangle.Dx(), rectangle.Dy()) / 2
	}
	fillRoundedPanel(canvas, rectangle, radius, fill)
}

func packedVisualLayout(length, count, requestedGap int) (item, gap int) {
	length = maxInt(1, length)
	count = maxInt(1, minInt(count, length))
	gap = maxInt(0, requestedGap)
	if count > 1 {
		gap = minInt(gap, maxInt(0, (length-count)/(count-1)))
	} else {
		gap = 0
	}
	return maxInt(1, (length-gap*(count-1))/count), gap
}

func visualBandsSilent(values []float64) bool {
	for _, value := range values {
		if value > 0 {
			return false
		}
	}
	return true
}

func interpolateVisualColor(first, second color.NRGBA, ratio float64) color.NRGBA {
	ratio = clampVisualUnit(ratio)
	lerp := func(left, right uint8) uint8 {
		return uint8(math.Round(float64(left)*(1-ratio) + float64(right)*ratio))
	}
	return color.NRGBA{R: lerp(first.R, second.R), G: lerp(first.G, second.G), B: lerp(first.B, second.B), A: lerp(first.A, second.A)}
}

func withVisualAlpha(value color.NRGBA, scale float64) color.NRGBA {
	value.A = uint8(math.Round(float64(value.A) * math.Max(0, math.Min(1, scale))))
	return value
}

func insetVisualRect(bounds image.Rectangle, left, top, right, bottom int) image.Rectangle {
	return image.Rect(bounds.Min.X+left, bounds.Min.Y+top, bounds.Max.X-right, bounds.Max.Y-bottom)
}

func clampVisualUnit(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Max(0, math.Min(1, value))
}

func smoothVisualStep(value float64) float64 {
	value = clampVisualUnit(value)
	return value * value * (3 - 2*value)
}
