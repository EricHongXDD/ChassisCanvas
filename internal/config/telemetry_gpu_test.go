package config

import (
	"path/filepath"
	"testing"
)

func TestTelemetryGPUSelectionRoundTripAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := store.Get(); cfg.TelemetryGPUAdapterID != "" || cfg.TelemetryGPUAdapterName != "" {
		t.Fatalf("默认应为自动监控 GPU: %+v", cfg)
	}
	if err := store.Update(func(cfg *Config) error {
		cfg.TelemetryGPUAdapterID = `PCI\VEN_1002&DEV_744C`
		cfg.TelemetryGPUAdapterName = "AMD Radeon RX 7900 XTX"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded := reopened.Get()
	if loaded.TelemetryGPUAdapterID != `PCI\VEN_1002&DEV_744C` || loaded.TelemetryGPUAdapterName != "AMD Radeon RX 7900 XTX" {
		t.Fatalf("监控 GPU 没有跨重启保存: %+v", loaded)
	}
	loaded.TelemetryGPUAdapterName = ""
	if err := loaded.Validate(); err == nil {
		t.Fatal("物理监控 GPU 缺少名称时应拒绝配置")
	}
	loaded.TelemetryGPUAdapterID = "auto"
	loaded.TelemetryGPUAdapterName = "AMD Radeon RX 7900 XTX"
	if err := loaded.Validate(); err == nil {
		t.Fatal("自动监控 GPU 不应接受残留名称")
	}
}
