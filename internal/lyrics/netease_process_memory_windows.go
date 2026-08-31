//go:build windows

package lyrics

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	neteaseMemoryMaximumProcesses = 8
	neteaseMemoryMaximumScanBytes = 64 << 20
	neteaseMemoryScanChunkBytes   = 1 << 20
	neteaseMemoryMaximumString    = 128
	neteaseMemoryDurationSlack    = 2 * time.Second
	neteaseMemoryPositionSlack    = 5 * time.Second
	neteaseMemoryStableReadSlack  = time.Second
	neteaseMemoryFailedProbeRetry = 10 * time.Second
	neteaseMemoryMaximumDuration  = 24 * time.Hour
)

// 特征码和网易云 3.x 播放器布局来自 Kxnrl 的 MIT 项目：
// https://github.com/Kxnrl/NetEase-Cloud-Music-DiscordRPC/blob/d3b77c679379aff1294cc83a285ad4f695376ad6/Vanessa/Players/NetEase.cs
// CaseScreen 只申请 PROCESS_QUERY_INFORMATION | PROCESS_VM_READ，
// 不写内存、不注入 DLL、不创建远程线程，也不读取音频内容。
var neteaseAudioPlayerPattern = []int16{
	0x48, 0x8D, 0x0D, -1, -1, -1, -1, 0xE8, -1, -1, -1, -1,
	0x48, 0x8D, 0x0D, -1, -1, -1, -1, 0xE8, -1, -1, -1, -1,
	0x90, 0x48, 0x8D, 0x0D, -1, -1, -1, -1, 0xE8, -1, -1, -1, -1,
	0x48, 0x8D, 0x05, -1, -1, -1, -1, 0x48, 0x8D, 0xA5, -1, -1, -1, -1,
	0x5F, 0x5D, 0xC3, 0xCC, 0xCC, 0xCC, 0xCC, 0xCC,
	0x48, 0x89, 0x4C, 0x24, -1, 0x55, 0x57, 0x48, 0x81, 0xEC, -1, -1, -1, -1,
	0x48, 0x8D, 0x6C, 0x24, -1, 0x48, 0x8D, 0x7C, 0x24,
}

var neteaseAudioSchedulePattern = []int16{
	0x66, 0x0F, 0x2E, 0x0D, -1, -1, -1, -1,
	0x7A, -1, 0x75, -1, 0x66, 0x0F, 0x2E, 0x15,
}

var (
	errNeteaseNoPlaybackEvidence        = errors.New("网易云进程当前没有播放证据")
	errNeteaseTransientPlaybackEvidence = errors.New("网易云播放结构正在变化")
)

type neteaseNativeIDSource interface {
	CurrentTrackID(context.Context, NowPlaying) (string, bool)
}

type neteaseMemoryEvidence struct {
	ProcessID uint32
	TrackID   string
	Status    int32
	Duration  time.Duration
	Position  time.Duration
	SampledAt time.Time
}

func (evidence neteaseMemoryEvidence) playing() bool { return evidence.Status == 1 }

type neteaseProcessProbe struct {
	handle             windows.Handle
	audioPlayerPointer uintptr
	schedulePointer    uintptr
}

type neteaseMemoryRange struct {
	start uintptr
	end   uintptr
}

type neteasePatternMatches struct {
	player   []uintptr
	schedule []uintptr
}

type neteaseProcessMemorySource struct {
	mu          sync.Mutex
	probes      map[uint32]*neteaseProcessProbe
	failedProbe map[uint32]time.Time

	// 以下函数只允许单测注入。生产实例始终使用 Windows
	// 只读实现，且整轮检查必须只得到一个有效播放进程。
	processIDs   func() ([]uint32, error)
	openProbe    func(context.Context, uint32) (*neteaseProcessProbe, error)
	readEvidence func(context.Context, *neteaseProcessProbe) (neteaseMemoryEvidence, error)
	now          func() time.Time
}

