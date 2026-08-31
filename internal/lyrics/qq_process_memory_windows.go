//go:build windows

package lyrics

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	qqMemoryMaximumProcesses  = 8
	qqMemoryMaximumScanBytes  = 20 << 20
	qqMemoryScanChunkBytes    = 1 << 20
	qqMemoryMaximumStringSize = 4096
	qqMemorySongInfoSize      = 0x74
	qqMemoryDurationSlack     = 2 * time.Second
	qqMemoryPositionSlack     = 5 * time.Second
	qqMemoryFailedProbeRetry  = 30 * time.Second
)

// 该特征码及 20.43+ CurrentSongInfo 布局来自 Kxnrl 的原始 MIT 项目：
// https://github.com/Kxnrl/NetEase-Cloud-Music-DiscordRPC/blob/3a97a4b5c8b673ce2f69b0205adbc6a3ac861ba5/Vanessa/Players/Tencent.cs
// CaseScreen 只使用 PROCESS_VM_READ，不向 QQMusic 写内存、不注入 DLL。
var qqCurrentSongInfoPattern = []int16{
	0xA2, -1, -1, -1, -1, 0xA3, -1, -1, -1, -1,
	0xC7, 0x05, -1, -1, -1, -1, -1, -1, -1, -1,
	0xA2, -1, -1, -1, -1, 0xA3, -1, -1, -1, -1,
	0xC7, 0x05, -1, -1, -1, -1, -1, -1, -1, -1,
	0xA2, -1, -1, -1, -1, 0xA3,
}

type qqNativeIDSource interface {
	CurrentSongID(context.Context, NowPlaying) (uint32, bool)
}

type qqMemoryEvidence struct {
	SongID   uint32
	Title    string
	Artist   string
	Album    string
	Duration time.Duration
	Position time.Duration
	Playing  bool
	// PlayStatusKnown 只对上游定义的 0（暂停）和 1（播放）为真；
	// 3（缓冲）是合法瞬态，但不能据此确认与 SMTC 状态一致。
	PlayStatusKnown bool
}

type qqProcessProbe struct {
	handle        windows.Handle
	structAddress uintptr
}

type qqMemoryRange struct {
	start uintptr
	end   uintptr
}

type qqProcessMemorySource struct {
	mu          sync.Mutex
	probes      map[uint32]*qqProcessProbe
	failedProbe map[uint32]time.Time
	// 以下函数只允许单测注入。生产实例固定使用只读 Windows 实现；
	// 把整轮候选检查留在 CurrentSongID 内，避免 fake 绕过失败关闭判断。
	processIDs   func() ([]uint32, error)
	openProbe    func(context.Context, uint32) (*qqProcessProbe, error)
	readEvidence func(context.Context, *qqProcessProbe) (qqMemoryEvidence, error)
	now          func() time.Time
}

func newQQProcessMemorySource() *qqProcessMemorySource {
	return &qqProcessMemorySource{
		probes:      make(map[uint32]*qqProcessProbe),
		failedProbe: make(map[uint32]time.Time),
		processIDs:  qqMusicProcessIDs,
		openProbe:   openQQProcessProbe,
		readEvidence: func(ctx context.Context, probe *qqProcessProbe) (qqMemoryEvidence, error) {
			return probe.readEvidence(ctx)
		},
		now: time.Now,
	}
}

// enrichQQPlatformTrackIDFromSources 从 CaseScreen 内置只读内存适配器取得
// 当前 songId，再用 QQ 单曲详情进行第二次完整交叉验证。正式路径不接受
// SMTC Genres 注入标记，也不依赖 companion、DLL 注入或外部桥。
func enrichQQPlatformTrackIDFromSources(ctx context.Context, track NowPlaying, native qqNativeIDSource,
	resolver qqSongMIDResolver) NowPlaying {
	if sourceProviderID(track.Source) != "qq" {
		return track
	}
	// 不信任任何上游预填的 TrackID/QQSongID；正式链路必须在
	// 同一轮由内置只读探针重新取得并核对，结构上彻底排除
	// companion 注入、上一轮缓存或外部桥预填的捷径。
	track.TrackID = ""
	track.TrackIDOrigin = ""
	track.QQSongID = 0
	if native == nil {
		return track
	}
	songID, ok := native.CurrentSongID(ctx, track)
	if !ok || songID == 0 {
		return track
	}
	track.QQSongID = songID
	return enrichQQPlatformTrackID(ctx, track, resolver)
}

