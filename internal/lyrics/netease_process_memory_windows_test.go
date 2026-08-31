//go:build windows

package lyrics

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDecodeNeteaseMemoryTrackIDSupportsMSVCSSO(t *testing.T) {
	inline := make([]byte, 0x20)
	copy(inline, "757567_0")
	binary.LittleEndian.PutUint64(inline[0x10:0x18], 8)
	binary.LittleEndian.PutUint64(inline[0x18:0x20], 15)
	trackID, err := decodeNeteaseMemoryTrackID(inline, func(uintptr, []byte) error {
		return errors.New("短字符串不应解引用指针")
	})
	if err != nil || trackID != "757567" {
		t.Fatalf("解析网易云 SSO 歌曲 ID 失败: id=%q err=%v", trackID, err)
	}

	heapValue := []byte("1234567890123456789_12")
	heap := make([]byte, 0x20)
	binary.LittleEndian.PutUint64(heap[:8], 0x12345678)
	binary.LittleEndian.PutUint64(heap[0x10:0x18], uint64(len(heapValue)))
	binary.LittleEndian.PutUint64(heap[0x18:0x20], uint64(len(heapValue)))
	trackID, err = decodeNeteaseMemoryTrackID(heap, func(address uintptr, target []byte) error {
		if address != 0x12345678 || len(target) != len(heapValue) {
			return errors.New("测试堆指针无效")
		}
		copy(target, heapValue)
		return nil
	})
	if err != nil || trackID != "1234567890123456789" {
		t.Fatalf("解析网易云堆 std::string 失败: id=%q err=%v", trackID, err)
	}
}

func TestDecodeNeteaseMemoryTrackIDRejectsMalformedStrings(t *testing.T) {
	tests := []struct {
		name  string
		value string
		len   uint64
		cap   uint64
	}{
		{name: "空", value: "", len: 0, cap: 15},
		{name: "无下划线", value: "757567", len: 6, cap: 15},
		{name: "非数字 ID", value: "abc_0", len: 5, cap: 15},
		{name: "前导零", value: "0757567_0", len: 9, cap: 15},
		{name: "容量小于长度", value: "757567_0", len: 8, cap: 7},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := make([]byte, 0x20)
			copy(raw, test.value)
			binary.LittleEndian.PutUint64(raw[0x10:0x18], test.len)
			binary.LittleEndian.PutUint64(raw[0x18:0x20], test.cap)
			if trackID, err := decodeNeteaseMemoryTrackID(raw, func(uintptr, []byte) error { return nil }); err == nil || trackID != "" {
				t.Fatalf("非法内存字符串被接受: id=%q err=%v", trackID, err)
			}
		})
	}
}

func TestNeteasePatternsAreStrictAndCrossReadableBoundary(t *testing.T) {
	base := uintptr(0x1000)
	data := make([]byte, len(neteaseAudioPlayerPattern)+len(neteaseAudioSchedulePattern)+64)
	writePattern := func(offset int, pattern []int16) {
		for index, wanted := range pattern {
			if wanted >= 0 {
				data[offset+index] = byte(wanted)
			} else {
				data[offset+index] = byte(index*7 + 3)
			}
		}
	}
	playerOffset := 13
	scheduleOffset := playerOffset + len(neteaseAudioPlayerPattern) + 11
	writePattern(playerOffset, neteaseAudioPlayerPattern)
	writePattern(scheduleOffset, neteaseAudioSchedulePattern)
	boundary := base + uintptr(playerOffset+len(neteaseAudioPlayerPattern)/2)
	ranges := []neteaseMemoryRange{
		{start: base, end: boundary},
		{start: boundary, end: base + uintptr(len(data))},
	}
	matches, err := scanNeteaseMemoryRangesWithReader(context.Background(), ranges,
		func(address uintptr, target []byte) error {
			offset := int(address - base)
			copy(target, data[offset:offset+len(target)])
			return nil
		})
	if err != nil || len(matches.player) != 1 || len(matches.schedule) != 1 ||
		matches.player[0] != base+uintptr(playerOffset) ||
		matches.schedule[0] != base+uintptr(scheduleOffset) {
		t.Fatalf("网易云特征码跨区间扫描失败: matches=%+v err=%v", matches, err)
	}

	data[playerOffset+25] ^= 0xff
	matches, err = scanNeteaseMemoryRangesWithReader(context.Background(), ranges,
		func(address uintptr, target []byte) error {
			offset := int(address - base)
			copy(target, data[offset:offset+len(target)])
			return nil
		})
	if err != nil || len(matches.player) != 0 || len(matches.schedule) != 1 {
		t.Fatalf("损坏的 AudioPlayer 特征仍被接受: matches=%+v err=%v", matches, err)
	}
}

func TestNeteaseModuleOverScanBudgetFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		base    uintptr
		size    uint32
		wantEnd uintptr
		wantErr bool
	}{
		{name: "空模块", base: 0x1000, size: 0, wantErr: true},
		{name: "完整上限", base: 0x1000, size: neteaseMemoryMaximumScanBytes,
			wantEnd: 0x1000 + neteaseMemoryMaximumScanBytes},
		{name: "超过上限", base: 0x1000, size: neteaseMemoryMaximumScanBytes + 1, wantErr: true},
		{name: "地址溢出", base: ^uintptr(0) - uintptr(neteaseMemoryMaximumScanBytes) + 1,
			size: neteaseMemoryMaximumScanBytes, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			end, err := neteaseModuleScanEnd(test.base, test.size)
			if (err != nil) != test.wantErr || (!test.wantErr && end != test.wantEnd) {
				t.Fatalf("neteaseModuleScanEnd(%#x, %d)=(%#x,%v)", test.base, test.size, end, err)
			}
		})
	}
}

func neteaseTestCurrent(now time.Time) NowPlaying {
	return NowPlaying{
		Source: "netease", Title: "1967", Artist: "ゴンチチ", Album: "humble music",
		Position: int64(42 * time.Second), Duration: int64(194 * time.Second),
		PositionKnown: true, Playing: true, PositionUpdatedAt: now,
	}
}

func TestNeteaseMemoryEvidenceRequiresSameSMTCTimeline(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	current := neteaseTestCurrent(now)
	evidence := neteaseMemoryEvidence{
		TrackID: "757567", Status: 1, Duration: 194 * time.Second,
		Position: 42 * time.Second, SampledAt: now,
	}
	if !neteaseMemoryEvidenceMatchesCurrent(evidence, current, now) {
		t.Fatal("同一网易云播放状态与时间轴没有通过交叉验证")
	}
	mutations := map[string]func(*neteaseMemoryEvidence, *NowPlaying){
		"invalid_id":       func(value *neteaseMemoryEvidence, _ *NowPlaying) { value.TrackID = "0" },
		"wrong_source":     func(_ *neteaseMemoryEvidence, track *NowPlaying) { track.Source = "qq" },
		"missing_title":    func(_ *neteaseMemoryEvidence, track *NowPlaying) { track.Title = "" },
		"missing_artist":   func(_ *neteaseMemoryEvidence, track *NowPlaying) { track.Artist = "" },
		"missing_album":    func(_ *neteaseMemoryEvidence, track *NowPlaying) { track.Album = "" },
		"unknown_position": func(_ *neteaseMemoryEvidence, track *NowPlaying) { track.PositionKnown = false },
		"status":           func(value *neteaseMemoryEvidence, _ *NowPlaying) { value.Status = 2 },
		"duration":         func(value *neteaseMemoryEvidence, _ *NowPlaying) { value.Duration += 3 * time.Second },
		"position":         func(value *neteaseMemoryEvidence, _ *NowPlaying) { value.Position += 6 * time.Second },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changedEvidence, changedCurrent := evidence, current
			mutate(&changedEvidence, &changedCurrent)
			if neteaseMemoryEvidenceMatchesCurrent(changedEvidence, changedCurrent, now) {
				t.Fatal("不一致的网易云内存/SMTC 证据没有失败关闭")
			}
		})
	}
}

