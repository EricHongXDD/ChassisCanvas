package appassets

import (
	"bytes"
	"image/png"
	"testing"
)

func TestIconPNGIsTransparent256Square(t *testing.T) {
	data := IconPNG()
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("解码应用图标失败: %v", err)
	}
	if config.Width != 256 || config.Height != 256 {
		t.Fatalf("应用图标必须是 256×256，实际为 %d×%d", config.Width, config.Height)
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("完整解码应用图标失败: %v", err)
	}
	_, _, _, cornerAlpha := decoded.At(0, 0).RGBA()
	if cornerAlpha != 0 {
		t.Fatalf("应用图标四角应保持透明，左上角 alpha=%d", cornerAlpha)
	}
}
