//go:build windows

package lyrics

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

type fixedQQNativeIDSource struct {
	songID uint32
	ok     bool
	calls  int
}

func (source *fixedQQNativeIDSource) CurrentSongID(context.Context, NowPlaying) (uint32, bool) {
	source.calls++
	return source.songID, source.ok
}

func TestQQProcessMemoryFallbackStillRequiresExactSongDetail(t *testing.T) {
	native := &fixedQQNativeIDSource{songID: 97773, ok: true}
	resolver := &fixedQQSongMIDResolver{detail: qqExactDetail()}
	result := enrichQQPlatformTrackIDFromSources(context.Background(), qqInjectorCurrentTrackWithoutID(), native, resolver)
	if result.SourceTrackID() != "0039MnYb0qxYhV" || result.TrackIDOrigin != TrackIDOriginSource {
		t.Fatalf("只读内存 songId 没有通过精确详情升级为 songmid: %+v", result)
	}
	if native.calls != 1 || resolver.calls != 1 {
		t.Fatalf("只读内存或详情解析调用次数错误: native=%d resolver=%d", native.calls, resolver.calls)
	}

	native.calls = 0
	resolver.calls = 0
	marked := qqInjectorCurrentTrack()
	result = enrichQQPlatformTrackIDFromSources(context.Background(), marked, native, resolver)
	if result.SourceTrackID() == "" || native.calls != 1 || resolver.calls != 1 {
		t.Fatalf("上游预填 ID 绕过了内置只读探针: result=%+v native=%d resolver=%d", result, native.calls, resolver.calls)
	}

	native.calls = 0
	resolver.calls = 0
	prefilled := qqInjectorCurrentTrack()
	prefilled.TrackID = "004Z8Ihr0JIu5s"
	prefilled.TrackIDOrigin = TrackIDOriginSource
	result = enrichQQPlatformTrackIDFromSources(context.Background(), prefilled, native, resolver)
	if result.SourceTrackID() != "0039MnYb0qxYhV" || result.TrackIDOrigin != TrackIDOriginSource ||
		native.calls != 1 || resolver.calls != 1 {
		t.Fatalf("预填 QQ songmid 没有被本轮内置证据覆盖: result=%+v native=%d resolver=%d",
			result, native.calls, resolver.calls)
	}

	native = &fixedQQNativeIDSource{}
	resolver.calls = 0
	result = enrichQQPlatformTrackIDFromSources(context.Background(), prefilled, native, resolver)
	if result.SourceTrackID() != "" || result.TrackIDOrigin != "" || result.QQSongID != 0 ||
		native.calls != 1 || resolver.calls != 0 {
		t.Fatalf("本轮 QQ 原生重验失败后仍沿用预填身份: result=%+v native=%d resolver=%d",
			result, native.calls, resolver.calls)
	}
}

func qqInjectorCurrentTrackWithoutID() NowPlaying {
	track := qqInjectorCurrentTrack()
	track.QQSongID = 0
	return track
}

func TestQQCurrentSongInfoPatternIsStrictAndFindsAbsolutePointer(t *testing.T) {
	data := make([]byte, len(qqCurrentSongInfoPattern)+20)
	offset := 7
	for index, wanted := range qqCurrentSongInfoPattern {
		if wanted >= 0 {
			data[offset+index] = byte(wanted)
		} else {
			data[offset+index] = byte(index + 1)
		}
	}
	binary.LittleEndian.PutUint32(data[offset+1:offset+5], 0x12345678)
	offsets := findQQPatternOffsets(data, qqCurrentSongInfoPattern)
	if len(offsets) != 1 || offsets[0] != offset {
		t.Fatalf("QQ 特征码没有唯一定位: %#v", offsets)
	}
	data[offset+30] ^= 0xff
	if offsets := findQQPatternOffsets(data, qqCurrentSongInfoPattern); len(offsets) != 0 {
		t.Fatalf("损坏的固定字节仍匹配 QQ 特征码: %#v", offsets)
	}
}