func newNeteaseProcessMemorySource() *neteaseProcessMemorySource {
	return &neteaseProcessMemorySource{
		probes:      make(map[uint32]*neteaseProcessProbe),
		failedProbe: make(map[uint32]time.Time),
		processIDs:  neteaseMusicProcessIDs,
		openProbe:   openNeteaseProcessProbe,
		readEvidence: func(ctx context.Context, probe *neteaseProcessProbe) (neteaseMemoryEvidence, error) {
			return probe.readEvidence(ctx)
		},
		now: time.Now,
	}
}

// enrichNeteasePlatformTrackIDFromSource 只接受本进程内置、只读
// 适配器在当前轮询中取得的原生 ID。不读取窗口标题推测
// ID，不读历史 webdb，也不接受外部进程预填的身份。
func enrichNeteasePlatformTrackIDFromSource(ctx context.Context, track NowPlaying,
	native neteaseNativeIDSource) NowPlaying {
	if sourceProviderID(track.Source) != "netease" {
		return track
	}
	// 即使输入对象已经携带 ID，也必须丢弃并用本轮只读原生证据重验；
	// 绝不信任外部调用点或上一轮留下的身份。
	track.TrackID = ""
	track.TrackIDOrigin = ""
	if native == nil {
		return track
	}
	trackID, ok := native.CurrentTrackID(ctx, track)
	if !ok || !validPlatformTrackID("netease", trackID) {
		return track
	}
	track.TrackID = trackID
	track.TrackIDOrigin = TrackIDOriginSource
	return track
}

// CurrentTrackID 是给已有 SMTC 路径保留的严格交叉验证入口。
// 正式网易云路径会直接使用 currentEvidence，再用 playingList
// 中完全相同的 ID 取得元数据；这里不按标题反查歌曲身份。
func (source *neteaseProcessMemorySource) CurrentTrackID(ctx context.Context,
	current NowPlaying) (string, bool) {
	if source == nil || ctx == nil || ctx.Err() != nil ||
		sourceProviderID(current.Source) != "netease" || !neteaseSMTCEvidenceComplete(current) {
		return "", false
	}
	evidence, err := source.currentEvidence(ctx)
	if err != nil || !neteaseMemoryEvidenceMatchesCurrent(evidence, current, time.Now()) {
		return "", false
	}
	return evidence.TrackID, true
}

