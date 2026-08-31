package render

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	stddraw "image/draw"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"casescreen/internal/audio"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
	_ "golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/f64"
	"golang.org/x/image/math/fixed"
	_ "golang.org/x/image/webp"
)

type LyricState struct {
	Current       string
	Next          string
	Progress      float64
	CurrentAt     time.Duration
	NextAt        time.Duration
	Position      time.Duration
	PositionKnown bool
	Playing       bool
}

type State struct {
	Telemetry        telemetry.Snapshot
	TelemetryEnabled bool
	Spectrum         audio.SpectrumFrame
	Lyrics           LyricState
	// VideoFrame 由视频解码器提供；没有新帧时渲染器使用视频图层的 poster_resource_id。
	VideoFrame    image.Image
	VideoSequence uint64
}

type faceKey struct {
	resourceID string
	size       int
}

// Renderer 将设备无关主题合成为主机渲染设备需要的完整画面。
type Renderer struct {
	manifest theme.Manifest
	images   map[string]image.Image
	fonts    map[string]*opentype.Font
	faces    map[faceKey]font.Face
	audio    map[string]audioVisualRuntime
	charts   map[string]chartVisualRuntime

	mu sync.Mutex
}

func New(manifest theme.Manifest) (*Renderer, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	renderer := &Renderer{
		manifest: manifest,
		images:   make(map[string]image.Image),
		fonts:    make(map[string]*opentype.Font),
		faces:    make(map[faceKey]font.Face),
		audio:    make(map[string]audioVisualRuntime),
		charts:   make(map[string]chartVisualRuntime),
	}
	for _, resource := range manifest.Resources {
		extension := strings.ToLower(filepath.Ext(resource.LocalPath))
		switch extension {
		case ".png", ".jpg", ".jpeg", ".gif", ".bmp", ".webp":
			file, err := os.Open(resource.LocalPath)
			if err != nil {
				renderer.Close()
				return nil, fmt.Errorf("打开图片资源 %q: %w", resource.ID, err)
			}
			decoded, _, decodeErr := image.Decode(file)
			closeErr := file.Close()
			if decodeErr != nil {
				renderer.Close()
				return nil, fmt.Errorf("解码图片资源 %q: %w", resource.ID, decodeErr)
			}
			if closeErr != nil {
				renderer.Close()
				return nil, closeErr
			}
			renderer.images[resource.ID] = decoded
		case ".ttf", ".otf", ".ttc", ".otc":
			parsed, err := parseFontFile(resource.LocalPath)
			if err != nil {
				renderer.Close()
				return nil, fmt.Errorf("解析字体资源 %q: %w", resource.ID, err)
			}
			renderer.fonts[resource.ID] = parsed
		}
	}
	for family, systemFont := range loadSystemFonts() {
		renderer.fonts[family] = systemFont
	}
	return renderer, nil
}

func (renderer *Renderer) Close() error {
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	var result error
	for key, face := range renderer.faces {
		if closer, ok := face.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil && result == nil {
				result = err
			}
		}
		delete(renderer.faces, key)
	}
	return result
}

