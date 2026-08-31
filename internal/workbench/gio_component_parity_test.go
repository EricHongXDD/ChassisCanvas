package workbench

import (
	"image"
	"math"
	"strings"
	"testing"

	caseTheme "casescreen/internal/theme"
)

func TestSpectrumPreviewUsesSameIdleBaselineAsPhysicalScreens(t *testing.T) {
	waveshare := &gioEditor{target: "waveshare-7"}
	visible := waveshare.visibleAudioPreviewValues([]float64{0, .02, .4})
	if len(visible) != 3 || math.Abs(visible[0]-caseTheme.DefaultAudioIdleAmplitude) > 1e-9 ||
		math.Abs(visible[1]-caseTheme.DefaultAudioIdleAmplitude) > 1e-9 || visible[2] != .4 {
		t.Fatalf("微雪预览静态基线与设备小包不一致: %v", visible)
	}

	hostFrame := &gioEditor{target: "coolermaster-atmos-v2"}
	if got := hostFrame.visibleAudioPreviewValues([]float64{0, .02}); got[0] != caseTheme.DefaultAudioIdleAmplitude || got[1] != caseTheme.DefaultAudioIdleAmplitude {
		t.Fatalf("主机合成预览没有与实体屏保持静态基线: %v", got)
	}
}

func TestSpectrumPreviewStrokeScalesAndCapsDoNotGrowPastLineWidth(t *testing.T) {
	if got := scaledSpectrumPreviewWidth(4, .5, 30); got != 2 {
		t.Fatalf("环形频谱线宽没有按画布缩放: %d", got)
	}
	if got := scaledSpectrumPreviewWidthAllowZero(6, .5, 16); got != 3 {
		t.Fatalf("环形频谱柔光宽度没有按画布缩放: %d", got)
	}
	for _, width := range []int{1, 2, 3, 4, 7} {
		cap := spectrumPreviewCapRect(image.Pt(20, 30), width)
		if cap.Dx() != width || cap.Dy() != width {
			t.Fatalf("圆头直径超过线宽: width=%d rect=%v", width, cap)
		}
	}
	passes := spectrumPreviewGlowPasses(1, 6)
	if len(passes) != 2 || passes[0].width != 13 || passes[1].width != 7 ||
		math.Abs(passes[0].alpha-.045) > 1e-9 || math.Abs(passes[1].alpha-.09) > 1e-9 {
		t.Fatalf("环形预览柔光没有采用低透明度双层轮廓: %+v", passes)
	}
	if passes := spectrumPreviewGlowPasses(3, 0); len(passes) != 0 {
		t.Fatalf("关闭柔光后仍生成轮廓: %+v", passes)
	}
}

func TestWaveshareNativeLinearPreviewLayoutMatchesCompilerPacking(t *testing.T) {
	tests := []struct {
		name                         string
		length, requestedCount, gap  int
		wantCount, wantSize, wantGap int
	}{
		{name: "水平空间足够", length: 120, requestedCount: 24, gap: 3, wantCount: 24, wantSize: 2, wantGap: 3},
		{name: "水平高度限制频带", length: 20, requestedCount: 24, gap: 3, wantCount: 20, wantSize: 1, wantGap: 0},
		{name: "单像素图层", length: 1, requestedCount: 48, gap: 9, wantCount: 1, wantSize: 1, wantGap: 0},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			count, size, gap := waveshareNativeLinearPreviewLayout(testCase.length, testCase.requestedCount, testCase.gap)
			if count != testCase.wantCount || size != testCase.wantSize || gap != testCase.wantGap {
				t.Fatalf("原生频带打包不一致: got=(%d,%d,%d) want=(%d,%d,%d)", count, size, gap, testCase.wantCount, testCase.wantSize, testCase.wantGap)
			}
			if used := count*size + max(0, count-1)*gap; used > max(1, testCase.length) {
				t.Fatalf("频带布局越界: used=%d length=%d", used, testCase.length)
			}
		})
	}
}

