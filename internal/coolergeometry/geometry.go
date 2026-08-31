// Package coolergeometry 提供酷冷至尊冷头屏的几何变换。
//
// 设备使用固定的正方形像素帧传输，实际 LCD 可见区是内接圆，
// 尖角朝上/下的六边形只用于表示外壳与安装正向。本包不依赖设备、API 或 UI，因此可在编辑器预览、主机合成
// 与离线素材生成中共用。
package coolergeometry

import (
	"errors"
	"image"
	"image/color"
	"image/draw"
	"math"
	"strconv"
	"strings"
	"sync"
)

const (
	// DefaultWidth 和 DefaultHeight 是酷冷至尊屏幕的物理帧大小。
	DefaultWidth  = 480
	DefaultHeight = 480

	hexagonStep       = 60
	hexRadiusRatio    = 0.48
	circleRadiusRatio = 0.49
)

// CalibrationRotations 列出钟面式校准图能产生的 30 度补偿档位。
// 旋转函数本身不受这些档位限制。
var CalibrationRotations = [...]int{0, 30, 60, 90, 120, 150, 180, 210, 240, 270, 300, 330}

var circleMasks sync.Map

// NormalizeOrientation 把任意整数角度归一化到 0..359。
func NormalizeOrientation(degrees int) int {
	normalized := degrees % 360
	if normalized < 0 {
		normalized += 360
	}
	return normalized
}

// NormalizeRotation 是供设备、API 与 UI 共用的稳定入口。
// 它与 NormalizeOrientation 保持相同的 0..359 归一化语义。
func NormalizeRotation(degrees int) int {
	return NormalizeOrientation(degrees)
}

// MarkerCorrection 返回校准补偿角。标识 1-12 按普通钟面排列，6 位于逻辑下方。
// 当用户选择物理下方所见的数字时，返回角度可把该数字旋转回逻辑下方。
func MarkerCorrection(marker string) (int, error) {
	markerNumber, err := strconv.Atoi(strings.TrimSpace(marker))
	if err != nil || markerNumber < 1 || markerNumber > 12 {
		return 0, errors.New("方向标识必须是 1-12")
	}
	return NormalizeOrientation((markerNumber - 6) * 30), nil
}

// RotationForMarker 是方向校准的稳定入口。
// 返回值可使选中的物理下方标识回到逻辑下方。
func RotationForMarker(marker string) (int, error) {
	return MarkerCorrection(marker)
}

// HexagonVertices 返回尖角朝上/下的正六边形顶点，顺序从下方尖角开始顺时针排列。
func HexagonVertices(width, height int) [6]image.Point {
	if width <= 0 || height <= 0 {
		return [6]image.Point{}
	}
	cx := float64(width) / 2
	cy := float64(height) / 2
	radius := math.Min(float64(width), float64(height)) * hexRadiusRatio
	var vertices [6]image.Point
	for index := range vertices {
		angle := (90 + float64(index*hexagonStep)) * math.Pi / 180
		vertices[index] = image.Pt(
			int(math.Round(cx+radius*math.Cos(angle))),
			int(math.Round(cy+radius*math.Sin(angle))),
		)
	}
	return vertices
}