func (renderer *Renderer) Render(width, height int, state State) (*image.RGBA, error) {
	if width <= 0 {
		width = renderer.manifest.Width
	}
	if height <= 0 {
		height = renderer.manifest.Height
	}
	if width <= 0 || height <= 0 {
		return nil, errors.New("渲染分辨率无效")
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()

	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	layers := append([]theme.Layer(nil), renderer.manifest.Layers...)
	sort.SliceStable(layers, func(left, right int) bool {
		return layers[left].ZIndex < layers[right].ZIndex
	})
	for _, layer := range layers {
		// 可见度是独立于透明度的图层状态。静态图片也会复用本渲染器烘焙进
		// 微雪驻留主题，因此必须在任何绘制之前统一过滤，不能只过滤动态控件。
		if !theme.LayerVisible(layer) || layer.Opacity <= 0 {
			continue
		}
		bounds := normalizedRect(layer.Bounds, width, height)
		if layer.Type == theme.LayerImage {
			bounds = rawNormalizedRect(layer.Bounds, width, height)
		}
		switch layer.Type {
		case theme.LayerCanvas:
			renderer.drawCanvas(canvas, layer)
		case theme.LayerImage:
			renderer.drawImageLayer(canvas, bounds, layer)
		case theme.LayerBackgroundImage:
			renderer.drawBackground(canvas, bounds, layer, nil)
		case theme.LayerBackgroundVideo:
			renderer.drawBackground(canvas, bounds, layer, state.VideoFrame)
		case theme.LayerTelemetry:
			// 即使采集暂时关闭，也绘制组件外壳和占位数据，避免应用后只剩图片。
			renderer.drawTelemetry(canvas, bounds, layer, state)
		case theme.LayerAudioBars:
			values, peaks := renderer.audioVisualFrame(layer, bounds, state.Spectrum)
			drawVisualBarsPrepared(canvas, bounds, layer, values, peaks)
		case theme.LayerAudioRing:
			values, peaks := renderer.audioVisualFrame(layer, bounds, state.Spectrum)
			drawVisualRingPrepared(canvas, bounds, layer, values, peaks)
		case theme.LayerLyrics:
			renderer.drawLyrics(canvas, bounds, layer, state.Lyrics)
		case theme.LayerText:
			renderer.drawTextLayer(canvas, bounds, layer)
		case theme.LayerClock:
			renderer.drawClockLayer(canvas, bounds, layer, state.Telemetry.CapturedAt)
		case theme.LayerProgressBar:
			renderer.drawProgressLayer(canvas, bounds, layer, state)
		case theme.LayerGauge, theme.LayerGradientGauge:
			renderer.drawGaugeLayer(canvas, bounds, layer, state)
		case theme.LayerLineChart:
			renderer.drawLineChartLayer(canvas, bounds, layer, state)
		case theme.LayerShape:
			renderer.drawShapeLayer(canvas, bounds, layer)
		}
	}
	return canvas, nil
}

func (renderer *Renderer) drawCanvas(canvas *image.RGBA, layer theme.Layer) {
	value := strings.ToLower(optionString(layer.Options, "color", "transparent"))
	background := color.NRGBA{}
	switch value {
	case "black":
		background = color.NRGBA{A: 255}
	case "white":
		background = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	case "transparent":
		return
	default:
		background = optionColor(layer.Options, "color", color.NRGBA{})
	}
	background.A = multiplyAlpha(background.A, layer.Opacity)
	fillRect(canvas, canvas.Bounds(), background)
}

func (renderer *Renderer) drawImageLayer(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer) {
	resourceID := optionString(layer.Options, "resource_id", "")
	source := renderer.images[resourceID]
	if source == nil {
		return
	}
	drawLayerImage(canvas, bounds, source, layer.Opacity, layer.Rotation)
}

func (renderer *Renderer) drawBackground(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, frame image.Image) {
	source := frame
	if source == nil {
		resourceID := optionString(layer.Options, "resource_id", "")
		if layer.Type == theme.LayerBackgroundVideo {
			resourceID = optionString(layer.Options, "poster_resource_id", resourceID)
		}
		source = renderer.images[resourceID]
		if source == nil {
			for _, candidate := range renderer.images {
				source = candidate
				break
			}
		}
	}
	if source == nil {
		background := optionColor(layer.Options, "color", color.NRGBA{A: 255})
		background.A = multiplyAlpha(background.A, layer.Opacity)
		fillRect(canvas, bounds, background)
		return
	}
	drawImage(canvas, bounds, source, layer.Opacity, optionString(layer.Options, "fit", "cover"))
}

func (renderer *Renderer) drawLyrics(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, lyrics LyricState) {
	renderer.drawArtisticLyrics(canvas, bounds, layer, lyrics)
}

func (renderer *Renderer) face(resourceID string, size float64) font.Face {
	if size < 6 {
		size = 6
	}
	key := faceKey{resourceID: resourceID, size: int(math.Round(size * 10))}
	if face := renderer.faces[key]; face != nil {
		return face
	}
	source := renderer.fonts[resourceID]
	if source == nil {
		source = renderer.fonts[""]
	}
	if source == nil {
		return basicfont.Face7x13
	}
	face, err := opentype.NewFace(source, &opentype.FaceOptions{Size: size, DPI: 96, Hinting: font.HintingFull})
	if err != nil {
		return basicfont.Face7x13
	}
	renderer.faces[key] = face
	return face
}

// fittedFace 在不超过期望字号的前提下，同时满足一行文字的宽度和高度约束。
func (renderer *Renderer) fittedFace(resourceID string, preferred float64, texts []string, maximumWidth, maximumHeight int) font.Face {
	preferred = math.Max(6, math.Min(512, preferred))
	maximumWidth = maxInt(1, maximumWidth)
	maximumHeight = maxInt(1, maximumHeight)
	fits := func(candidate font.Face) bool {
		if fontHeight(candidate) > maximumHeight {
			return false
		}
		for _, textValue := range texts {
			if font.MeasureString(candidate, textValue).Ceil() > maximumWidth {
				return false
			}
		}
		return true
	}
	preferredFace := renderer.face(resourceID, preferred)
	if fits(preferredFace) {
		return preferredFace
	}
	minimumFace := renderer.face(resourceID, 6)
	if !fits(minimumFace) {
		return minimumFace
	}
	low, high := 6.0, preferred
	best := minimumFace
	for iteration := 0; iteration < 10 && high-low > .1; iteration++ {
		middle := (low + high) / 2
		candidate := renderer.face(resourceID, middle)
		if fits(candidate) {
			best = candidate
			low = middle
		} else {
			high = middle
		}
	}
	return best
}

func parseFontFile(path string) (*opentype.Font, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	collection, err := opentype.ParseCollection(data)
	if err != nil {
		return nil, err
	}
	if collection.NumFonts() == 0 {
		return nil, errors.New("字体集合为空")
	}
	return collection.Font(0)
}

func loadSystemFonts() map[string]*opentype.Font {
	if runtime.GOOS != "windows" {
		return make(map[string]*opentype.Font)
	}
	return loadSystemFontsFromDirectory(filepath.Join(os.Getenv("WINDIR"), "Fonts"))
}

func loadSystemFontsFromDirectory(fontDirectory string) map[string]*opentype.Font {
	fonts := make(map[string]*opentype.Font)
	type weightFiles struct {
		weight string
		names  []string
	}
	type familyFiles struct {
		family  string
		weights []weightFiles
	}
	// 每个逻辑字体族都显式定义四档。某些 Windows 字库只提供
	// 两张字脸，此时先选最接近的真实文件，再由绘制层做可控细化/膨胀。
	families := []familyFiles{
		{"pangmen", []weightFiles{{"light", []string{"Dengl.ttf", "Deng.ttf", "msyhl.ttc", "msyh.ttc"}}, {"regular", []string{"Deng.ttf", "msyh.ttc"}}, {"medium", []string{"Dengb.ttf", "Deng.ttf", "msyhbd.ttc", "msyh.ttc"}}, {"bold", []string{"Dengb.ttf", "msyhbd.ttc"}}}},
		{"source-han", []weightFiles{{"light", []string{"msyhl.ttc", "msyh.ttc"}}, {"regular", []string{"msyh.ttc"}}, {"medium", []string{"NotoSansSC-VF.ttf", "msyhbd.ttc"}}, {"bold", []string{"msyhbd.ttc", "NotoSansSC-VF.ttf"}}}},
		{"microsoft-yahei", []weightFiles{{"light", []string{"msyhl.ttc", "msyh.ttc"}}, {"regular", []string{"msyh.ttc"}}, {"medium", []string{"msyhbd.ttc", "msyh.ttc"}}, {"bold", []string{"msyhbd.ttc"}}}},
		{"simhei", []weightFiles{{"light", []string{"simhei.ttf", "msyhl.ttc"}}, {"regular", []string{"simhei.ttf", "msyh.ttc"}}, {"medium", []string{"simhei.ttf", "msyhbd.ttc"}}, {"bold", []string{"simhei.ttf", "msyhbd.ttc"}}}},
		{"digital", []weightFiles{{"light", []string{"LiquidCrystal-Light.otf", "consola.ttf"}}, {"regular", []string{"LiquidCrystal-Normal.otf", "consola.ttf"}}, {"medium", []string{"LiquidCrystal-Bold.otf", "consolab.ttf"}}, {"bold", []string{"LiquidCrystal-ExBold.otf", "consolab.ttf"}}}},
		{"monospace", []weightFiles{{"light", []string{"CascadiaMono.ttf", "consola.ttf"}}, {"regular", []string{"consola.ttf"}}, {"medium", []string{"consolab.ttf", "consola.ttf"}}, {"bold", []string{"consolab.ttf"}}}},
		{"rounded", []weightFiles{{"light", []string{"YuGothL.ttc", "msyhl.ttc"}}, {"regular", []string{"YuGothR.ttc", "msyh.ttc"}}, {"medium", []string{"YuGothM.ttc", "msyhbd.ttc"}}, {"bold", []string{"YuGothB.ttc", "msyhbd.ttc"}}}},
		{"serif", []weightFiles{{"light", []string{"NotoSerifSC-VF.ttf", "simsun.ttc"}}, {"regular", []string{"simsun.ttc", "NotoSerifSC-VF.ttf"}}, {"medium", []string{"simsunb.ttf", "NotoSerifSC-VF.ttf"}}, {"bold", []string{"simsunb.ttf"}}}},
	}
	parsedByPath := make(map[string]*opentype.Font)
	for _, family := range families {
		for _, candidate := range family.weights {
			for _, name := range candidate.names {
				path := filepath.Join(fontDirectory, name)
				parsed := parsedByPath[path]
				if parsed == nil {
					loaded, err := parseFontFile(path)
					if err != nil {
						continue
					}
					parsed = loaded
					parsedByPath[path] = parsed
				}
				fonts[systemFontResourceID(family.family, candidate.weight)] = parsed
				if candidate.weight == "regular" {
					fonts[family.family] = parsed
				}
				break
			}
		}
		if fonts[family.family] == nil {
			for _, weight := range []string{"medium", "light", "bold"} {
				if parsed := fonts[systemFontResourceID(family.family, weight)]; parsed != nil {
					fonts[family.family] = parsed
					break
				}
			}
		}
	}
	for _, family := range []string{"source-han", "microsoft-yahei", "simhei", "rounded", "serif", "monospace", "digital", "pangmen"} {
		if fonts[family] != nil {
			fonts[""] = fonts[family]
			break
		}
	}
	return fonts
}

func drawImage(destination *image.RGBA, bounds image.Rectangle, source image.Image, opacity float64, fit string) {
	if bounds.Empty() || source == nil {
		return
	}
	target := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	sourceBounds := source.Bounds()
	scaleRect := target.Bounds()
	if strings.EqualFold(fit, "contain") {
		ratio := math.Min(float64(bounds.Dx())/float64(sourceBounds.Dx()), float64(bounds.Dy())/float64(sourceBounds.Dy()))
		width := maxInt(1, int(float64(sourceBounds.Dx())*ratio))
		height := maxInt(1, int(float64(sourceBounds.Dy())*ratio))
		scaleRect = image.Rect((bounds.Dx()-width)/2, (bounds.Dy()-height)/2, (bounds.Dx()+width)/2, (bounds.Dy()+height)/2)
	} else if strings.EqualFold(fit, "cover") {
		targetRatio := float64(bounds.Dx()) / float64(bounds.Dy())
		sourceRatio := float64(sourceBounds.Dx()) / float64(sourceBounds.Dy())
		if sourceRatio > targetRatio {
			width := int(float64(sourceBounds.Dy()) * targetRatio)
			sourceBounds.Min.X += (sourceBounds.Dx() - width) / 2
			sourceBounds.Max.X = sourceBounds.Min.X + width
		} else {
			height := int(float64(sourceBounds.Dx()) / targetRatio)
			sourceBounds.Min.Y += (sourceBounds.Dy() - height) / 2
			sourceBounds.Max.Y = sourceBounds.Min.Y + height
		}
	}
	xdraw.CatmullRom.Scale(target, scaleRect, source, sourceBounds, stddraw.Src, nil)
	alpha := uint8(math.Round(math.Max(0, math.Min(1, opacity)) * 255))
	stddraw.DrawMask(destination, bounds, target, image.Point{}, image.NewUniform(color.Alpha{A: alpha}), image.Point{}, stddraw.Over)
}

// drawLayerImage 先按图层矩形缩放，再围绕图层中心旋转，保证编辑器预览与设备输出一致。
func drawLayerImage(destination *image.RGBA, bounds image.Rectangle, source image.Image, opacity, rotation float64) {
	if bounds.Empty() || source == nil {
		return
	}
	target := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	xdraw.CatmullRom.Scale(target, target.Bounds(), source, source.Bounds(), stddraw.Src, nil)
	alpha := math.Max(0, math.Min(1, opacity))
	if alpha < 1 {
		for offset := 0; offset < len(target.Pix); offset += 4 {
			target.Pix[offset+0] = uint8(math.Round(float64(target.Pix[offset+0]) * alpha))
			target.Pix[offset+1] = uint8(math.Round(float64(target.Pix[offset+1]) * alpha))
			target.Pix[offset+2] = uint8(math.Round(float64(target.Pix[offset+2]) * alpha))
			target.Pix[offset+3] = uint8(math.Round(float64(target.Pix[offset+3]) * alpha))
		}
	}
	if math.Abs(rotation) < 0.0001 {
		stddraw.Draw(destination, bounds, target, image.Point{}, stddraw.Over)
		return
	}
	radians := rotation * math.Pi / 180
	cosine, sine := math.Cos(radians), math.Sin(radians)
	centerX := float64(bounds.Min.X+bounds.Max.X) / 2
	centerY := float64(bounds.Min.Y+bounds.Max.Y) / 2
	transform := f64.Aff3{
		cosine, -sine, centerX - cosine*float64(bounds.Dx())/2 + sine*float64(bounds.Dy())/2,
		sine, cosine, centerY - sine*float64(bounds.Dx())/2 - cosine*float64(bounds.Dy())/2,
	}
	xdraw.CatmullRom.Transform(destination, transform, target, target.Bounds(), stddraw.Over, nil)
}

func drawBars(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, bars []float64) {
	drawVisualBars(canvas, bounds, layer, bars)
}

func drawRing(canvas *image.RGBA, bounds image.Rectangle, layer theme.Layer, bars []float64) {
	drawVisualRing(canvas, bounds, layer, bars)
}

func drawTextCentered(canvas *image.RGBA, face font.Face, centerX, baseline int, text string, foreground color.NRGBA) {
	width := font.MeasureString(face, text).Ceil()
	drawText(canvas, face, centerX-width/2, baseline, text, foreground, true)
}

func drawText(canvas *image.RGBA, face font.Face, x, baseline int, text string, foreground color.NRGBA, shadow bool) {
	if shadow {
		drawer := font.Drawer{Dst: canvas, Src: image.NewUniform(color.NRGBA{A: foreground.A / 2}), Face: face, Dot: fixed.P(x+1, baseline+1)}
		drawer.DrawString(text)
	}
	drawer := font.Drawer{Dst: canvas, Src: image.NewUniform(foreground), Face: face, Dot: fixed.P(x, baseline)}
	drawer.DrawString(text)
}

// drawLayerText 在真实字重文件不可用时为中粗和粗体做小范围形态膨胀。
// 这样字体设置不仅写入清单，也会稳定地反映到利民、酷冷等主机合成画面；
// 额外绘制只发生在主机内存中，不会增加设备通讯字段。
func drawLayerText(canvas *image.RGBA, face font.Face, x, baseline int, textValue string, foreground color.NRGBA, shadow bool, layer theme.Layer) {
	if canvas == nil || face == nil || textValue == "" || foreground.A == 0 {
		return
	}
	weight := layerFontWeight(layer)
	base := foreground
	if weight == "light" {
		// 自定义字体可能只提供一张字脸；降低主笔画覆盖率作为
		// 可预期的细体后备，保证 light 不会与 regular 生成同一画面。
		base = withVisualAlpha(foreground, .68)
	}
	drawText(canvas, face, x, baseline, textValue, base, shadow)
	switch weight {
	case "bold":
		drawText(canvas, face, x+1, baseline, textValue, withVisualAlpha(foreground, .82), false)
		drawText(canvas, face, x, baseline+1, textValue, withVisualAlpha(foreground, .54), false)
	case "medium":
		drawText(canvas, face, x+1, baseline, textValue, withVisualAlpha(foreground, .42), false)
	}
}

// layerFontWeight 把 JSON 中的名称或 CSS 数字字重收敛为四档稳定契约。
func layerFontWeight(layer theme.Layer) string {
	fallback := "medium"
	if layer.Type == theme.LayerLyrics {
		fallback = "bold"
	}
	return normalizeFontWeight(optionString(layer.Options, "font_weight", fallback))
}

func normalizeFontWeight(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "light", "thin", "100", "200", "300":
		return "light"
	case "regular", "normal", "400":
		return "regular"
	case "bold", "700", "800", "900":
		return "bold"
	default:
		return "medium"
	}
}

