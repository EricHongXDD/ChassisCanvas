//go:build windows

package telemetry

import (
	"context"
	"encoding/csv"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	nvidiaRefreshInterval = 15 * time.Second
	nvidiaRetryInterval   = 60 * time.Second
	nvidiaCommandTimeout  = 1500 * time.Millisecond
)

type nvidiaInfo struct {
	model       string
	temperature *float64
}

type nvidiaCommandRunner func(context.Context, string, ...string) ([]byte, error)

// nvidiaSMISampler 只在 NVIDIA 驱动提供 nvidia-smi 时启用。
// 查询在后台低频执行；Capture 始终只读取缓存，不等待外部进程。
type nvidiaSMISampler struct {
	mu              sync.Mutex
	executable      string
	run             nvidiaCommandRunner
	refreshInterval time.Duration
	retryInterval   time.Duration
	timeout         time.Duration
	lastAttempt     time.Time
	refreshing      bool
	info            nvidiaInfo
}

func newNvidiaSMISampler(displayModel string) *nvidiaSMISampler {
	if displayModel != "" && !looksLikeNVIDIA(displayModel) {
		return nil
	}
	executable := findNvidiaSMI()
	if executable == "" {
		return nil
	}
	return &nvidiaSMISampler{
		executable:      executable,
		run:             runNvidiaSMI,
		refreshInterval: nvidiaRefreshInterval,
		retryInterval:   nvidiaRetryInterval,
		timeout:         nvidiaCommandTimeout,
		info:            nvidiaInfo{model: displayModel},
	}
}

func looksLikeNVIDIA(model string) bool {
	lower := strings.ToLower(model)
	return strings.Contains(lower, "nvidia") || strings.Contains(lower, "geforce") || strings.Contains(lower, "quadro")
}

func findNvidiaSMI() string {
	if path, err := exec.LookPath("nvidia-smi.exe"); err == nil {
		return path
	}
	if path, err := exec.LookPath("nvidia-smi"); err == nil {
		return path
	}
	if programFiles := os.Getenv("ProgramFiles"); programFiles != "" {
		path := filepath.Join(programFiles, "NVIDIA Corporation", "NVSMI", "nvidia-smi.exe")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func runNvidiaSMI(ctx context.Context, executable string, arguments ...string) ([]byte, error) {
	return exec.CommandContext(ctx, executable, arguments...).Output()
}

func (sampler *nvidiaSMISampler) cachedAndRefresh() nvidiaInfo {
	sampler.mu.Lock()
	defer sampler.mu.Unlock()

	now := time.Now()
	interval := sampler.refreshInterval
	if sampler.info.temperature == nil {
		interval = sampler.retryInterval
	}
	if !sampler.refreshing && (sampler.lastAttempt.IsZero() || now.Sub(sampler.lastAttempt) >= interval) {
		sampler.refreshing = true
		sampler.lastAttempt = now
		go sampler.refresh()
	}
	return cloneNvidiaInfo(sampler.info)
}

func (sampler *nvidiaSMISampler) refresh() {
	timeout := sampler.timeout
	if timeout <= 0 {
		timeout = nvidiaCommandTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	output, err := sampler.run(
		ctx,
		sampler.executable,
		"--query-gpu=name,temperature.gpu",
		"--format=csv,noheader,nounits",
	)
	info, parseErr := parseNvidiaSMIOutput(output)

	sampler.mu.Lock()
	defer sampler.mu.Unlock()
	sampler.refreshing = false
	if err != nil || parseErr != nil {
		return
	}
	if info.model == "" {
		info.model = sampler.info.model
	}
	sampler.info = info
}

func cloneNvidiaInfo(info nvidiaInfo) nvidiaInfo {
	clone := nvidiaInfo{model: info.model}
	if info.temperature != nil {
		value := *info.temperature
		clone.temperature = &value
	}
	return clone
}

func parseNvidiaSMIOutput(output []byte) (nvidiaInfo, error) {
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(output), "\ufeff")))
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		return nvidiaInfo{}, err
	}
	var result nvidiaInfo
	for _, record := range records {
		if len(record) < 2 {
			continue
		}
		model := normalizeHardwareName(record[0])
		temperatureText := strings.TrimSpace(record[1])
		temperature, parseErr := strconv.ParseFloat(temperatureText, 64)
		if model == "" || parseErr != nil || temperature < -50 || temperature > 200 {
			continue
		}
		if result.model == "" {
			result.model = model
		}
		if result.temperature == nil || temperature > *result.temperature {
			value := temperature
			result.temperature = &value
		}
	}
	if result.model == "" || result.temperature == nil {
		return nvidiaInfo{}, errors.New("nvidia-smi 未返回有效的显卡温度")
	}
	return result, nil
}
