package coolergeometry

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"strconv"
)

var calibrationColors = [...]color.RGBA{
	{R: 0x46, G: 0xE6, B: 0xD2, A: 0xFF},
	{R: 0x62, G: 0x9B, B: 0xFF, A: 0xFF},
	{R: 0xB1, G: 0x7A, B: 0xFF, A: 0xFF},
	{R: 0xFF, G: 0x6B, B: 0x91, A: 0xFF},
	{R: 0xFF, G: 0xB8, B: 0x4D, A: 0xFF},
	{R: 0x8F, G: 0xE0, B: 0x68, A: 0xFF},
	{R: 0x46, G: 0xE6, B: 0xD2, A: 0xFF},
	{R: 0x62, G: 0x9B, B: 0xFF, A: 0xFF},
	{R: 0xB1, G: 0x7A, B: 0xFF, A: 0xFF},
	{R: 0xFF, G: 0x6B, B: 0x91, A: 0xFF},
	{R: 0xFF, G: 0xB8, B: 0x4D, A: 0xFF},
	{R: 0x8F, G: 0xE0, B: 0x68, A: 0xFF},
}

// CalibrationImage 生成方向校准测试图。
// 1-12 按普通钟面排列，6 位于逻辑下方，中央箭头始终指向逻辑下方。
// 图中只使用内置的 ASCII 点阵字形，不依赖主机字体。
func CalibrationImage(width, height int) *image.RGBA {
	if width <= 0 || height <= 0 {
		return image.NewRGBA(image.Rectangle{})
	}
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(target, target.Bounds(), &image.Uniform{C: color.RGBA{R: 0x07, G: 0x0E, B: 0x16, A: 0xFF}}, image.Point{}, draw.Src)

	vertices := hexagonVerticesFloat(width, height)
	lineWidth := max(2, min(width, height)/90)
	for index := range vertices {
		current := vertices[index]
		next := vertices[(index+1)%len(vertices)]
		drawThickLine(target, current.x, current.y, next.x, next.y, lineWidth, color.RGBA{R: 0x29, G: 0x84, B: 0x91, A: 0xFF})
	}

	cx := float64(width) / 2
	cy := float64(height) / 2
	radius := math.Min(float64(width), float64(height)) * circleRadiusRatio
	markerRadius := math.Max(10, math.Min(float64(width), float64(height))*0.045)
	markerDistance := radius * 0.78
	for index := 0; index < 12; index++ {
		markerNumber := index + 1
		angle := (-90 + float64(markerNumber*30)) * math.Pi / 180
		markerX := cx + markerDistance*math.Cos(angle)
		markerY := cy + markerDistance*math.Sin(angle)
		drawFilledCircle(target, markerX, markerY, markerRadius, calibrationColors[index])
		label := strconv.Itoa(markerNumber)
		drawBitmapTextCentered(target, label, int(math.Round(markerX)), int(math.Round(markerY)), max(2, min(width, height)/170), color.White)
	}

	// 中央文字与箭头给出一个不依赖外部字体的绝对方向参考。
	textScale := max(2, min(width, height)/150)
	drawBitmapTextCentered(target, "POINT DOWN", width/2, int(float64(height)*0.43), textScale, color.RGBA{R: 0xE8, G: 0xF8, B: 0xFF, A: 0xFF})
	arrowTop := float64(height) * 0.50
	arrowBottom := float64(height) * 0.67
	arrowWidth := max(3, min(width, height)/70)
	drawThickLine(target, cx, arrowTop, cx, arrowBottom, arrowWidth, color.RGBA{R: 0xFF, G: 0xD2, B: 0x4A, A: 0xFF})
	drawThickLine(target, cx, arrowBottom, cx-float64(width)*0.045, arrowBottom-float64(height)*0.055, arrowWidth, color.RGBA{R: 0xFF, G: 0xD2, B: 0x4A, A: 0xFF})
	drawThickLine(target, cx, arrowBottom, cx+float64(width)*0.045, arrowBottom-float64(height)*0.055, arrowWidth, color.RGBA{R: 0xFF, G: 0xD2, B: 0x4A, A: 0xFF})

	return ApplyCircleMask(target)
}