// ApplyHexMask 把逻辑六边形以外的像素填充为不透明黑色。
// 边缘使用 4x4 覆盖率采样，减少斜边锯齿。返回图像的原点为 (0,0)。
func ApplyHexMask(source image.Image) *image.RGBA {
	if source == nil {
		return image.NewRGBA(image.Rectangle{})
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(target, target.Bounds(), &image.Uniform{C: color.Black}, image.Point{}, draw.Src)
	if width == 0 || height == 0 {
		return target
	}

	vertices := hexagonVerticesFloat(width, height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			coverage := hexPixelCoverage(float64(x), float64(y), vertices)
			if coverage <= 0 {
				continue
			}
			sourceColor := color.NRGBAModel.Convert(source.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
			alpha := coverage * float64(sourceColor.A) / 255
			target.SetRGBA(x, y, color.RGBA{
				R: uint8(math.Round(float64(sourceColor.R) * alpha)),
				G: uint8(math.Round(float64(sourceColor.G) * alpha)),
				B: uint8(math.Round(float64(sourceColor.B) * alpha)),
				A: 255,
			})
		}
	}
	return target
}

// MaskHexagon 是六边形遮罩的稳定入口。
func MaskHexagon(source image.Image) image.Image {
	return ApplyHexMask(source)
}

// ApplyCircleMask 把实际 LCD 圆形可见区以外的像素填充为不透明黑色。
// 边缘使用 4x4 覆盖率采样，返回图像的原点为 (0,0)。
func ApplyCircleMask(source image.Image) *image.RGBA {
	if source == nil {
		return image.NewRGBA(image.Rectangle{})
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(target, target.Bounds(), &image.Uniform{C: color.Black}, image.Point{}, draw.Src)
	if width == 0 || height == 0 {
		return target
	}
	mask := cachedCircleMask(width, height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			coverage := float64(mask[y*width+x]) / 255
			if coverage <= 0 {
				continue
			}
			sourceColor := color.NRGBAModel.Convert(source.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
			alpha := coverage * float64(sourceColor.A) / 255
			target.SetRGBA(x, y, color.RGBA{
				R: uint8(math.Round(float64(sourceColor.R) * alpha)),
				G: uint8(math.Round(float64(sourceColor.G) * alpha)),
				B: uint8(math.Round(float64(sourceColor.B) * alpha)),
				A: 255,
			})
		}
	}
	return target
}

// MaskCircle 是实际 LCD 圆形遮罩的稳定入口。
func MaskCircle(source image.Image) image.Image {
	return ApplyCircleMask(source)
}

// Rotate 把图像绕中心按任意整数角度旋转，正角度为视觉上顺时针。
// 输出尺寸与输入一致，旋转后无源像素的区域使用不透明黑色填充。
// 采样使用双线性插值，返回图像的原点为 (0,0)。
func Rotate(source image.Image, degrees int) *image.RGBA {
	if source == nil {
		return image.NewRGBA(image.Rectangle{})
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(target, target.Bounds(), &image.Uniform{C: color.Black}, image.Point{}, draw.Src)
	if width == 0 || height == 0 {
		return target
	}

	rotation := NormalizeOrientation(degrees)
	if rotation == 0 {
		draw.Draw(target, target.Bounds(), source, bounds.Min, draw.Src)
		return target
	}

	radians := float64(rotation) * math.Pi / 180
	cosine, sine := math.Cos(radians), math.Sin(radians)
	centerX := float64(width-1) / 2
	centerY := float64(height-1) / 2
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			destinationX := float64(x) - centerX
			destinationY := float64(y) - centerY
			// 使用逆变换找到目标像素对应的源坐标。
			sourceX := cosine*destinationX + sine*destinationY + centerX
			sourceY := -sine*destinationX + cosine*destinationY + centerY
			if sampled, ok := bilinearSample(source, bounds, sourceX, sourceY); ok {
				target.SetRGBA(x, y, sampled)
			}
		}
	}
	return target
}

// Transform 是面向设备输出的稳定入口。
// 它在一次目标遍历中融合圆形遮罩与方向旋转，避免主机高频帧的中间图像分配。
func Transform(source image.Image, rotation int) image.Image {
	if source == nil {
		return image.NewRGBA(image.Rectangle{})
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(target, target.Bounds(), &image.Uniform{C: color.Black}, image.Point{}, draw.Src)
	if width == 0 || height == 0 {
		return target
	}

	mask := cachedCircleMask(width, height)
	rotation = NormalizeRotation(rotation)
	sampler := newStraightColorSampler(source)
	if rotation == 0 || rotation == 180 || (width == height && (rotation == 90 || rotation == 270)) {
		transformIntegerRotation(target, bounds, mask, sampler, rotation)
		return target
	}

	radians := float64(rotation) * math.Pi / 180
	cosine, sine := math.Cos(radians), math.Sin(radians)
	centerX := float64(width-1) / 2
	centerY := float64(height-1) / 2
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			coverageByte := mask[y*width+x]
			if coverageByte == 0 {
				continue
			}
			destinationX := float64(x) - centerX
			destinationY := float64(y) - centerY
			sourceX := cosine*destinationX + sine*destinationY + centerX
			sourceY := -sine*destinationX + cosine*destinationY + centerY
			sampled, ok := sampler.bilinear(bounds, sourceX, sourceY)
			if !ok {
				continue
			}
			setMaskedPixel(target, x, y, sampled, coverageByte)
		}
	}
	return target
}

func transformIntegerRotation(
	target *image.RGBA,
	sourceBounds image.Rectangle,
	mask []uint8,
	sampler straightColorSampler,
	rotation int,
) {
	width, height := target.Bounds().Dx(), target.Bounds().Dy()
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			coverageByte := mask[y*width+x]
			if coverageByte == 0 {
				continue
			}
			var sourceX, sourceY int
			switch rotation {
			case 90:
				sourceX, sourceY = y, height-1-x
			case 180:
				sourceX, sourceY = width-1-x, height-1-y
			case 270:
				sourceX, sourceY = width-1-y, x
			default:
				sourceX, sourceY = x, y
			}
			sampled := sampler.at(sourceBounds.Min.X+sourceX, sourceBounds.Min.Y+sourceY)
			setMaskedPixel(target, x, y, sampled, coverageByte)
		}
	}
}