func TestDecodeQQMemoryEvidenceSupportsInlineAndHeapStrings(t *testing.T) {
	raw := make([]byte, qqMemorySongInfoSize)
	heap := map[uintptr][]byte{}
	writeQQTestString(t, raw[0x00:0x18], "晴天", 0, heap)
	writeQQTestString(t, raw[0x18:0x30], "周杰伦与测试长歌手名", 0x1000, heap)
	writeQQTestString(t, raw[0x30:0x48], "叶惠美", 0, heap)
	binary.LittleEndian.PutUint32(raw[0x60:0x64], 97773)
	binary.LittleEndian.PutUint32(raw[0x68:0x6c], 269000)
	binary.LittleEndian.PutUint32(raw[0x6c:0x70], 42000)
	binary.LittleEndian.PutUint32(raw[0x70:0x74], 1)
	read := func(address uintptr, target []byte) error {
		value, ok := heap[address]
		if !ok || len(value) != len(target) {
			return errors.New("测试指针不存在")
		}
		copy(target, value)
		return nil
	}
	evidence, err := decodeQQMemoryEvidence(raw, read)
	if err != nil {
		t.Fatalf("解析 QQ 当前曲目结构失败: %v", err)
	}
	if evidence.SongID != 97773 || evidence.Title != "晴天" ||
		evidence.Artist != "周杰伦与测试长歌手名" || evidence.Album != "叶惠美" ||
		evidence.Duration != 269*time.Second || evidence.Position != 42*time.Second || !evidence.Playing ||
		!evidence.PlayStatusKnown {
		t.Fatalf("QQ 当前曲目结构内容错误: %+v", evidence)
	}

	binary.LittleEndian.PutUint32(raw[0x70:0x74], 3)
	buffering, err := decodeQQMemoryEvidence(raw, read)
	if err != nil || buffering.PlayStatusKnown {
		t.Fatalf("缓冲状态应保留探针但不能成为可信播放状态: evidence=%+v err=%v", buffering, err)
	}
}

func writeQQTestString(t *testing.T, raw []byte, value string, address uintptr, heap map[uintptr][]byte) {
	t.Helper()
	data := []byte(value)
	binary.LittleEndian.PutUint32(raw[0x10:0x14], uint32(len(data)))
	if len(data) <= 15 && address == 0 {
		copy(raw, data)
		binary.LittleEndian.PutUint32(raw[0x14:0x18], 15)
		return
	}
	if address == 0 || address > 0xffffffff {
		t.Fatalf("测试长字符串地址无效: %#x", address)
	}
	binary.LittleEndian.PutUint32(raw[:4], uint32(address))
	binary.LittleEndian.PutUint32(raw[0x14:0x18], uint32(len(data)))
	heap[address] = append([]byte(nil), data...)
}

