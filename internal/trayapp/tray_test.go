package trayapp

import (
	"bytes"
	"image/png"
	"testing"
)

func TestTrayIconIsValidPNG(t *testing.T) {
	data, err := makeIcon()
	if err != nil {
		t.Fatal(err)
	}
	image, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if image.Bounds().Dx() != 256 || image.Bounds().Dy() != 256 {
		t.Fatalf("托盘图标尺寸错误: %v", image.Bounds())
	}
	_, _, _, alpha := image.At(0, 0).RGBA()
	if alpha != 0 {
		t.Fatalf("托盘图标四角应保持透明，alpha=%d", alpha)
	}
}
