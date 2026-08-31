package coolergeometry

import (
	"image"
	"image/color"
	"testing"
)

var transformResultSink image.Image

func TestNormalizeOrientation(t *testing.T) {
	tests := map[int]int{
		0: 0, 29: 29, 30: 30, 59: 59, 60: 60, 89: 89, 90: 90,
		299: 299, 300: 300, 329: 329, 330: 330, 359: 359, 360: 0, 420: 60,
		-1: 359, -29: 331, -30: 330, -31: 329, -60: 300, -420: 300,
	}
	for input, expected := range tests {
		if actual := NormalizeOrientation(input); actual != expected {
			t.Errorf("NormalizeOrientation(%d) = %d, 期望 %d", input, actual, expected)
		}
	}
}

func TestMarkerCorrection(t *testing.T) {
	tests := map[string]int{
		"6": 0, " 3 ": 270, "12": 180, "9": 90, "1": 210, "11": 150,
	}
	for marker, expected := range tests {
		actual, err := MarkerCorrection(marker)
		if err != nil {
			t.Fatalf("MarkerCorrection(%q) 返回意外错误: %v", marker, err)
		}
		if actual != expected {
			t.Errorf("MarkerCorrection(%q) = %d, 期望 %d", marker, actual, expected)
		}
	}
	for _, marker := range []string{"", "A", "0", "13"} {
		if _, err := MarkerCorrection(marker); err == nil {
			t.Errorf("MarkerCorrection(%q) 应返回错误", marker)
		}
	}
}

func TestHexagonVerticesStartAtBottomAndProceedClockwise(t *testing.T) {
	vertices := HexagonVertices(480, 480)
	if vertices[0].Y <= 240 || vertices[0].X != 240 {
		t.Fatalf("A 顶点应位于下方中央，实际为 %v", vertices[0])
	}
	if vertices[1].X >= 240 || vertices[1].Y <= 240 {
		t.Fatalf("B 顶点应位于左下方，实际为 %v", vertices[1])
	}
	if vertices[3].Y >= 240 || vertices[3].X != 240 {
		t.Fatalf("D 顶点应位于上方中央，实际为 %v", vertices[3])
	}
}

func TestApplyHexMask(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 120, 120))
	for y := 0; y < 120; y++ {
		for x := 0; x < 120; x++ {
			source.SetRGBA(x, y, color.RGBA{R: 220, G: 80, B: 30, A: 255})
		}
	}
	masked := ApplyHexMask(source)
	if masked.Bounds() != source.Bounds() {
		t.Fatalf("遮罩尺寸 = %v，期望 %v", masked.Bounds(), source.Bounds())
	}
	if got := masked.RGBAAt(60, 60); got.R < 200 || got.A != 255 {
		t.Fatalf("中心像素应保留源图颜色，实际为 %v", got)
	}
	for _, point := range []image.Point{{0, 0}, {119, 0}, {0, 119}, {119, 119}} {
		if got := masked.RGBAAt(point.X, point.Y); got != (color.RGBA{A: 255}) {
			t.Errorf("角落 %v 应为不透明黑色，实际为 %v", point, got)
		}
	}
}

func TestApplyCircleMask(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 120, 120))
	for y := 0; y < 120; y++ {
		for x := 0; x < 120; x++ {
			source.SetRGBA(x, y, color.RGBA{R: 30, G: 160, B: 230, A: 255})
		}
	}
	masked := ApplyCircleMask(source)
	if got := masked.RGBAAt(60, 60); got.B < 200 || got.A != 255 {
		t.Fatalf("圆形遮罩应保留中心像素，实际为 %v", got)
	}
	if got := masked.RGBAAt(0, 0); got != (color.RGBA{A: 255}) {
		t.Fatalf("圆形遮罩角落应为不透明黑色，实际为 %v", got)
	}
}

func TestRotateClockwiseAndKeepSize(t *testing.T) {
	const size = 121
	source := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 91; y <= 99; y++ {
		for x := 57; x <= 63; x++ {
			source.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	rotated := Rotate(source, 60)
	if rotated.Bounds() != source.Bounds() {
		t.Fatalf("旋转尺寸 = %v，期望 %v", rotated.Bounds(), source.Bounds())
	}
	// 下方标记顺时针旋转 60 度后应移到左下方。
	if !containsRed(rotated, image.Rect(25, 71, 40, 89)) {
		t.Fatal("顺时针旋转 60 度后未在左下方找到标记")
	}
	if got := rotated.RGBAAt(0, 0); got != (color.RGBA{A: 255}) {
		t.Fatalf("旋转后角落应为不透明黑色，实际为 %v", got)
	}
}

func TestTransformAppliesCircleMaskAndArbitraryRotation(t *testing.T) {
	const size = 121
	source := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			source.SetRGBA(x, y, color.RGBA{G: 210, A: 255})
		}
	}
	transformed := Transform(source, 17)
	if transformed.Bounds() != source.Bounds() {
		t.Fatalf("变换尺寸 = %v，期望 %v", transformed.Bounds(), source.Bounds())
	}
	if got := color.RGBAModel.Convert(transformed.At(0, 0)).(color.RGBA); got != (color.RGBA{A: 255}) {
		t.Fatalf("变换应先应用圆形遮罩，角落实际为 %v", got)
	}
	if got := color.RGBAModel.Convert(transformed.At(size/2, size/2)).(color.RGBA); got.G < 180 {
		t.Fatalf("任意角旋转后中心内容应保留，实际为 %v", got)
	}
}