func TestQQMemoryEvidenceRequiresSameSMTCTrackAndTimeline(t *testing.T) {
	now := time.Now()
	current := qqInjectorCurrentTrackWithoutID()
	current.PositionUpdatedAt = now.Add(-500 * time.Millisecond)
	evidence := qqMemoryEvidence{
		SongID: 97773, Title: "晴天", Artist: "周杰伦", Album: "叶惠美",
		Duration: 269 * time.Second, Position: 42500 * time.Millisecond, Playing: true, PlayStatusKnown: true,
	}
	if !qqMemoryEvidenceMatchesCurrent(evidence, current, now) {
		t.Fatal("同一 QQ 曲目与时间轴没有通过只读内存交叉验证")
	}

	mutations := map[string]func(*qqMemoryEvidence, *NowPlaying){
		"zero_id":          func(value *qqMemoryEvidence, _ *NowPlaying) { value.SongID = 0 },
		"title":            func(value *qqMemoryEvidence, _ *NowPlaying) { value.Title = "同名其他歌" },
		"artist":           func(value *qqMemoryEvidence, _ *NowPlaying) { value.Artist = "其他歌手" },
		"album":            func(value *qqMemoryEvidence, _ *NowPlaying) { value.Album = "其他专辑" },
		"duration":         func(value *qqMemoryEvidence, _ *NowPlaying) { value.Duration += 3 * time.Second },
		"position":         func(value *qqMemoryEvidence, _ *NowPlaying) { value.Position += 6 * time.Second },
		"playing":          func(value *qqMemoryEvidence, _ *NowPlaying) { value.Playing = false },
		"unknown_playing":  func(value *qqMemoryEvidence, _ *NowPlaying) { value.PlayStatusKnown = false },
		"unknown_position": func(_ *qqMemoryEvidence, track *NowPlaying) { track.PositionKnown = false },
		"wrong_source":     func(_ *qqMemoryEvidence, track *NowPlaying) { track.Source = "netease" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changedEvidence, changedCurrent := evidence, current
			mutate(&changedEvidence, &changedCurrent)
			if qqMemoryEvidenceMatchesCurrent(changedEvidence, changedCurrent, now) {
				t.Fatal("不一致的 QQ 内存证据没有失败关闭")
			}
		})
	}
}

func TestDecodeQQMemoryStringRejectsUnboundedOrInvalidData(t *testing.T) {
	raw := make([]byte, 0x18)
	binary.LittleEndian.PutUint32(raw[0x10:0x14], qqMemoryMaximumStringSize+1)
	binary.LittleEndian.PutUint32(raw[0x14:0x18], qqMemoryMaximumStringSize+1)
	if _, err := decodeQQMemoryString(raw, func(uintptr, []byte) error { return nil }); err == nil {
		t.Fatal("超长 QQ 字符串没有被拒绝")
	}
	raw = make([]byte, 0x18)
	raw[0], raw[1] = 0xff, 0xfe
	binary.LittleEndian.PutUint32(raw[0x10:0x14], 2)
	binary.LittleEndian.PutUint32(raw[0x14:0x18], 15)
	if _, err := decodeQQMemoryString(raw, func(uintptr, []byte) error { return nil }); err == nil {
		t.Fatal("非法 UTF-8 QQ 字符串没有被拒绝")
	}
}

func TestQQProcessMemorySourceCloseAllClearsCachedState(t *testing.T) {
	source := newQQProcessMemorySource()
	// 句柄保持为 0，单测不会触碰真实进程；这里只验证退出/异常进程数
	// 路径不会把地址或失败退避永久留在全局适配器中。
	source.probes[123] = &qqProcessProbe{structAddress: 0x1234}
	source.failedProbe[456] = time.Now()
	source.closeAll()
	if len(source.probes) != 0 || len(source.failedProbe) != 0 {
		t.Fatalf("QQ 探针缓存没有全部清理: probes=%d failed=%d", len(source.probes), len(source.failedProbe))
	}
}