func systemFontResourceID(family, weight string) string {
	return "@system:" + strings.ToLower(strings.TrimSpace(family)) + ":" + normalizeFontWeight(weight)
}

func fontHeight(face font.Face) int {
	return face.Metrics().Height.Ceil()
}

// clippedLayerCanvas 让字体绘制也受图层边界约束，作为极小尺寸下的最终防线。
func clippedLayerCanvas(canvas *image.RGBA, bounds image.Rectangle) *image.RGBA {
	clipped := bounds.Intersect(canvas.Bounds())
	if clipped.Empty() {
		return image.NewRGBA(image.Rectangle{})
	}
	return canvas.SubImage(clipped).(*image.RGBA)
}

func fitTextToWidth(face font.Face, textValue string, maximumWidth int) string {
	maximumWidth = maxInt(1, maximumWidth)
	if font.MeasureString(face, textValue).Ceil() <= maximumWidth {
		return textValue
	}
	ellipsis := "…"
	if font.MeasureString(face, ellipsis).Ceil() > maximumWidth {
		return ""
	}
	runes := []rune(textValue)
	for len(runes) > 0 {
		candidate := string(runes) + ellipsis
		if font.MeasureString(face, candidate).Ceil() <= maximumWidth {
			return candidate
		}
		runes = runes[:len(runes)-1]
	}
	return ellipsis
}