func drawThickLine(target *image.RGBA, x0, y0, x1, y1 float64, width int, lineColor color.RGBA) {
	distance := math.Hypot(x1-x0, y1-y0)
	steps := max(1, int(math.Ceil(distance)))
	radius := math.Max(1, float64(width)/2)
	for step := 0; step <= steps; step++ {
		ratio := float64(step) / float64(steps)
		x := x0 + (x1-x0)*ratio
		y := y0 + (y1-y0)*ratio
		drawFilledCircle(target, x, y, radius, lineColor)
	}
}

func drawFilledCircle(target *image.RGBA, centerX, centerY, radius float64, fill color.RGBA) {
	minimumX := max(0, int(math.Floor(centerX-radius)))
	maximumX := min(target.Bounds().Dx()-1, int(math.Ceil(centerX+radius)))
	minimumY := max(0, int(math.Floor(centerY-radius)))
	maximumY := min(target.Bounds().Dy()-1, int(math.Ceil(centerY+radius)))
	radiusSquared := radius * radius
	for y := minimumY; y <= maximumY; y++ {
		for x := minimumX; x <= maximumX; x++ {
			deltaX := float64(x) + 0.5 - centerX
			deltaY := float64(y) + 0.5 - centerY
			if deltaX*deltaX+deltaY*deltaY <= radiusSquared {
				target.SetRGBA(x, y, fill)
			}
		}
	}
}

var bitmapGlyphs = map[rune][7]string{
	'0': {"01110", "10001", "10011", "10101", "11001", "10001", "01110"},
	'1': {"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
	'2': {"01110", "10001", "00001", "00010", "00100", "01000", "11111"},
	'3': {"11110", "00001", "00001", "01110", "00001", "00001", "11110"},
	'4': {"00010", "00110", "01010", "10010", "11111", "00010", "00010"},
	'5': {"11111", "10000", "10000", "11110", "00001", "00001", "11110"},
	'6': {"01110", "10000", "10000", "11110", "10001", "10001", "01110"},
	'7': {"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
	'8': {"01110", "10001", "10001", "01110", "10001", "10001", "01110"},
	'9': {"01110", "10001", "10001", "01111", "00001", "00001", "01110"},
	'A': {"01110", "10001", "10001", "11111", "10001", "10001", "10001"},
	'B': {"11110", "10001", "10001", "11110", "10001", "10001", "11110"},
	'C': {"01111", "10000", "10000", "10000", "10000", "10000", "01111"},
	'D': {"11110", "10001", "10001", "10001", "10001", "10001", "11110"},
	'E': {"11111", "10000", "10000", "11110", "10000", "10000", "11111"},
	'F': {"11111", "10000", "10000", "11110", "10000", "10000", "10000"},
	'I': {"11111", "00100", "00100", "00100", "00100", "00100", "11111"},
	'N': {"10001", "11001", "11001", "10101", "10011", "10011", "10001"},
	'O': {"01110", "10001", "10001", "10001", "10001", "10001", "01110"},
	'P': {"11110", "10001", "10001", "11110", "10000", "10000", "10000"},
	'T': {"11111", "00100", "00100", "00100", "00100", "00100", "00100"},
	'W': {"10001", "10001", "10001", "10101", "10101", "11011", "10001"},
}

func drawBitmapTextCentered(target *image.RGBA, text string, centerX, centerY, scale int, textColor color.Color) {
	if scale <= 0 || text == "" {
		return
	}
	characterWidth := 5 * scale
	spacing := scale
	textWidth := len([]rune(text))*(characterWidth+spacing) - spacing
	startX := centerX - textWidth/2
	startY := centerY - 7*scale/2
	for _, character := range text {
		if character == ' ' {
			startX += characterWidth + spacing
			continue
		}
		glyph, exists := bitmapGlyphs[character]
		if exists {
			for row, pattern := range glyph {
				for column, pixel := range pattern {
					if pixel != '1' {
						continue
					}
					rectangle := image.Rect(startX+column*scale, startY+row*scale, startX+(column+1)*scale, startY+(row+1)*scale)
					draw.Draw(target, rectangle, &image.Uniform{C: textColor}, image.Point{}, draw.Src)
				}
			}
		}
		startX += characterWidth + spacing
	}
}