func TestQQProcessMemoryRejectsWholeRoundWhenAnyCandidateCannotBeVerified(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	current := qqInjectorCurrentTrackWithoutID()
	current.PositionUpdatedAt = now
	matching := qqMemoryEvidence{
		SongID: 97773, Title: "晴天", Artist: "周杰伦", Album: "叶惠美",
		Duration: 269 * time.Second, Position: 42 * time.Second, Playing: true, PlayStatusKnown: true,
	}
	newSource := func() (*qqProcessMemorySource, *qqProcessProbe, *qqProcessProbe) {
		first, second := &qqProcessProbe{}, &qqProcessProbe{}
		source := newQQProcessMemorySource()
		source.now = func() time.Time { return now }
		source.processIDs = func() ([]uint32, error) { return []uint32{101, 202}, nil }
		source.openProbe = func(_ context.Context, pid uint32) (*qqProcessProbe, error) {
			switch pid {
			case 101:
				return first, nil
			case 202:
				return second, nil
			default:
				return nil, errors.New("未知测试进程")
			}
		}
		return source, first, second
	}

	t.Run("身份或模块无法检查", func(t *testing.T) {
		source, first, _ := newSource()
		source.openProbe = func(_ context.Context, pid uint32) (*qqProcessProbe, error) {
			if pid == 101 {
				return first, nil
			}
			return nil, errors.New("拒绝访问或 QQMusic.dll 无法验证")
		}
		source.readEvidence = func(context.Context, *qqProcessProbe) (qqMemoryEvidence, error) { return matching, nil }
		if songID, ok := source.CurrentSongID(context.Background(), current); ok || songID != 0 {
			t.Fatalf("忽略不可检查进程后接受了另一个匹配: songID=%d ok=%v", songID, ok)
		}
	})

	t.Run("关键结构读取失败", func(t *testing.T) {
		source, first, second := newSource()
		source.readEvidence = func(_ context.Context, probe *qqProcessProbe) (qqMemoryEvidence, error) {
			if probe == first {
				return matching, nil
			}
			if probe == second {
				return qqMemoryEvidence{}, errors.New("关键结构短读")
			}
			return qqMemoryEvidence{}, errors.New("未知测试探针")
		}
		if songID, ok := source.CurrentSongID(context.Background(), current); ok || songID != 0 {
			t.Fatalf("忽略短读进程后接受了另一个匹配: songID=%d ok=%v", songID, ok)
		}
	})

	t.Run("近期失败负缓存仍代表未验证候选", func(t *testing.T) {
		source, first, _ := newSource()
		source.failedProbe[202] = now.Add(-time.Second)
		openCalls := 0
		source.openProbe = func(_ context.Context, pid uint32) (*qqProcessProbe, error) {
			openCalls++
			if pid == 101 {
				return first, nil
			}
			return nil, errors.New("负缓存期间不应重试")
		}
		source.readEvidence = func(context.Context, *qqProcessProbe) (qqMemoryEvidence, error) { return matching, nil }
		if songID, ok := source.CurrentSongID(context.Background(), current); ok || songID != 0 || openCalls != 1 {
			t.Fatalf("负缓存候选被当作不存在: songID=%d ok=%v openCalls=%d", songID, ok, openCalls)
		}
	})

	t.Run("完整读取但证据不一致", func(t *testing.T) {
		source, first, second := newSource()
		source.readEvidence = func(_ context.Context, probe *qqProcessProbe) (qqMemoryEvidence, error) {
			if probe == first {
				return matching, nil
			}
			if probe == second {
				inconsistent := matching
				inconsistent.SongID++
				inconsistent.Title = "另一个进程中的歌曲"
				return inconsistent, nil
			}
			return qqMemoryEvidence{}, errors.New("未知测试探针")
		}
		if songID, ok := source.CurrentSongID(context.Background(), current); ok || songID != 0 {
			t.Fatalf("忽略不一致证据后接受了唯一匹配: songID=%d ok=%v", songID, ok)
		}
	})
}

func TestQQProcessMemoryAcceptsOnlyFullyVerifiedCandidateSet(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	current := qqInjectorCurrentTrackWithoutID()
	current.PositionUpdatedAt = now
	source := newQQProcessMemorySource()
	source.now = func() time.Time { return now }
	source.processIDs = func() ([]uint32, error) { return []uint32{101}, nil }
	source.openProbe = func(context.Context, uint32) (*qqProcessProbe, error) { return &qqProcessProbe{}, nil }
	source.readEvidence = func(context.Context, *qqProcessProbe) (qqMemoryEvidence, error) {
		return qqMemoryEvidence{
			SongID: 97773, Title: "晴天", Artist: "周杰伦", Album: "叶惠美",
			Duration: 269 * time.Second, Position: 42 * time.Second, Playing: true, PlayStatusKnown: true,
		}, nil
	}
	if songID, ok := source.CurrentSongID(context.Background(), current); !ok || songID != 97773 {
		t.Fatalf("完整且一致的唯一候选被拒绝: songID=%d ok=%v", songID, ok)
	}
}

