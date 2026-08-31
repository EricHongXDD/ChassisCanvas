package workbench

import (
	"image"
	"image/color"
	"testing"

	"gioui.org/gpu/headless"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
)

// TestSpectrumPreviewGlowRasterStaysThin 直接栅格化 Gio 预览操作，
// 防止柔光再回归为宽圆头线段，在环形频谱上叠成粗大珠圈。
func TestSpectrumPreviewGlowRasterStaysThin(t *testing.T) {
	size := image.Pt(96, 48)
	var operations op.Ops
	gtx := layout.Context{
		Ops:         &operations,
		Constraints: layout.Exact(size),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
	}
	model := &gioEditor{}
	fill := color.NRGBA{R: 76, G: 224, B: 255, A: 255}
	model.previewSpectrumGlowLine(gtx, 20, 24, 76, 24, 1, 6, fill)
	model.previewSpectrumLine(gtx, 20, 24, 76, 24, 1, fill, false)

	window, err := headless.NewWindow(size.X, size.Y)
	if err != nil {
		t.Fatalf("无法创建 Gio 栅格回归窗口: %v", err)
	}
	defer window.Release()
	if err := window.Frame(&operations); err != nil {
		t.Fatalf("Gio 频谱栅格化失败: %v", err)
	}
	canvas := image.NewRGBA(image.Rectangle{Max: size})
	if err := window.Screenshot(canvas); err != nil {
		t.Fatalf("读取 Gio 频谱栅格图失败: %v", err)
	}

	visibleMinX, visibleMaxX := size.X, -1
	visibleMinY, visibleMaxY := size.Y, -1
	highAlphaPixels := 0
	maximumGlowAlpha := uint8(0)
	for y := 0; y < size.Y; y++ {
		for x := 0; x < size.X; x++ {
			pixel := canvas.RGBAAt(x, y)
			if pixel.A == 0 {
				continue
			}
			visibleMinX, visibleMaxX = min(visibleMinX, x), max(visibleMaxX, x)
			visibleMinY, visibleMaxY = min(visibleMinY, y), max(visibleMaxY, y)
			if pixel.A >= 128 {
				highAlphaPixels++
			}
			if y < 23 || y > 25 {
				if pixel.A > maximumGlowAlpha {
					maximumGlowAlpha = pixel.A
				}
			}
		}
	}
	if visibleMaxX < 0 {
		t.Fatal("Gio 环形频谱预览没有任何可见像素")
	}
	// 平头柔光只允许 1px 抗锯齿裕量；若改回圆头，
	// 6px 柔光半径会把 X 包络扩到约 14..82，此处必然失败。
	if visibleMinX < 19 || visibleMaxX > 77 {
		t.Fatalf("柔光端点形成粗大圆珠: x=[%d,%d]", visibleMinX, visibleMaxX)
	}
	if visibleMaxY-visibleMinY+1 > 15 {
		t.Fatalf("柔光横向包络过粗: y=[%d,%d]", visibleMinY, visibleMaxY)
	}
	// 不透明主线长约 57px；高 alpha 面积不应扩张成宽带。
	if highAlphaPixels < 45 || highAlphaPixels > 180 {
		t.Fatalf("细环主线的高 alpha 覆盖面积异常: %d", highAlphaPixels)
	}
	if maximumGlowAlpha > 48 {
		t.Fatalf("主线外柔光过于不透明: alpha=%d", maximumGlowAlpha)
	}
}