func fillRoundedPanel(canvas *image.RGBA, bounds image.Rectangle, radius int, fill color.NRGBA) {
	if radius <= 1 {
		fillRect(canvas, bounds, fill)
		return
	}
	fillRect(canvas, image.Rect(bounds.Min.X+radius, bounds.Min.Y, bounds.Max.X-radius, bounds.Max.Y), fill)
	fillRect(canvas, image.Rect(bounds.Min.X, bounds.Min.Y+radius, bounds.Max.X, bounds.Max.Y-radius), fill)
	for y := 0; y < radius; y++ {
		dx := int(math.Sqrt(float64(radius*radius - y*y)))
		fillRect(canvas, image.Rect(bounds.Min.X+radius-dx, bounds.Min.Y+radius-y, bounds.Max.X-radius+dx, bounds.Min.Y+radius-y+1), fill)
		fillRect(canvas, image.Rect(bounds.Min.X+radius-dx, bounds.Max.Y-radius+y-1, bounds.Max.X-radius+dx, bounds.Max.Y-radius+y), fill)
	}
}

func fillRect(canvas *image.RGBA, bounds image.Rectangle, fill color.NRGBA) {
	bounds = bounds.Intersect(canvas.Bounds())
	if bounds.Empty() || fill.A == 0 {
		return
	}
	stddraw.Draw(canvas, bounds, image.NewUniform(fill), image.Point{}, stddraw.Over)
}