func TestScanQQMemoryRegionFailsOnAnyReadableChunkError(t *testing.T) {
	reads := 0
	_, err := scanQQMemoryRegionWithReader(context.Background(), 0x1000, 0x1000+qqMemoryScanChunkBytes+16,
		func(_ uintptr, _ []byte) error {
			reads++
			if reads == 2 {
				return errors.New("测试短读")
			}
			return nil
		})
	if err == nil || reads != 2 {
		t.Fatalf("模块可读区域的局部失败被忽略: reads=%d err=%v", reads, err)
	}
}

func TestScanQQMemoryRegionStopsAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reads := 0
	_, err := scanQQMemoryRegionWithReader(ctx, 0x1000, 0x1000+2*qqMemoryScanChunkBytes,
		func(_ uintptr, _ []byte) error {
			reads++
			cancel()
			return nil
		})
	if !errors.Is(err, context.Canceled) || reads != 1 {
		t.Fatalf("取消后仍继续扫描 QQMusic.dll: reads=%d err=%v", reads, err)
	}
}

func TestQQPatternAcrossAdjacentReadableRegionsStillCountsForUniqueness(t *testing.T) {
	base := uintptr(0x1000)
	data := make([]byte, len(qqCurrentSongInfoPattern)*3+32)
	writePattern := func(offset int, pointer uint32) {
		for index, wanted := range qqCurrentSongInfoPattern {
			if wanted >= 0 {
				data[offset+index] = byte(wanted)
			} else {
				data[offset+index] = byte(index + 1)
			}
		}
		binary.LittleEndian.PutUint32(data[offset+1:offset+5], pointer)
	}
	firstOffset := 4
	secondOffset := len(qqCurrentSongInfoPattern) + 18
	writePattern(firstOffset, 0x12345678)
	writePattern(secondOffset, 0x87654321)
	// 第一处特征跨过两个相邻可读 region；第二处完整位于后一个 region。
	// 若按 region 分开扫描会漏掉第一处并错误接受第二处为唯一。
	boundary := base + uintptr(firstOffset+len(qqCurrentSongInfoPattern)/2)
	ranges := []qqMemoryRange{
		{start: base, end: boundary},
		{start: boundary, end: base + uintptr(len(data))},
	}
	found, err := scanQQMemoryRangesWithReader(context.Background(), ranges,
		func(address uintptr, target []byte) error {
			offset := int(address - base)
			copy(target, data[offset:offset+len(target)])
			return nil
		})
	if err != nil || len(found) != 2 {
		t.Fatalf("跨 region 特征没有参与唯一性判断: found=%#x err=%v", found, err)
	}
}

func TestQQModuleOverScanBudgetCannotBePartiallyAccepted(t *testing.T) {
	tests := []struct {
		name    string
		base    uintptr
		size    uint32
		wantEnd uintptr
		wantErr bool
	}{
		{name: "空模块", base: 0x1000, size: 0, wantErr: true},
		{name: "完整上限", base: 0x1000, size: qqMemoryMaximumScanBytes, wantEnd: 0x1000 + qqMemoryMaximumScanBytes},
		{name: "超过上限", base: 0x1000, size: qqMemoryMaximumScanBytes + 1, wantErr: true},
		{name: "地址溢出", base: ^uintptr(0) - uintptr(qqMemoryMaximumScanBytes) + 1, size: qqMemoryMaximumScanBytes, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			end, err := qqModuleScanEnd(test.base, test.size)
			if (err != nil) != test.wantErr || (!test.wantErr && end != test.wantEnd) {
				t.Fatalf("qqModuleScanEnd(%#x, %d) = (%#x, %v), want (%#x, err=%v)",
					test.base, test.size, end, err, test.wantEnd, test.wantErr)
			}
		})
	}
}