// currentEvidence 扫描所有 cloudmusic.exe，但只接受唯一一个能返回
// 合法 ID、状态、时长和进度的进程。辅助进程虽然也加载
// cloudmusic.dll 和同一段代码，但 Waiting/空播放结构不会成为候选。
// 只要有任一候选进程无法被完整验证，本轮就失败关闭。
func (source *neteaseProcessMemorySource) currentEvidence(ctx context.Context) (neteaseMemoryEvidence, error) {
	if source == nil || ctx == nil {
		return neteaseMemoryEvidence{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return neteaseMemoryEvidence{}, err
	}
	source.mu.Lock()
	defer source.mu.Unlock()

	processIDs := source.processIDs
	if processIDs == nil {
		processIDs = neteaseMusicProcessIDs
	}
	openProbe := source.openProbe
	if openProbe == nil {
		openProbe = openNeteaseProcessProbe
	}
	readEvidence := source.readEvidence
	if readEvidence == nil {
		readEvidence = func(ctx context.Context, probe *neteaseProcessProbe) (neteaseMemoryEvidence, error) {
			return probe.readEvidence(ctx)
		}
	}
	nowFunction := source.now
	if nowFunction == nil {
		nowFunction = time.Now
	}

	pids, err := processIDs()
	if err != nil {
		source.closeAllLocked()
		return neteaseMemoryEvidence{}, fmt.Errorf("枚举网易云进程: %w", err)
	}
	if len(pids) == 0 {
		source.closeAllLocked()
		return neteaseMemoryEvidence{}, errNeteaseNoPlaybackEvidence
	}
	if len(pids) > neteaseMemoryMaximumProcesses {
		source.closeAllLocked()
		return neteaseMemoryEvidence{}, fmt.Errorf("网易云进程数量 %d 超过上限 %d",
			len(pids), neteaseMemoryMaximumProcesses)
	}

	alive := make(map[uint32]struct{}, len(pids))
	now := nowFunction()
	var candidate neteaseMemoryEvidence
	validCandidates := 0
	var verificationErr error
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return neteaseMemoryEvidence{}, err
		}
		if _, duplicate := alive[pid]; duplicate {
			return neteaseMemoryEvidence{}, fmt.Errorf("网易云进程枚举包含重复 PID %d", pid)
		}
		alive[pid] = struct{}{}
		probe := source.probes[pid]
		if probe == nil {
			if failedAt, failed := source.failedProbe[pid]; failed && now.Sub(failedAt) >= 0 &&
				now.Sub(failedAt) < neteaseMemoryFailedProbeRetry {
				verificationErr = fmt.Errorf("网易云 PID %d 处于只读探针退避期", pid)
				continue
			}
			probe, err = openProbe(ctx, pid)
			if err != nil {
				source.failedProbe[pid] = now
				if ctxErr := ctx.Err(); ctxErr != nil {
					return neteaseMemoryEvidence{}, ctxErr
				}
				verificationErr = fmt.Errorf("打开网易云 PID %d 只读探针: %w", pid, err)
				continue
			}
			source.probes[pid] = probe
			delete(source.failedProbe, pid)
		}

		evidence, readErr := readEvidence(ctx, probe)
		switch {
		case readErr == nil:
			evidence.ProcessID = pid
			validCandidates++
			candidate = evidence
		case errors.Is(readErr, errNeteaseNoPlaybackEvidence):
			// 已完整读到 Waiting 或空播放结构，证明该进程
			// 不是当前播放者，可安全忽略。
		case errors.Is(readErr, errNeteaseTransientPlaybackEvidence):
			verificationErr = fmt.Errorf("读取网易云 PID %d 播放证据: %w", pid, readErr)
		default:
			// 地址在客户端更新或进程重启后可能失效。
			// 本轮释放并失败关闭，下轮重新扫描。
			probe.close()
			delete(source.probes, pid)
			verificationErr = fmt.Errorf("读取网易云 PID %d 只读内存: %w", pid, readErr)
		}
	}

	for pid, probe := range source.probes {
		if _, ok := alive[pid]; !ok {
			probe.close()
			delete(source.probes, pid)
		}
	}
	for pid := range source.failedProbe {
		if _, ok := alive[pid]; !ok {
			delete(source.failedProbe, pid)
		}
	}
	if verificationErr != nil {
		return neteaseMemoryEvidence{}, verificationErr
	}
	if validCandidates == 0 {
		return neteaseMemoryEvidence{}, errNeteaseNoPlaybackEvidence
	}
	if validCandidates != 1 || !validPlatformTrackID("netease", candidate.TrackID) {
		return neteaseMemoryEvidence{}, fmt.Errorf("网易云有效播放进程数量无效: %d", validCandidates)
	}
	return candidate, nil
}

func neteaseSMTCEvidenceComplete(current NowPlaying) bool {
	return current.PositionKnown && current.Duration > 0 &&
		normalizeText(current.Title) != "" && normalizeText(current.Artist) != "" &&
		normalizeText(current.Album) != ""
}

func neteaseMemoryEvidenceMatchesCurrent(evidence neteaseMemoryEvidence,
	current NowPlaying, now time.Time) bool {
	if !validPlatformTrackID("netease", evidence.TrackID) ||
		sourceProviderID(current.Source) != "netease" || !neteaseSMTCEvidenceComplete(current) ||
		(evidence.Status != 1 && evidence.Status != 2) || evidence.playing() != current.Playing ||
		evidence.Duration <= 0 || evidence.Duration > neteaseMemoryMaximumDuration ||
		evidence.Position < 0 || evidence.Position > evidence.Duration+neteaseMemoryPositionSlack {
		return false
	}
	durationDifference := evidence.Duration - time.Duration(current.Duration)
	if durationDifference < 0 {
		durationDifference = -durationDifference
	}
	if durationDifference > neteaseMemoryDurationSlack {
		return false
	}
	sampledAt := evidence.SampledAt
	if sampledAt.IsZero() {
		sampledAt = now
	}
	positionDifference := evidence.Position - playbackPositionAt(current, sampledAt)
	if positionDifference < 0 {
		positionDifference = -positionDifference
	}
	return positionDifference <= neteaseMemoryPositionSlack
}