func setMaskedPixel(target *image.RGBA, x, y int, sampled color.NRGBA, coverageByte uint8) {
	offset := target.PixOffset(x, y)
	if coverageByte == 255 && sampled.A == 255 {
		target.Pix[offset+0] = sampled.R
		target.Pix[offset+1] = sampled.G
		target.Pix[offset+2] = sampled.B
		target.Pix[offset+3] = 255
		return
	}
	// 与浮点 round(channel*coverage*alpha/255²) 相同，但避免不透明高频帧中的浮点开销。
	const denominator = 255 * 255
	scale := int(coverageByte) * int(sampled.A)
	target.Pix[offset+0] = uint8((int(sampled.R)*scale + denominator/2) / denominator)
	target.Pix[offset+1] = uint8((int(sampled.G)*scale + denominator/2) / denominator)
	target.Pix[offset+2] = uint8((int(sampled.B)*scale + denominator/2) / denominator)
	target.Pix[offset+3] = 255
}

type pointFloat struct {
	x float64
	y float64
}

func hexagonVerticesFloat(width, height int) [6]pointFloat {
	cx := float64(width) / 2
	cy := float64(height) / 2
	radius := math.Min(float64(width), float64(height)) * hexRadiusRatio
	var vertices [6]pointFloat
	for index := range vertices {
		angle := (90 + float64(index*hexagonStep)) * math.Pi / 180
		vertices[index] = pointFloat{
			x: cx + radius*math.Cos(angle),
			y: cy + radius*math.Sin(angle),
		}
	}
	return vertices
}

func hexPixelCoverage(x, y float64, vertices [6]pointFloat) float64 {
	const sampleGrid = 4
	inside := 0
	for sampleY := 0; sampleY < sampleGrid; sampleY++ {
		for sampleX := 0; sampleX < sampleGrid; sampleX++ {
			pointX := x + (float64(sampleX)+0.5)/sampleGrid
			pointY := y + (float64(sampleY)+0.5)/sampleGrid
			if pointInConvexHexagon(pointX, pointY, vertices) {
				inside++
			}
		}
	}
	return float64(inside) / (sampleGrid * sampleGrid)
}

func circlePixelCoverage(x, y, centerX, centerY, radius float64) float64 {
	const sampleGrid = 4
	inside := 0
	radiusSquared := radius * radius
	for sampleY := 0; sampleY < sampleGrid; sampleY++ {
		for sampleX := 0; sampleX < sampleGrid; sampleX++ {
			pointX := x + (float64(sampleX)+0.5)/sampleGrid
			pointY := y + (float64(sampleY)+0.5)/sampleGrid
			deltaX := pointX - centerX
			deltaY := pointY - centerY
			if deltaX*deltaX+deltaY*deltaY <= radiusSquared {
				inside++
			}
		}
	}
	return float64(inside) / (sampleGrid * sampleGrid)
}

type circleMaskKey struct {
	width  int
	height int
}

func cachedCircleMask(width, height int) []uint8 {
	key := circleMaskKey{width: width, height: height}
	if cached, ok := circleMasks.Load(key); ok {
		return cached.([]uint8)
	}
	mask := make([]uint8, width*height)
	centerX := float64(width) / 2
	centerY := float64(height) / 2
	radius := math.Min(float64(width), float64(height)) * circleRadiusRatio
	const halfPixelDiagonal = 0.708
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			distance := math.Hypot(float64(x)+0.5-centerX, float64(y)+0.5-centerY)
			switch {
			case distance <= radius-halfPixelDiagonal:
				mask[y*width+x] = 255
			case distance >= radius+halfPixelDiagonal:
				mask[y*width+x] = 0
			default:
				mask[y*width+x] = uint8(math.Round(circlePixelCoverage(float64(x), float64(y), centerX, centerY, radius) * 255))
			}
		}
	}
	actual, _ := circleMasks.LoadOrStore(key, mask)
	return actual.([]uint8)
}

type straightColorSampler struct {
	source image.Image
	rgba   *image.RGBA
	nrgba  *image.NRGBA
}

func newStraightColorSampler(source image.Image) straightColorSampler {
	sampler := straightColorSampler{source: source}
	sampler.rgba, _ = source.(*image.RGBA)
	sampler.nrgba, _ = source.(*image.NRGBA)
	return sampler
}