// CurrentSongID 返回与同一轮 QQ SMTC 元数据唯一匹配的数值 songId。
// 任一同名进程无法完整检查、多个不同 ID、字段不一致、时间轴未知或
// 读取失败时都不返回。
func (source *qqProcessMemorySource) CurrentSongID(ctx context.Context, current NowPlaying) (uint32, bool) {
	if source == nil || ctx == nil || ctx.Err() != nil || sourceProviderID(current.Source) != "qq" || !current.PositionKnown {
		return 0, false
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	processIDs := source.processIDs
	if processIDs == nil {
		processIDs = qqMusicProcessIDs
	}
	openProbe := source.openProbe
	if openProbe == nil {
		openProbe = openQQProcessProbe
	}
	readEvidence := source.readEvidence
	if readEvidence == nil {
		readEvidence = func(ctx context.Context, probe *qqProcessProbe) (qqMemoryEvidence, error) {
			return probe.readEvidence(ctx)
		}
	}
	nowFunction := source.now
	if nowFunction == nil {
		nowFunction = time.Now
	}
	pids, err := processIDs()
	if err != nil {
		return 0, false
	}
	if len(pids) == 0 || len(pids) > qqMemoryMaximumProcesses {
		// QQ 已退出或出现异常多的同名进程时立即释放所有句柄；
		// 后者也失败关闭，避免在不可判定的进程集合中猜测。
		source.closeAllLocked()
		return 0, false
	}
	alive := make(map[uint32]struct{}, len(pids))
	matched := make(map[uint32]struct{})
	now := nowFunction()
	allCandidatesVerified := true
	allEvidenceConsistent := true
	for _, pid := range pids {
		if ctx.Err() != nil {
			return 0, false
		}
		alive[pid] = struct{}{}
		probe := source.probes[pid]
		if probe == nil {
			if failedAt, failed := source.failedProbe[pid]; failed && now.Sub(failedAt) >= 0 &&
				now.Sub(failedAt) < qqMemoryFailedProbeRetry {
				// 负缓存只能减少重复扫描，不能把这个候选当作“不存在”。
				// 只要它仍在进程快照中，本轮就必须失败关闭。
				allCandidatesVerified = false
				continue
			}
			probe, err = openProbe(ctx, pid)
			if err != nil {
				source.failedProbe[pid] = now
				allCandidatesVerified = false
				continue
			}
			source.probes[pid] = probe
			delete(source.failedProbe, pid)
		}
		evidence, readErr := readEvidence(ctx, probe)
		if readErr != nil {
			// 客户端更新或进程重启后地址可能失效；本轮清理，下一轮
			// 重新扫描，绝不沿用旧 songId。这里不写入负缓存：切歌时
			// std::string 恰好更新造成的瞬时短读不应等待 30 秒。
			probe.close()
			delete(source.probes, pid)
			allCandidatesVerified = false
			continue
		}
		if !qqMemoryEvidenceMatchesCurrent(evidence, current, now) {
			// 同名且已通过模块/结构读取的进程给出不一致证据时，
			// 也不能忽略它并接受另一个进程的唯一匹配。
			allEvidenceConsistent = false
			continue
		}
		matched[evidence.SongID] = struct{}{}
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
	if !allCandidatesVerified || !allEvidenceConsistent || len(matched) != 1 {
		return 0, false
	}
	for songID := range matched {
		return songID, songID != 0
	}
	return 0, false
}

func (source *qqProcessMemorySource) closeAll() {
	if source == nil {
		return
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	source.closeAllLocked()
}

func (source *qqProcessMemorySource) closeAllLocked() {
	for pid, probe := range source.probes {
		probe.close()
		delete(source.probes, pid)
	}
	clear(source.failedProbe)
}

func qqMemoryEvidenceMatchesCurrent(evidence qqMemoryEvidence, current NowPlaying, now time.Time) bool {
	if evidence.SongID == 0 || sourceProviderID(current.Source) != "qq" ||
		!current.PositionKnown || normalizeText(evidence.Title) == "" ||
		normalizeText(evidence.Title) != normalizeText(current.Title) ||
		normalizeText(evidence.Artist) == "" ||
		normalizeText(evidence.Artist) != normalizeText(current.Artist) ||
		normalizeText(evidence.Album) == "" || normalizeText(current.Album) == "" ||
		normalizeText(evidence.Album) != normalizeText(current.Album) ||
		evidence.Duration <= 0 || current.Duration <= 0 || !evidence.PlayStatusKnown ||
		evidence.Playing != current.Playing {
		return false
	}
	durationDifference := evidence.Duration - time.Duration(current.Duration)
	if durationDifference < 0 {
		durationDifference = -durationDifference
	}
	if durationDifference > qqMemoryDurationSlack || evidence.Position < 0 ||
		evidence.Position > evidence.Duration+qqMemoryPositionSlack {
		return false
	}
	positionDifference := evidence.Position - playbackPositionAt(current, now)
	if positionDifference < 0 {
		positionDifference = -positionDifference
	}
	return positionDifference <= qqMemoryPositionSlack
}

func qqMusicProcessIDs() ([]uint32, error) {
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
		if strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), "QQMusic.exe") {
			pids = append(pids, entry.ProcessID)
			if len(pids) > qqMemoryMaximumProcesses {
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

func openQQProcessProbe(ctx context.Context, pid uint32) (*qqProcessProbe, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, context.Canceled
	}
	// VirtualQueryEx 按微软文档要求 PROCESS_QUERY_INFORMATION，
	// ReadProcessMemory 只再增加 PROCESS_VM_READ；不请求任何写入、
	// 远程线程、暂停、终止或调试权限。
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return nil, err
	}
	address, err := findQQCurrentSongInfoAddress(ctx, handle, pid)
	if err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	return &qqProcessProbe{handle: handle, structAddress: address}, nil
}

func (probe *qqProcessProbe) close() {
	if probe != nil && probe.handle != 0 {
		_ = windows.CloseHandle(probe.handle)
		probe.handle = 0
		probe.structAddress = 0
	}
}

func (probe *qqProcessProbe) readEvidence(ctx context.Context) (qqMemoryEvidence, error) {
	if probe == nil || probe.handle == 0 || probe.structAddress == 0 {
		return qqMemoryEvidence{}, errors.New("QQ 音乐内存探针未初始化")
	}
	read := func(address uintptr, target []byte) error {
		if ctx == nil || ctx.Err() != nil {
			return context.Canceled
		}
		return readQQProcessBytes(probe.handle, address, target)
	}
	raw := make([]byte, qqMemorySongInfoSize)
	if err := read(probe.structAddress, raw); err != nil {
		return qqMemoryEvidence{}, err
	}
	return decodeQQMemoryEvidence(raw, read)
}

func decodeQQMemoryEvidence(raw []byte, read func(uintptr, []byte) error) (qqMemoryEvidence, error) {
	if len(raw) != qqMemorySongInfoSize || read == nil {
		return qqMemoryEvidence{}, errors.New("QQ 音乐当前曲目结构长度无效")
	}
	title, err := decodeQQMemoryString(raw[0x00:0x18], read)
	if err != nil {
		return qqMemoryEvidence{}, fmt.Errorf("读取 QQ 曲名: %w", err)
	}
	artist, err := decodeQQMemoryString(raw[0x18:0x30], read)
	if err != nil {
		return qqMemoryEvidence{}, fmt.Errorf("读取 QQ 歌手: %w", err)
	}
	album, err := decodeQQMemoryString(raw[0x30:0x48], read)
	if err != nil {
		return qqMemoryEvidence{}, fmt.Errorf("读取 QQ 专辑: %w", err)
	}
	songID := binary.LittleEndian.Uint32(raw[0x60:0x64])
	durationMillis := binary.LittleEndian.Uint32(raw[0x68:0x6c])
	positionMillis := binary.LittleEndian.Uint32(raw[0x6c:0x70])
	playStatus := binary.LittleEndian.Uint32(raw[0x70:0x74])
	if songID == 0 || durationMillis == 0 || (playStatus != 0 && playStatus != 1 && playStatus != 3) {
		return qqMemoryEvidence{}, errors.New("QQ 音乐当前曲目 ID、时长或播放状态无效")
	}
	return qqMemoryEvidence{
		SongID: songID, Title: strings.TrimSpace(title), Artist: strings.TrimSpace(artist),
		Album: strings.TrimSpace(album), Duration: time.Duration(durationMillis) * time.Millisecond,
		Position: time.Duration(positionMillis) * time.Millisecond, Playing: playStatus == 1,
		PlayStatusKnown: playStatus == 0 || playStatus == 1,
	}, nil
}

func decodeQQMemoryString(raw []byte, read func(uintptr, []byte) error) (string, error) {
	if len(raw) != 0x18 {
		return "", errors.New("std::string 结构长度无效")
	}
	length := binary.LittleEndian.Uint32(raw[0x10:0x14])
	capacity := binary.LittleEndian.Uint32(raw[0x14:0x18])
	if length > qqMemoryMaximumStringSize || capacity < length {
		return "", errors.New("std::string 长度或容量无效")
	}
	if length == 0 {
		return "", nil
	}
	var data []byte
	if capacity < 16 {
		if length > 15 {
			return "", errors.New("std::string 短字符串长度无效")
		}
		data = append([]byte(nil), raw[:length]...)
	} else {
		address := uintptr(binary.LittleEndian.Uint32(raw[:4]))
		if address == 0 {
			return "", errors.New("std::string 指针为空")
		}
		data = make([]byte, length)
		if err := read(address, data); err != nil {
			return "", err
		}
	}
	if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
		return "", errors.New("std::string 不是有效 UTF-8")
	}
	return string(data), nil
}

func findQQCurrentSongInfoAddress(ctx context.Context, handle windows.Handle, pid uint32) (uintptr, error) {
	if ctx == nil || ctx.Err() != nil {
		return 0, context.Canceled
	}
	base, size, err := qqMusicModule(pid)
	if err != nil {
		return 0, err
	}
	end, err := qqModuleScanEnd(base, size)
	if err != nil {
		return 0, err
	}
	readableRanges := make([]qqMemoryRange, 0, 8)
	for cursor := base; cursor < end; {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var information windows.MemoryBasicInformation
		if err := windows.VirtualQueryEx(handle, cursor, &information, unsafe.Sizeof(information)); err != nil {
			return 0, err
		}
		regionEnd := information.BaseAddress + information.RegionSize
		if regionEnd <= cursor {
			return 0, errors.New("QQMusic.dll 内存区域没有前进")
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
			readableRanges = append(readableRanges, qqMemoryRange{start: readStart, end: readEnd})
		}
		cursor = regionEnd
	}
	regionAddresses, err := scanQQMemoryRangesWithReader(ctx, readableRanges,
		func(address uintptr, target []byte) error {
			return readQQProcessBytes(handle, address, target)
		})
	if err != nil {
		// 无法完整检查标记为可读的模块区域时，不能证明特征码唯一。
		return 0, err
	}
	addresses := make(map[uintptr]struct{}, len(regionAddresses))
	for _, address := range regionAddresses {
		addresses[address] = struct{}{}
	}
	if len(addresses) != 1 {
		return 0, fmt.Errorf("QQMusic 当前曲目特征码数量不是 1: %d", len(addresses))
	}
	for address := range addresses {
		return address, nil
	}
	return 0, errors.New("QQMusic 当前曲目特征码不存在")
}

func qqModuleScanEnd(base uintptr, size uint32) (uintptr, error) {
	if size == 0 {
		return 0, errors.New("QQMusic.dll 大小为 0")
	}
	if size > qqMemoryMaximumScanBytes {
		// 扫描预算只用于限制资源消耗，不能把超大模块截断后仍把前缀中的
		// 一个特征码当作全模块唯一。无法完整扫描时必须失败关闭。
		return 0, fmt.Errorf("QQMusic.dll 大小 %d 超过完整扫描上限 %d", size, qqMemoryMaximumScanBytes)
	}
	end := base + uintptr(size)
	if end < base {
		return 0, errors.New("QQMusic.dll 地址溢出")
	}
	return end, nil
}

func qqMemoryProtectionReadable(protection uint32) bool {
	base := protection & 0xff
	return base == windows.PAGE_READONLY || base == windows.PAGE_READWRITE ||
		base == windows.PAGE_WRITECOPY || base == windows.PAGE_EXECUTE_READ ||
		base == windows.PAGE_EXECUTE_READWRITE || base == windows.PAGE_EXECUTE_WRITECOPY
}

func scanQQMemoryRegion(ctx context.Context, handle windows.Handle, start, end uintptr) ([]uintptr, error) {
	return scanQQMemoryRegionWithReader(ctx, start, end, func(address uintptr, target []byte) error {
		return readQQProcessBytes(handle, address, target)
	})
}

// scanQQMemoryRangesWithReader 合并相邻的可读区间后再扫描，确保特征码即使
// 跨两个不同保护属性的连续 VirtualQueryEx region，也会参与唯一性判断。
func scanQQMemoryRangesWithReader(ctx context.Context, ranges []qqMemoryRange,
	read func(uintptr, []byte) error) ([]uintptr, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, context.Canceled
	}
	if read == nil {
		return nil, errors.New("QQMusic.dll 扫描读取器为空")
	}
	addresses := make(map[uintptr]struct{})
	var group qqMemoryRange
	flush := func() error {
		if group.start >= group.end {
			return nil
		}
		found, err := scanQQMemoryRegionWithReader(ctx, group.start, group.end, read)
		if err != nil {
			return err
		}
		for _, address := range found {
			addresses[address] = struct{}{}
		}
		group = qqMemoryRange{}
		return nil
	}
	for _, current := range ranges {
		if current.start >= current.end {
			return nil, errors.New("QQMusic.dll 可读区间无效")
		}
		if group.start == 0 && group.end == 0 {
			group = current
			continue
		}
		if current.start < group.end {
			return nil, errors.New("QQMusic.dll 可读区间重叠或乱序")
		}
		if current.start == group.end {
			group.end = current.end
			continue
		}
		if err := flush(); err != nil {
			return nil, err
		}
		group = current
	}
	if err := flush(); err != nil {
		return nil, err
	}
	result := make([]uintptr, 0, len(addresses))
	for address := range addresses {
		result = append(result, address)
	}
	return result, nil
}