func (source *neteaseProcessMemorySource) closeAll() {
	if source == nil {
		return
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	source.closeAllLocked()
}

func (source *neteaseProcessMemorySource) closeAllLocked() {
	for pid, probe := range source.probes {
		probe.close()
		delete(source.probes, pid)
	}
	clear(source.failedProbe)
}

func neteaseMusicProcessIDs() ([]uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, err
	}
	var pids []uint32
	for {
		if strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), "cloudmusic.exe") {
			pids = append(pids, entry.ProcessID)
			if len(pids) > neteaseMemoryMaximumProcesses {
				return pids, nil
			}
		}
		err = windows.Process32Next(snapshot, &entry)
		if errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
			return pids, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func openNeteaseProcessProbe(ctx context.Context, pid uint32) (*neteaseProcessProbe, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, context.Canceled
	}
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return nil, errors.New("网易云 3.x 内存适配器只支持 Windows x64")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return nil, err
	}
	playerMatch, scheduleMatch, err := findNeteasePlaybackPatternAddresses(ctx, handle, pid)
	if err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	playerPointer, err := resolveNeteaseRIPPointer(handle, playerMatch, 3)
	if err != nil {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("解析网易云 AudioPlayer 指针: %w", err)
	}
	schedulePointer, err := resolveNeteaseRIPPointer(handle, scheduleMatch, 4)
	if err != nil {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("解析网易云 Schedule 指针: %w", err)
	}
	return &neteaseProcessProbe{
		handle: handle, audioPlayerPointer: playerPointer, schedulePointer: schedulePointer,
	}, nil
}

func (probe *neteaseProcessProbe) close() {
	if probe != nil && probe.handle != 0 {
		_ = windows.CloseHandle(probe.handle)
		probe.handle = 0
		probe.audioPlayerPointer = 0
		probe.schedulePointer = 0
	}
}

func (probe *neteaseProcessProbe) readEvidence(ctx context.Context) (neteaseMemoryEvidence, error) {
	if probe == nil || probe.handle == 0 || probe.audioPlayerPointer == 0 || probe.schedulePointer == 0 {
		return neteaseMemoryEvidence{}, errors.New("网易云内存探针未初始化")
	}
	first, err := probe.readEvidenceOnce(ctx)
	if err != nil {
		return neteaseMemoryEvidence{}, err
	}
	second, err := probe.readEvidenceOnce(ctx)
	if err != nil {
		return neteaseMemoryEvidence{}, err
	}
	if first.Status == 0 && second.Status == 0 && first.TrackID == "" && second.TrackID == "" {
		return neteaseMemoryEvidence{}, errNeteaseNoPlaybackEvidence
	}
	if first.Status != second.Status || first.TrackID != second.TrackID ||
		first.Duration != second.Duration || first.Status < 1 || first.Status > 2 ||
		!validPlatformTrackID("netease", second.TrackID) {
		return neteaseMemoryEvidence{}, errNeteaseTransientPlaybackEvidence
	}
	positionDelta := second.Position - first.Position
	if first.Status == 1 {
		if positionDelta < -100*time.Millisecond || positionDelta > neteaseMemoryStableReadSlack {
			return neteaseMemoryEvidence{}, errNeteaseTransientPlaybackEvidence
		}
	} else if positionDelta < -250*time.Millisecond || positionDelta > 250*time.Millisecond {
		return neteaseMemoryEvidence{}, errNeteaseTransientPlaybackEvidence
	}
	second.SampledAt = time.Now()
	return second, nil
}

