package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"casescreen/internal/api"
	"casescreen/internal/audio"
	"casescreen/internal/config"
	"casescreen/internal/discovery"
	"casescreen/internal/lyrics"
	"casescreen/internal/platformauth"
	"casescreen/internal/processlifecycle"
	"casescreen/internal/service"
	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
	"casescreen/internal/trayapp"
)

const version = "1.0.2"

func main() {
	if err := run(); err != nil {
		log.Printf("CaseScreen 启动失败: %v", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "配置文件路径")
	headless := flag.Bool("headless", false, "不创建托盘图标")
	desktopOwnerPID := flag.Int("desktop-owner-pid", 0, "桌面工作台所有者进程 PID（仅由 CaseScreenDesktop 使用）")
	desktopOwnerStarted := flag.Uint64("desktop-owner-started", 0, "桌面工作台所有者启动标记（仅由 CaseScreenDesktop 使用）")
	showVersion := flag.Bool("version", false, "显示版本")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if err := validateDesktopOwnerFlags(*headless, *desktopOwnerPID, *desktopOwnerStarted); err != nil {
		return err
	}
	var desktopOwner processlifecycle.Snapshot
	if *desktopOwnerPID > 0 {
		owner, ownerErr := processlifecycle.Inspect(*desktopOwnerPID)
		if ownerErr != nil {
			return fmt.Errorf("桌面工作台所有者无效: %w", ownerErr)
		}
		desktopOwner = owner
		if *desktopOwnerStarted != 0 && desktopOwner.Started != *desktopOwnerStarted {
			return errors.New("桌面工作台所有者身份已变化")
		}
	}

	store, err := config.Open(*configPath)
	if err != nil {
		return err
	}
	logFile, err := setupLogging(store.Path())
	if err == nil {
		defer logFile.Close()
	}
	cfg := store.Get()
	collector := telemetry.NewCollector()
	if _, targetErr := collector.SetGPUTarget(cfg.TelemetryGPUAdapterID); targetErr != nil {
		// 保存的物理显卡临时不可用时保持服务可启动，但不偷偷改写用户选择；
		// API 会继续报告配置值，用户可在组件设置中切换为自动或现有显卡。
		log.Printf("恢复监控 GPU 失败: %v", targetErr)
	}
	controller := service.New(collector, time.Duration(cfg.TelemetryIntervalM)*time.Millisecond)
	controller.SetTelemetryEnabled(cfg.TelemetryEnabled)
	controller.SetRenderInterval(time.Second / time.Duration(cfg.HostRenderFPS))
	controller.SetJPEGQuality(cfg.JPEGQuality)
	ffmpegPath := cfg.FFmpegPath
	if ffmpegPath == "" {
		if executable, executableErr := os.Executable(); executableErr == nil {
			ffmpegPath = filepath.Join(filepath.Dir(executable), "ffmpeg.exe")
		}
	}
	controller.SetFFmpegPath(ffmpegPath)
	audioEngine := audio.NewEngine(audio.NewLoopback(), cfg.AudioBands)
	controller.SetSpectrumProvider(audioEngine)
	controller.SetAudioEnabled(cfg.AudioEnabled)
	cacheDirectory := filepath.Join(filepath.Dir(store.Path()), "lyrics-cache")
	platformSessions, err := platformauth.Open(filepath.Join(filepath.Dir(store.Path()), "platform-auth"))
	if err != nil {
		return fmt.Errorf("打开音乐平台登录态失败: %w", err)
	}
	localLyrics := &lyrics.LocalFileProvider{Path: cfg.LyricsPath}
	resolver := lyrics.NewResolver(cacheDirectory,
		lyrics.NeteaseProvider{Session: platformSessions},
		lyrics.QQProvider{Session: platformSessions},
		localLyrics,
	)
	resolver.SetPreferredProvider(cfg.LyricsProvider)
	// QQ 音乐与网易云音乐的原生歌曲 ID 都由主机内置只读适配器取得；
	// 不需要重启播放器、打开调试端口或运行额外辅助程序。
	lyricsEngine := lyrics.NewEngine(lyrics.NewNowPlayingSource(), resolver)
	controller.SetLyricsProvider(lyricsEngine)
	controller.SetLyricsEnabled(cfg.LyricsEnabled)
	runtimeDevices := newRuntimeDeviceManager(controller, store, filepath.Dir(store.Path()))
	startupDiscoveryContext, finishStartupDiscovery := context.WithTimeout(context.Background(), runtimeProbeTimeout+time.Second)
	cfg, err = runtimeDevices.ReconcileConfiguredBeforeRegister(startupDiscoveryContext, cfg)
	finishStartupDiscovery()
	if err != nil {
		return err
	}
	runtimeDevices.RegisterConfigured(cfg)
	// 每台副屏拥有独立分辨率和主题；旧版全局主题不再自动套用到所有设备。
	deviceKinds := make(map[string]string, len(cfg.Devices))
	for _, configured := range cfg.Devices {
		deviceKinds[configured.ID] = configured.Kind
	}
	for deviceID, themePath := range cfg.DeviceThemePaths {
		manifest, themeErr := loadTheme(themePath)
		if themeErr != nil {
			log.Printf("读取设备 %s 上次主题失败: %v", deviceID, themeErr)
			continue
		}
		if deviceKinds[deviceID] == "waveshare" && manifest.NeedsWaveshareCompilerUpgrade() {
			// 旧 Theme 可能永久烘焙了歌词面板，新的 LV 小包无法擦掉这些
			// 像素。禁止静默恢复，用户在编辑器点一次应用后会生成当前版本。
			log.Printf("设备 %s 上次微雪歌词主题需要重新应用后才能恢复", deviceID)
			continue
		}
		if themeErr = controller.SetInitialDeviceTheme(deviceID, manifest); themeErr != nil {
			log.Printf("设备 %s 上次主题无效: %v", deviceID, themeErr)
		}
	}

	server, err := api.New(controller, store, platformSessions)
	if err != nil {
		return err
	}
	server.SetApplicationVersion(version)
	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", cfg.ListenAddress, err)
	}
	defer listener.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	launchMode := "tray"
	if *headless {
		launchMode = "headless"
	}
	if desktopOwner.PID > 0 {
		launchMode = "desktop"
	}
	server.SetProcessLifecycle(api.ProcessLifecycle{
		Mode: launchMode, ProcessID: os.Getpid(), OwnerProcessID: desktopOwner.PID,
		OwnerProcessStarted: desktopOwner.Started,
	}, cancel)
	if desktopOwner.PID > 0 {
		go func() {
			// 桌面进程无论正常关闭、崩溃还是被任务管理器结束，托管后台都会
			// 取消服务上下文并释放 USB；手动 tray/headless 模式不启动此守护。
			if waitErr := processlifecycle.WaitForExit(ctx, desktopOwner); waitErr == nil {
				cancel()
			}
		}()
	}
	errorsChannel := make(chan error, 2)
	go func() { errorsChannel <- controller.Run(ctx) }()
	go runtimeDevices.Run(ctx)
	go func() { errorsChannel <- server.Serve(ctx, listener) }()
	go func() {
		if audioErr := audioEngine.Run(ctx); audioErr != nil {
			log.Printf("系统音频采集停止: %v", audioErr)
		}
	}()
	go func() {
		if lyricsErr := lyricsEngine.Run(ctx); lyricsErr != nil {
			log.Printf("歌词服务停止: %v", lyricsErr)
		}
	}()
	url := "http://" + listener.Addr().String() + "/"
	log.Printf("CaseScreen %s 已启动，控制台 %s", version, url)

	setTelemetry := func(enabled bool) error {
		if err := store.Update(func(current *config.Config) error {
			current.TelemetryEnabled = enabled
			return nil
		}); err != nil {
			return err
		}
		controller.SetTelemetryEnabled(enabled)
		return nil
	}
	if *headless {
		select {
		case <-ctx.Done():
		case runErr := <-errorsChannel:
			if runErr != nil {
				return runErr
			}
		}
		cancel()
		return nil
	}

	go func() {
		if runErr := <-errorsChannel; runErr != nil {
			log.Printf("后台服务停止: %v", runErr)
			cancel()
		}
	}()
	err = trayapp.Run(ctx, trayapp.Options{URL: url, Service: controller, SetTelemetry: setTelemetry, Cancel: cancel})
	cancel()
	return err
}

