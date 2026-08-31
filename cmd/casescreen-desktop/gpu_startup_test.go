package main

import (
	"errors"
	"strings"
	"testing"

	"casescreen/internal/gpupreference"
)

type fakeStartupGPUPreferenceManager struct {
	applyResult        gpupreference.ApplyResult
	applyAdapterResult gpupreference.ApplyResult
	applyErr           error
	applyAdapterErr    error
	appliedPreference  gpupreference.Preference
	appliedAdapterID   string
}

func (manager *fakeStartupGPUPreferenceManager) Apply(preference gpupreference.Preference) (gpupreference.ApplyResult, error) {
	manager.appliedPreference = preference
	return manager.applyResult, manager.applyErr
}

func (manager *fakeStartupGPUPreferenceManager) ApplyAdapter(adapterID string) (gpupreference.ApplyResult, error) {
	manager.appliedAdapterID = adapterID
	return manager.applyAdapterResult, manager.applyAdapterErr
}

func TestApplySavedGPUPreferenceUsesSavedAdapter(t *testing.T) {
	manager := &fakeStartupGPUPreferenceManager{
		applyAdapterResult: gpupreference.ApplyResult{Changed: true},
	}
	result, err := applySavedGPUPreference(manager, string(gpupreference.PowerSaving), ` PCI\VEN_8086&DEV_7D67 `)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || manager.appliedAdapterID != `PCI\VEN_8086&DEV_7D67` || manager.appliedPreference != "" {
		t.Fatalf("没有优先恢复已保存适配器: result=%+v manager=%+v", result, manager)
	}
}

func TestApplySavedGPUPreferenceFallsBackWhenAdapterTemporarilyMissing(t *testing.T) {
	manager := &fakeStartupGPUPreferenceManager{
		applyResult:     gpupreference.ApplyResult{Changed: true},
		applyAdapterErr: errors.Join(gpupreference.ErrAdapterNotFound, errors.New("not enumerated")),
	}
	result, err := applySavedGPUPreference(manager, string(gpupreference.PowerSaving), `PCI\VEN_8086&DEV_7D67`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || manager.appliedPreference != gpupreference.PowerSaving {
		t.Fatalf("适配器暂时缺失时没有恢复已保存类别: result=%+v manager=%+v", result, manager)
	}
}

func TestApplySavedGPUPreferenceUsesSavedCategoryWithoutAdapter(t *testing.T) {
	manager := &fakeStartupGPUPreferenceManager{applyResult: gpupreference.ApplyResult{Changed: true}}
	result, err := applySavedGPUPreference(manager, string(gpupreference.SystemDefault), "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || manager.appliedPreference != gpupreference.SystemDefault || manager.appliedAdapterID != "" {
		t.Fatalf("无适配器配置没有恢复已保存类别: result=%+v manager=%+v", result, manager)
	}
}

func TestApplySavedGPUPreferenceDoesNotHideRegistryFailure(t *testing.T) {
	want := errors.New("access denied")
	manager := &fakeStartupGPUPreferenceManager{applyAdapterErr: want}
	if _, err := applySavedGPUPreference(manager, string(gpupreference.PowerSaving), `PCI\VEN_8086&DEV_7D67`); !errors.Is(err, want) {
		t.Fatalf("注册表失败被隐藏: %v", err)
	}
	if manager.appliedPreference != "" {
		t.Fatal("非适配器缺失错误后不应再覆盖写入")
	}
}

func TestEnvironmentWithGPUPreferenceRestartMarkerReplacesExistingValue(t *testing.T) {
	environment := environmentWithGPUPreferenceRestartMarker([]string{
		"PATH=test",
		strings.ToLower(gpuPreferenceRestartMarker) + "=old",
		gpuPreferenceRestartMarker + "=duplicate",
	})
	markerCount := 0
	for _, entry := range environment {
		if strings.HasPrefix(strings.ToUpper(entry), gpuPreferenceRestartMarker+"=") {
			markerCount++
			if entry != gpuPreferenceRestartMarker+"="+gpuPreferenceRestartMarkerValue {
				t.Fatalf("一次性标记值错误: %q", entry)
			}
		}
	}
	if markerCount != 1 {
		t.Fatalf("一次性标记数量=%d，环境=%v", markerCount, environment)
	}
}

func TestShouldRestartDesktopForGPUPreferenceOnlyOnce(t *testing.T) {
	tests := []struct {
		name             string
		changed          bool
		restartAttempted bool
		want             bool
	}{
		{name: "没有变化", changed: false, want: false},
		{name: "首次迁移", changed: true, want: true},
		{name: "已经自重启", changed: true, restartAttempted: true, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldRestartDesktopForGPUPreference(test.changed, test.restartAttempted); got != test.want {
				t.Fatalf("shouldRestart=%v，want %v", got, test.want)
			}
		})
	}
}