func TestTransformKeepsAllocationsBounded(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			source.SetRGBA(x, y, color.RGBA{R: uint8(x * 3), G: uint8(y * 3), B: 90, A: 255})
		}
	}
	// 先预热尺寸缓存，测量的是每帧路径，而不是首次建立圆形遮罩。
	transformResultSink = Transform(source, 30)
	allocations := testing.AllocsPerRun(20, func() {
		transformResultSink = Transform(source, 30)
	})
	if allocations > 4 {
		t.Fatalf("Transform 每帧分配 %.1f 次，期望不超过 4 次", allocations)
	}
}

func TestTransformIntegerRotationFastPaths(t *testing.T) {
	tests := []struct {
		rotation int
		expected image.Point
	}{
		{rotation: 0, expected: image.Pt(7, 5)},
		{rotation: 90, expected: image.Pt(5, 7)},
		{rotation: 180, expected: image.Pt(3, 5)},
		{rotation: 270, expected: image.Pt(5, 3)},
	}
	for _, test := range tests {
		source := image.NewRGBA(image.Rect(0, 0, 11, 11))
		for y := 0; y < 11; y++ {
			for x := 0; x < 11; x++ {
				source.SetRGBA(x, y, color.RGBA{A: 255})
			}
		}
		source.SetRGBA(7, 5, color.RGBA{R: 255, A: 255})
		transformed := Transform(source, test.rotation)
		actual := color.RGBAModel.Convert(transformed.At(test.expected.X, test.expected.Y)).(color.RGBA)
		if actual.R != 255 || actual.G != 0 || actual.B != 0 || actual.A != 255 {
			t.Errorf("rotation=%d 的整数快路映射错误: point=%v color=%v", test.rotation, test.expected, actual)
		}
	}
}

func TestTransformIntegerFastPathPreservesTransparentComposition(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 11, 11))
	source.SetNRGBA(5, 5, color.NRGBA{R: 200, G: 100, B: 50, A: 128})
	transformed := Transform(source, 0)
	actual := color.RGBAModel.Convert(transformed.At(5, 5)).(color.RGBA)
	if actual.A != 255 || absoluteDifference(actual.R, 100) > 1 || absoluteDifference(actual.G, 50) > 1 || absoluteDifference(actual.B, 25) > 1 {
		t.Fatalf("整数快路应把半透明像素合成到不透明黑底，实际为 %v", actual)
	}
}

func BenchmarkTransform480(b *testing.B) {
	source := image.NewRGBA(image.Rect(0, 0, DefaultWidth, DefaultHeight))
	for y := 0; y < DefaultHeight; y++ {
		for x := 0; x < DefaultWidth; x++ {
			source.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: uint8(x + y), A: 255})
		}
	}
	benchmarks := []struct {
		name     string
		rotation int
	}{
		{name: "rotation_0", rotation: 0},
		{name: "rotation_90", rotation: 90},
		{name: "rotation_180", rotation: 180},
		{name: "rotation_270", rotation: 270},
		{name: "rotation_30_bilinear", rotation: 30},
	}
	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				transformResultSink = Transform(source, benchmark.rotation)
			}
		})
	}
}

func TestCalibrationImage(t *testing.T) {
	calibration := CalibrationImage(DefaultWidth, DefaultHeight)
	if calibration.Bounds() != image.Rect(0, 0, DefaultWidth, DefaultHeight) {
		t.Fatalf("校准图尺寸错误: %v", calibration.Bounds())
	}
	if got := calibration.RGBAAt(0, 0); got != (color.RGBA{A: 255}) {
		t.Fatalf("校准图角落应为不透明黑色，实际为 %v", got)
	}
	// 钟面的 6 位于下方，检查其周边确保标识已绘制。
	if !containsColorNear(calibration, image.Pt(240, 420), calibrationColors[5], 45) {
		t.Fatal("校准图下方未找到数字 6 标识颜色")
	}
}

func containsRed(source *image.RGBA, bounds image.Rectangle) bool {
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			pixel := source.RGBAAt(x, y)
			if pixel.R > 180 && pixel.G < 60 && pixel.B < 60 {
				return true
			}
		}
	}
	return false
}

func containsColorNear(source *image.RGBA, center image.Point, expected color.RGBA, radius int) bool {
	for y := max(0, center.Y-radius); y <= min(source.Bounds().Max.Y-1, center.Y+radius); y++ {
		for x := max(0, center.X-radius); x <= min(source.Bounds().Max.X-1, center.X+radius); x++ {
			pixel := source.RGBAAt(x, y)
			if absoluteDifference(pixel.R, expected.R) < 8 && absoluteDifference(pixel.G, expected.G) < 8 && absoluteDifference(pixel.B, expected.B) < 8 {
				return true
			}
		}
	}
	return false
}

func absoluteDifference(left, right uint8) int {
	if left > right {
		return int(left - right)
	}
	return int(right - left)
}