func validateDesktopOwnerFlags(headless bool, ownerPID int, ownerStarted uint64) error {
	if ownerPID < 0 || (ownerPID == 0) != (ownerStarted == 0) || (ownerPID > 0 && !headless) {
		return errors.New("桌面托管服务必须同时使用 -headless、所有者 PID 和非零启动标记")
	}
	return nil
}

// mergeDiscoveredWaveshareDevices 在驱动注册前对账微雪 USB 物理身份。
// 有 USB UniqueID 能力时按固件序列号绑定，COM 号只是当前端点。
// 无序列号且有多个候选时不猜测、不新增重复项，交给运行期握手分辨率匹配。
func mergeDiscoveredWaveshareDevices(cfg config.Config, discovered []discovery.Device) (config.Config, bool) {
	cfg.Devices = append([]config.Device(nil), cfg.Devices...)
	knownIDs := make(map[string]bool)
	waveshareIndexes := make([]int, 0, len(cfg.Devices))
	disabledWaveshareIndexes := make([]int, 0, len(cfg.Devices))
	allWaveshareIndexes := make([]int, 0, len(cfg.Devices))
	for index, configured := range cfg.Devices {
		knownIDs[configured.ID] = true
		if configured.Kind == "waveshare" {
			allWaveshareIndexes = append(allWaveshareIndexes, index)
			if configured.Enabled {
				waveshareIndexes = append(waveshareIndexes, index)
			} else {
				disabledWaveshareIndexes = append(disabledWaveshareIndexes, index)
			}
		}
	}
	candidates := make([]discovery.Device, 0, len(discovered))
	for _, candidate := range discovered {
		port := strings.ToUpper(strings.TrimSpace(candidate.Port))
		if candidate.Kind == "waveshare" && port != "" {
			candidate.Port = port
			candidate.StableID = normalizeWaveshareStableID(candidate.StableID)
			candidates = append(candidates, candidate)
		}
	}
	sort.SliceStable(candidates, func(left, right int) bool { return candidates[left].Port < candidates[right].Port })
	changed := false
	if len(candidates) == 0 {
		return cfg, false
	}

	// 有主题、模板、轮播、关屏意图或非临时 ID 的条目是用户逻辑设备。
	// 当旧版额外生成 waveshare-comN 重复项时，先保留这些逻辑所有者。
	selected := make([]int, 0, len(waveshareIndexes))
	nonOwners := make([]int, 0, len(waveshareIndexes))
	for _, index := range waveshareIndexes {
		if isPersistentWaveshareConfig(cfg, index) {
			selected = append(selected, index)
		} else {
			nonOwners = append(nonOwners, index)
		}
	}
	for _, index := range nonOwners {
		if len(selected) >= len(candidates) {
			break
		}
		selected = append(selected, index)
	}
	sort.SliceStable(selected, func(left, right int) bool {
		return waveshareConfigIdentityRank(cfg, selected[left]) > waveshareConfigIdentityRank(cfg, selected[right])
	})

	assignments := make(map[int]int)
	claimedCandidates := make(map[int]bool)
	assign := func(configuredIndex, candidateIndex int) {
		assignments[configuredIndex] = candidateIndex
		claimedCandidates[candidateIndex] = true
	}

	stableConfigCounts := make(map[string]int)
	stableCandidateCounts := make(map[string]int)
	for _, configuredIndex := range allWaveshareIndexes {
		if stableID := normalizeWaveshareStableID(cfg.Devices[configuredIndex].StableID); stableID != "" {
			stableConfigCounts[stableID]++
		}
	}
	for _, candidate := range candidates {
		if candidate.StableID != "" {
			stableCandidateCounts[candidate.StableID]++
		}
	}

	// 禁用条目的强序列号仍保留物理设备，但克隆序列号不做猜测。
	for _, configuredIndex := range disabledWaveshareIndexes {
		stableID := normalizeWaveshareStableID(cfg.Devices[configuredIndex].StableID)
		if stableID == "" || stableConfigCounts[stableID] != 1 || stableCandidateCounts[stableID] != 1 {
			continue
		}
		for candidateIndex := range candidates {
			if !claimedCandidates[candidateIndex] && candidates[candidateIndex].StableID == stableID {
				assign(configuredIndex, candidateIndex)
				break
			}
		}
	}

	// 1. 只有固件 USB 序列号才能跨插口强匹配。
	for _, configuredIndex := range selected {
		stableID := normalizeWaveshareStableID(cfg.Devices[configuredIndex].StableID)
		if stableID == "" || stableConfigCounts[stableID] != 1 || stableCandidateCounts[stableID] != 1 {
			continue
		}
		for candidateIndex := range candidates {
			if !claimedCandidates[candidateIndex] && candidates[candidateIndex].StableID == stableID {
				assign(configuredIndex, candidateIndex)
				break
			}
		}
	}

	// 部分 7 英寸镜像在升级 USB gadget 后会从“报告 SYK
	// 序列号”变为“不报告序列号”。只有保存的强身份已完全
	// 不在本轮枚举中、候选端点也没有另一个强身份，且屏端握手
	// 分辨率对逻辑设备和物理端点都是双向唯一时，才允许降级回绑。
	// 候选如果报告了不同序列号，仍然绝不会被接管。
	degradedStableAssignments := make(map[int]bool)
	for _, configuredIndex := range selected {
		if _, assigned := assignments[configuredIndex]; assigned {
			continue
		}
		stableID := normalizeWaveshareStableID(cfg.Devices[configuredIndex].StableID)
		if stableID == "" || stableConfigCounts[stableID] != 1 || stableCandidateCounts[stableID] != 0 {
			continue
		}
		expected := configuredWaveshareExpectedResolution(cfg, configuredIndex)
		if !validResolution(expected) {
			continue
		}
		matches := make([]int, 0, 1)
		for candidateIndex, candidate := range candidates {
			if claimedCandidates[candidateIndex] || normalizeWaveshareStableID(candidate.StableID) != "" {
				continue
			}
			if sameResolution(expected, [2]int{candidate.Width, candidate.Height}) {
				matches = append(matches, candidateIndex)
			}
		}
		if len(matches) != 1 {
			continue
		}
		chosen := matches[0]
		logicalMatches := 0
		for _, otherIndex := range append(append([]int(nil), selected...), disabledWaveshareIndexes...) {
			if _, assigned := assignments[otherIndex]; assigned {
				continue
			}
			if sameResolution(
				configuredWaveshareExpectedResolution(cfg, otherIndex),
				[2]int{candidates[chosen].Width, candidates[chosen].Height},
			) {
				logicalMatches++
			}
		}
		if logicalMatches != 1 {
			continue
		}
		assign(configuredIndex, chosen)
		degradedStableAssignments[configuredIndex] = true
	}

	// 强序列号所有者都完成后，禁用且无序列号的条目才可
	// 用同 COM 作当次弱保留，不能抢走换口后的序列号屏。
	for _, configuredIndex := range disabledWaveshareIndexes {
		if normalizeWaveshareStableID(cfg.Devices[configuredIndex].StableID) != "" {
			continue
		}
		port := normalizeCOMPort(cfg.Devices[configuredIndex].Port)
		for candidateIndex := range candidates {
			if !claimedCandidates[candidateIndex] && candidates[candidateIndex].Port == port {
				assign(configuredIndex, candidateIndex)
				break
			}
		}
	}
	remainingLogical := 0
	remainingPhysical := 0
	for _, configuredIndex := range selected {
		if _, assigned := assignments[configuredIndex]; !assigned {
			remainingLogical++
		}
	}
	for candidateIndex := range candidates {
		if !claimedCandidates[candidateIndex] {
			remainingPhysical++
		}
	}
	allowWeakMatching := remainingLogical <= remainingPhysical
	resolutionIndexes := append([]int(nil), selected...)
	for _, configuredIndex := range disabledWaveshareIndexes {
		if _, assigned := assignments[configuredIndex]; !assigned &&
			normalizeWaveshareStableID(cfg.Devices[configuredIndex].StableID) == "" {
			resolutionIndexes = append(resolutionIndexes, configuredIndex)
		}
	}

	// 2. 无序列号屏按实机握手分辨率匹配。逻辑期望尺寸优先
	// 从其 active theme 清单读取，不信任可能已被错误 COM 会话覆写的 Device 尺寸。
	for _, configuredIndex := range resolutionIndexes {
		if _, assigned := assignments[configuredIndex]; assigned || normalizeWaveshareStableID(cfg.Devices[configuredIndex].StableID) != "" {
			continue
		}
		expected := configuredWaveshareExpectedResolution(cfg, configuredIndex)
		matches := make([]int, 0, 1)
		for candidateIndex, candidate := range candidates {
			if !claimedCandidates[candidateIndex] && sameResolution(expected, [2]int{candidate.Width, candidate.Height}) {
				matches = append(matches, candidateIndex)
			}
		}
		if len(matches) == 0 {
			continue
		}
		chosen := matches[0]
		matchedCurrentPort := false
		for _, candidateIndex := range matches {
			if candidates[candidateIndex].Port == normalizeCOMPort(cfg.Devices[configuredIndex].Port) {
				chosen = candidateIndex
				matchedCurrentPort = true
				break
			}
		}
		if len(matches) > 1 && !matchedCurrentPort {
			// 多块同分辨率无序列号屏无法跨口区分。
			continue
		}
		candidateMatches := 0
		for _, otherConfiguredIndex := range resolutionIndexes {
			if _, assigned := assignments[otherConfiguredIndex]; assigned ||
				normalizeWaveshareStableID(cfg.Devices[otherConfiguredIndex].StableID) != "" {
				continue
			}
			if sameResolution(
				configuredWaveshareExpectedResolution(cfg, otherConfiguredIndex),
				[2]int{candidates[chosen].Width, candidates[chosen].Height},
			) {
				candidateMatches++
			}
		}
		if candidateMatches != 1 {
			// 当前 COM 只可在多个物理候选间破平局，不能绕过
			// 多个同型逻辑设备共享同一分辨率造成的身份歧义。
			continue
		}
		assign(configuredIndex, chosen)
	}

	// 3. 仅对缺失强身份且分辨率信息不完整的旧配置，使用同 COM 弱提示。
	for _, configuredIndex := range selected {
		if !allowWeakMatching {
			break
		}
		if _, assigned := assignments[configuredIndex]; assigned || normalizeWaveshareStableID(cfg.Devices[configuredIndex].StableID) != "" {
			continue
		}
		expected := configuredWaveshareExpectedResolution(cfg, configuredIndex)
		if validResolution(expected) {
			continue
		}
		for candidateIndex, candidate := range candidates {
			if !claimedCandidates[candidateIndex] && candidate.Port == normalizeCOMPort(cfg.Devices[configuredIndex].Port) {
				assign(configuredIndex, candidateIndex)
				break
			}
		}
	}

	// 当 active theme 已确定两个逻辑型号时，剩余一对可由一对一关系确定。
	remainingConfigured := make([]int, 0, 1)
	remainingCandidates := make([]int, 0, 1)
	for _, configuredIndex := range selected {
		if _, assigned := assignments[configuredIndex]; !assigned && normalizeWaveshareStableID(cfg.Devices[configuredIndex].StableID) == "" {
			remainingConfigured = append(remainingConfigured, configuredIndex)
		}
	}
	for candidateIndex := range candidates {
		if !claimedCandidates[candidateIndex] {
			remainingCandidates = append(remainingCandidates, candidateIndex)
		}
	}
	if allowWeakMatching && len(remainingConfigured) == 1 && len(remainingCandidates) == 1 && len(selected) > 1 {
		configuredIndex := remainingConfigured[0]
		candidateIndex := remainingCandidates[0]
		expected := configuredWaveshareExpectedResolution(cfg, configuredIndex)
		actual := [2]int{candidates[candidateIndex].Width, candidates[candidateIndex].Height}
		compatibleResolution := validResolution(expected) && validResolution(actual) && sameResolution(expected, actual)
		// 双方都没有尺寸时，仅保守接受仍在原 COM 的旧端点；任何一方
		// 已知且不相等都不是“剩余一对”可以覆盖的型号证据。
		if !validResolution(expected) && !validResolution(actual) &&
			normalizeCOMPort(cfg.Devices[configuredIndex].Port) == candidates[candidateIndex].Port {
			compatibleResolution = true
		}
		if waveshareStableIdentityCompatible(
			cfg.Devices[configuredIndex].StableID, candidates[candidateIndex].StableID,
		) && compatibleResolution {
			assign(configuredIndex, candidateIndex)
		}
	}

	// 强序列号屏占用了另一条离线逻辑设备保存的旧 COM 时，
	// 用强所有者已确认离线的旧端点做原子交换。这只是占位，
	// 不声称离线无序列号屏已在该端点，但可确保它不会打开强所有者。
	presentPorts := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		presentPorts[candidate.Port] = true
	}
	for ownerIndex, candidateIndex := range assignments {
		owner := cfg.Devices[ownerIndex]
		candidateStableID := normalizeWaveshareStableID(candidates[candidateIndex].StableID)
		expected := configuredWaveshareExpectedResolution(cfg, ownerIndex)
		actual := [2]int{candidates[candidateIndex].Width, candidates[candidateIndex].Height}
		resolutionVerified := validResolution(expected) && validResolution(actual) && sameResolution(expected, actual)
		strongAssignment := normalizeWaveshareStableID(owner.StableID) != "" ||
			(candidateStableID != "" && stableCandidateCounts[candidateStableID] == 1) || resolutionVerified
		if !strongAssignment || owner.Port == candidates[candidateIndex].Port ||
			presentPorts[normalizeCOMPort(owner.Port)] {
			continue
		}
		for displacedIndex := range cfg.Devices {
			if displacedIndex == ownerIndex || cfg.Devices[displacedIndex].Kind != "waveshare" ||
				normalizeCOMPort(cfg.Devices[displacedIndex].Port) != candidates[candidateIndex].Port {
				continue
			}
			if _, physicallyAssigned := assignments[displacedIndex]; physicallyAssigned {
				continue
			}
			cfg.Devices[displacedIndex].Port = normalizeCOMPort(owner.Port)
			changed = true
			break
		}
	}

	for configuredIndex, candidateIndex := range assignments {
		configured := &cfg.Devices[configuredIndex]
		candidate := candidates[candidateIndex]
		if configured.Port != candidate.Port {
			configured.Port = candidate.Port
			changed = true
		}
		if degradedStableAssignments[configuredIndex] && configured.StableID != "" {
			// 当前固件已不再提供旧序列号，清除过期强身份；
			// 以后若再报告唯一序列号，下方逻辑会自动重新学习。
			configured.StableID = ""
			changed = true
		} else if normalized := normalizeWaveshareStableID(configured.StableID); configured.StableID != normalized {
			configured.StableID = normalized
			changed = true
		}
		if configured.StableID == "" && candidate.StableID != "" {
			configured.StableID = candidate.StableID
			changed = true
		}
		resolutionChanged := candidate.Width > 0 && candidate.Height > 0 &&
			(configured.Width != candidate.Width || configured.Height != candidate.Height)
		if resolutionChanged {
			configured.Width, configured.Height = candidate.Width, candidate.Height
			changed = true
		}
		if isDefaultConfiguredWaveshareName(configured.Name, configured.ID) && strings.TrimSpace(candidate.Name) != "" &&
			(configured.Name == "" || strings.HasPrefix(configured.Name, "微雪 USB Monitor") || resolutionChanged) && configured.Name != candidate.Name {
			configured.Name = candidate.Name
			changed = true
		}
	}

	// 只有当所有当前实机都已分配给逻辑所有者时，才删除无任何
	// 用户状态的旧版自动重复项。有主题的两个 ID 始终分别保留。
	allCandidatesAssigned := len(claimedCandidates) == len(candidates)
	removed := make(map[int]bool)
	aliases := make(map[string]string)
	if allCandidatesAssigned {
		selectedSet := make(map[int]bool, len(selected))
		for _, index := range selected {
			selectedSet[index] = true
		}
		for _, index := range nonOwners {
			if selectedSet[index] {
				continue
			}
			removed[index] = true
			removedPort := normalizeCOMPort(cfg.Devices[index].Port)
			for keeperIndex, candidateIndex := range assignments {
				if candidates[candidateIndex].Port == removedPort || sameResolution(
					configuredWaveshareExpectedResolution(cfg, index),
					[2]int{candidates[candidateIndex].Width, candidates[candidateIndex].Height},
				) {
					aliases[cfg.Devices[index].ID] = cfg.Devices[keeperIndex].ID
					break
				}
			}
			changed = true
		}
	}
	if len(removed) > 0 {
		retained := make([]config.Device, 0, len(cfg.Devices)-len(removed))
		for index, configured := range cfg.Devices {
			if !removed[index] {
				retained = append(retained, configured)
			}
		}
		cfg.Devices = retained
		for removedID, keeperID := range aliases {
			rebindWaveshareConfigState(&cfg, removedID, keeperID)
			delete(knownIDs, removedID)
		}
	}

	unmatchedLogical := false
	for _, configuredIndex := range append(append([]int(nil), selected...), disabledWaveshareIndexes...) {
		if _, assigned := assignments[configuredIndex]; !assigned {
			unmatchedLogical = true
			break
		}
	}
	if len(claimedCandidates) < len(candidates) && unmatchedLogical {
		// 有旧逻辑设备仍未确认身份时，不生成新 ID 来扩大重复。
		return cfg, changed
	}
	for candidateIndex, candidate := range candidates {
		if claimedCandidates[candidateIndex] {
			continue
		}
		deviceID := strings.TrimSpace(candidate.ID)
		if deviceID == "" {
			deviceID = "waveshare-" + strings.ToLower(candidate.Port)
		}
		baseID := deviceID
		for suffix := 2; knownIDs[deviceID]; suffix++ {
			deviceID = fmt.Sprintf("%s-%d", baseID, suffix)
		}
		name := strings.TrimSpace(candidate.Name)
		if name == "" {
			name = "微雪 USB Monitor（" + candidate.Port + "）"
		}
		width, height := candidate.Width, candidate.Height
		if width <= 0 || height <= 0 {
			width, height = 1024, 600
		}
		cfg.Devices = append(cfg.Devices, config.Device{
			ID: deviceID, Name: name, Kind: "waveshare", Enabled: true,
			Port: candidate.Port, StableID: candidate.StableID, Width: width, Height: height,
		})
		knownIDs[deviceID] = true
		changed = true
	}
	return cfg, changed
}

