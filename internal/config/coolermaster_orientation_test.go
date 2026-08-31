package config

import (
	"path/filepath"
	"testing"
)

func coolerOrientationConfig(rotation, profile int, calibrated bool) Config {
	cfg := Default()
	cfg.Devices = []Device{{
		ID: "coolermaster-atmos-v2", Kind: "coolermaster", Enabled: true,
		Width: 480, Height: 480, Slot: 4,
		DisplayRotation: rotation, OrientationCalibrated: calibrated, OrientationProfile: profile,
	}}
	return cfg
}

func TestCoolerMasterOrientationConfigAcceptsThirtyDegreeSteps(t *testing.T) {
	for rotation := 0; rotation < 360; rotation += 30 {
		cfg := coolerOrientationConfig(rotation, 1, true)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("合法方向 %d 度未通过校验: %v", rotation, err)
		}
	}
}

func TestCoolerMasterOrientationConfigRejectsInvalidValues(t *testing.T) {
	for _, rotation := range []int{-30, -1, 1, 29, 31, 359, 360, 390} {
		t.Run("rotation", func(t *testing.T) {
			cfg := coolerOrientationConfig(rotation, 1, true)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("非法方向 %d 度未被拒绝", rotation)
			}
		})
	}
	for _, profile := range []int{-1, 2, 99} {
		t.Run("profile", func(t *testing.T) {
			cfg := coolerOrientationConfig(90, profile, true)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("非法方向校准版本 %d 未被拒绝", profile)
			}
		})
	}
}

func TestCoolerMasterOrientationConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := coolerOrientationConfig(330, 1, true)
	if err := store.Set(want); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	devices := reopened.Get().Devices
	if len(devices) != 1 {
		t.Fatalf("方向配置重开后设备数量错误: %+v", devices)
	}
	got := devices[0]
	if got.DisplayRotation != 330 || !got.OrientationCalibrated || got.OrientationProfile != 1 {
		t.Fatalf("方向配置未完整持久化: %+v", got)
	}
}
