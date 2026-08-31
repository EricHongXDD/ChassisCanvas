package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"casescreen/internal/gpupreference"
)

func TestGPUPreferenceRoundTripAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	if cfg.GPUPreference != string(gpupreference.SystemDefault) {
		t.Fatalf("默认 GPU 首选项 = %q", cfg.GPUPreference)
	}
	cfg.GPUPreference = string(gpupreference.PowerSaving)
	cfg.GPUAdapterID = `PCI\VEN_8086&DEV_7D67`
	cfg.GPUAdapterName = "Intel(R) Graphics"
	if err := store.Set(cfg); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reopenedConfig := reopened.Get()
	if reopenedConfig.GPUPreference != string(gpupreference.PowerSaving) || reopenedConfig.GPUAdapterID != `PCI\VEN_8086&DEV_7D67` || reopenedConfig.GPUAdapterName != "Intel(R) Graphics" {
		t.Fatalf("重开后的显卡选择 = %+v", reopenedConfig)
	}
	cfg = reopened.Get()
	cfg.GPUPreference = "force-discrete"
	if err := cfg.Validate(); err == nil {
		t.Fatal("应拒绝无法实现的强制 GPU 设置")
	}
	cfg = Default()
	cfg.GPUAdapterID = `PCI\VEN_8086&DEV_7D67`
	if err := cfg.Validate(); err == nil {
		t.Fatal("应拒绝缺少名称的不完整显卡选择")
	}
}

func TestOpenMigratesMissingGPUPreference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Default()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	delete(document, "gpu_preference")
	data, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("旧配置应可迁移: %v", err)
	}
	if got := store.Get().GPUPreference; got != string(gpupreference.SystemDefault) {
		t.Fatalf("迁移值 = %q", got)
	}
	if store.Get().GPUAdapterID != "" || store.Get().GPUAdapterName != "" {
		t.Fatalf("旧配置迁移不应伪造显卡选择: %+v", store.Get())
	}
}

func TestOpenAcceptsLegacyGPUPreferenceWithoutAdapterIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Default()
	cfg.GPUPreference = string(gpupreference.HighPerformance)
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("只有旧版枚举字段的配置应继续可用: %v", err)
	}
	loaded := store.Get()
	if loaded.GPUPreference != string(gpupreference.HighPerformance) || loaded.GPUAdapterID != "" || loaded.GPUAdapterName != "" {
		t.Fatalf("旧版 GPU 配置迁移错误: %+v", loaded)
	}
}
