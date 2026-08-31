package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"casescreen/internal/config"
	"casescreen/internal/device"
	devicecoolermaster "casescreen/internal/device/coolermaster"
	devicewaveshare "casescreen/internal/device/waveshare"
	"casescreen/internal/discovery"
	"casescreen/internal/serialport"
	"casescreen/internal/service"
	"casescreen/internal/usb/hidapi"
)

const (
	runtimeDiscoveryInterval = 2 * time.Second
	runtimeProbeTimeout      = 5 * time.Second
	defaultCoolerMasterSlot  = 4
)

// mutableSerialEndpoint 把逻辑设备 ID 与当前 COM 号分开。Windows 重新枚举
// CDC 设备后只替换端点，Service 中的驱动、主题和用户选择仍使用原 ID。
type mutableSerialEndpoint struct {
	mu       sync.RWMutex
	port     string
	openPort func(string) (io.ReadWriteCloser, error)
}

func newMutableSerialEndpoint(port string, openPort func(string) (io.ReadWriteCloser, error)) *mutableSerialEndpoint {
	return &mutableSerialEndpoint{port: normalizeCOMPort(port), openPort: openPort}
}

func (endpoint *mutableSerialEndpoint) Port() string {
	endpoint.mu.RLock()
	defer endpoint.mu.RUnlock()
	return endpoint.port
}

func (endpoint *mutableSerialEndpoint) SetPort(port string) {
	endpoint.mu.Lock()
	endpoint.port = normalizeCOMPort(port)
	endpoint.mu.Unlock()
}

func (endpoint *mutableSerialEndpoint) Open() (io.ReadWriteCloser, error) {
	endpoint.mu.RLock()
	port := endpoint.port
	openPort := endpoint.openPort
	endpoint.mu.RUnlock()
	if port == "" {
		return nil, fmt.Errorf("微雪串口未配置")
	}
	if openPort == nil {
		return nil, fmt.Errorf("微雪串口打开器未配置")
	}
	return openPort(port)
}

type runtimeDeviceEntry struct {
	configured    config.Device
	waveshare     *mutableSerialEndpoint
	cooler        *devicecoolermaster.Driver
	presenceKnown bool
	present       bool
	requiresProbe bool
}

type waveshareProbeResult struct {
	candidate discovery.Device
	info      device.Info
	err       error
}

type runtimeDeviceManager struct {
	controller     *service.Service
	store          *config.Store
	stateDirectory string
	discover       func() []discovery.Device
	status         func() service.Status
	openSerial     func(string) (io.ReadWriteCloser, error)
	probeWaveshare func(context.Context, string) (device.Info, error)
	openCooler     func(string) (devicecoolermaster.HIDTransport, error)
	coolerConflict func() error
	interval       time.Duration

	mu      sync.Mutex
	entries map[string]*runtimeDeviceEntry
	// 启动预检发现禁用/启用无序列号同型歧义时，仍注册逻辑设备供 UI
	// 显示，但先清空 opener，直到运行期探针取得唯一证据。
	startupBlockedWaveshare map[string]bool
}

func newRuntimeDeviceManager(controller *service.Service, store *config.Store, stateDirectory string) *runtimeDeviceManager {
	manager := &runtimeDeviceManager{
		controller: controller, store: store, stateDirectory: stateDirectory,
		discover: discovery.Find,
		status:   controller.Status,
		openSerial: func(port string) (io.ReadWriteCloser, error) {
			return serialport.Open(port, serialport.DefaultOptions())
		},
		openCooler: func(libraryPath string) (devicecoolermaster.HIDTransport, error) {
			return hidapi.Open(libraryPath, 0x2516, 0x0222, 0)
		},
		coolerConflict:          devicecoolermaster.CheckOEMControlConflict,
		interval:                runtimeDiscoveryInterval,
		entries:                 make(map[string]*runtimeDeviceEntry),
		startupBlockedWaveshare: make(map[string]bool),
	}
	manager.probeWaveshare = manager.probeWavesharePort
	return manager
}

// RegisterConfigured 在 Service.Run 前注册已保存的逻辑设备。
// 不在这里预先占用 USB；首次打开与后续重连使用同一条有界路径。
func (manager *runtimeDeviceManager) RegisterConfigured(cfg config.Config) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, configured := range orderedConfiguredDevices(cfg.Devices) {
		if !configured.Enabled {
			continue
		}
		if manager.registerDeviceLocked(configured, nil) && configured.Kind == "waveshare" &&
			manager.startupBlockedWaveshare[configured.ID] {
			entry := manager.entries[configured.ID]
			entry.waveshare.SetPort("")
			entry.requiresProbe = true
			entry.presenceKnown = true
			entry.present = false
		}
	}
}

// ReconcileConfiguredBeforeRegister 在任何驱动占用串口前完成一次物理身份对账。
// 这一步必须先握手取得实机分辨率，才能修复旧版把 7 英寸
// 与 8.8 英寸的 COM 端点写反、并额外产生第三个逻辑设备的配置。
func (manager *runtimeDeviceManager) ReconcileConfiguredBeforeRegister(ctx context.Context, cfg config.Config) (config.Config, error) {
	if manager == nil || manager.discover == nil {
		return cfg, nil
	}
	discovered := manager.discover()
	type probeResult struct {
		index int
		info  device.Info
	}
	results := make(chan probeResult, len(discovered))
	var wait sync.WaitGroup
	for index := range discovered {
		if discovered[index].Kind != "waveshare" || normalizeCOMPort(discovered[index].Port) == "" {
			continue
		}
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			probeContext, cancel := context.WithTimeout(ctx, runtimeProbeTimeout)
			defer cancel()
			info, _ := manager.probeWaveshare(probeContext, discovered[index].Port)
			// FindDevice 已能验证实机分辨率；后续读主题/容量的
			// 可选 ACK 超时不应丢弃这份身份证据。
			if validResolution(info.Resolution) {
				results <- probeResult{index: index, info: info}
			}
		}(index)
	}
	wait.Wait()
	close(results)
	for result := range results {
		discovered[result.index].Width = result.info.Resolution[0]
		discovered[result.index].Height = result.info.Resolution[1]
		if strings.TrimSpace(result.info.Name) != "" {
			discovered[result.index].Name = strings.TrimSpace(result.info.Name)
		}
	}
	manager.startupBlockedWaveshare = startupAmbiguousDisabledWaveshareOwners(cfg, discovered)
	merged, changed := mergeDiscoveredWaveshareDevices(cfg, discovered)
	if !changed || manager.store == nil {
		return merged, nil
	}
	if err := manager.store.Set(merged); err != nil {
		return cfg, fmt.Errorf("保存微雪 USB 身份对账结果: %w", err)
	}
	return manager.store.Get(), nil
}