func drawLine(canvas *image.RGBA, x0, y0, x1, y1, thickness int, fill color.NRGBA) {
	steps := maxInt(absInt(x1-x0), absInt(y1-y0))
	if steps == 0 {
		fillCircle(canvas, x0, y0, maxInt(1, thickness/2), fill)
		return
	}
	radius := maxInt(1, thickness/2)
	for index := 0; index <= steps; index++ {
		ratio := float64(index) / float64(steps)
		x := int(math.Round(float64(x0) + float64(x1-x0)*ratio))
		y := int(math.Round(float64(y0) + float64(y1-y0)*ratio))
		fillCircle(canvas, x, y, radius, fill)
	}
}

func fillCircle(canvas *image.RGBA, centerX, centerY, radius int, fill color.NRGBA) {
	for y := -radius; y <= radius; y++ {
		dx := int(math.Sqrt(float64(radius*radius - y*y)))
		fillRect(canvas, image.Rect(centerX-dx, centerY+y, centerX+dx+1, centerY+y+1), fill)
	}
}

func normalizedRect(bounds theme.Rect, width, height int) image.Rectangle {
	return rawNormalizedRect(bounds, width, height).Intersect(image.Rect(0, 0, width, height))
}

func rawNormalizedRect(bounds theme.Rect, width, height int) image.Rectangle {
	return image.Rect(
		int(math.Round(bounds.X*float64(width))),
		int(math.Round(bounds.Y*float64(height))),
		int(math.Round((bounds.X+bounds.Width)*float64(width))),
		int(math.Round((bounds.Y+bounds.Height)*float64(height))),
	)
}

