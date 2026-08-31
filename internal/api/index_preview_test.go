package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReachableWebEditorExposesAndPreviewsFourFontWeights(t *testing.T) {
	server, _ := newTestServer(t)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("Web 编辑器入口状态=%d", response.Code)
	}
	page := response.Body.String()
	required := []string{
		`id="layerFontFamily"`, `id="layerFontWeight"`,
		`<option value="light">细体</option>`, `<option value="regular">常规</option>`,
		`<option value="medium">中粗</option>`, `<option value="bold">粗体</option>`,
		"function previewFontWeight(options={})", "function previewFontFamily(options={})",
		"function drawPreviewStyledText(ctx,text,x,y,size,options={})",
		"layer.options.font_family=$('layerFontFamily').value",
		"layer.options.font_weight=$('layerFontWeight').value",
	}
	for _, fragment := range required {
		if !strings.Contains(page, fragment) {
			t.Errorf("Web 编辑器字体契约缺少 %q", fragment)
		}
	}
}

func TestWebEditorSourceHanNeverFallsBackToMicrosoftYaHei(t *testing.T) {
	const sourceHanStack = `'source-han':'Noto Sans CJK SC,SimHei,DengXian,sans-serif'`
	if !strings.Contains(indexHTML, sourceHanStack) {
		t.Fatalf("Web Source Han 缺少与雅黑不同的有界后备字体栈")
	}
	if strings.Contains(indexHTML, `'source-han':'Noto Sans CJK SC,Microsoft YaHei`) {
		t.Fatal("Web Source Han 仍会回退到微软雅黑")
	}
}

func TestWebEditorLabelsNonCJKFontsAsLyricsFallback(t *testing.T) {
	for _, fragment := range []string{
		`液晶数字（数字/英文组件专用，歌词回退思源）`,
		`等宽（数字/英文组件专用，歌词回退思源）`,
	} {
		if !strings.Contains(indexHTML, fragment) {
			t.Fatalf("Web 字体选择器缺少歌词安全提示 %q", fragment)
		}
	}
}

func TestWebEditorSpectrumPreviewUsesAnimatedSimulationWithoutLiveAudio(t *testing.T) {
	required := []string{
		"function simulatedPreviewAudioBands(count,now=Date.now())",
		"if(!liveFresh)input=simulatedPreviewAudioBands(output.length,now)",
		"previewNumberOption(options,'min_amplitude',0)",
		"function tickAudioPreview()",
		"setInterval(tickAudioPreview,100)",
	}
	for _, fragment := range required {
		if !strings.Contains(indexHTML, fragment) {
			t.Fatalf("Web 编辑器频谱演示缺少行为片段 %q", fragment)
		}
	}
}

func TestWebEditorSpectrumPreviewMatchesStoredThemeGeometryAndIdleBaseline(t *testing.T) {
	required := []string{
		"function previewVisibleSpectrumValue(value)",
		"return Math.max(.04,value)",
		"function previewPackedSpectrumLayout(length,requestedCount,requestedGap)",
		"if(direction==='top')",
		"else if(direction==='center')",
		"if(direction==='inward')",
		"else if(direction==='bidirectional')",
		"previewNumberOption(options,'thickness',1)",
		"options.bar_shape||'square'",
		"ctx.lineCap=roundCaps?'round':'butt'",
		"ctx.lineWidth=lineWidth",
	}
	for _, fragment := range required {
		if !strings.Contains(indexHTML, fragment) {
			t.Fatalf("Web 编辑器频谱与驻留主题语义不一致：缺少 %q", fragment)
		}
	}
	if strings.Contains(indexHTML, "previewIsWaveshareTarget()?Math.max(.04,value):value") {
		t.Fatal("Web 端仍只给微雪预览保留静态基线")
	}
}

func TestWebEditorLyricsPreviewUsesSharedDefaultFontSizes(t *testing.T) {
	required := []string{
		"defaultLyricsFontSize=34,defaultLyricsNextFontSize=20",
		"Number(options.font_size)||defaultLyricsFontSize",
		"Number(options.next_font_size)||defaultLyricsNextFontSize",
		"options.font_size=defaultLyricsFontSize",
		"options.next_font_size=defaultLyricsNextFontSize",
	}
	for _, fragment := range required {
		if !strings.Contains(indexHTML, fragment) {
			t.Fatalf("Web 歌词默认字号与主题常量不一致：缺少 %q", fragment)
		}
	}
}
