package api

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("CASESCREEN_WAVESHARE_BASE_THEME") != "" {
		os.Exit(m.Run())
	}
	directory, err := os.MkdirTemp("", "casescreen-waveshare-test-")
	if err != nil {
		os.Exit(1)
	}
	fixturePath := filepath.Join(directory, "synthetic-base.Theme")
	if err := os.WriteFile(fixturePath, syntheticWaveshareTestBaseTheme(), 0o600); err != nil {
		_ = os.RemoveAll(directory)
		os.Exit(1)
	}
	_ = os.Setenv("CASESCREEN_WAVESHARE_BASE_THEME", fixturePath)
	code := m.Run()
	_ = os.Unsetenv("CASESCREEN_WAVESHARE_BASE_THEME")
	_ = os.RemoveAll(directory)
	os.Exit(code)
}

// requireWaveshareTestBaseTheme 只从用户本机读取原厂主题测试样本。
// 该样本不属于公开源码；干净环境缺少样本时，仅跳过依赖它的兼容性测试。
func requireWaveshareTestBaseTheme(t *testing.T) []byte {
	t.Helper()
	candidates := make([]string, 0, 3)
	if configured := os.Getenv("CASESCREEN_WAVESHARE_BASE_THEME"); configured != "" {
		candidates = append(candidates, configured)
	}
	candidates = append(candidates,
		filepath.Join("assets", "070-astronaut.Theme"),
		filepath.Join("..", "..", ".tmp", "waveshare", "070-astronaut.Theme"),
	)
	for _, candidate := range candidates {
		data, err := os.ReadFile(candidate)
		if err == nil && waveshareThemeHasRequiredTemplates(data) {
			return data
		}
	}
	t.Skip("未配置用户自有的微雪原厂 Theme 测试样本")
	return nil
}

// syntheticWaveshareTestBaseTheme 只描述公开可推导的文件结构和控件字段，
// 不复制原厂主题中的媒体、布局、文字、图像或其他创作内容。
func syntheticWaveshareTestBaseTheme() []byte {
	baseFields := map[string]string{
		"id": "1", "itemName": "synthetic", "x": "0", "y": "0", "z": "0",
		"rotate": "0", "scale": "1", "lock": "1", "system_data_flag": "0",
		"system_data_name": "", "system_data_min_value": "0", "system_data_max_value": "100",
	}
	widget := func(widgetType string, extra map[string]string) map[string]string {
		fields := make(map[string]string, len(baseFields)+len(extra)+1)
		for key, value := range baseFields {
			fields[key] = value
		}
		fields["type"] = widgetType
		for key, value := range extra {
			fields[key] = value
		}
		return fields
	}
	widgets := []map[string]string{
		widget("8", map[string]string{
			"text_str": "", "text_size": "24", "text_color": "r=255,g=255,b=255,a=255",
			"text_customFont_path": "", "text_blod": "0", "text_italic": "0",
		}),
		widget("100", map[string]string{
			"w": "1024", "h": "600", "maxWidth": "1024", "maxHeight": "600",
			"path": "/video/background.mp4",
		}),
		widget("101", map[string]string{
			"radius": "50", "margin": "5", "front_color": "r=255,g=255,b=255,a=255",
			"back_color": "r=0,g=0,b=0,a=0",
		}),
		widget("102", map[string]string{
			"w": "100", "h": "10", "front_color": "r=255,g=255,b=255,a=255",
			"back_color": "r=0,g=0,b=0,a=0", "border_color": "r=0,g=0,b=0,a=0",
			"border_width": "0", "corner_radius": "0", "lineargradient_flag": "0",
			"lineargradient_color": "r=255,g=255,b=255,a=255",
		}),
		widget("108", map[string]string{
			"w": "100", "h": "100", "front_color": "r=255,g=255,b=255,a=255",
			"back_color": "r=0,g=0,b=0,a=0",
		}),
	}

	var output bytes.Buffer
	output.WriteString(waveshareThemeHeader)
	writeWaveshareUint32(&output, 0)
	writeWaveshareStringMap(&output, map[string]string{
		"canvas_w": "1024", "canvas_h": "600", "canvas_rotate": "0", "canvas_flip": "0",
	}, nil)
	writeWaveshareUint32(&output, uint32(len(widgets)))
	for _, fields := range widgets {
		writeWaveshareStringMap(&output, fields, nil)
	}
	video := syntheticWaveshareTestMP4()
	output.Write([]byte{byte(len(video) >> 16), byte(len(video) >> 8), byte(len(video))})
	output.Write(video)
	return output.Bytes()
}

func syntheticWaveshareTestMP4() []byte {
	var output bytes.Buffer
	writeBox := func(boxType string, body []byte) {
		_ = binary.Write(&output, binary.BigEndian, uint32(8+len(body)))
		output.WriteString(boxType)
		output.Write(body)
	}
	writeBox("ftyp", []byte("isom\x00\x00\x02\x00isom"))
	writeBox("moov", nil)
	writeBox("mdat", []byte("synthetic"))
	return output.Bytes()
}