func (probe *neteaseProcessProbe) readEvidenceOnce(ctx context.Context) (neteaseMemoryEvidence, error) {
	read := func(address uintptr, target []byte) error {
		if ctx == nil || ctx.Err() != nil {
			return context.Canceled
		}
		return readNeteaseProcessBytes(probe.handle, address, target)
	}
	statusRaw := make([]byte, 4)
	if err := read(probe.audioPlayerPointer+0x60, statusRaw); err != nil {
		return neteaseMemoryEvidence{}, err
	}
	status := int32(binary.LittleEndian.Uint32(statusRaw))
	if status == 0 {
		return neteaseMemoryEvidence{Status: status}, nil
	}
	if status < 1 || status > 2 {
		return neteaseMemoryEvidence{}, fmt.Errorf("%w: 播放状态=%d", errNeteaseTransientPlaybackEvidence, status)
	}

	pointerRaw := make([]byte, 8)
	if err := read(probe.audioPlayerPointer+0x50, pointerRaw); err != nil {
		return neteaseMemoryEvidence{}, err
	}
	audioPlayInfo := uintptr(binary.LittleEndian.Uint64(pointerRaw))
	if audioPlayInfo == 0 || audioPlayInfo > ^uintptr(0)-0x10 {
		return neteaseMemoryEvidence{}, fmt.Errorf("%w: AudioPlayInfo 指针无效", errNeteaseTransientPlaybackEvidence)
	}
	stringRaw := make([]byte, 0x20)
	if err := read(audioPlayInfo+0x10, stringRaw); err != nil {
		return neteaseMemoryEvidence{}, err
	}
	trackID, err := decodeNeteaseMemoryTrackID(stringRaw, read)
	if err != nil {
		return neteaseMemoryEvidence{}, fmt.Errorf("%w: 当前曲目字符串无效: %v", errNeteaseTransientPlaybackEvidence, err)
	}

	durationRaw := make([]byte, 8)
	positionRaw := make([]byte, 8)
	if err := read(probe.audioPlayerPointer+0xa8, durationRaw); err != nil {
		return neteaseMemoryEvidence{}, err
	}
	if err := read(probe.schedulePointer, positionRaw); err != nil {
		return neteaseMemoryEvidence{}, err
	}
	durationSeconds := math.Float64frombits(binary.LittleEndian.Uint64(durationRaw))
	positionSeconds := math.Float64frombits(binary.LittleEndian.Uint64(positionRaw))
	if math.IsNaN(durationSeconds) || math.IsInf(durationSeconds, 0) || durationSeconds <= 0 ||
		durationSeconds > neteaseMemoryMaximumDuration.Seconds() || math.IsNaN(positionSeconds) ||
		math.IsInf(positionSeconds, 0) || positionSeconds < 0 ||
		positionSeconds > durationSeconds+neteaseMemoryPositionSlack.Seconds() {
		return neteaseMemoryEvidence{}, fmt.Errorf("%w: 时间轴无效 duration=%g position=%g",
			errNeteaseTransientPlaybackEvidence, durationSeconds, positionSeconds)
	}
	return neteaseMemoryEvidence{
		TrackID: trackID, Status: status,
		Duration: time.Duration(math.Round(durationSeconds * float64(time.Second))),
		Position: time.Duration(math.Round(positionSeconds * float64(time.Second))),
	}, nil
}