func normalizeWaveshareStableID(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func autoWaveshareLogicalID(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if !strings.HasPrefix(value, "waveshare-com") {
		return false
	}
	rest := strings.TrimPrefix(value, "waveshare-com")
	for _, part := range strings.Split(rest, "-") {
		if part == "" {
			return false
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	return true
}

func isPersistentWaveshareConfig(cfg config.Config, index int) bool {
	if index < 0 || index >= len(cfg.Devices) {
		return false
	}
	configured := cfg.Devices[index]
	if !autoWaveshareLogicalID(configured.ID) || configured.PoweredOff || normalizeWaveshareStableID(configured.StableID) != "" {
		return true
	}
	if !isDefaultConfiguredWaveshareName(configured.Name, configured.ID) {
		return true
	}
	if strings.TrimSpace(cfg.DeviceThemePaths[configured.ID]) != "" || len(cfg.WaveshareTemplates[configured.ID]) > 0 {
		return true
	}
	_, hasPlaylist := cfg.WavesharePlaylists[configured.ID]
	return hasPlaylist
}

func waveshareConfigIdentityRank(cfg config.Config, index int) int {
	configured := cfg.Devices[index]
	rank := 0
	if normalizeWaveshareStableID(configured.StableID) != "" {
		rank += 100
	}
	if !autoWaveshareLogicalID(configured.ID) {
		rank += 40
	}
	if strings.TrimSpace(cfg.DeviceThemePaths[configured.ID]) != "" {
		rank += 20
	}
	if len(cfg.WaveshareTemplates[configured.ID]) > 0 {
		rank += 10
	}
	if _, exists := cfg.WavesharePlaylists[configured.ID]; exists {
		rank += 5
	}
	if configured.PoweredOff {
		rank++
	}
	return rank
}

func configuredWaveshareExpectedResolution(cfg config.Config, index int) [2]int {
	if index < 0 || index >= len(cfg.Devices) {
		return [2]int{}
	}
	configured := cfg.Devices[index]
	paths := make([]string, 0, 1+len(cfg.WaveshareTemplates[configured.ID]))
	if path := strings.TrimSpace(cfg.DeviceThemePaths[configured.ID]); path != "" {
		paths = append(paths, path)
	}
	for _, record := range cfg.WaveshareTemplates[configured.ID] {
		if path := strings.TrimSpace(record.ManifestPath); path != "" {
			paths = append(paths, path)
		}
	}
	for _, path := range paths {
		manifest, err := loadTheme(path)
		if err == nil && manifest.Width > 0 && manifest.Height > 0 {
			return [2]int{manifest.Width, manifest.Height}
		}
	}
	return [2]int{configured.Width, configured.Height}
}

func rebindWaveshareConfigState(cfg *config.Config, removedID, keeperID string) {
	if cfg == nil || removedID == "" || keeperID == "" || removedID == keeperID {
		return
	}
	if path, exists := cfg.DeviceThemePaths[removedID]; exists {
		if strings.TrimSpace(cfg.DeviceThemePaths[keeperID]) == "" {
			cfg.DeviceThemePaths[keeperID] = path
		}
		delete(cfg.DeviceThemePaths, removedID)
	}
	if removed, exists := cfg.WaveshareTemplates[removedID]; exists {
		known := make(map[string]bool, len(cfg.WaveshareTemplates[keeperID]))
		for _, record := range cfg.WaveshareTemplates[keeperID] {
			known[record.ID] = true
		}
		for _, record := range removed {
			if !known[record.ID] {
				cfg.WaveshareTemplates[keeperID] = append(cfg.WaveshareTemplates[keeperID], record)
				known[record.ID] = true
			}
		}
		delete(cfg.WaveshareTemplates, removedID)
	}
	if playlist, exists := cfg.WavesharePlaylists[removedID]; exists {
		if _, keeperExists := cfg.WavesharePlaylists[keeperID]; !keeperExists {
			cfg.WavesharePlaylists[keeperID] = playlist
		}
		delete(cfg.WavesharePlaylists, removedID)
	}
}

func addConfiguredDevices(controller *service.Service, cfg config.Config, stateDirectory string) {
	manager := newRuntimeDeviceManager(controller, nil, stateDirectory)
	manager.RegisterConfigured(cfg)
}

// resolveCoolerMasterLibraryPath 保留仍然存在的用户自定义 hidapi 路径；
// 旧发行目录被清理后，陈旧路径必须自动回退到当前 CaseScreen.exe 同目录，
// 避免新版工作台仍依赖已经删除的旧版运行库。
func resolveCoolerMasterLibraryPath(configuredPath, executablePath string) string {
	configuredPath = strings.TrimSpace(configuredPath)
	if configuredPath != "" {
		if info, err := os.Stat(configuredPath); err == nil && !info.IsDir() {
			return configuredPath
		}
	}
	if strings.TrimSpace(executablePath) == "" {
		return configuredPath
	}
	return filepath.Join(filepath.Dir(executablePath), "hidapi.dll")
}

// orderedConfiguredDevices 固定后端注册顺序，使 API、托盘和工作台始终先看到
// 微雪 7 英寸、微雪 8.8 英寸，再看到酷冷至尊。排序使用副本，不改写配置文件顺序。
func orderedConfiguredDevices(devices []config.Device) []config.Device {
	ordered := append([]config.Device(nil), devices...)
	sort.SliceStable(ordered, func(left, right int) bool {
		return configuredDeviceRank(ordered[left]) < configuredDeviceRank(ordered[right])
	})
	return ordered
}

func configuredDeviceRank(configured config.Device) int {
	if configured.Kind == "waveshare" {
		identity := strings.ToLower(strings.TrimSpace(configured.ID + " " + configured.Name))
		if strings.Contains(identity, "8.8") || (configured.Height > configured.Width && configured.Height >= 1000) {
			return 1
		}
		if strings.Contains(identity, "7") || (configured.Width == 1024 && configured.Height == 600) {
			return 0
		}
		return 2
	}
	if configured.Kind == "coolermaster" {
		return 3
	}
	return 4
}

func setupLogging(configPath string) (*os.File, error) {
	path := filepath.Join(filepath.Dir(configPath), "casescreen.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	log.SetOutput(file)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	return file, nil
}

func loadTheme(path string) (theme.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return theme.Manifest{}, err
	}
	var manifest theme.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return theme.Manifest{}, fmt.Errorf("解析主题 %s: %w", path, err)
	}
	return manifest, nil
}