func startupAmbiguousDisabledWaveshareOwners(cfg config.Config, discovered []discovery.Device) map[string]bool {
	blocked := make(map[string]bool)
	byPort := make(map[string]discovery.Device)
	for _, candidate := range discovered {
		if candidate.Kind == "waveshare" && normalizeCOMPort(candidate.Port) != "" {
			byPort[normalizeCOMPort(candidate.Port)] = candidate
		}
	}
	for disabledIndex, disabled := range cfg.Devices {
		if disabled.Kind != "waveshare" || disabled.Enabled || normalizeWaveshareStableID(disabled.StableID) != "" {
			continue
		}
		if _, stillPresent := byPort[normalizeCOMPort(disabled.Port)]; stillPresent {
			continue
		}
		disabledExpected := configuredWaveshareExpectedResolution(cfg, disabledIndex)
		for enabledIndex, enabled := range cfg.Devices {
			if enabled.Kind != "waveshare" || !enabled.Enabled || normalizeWaveshareStableID(enabled.StableID) != "" {
				continue
			}
			candidate, present := byPort[normalizeCOMPort(enabled.Port)]
			if !present {
				continue
			}
			enabledExpected := configuredWaveshareExpectedResolution(cfg, enabledIndex)
			actual := [2]int{candidate.Width, candidate.Height}
			if validResolution(actual) {
				if sameResolution(disabledExpected, actual) && sameResolution(enabledExpected, actual) {
					blocked[enabled.ID] = true
				}
			} else if sameResolution(disabledExpected, enabledExpected) {
				blocked[enabled.ID] = true
			}
		}
	}
	return blocked
}

func (manager *runtimeDeviceManager) registerDeviceLocked(configured config.Device, initialCooler devicecoolermaster.HIDTransport) bool {
	configured.ID = strings.TrimSpace(configured.ID)
	if configured.ID == "" || manager.controller == nil {
		if initialCooler != nil {
			_ = initialCooler.Close()
		}
		return false
	}
	if _, exists := manager.entries[configured.ID]; exists {
		if initialCooler != nil {
			_ = initialCooler.Close()
		}
		return false
	}
	name := strings.TrimSpace(configured.Name)
	if name == "" {
		name = configured.ID
	}
	entry := &runtimeDeviceEntry{configured: configured}
	var driver device.Driver
	switch configured.Kind {
	case "waveshare":
		endpoint := newMutableSerialEndpoint(configured.Port, manager.openSerial)
		waveshare := devicewaveshare.New(
			configured.ID, name, [2]int{configured.Width, configured.Height}, nil, nil,
		)
		waveshare.Opener = endpoint.Open
		entry.waveshare = endpoint
		driver = waveshare
	case "coolermaster":
		libraryPath := manager.coolerLibraryPath(configured.LibraryPath)
		opener := func() (devicecoolermaster.HIDTransport, error) {
			return manager.openCoolerTransport(libraryPath)
		}
		digest := sha256.Sum256([]byte(configured.ID))
		statePath := filepath.Join(manager.stateDirectory, fmt.Sprintf("coolermaster-%x.json", digest[:8]))
		cooler := devicecoolermaster.New(
			configured.ID, name, [2]int{configured.Width, configured.Height},
			configured.Slot, initialCooler, statePath,
		)
		if err := cooler.SetDisplayRotation(configured.DisplayRotation); err != nil {
			if initialCooler != nil {
				_ = initialCooler.Close()
			}
			return false
		}
		if manager.store != nil {
			cooler.SetFFmpegPath(manager.store.Get().FFmpegPath)
		} else {
			// 测试或无持久化配置启动时，仍允许驱动从发行目录自动查找 FFmpeg。
			cooler.SetFFmpegPath("")
		}
		cooler.Opener = opener
		entry.cooler = cooler
		driver = cooler
	default:
		if initialCooler != nil {
			_ = initialCooler.Close()
		}
		return false
	}
	if !manager.controller.AddDriverWithInitialPower(driver, configured.PoweredOff) {
		_ = driver.Close()
		return false
	}
	manager.entries[configured.ID] = entry
	return true
}

func (manager *runtimeDeviceManager) openCoolerTransport(libraryPath string) (devicecoolermaster.HIDTransport, error) {
	if manager.coolerConflict != nil {
		if err := manager.coolerConflict(); err != nil {
			return nil, err
		}
	}
	return manager.openCooler(libraryPath)
}

func (manager *runtimeDeviceManager) coolerLibraryPath(configuredPath string) string {
	executablePath := ""
	if executable, err := os.Executable(); err == nil {
		executablePath = executable
	}
	return resolveCoolerMasterLibraryPath(strings.TrimSpace(configuredPath), executablePath)
}

func (manager *runtimeDeviceManager) Run(ctx context.Context) {
	if manager == nil || manager.controller == nil || manager.discover == nil {
		return
	}
	interval := manager.interval
	if interval <= 0 {
		interval = runtimeDiscoveryInterval
	}
	// 启动扫描已在进程建立前完成；第一个节拍后再复核，
	// 避免与 connectAll 同时打开刚枚举的设备。
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := manager.Scan(ctx); err != nil && ctx.Err() == nil {
				log.Printf("运行期设备扫描失败: %v", err)
			}
		}
	}
}