func decodeNeteaseMemoryTrackID(raw []byte, read func(uintptr, []byte) error) (string, error) {
	if len(raw) != 0x20 || read == nil {
		return "", errors.New("网易云 std::string 结构长度无效")
	}
	length := binary.LittleEndian.Uint64(raw[0x10:0x18])
	capacity := binary.LittleEndian.Uint64(raw[0x18:0x20])
	if length == 0 || length > neteaseMemoryMaximumString || capacity < length {
		return "", errors.New("网易云 std::string 长度或容量无效")
	}
	var data []byte
	if length <= 15 {
		data = append([]byte(nil), raw[:int(length)]...)
	} else {
		address64 := binary.LittleEndian.Uint64(raw[:8])
		if address64 == 0 || uint64(uintptr(address64)) != address64 {
			return "", errors.New("网易云 std::string 指针无效")
		}
		data = make([]byte, int(length))
		if err := read(uintptr(address64), data); err != nil {
			return "", err
		}
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", errors.New("网易云 std::string 不是有效 UTF-8")
	}
	value := string(data)
	separator := strings.IndexByte(value, '_')
	if separator <= 0 || separator == len(value)-1 {
		return "", errors.New("网易云当前播放身份缺少分隔符")
	}
	trackID := value[:separator]
	if !validPlatformTrackID("netease", trackID) {
		return "", errors.New("网易云当前播放身份不是有效数字 ID")
	}
	// 下划线后的内容是客户端内部播放实例标识，不是歌曲身份。
	// 不解释其格式，只以经过严格数字校验的前缀作为网易云歌曲 ID。
	return trackID, nil
}

func findNeteasePlaybackPatternAddresses(ctx context.Context, handle windows.Handle,
	pid uint32) (uintptr, uintptr, error) {
	base, size, err := neteaseCloudMusicModule(pid)
	if err != nil {
		return 0, 0, err
	}
	end, err := neteaseModuleScanEnd(base, size)
	if err != nil {
		return 0, 0, err
	}
	ranges, err := neteaseReadableModuleRanges(ctx, handle, base, end)
	if err != nil {
		return 0, 0, err
	}
	matches, err := scanNeteaseMemoryRangesWithReader(ctx, ranges,
		func(address uintptr, target []byte) error {
			return readNeteaseProcessBytes(handle, address, target)
		})
	if err != nil {
		return 0, 0, err
	}
	if len(matches.player) != 1 || len(matches.schedule) != 1 {
		return 0, 0, fmt.Errorf("网易云播放器特征码数量无效: player=%d schedule=%d",
			len(matches.player), len(matches.schedule))
	}
	return matches.player[0], matches.schedule[0], nil
}

func neteaseCloudMusicModule(pid uint32) (uintptr, uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPMODULE|windows.TH32CS_SNAPMODULE32, pid)
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ModuleEntry32{Size: uint32(unsafe.Sizeof(windows.ModuleEntry32{}))}
	if err := windows.Module32First(snapshot, &entry); err != nil {
		return 0, 0, err
	}
	for {
		if strings.EqualFold(windows.UTF16ToString(entry.Module[:]), "cloudmusic.dll") {
			return entry.ModBaseAddr, entry.ModBaseSize, nil
		}
		err = windows.Module32Next(snapshot, &entry)
		if errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
			return 0, 0, errors.New("cloudmusic.dll 未加载")
		}
		if err != nil {
			return 0, 0, err
		}
	}
}

func neteaseModuleScanEnd(base uintptr, size uint32) (uintptr, error) {
	if size == 0 {
		return 0, errors.New("cloudmusic.dll 大小为 0")
	}
	if size > neteaseMemoryMaximumScanBytes {
		return 0, fmt.Errorf("cloudmusic.dll 大小 %d 超过完整扫描上限 %d",
			size, neteaseMemoryMaximumScanBytes)
	}
	end := base + uintptr(size)
	if end < base {
		return 0, errors.New("cloudmusic.dll 地址溢出")
	}
	return end, nil
}