func TestNeteaseProcessMemoryAcceptsOnlyOneValidPlaybackProcess(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	current := neteaseTestCurrent(now)
	matching := neteaseMemoryEvidence{
		TrackID: "757567", Status: 1, Duration: 194 * time.Second, Position: 42 * time.Second,
	}
	newSource := func() (*neteaseProcessMemorySource, map[uint32]*neteaseProcessProbe) {
		probes := map[uint32]*neteaseProcessProbe{
			101: {}, 202: {}, 303: {},
		}
		source := newNeteaseProcessMemorySource()
		source.now = func() time.Time { return now }
		source.processIDs = func() ([]uint32, error) { return []uint32{101, 202, 303}, nil }
		source.openProbe = func(_ context.Context, pid uint32) (*neteaseProcessProbe, error) {
			return probes[pid], nil
		}
		return source, probes
	}

	t.Run("两个 Waiting 辅助进程与一个播放进程", func(t *testing.T) {
		source, probes := newSource()
		source.readEvidence = func(_ context.Context, probe *neteaseProcessProbe) (neteaseMemoryEvidence, error) {
			if probe == probes[202] {
				return matching, nil
			}
			return neteaseMemoryEvidence{}, errNeteaseNoPlaybackEvidence
		}
		if trackID, ok := source.CurrentTrackID(context.Background(), current); !ok || trackID != "757567" {
			t.Fatalf("唯一有效播放进程被拒绝: id=%q ok=%v", trackID, ok)
		}
	})

	t.Run("两个有效播放进程", func(t *testing.T) {
		source, probes := newSource()
		source.readEvidence = func(_ context.Context, probe *neteaseProcessProbe) (neteaseMemoryEvidence, error) {
			if probe == probes[101] || probe == probes[202] {
				return matching, nil
			}
			return neteaseMemoryEvidence{}, errNeteaseNoPlaybackEvidence
		}
		if trackID, ok := source.CurrentTrackID(context.Background(), current); ok || trackID != "" {
			t.Fatalf("多个有效进程没有失败关闭: id=%q ok=%v", trackID, ok)
		}
	})

	t.Run("任一进程无法完整检查", func(t *testing.T) {
		source, probes := newSource()
		source.readEvidence = func(_ context.Context, probe *neteaseProcessProbe) (neteaseMemoryEvidence, error) {
			if probe == probes[202] {
				return matching, nil
			}
			if probe == probes[303] {
				return neteaseMemoryEvidence{}, errors.New("模拟内存短读")
			}
			return neteaseMemoryEvidence{}, errNeteaseNoPlaybackEvidence
		}
		if trackID, ok := source.CurrentTrackID(context.Background(), current); ok || trackID != "" {
			t.Fatalf("忽略不可检查进程后接受了 ID: id=%q ok=%v", trackID, ok)
		}
	})
}

func TestEnrichNeteasePlatformTrackIDUsesOnlyNativeSource(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	track := neteaseTestCurrent(now)
	native := &fixedNeteaseNativeIDSource{trackID: "757567", ok: true}
	result := enrichNeteasePlatformTrackIDFromSource(context.Background(), track, native)
	if result.SourceTrackID() != "757567" || result.TrackIDOrigin != TrackIDOriginSource || native.calls != 1 {
		t.Fatalf("网易云内置只读来源没有标记精确 ID: result=%+v calls=%d", result, native.calls)
	}
	result = enrichNeteasePlatformTrackIDFromSource(context.Background(), track,
		&fixedNeteaseNativeIDSource{trackID: "同名搜索结果", ok: true})
	if result.SourceTrackID() != "" || result.TrackIDOrigin != "" {
		t.Fatalf("非法或推测 ID 被标记为来源: %+v", result)
	}

	prefilled := track
	prefilled.TrackID = "186016"
	prefilled.TrackIDOrigin = TrackIDOriginSource
	native = &fixedNeteaseNativeIDSource{trackID: "757567", ok: true}
	result = enrichNeteasePlatformTrackIDFromSource(context.Background(), prefilled, native)
	if result.SourceTrackID() != "757567" || result.TrackIDOrigin != TrackIDOriginSource || native.calls != 1 {
		t.Fatalf("预填网易云 ID 没有被本轮原生证据覆盖: result=%+v calls=%d", result, native.calls)
	}
	native = &fixedNeteaseNativeIDSource{}
	result = enrichNeteasePlatformTrackIDFromSource(context.Background(), prefilled, native)
	if result.SourceTrackID() != "" || result.TrackIDOrigin != "" || native.calls != 1 {
		t.Fatalf("本轮原生重验失败后仍沿用了预填 ID: result=%+v calls=%d", result, native.calls)
	}
	result = enrichNeteasePlatformTrackIDFromSource(context.Background(), prefilled, nil)
	if result.SourceTrackID() != "" || result.TrackIDOrigin != "" {
		t.Fatalf("原生来源缺失时仍沿用了预填 ID: result=%+v", result)
	}
}

type fixedNeteaseNativeIDSource struct {
	trackID string
	ok      bool
	calls   int
}

func (source *fixedNeteaseNativeIDSource) CurrentTrackID(context.Context, NowPlaying) (string, bool) {
	source.calls++
	return source.trackID, source.ok
}