func TestWaveshareNativeRingStyleGeometryMatchesCompiler(t *testing.T) {
	tests := []struct {
		style                 string
		wantLength, wantWidth int
	}{
		{style: "radial", wantLength: 20, wantWidth: 5},
		{style: "waveform", wantLength: 20, wantWidth: 5},
		{style: "donut", wantLength: 10, wantWidth: 10},
		{style: "dots", wantLength: 5, wantWidth: 5},
		{style: "dashed", wantLength: 15, wantWidth: 5},
		{style: "halo", wantLength: 20, wantWidth: 5},
		{style: "spokes", wantLength: 49, wantWidth: 5},
		{style: "pulse", wantLength: 20, wantWidth: 10},
	}
	for _, testCase := range tests {
		t.Run(testCase.style, func(t *testing.T) {
			layer := caseTheme.Layer{Options: map[string]any{
				"style": testCase.style, "inner_radius": .8, "thickness": 5,
			}}
			geometry := waveshareNativeRingPreviewGeometry(image.Pt(200, 200), layer, 1)
			if geometry.outerRadius != 98 || geometry.innerRadius != 78 || geometry.columnLength != testCase.wantLength || geometry.columnWidth != testCase.wantWidth {
				t.Fatalf("%s 原生环形几何漂移: %+v", testCase.style, geometry)
			}
		})
	}
}

func TestNativeWaveshareControlLabelsDiscloseProtocolFallbacks(t *testing.T) {
	var nativeDial, genericDial string
	for _, choice := range gaugeStyleChoices(true) {
		if choice.value == "dial" {
			nativeDial = choice.label
		}
	}
	for _, choice := range gaugeStyleChoices(false) {
		if choice.value == "dial" {
			genericDial = choice.label
		}
	}
	if !strings.Contains(nativeDial, "无指针") || genericDial != "指针" {
		t.Fatalf("仪表能力标签不准确: native=%q generic=%q", nativeDial, genericDial)
	}

	for _, choice := range lineChartStyleChoices(true) {
		if (choice.value == "step" || choice.value == "spline" || choice.value == "dots") && !strings.Contains(choice.label, "→折线") {
			t.Fatalf("微雪曲线降级没有在样式名称中明示: %+v", choice)
		}
	}
}

func TestLineChartPreviewKeepsHostShapesAndUsesNativeFallbacks(t *testing.T) {
	tests := []struct {
		style      string
		waveshare  bool
		wantStyle  string
		wantNoGrid bool
		wantFixed  bool
	}{
		{style: "step", waveshare: true, wantStyle: "line", wantFixed: true},
		{style: "spline", waveshare: true, wantStyle: "line", wantNoGrid: true, wantFixed: true},
		{style: "dots", waveshare: true, wantStyle: "line", wantNoGrid: true, wantFixed: true},
		{style: "neon", waveshare: true, wantStyle: "area", wantNoGrid: true, wantFixed: true},
		{style: "step", waveshare: false, wantStyle: "step"},
		{style: "spline", waveshare: false, wantStyle: "spline"},
		{style: "dots", waveshare: false, wantStyle: "dots"},
	}
	for _, testCase := range tests {
		renderStyle, noGrid, fixed := lineChartPreviewStyle(testCase.style, testCase.waveshare)
		if renderStyle != testCase.wantStyle || noGrid != testCase.wantNoGrid || fixed != testCase.wantFixed {
			t.Fatalf("曲线能力映射错误: style=%s waveshare=%v got=(%s,%v,%v) want=(%s,%v,%v)", testCase.style, testCase.waveshare, renderStyle, noGrid, fixed, testCase.wantStyle, testCase.wantNoGrid, testCase.wantFixed)
		}
	}

	first, second, third, fourth := image.Pt(0, 5), image.Pt(10, 10), image.Pt(20, 0), image.Pt(30, 5)
	if got := previewCatmullRomPoint(first, second, third, fourth, 0); got != second {
		t.Fatalf("平滑曲线起点错误: got=%v want=%v", got, second)
	}
	if got := previewCatmullRomPoint(first, second, third, fourth, 1); got != third {
		t.Fatalf("平滑曲线终点错误: got=%v want=%v", got, third)
	}
}