func neteaseReadableModuleRanges(ctx context.Context, handle windows.Handle,
	base, end uintptr) ([]neteaseMemoryRange, error) {
	if ctx == nil || ctx.Err() != nil || handle == 0 || base == 0 || end <= base {
		return nil, context.Canceled
	}
	var ranges []neteaseMemoryRange
	for cursor := base; cursor < end; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var information windows.MemoryBasicInformation
		if err := windows.VirtualQueryEx(handle, cursor, &information, unsafe.Sizeof(information)); err != nil {
			return nil, err
		}
		regionEnd := information.BaseAddress + information.RegionSize
		if regionEnd <= cursor {
			return nil, errors.New("cloudmusic.dll 内存区域没有前进")
		}
		readStart := cursor
		if information.BaseAddress > readStart {
			readStart = information.BaseAddress
		}
		readEnd := regionEnd
		if readEnd > end {
			readEnd = end
		}
		if information.State == windows.MEM_COMMIT && information.Protect&windows.PAGE_GUARD == 0 &&
			qqMemoryProtectionReadable(information.Protect) {
			ranges = append(ranges, neteaseMemoryRange{start: readStart, end: readEnd})
		}
		cursor = regionEnd
	}
	return ranges, nil
}

func scanNeteaseMemoryRangesWithReader(ctx context.Context, ranges []neteaseMemoryRange,
	read func(uintptr, []byte) error) (neteasePatternMatches, error) {
	if ctx == nil || ctx.Err() != nil {
		return neteasePatternMatches{}, context.Canceled
	}
	if read == nil {
		return neteasePatternMatches{}, errors.New("cloudmusic.dll 扫描读取器为空")
	}
	playerMatches := make(map[uintptr]struct{})
	scheduleMatches := make(map[uintptr]struct{})
	var group neteaseMemoryRange
	flush := func() error {
		if group.start >= group.end {
			return nil
		}
		matches, err := scanNeteaseMemoryRegionWithReader(ctx, group.start, group.end, read)
		if err != nil {
			return err
		}
		for _, address := range matches.player {
			playerMatches[address] = struct{}{}
		}
		for _, address := range matches.schedule {
			scheduleMatches[address] = struct{}{}
		}
		group = neteaseMemoryRange{}
		return nil
	}
	for _, current := range ranges {
		if current.start >= current.end {
			return neteasePatternMatches{}, errors.New("cloudmusic.dll 可读区间无效")
		}
		if group.start == 0 && group.end == 0 {
			group = current
			continue
		}
		if current.start < group.end {
			return neteasePatternMatches{}, errors.New("cloudmusic.dll 可读区间重叠或乱序")
		}
		if current.start == group.end {
			group.end = current.end
			continue
		}
		if err := flush(); err != nil {
			return neteasePatternMatches{}, err
		}
		group = current
	}
	if err := flush(); err != nil {
		return neteasePatternMatches{}, err
	}
	result := neteasePatternMatches{
		player: make([]uintptr, 0, len(playerMatches)), schedule: make([]uintptr, 0, len(scheduleMatches)),
	}
	for address := range playerMatches {
		result.player = append(result.player, address)
	}
	for address := range scheduleMatches {
		result.schedule = append(result.schedule, address)
	}
	return result, nil
}