func optionString(options map[string]any, key, fallback string) string {
	if value, ok := options[key].(string); ok && value != "" {
		return value
	}
	return fallback
}

func optionFloat(options map[string]any, key string, fallback float64) float64 {
	switch value := options[key].(type) {
	case float64:
		return value
	case float32:
		return float64(value)
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case string:
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed
		}
	}
	return fallback
}

func optionInt(options map[string]any, key string, fallback int) int {
	return int(math.Round(optionFloat(options, key, float64(fallback))))
}

func optionStrings(options map[string]any, key string) []string {
	switch values := options[key].(type) {
	case []string:
		return append([]string(nil), values...)
	case []any:
		output := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				output = append(output, text)
			}
		}
		return output
	}
	return nil
}

func optionColor(options map[string]any, key string, fallback color.NRGBA) color.NRGBA {
	text := optionString(options, key, "")
	if text == "" {
		return fallback
	}
	text = strings.TrimPrefix(strings.TrimSpace(text), "#")
	if len(text) != 6 && len(text) != 8 {
		return fallback
	}
	value, err := strconv.ParseUint(text, 16, 32)
	if err != nil {
		return fallback
	}
	if len(text) == 6 {
		return color.NRGBA{R: uint8(value >> 16), G: uint8(value >> 8), B: uint8(value), A: 255}
	}
	return color.NRGBA{R: uint8(value >> 24), G: uint8(value >> 16), B: uint8(value >> 8), A: uint8(value)}
}

func multiplyAlpha(alpha uint8, opacity float64) uint8 {
	return uint8(math.Round(float64(alpha) * math.Max(0, math.Min(1, opacity))))
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