func (sampler straightColorSampler) at(x, y int) color.NRGBA {
	if sampler.nrgba != nil {
		return sampler.nrgba.NRGBAAt(x, y)
	}
	if sampler.rgba != nil {
		value := sampler.rgba.RGBAAt(x, y)
		if value.A == 0 {
			return color.NRGBA{}
		}
		if value.A == 255 {
			return color.NRGBA{R: value.R, G: value.G, B: value.B, A: value.A}
		}
		return color.NRGBA{
			R: uint8(min(255, (int(value.R)*255+int(value.A)/2)/int(value.A))),
			G: uint8(min(255, (int(value.G)*255+int(value.A)/2)/int(value.A))),
			B: uint8(min(255, (int(value.B)*255+int(value.A)/2)/int(value.A))),
			A: value.A,
		}
	}
	return color.NRGBAModel.Convert(sampler.source.At(x, y)).(color.NRGBA)
}

func (sampler straightColorSampler) bilinear(bounds image.Rectangle, x, y float64) (color.NRGBA, bool) {
	width, height := bounds.Dx(), bounds.Dy()
	if x < 0 || y < 0 || x > float64(width-1) || y > float64(height-1) {
		return color.NRGBA{}, false
	}
	x0 := int(math.Floor(x))
	y0 := int(math.Floor(y))
	x1 := min(x0+1, width-1)
	y1 := min(y0+1, height-1)
	fractionX := x - float64(x0)
	fractionY := y - float64(y0)
	c00 := sampler.at(bounds.Min.X+x0, bounds.Min.Y+y0)
	c10 := sampler.at(bounds.Min.X+x1, bounds.Min.Y+y0)
	c01 := sampler.at(bounds.Min.X+x0, bounds.Min.Y+y1)
	c11 := sampler.at(bounds.Min.X+x1, bounds.Min.Y+y1)
	interpolate := func(a00, a10, a01, a11 uint8) uint8 {
		top := float64(a00)*(1-fractionX) + float64(a10)*fractionX
		bottom := float64(a01)*(1-fractionX) + float64(a11)*fractionX
		return uint8(math.Round(top*(1-fractionY) + bottom*fractionY))
	}
	return color.NRGBA{
		R: interpolate(c00.R, c10.R, c01.R, c11.R),
		G: interpolate(c00.G, c10.G, c01.G, c11.G),
		B: interpolate(c00.B, c10.B, c01.B, c11.B),
		A: interpolate(c00.A, c10.A, c01.A, c11.A),
	}, true
}

func pointInConvexHexagon(x, y float64, vertices [6]pointFloat) bool {
	var sign float64
	for index := range vertices {
		current := vertices[index]
		next := vertices[(index+1)%len(vertices)]
		cross := (next.x-current.x)*(y-current.y) - (next.y-current.y)*(x-current.x)
		if math.Abs(cross) < 1e-9 {
			continue
		}
		if sign == 0 {
			sign = cross
			continue
		}
		if cross*sign < 0 {
			return false
		}
	}
	return true
}

func bilinearSample(source image.Image, bounds image.Rectangle, x, y float64) (color.RGBA, bool) {
	width, height := bounds.Dx(), bounds.Dy()
	if x < 0 || y < 0 || x > float64(width-1) || y > float64(height-1) {
		return color.RGBA{}, false
	}
	x0 := int(math.Floor(x))
	y0 := int(math.Floor(y))
	x1 := min(x0+1, width-1)
	y1 := min(y0+1, height-1)
	fractionX := x - float64(x0)
	fractionY := y - float64(y0)

	c00 := color.NRGBAModel.Convert(source.At(bounds.Min.X+x0, bounds.Min.Y+y0)).(color.NRGBA)
	c10 := color.NRGBAModel.Convert(source.At(bounds.Min.X+x1, bounds.Min.Y+y0)).(color.NRGBA)
	c01 := color.NRGBAModel.Convert(source.At(bounds.Min.X+x0, bounds.Min.Y+y1)).(color.NRGBA)
	c11 := color.NRGBAModel.Convert(source.At(bounds.Min.X+x1, bounds.Min.Y+y1)).(color.NRGBA)
	interpolate := func(a00, a10, a01, a11 uint8) uint8 {
		top := float64(a00)*(1-fractionX) + float64(a10)*fractionX
		bottom := float64(a01)*(1-fractionX) + float64(a11)*fractionX
		return uint8(math.Round(top*(1-fractionY) + bottom*fractionY))
	}
	return color.RGBA{
		R: interpolate(c00.R, c10.R, c01.R, c11.R),
		G: interpolate(c00.G, c10.G, c01.G, c11.G),
		B: interpolate(c00.B, c10.B, c01.B, c11.B),
		A: interpolate(c00.A, c10.A, c01.A, c11.A),
	}, true
}