func scanNeteaseMemoryRegionWithReader(ctx context.Context, start, end uintptr,
	read func(uintptr, []byte) error) (neteasePatternMatches, error) {
	if ctx == nil || ctx.Err() != nil {
		return neteasePatternMatches{}, context.Canceled
	}
	if start >= end {
		return neteasePatternMatches{}, nil
	}
	maximumPatternLength := len(neteaseAudioPlayerPattern)
	if len(neteaseAudioSchedulePattern) > maximumPatternLength {
		maximumPatternLength = len(neteaseAudioSchedulePattern)
	}
	overlap := maximumPatternLength - 1
	var carry []byte
	playerMatches := make(map[uintptr]struct{})
	scheduleMatches := make(map[uintptr]struct{})
	for cursor := start; cursor < end; {
		if err := ctx.Err(); err != nil {
			return neteasePatternMatches{}, err
		}
		remaining := end - cursor
		chunkSize := uintptr(neteaseMemoryScanChunkBytes)
		if remaining < chunkSize {
			chunkSize = remaining
		}
		chunk := make([]byte, int(chunkSize))
		if err := read(cursor, chunk); err != nil {
			return neteasePatternMatches{}, fmt.Errorf("完整扫描 cloudmusic.dll 可读区域: %w", err)
		}
		combined := make([]byte, 0, len(carry)+len(chunk))
		combined = append(combined, carry...)
		combined = append(combined, chunk...)
		combinedBase := cursor - uintptr(len(carry))
		for _, offset := range findNeteasePatternOffsets(combined, neteaseAudioPlayerPattern) {
			playerMatches[combinedBase+uintptr(offset)] = struct{}{}
		}
		for _, offset := range findNeteasePatternOffsets(combined, neteaseAudioSchedulePattern) {
			scheduleMatches[combinedBase+uintptr(offset)] = struct{}{}
		}
		if len(combined) > overlap {
			carry = append(carry[:0], combined[len(combined)-overlap:]...)
		} else {
			carry = append(carry[:0], combined...)
		}
		cursor += chunkSize
	}
	result := neteasePatternMatches{
		player: make([]uintptr, 0, len(playerMatches)), schedule: make([]uintptr, 0, len(scheduleMatches)),
	}
	for address := range playerMatches {
		result.player = append(result.player, address)
	}
	for address := range scheduleMatches {
		result.schedule = append(result.schedule, address)
	}
	return result, nil
}

func findNeteasePatternOffsets(data []byte, pattern []int16) []int {
	if len(pattern) == 0 || len(data) < len(pattern) {
		return nil
	}
	anchor := -1
	for index, wanted := range pattern {
		if wanted >= 0 {
			anchor = index
			break
		}
	}
	if anchor < 0 {
		return nil
	}
	var offsets []int
	searchFrom := anchor
	for searchFrom < len(data) {
		relative := bytes.IndexByte(data[searchFrom:], byte(pattern[anchor]))
		if relative < 0 {
			break
		}
		candidateAnchor := searchFrom + relative
		offset := candidateAnchor - anchor
		searchFrom = candidateAnchor + 1
		if offset < 0 || offset+len(pattern) > len(data) {
			continue
		}
		matched := true
		for index, wanted := range pattern {
			if wanted >= 0 && data[offset+index] != byte(wanted) {
				matched = false
				break
			}
		}
		if matched {
			offsets = append(offsets, offset)
		}
	}
	return offsets
}

func resolveNeteaseRIPPointer(handle windows.Handle, match uintptr,
	displacementOffset uintptr) (uintptr, error) {
	if handle == 0 || match == 0 || match > ^uintptr(0)-displacementOffset-4 {
		return 0, errors.New("RIP 相对指针参数无效")
	}
	fieldAddress := match + displacementOffset
	raw := make([]byte, 4)
	if err := readNeteaseProcessBytes(handle, fieldAddress, raw); err != nil {
		return 0, err
	}
	base := uint64(fieldAddress + 4)
	displacement := int64(int32(binary.LittleEndian.Uint32(raw)))
	var resolved uint64
	if displacement < 0 {
		magnitude := uint64(-displacement)
		if magnitude >= base {
			return 0, errors.New("RIP 相对指针下溢")
		}
		resolved = base - magnitude
	} else {
		if uint64(displacement) > ^uint64(0)-base {
			return 0, errors.New("RIP 相对指针上溢")
		}
		resolved = base + uint64(displacement)
	}
	if resolved == 0 || uint64(uintptr(resolved)) != resolved {
		return 0, errors.New("RIP 相对指针结果无效")
	}
	return uintptr(resolved), nil
}

func readNeteaseProcessBytes(handle windows.Handle, address uintptr, target []byte) error {
	if handle == 0 || address == 0 || len(target) == 0 {
		return errors.New("网易云内存读取参数无效")
	}
	var read uintptr
	if err := windows.ReadProcessMemory(handle, address, &target[0], uintptr(len(target)), &read); err != nil {
		return err
	}
	if read != uintptr(len(target)) {
		return fmt.Errorf("网易云内存短读: %d/%d", read, len(target))
	}
	return nil
}