// TestNeteaseProcessMemoryNativeIDLive 只读取当前 Windows 网易云
// 客户端进程与本机 playingList。SMTC 若存在只做额外交叉；
// 它不重启客户端、不写进程、不识歌、不开启端口，也不读取 Cookie。
func TestNeteaseProcessMemoryNativeIDLive(t *testing.T) {
	if os.Getenv("CASESCREEN_NETEASE_MEMORY_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_NETEASE_MEMORY_LIVE=1 后验证真实网易云 3.x 客户端")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rawStartedAt := time.Now()
	rawPID, rawEvidence := readNeteaseLiveMemoryEvidence(t, ctx)
	rawScanDuration := time.Since(rawStartedAt)
	t.Logf("网易云内存原生证据: pid=%d id=%s status=%d position=%s duration=%s",
		rawPID, rawEvidence.TrackID, rawEvidence.Status, rawEvidence.Position, rawEvidence.Duration)
	memory := newNeteaseProcessMemorySource()
	source := newNeteaseNowPlayingSource(memory)
	defer source.Close()
	startedAt := time.Now()
	track, err := source.Current(ctx)
	if err != nil || !track.Valid() || track.SourceTrackID() == "" || !track.PositionKnown {
		t.Fatalf("内置只读网易云当前曲目源失败: track=%+v err=%v", track, err)
	}
	if track.SourceTrackID() != rawEvidence.TrackID {
		t.Fatalf("裸内存读取与完整直接源期间发生切歌: raw=%s direct=%s",
			rawEvidence.TrackID, track.SourceTrackID())
	}
	expected := strings.TrimSpace(os.Getenv("CASESCREEN_NETEASE_EXPECT_TRACK_ID"))
	if expected != "" && track.SourceTrackID() != expected {
		t.Fatalf("网易云只读 ID=%q，期望=%q", track.SourceTrackID(), expected)
	}
	pids, _ := neteaseMusicProcessIDs()
	_, moduleSize, _ := neteaseCloudMusicModule(rawPID)
	status := "Paused"
	if track.Playing {
		status = "Playing"
	}
	smtcState := "absent"
	if smtcTrack, smtcErr := (&smtcSource{preferredSource: "netease"}).Current(ctx); smtcErr != nil {
		smtcState = "unavailable"
	} else if neteaseSMTCEvidenceComplete(smtcTrack) {
		smtcState = "matched"
	}
	t.Logf("网易云 3.x 只读真机验证通过: id=%s status_code=%d status=%s position=%s duration=%s title=%q artist=%q album=%q playing_list_cross_check=true smtc=%s processes=%d module_bytes=%d raw_scan=%s direct_scan_and_metadata=%s",
		track.SourceTrackID(), rawEvidence.Status, status, time.Duration(track.Position),
		time.Duration(track.Duration), track.Title, track.Artist, track.Album, smtcState,
		len(pids), moduleSize, rawScanDuration, time.Since(startedAt))
}

func readNeteaseLiveMemoryEvidence(t *testing.T, ctx context.Context) (uint32, neteaseMemoryEvidence) {
	t.Helper()
	pids, err := neteaseMusicProcessIDs()
	if err != nil || len(pids) == 0 {
		t.Fatalf("枚举网易云进程失败: pids=%v err=%v", pids, err)
	}
	var selectedPID uint32
	var selected neteaseMemoryEvidence
	for _, pid := range pids {
		probe, openErr := openNeteaseProcessProbe(ctx, pid)
		if openErr != nil {
			t.Fatalf("打开网易云只读探针失败: pid=%d err=%v", pid, openErr)
		}
		first, firstErr := probe.readEvidenceOnce(ctx)
		second, secondErr := probe.readEvidenceOnce(ctx)
		t.Logf("网易云内存候选: pid=%d first=%+v first_err=%v second=%+v second_err=%v",
			pid, first, firstErr, second, secondErr)
		evidence, readErr := probe.readEvidence(ctx)
		probe.close()
		if errors.Is(readErr, errNeteaseNoPlaybackEvidence) {
			continue
		}
		if readErr != nil {
			t.Fatalf("读取网易云只读证据失败: pid=%d err=%v", pid, readErr)
		}
		if selectedPID != 0 {
			t.Fatalf("多个 cloudmusic.exe 返回有效播放证据: first=%d second=%d", selectedPID, pid)
		}
		selectedPID, selected = pid, evidence
	}
	if selectedPID == 0 {
		t.Fatal("没有 cloudmusic.exe 返回有效播放证据")
	}
	return selectedPID, selected
}