func scanQQMemoryRegionWithReader(ctx context.Context, start, end uintptr,
	read func(uintptr, []byte) error) ([]uintptr, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, context.Canceled
	}
	if start >= end {
		return nil, nil
	}
	if read == nil {
		return nil, errors.New("QQMusic.dll 扫描读取器为空")
	}
	overlap := len(qqCurrentSongInfoPattern) - 1
	var carry []byte
	addresses := make(map[uintptr]struct{})
	for cursor := start; cursor < end; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining := end - cursor
		chunkSize := uintptr(qqMemoryScanChunkBytes)
		if remaining < chunkSize {
			chunkSize = remaining
		}
		chunk := make([]byte, int(chunkSize))
		if err := read(cursor, chunk); err != nil {
			return nil, fmt.Errorf("完整扫描 QQMusic.dll 可读区域: %w", err)
		}
		combined := make([]byte, 0, len(carry)+len(chunk))
		combined = append(combined, carry...)
		combined = append(combined, chunk...)
		for _, offset := range findQQPatternOffsets(combined, qqCurrentSongInfoPattern) {
			pointerOffset := offset + 1
			if pointerOffset+4 > len(combined) {
				continue
			}
			address := uintptr(binary.LittleEndian.Uint32(combined[pointerOffset : pointerOffset+4]))
			if address != 0 {
				addresses[address] = struct{}{}
			}
		}
		if len(combined) > overlap {
			carry = append(carry[:0], combined[len(combined)-overlap:]...)
		} else {
			carry = append(carry[:0], combined...)
		}
		cursor += chunkSize
	}
	result := make([]uintptr, 0, len(addresses))
	for address := range addresses {
		result = append(result, address)
	}
	return result, nil
}

func findQQPatternOffsets(data []byte, pattern []int16) []int {
	if len(pattern) == 0 || len(data) < len(pattern) {
		return nil
	}
	var offsets []int
	for offset := 0; offset <= len(data)-len(pattern); offset++ {
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

func readQQProcessBytes(handle windows.Handle, address uintptr, target []byte) error {
	if handle == 0 || address == 0 || len(target) == 0 {
		return errors.New("QQ 音乐内存读取参数无效")
	}
	var read uintptr
	if err := windows.ReadProcessMemory(handle, address, &target[0], uintptr(len(target)), &read); err != nil {
		return err
	}
	if read != uintptr(len(target)) {
		return fmt.Errorf("QQ 音乐内存短读: %d/%d", read, len(target))
	}
	return nil
}

func qqMusicModule(pid uint32) (uintptr, uint32, error) {
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
		if strings.EqualFold(windows.UTF16ToString(entry.Module[:]), "QQMusic.dll") {
			return entry.ModBaseAddr, entry.ModBaseSize, nil
		}
		err = windows.Module32Next(snapshot, &entry)
		if errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
			return 0, 0, errors.New("QQMusic.dll 未加载")
		}
		if err != nil {
			return 0, 0, err
		}
	}
}