// Scan 对当前在线端点做一次幂等对账。
func (manager *runtimeDeviceManager) Scan(ctx context.Context) error {
	if manager == nil || manager.discover == nil {
		return nil
	}
	discovered := manager.discover()
	if err := ctx.Err(); err != nil {
		return err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.reconcileLocked(ctx, discovered)
}

func (manager *runtimeDeviceManager) reconcileLocked(ctx context.Context, discovered []discovery.Device) error {
	presentWaveshare := make(map[string]discovery.Device)
	var discoveredCooler *discovery.Device
	for index := range discovered {
		candidate := discovered[index]
		switch candidate.Kind {
		case "waveshare":
			port := normalizeCOMPort(candidate.Port)
			if port != "" {
				candidate.Port = port
				candidate.StableID = normalizeWaveshareStableID(candidate.StableID)
				presentWaveshare[port] = candidate
			}
		case "coolermaster":
			if discoveredCooler == nil {
				copied := candidate
				discoveredCooler = &copied
			}
		}
	}

	configured := manager.configuredSnapshotLocked()
	if err := manager.handleCoolerOEMConflictLocked(ctx, configured); err != nil {
		return err
	}
	knownIDs := make(map[string]bool, len(configured.Devices)+len(manager.entries))
	configuredByID := make(map[string]config.Device, len(configured.Devices))
	reservedDisabledPorts := make(map[string]bool)
	disabledByStableID := make(map[string]config.Device)
	disabledDevices := make([]config.Device, 0)
	disabledWithoutStableID := make([]config.Device, 0)
	disabledStableCounts := make(map[string]int)
	configuredStableCounts := make(map[string]int)
	candidateStableCounts := make(map[string]int)
	for _, candidate := range presentWaveshare {
		if candidate.StableID != "" {
			candidateStableCounts[candidate.StableID]++
		}
	}
	hasConfiguredCooler := false
	for _, current := range configured.Devices {
		knownIDs[current.ID] = true
		configuredByID[current.ID] = current
		if current.Kind == "waveshare" {
			if stableID := normalizeWaveshareStableID(current.StableID); stableID != "" {
				configuredStableCounts[stableID]++
			}
		}
		if current.Kind == "waveshare" && !current.Enabled {
			disabledDevices = append(disabledDevices, current)
			if stableID := normalizeWaveshareStableID(current.StableID); stableID != "" {
				disabledStableCounts[stableID]++
				disabledByStableID[stableID] = current
			} else {
				disabledWithoutStableID = append(disabledWithoutStableID, current)
			}
		}
		if current.Kind == "coolermaster" {
			hasConfiguredCooler = true
		}
	}
	for id := range manager.entries {
		knownIDs[id] = true
	}
	enabledWaveshareCount := 0
	for _, entry := range manager.entries {
		if entry != nil && entry.waveshare != nil {
			enabledWaveshareCount++
		}
	}

	updates := make(map[string]config.Device)
	activeStrongPortOwner := make(map[string]string)
	for id, entry := range manager.entries {
		if entry == nil || entry.waveshare == nil {
			continue
		}
		stableID := normalizeWaveshareStableID(entry.configured.StableID)
		if stableID == "" || configuredStableCounts[stableID] != 1 || candidateStableCounts[stableID] != 1 {
			continue
		}
		for port, candidate := range presentWaveshare {
			if candidate.StableID == stableID {
				activeStrongPortOwner[port] = id
				break
			}
		}
	}
	disabledStrongMatched := make(map[string]bool)
	forcedOfflineIDs := make(map[string]bool)
	wrongStrongClaimPorts := make(map[string]string)
	for id, entry := range manager.entries {
		if entry == nil || entry.waveshare == nil {
			continue
		}
		port := entry.waveshare.Port()
		candidate, present := presentWaveshare[port]
		if !present || candidate.StableID == "" {
			continue
		}
		if configuredStableID := normalizeWaveshareStableID(entry.configured.StableID); configuredStableID != candidate.StableID {
			wrongStrongClaimPorts[id] = port
		}
	}
	// 某台无序列号禁用屏的旧端口已消失时，当前任意弱 COM claimant
	// 都必须先让出端口参与全局探针；否则禁用屏换口后会被新增为 enabled。
	missingDisabledWithoutStableID := false
	for _, disabled := range disabledWithoutStableID {
		if _, present := presentWaveshare[normalizeCOMPort(disabled.Port)]; !present {
			missingDisabledWithoutStableID = true
			break
		}
	}
	if missingDisabledWithoutStableID {
		for id, entry := range manager.entries {
			if entry == nil || entry.waveshare == nil || normalizeWaveshareStableID(entry.configured.StableID) != "" {
				continue
			}
			if entry.presenceKnown && entry.present && !entry.requiresProbe {
				continue
			}
			port := entry.waveshare.Port()
			if _, present := presentWaveshare[port]; present {
				wrongStrongClaimPorts[id] = port
			}
		}
	}
	// 禁用设备不注册驱动，但其唯一序列号仍保留当前物理端点。
	// 若它换口到一个 enabled 条目的旧 COM，与禁用条目的已离线
	// 旧端点原子交换，避免运行期生成 enabled 重复项或打开错屏。
	for port, candidate := range presentWaveshare {
		stableID := candidate.StableID
		disabled, exists := disabledByStableID[stableID]
		if stableID == "" || !exists || disabledStableCounts[stableID] != 1 ||
			configuredStableCounts[stableID] != 1 || candidateStableCounts[stableID] != 1 {
			continue
		}
		disabledStrongMatched[disabled.ID] = true
		reservedDisabledPorts[port] = true
		if normalizeCOMPort(disabled.Port) == port {
			continue
		}
		oldPort := normalizeCOMPort(disabled.Port)
		if oldPort == "" {
			continue
		}
		displacedID := ""
		for id, entry := range manager.entries {
			if entry != nil && entry.waveshare != nil && entry.waveshare.Port() == port {
				displacedID = id
				break
			}
		}
		nextDisabled := disabled
		nextDisabled.Port = port
		nextDisabled.StableID = stableID
		updates[nextDisabled.ID] = nextDisabled
		if displacedID != "" {
			displaced := manager.entries[displacedID]
			nextDisplaced := displaced.configured
			nextDisplaced.Port = oldPort
			updates[displacedID] = nextDisplaced
			forcedOfflineIDs[displacedID] = true
		}
	}
	// 其余禁用设备只保留旧端点。若该端点已经由 enabled 强序列号
	// 所有者证明占用，则把禁用条目换到强所有者的旧端点；禁用状态不变。
	for _, disabled := range disabledDevices {
		if disabledStrongMatched[disabled.ID] {
			continue
		}
		port := normalizeCOMPort(disabled.Port)
		if port == "" {
			continue
		}
		if ownerID := activeStrongPortOwner[port]; ownerID != "" {
			owner := manager.entries[ownerID]
			oldOwnerPort := ""
			if owner != nil && owner.waveshare != nil {
				oldOwnerPort = owner.waveshare.Port()
			}
			if oldOwnerPort != "" && oldOwnerPort != port {
				nextDisabled := disabled
				nextDisabled.Port = oldOwnerPort
				updates[disabled.ID] = nextDisabled
				reservedDisabledPorts[oldOwnerPort] = true
			}
			continue
		}
		reservedDisabledPorts[port] = true
	}
	if manager.status != nil {
		// Driver.Info 会在微雪握手后更新为实机尺寸。必须先把这份
		// 已验证身份写回 entry，再按新 COM 做匹配；否则旧版默认的
		// 1024x600 会把真实 480x1920 屏误判成另一台设备。
		for _, live := range manager.status().Devices {
			entry := manager.entries[live.Info.ID]
			if entry == nil || entry.waveshare == nil || !validResolution(live.Info.Resolution) ||
				(!live.Connected && live.LastConnectedAt.IsZero() && live.LastVerifiedAt.IsZero()) {
				continue
			}
			if _, wrongStrongClaim := wrongStrongClaimPorts[live.Info.ID]; wrongStrongClaim {
				// 当前端点已由发现层证明属于另一强身份；它报告的型号来自
				// 错误会话，绝不能反向覆盖该逻辑设备的历史尺寸。
				continue
			}
			expected := [2]int{entry.configured.Width, entry.configured.Height}
			if enabledWaveshareCount > 1 && validResolution(expected) && !sameResolution(expected, live.Info.Resolution) {
				// 同一组 COM 端口上的两块无序列号屏也可能互换。
				// 已连接会话型号与逻辑主题尺寸不符时，先关闭并全局探针，
				// 不能把错误会话的尺寸写回后永久交换两个主题。
				wrongStrongClaimPorts[live.Info.ID] = entry.waveshare.Port()
				continue
			}
			next := entry.configured
			next.Width, next.Height = live.Info.Resolution[0], live.Info.Resolution[1]
			if isDefaultConfiguredWaveshareName(next.Name, next.ID) && strings.TrimSpace(live.Info.Name) != "" {
				next.Name = strings.TrimSpace(live.Info.Name)
			}
			entry.configured = next
			stored, exists := configuredByID[next.ID]
			if !exists || stored.Width != next.Width || stored.Height != next.Height || stored.Name != next.Name {
				updates[next.ID] = next
			}
		}
	}

	claimedPorts := make(map[string]bool)
	identityClaimedIDs := make(map[string]bool)
	missing := make([]*runtimeDeviceEntry, 0)
	reappeared := make([]*runtimeDeviceEntry, 0)
	// 真实 USB 序列号先于 COM 号。同一块屏换口后，直接把
	// 新端点绑回原逻辑 ID，主题、轮播和关屏意图都不改键。
	for id, entry := range manager.entries {
		if entry.waveshare == nil {
			continue
		}
		stableID := normalizeWaveshareStableID(entry.configured.StableID)
		if stableID == "" || configuredStableCounts[stableID] != 1 || candidateStableCounts[stableID] != 1 {
			continue
		}
		matchedPort := ""
		for port, candidate := range presentWaveshare {
			if candidate.StableID == stableID {
				if matchedPort != "" {
					matchedPort = ""
					break
				}
				matchedPort = port
			}
		}
		if matchedPort == "" {
			continue
		}
		next, alreadyUpdated := updates[id]
		if !alreadyUpdated {
			next = entry.configured
		}
		next.Port = matchedPort
		next.StableID = stableID
		if alreadyUpdated || entry.configured.Port != next.Port || entry.configured.StableID != next.StableID {
			updates[id] = next
		}
		identityClaimedIDs[id] = true
		claimedPorts[matchedPort] = true
		wasKnown, wasPresent := entry.presenceKnown, entry.present
		entry.presenceKnown = true
		entry.present = true
		entry.requiresProbe = false
		if wasKnown && !wasPresent && entry.waveshare.Port() == matchedPort {
			reappeared = append(reappeared, entry)
		}
	}
	for _, entry := range manager.entries {
		if entry.waveshare == nil {
			continue
		}
		if identityClaimedIDs[entry.configured.ID] {
			continue
		}
		port := entry.waveshare.Port()
		candidate, present := presentWaveshare[port]
		configuredStableID := normalizeWaveshareStableID(entry.configured.StableID)
		if present && claimedPorts[port] {
			// 该端点已被另一个序列号逻辑设备证明。
			// 无序列号条目不得再按旧 COM 号二次认领。
			present = false
		}
		if present && reservedDisabledPorts[port] {
			present = false
		}
		if present && configuredStableID != "" && candidate.StableID != configuredStableID {
			// 原 COM 号已被另一块物理屏占用，不得按端口误绑。
			present = false
		}
		if present && configuredStableID == "" && candidate.StableID != "" {
			// 新出现的强序列号不能被“恰好占旧 COM”的无身份条目吞掉；
			// 必须握手并参与全局分辨率匹配，才能写回正确逻辑 ID。
			present = false
		}
		if present && configuredStableID == "" && enabledWaveshareCount > len(presentWaveshare) {
			// 多个无身份逻辑设备只回来一部分时，旧 COM 不能证明是谁。
			// 先关闭旧 claimant，再由双向唯一分辨率完成全局匹配。
			present = false
			entry.requiresProbe = true
			wrongStrongClaimPorts[entry.configured.ID] = port
		}
		if present && entry.requiresProbe {
			present = false
		}
		if _, wrongEndpoint := wrongStrongClaimPorts[entry.configured.ID]; wrongEndpoint {
			present = false
		}
		if entry.presenceKnown && !entry.present && present {
			reappeared = append(reappeared, entry)
		}
		entry.presenceKnown = true
		entry.present = present
		if present {
			claimedPorts[port] = true
			continue
		}
		if configuredStableID == "" && (enabledWaveshareCount > 1 || len(disabledWithoutStableID) > 0) {
			entry.requiresProbe = true
		}
		missing = append(missing, entry)
	}
	for _, disabled := range disabledWithoutStableID {
		port := normalizeCOMPort(disabled.Port)
		if _, stillAtSavedPort := presentWaveshare[port]; stillAtSavedPort && reservedDisabledPorts[port] {
			continue
		}
		// 只作为身份匹配记录参与探针，不注册运行期驱动。
		missing = append(missing, &runtimeDeviceEntry{configured: disabled})
	}
	unclaimedPorts := make([]string, 0)
	for port := range presentWaveshare {
		if !claimedPorts[port] && !reservedDisabledPorts[port] {
			unclaimedPorts = append(unclaimedPorts, port)
		}
	}
	sort.Strings(unclaimedPorts)
	sort.SliceStable(missing, func(left, right int) bool {
		return configuredDeviceRank(missing[left].configured) < configuredDeviceRank(missing[right].configured)
	})

	// 在打开探针前同步关闭占错端点的旧会话，并临时清空 opener。
	// 即使 Service 的重连节拍同时触发，也无法在探针前重新抢占该 COM。
	for id, oldPort := range wrongStrongClaimPorts {
		entry := manager.entries[id]
		if entry == nil || entry.waveshare == nil {
			continue
		}
		entry.waveshare.SetPort("")
		entry.requiresProbe = true
		entry.presenceKnown = true
		entry.present = false
		_ = manager.controller.ResetDeviceConnection(
			ctx, id, fmt.Sprintf("%s 已由另一 USB 序列号占用，正在重新核对物理屏", oldPort),
		)
	}

	probes := make(map[string]waveshareProbeResult, len(unclaimedPorts))
	for _, port := range unclaimedPorts {
		probeContext, cancel := context.WithTimeout(ctx, runtimeProbeTimeout)
		info, err := manager.probeWaveshare(probeContext, port)
		cancel()
		probes[port] = waveshareProbeResult{candidate: presentWaveshare[port], info: info, err: err}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	allowMissingStableIdentity := make(map[string]bool)
	for _, entry := range missing {
		if entry == nil {
			continue
		}
		stableID := normalizeWaveshareStableID(entry.configured.StableID)
		if stableID != "" && configuredStableCounts[stableID] == 1 && candidateStableCounts[stableID] == 0 {
			allowMissingStableIdentity[entry.configured.ID] = true
		}
	}
	assignments := matchWaveshareEndpoints(missing, unclaimedPorts, probes, allowMissingStableIdentity)
	assignedPorts := make(map[string]bool, len(assignments))
	assignedIDs := make(map[string]bool, len(assignments))
	strongAssignedIDs := make(map[string]bool)
	resolutionAssignedIDs := make(map[string]bool)
	for id, port := range assignments {
		entry := manager.entries[id]
		var next config.Device
		if entry != nil && entry.waveshare != nil {
			next = entry.configured
		} else {
			configuredEntry, exists := configuredByID[id]
			if !exists || configuredEntry.Kind != "waveshare" || configuredEntry.Enabled {
				continue
			}
			next = configuredEntry
		}
		expected := [2]int{next.Width, next.Height}
		next.Port = port
		if allowMissingStableIdentity[id] && normalizeWaveshareStableID(next.StableID) != "" &&
			normalizeWaveshareStableID(probes[port].candidate.StableID) == "" {
			// 屏端固件已停止报告原 USB 序列号，且本轮已用
			// 双向唯一的握手分辨率确认它仍是同一逻辑屏。
			next.StableID = ""
		}
		if next.StableID == "" && probes[port].candidate.StableID != "" {
			next.StableID = probes[port].candidate.StableID
		}
		if info := probes[port].info; validResolution(info.Resolution) {
			next.Width, next.Height = info.Resolution[0], info.Resolution[1]
			if isDefaultConfiguredWaveshareName(next.Name, next.ID) && strings.TrimSpace(info.Name) != "" {
				next.Name = info.Name
			}
		}
		updates[id] = next
		assignedPorts[port] = true
		assignedIDs[id] = true
		if stableID := probes[port].candidate.StableID; stableID != "" && candidateStableCounts[stableID] == 1 {
			strongAssignedIDs[id] = true
		}
		if validResolution(expected) && validResolution(probes[port].info.Resolution) &&
			sameResolution(expected, probes[port].info.Resolution) {
			resolutionAssignedIDs[id] = true
		}
	}
	// 禁用无序列号屏按唯一分辨率换口后，也要把占用其新端口的
	// enabled 旧 claimant 原子移到禁用屏已消失的旧端点并保持离线。
	for disabledID, port := range assignments {
		if manager.entries[disabledID] != nil {
			continue
		}
		disabled, exists := configuredByID[disabledID]
		if !exists || disabled.Enabled || disabled.Kind != "waveshare" {
			continue
		}
		oldPort := normalizeCOMPort(disabled.Port)
		newPort := normalizeCOMPort(port)
		if oldPort == "" || oldPort == newPort {
			continue
		}
		for displacedID, displaced := range manager.entries {
			if displaced == nil || displaced.waveshare == nil {
				continue
			}
			displacedPort := displaced.waveshare.Port()
			if blockedPort := wrongStrongClaimPorts[displacedID]; blockedPort != "" {
				displacedPort = blockedPort
			} else if displacedPort == "" && displaced.requiresProbe {
				displacedPort = normalizeCOMPort(displaced.configured.Port)
			}
			if displacedPort != newPort || assignedIDs[displacedID] {
				continue
			}
			nextDisplaced := displaced.configured
			nextDisplaced.Port = oldPort
			updates[displacedID] = nextDisplaced
			forcedOfflineIDs[displacedID] = true
			break
		}
	}
	// 强序列号所有者移到另一逻辑设备的旧 COM，且它自己的
	// 旧 COM 本轮确认不在线时，必须把被挤占条目一起换到该离线端点。
	// 两条更新在同一次 Store.Update 中持久化，不会出现短暂重复 COM。
	strongOwners := make(map[string]bool, len(identityClaimedIDs)+len(strongAssignedIDs)+len(resolutionAssignedIDs))
	for ownerID := range identityClaimedIDs {
		strongOwners[ownerID] = true
	}
	for ownerID := range strongAssignedIDs {
		strongOwners[ownerID] = true
	}
	for ownerID := range resolutionAssignedIDs {
		strongOwners[ownerID] = true
	}
	for ownerID := range strongOwners {
		owner := manager.entries[ownerID]
		nextOwner, changing := updates[ownerID]
		if owner == nil || owner.waveshare == nil || !changing {
			continue
		}
		oldPort := owner.waveshare.Port()
		if blockedPort := wrongStrongClaimPorts[ownerID]; blockedPort != "" {
			oldPort = blockedPort
		} else if oldPort == "" && owner.requiresProbe {
			oldPort = normalizeCOMPort(owner.configured.Port)
		}
		newPort := normalizeCOMPort(nextOwner.Port)
		if oldPort == "" || oldPort == newPort {
			continue
		}
		if _, oldPortPresent := presentWaveshare[oldPort]; oldPortPresent {
			continue
		}
		for displacedID, displaced := range manager.entries {
			if displacedID == ownerID || displaced == nil || displaced.waveshare == nil || identityClaimedIDs[displacedID] {
				continue
			}
			displacedPort := displaced.waveshare.Port()
			if blockedPort := wrongStrongClaimPorts[displacedID]; blockedPort != "" {
				displacedPort = blockedPort
			} else if displacedPort == "" && displaced.requiresProbe {
				displacedPort = normalizeCOMPort(displaced.configured.Port)
			}
			if displacedPort != newPort {
				continue
			}
			if assignedPort, resolved := assignments[displacedID]; resolved && normalizeCOMPort(assignedPort) != newPort {
				continue
			}
			nextDisplaced, exists := updates[displacedID]
			if !exists {
				nextDisplaced = displaced.configured
			}
			nextDisplaced.Port = oldPort
			updates[displacedID] = nextDisplaced
			assignedIDs[displacedID] = false
			forcedOfflineIDs[displacedID] = true
			break
		}
	}
	unmatchedMissing := make([]*runtimeDeviceEntry, 0, len(missing)-len(assignments))
	for _, entry := range missing {
		if !assignedIDs[entry.configured.ID] {
			unmatchedMissing = append(unmatchedMissing, entry)
		}
	}

	additions := make([]config.Device, 0)
	for _, port := range unclaimedPorts {
		if assignedPorts[port] {
			continue
		}
		probe := probes[port]
		if len(unmatchedMissing) > 0 {
			// 新 COM 可能就是某个尚未映射的旧逻辑设备。探针尚未成功，
			// 或多块同型号无法唯一匹配时，先等下一轮扫描，不生成
			// 第二个驱动和丢失主题绑定的临时 ID。
			if probe.err != nil || !validResolution(probe.info.Resolution) ||
				probeCouldMatchMissing(probe.info.Resolution, unmatchedMissing) {
				continue
			}
		}
		candidate := probe.candidate
		id := uniqueRuntimeDeviceID(strings.TrimSpace(candidate.ID), "waveshare-"+strings.ToLower(port), knownIDs)
		knownIDs[id] = true
		width, height := candidate.Width, candidate.Height
		name := strings.TrimSpace(candidate.Name)
		if validResolution(probe.info.Resolution) {
			width, height = probe.info.Resolution[0], probe.info.Resolution[1]
			if strings.TrimSpace(probe.info.Name) != "" {
				name = probe.info.Name
			}
		}
		if width <= 0 || height <= 0 {
			width, height = 1024, 600
		}
		if name == "" {
			name = "微雪 USB Monitor（" + port + "）"
		}
		additions = append(additions, config.Device{
			ID: id, Name: name, Kind: "waveshare", Enabled: true,
			Port: port, StableID: candidate.StableID, Width: width, Height: height,
		})
	}

	var coolerAddition *config.Device
	var coolerTransport devicecoolermaster.HIDTransport
	if discoveredCooler != nil && !hasConfiguredCooler && !manager.hasRuntimeKindLocked("coolermaster") {
		candidate := *discoveredCooler
		id := uniqueRuntimeDeviceID(strings.TrimSpace(candidate.ID), "coolermaster-atmos-v2", knownIDs)
		libraryPath := manager.coolerLibraryPath(candidate.LibraryPath)
		transport, err := manager.openCoolerTransport(libraryPath)
		if err == nil && transport != nil {
			// 自动接入使用已确认的驻留槽位 4；已配置设备仍保留用户值。
			slot := defaultCoolerMasterSlot
			width, height := candidate.Width, candidate.Height
			if width <= 0 || height <= 0 {
				width, height = 480, 480
			}
			name := strings.TrimSpace(candidate.Name)
			if name == "" {
				name = "酷冷至尊冷龙影二代"
			}
			addition := config.Device{
				ID: id, Name: name, Kind: "coolermaster", Enabled: true,
				LibraryPath: libraryPath, Width: width, Height: height, Slot: slot,
			}
			coolerAddition = &addition
			coolerTransport = transport
		}
	}

	if coolerAddition != nil {
		additions = append(additions, *coolerAddition)
	}
	if len(updates) > 0 || len(additions) > 0 {
		if err := manager.persistReconciliationLocked(updates, additions); err != nil {
			if coolerTransport != nil {
				_ = coolerTransport.Close()
			}
			return err
		}
		// Store.Update 可能与工作台保存配置相邻发生。重新读取最终值，
		// 避免内存 entry 用扫描前的默认名称覆盖用户刚保存的自定义名称。
		persisted := manager.configuredSnapshotLocked()
		for _, current := range persisted.Devices {
			if _, exists := updates[current.ID]; exists {
				updates[current.ID] = current
			}
		}
	}

	type endpointChange struct {
		entry   *runtimeDeviceEntry
		oldPort string
		newPort string
	}
	endpointChanges := make(map[string]endpointChange, len(updates))
	for id, next := range updates {
		entry := manager.entries[id]
		if entry == nil || entry.waveshare == nil {
			continue
		}
		oldPort := entry.waveshare.Port()
		if blockedPort := wrongStrongClaimPorts[id]; blockedPort != "" {
			oldPort = blockedPort
		}
		wasPresent := entry.present
		entry.configured = next
		verified := identityClaimedIDs[id] || assignedIDs[id]
		if verified {
			entry.requiresProbe = false
			entry.presenceKnown = true
			entry.present = true
		} else if forcedOfflineIDs[id] {
			entry.requiresProbe = true
			entry.presenceKnown = true
			entry.present = false
		}
		if oldPort != normalizeCOMPort(next.Port) || wrongStrongClaimPorts[id] != "" {
			endpointChanges[id] = endpointChange{entry: entry, oldPort: oldPort, newPort: normalizeCOMPort(next.Port)}
		} else if verified && entry.presenceKnown && !wasPresent {
			reappeared = append(reappeared, entry)
		}
	}
	if len(endpointChanges) > 0 {
		resets := make([]service.ConnectionReset, 0, len(endpointChanges))
		for id, change := range endpointChanges {
			resets = append(resets, service.ConnectionReset{
				DeviceID: id,
				Reason: fmt.Sprintf(
					"微雪串口已从 %s 变为 %s，正在重新连接", change.oldPort, change.newPort,
				),
			})
		}
		_ = manager.controller.ResetDeviceConnectionsAtomically(ctx, resets, func() {
			for id, change := range endpointChanges {
				change.entry.waveshare.SetPort(change.newPort)
				log.Printf("微雪逻辑设备 %s 从 %s 重新枚举为 %s，保留原主题绑定", id, change.oldPort, change.newPort)
			}
		})
	}
	for _, entry := range reappeared {
		if _, changed := endpointChanges[entry.configured.ID]; changed {
			continue
		}
		port := entry.waveshare.Port()
		_ = manager.controller.ResetDeviceConnection(
			ctx, entry.configured.ID, fmt.Sprintf("微雪设备已在 %s 重新插入，正在重新连接", port),
		)
	}
	for _, addition := range additions {
		var initial devicecoolermaster.HIDTransport
		if coolerAddition != nil && addition.ID == coolerAddition.ID {
			initial = coolerTransport
		}
		if manager.registerDeviceLocked(addition, initial) {
			log.Printf("运行期发现并接入设备 %s（%s）", addition.ID, addition.Name)
		}
	}
	return nil
}

// handleCoolerOEMConflictLocked 处理原厂服务比 CaseScreen 更晚启动的竞态，
// 也处理 CaseScreen 启动前原厂已经占用设备的情况。
// 原厂代理能在不拔 USB 的情况下切换同一块 LCD 的显示模式；如果
// CaseScreen 继续保留 HID 句柄，两边的实时帧与默认画面会反复覆盖。
// 运行期扫描一旦发现冲突，会通过 Service 的单设备门闩先停帧、关句柄
// 并标记离线；同时把驻留槽摘要置为不可信，避免原厂覆盖槽位后仍错误
// 命中旧 digest。原厂退出后，既有重连路径会重传并恢复最新主题。
func (manager *runtimeDeviceManager) handleCoolerOEMConflictLocked(
	ctx context.Context,
	configured config.Config,
) error {
	if manager == nil || manager.controller == nil || manager.status == nil || manager.coolerConflict == nil {
		return nil
	}
	enabled := make(map[string]bool)
	for _, current := range configured.Devices {
		if current.Enabled && current.Kind == "coolermaster" {
			enabled[current.ID] = true
		}
	}
	if len(enabled) == 0 {
		return nil
	}
	conflict := manager.coolerConflict()
	if conflict == nil {
		return nil
	}
	if !errors.Is(conflict, devicecoolermaster.ErrOEMControlConflict) {
		return nil
	}
	connected := make(map[string]bool)
	for _, current := range manager.status().Devices {
		connected[strings.TrimSpace(current.Info.ID)] = current.Connected
	}
	var resetErrors []error
	for id := range enabled {
		entry := manager.entries[id]
		if !connected[id] {
			if entry != nil && entry.cooler != nil {
				if err := entry.cooler.InvalidateResidentThemeCache(); err != nil {
					resetErrors = append(resetErrors, fmt.Errorf("失效酷冷至尊驻留主题缓存 %s: %w", id, err))
				}
			}
			continue
		}
		reason := conflict.Error() + "；CaseScreen 已释放 HID 避免两套程序互相覆盖；原厂控制停止后会自动恢复已保存主题"
		var invalidateErr error
		if err := manager.controller.ResetDeviceConnectionsAtomically(
			ctx,
			[]service.ConnectionReset{{DeviceID: id, Reason: reason}},
			func() {
				if entry != nil && entry.cooler != nil {
					invalidateErr = entry.cooler.InvalidateResidentThemeCache()
				}
			},
		); err != nil {
			resetErrors = append(resetErrors, fmt.Errorf("释放酷冷至尊设备 %s: %w", id, err))
		}
		if invalidateErr != nil {
			resetErrors = append(resetErrors, fmt.Errorf("失效酷冷至尊驻留主题缓存 %s: %w", id, invalidateErr))
		}
		log.Printf("检测到酷冷原厂控制冲突，已释放 CaseScreen HID %s：%v", id, conflict)
	}
	return errors.Join(resetErrors...)
}

func (manager *runtimeDeviceManager) configuredSnapshotLocked() config.Config {
	if manager.store != nil {
		return manager.store.Get()
	}
	cfg := config.Default()
	for _, entry := range manager.entries {
		cfg.Devices = append(cfg.Devices, entry.configured)
	}
	return cfg
}

func (manager *runtimeDeviceManager) persistReconciliationLocked(updates map[string]config.Device, additions []config.Device) error {
	if manager.store == nil {
		return nil
	}
	return manager.store.Update(func(current *config.Config) error {
		indexes := make(map[string]int, len(current.Devices))
		for index := range current.Devices {
			indexes[current.Devices[index].ID] = index
		}
		for id, next := range updates {
			index, exists := indexes[id]
			if !exists {
				continue
			}
			current.Devices[index].Port = next.Port
			// next 是完整设备快照。允许在“旧序列号已消失 +
			// 握手分辨率双向唯一”的严格分支中清除过期强身份。
			current.Devices[index].StableID = normalizeWaveshareStableID(next.StableID)
			current.Devices[index].Width = next.Width
			current.Devices[index].Height = next.Height
			// 扫描只可修正系统生成的默认名称，不能覆盖工作台中
			// 与本轮 Store.Update 并发保存的用户自定义名称。
			if isDefaultConfiguredWaveshareName(current.Devices[index].Name, id) && strings.TrimSpace(next.Name) != "" {
				current.Devices[index].Name = next.Name
			}
		}
		for _, addition := range additions {
			if _, exists := indexes[addition.ID]; exists {
				continue
			}
			indexes[addition.ID] = len(current.Devices)
			current.Devices = append(current.Devices, addition)
		}
		return nil
	})
}

func (manager *runtimeDeviceManager) hasRuntimeKindLocked(kind string) bool {
	for _, entry := range manager.entries {
		if entry.configured.Kind == kind {
			return true
		}
	}
	return false
}

func (manager *runtimeDeviceManager) probeWavesharePort(ctx context.Context, port string) (device.Info, error) {
	opened, err := manager.openSerial(port)
	if err != nil {
		return device.Info{}, err
	}
	driver := devicewaveshare.New("runtime-probe", "微雪 USB Monitor", [2]int{}, opened, nil)
	driver.ActivationDelay = 0
	driver.ReconnectSettleDelay = 0
	driver.StorageTimeout = 300 * time.Millisecond
	driver.CommandTimeout = 2 * time.Second
	err = driver.Connect(ctx)
	info := driver.Info()
	closeErr := driver.Close()
	if err != nil {
		return info, err
	}
	if closeErr != nil {
		return info, closeErr
	}
	if !validResolution(info.Resolution) {
		return info, fmt.Errorf("微雪 %s 未报告有效分辨率", port)
	}
	return info, nil
}

func matchWaveshareEndpoints(
	missing []*runtimeDeviceEntry,
	ports []string,
	probes map[string]waveshareProbeResult,
	allowMissingStableIdentity map[string]bool,
) map[string]string {
	assignments := make(map[string]string)
	remainingEntries := append([]*runtimeDeviceEntry(nil), missing...)
	remainingPorts := append([]string(nil), ports...)
	for {
		matched := false
		for entryIndex, entry := range remainingEntries {
			matches := make([]int, 0, 1)
			for portIndex, port := range remainingPorts {
				if !waveshareRuntimeIdentityCompatible(
					entry.configured, probes[port].candidate, allowMissingStableIdentity[entry.configured.ID],
				) {
					continue
				}
				if sameResolution(
					[2]int{entry.configured.Width, entry.configured.Height}, probes[port].info.Resolution,
				) {
					matches = append(matches, portIndex)
				}
			}
			if len(matches) != 1 {
				continue
			}
			portIndex := matches[0]
			port := remainingPorts[portIndex]
			entryMatches := 0
			for _, candidateEntry := range remainingEntries {
				if !waveshareRuntimeIdentityCompatible(
					candidateEntry.configured, probes[port].candidate,
					allowMissingStableIdentity[candidateEntry.configured.ID],
				) {
					continue
				}
				if sameResolution(
					[2]int{candidateEntry.configured.Width, candidateEntry.configured.Height}, probes[port].info.Resolution,
				) {
					entryMatches++
				}
			}
			if entryMatches != 1 {
				continue
			}
			assignments[entry.configured.ID] = port
			remainingEntries = append(remainingEntries[:entryIndex], remainingEntries[entryIndex+1:]...)
			remainingPorts = append(remainingPorts[:portIndex], remainingPorts[portIndex+1:]...)
			matched = true
			break
		}
		if !matched {
			break
		}
	}
	if len(remainingEntries) == 1 && len(remainingPorts) == 1 {
		entry := remainingEntries[0]
		port := remainingPorts[0]
		probeResolution := probes[port].info.Resolution
		expected := [2]int{entry.configured.Width, entry.configured.Height}
		compatibleResolution := validResolution(probeResolution) && validResolution(expected) &&
			sameResolution(expected, probeResolution)
		if !validResolution(probeResolution) && !validResolution(expected) &&
			runtimeEntryPort(entry) == normalizeCOMPort(port) {
			compatibleResolution = true
		}
		// 单入单出也不能覆盖已经证明不兼容的型号或强身份。
		if waveshareRuntimeIdentityCompatible(
			entry.configured, probes[port].candidate, allowMissingStableIdentity[entry.configured.ID],
		) && compatibleResolution {
			assignments[entry.configured.ID] = port
		}
	}
	return assignments
}

func waveshareRuntimeIdentityCompatible(configured config.Device, candidate discovery.Device, allowMissingStableIdentity bool) bool {
	if waveshareStableIdentityCompatible(configured.StableID, candidate.StableID) {
		return true
	}
	// 降级只接受“候选没有序列号”，不接受任何不同序列号。
	// allowMissingStableIdentity 也只会在保存的序列号完全不在场时置为 true；
	// 真正分配前还必须通过 matchWaveshareEndpoints 的双向唯一分辨率检查。
	return allowMissingStableIdentity && normalizeWaveshareStableID(configured.StableID) != "" &&
		normalizeWaveshareStableID(candidate.StableID) == ""
}

func runtimeEntryPort(entry *runtimeDeviceEntry) string {
	if entry == nil {
		return ""
	}
	if entry.waveshare != nil {
		if port := entry.waveshare.Port(); port != "" {
			return port
		}
	}
	return normalizeCOMPort(entry.configured.Port)
}

func waveshareStableIdentityCompatible(configured, candidate string) bool {
	configured = normalizeWaveshareStableID(configured)
	candidate = normalizeWaveshareStableID(candidate)
	// 已绑定序列号的设备不会降级成分辨率猜测。
	// 候选端点无序列号或序列号不同时，都必须留给其他逻辑设备。
	return configured == "" || (candidate != "" && configured == candidate)
}

func probeCouldMatchMissing(resolution [2]int, missing []*runtimeDeviceEntry) bool {
	for _, entry := range missing {
		if entry != nil && sameResolution(
			resolution, [2]int{entry.configured.Width, entry.configured.Height},
		) {
			return true
		}
	}
	return false
}

func sameResolution(left, right [2]int) bool {
	if !validResolution(left) || !validResolution(right) {
		return false
	}
	return left == right || (left[0] == right[1] && left[1] == right[0])
}

func validResolution(resolution [2]int) bool {
	return resolution[0] > 0 && resolution[1] > 0
}

func normalizeCOMPort(port string) string {
	return strings.ToUpper(strings.TrimSpace(port))
}

func uniqueRuntimeDeviceID(preferred, fallback string, known map[string]bool) string {
	base := strings.TrimSpace(preferred)
	if base == "" {
		base = strings.TrimSpace(fallback)
	}
	if base == "" {
		base = "casescreen-device"
	}
	if !known[base] {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", base, suffix)
		if !known[candidate] {
			return candidate
		}
	}
}

func isDefaultConfiguredWaveshareName(name, id string) bool {
	name = strings.TrimSpace(name)
	return name == "" || name == strings.TrimSpace(id) || name == "微雪" || name == "微雪 7 英寸" ||
		name == "微雪 8.8 英寸" || strings.HasPrefix(name, "微雪 USB Monitor")
}
