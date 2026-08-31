package workbench

import (
	"image"
	"image/color"
	"math"

	"gioui.org/f32"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
)

const (
	// 官方尺寸为 85 × 79.2 mm，宽度略大于高度；
	// LCD 直径 53.3 mm，逻辑画布只占外壳高度的近 67.3%。
	coolerShellAspect    = 85.0 / 79.2
	coolerScreenFraction = 53.3 / 79.2
)

type coolerCanvasLayout struct {
	shell  image.Point
	screen image.Point
}

// coolerCanvasGeometry 把 480×480 逻辑画布放进圆形 LCD，并在外侧保留
// 接近实物比例的六边形磁吸外壳。逻辑坐标只映射到 screen，不包含外壳。
func coolerCanvasGeometry(available image.Point, margin int) coolerCanvasLayout {
	maxWidth := max(1, available.X-2*margin)
	maxHeight := max(1, available.Y-2*margin)
	shellHeight := maxHeight
	shellWidth := int(math.Round(float64(shellHeight) * coolerShellAspect))
	if shellWidth > maxWidth {
		shellWidth = maxWidth
		shellHeight = int(math.Round(float64(shellWidth) / coolerShellAspect))
	}
	shellWidth, shellHeight = max(1, shellWidth), max(1, shellHeight)
	diameter := max(1, int(math.Round(float64(min(shellWidth, shellHeight))*coolerScreenFraction)))
	return coolerCanvasLayout{shell: image.Pt(shellWidth, shellHeight), screen: image.Pt(diameter, diameter)}
}

func (model *gioEditor) layoutCoolerCanvas(gtx layout.Context) layout.Dimensions {
	geometry := coolerCanvasGeometry(gtx.Constraints.Max, gtx.Metric.Dp(12))
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min, gtx.Constraints.Max = geometry.shell, geometry.shell
		return layout.Stack{Alignment: layout.Center}.Layout(gtx,
			layout.Expanded(func(gtx layout.Context) layout.Dimensions {
				model.drawCoolerShell(gtx, geometry.shell, geometry.screen.X)
				return layout.Dimensions{Size: geometry.shell}
			}),
			layout.Stacked(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min, gtx.Constraints.Max = geometry.screen, geometry.screen
				return model.layoutCanvasSurface(gtx)
			}),
		)
	})
}

func (model *gioEditor) drawCoolerShell(gtx layout.Context, size image.Point, screenDiameter int) {
	outerRect := image.Rect(3, 3, max(4, size.X-3), max(4, size.Y-3))
	outer := roundedHexagonPath(gtx.Ops, outerRect, float32(max(6, min(size.X, size.Y)/15)))
	paint.FillShape(gtx.Ops, color.NRGBA{R: 28, G: 36, B: 40, A: 255}, clip.Outline{Path: outer}.Op())
	paint.FillShape(gtx.Ops, color.NRGBA{R: 79, G: 96, B: 103, A: 255}, clip.Stroke{Path: outer, Width: 3}.Op())

	center := image.Pt(size.X/2, size.Y/2)
	radius := screenDiameter/2 + max(5, min(size.X, size.Y)/45)
	recess := image.Rect(center.X-radius, center.Y-radius, center.X+radius, center.Y+radius)
	paint.FillShape(gtx.Ops, color.NRGBA{R: 4, G: 10, B: 13, A: 255}, clip.Ellipse(recess).Op(gtx.Ops))
	paint.FillShape(gtx.Ops, color.NRGBA{R: 42, G: 198, B: 207, A: 150}, clip.Stroke{Path: clip.Ellipse(recess).Path(gtx.Ops), Width: 2}.Op())

	// 底部小三角把“一个尖角朝下”的编辑正向固定下来；它只属于编辑器外壳，
	// 不会进入主题或传输到设备。
	markerY := size.Y - max(13, size.Y/15)
	markerHalf := max(4, min(size.X, size.Y)/45)
	var marker clip.Path
	marker.Begin(gtx.Ops)
	marker.MoveTo(f32.Pt(float32(center.X-markerHalf), float32(markerY-markerHalf)))
	marker.LineTo(f32.Pt(float32(center.X+markerHalf), float32(markerY-markerHalf)))
	marker.LineTo(f32.Pt(float32(center.X), float32(markerY+markerHalf)))
	marker.Close()
	paint.FillShape(gtx.Ops, color.NRGBA{R: 73, G: 225, B: 220, A: 255}, clip.Outline{Path: marker.End()}.Op())
}

// roundedHexagonPath 生成上下各有一个尖角的圆角六边形，底部顶点即冷头正向。
func roundedHexagonPath(ops *op.Ops, bounds image.Rectangle, radius float32) clip.PathSpec {
	minimum := f32.Pt(float32(bounds.Min.X), float32(bounds.Min.Y))
	maximum := f32.Pt(float32(bounds.Max.X), float32(bounds.Max.Y))
	centerX := (minimum.X + maximum.X) / 2
	quarterY := minimum.Y + (maximum.Y-minimum.Y)*.25
	threeQuarterY := minimum.Y + (maximum.Y-minimum.Y)*.75
	vertices := []f32.Point{
		f32.Pt(centerX, minimum.Y),
		f32.Pt(maximum.X, quarterY),
		f32.Pt(maximum.X, threeQuarterY),
		f32.Pt(centerX, maximum.Y),
		f32.Pt(minimum.X, threeQuarterY),
		f32.Pt(minimum.X, quarterY),
	}
	radius = maxFloat32(0, minFloat32(radius, float32(min(bounds.Dx(), bounds.Dy()))*.12))
	var path clip.Path
	path.Begin(ops)
	for index, vertex := range vertices {
		previous := vertices[(index+len(vertices)-1)%len(vertices)]
		next := vertices[(index+1)%len(vertices)]
		start := pointToward(vertex, previous, radius)
		end := pointToward(vertex, next, radius)
		if index == 0 {
			path.MoveTo(start)
		} else {
			path.LineTo(start)
		}
		path.QuadTo(vertex, end)
	}
	path.Close()
	return path.End()
}

func pointToward(from, to f32.Point, distance float32) f32.Point {
	dx, dy := to.X-from.X, to.Y-from.Y
	length := float32(math.Hypot(float64(dx), float64(dy)))
	if length <= 0 || distance <= 0 {
		return from
	}
	scale := minFloat32(1, distance/length)
	return f32.Pt(from.X+dx*scale, from.Y+dy*scale)
}

func minFloat32(left, right float32) float32 {
	if left < right {
		return left
	}
	return right
}

func maxFloat32(left, right float32) float32 {
	if left > right {
		return left
	}
	return right
}
