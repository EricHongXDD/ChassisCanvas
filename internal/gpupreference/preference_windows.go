//go:build windows

package gpupreference

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const userGPUPreferencesKey = `Software\Microsoft\DirectX\UserGpuPreferences`

type registryValue struct {
	exists bool
	raw    string
}

func platformStatus(targets []ApplicationStatus) (Status, error) {
	applications := cloneTargets(targets)
	key, err := registry.OpenKey(registry.CURRENT_USER, userGPUPreferencesKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		for index := range applications {
			applications[index].Preference = SystemDefault
		}
		return enrichAdapterStatus(statusFromApplications(true, applications)), nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("读取 Windows GPU 首选项: %w", err)
	}
	defer key.Close()
	for index := range applications {
		value, readErr := readRegistryValue(key, applications[index].Path)
		if readErr != nil {
			return Status{}, fmt.Errorf("读取 %s 的 GPU 首选项: %w", applications[index].Name, readErr)
		}
		applications[index].Preference = preferenceFromRaw(value.raw, value.exists)
	}
	return enrichAdapterStatus(statusFromApplications(true, applications)), nil
}

func platformApply(targets []ApplicationStatus, preference Preference) (ApplyResult, error) {
	applications := cloneTargets(targets)
	key, _, err := registry.CreateKey(registry.CURRENT_USER, userGPUPreferencesKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("打开 Windows GPU 首选项: %w", err)
	}
	defer key.Close()

	before := make([]registryValue, len(applications))
	desired := make([]registryValue, len(applications))
	changed := false
	for index, application := range applications {
		before[index], err = readRegistryValue(key, application.Path)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("读取 %s 的 GPU 首选项: %w", application.Name, err)
		}
		desired[index] = registryValueForPreference(before[index], preference)
		if before[index] != desired[index] {
			changed = true
		}
	}

	for index, application := range applications {
		if before[index] == desired[index] {
			continue
		}
		if err = writeRegistryValue(key, application.Path, desired[index]); err != nil {
			rollbackFailures := make([]string, 0)
			for rollbackIndex := index - 1; rollbackIndex >= 0; rollbackIndex-- {
				if before[rollbackIndex] == desired[rollbackIndex] {
					continue
				}
				if rollbackErr := writeRegistryValue(key, applications[rollbackIndex].Path, before[rollbackIndex]); rollbackErr != nil {
					rollbackFailures = append(rollbackFailures, applications[rollbackIndex].Name+": "+rollbackErr.Error())
				}
			}
			message := fmt.Sprintf("写入 %s 的 GPU 首选项: %v", application.Name, err)
			if len(rollbackFailures) > 0 {
				message += "；回滚失败: " + strings.Join(rollbackFailures, "；")
			}
			return ApplyResult{}, errors.New(message)
		}
	}

	for index := range applications {
		applications[index].Preference = preference
	}
	return ApplyResult{Status: enrichAdapterStatus(statusFromApplications(true, applications)), Changed: changed}, nil
}

func cloneTargets(targets []ApplicationStatus) []ApplicationStatus {
	return append([]ApplicationStatus(nil), targets...)
}

func readRegistryValue(key registry.Key, name string) (registryValue, error) {
	value, _, err := key.GetStringValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return registryValue{}, nil
	}
	if err != nil {
		return registryValue{}, err
	}
	return registryValue{exists: true, raw: value}, nil
}

func writeRegistryValue(key registry.Key, name string, value registryValue) error {
	if !value.exists {
		err := key.DeleteValue(name)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	return key.SetStringValue(name, value.raw)
}

func preferenceFromRaw(raw string, exists bool) Preference {
	if !exists {
		return SystemDefault
	}
	found := false
	result := SystemDefault
	for _, token := range strings.Split(raw, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(token), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "GpuPreference") {
			continue
		}
		number, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return Unknown
		}
		current := Unknown
		switch number {
		case 0:
			current = SystemDefault
		case 1:
			current = PowerSaving
		case 2:
			current = HighPerformance
		}
		if current == Unknown || (found && current != result) {
			return Unknown
		}
		found = true
		result = current
	}
	if !found {
		return SystemDefault
	}
	return result
}

func registryValueForPreference(existing registryValue, preference Preference) registryValue {
	kept := make([]string, 0)
	if existing.exists {
		for _, token := range strings.Split(existing.raw, ";") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			key, _, ok := strings.Cut(token, "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "GpuPreference") {
				continue
			}
			kept = append(kept, token)
		}
	}
	switch preference {
	case PowerSaving:
		kept = append(kept, "GpuPreference=1")
	case HighPerformance:
		kept = append(kept, "GpuPreference=2")
	}
	if len(kept) == 0 {
		return registryValue{}
	}
	return registryValue{exists: true, raw: strings.Join(kept, ";") + ";"}
}
