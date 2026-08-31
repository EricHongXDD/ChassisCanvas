package coolermaster

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	protocol "casescreen/internal/protocol/coolermaster"
	"casescreen/internal/theme"
)

type fakeHID struct {
	mu                       sync.Mutex
	writes                   [][]byte
	responses                [][]byte
	staleOnNextCommand       int
	suppressNextCommand      bool
	nextCommandStatus        int
	dataResponseStatus       int
	suppressDataResponse     bool
	malformedDataResponse    bool
	malformedNextCommand     bool
	suppressTransported      bool
	suppressTransportedCount int
	suppressDisplayMode      bool
	transportedStatus        int
	transportedACK           int
	displayModeStatus        int
	displayModeWriteErr      error
	malformedDisplayMode     bool
	unexpectedDisplayACK     int
	malformedTransported     bool
	lateTransportedACK       bool
	transportedLateStatus    int
	pendingLateStatus        int
	transportedWriteErr      error
	lateDataACKOnTransported bool
	lateOfflineStageReplies  bool
	brightnessWriteErr       error
	suppressBrightnessReply  bool
	brightnessFirstTimeout   bool
	awaitingBrightnessRead   bool
	brightnessStatuses       []int
	lateModeACKOnBrightness  bool
	suppressParamResponse    bool
	paramActiveMasks         [][]int
	deleteStatuses           []int
	awaitingTransportedRead  bool
	transportedReadTimeouts  []time.Duration
	readTimeouts             []time.Duration
	onReadTimeout            func(time.Duration)
	onDisplayModeResponse    func()
	pendingResponseHook      func()
	onTransported            func()
}

type trackedHID struct {
	*fakeHID
	closed    chan struct{}
	closeOnce sync.Once
}

func (transport *trackedHID) Close() error {
	transport.closeOnce.Do(func() { close(transport.closed) })
	return nil
}

func (fake *fakeHID) Write(_ context.Context, data []byte, _ time.Duration) (int, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	copyOfData := append([]byte(nil), data...)
	fake.writes = append(fake.writes, copyOfData)
	if data[0] == protocol.RequestReportID {
		isParam := bytes.Contains(data, []byte("POST param 1"))
		isTransported := bytes.Contains(data, []byte("POST transported 1"))
		isDisplayMode := bytes.Contains(data, []byte("POST realtimeDisplay 1"))
		isBrightness := bytes.Contains(data, []byte("POST brightness 1"))
		isDeleteMedia := bytes.Contains(data, []byte("POST delMedia 1"))
		ack := 44
		if bytes.Contains(data, []byte("SeqNumber=42")) {
			ack = 43
		} else if bytes.Contains(data, []byte("SeqNumber=44")) {
			ack = 45
		} else if bytes.Contains(data, []byte("SeqNumber=45")) {
			ack = 46
		}
		if isTransported && fake.onTransported != nil {
			fake.onTransported()
		}
		if isParam && fake.suppressParamResponse {
			return len(data), nil
		}
		if isParam && len(fake.paramActiveMasks) > 0 {
			mask := append([]int(nil), fake.paramActiveMasks[0]...)
			fake.paramActiveMasks = fake.paramActiveMasks[1:]
			fake.responses = append(fake.responses, fakeSuspendResponse(ack, mask))
			return len(data), nil
		}
		if isTransported && fake.transportedWriteErr != nil {
			err := fake.transportedWriteErr
			fake.transportedWriteErr = nil
			return 0, err
		}
		if isDisplayMode && fake.displayModeWriteErr != nil {
			err := fake.displayModeWriteErr
			fake.displayModeWriteErr = nil
			return 0, err
		}
		if isDisplayMode && fake.malformedDisplayMode {
			fake.malformedDisplayMode = false
			malformed := make([]byte, protocol.ReportSize)
			malformed[0] = protocol.ResponseReportID
			fake.responses = append(fake.responses, malformed)
			return len(data), nil
		}
		if isBrightness && fake.brightnessWriteErr != nil {
			err := fake.brightnessWriteErr
			fake.brightnessWriteErr = nil
			return 0, err
		}
		if isTransported {
			fake.awaitingTransportedRead = true
		}
		if isTransported && fake.lateDataACKOnTransported {
			fake.lateDataACKOnTransported = false
			fake.responses = append(fake.responses, fakeResponse(43))
		}
		if isTransported && fake.malformedTransported {
			fake.malformedTransported = false
			malformed := make([]byte, protocol.ReportSize)
			malformed[0] = protocol.ResponseReportID
			fake.responses = append(fake.responses, malformed)
			return len(data), nil
		}
		if isTransported && fake.suppressTransported {
			fake.suppressTransported = false
			return len(data), nil
		}
		if isTransported && fake.suppressTransportedCount > 0 {
			fake.suppressTransportedCount--
			return len(data), nil
		}
		if isTransported && fake.transportedLateStatus != 0 {
			fake.pendingLateStatus = fake.transportedLateStatus
			fake.transportedLateStatus = 0
			return len(data), nil
		}
		if isDisplayMode && fake.lateTransportedACK {
			fake.lateTransportedACK = false
			fake.responses = append(fake.responses, fakeResponse(44))
		}
		if isDisplayMode && fake.unexpectedDisplayACK != 0 {
			fake.responses = append(fake.responses, fakeResponse(fake.unexpectedDisplayACK))
			fake.unexpectedDisplayACK = 0
		}
		if isDisplayMode && fake.onDisplayModeResponse != nil {
			fake.pendingResponseHook = fake.onDisplayModeResponse
			fake.onDisplayModeResponse = nil
		}
		if isDisplayMode && fake.suppressDisplayMode {
			fake.suppressDisplayMode = false
			return len(data), nil
		}
		if isBrightness {
			fake.awaitingBrightnessRead = true
		}
		if isBrightness && fake.lateModeACKOnBrightness {
			fake.lateModeACKOnBrightness = false
			fake.responses = append(fake.responses, fakeResponse(46))
		}
		if isBrightness && fake.suppressBrightnessReply {
			return len(data), nil
		}
		if isBrightness && len(fake.brightnessStatuses) > 0 {
			for _, brightnessStatus := range fake.brightnessStatuses {
				fake.responses = append(fake.responses, fakeResponseStatus(ack, brightnessStatus))
			}
			fake.brightnessStatuses = nil
			return len(data), nil
		}
		if fake.suppressNextCommand && !isParam {
			fake.suppressNextCommand = false
			return len(data), nil
		}
		if fake.malformedNextCommand && !isParam {
			fake.malformedNextCommand = false
			malformed := make([]byte, protocol.ReportSize)
			malformed[0] = protocol.ResponseReportID
			fake.responses = append(fake.responses, malformed)
			return len(data), nil
		}
		for range fake.staleOnNextCommand {
			fake.responses = append(fake.responses, fakeResponse(43))
		}
		fake.staleOnNextCommand = 0
		status := 200
		if isTransported && fake.transportedACK != 0 {
			ack = fake.transportedACK
		}
		if isTransported && fake.transportedStatus != 0 {
			status = fake.transportedStatus
			fake.transportedStatus = 0
		} else if isDisplayMode && fake.displayModeStatus != 0 {
			status = fake.displayModeStatus
			fake.displayModeStatus = 0
		} else if isDeleteMedia && len(fake.deleteStatuses) > 0 {
			status = fake.deleteStatuses[0]
			fake.deleteStatuses = fake.deleteStatuses[1:]
		} else if fake.nextCommandStatus != 0 && !isParam {
			status = fake.nextCommandStatus
			fake.nextCommandStatus = 0
		}
		fake.responses = append(fake.responses, fakeResponseStatus(ack, status))
	} else if data[0] == protocol.DataReportID {
		total := binary.BigEndian.Uint16(data[5:7])
		index := binary.BigEndian.Uint16(data[7:9])
		if index+1 == total {
			if fake.lateOfflineStageReplies {
				fake.lateOfflineStageReplies = false
				// 前一个是迟到的 transport ACK，后一个是 data ACK。
				fake.responses = append(fake.responses, fakeResponse(43), fakeResponse(43))
				return len(data), nil
			}
			if fake.suppressDataResponse {
				fake.suppressDataResponse = false
				return len(data), nil
			}
			if fake.malformedDataResponse {
				fake.malformedDataResponse = false
				malformed := make([]byte, protocol.ReportSize)
				malformed[0] = protocol.ResponseReportID
				fake.responses = append(fake.responses, malformed)
				return len(data), nil
			}
			status := 200
			if fake.dataResponseStatus != 0 {
				status = fake.dataResponseStatus
			}
			fake.responses = append(fake.responses, fakeResponseStatus(43, status))
		}
	}
	return len(data), nil
}

func (fake *fakeHID) Read(_ context.Context, data []byte, timeout time.Duration) (int, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.readTimeouts = append(fake.readTimeouts, timeout)
	if fake.onReadTimeout != nil {
		fake.onReadTimeout(timeout)
	}
	if fake.awaitingBrightnessRead && fake.brightnessFirstTimeout {
		fake.brightnessFirstTimeout = false
		fake.awaitingBrightnessRead = false
		return 0, context.DeadlineExceeded
	}
	if fake.awaitingTransportedRead {
		fake.transportedReadTimeouts = append(fake.transportedReadTimeouts, timeout)
		fake.awaitingTransportedRead = false
	}
	if len(fake.responses) == 0 && fake.pendingLateStatus != 0 {
		fake.responses = append(fake.responses, fakeResponseStatus(43, fake.pendingLateStatus))
		fake.pendingLateStatus = 0
		return 0, context.DeadlineExceeded
	}
	if len(fake.responses) == 0 {
		return 0, context.DeadlineExceeded
	}
	if fake.pendingResponseHook != nil {
		hook := fake.pendingResponseHook
		fake.pendingResponseHook = nil
		hook()
	}
	response := fake.responses[0]
	fake.responses = fake.responses[1:]
	return copy(data, response), nil
}

func (*fakeHID) Close() error { return nil }

func TestApplyThemeUploadsOnce(t *testing.T) {
	transport := &fakeHID{}
	driver := New("cooler-1", "冷龙影二代", [2]int{480, 480}, 2, transport, t.TempDir()+"/state.json")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifest := theme.Manifest{
		ID: "solid", Name: "纯色", Width: 480, Height: 480,
		Layers: []theme.Layer{{
			ID: "background", Type: theme.LayerBackgroundImage,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
			Options: map[string]any{"color": "#123456ff"},
		}},
	}
	if err := driver.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	firstDataWrites := countReportID(transport.writes, protocol.DataReportID)
	if firstDataWrites == 0 {
		t.Fatal("首次应用主题没有发送资源数据块")
	}
	if !hasMediaDataKindReport(transport.writes, protocol.MediaDataJPEG) {
		t.Fatal("离线 JPG 没有使用原厂媒体类型 1")
	}
	modeIndex := indexReportContaining(transport.writes, []byte("POST realtimeDisplay 1"))
	paramIndex := indexReportContaining(transport.writes, []byte("POST param 1"))
	if modeIndex < 0 || paramIndex <= modeIndex {
		t.Fatalf("原厂后置刷新顺序无效: realtimeDisplay=%d param=%d", modeIndex, paramIndex)
	}
	beforeSecondApply := len(transport.writes)
	if err := driver.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if got := countReportID(transport.writes, protocol.DataReportID); got != firstDataWrites {
		t.Fatalf("相同主题重复上传: %d -> %d", firstDataWrites, got)
	}
	if len(transport.writes) != beforeSecondApply {
		t.Fatal("已处于离线模式时仍重复发送显示模式命令")
	}
	if info := driver.Info(); !info.Capabilities.StoresResources || info.Capabilities.SmallTelemetry {
		t.Fatalf("能力声明无效: %+v", info.Capabilities)
	}
	frame := image.NewRGBA(image.Rect(0, 0, 480, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 480; x++ {
			frame.SetRGBA(x, y, color.RGBA{R: 40, G: 80, B: 120, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, frame, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	beforeRealtime := len(transport.writes)
	if err := driver.SendJPEG(context.Background(), encoded.Bytes()); err != nil {
		t.Fatal(err)
	}
	realtimeWrites := transport.writes[beforeRealtime:]
	if countReportID(realtimeWrites, protocol.RequestReportID) != 1 {
		t.Fatal("首次实时帧没有且仅有一次模式切换")
	}
	if !hasMediaDataKindReport(realtimeWrites, protocol.MediaDataJPEG) {
		t.Fatal("实时 JPEG 没有使用原厂媒体类型 1")
	}
	beforeSecondFrame := len(transport.writes)
	if err := driver.SendJPEG(context.Background(), encoded.Bytes()); err != nil {
		t.Fatal(err)
	}
	if countReportID(transport.writes[beforeSecondFrame:], protocol.RequestReportID) != 0 {
		t.Fatal("第二帧重复切换实时模式")
	}
	if err := driver.SetRealtimeEnabled(context.Background(), false); err != nil {
		t.Fatalf("滞留数据 ACK 导致 Offline Mode 恢复失败: %v", err)
	}
}

func TestLegacyTrustStateForcesExactlyOneReplacement(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	manifest := offlineTestManifest("legacy-trust-version")
	first := New("cooler-trust-first", "冷龙影二代", [2]int{480, 480}, 4, &fakeHID{}, statePath)
	first.pause = func(context.Context, time.Duration) error { return nil }
	if err := first.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	legacy, err := loadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	legacy.TrustVersion = 0
	if err := saveState(statePath, legacy); err != nil {
		t.Fatal(err)
	}

	transport := &fakeHID{}
	restarted := New("cooler-trust-restart", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	restarted.pause = func(context.Context, time.Duration) error { return nil }
	if err := restarted.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatalf("旧信任状态没有完成一次强制替换: %v", err)
	}
	firstDataWrites := countReportID(transport.writes, protocol.DataReportID)
	if firstDataWrites == 0 {
		t.Fatal("旧信任状态错误命中 digest 快路径")
	}
	persisted, err := loadState(statePath)
	if err != nil || persisted.TrustVersion != offlineStateTrustVersion || persisted.Slots[4].Uncertain {
		t.Fatalf("强制替换后没有建立新版可信状态: state=%+v err=%v", persisted, err)
	}
	if err := restarted.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if got := countReportID(transport.writes, protocol.DataReportID); got != firstDataWrites {
		t.Fatalf("新版可信状态仍重复上传: %d -> %d", firstDataWrites, got)
	}
}

func TestInvalidateResidentThemeCacheIsPersistentAndIdempotent(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	want := storedSlot{
		Digest: "verified", FileName: "suspend_3.jpg", CommittedAtUnixMilli: 123456,
		ReplaceVersion: offlineReplaceVersion, LegacyAliasMigration: legacySlot5MigrationComplete,
	}
	if err := saveState(statePath, storedState{
		TrustVersion: offlineStateTrustVersion,
		Slots:        map[int]storedSlot{4: want},
	}); err != nil {
		t.Fatal(err)
	}
	driver := New("cooler-invalidate", "冷龙影二代", [2]int{480, 480}, 4, nil, statePath)
	saves := 0
	driver.stateSaver = func(path string, state storedState) error {
		saves++
		return saveState(path, state)
	}
	if err := driver.InvalidateResidentThemeCache(); err != nil {
		t.Fatal(err)
	}
	persisted, err := loadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	got := persisted.Slots[4]
	if !got.Uncertain || got.Digest != want.Digest || got.FileName != want.FileName ||
		got.CommittedAtUnixMilli != want.CommittedAtUnixMilli || got.ReplaceVersion != want.ReplaceVersion ||
		got.LegacyAliasMigration != want.LegacyAliasMigration {
		t.Fatalf("失效驻留缓存时破坏了提交证据: %+v", got)
	}
	if persisted.TrustVersion != offlineStateTrustVersion || !driver.offlineCommitUncertain {
		t.Fatalf("驻留缓存没有完整失效: state=%+v memory=%t", persisted, driver.offlineCommitUncertain)
	}
	if err := driver.InvalidateResidentThemeCache(); err != nil {
		t.Fatal(err)
	}
	if saves != 1 {
		t.Fatalf("重复冲突检测反复改写状态文件: saves=%d", saves)
	}
}

func TestInvalidateResidentThemeCacheRetriesAfterPersistenceFailure(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := saveState(statePath, storedState{
		TrustVersion: offlineStateTrustVersion,
		Slots: map[int]storedSlot{4: {
			Digest: "verified", FileName: "suspend_3.jpg", ReplaceVersion: offlineReplaceVersion,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	driver := New("cooler-invalidate-failure", "冷龙影二代", [2]int{480, 480}, 4, nil, statePath)
	saveErr := errors.New("模拟驻留缓存失效落盘失败")
	driver.stateSaver = func(string, storedState) error { return saveErr }
	if err := driver.InvalidateResidentThemeCache(); !errors.Is(err, saveErr) {
		t.Fatalf("失效落盘错误未返回: %v", err)
	}
	if !driver.offlineCommitUncertain {
		t.Fatal("落盘失败后当前进程仍可能命中旧摘要")
	}
	persisted, err := loadState(statePath)
	if err != nil || persisted.Slots[4].Uncertain {
		t.Fatalf("原子落盘失败破坏了旧状态: state=%+v err=%v", persisted, err)
	}
	driver.stateSaver = saveState
	if err := driver.InvalidateResidentThemeCache(); err != nil {
		t.Fatalf("落盘恢复后没有重试: %v", err)
	}
	persisted, err = loadState(statePath)
	if err != nil || !persisted.Slots[4].Uncertain {
		t.Fatalf("重试后未持久失效标记: state=%+v err=%v", persisted, err)
	}
}

func TestApplyThemeUsesTransportedCommitAfterDataNACK(t *testing.T) {
	transport := &fakeHID{dataResponseStatus: 500}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-data-nack", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifest := theme.Manifest{
		ID: "data-nack", Name: "数据拒绝", Width: 480, Height: 480,
		Layers: []theme.Layer{{
			ID: "background", Type: theme.LayerBackgroundImage,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
			Options: map[string]any{"color": "#123456ff"},
		}},
	}
	if err := driver.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatalf("数据阶段 NACK 后已收到 transported/200，不应否定提交: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transported 1")); got != 1 {
		t.Fatalf("数据阶段 NACK 后 transported 次数=%d，want 1", got)
	}
	state, err := loadState(statePath)
	if err != nil || state.Slots[4].Digest == "" {
		t.Fatalf("transported/200 后未保存摘要: state=%+v err=%v", state, err)
	}
}

func TestOfflineJPEGRetriesBeforeTransportedWhenDataResponseMissing(t *testing.T) {
	transport := &fakeHID{suppressDataResponse: true}
	driver := New("cooler-jpeg-missing-data-response", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.uploadLocked(context.Background(), "suspend_3.jpg", []byte("test-jpeg")); err != nil {
		t.Fatal(err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transport 1")); got != 2 {
		t.Fatalf("JPEG 数据无回包后完整传输轮数=%d，want 2", got)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transported 1")); got != 1 {
		t.Fatalf("JPEG 首轮数据无回包时仍提前提交，transported 次数=%d，want 1", got)
	}
}

func TestOfflineJPEGMalformedDataResponseDoesNotPermitTransported(t *testing.T) {
	transport := &fakeHID{malformedDataResponse: true}
	driver := New("cooler-jpeg-malformed-data-response", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.uploadLocked(context.Background(), "suspend_3.jpg", []byte("test-jpeg")); err != nil {
		t.Fatal(err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transport 1")); got != 2 {
		t.Fatalf("JPEG 数据坏包后完整传输轮数=%d，want 2", got)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transported 1")); got != 1 {
		t.Fatalf("JPEG 首轮数据坏包时仍提前提交，transported 次数=%d，want 1", got)
	}
}

func TestOfflineMP4UsesOriginalMediaDataKind(t *testing.T) {
	transport := &fakeHID{suppressDataResponse: true}
	driver := New("cooler-mp4-kind", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.uploadLocked(context.Background(), "suspend_3.mp4", []byte("test-mp4")); err != nil {
		t.Fatal(err)
	}
	if !hasMediaDataKindReport(transport.writes, protocol.MediaDataMP4) {
		t.Fatal("离线 MP4 没有使用原厂媒体类型 0")
	}
	if got := countReportsContaining(transport.writes, []byte("POST transport 1")); got != 1 {
		t.Fatalf("MP4 数据无回包却错误重试，完整传输轮数=%d，want 1", got)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transported 1")); got != 1 {
		t.Fatalf("MP4 数据无回包后没有继续提交，transported 次数=%d，want 1", got)
	}
}

func TestApplyThemeIgnoresOriginalBestEffortParamTimeout(t *testing.T) {
	transport := &fakeHID{suppressParamResponse: true}
	driver := New(
		"cooler-param-timeout", "冷龙影二代", [2]int{480, 480}, 4,
		transport, t.TempDir()+"/state.json",
	)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyTheme(context.Background(), offlineTestManifest("param-timeout")); err != nil {
		t.Fatalf("原厂 best-effort param 无回包不应否定已提交主题: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST param 1")); got != 1 {
		t.Fatalf("没有发送原厂后置 param 刷新: %d", got)
	}
}

func TestSuspendSlotActiveParsesDeviceStatus(t *testing.T) {
	tests := []struct {
		name   string
		status map[string]any
		slot   int
		active bool
		found  bool
	}{
		{name: "数字活动", status: map[string]any{"suspendMediaActive": []any{float64(0), float64(1)}}, slot: 2, active: true, found: true},
		{name: "数字未活动", status: map[string]any{"suspendMediaActive": []any{0, 1}}, slot: 1, found: true},
		{name: "布尔活动", status: map[string]any{"suspendMediaActive": []any{true}}, slot: 1, active: true, found: true},
		{name: "字符串未活动", status: map[string]any{"suspendMediaActive": []any{"0"}}, slot: 1, found: true},
		{name: "没有字段", status: map[string]any{}, slot: 1},
		{name: "越界", status: map[string]any{"suspendMediaActive": []any{1}}, slot: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			active, found := suspendSlotActive(test.status, test.slot)
			if active != test.active || found != test.found {
				t.Fatalf("active=%t found=%t，want active=%t found=%t", active, found, test.active, test.found)
			}
		})
	}
}

func TestLegacySlot5EvidencePersistsPendingMarker(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	state := storedState{Slots: map[int]storedSlot{4: {
		Digest: "legacy", FileName: "suspend_4.jpg",
	}}}
	if err := saveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	driver := New("cooler-legacy-evidence", "冷龙影二代", [2]int{480, 480}, 4, nil, statePath)
	loaded, err := loadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	previous := loaded.Slots[4]
	if err := driver.rememberLegacySlot5MigrationEvidenceLocked(&loaded, &previous); err != nil {
		t.Fatal(err)
	}
	if previous.LegacyAliasMigration != legacySlot5MigrationPending {
		t.Fatalf("旧别名证据没有建立 pending 标记: %+v", previous)
	}
	persisted, err := loadState(statePath)
	if err != nil || persisted.Slots[4].LegacyAliasMigration != legacySlot5MigrationPending {
		t.Fatalf("pending 标记没有先于设备写入落盘: state=%+v err=%v", persisted, err)
	}
}

func TestLegacySlot5MigrationRequiresExplicitEvidence(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	state := storedState{Slots: map[int]storedSlot{4: {
		Digest: "current", FileName: "suspend_3.jpg", ReplaceVersion: offlineReplaceVersion,
	}}}
	transport := &fakeHID{paramActiveMasks: [][]int{{0, 0, 0, 1, 1}}}
	driver := New("cooler-no-legacy-evidence", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	err := driver.migrateLegacySlot5IfNeededLocked(context.Background(), &state)
	driver.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(transport.writes) != 0 {
		t.Fatal("没有旧版归属证据或 pending 标记时仍访问了设备")
	}
}

func TestLegacySlot5PendingNeverAuthorizesDeletion(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	state := storedState{Slots: map[int]storedSlot{4: {
		Digest: "current", FileName: "suspend_3.jpg", ReplaceVersion: offlineReplaceVersion,
		LegacyAliasMigration: legacySlot5MigrationPending,
	}}}
	if err := saveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	transport := &fakeHID{paramActiveMasks: [][]int{{0, 0, 0, 1, 1}}}
	driver := New("cooler-pending-no-delete", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	err := driver.migrateLegacySlot5IfNeededLocked(context.Background(), &state)
	driver.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(transport.writes) != 0 {
		t.Fatal("pending 只是历史提示，未显式批准却访问了设备")
	}
	persisted, err := loadState(statePath)
	if err != nil || persisted.Slots[4].LegacyAliasMigration != legacySlot5MigrationPending {
		t.Fatalf("pending 状态被普通流程改变: state=%+v err=%v", persisted, err)
	}
}

func TestLegacySlot5MigrationDeletesOnlyLegacyAliasAndPersistsCompletion(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	state := approvedLegacySlot5State()
	if err := saveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	transport := &fakeHID{paramActiveMasks: [][]int{
		{0, 0, 0, 1, 1},
		{0, 0, 0, 1, 0},
	}}
	driver := New("cooler-legacy-cleanup", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	err := driver.migrateLegacySlot5IfNeededLocked(context.Background(), &state)
	driver.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := countReportsContaining(transport.writes, []byte(`"fileName":"suspend_4.mp4"`)); got != 2 {
		t.Fatalf("原厂 MP4 首发+确认次数=%d，want 2", got)
	}
	if got := countReportsContaining(transport.writes, []byte(`"fileName":"suspend_4.jpg"`)); got != 1 {
		t.Fatalf("原厂 JPG 删除次数=%d，want 1", got)
	}
	mp4Index := indexReportContaining(transport.writes, []byte(`"fileName":"suspend_4.mp4"`))
	jpgIndex := indexReportContaining(transport.writes, []byte(`"fileName":"suspend_4.jpg"`))
	modeIndex := indexReportContaining(transport.writes, []byte("POST realtimeDisplay 1"))
	if mp4Index < 0 || jpgIndex <= mp4Index || modeIndex <= jpgIndex {
		t.Fatalf("迁移命令顺序错误: mp4=%d jpg=%d mode=%d", mp4Index, jpgIndex, modeIndex)
	}
	for _, report := range transport.writes {
		if bytes.Contains(report, []byte("POST delMedia 1")) &&
			!bytes.Contains(report, []byte("SeqNumber=42")) {
			t.Fatal("delMedia 没有使用原厂 SeqNumber=42")
		}
		if bytes.Contains(report, []byte(`"fileName":"all"`)) {
			t.Fatal("安全迁移禁止发送 delMedia all")
		}
	}
	persisted, err := loadState(statePath)
	if err != nil || persisted.Slots[4].LegacyAliasMigration != legacySlot5MigrationComplete {
		t.Fatalf("精确活动位验证后没有保存 complete: state=%+v err=%v", persisted, err)
	}
}

func TestLegacySlot5MigrationContinuesAfterSixMP4Failures(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	state := approvedLegacySlot5State()
	if err := saveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	transport := &fakeHID{
		paramActiveMasks: [][]int{{0, 0, 0, 1, 1}, {0, 0, 0, 1, 0}},
		deleteStatuses:   []int{500, 500, 500, 500, 500, 500, 200},
	}
	driver := New("cooler-legacy-mp4-missing", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	err := driver.migrateLegacySlot5IfNeededLocked(context.Background(), &state)
	driver.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := countReportsContaining(transport.writes, []byte(`"fileName":"suspend_4.mp4"`)); got != legacySlot5DeleteAttempts {
		t.Fatalf("MP4 删除失败时总尝试次数=%d，want %d", got, legacySlot5DeleteAttempts)
	}
	if got := countReportsContaining(transport.writes, []byte(`"fileName":"suspend_4.jpg"`)); got != 1 {
		t.Fatalf("MP4 六次失败后没有继续删除 JPG: %d", got)
	}
}

func TestLegacySlot5MigrationNeverDeletesWhenTargetSlotIsInactive(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	state := approvedLegacySlot5State()
	transport := &fakeHID{paramActiveMasks: [][]int{{0, 0, 0, 0, 1}}}
	driver := New("cooler-legacy-target-inactive", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	err := driver.migrateLegacySlot5IfNeededLocked(context.Background(), &state)
	driver.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "未报告物理槽位 4 已激活") {
		t.Fatalf("目标槽位未激活时没有安全停止: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST delMedia 1")); got != 0 {
		t.Fatalf("目标槽位未激活仍删除旧槽位: %d", got)
	}
}

func TestLegacySlot5MigrationKeepsApprovalUntilExactPostcondition(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	state := approvedLegacySlot5State()
	if err := saveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	transport := &fakeHID{paramActiveMasks: [][]int{
		{0, 0, 0, 1, 1},
		{0, 0, 0, 1, 1},
	}}
	driver := New("cooler-legacy-postcondition", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	err := driver.migrateLegacySlot5IfNeededLocked(context.Background(), &state)
	driver.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "未达到安全状态") {
		t.Fatalf("槽位 5 仍活动却误报迁移成功: %v", err)
	}
	persisted, loadErr := loadState(statePath)
	if loadErr != nil || persisted.Slots[4].LegacyAliasMigration != legacySlot5MigrationApproved {
		t.Fatalf("后置条件失败后没有保留 approved: state=%+v err=%v", persisted, loadErr)
	}
}

func TestLegacySlot5ApprovedMigrationResumesAfterCompletionSaveFailure(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	state := approvedLegacySlot5State()
	if err := saveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	firstTransport := &fakeHID{paramActiveMasks: [][]int{
		{0, 0, 0, 1, 1},
		{0, 0, 0, 1, 0},
	}}
	first := New("cooler-approved-save-failure", "冷龙影二代", [2]int{480, 480}, 4, firstTransport, statePath)
	first.pause = func(context.Context, time.Duration) error { return nil }
	first.stateSaver = func(path string, candidate storedState) error {
		if candidate.Slots[4].LegacyAliasMigration == legacySlot5MigrationComplete {
			return errors.New("模拟完成标记落盘前退出")
		}
		return saveState(path, candidate)
	}
	if err := first.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	first.mu.Lock()
	firstErr := first.migrateLegacySlot5IfNeededLocked(context.Background(), &state)
	first.mu.Unlock()
	if firstErr == nil || !strings.Contains(firstErr.Error(), "保存酷冷至尊旧槽位迁移完成状态") {
		t.Fatalf("完成状态保存失败没有明确返回: %v", firstErr)
	}
	persisted, err := loadState(statePath)
	if err != nil || persisted.Slots[4].LegacyAliasMigration != legacySlot5MigrationApproved {
		t.Fatalf("完成落盘失败后没有保留 approved: state=%+v err=%v", persisted, err)
	}

	secondTransport := &fakeHID{paramActiveMasks: [][]int{
		{0, 0, 0, 1, 0},
		{0, 0, 0, 1, 0},
	}}
	restarted := New("cooler-approved-resume", "冷龙影二代", [2]int{480, 480}, 4, secondTransport, statePath)
	restarted.pause = func(context.Context, time.Duration) error { return nil }
	if err := restarted.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted.mu.Lock()
	err = restarted.migrateLegacySlot5IfNeededLocked(context.Background(), &persisted)
	restarted.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := countReportsContaining(secondTransport.writes, []byte("POST delMedia 1")); got != 0 {
		t.Fatalf("续跑时槽位 5 已不活动却重复删除: %d", got)
	}
	completed, err := loadState(statePath)
	if err != nil || completed.Slots[4].LegacyAliasMigration != legacySlot5MigrationComplete {
		t.Fatalf("续跑没有完成落盘: state=%+v err=%v", completed, err)
	}
}

func approvedLegacySlot5State() storedState {
	return storedState{Slots: map[int]storedSlot{4: {
		Digest: "current", FileName: "suspend_3.jpg", ReplaceVersion: offlineReplaceVersion,
		LegacyAliasMigration: legacySlot5MigrationApproved,
	}}}
}

func TestApplyThemeRetriesWholeUploadAfterTransportedTimeoutAndCachesOnlyExplicitCommit(t *testing.T) {
	transport := &fakeHID{suppressTransported: true}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-transported-timeout", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifest := offlineTestManifest("transported-timeout")
	if err := driver.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatalf("首轮 transported 超时后第二轮未恢复: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transport 1")); got != 2 {
		t.Fatalf("没有完整重发 transport: %d", got)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transported 1")); got != 2 {
		t.Fatalf("transported 尝试次数=%d，want 2", got)
	}
	if got := countReportID(transport.writes, protocol.DataReportID); got == 0 || got%2 != 0 {
		t.Fatalf("重试没有重发全部数据块: %d", got)
	}
	if got := countReportsContaining(transport.writes, []byte("POST realtimeDisplay 1\r\nSeqNumber=42")); got != 1 {
		t.Fatalf("最终离线模式没有使用一次原厂 seq 0x2A 命令: %d", got)
	}
	if got := countReportsContaining(transport.writes, []byte(`"enable":true`)); got != 0 {
		t.Fatalf("原厂直接离线路径不应制造实时模式边沿: %d", got)
	}
	if got := countReportsContaining(transport.writes, []byte(`"enable":false`)); got != 1 {
		t.Fatalf("最终离线模式命令次数=%d，want 1", got)
	}
	if got := countReportsContaining(transport.writes, []byte("POST param 1")); got != 1 {
		t.Fatalf("最终离线模式后缺少原厂 param 刷新探测: %d", got)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transported 1\r\nSeqNumber=42")); got != 2 {
		t.Fatalf("transported 没有使用原厂 seq 0x2A: %d", got)
	}
	if got := transport.transportedReadTimeouts; len(got) != 2 || got[0] != offlineResponseTimeout || got[1] != offlineResponseTimeout {
		t.Fatalf("transported 专用 ACK 窗口无效: %v", got)
	}
	offlineReads := 0
	for _, timeout := range transport.readTimeouts {
		if timeout == offlineResponseTimeout {
			offlineReads++
		}
		if timeout == 5*time.Second {
			t.Fatalf("离线上传路径误用了通用 5s 响应窗口: %v", transport.readTimeouts)
		}
	}
	if offlineReads < 10 {
		t.Fatalf("离线 1s 响应窗口次数过少=%d：%v", offlineReads, transport.readTimeouts)
	}
	state, err := loadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if slot, ok := state.Slots[4]; !ok || slot.Digest == "" || slot.FileName != "suspend_3.jpg" ||
		slot.Uncertain || slot.ReplaceVersion != offlineReplaceVersion {
		t.Fatalf("明确恢复成功后没有保存槽位摘要: %+v", state.Slots)
	}
	before := len(transport.writes)
	if err := driver.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatalf("摘要缓存快路径失败: %v", err)
	}
	if len(transport.writes) != before {
		t.Fatal("资源和离线模式均已确认后仍重复写入")
	}
}

func TestApplyThemeAcceptsOriginalTransported200WithoutAckMatch(t *testing.T) {
	transport := &fakeHID{transportedACK: 99}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-transported-status-only", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyTheme(context.Background(), offlineTestManifest("transported-status-only")); err != nil {
		t.Fatalf("原厂 transported response=200 被额外 ACK 条件错误拒绝: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transported 1")); got != 1 {
		t.Fatalf("transported response=200 后仍发生重试: %d", got)
	}
	state, err := loadState(statePath)
	if err != nil || state.Slots[4].Uncertain || state.Slots[4].Digest == "" {
		t.Fatalf("原厂 transported response=200 后未记录已验证提交: state=%+v err=%v", state, err)
	}
}

func TestApplyThemeDrainsShiftedTransportAndDataACKsBeforeTransportedCommit(t *testing.T) {
	transport := &fakeHID{
		suppressNextCommand:     true,
		lateOfflineStageReplies: true,
		suppressTransported:     true,
	}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-late-data-ack", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyTheme(context.Background(), offlineTestManifest("shifted-stage-acks")); err != nil {
		t.Fatalf("阶段错位 ACK 被清理后第二轮未恢复: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transport 1")); got != 2 {
		t.Fatalf("迟到 transport/data 200 被误当成 transported 提交: attempts=%d", got)
	}
	state, err := loadState(statePath)
	if err != nil || state.Slots[4].Digest == "" {
		t.Fatalf("第二轮明确 transported/200 后未缓存: state=%+v err=%v", state, err)
	}
}

func TestPrepareOfflineResourceMapsHumanSlotToOriginalZeroBasedFile(t *testing.T) {
	manifest := offlineTestManifest("slot-mapping")
	for _, test := range []struct {
		slot int
		want string
	}{{1, "suspend_0.jpg"}, {4, "suspend_3.jpg"}, {5, "suspend_4.jpg"}} {
		driver := New("cooler-slot-mapping", "冷龙影二代", [2]int{480, 480}, test.slot, nil, "")
		fileName, _, err := driver.prepareOfflineResource(manifest)
		if err != nil {
			t.Fatalf("槽位 %d 准备资源: %v", test.slot, err)
		}
		if fileName != test.want {
			t.Fatalf("用户槽位 %d 映射为 %q，want %q", test.slot, fileName, test.want)
		}
	}
}

func TestApplyThemeLegacyCacheForcesOneOriginalReplacement(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	manifest := offlineTestManifest("legacy-replace-version")
	first := New("cooler-legacy-establish", "冷龙影二代", [2]int{480, 480}, 4, &fakeHID{}, statePath)
	first.pause = func(context.Context, time.Duration) error { return nil }
	if err := first.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatalf("建立旧缓存前置状态: %v", err)
	}
	legacyState, err := loadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	legacySlot := legacyState.Slots[4]
	// 1 是曾把离线 JPG 错标为媒体类型 0 的实际发布缓存版本。
	legacySlot.ReplaceVersion = 1
	legacyState.Slots[4] = legacySlot
	if err := saveState(statePath, legacyState); err != nil {
		t.Fatal(err)
	}
	transport := &fakeHID{}
	driver := New(
		"cooler-legacy-replace-version", "冷龙影二代", [2]int{480, 480}, 4,
		transport, statePath,
	)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatalf("旧缓存没有完成一次强制替换: %v", err)
	}
	if countReportID(transport.writes, protocol.DataReportID) == 0 {
		t.Fatal("旧缓存错误命中快路径，没有重新上传")
	}
	if got := countReportsContaining(transport.writes, []byte(`"fileName":"suspend_3.jpg"`)); got < 2 {
		t.Fatalf("槽位 4 没有按原厂 0 基文件名重传: %d", got)
	}
	state, err := loadState(statePath)
	if err != nil || state.Slots[4].ReplaceVersion != offlineReplaceVersion {
		t.Fatalf("强制替换后没有升级缓存版本: state=%+v err=%v", state, err)
	}
}

func TestApplyThemeCachedPendingMigrationNeverDeletesOrReuploads(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	manifest := offlineTestManifest("cached-pending-migration")
	first := New("cooler-migration-establish", "冷龙影二代", [2]int{480, 480}, 4, &fakeHID{}, statePath)
	first.pause = func(context.Context, time.Duration) error { return nil }
	if err := first.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	state, err := loadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	slot := state.Slots[4]
	slot.LegacyAliasMigration = legacySlot5MigrationPending
	state.Slots[4] = slot
	if err := saveState(statePath, state); err != nil {
		t.Fatal(err)
	}

	transport := &fakeHID{}
	restarted := New("cooler-migration-resume", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	restarted.pause = func(context.Context, time.Duration) error { return nil }
	if err := restarted.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if got := countReportID(transport.writes, protocol.DataReportID); got != 0 {
		t.Fatalf("已提交资源的 pending 迁移不应重传大文件: %d", got)
	}
	if got := countReportsContaining(transport.writes, []byte("POST delMedia 1")); got != 0 {
		t.Fatalf("缓存快路径把 pending 误当删除授权: %d", got)
	}
	pending, err := loadState(statePath)
	if err != nil || pending.Slots[4].LegacyAliasMigration != legacySlot5MigrationPending {
		t.Fatalf("缓存快路径没有保留 pending: state=%+v err=%v", pending, err)
	}
}

func TestApplyThemePreservesCompletedMigrationAcrossReplacement(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	state := storedState{Slots: map[int]storedSlot{4: {
		Digest: "old", FileName: "suspend_3.jpg", ReplaceVersion: offlineReplaceVersion,
		LegacyAliasMigration: legacySlot5MigrationComplete,
	}}}
	if err := saveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	transport := &fakeHID{}
	driver := New("cooler-complete-preserved", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyTheme(context.Background(), offlineTestManifest("complete-preserved")); err != nil {
		t.Fatal(err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST delMedia 1")); got != 0 {
		t.Fatalf("complete 状态下替换资源仍发送删除: %d", got)
	}
	persisted, err := loadState(statePath)
	if err != nil || persisted.Slots[4].LegacyAliasMigration != legacySlot5MigrationComplete {
		t.Fatalf("资源替换丢失 complete 标记: state=%+v err=%v", persisted, err)
	}
}

func TestApplyThemeStopsAfterFiveTransportedTimeoutsWithoutCaching(t *testing.T) {
	transport := &fakeHID{suppressTransportedCount: offlineUploadAttempts}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-transported-five-timeouts", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := driver.ApplyTheme(context.Background(), offlineTestManifest("transported-five-timeouts"))
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "5 次完整尝试") {
		t.Fatalf("5 次 transported 超时未明确失败: %v", err)
	}
	if strings.Count(err.Error(), "transportObserved=") != offlineUploadAttempts ||
		strings.Count(err.Error(), "transported200=0") != offlineUploadAttempts {
		t.Fatalf("五轮失败缺少逐轮协议诊断: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transport 1")); got != offlineUploadAttempts {
		t.Fatalf("transport 完整尝试次数=%d，want %d", got, offlineUploadAttempts)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transported 1")); got != offlineUploadAttempts {
		t.Fatalf("transported 完整尝试次数=%d，want %d", got, offlineUploadAttempts)
	}
	if got := countReportID(transport.writes, protocol.DataReportID); got == 0 || got%offlineUploadAttempts != 0 {
		t.Fatalf("五轮未完整重发数据块: %d", got)
	}
	if got := countReportsContaining(transport.writes, []byte("POST realtimeDisplay 1")); got != 0 {
		t.Fatalf("没有 transported/200 却仍切换模式: %d", got)
	}
	if len(transport.transportedReadTimeouts) != offlineUploadAttempts {
		t.Fatalf("transported 超时窗口次数=%d，want %d", len(transport.transportedReadTimeouts), offlineUploadAttempts)
	}
	assertStateFileMissing(t, statePath)
}

func TestApplyThemeDrainsUncertainPreviousCommitBeforeDifferentResource(t *testing.T) {
	transport := &fakeHID{suppressTransportedCount: offlineUploadAttempts}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-cross-apply-stale", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyTheme(context.Background(), offlineTestManifest("old-resource")); err == nil {
		t.Fatal("首个资源的5次提交超时被错误接受")
	}
	if !driver.offlineCommitUncertain {
		t.Fatal("提交超时后没有标记跨请求迟到响应风险")
	}
	transport.mu.Lock()
	transport.responses = append(transport.responses, fakeResponse(43))
	transport.mu.Unlock()
	beforeReads := len(transport.readTimeouts)
	newManifest := offlineTestManifest("new-resource")
	newManifest.Layers[0].Options = map[string]any{"color": "#654321ff"}
	if err := driver.ApplyTheme(context.Background(), newManifest); err != nil {
		t.Fatalf("清理上次迟到响应后新资源未成功: %v", err)
	}
	if driver.offlineCommitUncertain {
		t.Fatal("新资源明确提交后仍保留不确定标记")
	}
	newReads := transport.readTimeouts[beforeReads:]
	if !hasConsecutiveTimeouts(newReads, offlineResponseTimeout, 2) {
		t.Fatalf("新资源上传前没有消费旧响应并等待完整 1s 静默窗口: %v", newReads)
	}
}

func TestApplyThemePersistedUncertainStateSurvivesRestartAndForcesRetransmit(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	firstTransport := &fakeHID{}
	first := New("cooler-persisted-uncertain-first", "冷龙影二代", [2]int{480, 480}, 4, firstTransport, statePath)
	first.pause = func(context.Context, time.Duration) error { return nil }
	if err := first.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldManifest := offlineTestManifest("verified-a")
	if err := first.ApplyTheme(context.Background(), oldManifest); err != nil {
		t.Fatalf("建立已验证 A 失败: %v", err)
	}
	firstTransport.suppressTransportedCount = offlineUploadAttempts
	newManifest := offlineTestManifest("uncertain-b")
	newManifest.Layers[0].Type = theme.LayerCanvas
	newManifest.Layers[0].Options = map[string]any{"color": "#abcdef"}
	if err := first.ApplyTheme(context.Background(), newManifest); err == nil {
		t.Fatal("B 的五次提交超时被错误接受")
	}
	state, err := loadState(statePath)
	if err != nil || !state.Slots[4].Uncertain {
		t.Fatalf("B 失败后未持久 uncertain: state=%+v err=%v", state, err)
	}

	// 用新 Driver 模拟进程重启；再选 A 不得命中旧 digest 快路。
	restartedTransport := &fakeHID{}
	restarted := New("cooler-persisted-uncertain-restart", "冷龙影二代", [2]int{480, 480}, 4, restartedTransport, statePath)
	restarted.pause = func(context.Context, time.Duration) error { return nil }
	if err := restarted.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ApplyTheme(context.Background(), oldManifest); err != nil {
		t.Fatalf("重启后恢复 A 失败: %v", err)
	}
	if got := countReportID(restartedTransport.writes, protocol.DataReportID); got == 0 {
		t.Fatal("重启后 uncertain 槽位错误命中旧 A 快路，没有重传")
	}
	state, err = loadState(statePath)
	if err != nil || state.Slots[4].Uncertain {
		t.Fatalf("重传 A 明确提交后未恢复 verified: state=%+v err=%v", state, err)
	}
}

func TestApplyThemeDoesNotTouchUSBWhenPersistingUncertainStateFails(t *testing.T) {
	transport := &fakeHID{}
	driver := New("cooler-uncertain-save-failure", "冷龙影二代", [2]int{480, 480}, 4, transport, t.TempDir()+"/state.json")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	driver.stateSaver = func(string, storedState) error { return errors.New("模拟 uncertain 落盘失败") }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := driver.ApplyTheme(context.Background(), offlineTestManifest("uncertain-save-failure"))
	if err == nil || !strings.Contains(err.Error(), "uncertain 落盘失败") {
		t.Fatalf("预写 uncertain 失败未返回: %v", err)
	}
	if len(transport.writes) != 0 {
		t.Fatalf("uncertain 未持久却已触碰 USB: writes=%d", len(transport.writes))
	}
}

func TestApplyThemeFinalStateSaveFailureLeavesPersistedUncertain(t *testing.T) {
	transport := &fakeHID{}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-final-state-save-failure", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	saveCalls := 0
	driver.stateSaver = func(path string, state storedState) error {
		saveCalls++
		if saveCalls == 1 {
			return saveState(path, state)
		}
		return errors.New("模拟 verified 落盘失败")
	}
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := driver.ApplyTheme(context.Background(), offlineTestManifest("verified-save-failure"))
	if err == nil || !strings.Contains(err.Error(), "verified 落盘失败") {
		t.Fatalf("最终 verified 落盘失败未返回: %v", err)
	}
	if countReportsContaining(transport.writes, []byte("POST transported 1")) != 1 {
		t.Fatal("测试未到达明确 transported/200 后的最终落盘阶段")
	}
	state, loadErr := loadState(statePath)
	if loadErr != nil || !state.Slots[4].Uncertain {
		t.Fatalf("最终落盘失败后未保留 uncertain: state=%+v err=%v", state, loadErr)
	}
}

func TestApplyThemeRetriesExplicitTransportedNACK(t *testing.T) {
	transport := &fakeHID{transportedStatus: 500}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-transported-nack", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyTheme(context.Background(), offlineTestManifest("transported-nack")); err != nil {
		t.Fatalf("首次 transported NACK 后未按原厂完整重试: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transported 1")); got != 2 {
		t.Fatalf("NACK 后 transported 尝试次数=%d，want 2", got)
	}
	state, err := loadState(statePath)
	if err != nil || state.Slots[4].Digest == "" {
		t.Fatalf("第二次 transported/200 后未缓存: state=%+v err=%v", state, err)
	}
}

func TestApplyThemeRetriesMalformedTransportedResponse(t *testing.T) {
	transport := &fakeHID{malformedTransported: true}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-transported-malformed", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyTheme(context.Background(), offlineTestManifest("transported-malformed")); err != nil {
		t.Fatalf("损坏 transported 响应后未按原厂完整重试: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transported 1")); got != 2 {
		t.Fatalf("损坏响应后 transported 尝试次数=%d，want 2", got)
	}
}

func TestApplyThemeDoesNotRecoverTransportedTimeoutAfterCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	transport := &fakeHID{suppressTransported: true, onTransported: cancel}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-transported-canceled", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := driver.ApplyTheme(ctx, offlineTestManifest("transported-canceled"))
	if err == nil {
		t.Fatal("调用方取消后的 transported 超时被错误恢复")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("调用方取消没有原样返回: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transported 1")); got != 1 {
		t.Fatalf("调用方取消后仍重试 transported: %d", got)
	}
	if got := countReportsContaining(transport.writes, []byte("POST realtimeDisplay 1")); got != 0 {
		t.Fatalf("调用方取消后仍切换显示模式: %d", got)
	}
	assertStateFileMissing(t, statePath)
}

func TestApplyThemeDoesNotRecoverTransportedWriteTimeout(t *testing.T) {
	transport := &fakeHID{transportedWriteErr: context.DeadlineExceeded}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-transported-write-timeout", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := driver.ApplyTheme(context.Background(), offlineTestManifest("transported-write-timeout"))
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("transported 写入超时没有原样失败: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST realtimeDisplay 1")); got != 0 {
		t.Fatalf("transported 写入未完成却仍尝试模式切换: %d", got)
	}
	if got := countReportsContaining(transport.writes, []byte("POST transported 1")); got != 1 {
		t.Fatalf("transported 写超时后仍进入下一轮: %d", got)
	}
	assertStateFileMissing(t, statePath)
}

func TestApplyThemeFinalModeCancellationIsNotIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	transport := &fakeHID{onDisplayModeResponse: cancel}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-final-mode-canceled", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifest := offlineTestManifest("final-mode-canceled")
	err := driver.ApplyTheme(ctx, manifest)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("最终模式阶段的调用方取消被吞掉: %v", err)
	}
	state, loadErr := loadState(statePath)
	if loadErr != nil || state.Slots[4].Uncertain || state.Slots[4].Digest == "" {
		t.Fatalf("模式阶段取消后丢失已提交资源: state=%+v err=%v", state, loadErr)
	}
	dataWrites := countReportID(transport.writes, protocol.DataReportID)
	before := len(transport.writes)
	if err := driver.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatalf("取消后已确认的离线快路径失败: %v", err)
	}
	if countReportID(transport.writes, protocol.DataReportID) != dataWrites {
		t.Fatal("模式取消后不必要地重传了已提交资源")
	}
	newWrites := transport.writes[before:]
	if len(newWrites) != 1 ||
		countReportsContaining(newWrites, []byte("POST realtimeDisplay 1\r\nSeqNumber=42")) != 1 {
		t.Fatalf("模式取消后没有只重试一次原厂载入命令: writes=%d", len(newWrites))
	}
}

func TestApplyThemeFinalModeTransportFailureIsNotIgnored(t *testing.T) {
	transport := &fakeHID{displayModeWriteErr: errors.New("HID 已断开")}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-final-mode-io", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifest := offlineTestManifest("final-mode-io")
	err := driver.ApplyTheme(context.Background(), manifest)
	if err == nil || !strings.Contains(err.Error(), "HID 已断开") {
		t.Fatalf("最终模式 HID 故障被吞掉: %v", err)
	}
	if driver.Connected() || driver.Transport != nil {
		t.Fatal("最终模式 HID 故障后没有释放坏会话")
	}
	state, loadErr := loadState(statePath)
	if loadErr != nil || state.Slots[4].Uncertain || state.Slots[4].Digest == "" {
		t.Fatalf("模式 HID 故障后丢失已提交资源: state=%+v err=%v", state, loadErr)
	}
	restartedTransport := &fakeHID{}
	restarted := New("cooler-final-mode-io-restart", "冷龙影二代", [2]int{480, 480}, 4, restartedTransport, statePath)
	restarted.pause = func(context.Context, time.Duration) error { return nil }
	if err := restarted.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatalf("重连后只重试模式失败: %v", err)
	}
	if countReportID(restartedTransport.writes, protocol.DataReportID) != 0 {
		t.Fatal("模式 HID 故障后不必要地重传了已提交资源")
	}
}

func TestApplyThemeReportsFinalModeNACKButKeepsCommittedOfflineState(t *testing.T) {
	transport := &fakeHID{displayModeStatus: 500}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-final-mode-nack", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifest := offlineTestManifest("final-mode-nack")
	err := driver.ApplyTheme(context.Background(), manifest)
	if err == nil || !strings.Contains(err.Error(), "资源已提交") {
		t.Fatalf("最终模式 NACK 被假报为主题已应用: %v", err)
	}
	state, err := loadState(statePath)
	if err != nil || state.Slots[4].Digest == "" {
		t.Fatalf("明确 transported/200 后未缓存: state=%+v err=%v", state, err)
	}
	if driver.realtimeKnown {
		t.Fatal("模式 NACK 后被错误标记为已确认离线")
	}
	dataWrites := countReportID(transport.writes, protocol.DataReportID)
	before := len(transport.writes)
	if err := driver.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatalf("NACK 后已确认的离线快路径失败: %v", err)
	}
	newWrites := transport.writes[before:]
	if countReportsContaining(newWrites, []byte("POST realtimeDisplay 1\r\nSeqNumber=42")) != 1 ||
		countReportID(transport.writes, protocol.DataReportID) != dataWrites {
		t.Fatalf("模式 NACK 后没有只重试载入命令: writes=%d", len(newWrites))
	}
}

func TestApplyThemeFinalModeMalformedResponseIsNotAccepted(t *testing.T) {
	transport := &fakeHID{malformedDisplayMode: true}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-final-mode-malformed", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := driver.ApplyTheme(context.Background(), offlineTestManifest("final-mode-malformed"))
	if err == nil || !strings.Contains(err.Error(), "资源已提交") {
		t.Fatalf("损坏模式回包被当成重复命令纯超时: %v", err)
	}
	state, loadErr := loadState(statePath)
	if loadErr != nil || state.Slots[4].Uncertain || state.Slots[4].Digest == "" {
		t.Fatalf("损坏模式回包后丢失已提交资源: state=%+v err=%v", state, loadErr)
	}
}

func TestApplyThemeCachedVerifiedThemeRecoversDuplicateOfflineModeTimeoutAfterRestart(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	manifest := offlineTestManifest("cached-duplicate-mode-timeout")
	first := New("cooler-cache-establish", "冷龙影二代", [2]int{480, 480}, 4, &fakeHID{}, statePath)
	first.pause = func(context.Context, time.Duration) error { return nil }
	if err := first.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatalf("建立 verified 槽位失败: %v", err)
	}

	restartedTransport := &fakeHID{suppressDisplayMode: true}
	restarted := New("cooler-cache-restart", "冷龙影二代", [2]int{480, 480}, 4, restartedTransport, statePath)
	restarted.pause = func(context.Context, time.Duration) error { return nil }
	if err := restarted.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatalf("重启后重复离线命令超时未通过模式边沿恢复: %v", err)
	}
	if !restarted.realtimeKnown || restarted.realtime {
		t.Fatalf("模式边沿恢复后没有确认离线状态: known=%t realtime=%t", restarted.realtimeKnown, restarted.realtime)
	}
	if countReportID(restartedTransport.writes, protocol.DataReportID) != 0 {
		t.Fatal("重启后 verified 主题因重复 mode 无 ACK 而重传资源")
	}
	if got := countReportsContaining(restartedTransport.writes, []byte("POST realtimeDisplay 1\r\nSeqNumber=42")); got != 3 {
		t.Fatalf("重启后没有发送离线探测和完整模式边沿: %d", got)
	}
	if got := countReportsContaining(restartedTransport.writes, []byte(`"enable":true`)); got != 1 {
		t.Fatalf("重启恢复缺少一次实时模式边沿: %d", got)
	}
	if got := countReportsContaining(restartedTransport.writes, []byte(`"enable":false`)); got != 2 {
		t.Fatalf("重启恢复的离线模式命令次数=%d，want 2", got)
	}
}

func TestApplyThemeCachedVerifiedThemeDoesNotHideOfflineModeNACK(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	manifest := offlineTestManifest("cached-mode-nack")
	first := New("cooler-cache-nack-establish", "冷龙影二代", [2]int{480, 480}, 4, &fakeHID{}, statePath)
	first.pause = func(context.Context, time.Duration) error { return nil }
	if err := first.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatalf("建立 verified 槽位失败: %v", err)
	}

	restartedTransport := &fakeHID{displayModeStatus: 500}
	restarted := New("cooler-cache-nack-restart", "冷龙影二代", [2]int{480, 480}, 4, restartedTransport, statePath)
	restarted.pause = func(context.Context, time.Duration) error { return nil }
	if err := restarted.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := restarted.ApplyTheme(context.Background(), manifest)
	if err == nil || !strings.Contains(err.Error(), "响应状态无效") {
		t.Fatalf("重启后 verified 主题的明确 mode NACK 被隐藏: %v", err)
	}
	if restarted.realtimeKnown {
		t.Fatal("重启后未知模式遇到 NACK，却被错误标记为已确认离线")
	}
	if countReportID(restartedTransport.writes, protocol.DataReportID) != 0 {
		t.Fatal("重启后 mode NACK 导致不必要的资源重传")
	}
}

func TestApplyThemeKnownRealtimeTransitionStillRequiresOfflineModeACK(t *testing.T) {
	transport := &fakeHID{}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-known-realtime-to-offline", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifest := offlineTestManifest("known-realtime-to-offline")
	if err := driver.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatalf("建立 verified 离线主题失败: %v", err)
	}
	driver.mu.Lock()
	driver.realtime = true
	driver.realtimeKnown = true
	driver.mu.Unlock()
	transport.suppressDisplayMode = true
	dataWrites := countReportID(transport.writes, protocol.DataReportID)
	err := driver.ApplyTheme(context.Background(), manifest)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("已知实时→离线的实际转换无 ACK 却被当成重复 false: %v", err)
	}
	if !driver.realtimeKnown || !driver.realtime {
		t.Fatalf("转换未确认时丢失旧模式: known=%t realtime=%t", driver.realtimeKnown, driver.realtime)
	}
	if countReportID(transport.writes, protocol.DataReportID) != dataWrites {
		t.Fatal("已知实时→离线模式失败时重传了 verified 资源")
	}
}

func TestStateAtomicReplaceFailureKeepsPreviousState(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	previous := storedState{Slots: map[int]storedSlot{4: {
		Digest: "old", FileName: "suspend_4.jpg",
	}}}
	if err := saveState(path, previous); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replaceErr := errors.New("模拟原子替换失败")
	err = writeStateFileAtomically(path, []byte("{\"slots\":{}}\n"), func(string, string) error {
		return replaceErr
	})
	if !errors.Is(err, replaceErr) {
		t.Fatalf("原子替换失败未原样返回: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("原子替换失败破坏了旧状态文件")
	}
	temporaryFiles, err := filepath.Glob(filepath.Join(directory, ".coolermaster-state-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporaryFiles) != 0 {
		t.Fatalf("原子替换失败遗留临时文件: %v", temporaryFiles)
	}
}

func TestSaveStateAtomicallyReplacesExistingState(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	if err := saveState(path, storedState{Slots: map[int]storedSlot{4: {
		Digest: "old", FileName: "suspend_4.jpg",
	}}}); err != nil {
		t.Fatal(err)
	}
	want := storedSlot{Digest: "new", FileName: "suspend_4.jpg", Uncertain: true}
	if err := saveState(path, storedState{Slots: map[int]storedSlot{4: want}}); err != nil {
		t.Fatal(err)
	}
	state, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Slots[4]; got != want {
		t.Fatalf("原子替换后状态=%+v，want %+v", got, want)
	}
	temporaryFiles, err := filepath.Glob(filepath.Join(directory, ".coolermaster-state-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporaryFiles) != 0 {
		t.Fatalf("成功替换后遗留临时文件: %v", temporaryFiles)
	}
}

func TestOfflineUploadTimeoutConstantsMatchOriginalAndStayBounded(t *testing.T) {
	// 原厂离线流程的 transport、data 和 transported 读取均是
	// 1000ms，总尝试数是5。额外的静默屏障也必须有独立上限。
	if offlineResponseTimeout != time.Second {
		t.Fatalf("离线响应窗口=%v，want 原厂 1s", offlineResponseTimeout)
	}
	if offlineUploadAttempts != 5 {
		t.Fatalf("离线完整尝试数=%d，want 原厂 5", offlineUploadAttempts)
	}
	if offlineStaleBarrierLimit != 5*time.Second {
		t.Fatalf("离线静默屏障上限=%v，want 5s", offlineStaleBarrierLimit)
	}
	if offlineCommitCooldown != 45*time.Second {
		t.Fatalf("连续离线主题 commit 冷却窗=%v，want 45s", offlineCommitCooldown)
	}
	worstCase := offlineCommitCooldown
	for attempt := 1; attempt < offlineUploadAttempts; attempt++ {
		worstCase += offlineUploadRetryDelay(attempt) + offlineStaleBarrierLimit
	}
	// 为每轮 transport 响应、data 静默、transported 响应以及
	// 固定暂停留出宽松上界，仍远低于 API 15min 作业时限。
	worstCase += offlineUploadAttempts * (2*offlineResponseTimeout + offlineStaleBarrierLimit + 2*time.Second)
	if worstCase >= 15*time.Minute {
		t.Fatalf("离线上传最坏有界窗=%v，超过15min", worstCase)
	}
}

func TestOfflineCommitCooldownDelaysOnlyChangedUploadAndSurvivesRestart(t *testing.T) {
	logicalNow := time.Unix(1_700_000_000, 0)
	statePath := t.TempDir() + "/state.json"
	manifest := offlineTestManifest("cooldown-original")
	firstPauses := make([]time.Duration, 0)
	first := New("cooler-cooldown-first", "冷龙影二代", [2]int{480, 480}, 4, &fakeHID{}, statePath)
	first.now = func() time.Time { return logicalNow }
	first.pause = func(_ context.Context, duration time.Duration) error {
		firstPauses = append(firstPauses, duration)
		logicalNow = logicalNow.Add(duration)
		return nil
	}
	if err := first.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatalf("首个离线主题失败: %v", err)
	}
	for _, duration := range firstPauses {
		if duration >= offlineCommitCooldown {
			t.Fatalf("首次上传错误等待冷却: %v", firstPauses)
		}
	}
	state, err := loadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	storedCommit := time.UnixMilli(state.Slots[4].CommittedAtUnixMilli)
	if state.Slots[4].CommittedAtUnixMilli <= 0 {
		t.Fatal("已验证 commit 时间未持久化")
	}

	restartedTransport := &fakeHID{}
	restartedPauses := make([]time.Duration, 0)
	restarted := New("cooler-cooldown-restarted", "冷龙影二代", [2]int{480, 480}, 4, restartedTransport, statePath)
	restarted.now = func() time.Time { return logicalNow }
	restarted.pause = func(_ context.Context, duration time.Duration) error {
		restartedPauses = append(restartedPauses, duration)
		logicalNow = logicalNow.Add(duration)
		return nil
	}
	if err := restarted.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ApplyTheme(context.Background(), manifest); err != nil {
		t.Fatalf("重启后缓存快路失败: %v", err)
	}
	if countReportID(restartedTransport.writes, protocol.DataReportID) != 0 {
		t.Fatal("重启后缓存快路重传了资源")
	}
	for _, duration := range restartedPauses {
		if duration > time.Second {
			t.Fatalf("缓存快路错误等待了 commit 冷却: %v", restartedPauses)
		}
	}

	restartedPauses = restartedPauses[:0]
	changed := offlineTestManifest("cooldown-changed")
	changed.Layers[0].Type = theme.LayerCanvas
	changed.Layers[0].Options = map[string]any{"color": "#abcdef"}
	expectedCooldown := offlineCommitCooldown - logicalNow.Sub(storedCommit)
	if expectedCooldown <= 0 {
		t.Fatalf("测试逻辑已越过冷却窗: %v", expectedCooldown)
	}
	if err := restarted.ApplyTheme(context.Background(), changed); err != nil {
		t.Fatalf("冷却后连续更换主题失败: %v", err)
	}
	if len(restartedPauses) == 0 || restartedPauses[0] != expectedCooldown {
		t.Fatalf("没有等待跨重启恢复的剩余 commit 冷却: got=%v want=%v", restartedPauses, expectedCooldown)
	}
	if elapsed := logicalNow.Sub(storedCommit); elapsed < offlineCommitCooldown {
		t.Fatalf("新资源在冷却窗内就开始传输: elapsed=%v", elapsed)
	}
}

func TestOfflineUploadTransportedFailuresUseBoundedExponentialBackoff(t *testing.T) {
	logicalNow := time.Unix(1_700_000_000, 0)
	transport := &fakeHID{suppressTransportedCount: offlineUploadAttempts - 1}
	driver := New("cooler-retry-backoff", "冷龙影二代", [2]int{480, 480}, 4, transport, t.TempDir()+"/state.json")
	driver.now = func() time.Time { return logicalNow }
	pauses := make([]time.Duration, 0)
	driver.pause = func(_ context.Context, duration time.Duration) error {
		pauses = append(pauses, duration)
		logicalNow = logicalNow.Add(duration)
		return nil
	}
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyTheme(context.Background(), offlineTestManifest("retry-backoff")); err != nil {
		t.Fatalf("第五轮 transported 成功未恢复: %v", err)
	}
	var got []time.Duration
	for _, duration := range pauses {
		if duration >= offlineRetryInitialDelay {
			got = append(got, duration)
		}
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 12 * time.Second}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("transported 失败退避=%v，want %v（全部暂停=%v）", got, want, pauses)
	}
	if gotAttempts := countReportsContaining(transport.writes, []byte("POST transport 1")); gotAttempts != offlineUploadAttempts {
		t.Fatalf("退避后未保留5次完整重传: %d", gotAttempts)
	}
}

func TestOfflineCommitCooldownCancellationKeepsPreviousVerifiedState(t *testing.T) {
	logicalNow := time.Unix(1_700_000_000, 0)
	transport := &fakeHID{}
	statePath := t.TempDir() + "/state.json"
	driver := New("cooler-cooldown-cancel", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	driver.now = func() time.Time { return logicalNow }
	driver.pause = func(_ context.Context, duration time.Duration) error {
		logicalNow = logicalNow.Add(duration)
		return nil
	}
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyTheme(context.Background(), offlineTestManifest("cooldown-cancel-old")); err != nil {
		t.Fatal(err)
	}
	beforeState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	beforeWrites := len(transport.writes)
	driver.pause = func(ctx context.Context, duration time.Duration) error {
		if duration > time.Second {
			return ctx.Err()
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	changed := offlineTestManifest("cooldown-cancel-new")
	changed.Layers[0].Type = theme.LayerCanvas
	changed.Layers[0].Options = map[string]any{"color": "#fedcba"}
	err = driver.ApplyTheme(ctx, changed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("冷却期取消未原样返回: %v", err)
	}
	afterState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterState, beforeState) {
		t.Fatal("冷却期取消污染了上一份 verified 状态")
	}
	if len(transport.writes) != beforeWrites {
		t.Fatal("冷却期取消后仍写入了 USB")
	}
}

func TestLegacyStateWithoutCommitTimeDoesNotAddCooldown(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	if err := saveState(statePath, storedState{Slots: map[int]storedSlot{4: {
		Digest: "legacy", FileName: "suspend_4.jpg",
	}}}); err != nil {
		t.Fatal(err)
	}
	transport := &fakeHID{}
	driver := New("cooler-legacy-state", "冷龙影二代", [2]int{480, 480}, 4, transport, statePath)
	pauses := make([]time.Duration, 0)
	driver.pause = func(_ context.Context, duration time.Duration) error {
		pauses = append(pauses, duration)
		return nil
	}
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.ApplyTheme(context.Background(), offlineTestManifest("legacy-state-new")); err != nil {
		t.Fatalf("旧 state 无 commit 时间时上传失败: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST delMedia 1")); got != 0 {
		t.Fatalf("普通上传根据历史旧别名擅自删除槽位 5: %d", got)
	}
	persisted, err := loadState(statePath)
	if err != nil || persisted.Slots[4].LegacyAliasMigration != legacySlot5MigrationPending {
		t.Fatalf("普通上传没有只记录 pending: state=%+v err=%v", persisted, err)
	}
	for _, duration := range pauses {
		if duration >= offlineRetryInitialDelay {
			t.Fatalf("旧 state 缺少时间字段却增加冷却: %v", pauses)
		}
	}
}

func TestCommandResponseTimeoutIsTotalStageDeadline(t *testing.T) {
	logicalNow := time.Unix(1_700_000_000, 0)
	transport := &fakeHID{staleOnNextCommand: 64}
	transport.onReadTimeout = func(timeout time.Duration) {
		if timeout > 5*time.Millisecond {
			logicalNow = logicalNow.Add(400 * time.Millisecond)
		}
	}
	driver := New("cooler-stage-deadline", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	driver.now = func() time.Time { return logicalNow }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	_, err := driver.sendCommandLockedTimeout(
		context.Background(), "brightness", protocol.BrightnessCommandSeq,
		map[string]int{"value": 80}, offlineResponseTimeout,
	)
	driver.mu.Unlock()
	if !commandResponseTimedOut(err) {
		t.Fatalf("过期 ACK 将响应窗口逐次重置: %v", err)
	}
	var stageReads []time.Duration
	for _, timeout := range transport.readTimeouts {
		if timeout > 5*time.Millisecond {
			stageReads = append(stageReads, timeout)
		}
	}
	want := []time.Duration{time.Second, 600 * time.Millisecond, 200 * time.Millisecond}
	if len(stageReads) != len(want) {
		t.Fatalf("阶段总窗口读取次数=%d，want %d：%v", len(stageReads), len(want), stageReads)
	}
	for index := range want {
		if stageReads[index] != want[index] {
			t.Fatalf("第 %d 次剩余窗口=%v，want %v", index+1, stageReads[index], want[index])
		}
	}
}

func TestHybridThemeSelectionDoesNotUploadDynamicTheme(t *testing.T) {
	transport := &fakeHID{}
	driver := New("cooler-hybrid", "冷龙影二代", [2]int{480, 480}, 1, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	static := theme.Manifest{
		ID: "static", Name: "静态", Width: 480, Height: 480,
		Layers: []theme.Layer{{
			ID: "background", Type: theme.LayerBackgroundImage,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
		}},
	}
	if driver.UseHostFrames(static) {
		t.Fatal("纯背景主题不应使用实时帧")
	}
	dynamic := static
	dynamic.ID = "dynamic"
	dynamic.Name = "动态"
	dynamic.Layers = append(dynamic.Layers, theme.Layer{
		ID: "ring", Type: theme.LayerAudioRing,
		Bounds: theme.Rect{X: 0.1, Y: 0.1, Width: 0.8, Height: 0.8}, Opacity: 0.8,
	})
	if !driver.UseHostFrames(dynamic) {
		t.Fatal("环状频谱主题应使用实时帧")
	}
	before := len(transport.writes)
	if err := driver.ApplyHostTheme(context.Background(), dynamic); err != nil {
		t.Fatal(err)
	}
	if len(transport.writes) != before {
		t.Fatal("登记动态主题时不应上传离线资源或切换模式")
	}
}

func TestUseHostFramesMatchesOfflineRendererCapabilities(t *testing.T) {
	driver := New("cooler-routing", "冷龙影二代", [2]int{480, 480}, 4, nil, "")
	base := theme.Manifest{
		ID: "routing", Name: "路由", Width: 480, Height: 480,
		Layers: []theme.Layer{{
			ID: "canvas", Type: theme.LayerCanvas,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
		}},
	}
	if driver.UseHostFrames(base) {
		t.Fatal("纯画布不应使用实时帧")
	}
	foregroundTypes := []theme.LayerType{
		theme.LayerTelemetry, theme.LayerAudioBars, theme.LayerAudioRing, theme.LayerLyrics,
		theme.LayerClock, theme.LayerProgressBar, theme.LayerGauge,
		theme.LayerGradientGauge, theme.LayerLineChart,
	}
	for _, layerType := range foregroundTypes {
		manifest := base
		manifest.Layers = append(append([]theme.Layer(nil), base.Layers...), theme.Layer{
			ID: string(layerType), Type: layerType,
			Bounds: theme.Rect{X: .1, Y: .1, Width: .5, Height: .5}, Opacity: 1,
		})
		if !driver.UseHostFrames(manifest) {
			t.Errorf("可见 %s 图层会被离线渲染丢弃，却没有路由到实时帧", layerType)
		}
		manifest.Layers[1].Options = map[string]any{"visible": false}
		if driver.UseHostFrames(manifest) {
			t.Errorf("隐藏的 %s 图层不应强制实时帧", layerType)
		}
		manifest.Layers[1].Options = nil
		manifest.Layers[1].Opacity = 0
		if driver.UseHostFrames(manifest) {
			t.Errorf("透明的 %s 图层不应强制实时帧", layerType)
		}
	}
	for _, layerType := range []theme.LayerType{theme.LayerText, theme.LayerShape} {
		manifest := base
		manifest.Layers = append(append([]theme.Layer(nil), base.Layers...), theme.Layer{
			ID: string(layerType), Type: layerType,
			Bounds: theme.Rect{X: .1, Y: .1, Width: .5, Height: .5}, Opacity: 1,
		})
		if driver.UseHostFrames(manifest) {
			t.Errorf("静态 %s 图层应烘焙进离线 JPEG", layerType)
		}
	}
}

func TestUseHostFramesRejectsOfflineVideoOverlays(t *testing.T) {
	driver := New("cooler-video-routing", "冷龙影二代", [2]int{480, 480}, 4, nil, "")
	manifest := theme.Manifest{
		ID: "video", Name: "视频", Width: 480, Height: 480,
		Resources: []theme.Resource{{ID: "video", LocalPath: "background.mp4"}},
		Layers: []theme.Layer{
			{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1},
			{ID: "video", Type: theme.LayerBackgroundVideo, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"resource_id": "video"}},
		},
	}
	if driver.UseHostFrames(manifest) {
		t.Fatal("单一 MP4 背景应保留离线模式")
	}
	for _, layerType := range []theme.LayerType{theme.LayerImage, theme.LayerText, theme.LayerShape} {
		withOverlay := manifest
		withOverlay.Layers = append(append([]theme.Layer(nil), manifest.Layers...), theme.Layer{
			ID: "overlay-" + string(layerType), Type: layerType,
			Bounds: theme.Rect{X: .1, Y: .1, Width: .5, Height: .5}, Opacity: 1,
		})
		if !driver.UseHostFrames(withOverlay) {
			t.Errorf("MP4 上的 %s 叠加会被原文件直传丢弃，应使用实时帧", layerType)
		}
		withOverlay.Layers[1], withOverlay.Layers[2] = withOverlay.Layers[2], withOverlay.Layers[1]
		if !driver.UseHostFrames(withOverlay) {
			t.Errorf("位于 MP4 前的 %s 叠加也应使用实时帧", layerType)
		}
	}
	withTwoVideos := manifest
	withTwoVideos.Layers = append(append([]theme.Layer(nil), manifest.Layers...), theme.Layer{
		ID: "video-2", Type: theme.LayerBackgroundVideo,
		Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
		Options: map[string]any{"resource_id": "video"},
	})
	if !driver.UseHostFrames(withTwoVideos) {
		t.Fatal("两个可见 MP4 不能同时由离线原文件表达")
	}
}

func TestUseHostFramesRejectsLossyOfflineVideoSettings(t *testing.T) {
	driver := New("cooler-video-settings", "冷龙影二代", [2]int{480, 480}, 4, nil, "")
	newManifest := func() theme.Manifest {
		return theme.Manifest{
			ID: "video-settings", Name: "视频设置", Width: 480, Height: 480,
			Resources: []theme.Resource{{ID: "video", LocalPath: "background.mp4"}},
			Layers: []theme.Layer{
				{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1},
				{ID: "video", Type: theme.LayerBackgroundVideo, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"resource_id": "video", "fit": "cover", "loop": true}},
			},
		}
	}
	if driver.UseHostFrames(newManifest()) {
		t.Fatal("原样全屏循环 MP4 应保留离线模式")
	}
	tests := []struct {
		name  string
		apply func(*theme.Layer)
	}{
		{"非全屏边界", func(layer *theme.Layer) { layer.Bounds = theme.Rect{X: .1, Y: .1, Width: .8, Height: .8} }},
		{"半透明", func(layer *theme.Layer) { layer.Opacity = .5 }},
		{"旋转", func(layer *theme.Layer) { layer.Rotation = 90 }},
		{"contain", func(layer *theme.Layer) { layer.Options["fit"] = "contain" }},
		{"不循环", func(layer *theme.Layer) { layer.Options["loop"] = false }},
	}
	for _, test := range tests {
		manifest := newManifest()
		test.apply(&manifest.Layers[1])
		if !driver.UseHostFrames(manifest) {
			t.Errorf("%s 会被原样 MP4 直传忽略，却没有使用实时帧", test.name)
		}
	}
}

func TestPrepareOfflineResourceRejectsVideoOverlayWhenCalledDirectly(t *testing.T) {
	driver := New("cooler-video-direct", "冷龙影二代", [2]int{480, 480}, 4, nil, "")
	manifest := theme.Manifest{
		ID: "video-direct", Name: "直接离线", Width: 480, Height: 480,
		Resources: []theme.Resource{{ID: "video", LocalPath: "background.mp4"}},
		Layers: []theme.Layer{
			{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1},
			{ID: "video", Type: theme.LayerBackgroundVideo, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"resource_id": "video"}},
			{ID: "text", Type: theme.LayerText, Bounds: theme.Rect{X: .1, Y: .1, Width: .5, Height: .2}, Opacity: 1, Options: map[string]any{"text": "不能丢失"}},
		},
	}
	if _, _, err := driver.prepareOfflineResource(manifest); err == nil {
		t.Fatal("直接准备 MP4 离线资源时静默丢弃了前景")
	}
}

func TestMissingVideoResourceIDDoesNotFallbackToAnotherMP4(t *testing.T) {
	resources := []theme.Resource{{ID: "other", LocalPath: "other.mp4"}}
	if _, ok := findResource(resources, "missing", ".mp4"); ok {
		t.Fatal("不存在的 resource_id 错误回退到另一段 MP4")
	}
	if resource, ok := findResource(resources, "", ".mp4"); !ok || resource.ID != "other" {
		t.Fatal("旧清单没有 resource_id 时应继续按扩展名兼容")
	}
}

func TestHiddenOfflineVideoDoesNotReplaceHumanSlot4JPEG(t *testing.T) {
	driver := New("cooler-hidden-video", "冷龙影二代", [2]int{480, 480}, 4, nil, "")
	manifest := theme.Manifest{
		ID: "hidden-video", Name: "隐藏视频", Width: 480, Height: 480,
		Resources: []theme.Resource{{ID: "missing", LocalPath: "missing.mp4"}},
		Layers: []theme.Layer{
			{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"color": "#123456ff"}},
			{ID: "video", Type: theme.LayerBackgroundVideo, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"resource_id": "missing", "visible": false}},
		},
	}
	fileName, data, err := driver.prepareOfflineResource(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if fileName != "suspend_3.jpg" || len(data) == 0 {
		t.Fatalf("隐藏视频覆盖了槽位 4 的离线 JPEG: file=%q size=%d", fileName, len(data))
	}
}

func TestPrepareOfflineResourceBakesTextAndShapeIntoHumanSlot4JPEG(t *testing.T) {
	driver := New("cooler-static-components", "冷龙影二代", [2]int{240, 240}, 4, nil, "")
	manifest := func(fill, text string) theme.Manifest {
		return theme.Manifest{
			ID: "static-components", Name: "静态组件", Width: 240, Height: 240,
			Layers: []theme.Layer{
				{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"color": "000000"}},
				{ID: "shape", Type: theme.LayerShape, Bounds: theme.Rect{X: .1, Y: .1, Width: .8, Height: .8}, Opacity: 1,
					Options: map[string]any{"shape": "rect", "fill_color": fill, "stroke_width": 0}},
				{ID: "text", Type: theme.LayerText, Bounds: theme.Rect{X: .2, Y: .35, Width: .6, Height: .3}, Opacity: 1,
					Options: map[string]any{"text": text, "color": "FFFFFF", "font_size": 64}},
			},
		}
	}
	fileName, redAData, err := driver.prepareOfflineResource(manifest("FF0000", "A"))
	if err != nil {
		t.Fatal(err)
	}
	if fileName != "suspend_3.jpg" {
		t.Fatalf("静态组件没有写入槽位 4 JPEG: %q", fileName)
	}
	_, greenAData, err := driver.prepareOfflineResource(manifest("00FF00", "A"))
	if err != nil {
		t.Fatal(err)
	}
	_, redWData, err := driver.prepareOfflineResource(manifest("FF0000", "W"))
	if err != nil {
		t.Fatal(err)
	}
	redA := decodeJPEG(t, redAData)
	greenA := decodeJPEG(t, greenAData)
	redPixel := color.NRGBAModel.Convert(redA.At(40, 40)).(color.NRGBA)
	greenPixel := color.NRGBAModel.Convert(greenA.At(40, 40)).(color.NRGBA)
	if redPixel.R < 180 || redPixel.R <= redPixel.G*2 || redPixel.R <= redPixel.B*2 {
		t.Fatalf("Shape 红色设置没有进入离线 JPEG: %+v", redPixel)
	}
	if greenPixel.G < 180 || greenPixel.G <= greenPixel.R*2 || greenPixel.G <= greenPixel.B*2 {
		t.Fatalf("Shape 绿色设置没有进入离线 JPEG: %+v", greenPixel)
	}
	if differentPixels(redA, decodeJPEG(t, redWData)) == 0 {
		t.Fatal("Text 内容变化没有改变离线 JPEG 像素")
	}
}

func TestDisplayGeometryAndRotationInterfaces(t *testing.T) {
	driver := New("cooler-orientation", "冷龙影二代", [2]int{480, 480}, 4, nil, "")
	if err := driver.SetDisplayRotation(-30); err != nil {
		t.Fatal(err)
	}
	if driver.DisplayRotation() != 330 {
		t.Fatalf("方向归一化 = %d，期望 330", driver.DisplayRotation())
	}
	info := driver.Info()
	if info.DisplayShape != "circle_in_hexagon_point_down" || info.DisplayRotation != 330 {
		t.Fatalf("设备屏幕几何信息错误: shape=%q rotation=%d", info.DisplayShape, info.DisplayRotation)
	}
	if key := driver.HostFrameTransformKey(); !strings.Contains(key, "480x480:r330") {
		t.Fatalf("主机帧变换键未包含尺寸与方向: %q", key)
	}
	source := image.NewRGBA(image.Rect(0, 0, 480, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 480; x++ {
			source.SetRGBA(x, y, color.RGBA{R: 240, G: 180, B: 60, A: 255})
		}
	}
	transformed := driver.TransformHostFrame(source)
	corner := color.RGBAModel.Convert(transformed.At(0, 0)).(color.RGBA)
	center := color.RGBAModel.Convert(transformed.At(240, 240)).(color.RGBA)
	if corner != (color.RGBA{A: 255}) || center.R < 200 {
		t.Fatalf("主机帧没有应用圆形遮罩: corner=%v center=%v", corner, center)
	}
}

func TestOfflineStaticJPEGAppliesCircleMask(t *testing.T) {
	driver := New("cooler-static-mask", "冷龙影二代", [2]int{240, 240}, 4, nil, "")
	if err := driver.SetDisplayRotation(30); err != nil {
		t.Fatal(err)
	}
	manifest := theme.Manifest{
		ID: "static-mask", Name: "圆形遮罩", Width: 240, Height: 240,
		Layers: []theme.Layer{{
			ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
			Options: map[string]any{"color": "FFFFFFFF"},
		}},
	}
	fileName, data, err := driver.prepareOfflineResource(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if fileName != "suspend_3.jpg" {
		t.Fatalf("离线静态文件名错误: %q", fileName)
	}
	decoded := decodeJPEG(t, data)
	corner := color.NRGBAModel.Convert(decoded.At(0, 0)).(color.NRGBA)
	center := color.NRGBAModel.Convert(decoded.At(120, 120)).(color.NRGBA)
	if corner.R > 12 || corner.G > 12 || corner.B > 12 || center.R < 230 {
		t.Fatalf("离线 JPEG 未正确应用圆形遮罩: corner=%v center=%v", corner, center)
	}
}

func TestRotatedOfflineVideoRequiresFFmpegInsteadOfSilentDirectUpload(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source.mp4")
	sourceData := []byte("test-video-payload")
	if err := os.WriteFile(sourcePath, sourceData, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := theme.Manifest{
		ID: "rotated-video", Name: "方向视频", Width: 480, Height: 480,
		Resources: []theme.Resource{{ID: "video", LocalPath: sourcePath}},
		Layers: []theme.Layer{
			{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1},
			{ID: "video", Type: theme.LayerBackgroundVideo, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1, Options: map[string]any{"resource_id": "video"}},
		},
	}
	driver := New("cooler-video-rotation", "冷龙影二代", [2]int{480, 480}, 4, nil, filepath.Join(directory, "state.json"))
	fileName, direct, err := driver.prepareOfflineResource(manifest)
	if err != nil || fileName != "suspend_3.mp4" || !bytes.Equal(direct, sourceData) {
		t.Fatalf("0 度离线视频快路失败: file=%q data=%q err=%v", fileName, direct, err)
	}
	if err := driver.SetDisplayRotation(30); err != nil {
		t.Fatal(err)
	}
	driver.SetFFmpegPath(filepath.Join(directory, "missing-ffmpeg.exe"))
	if _, _, err := driver.prepareOfflineResource(manifest); err == nil || !strings.Contains(err.Error(), "方向") {
		t.Fatalf("非 0 度离线视频被静默原样直传: %v", err)
	}
}

func TestShowOrientationCalibrationBypassesSavedRotation(t *testing.T) {
	transport := &fakeHID{}
	driver := New("cooler-calibration", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.SetDisplayRotation(210); err != nil {
		t.Fatal(err)
	}
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.ShowOrientationCalibration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if driver.DisplayRotation() != 210 {
		t.Fatalf("显示校准图意外修改了已保存方向: %d", driver.DisplayRotation())
	}
}

func TestOrientationCalibrationKeepaliveUsesOriginalCommandPairAndRefreshesFrame(t *testing.T) {
	transport := &fakeHID{}
	driver := New("cooler-keepalive", "冷龙影二代", [2]int{480, 480}, 4, transport, t.TempDir()+"/state.json")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	driver.now = func() time.Time { return time.Unix(1_700_000_000, 123_000_000) }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := driver.OrientationCalibrationKeepaliveInterval(); got != 2*time.Second {
		t.Fatalf("保活周期错误: %s", got)
	}
	if err := driver.SendOrientationCalibrationKeepalive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := countReportID(transport.writes, protocol.RequestReportID); got != 2 {
		t.Fatalf("保活请求数量错误: %d", got)
	}
	if got := countReportID(transport.writes, protocol.DataReportID); got == 0 {
		t.Fatal("保活没有重发缓存的测试 JPEG，真机会在 5 秒后恢复离线主题")
	}
	for _, report := range transport.writes {
		if len(report) > 9 && report[0] == protocol.DataReportID && report[9] != protocol.MediaDataJPEG {
			t.Fatalf("方向测试刷新发送了非 JPEG 数据类型: %d", report[9])
		}
	}
	for _, forbidden := range []string{"POST delMedia 1", "POST transport 1", "POST transported 1"} {
		if got := countReportsContaining(transport.writes, []byte(forbidden)); got != 0 {
			t.Fatalf("方向测试刷新不应写入离线槽位: %s=%d", forbidden, got)
		}
	}
	if got := countReportsContaining(transport.writes, []byte("POST timestamp 1")); got != 1 {
		t.Fatalf("timestamp 保活命令数量错误: %d", got)
	}
	if got := countReportsContaining(transport.writes, []byte("{\"timestamp\":1700000000123}")); got != 1 {
		t.Fatalf("timestamp 保活没有使用 Unix 毫秒: %d", got)
	}
	if got := countReportsContaining(transport.writes, []byte("POST realtimeDisplay 1")); got != 1 {
		t.Fatalf("realtimeDisplay 保活命令数量错误: %d", got)
	}
	if got := countReportsContaining(transport.writes, []byte("SeqNumber=42")); got != 2 {
		t.Fatalf("保活命令没有全部使用原厂序号 42: %d", got)
	}
}

func TestOrientationCalibrationKeepaliveStillRenewsModeWhenTimestampACKIsLost(t *testing.T) {
	transport := &fakeHID{suppressNextCommand: true}
	driver := New("cooler-keepalive", "冷龙影二代", [2]int{480, 480}, 4, transport, t.TempDir()+"/state.json")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.SendOrientationCalibrationKeepalive(context.Background()); err != nil {
		t.Fatalf("timestamp 丢 ACK 后没有继续发送模式续期: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST realtimeDisplay 1")); got != 1 {
		t.Fatalf("timestamp 丢 ACK 后 realtimeDisplay 数量错误: %d", got)
	}
}

func TestRealtimeFramesConsumeInboundACKs(t *testing.T) {
	transport := &fakeHID{}
	driver := New("cooler-realtime-acks", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	encoded := solidJPEG(t, 480, 480)
	for frame := 0; frame < 20; frame++ {
		if err := driver.SendJPEG(context.Background(), encoded); err != nil {
			t.Fatalf("第 %d 帧发送失败: %v", frame+1, err)
		}
	}
	transport.mu.Lock()
	pendingResponses := len(transport.responses)
	transport.mu.Unlock()
	if pendingResponses != 0 {
		t.Fatalf("实时帧 ACK 在 HID 入站队列累积: %d", pendingResponses)
	}
	beforeHealth := countReportID(transport.writes, protocol.RequestReportID)
	if err := driver.CheckConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if afterHealth := countReportID(transport.writes, protocol.RequestReportID); afterHealth != beforeHealth {
		t.Fatal("刚完成实时帧后健康检查不应插入 param")
	}
}

func TestRealtimeACKCleanupLimitDoesNotDisconnectHealthyBacklog(t *testing.T) {
	transport := &fakeHID{}
	driver := New("cooler-realtime-ack-limit", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	for range 65 {
		transport.responses = append(transport.responses, fakeResponse(43))
	}
	driver.mu.Lock()
	err := driver.consumeRealtimeResponsesLocked(context.Background())
	driver.mu.Unlock()
	if err != nil {
		t.Fatalf("成功 ACK 积压被误判为断线: %v", err)
	}
	transport.mu.Lock()
	remaining := len(transport.responses)
	transport.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("单次清理应有界保留第 65 条 ACK: %d", remaining)
	}
	driver.mu.Lock()
	err = driver.consumeRealtimeResponsesLocked(context.Background())
	driver.mu.Unlock()
	if err != nil {
		t.Fatalf("下一轮未能继续清理 ACK: %v", err)
	}
	transport.mu.Lock()
	remaining = len(transport.responses)
	transport.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("下一轮清理后仍有 ACK: %d", remaining)
	}
}

func TestSetBrightnessUsesOriginalAtmosV2Command(t *testing.T) {
	transport := &fakeHID{}
	driver := New("cooler-brightness", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	if got := driver.DefaultBrightness(); got != 80 {
		t.Fatalf("原厂默认亮度不同: %d", got)
	}
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, value := range []int{0, 73, 100} {
		before := len(transport.writes)
		if err := driver.SetBrightness(context.Background(), value); err != nil {
			t.Fatalf("设置亮度 %d: %v", value, err)
		}
		writes := transport.writes[before:]
		if len(writes) != 1 || !bytes.Contains(writes[0], []byte("POST brightness 1")) ||
			!bytes.Contains(writes[0], []byte("SeqNumber=44")) ||
			!bytes.Contains(writes[0], []byte(fmt.Sprintf(`{"value":%d}`, value))) {
			t.Fatalf("亮度 %d 报文不符合原厂协议: writes=%d", value, len(writes))
		}
	}
	before := len(transport.writes)
	for _, value := range []int{-1, 101} {
		if err := driver.SetBrightness(context.Background(), value); err == nil {
			t.Fatalf("越界亮度 %d 被接受", value)
		}
	}
	if len(transport.writes) != before {
		t.Fatal("越界亮度仍写入了 HID")
	}
}

func TestSetBrightnessDoesNotTreatLateOfflineModeACKAsBrightnessACK(t *testing.T) {
	transport := &fakeHID{lateModeACKOnBrightness: true}
	driver := New("cooler-brightness-after-offline-mode", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.SetBrightness(context.Background(), 80); err != nil {
		t.Fatalf("跳过迟到模式 ACK 46 后未接受亮度 ACK 45: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST brightness 1")); got != 1 {
		t.Fatalf("跳过错序 ACK 时重复写了亮度命令: %d", got)
	}
}

func TestSetBrightnessReadsTwiceAfterFirstTimeoutWithoutRewriting(t *testing.T) {
	transport := &fakeHID{brightnessFirstTimeout: true}
	driver := New("cooler-brightness-late-ack", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.SetBrightness(context.Background(), 80); err != nil {
		t.Fatalf("首次读超时后第二次 ACK 未恢复: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST brightness 1")); got != 1 {
		t.Fatalf("亮度 ACK 迟到时重复写了命令: %d", got)
	}
}

func TestSetBrightnessStopsAfterTwoResponseTimeouts(t *testing.T) {
	transport := &fakeHID{suppressBrightnessReply: true}
	driver := New("cooler-brightness-two-timeouts", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := driver.SetBrightness(context.Background(), 80)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("两次亮度响应超时未返回: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST brightness 1")); got != 1 {
		t.Fatalf("两次读超时后重复写了亮度命令: %d", got)
	}
	if !driver.Connected() {
		t.Fatal("纯响应超时错误关闭了仍可写会话")
	}
}

func TestSetBrightnessReadsAgainAfterNACK(t *testing.T) {
	transport := &fakeHID{brightnessStatuses: []int{500, 200}}
	driver := New("cooler-brightness-nack-then-ok", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.SetBrightness(context.Background(), 80); err != nil {
		t.Fatalf("首个 NACK 后第二个有效 ACK 未恢复: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST brightness 1")); got != 1 {
		t.Fatalf("NACK 后应只继续读，不应重写: %d", got)
	}
}

func TestSetBrightnessWriteFailureDoesNotRetry(t *testing.T) {
	transport := &fakeHID{brightnessWriteErr: errors.New("亮度 HID 写失败")}
	driver := New("cooler-brightness-write-error", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := driver.SetBrightness(context.Background(), 80)
	if err == nil || !strings.Contains(err.Error(), "亮度 HID 写失败") {
		t.Fatalf("亮度写失败被错误恢复: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST brightness 1")); got != 1 {
		t.Fatalf("亮度写失败后重试了命令: %d", got)
	}
	if driver.Connected() || driver.Transport != nil {
		t.Fatal("亮度 HID 写失败后未释放坏会话")
	}
}

func TestSetBrightnessFailureDisconnectsSession(t *testing.T) {
	transport := &failedHealthHID{}
	driver := New("cooler-brightness-failure", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.SetBrightness(context.Background(), 50); err == nil {
		t.Fatal("亮度命令读取失败后仍报告成功")
	}
	if driver.Connected() || driver.Transport != nil || !transport.closed {
		t.Fatalf("亮度命令失败后没有释放坏会话: connected=%v transport=%v closed=%v", driver.Connected(), driver.Transport != nil, transport.closed)
	}
}

func TestSetBrightnessResponseTimeoutKeepsWritableSession(t *testing.T) {
	transport := &timeoutHealthHID{}
	driver := New("cooler-brightness-timeout", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.SetBrightness(context.Background(), 80); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("亮度 ACK 超时错误=%v，want context deadline exceeded", err)
	}
	if !driver.Connected() || driver.Transport == nil || transport.closed {
		t.Fatalf("ACK 超时错误关闭了仍可写会话: connected=%v closed=%v", driver.Connected(), transport.closed)
	}
}

func TestCommandSkipsManyStaleACKs(t *testing.T) {
	transport := &fakeHID{staleOnNextCommand: 12}
	driver := New("cooler-stale", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.SetRealtimeEnabled(context.Background(), true); err != nil {
		t.Fatalf("连续过期 ACK 不应阻断后续有效响应: %v", err)
	}
}

func TestDisplayModeConfirmationRecoversMissingACK(t *testing.T) {
	transport := &fakeHID{suppressNextCommand: true}
	driver := New("cooler-confirm", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// param 只证明会话仍可通信；仅当驱动原本就确认处于目标模式时，
	// 才能把重复模式命令不回 ACK 视为固件兼容行为。
	driver.mu.Lock()
	driver.realtime = false
	driver.realtimeKnown = true
	driver.mu.Unlock()
	driver.mu.Lock()
	err := driver.setDisplayModeLocked(context.Background(), false, protocol.OfflineCommandSeq, true)
	driver.mu.Unlock()
	if err != nil {
		t.Fatalf("命令 ACK 缺失时没有通过状态查询恢复: %v", err)
	}
}

func TestDisplayModeMissingACKDoesNotUseParamWhenTargetModeIsUnknown(t *testing.T) {
	transport := &fakeHID{suppressNextCommand: true}
	driver := New("cooler-confirm-unknown", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	err := driver.setDisplayModeLocked(context.Background(), false, protocol.OfflineCommandSeq, true)
	driver.mu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("目标模式未知时，命令 ACK 缺失被错误接受: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST param 1")); got != 0 {
		t.Fatalf("目标模式未知时仍用 param 掩盖模式命令失败: %d", got)
	}
}

func TestSetRealtimeUnknownOfflineTransitionStillRequiresACK(t *testing.T) {
	transport := &fakeHID{suppressNextCommand: true}
	driver := New("cooler-unknown-offline-transition", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := driver.SetRealtimeEnabled(context.Background(), false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("未知模式的 false 转换没有 ACK 却被当成重复命令: %v", err)
	}
	if driver.realtimeKnown {
		t.Fatal("未知模式转换纯超时后错误标记为已确认")
	}
}

func TestDisplayModeExplicitNACKIsNotMaskedByParam(t *testing.T) {
	transport := &fakeHID{nextCommandStatus: 500}
	driver := New("cooler-confirm-nack", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	driver.realtime = false
	driver.realtimeKnown = true
	err := driver.setDisplayModeLocked(context.Background(), false, protocol.OfflineCommandSeq, true)
	driver.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "响应状态无效") {
		t.Fatalf("明确的显示模式 NACK 被错误接受: %v", err)
	}
	if got := countReportsContaining(transport.writes, []byte("POST param 1")); got != 0 {
		t.Fatalf("明确 NACK 后仍发送 param 并可能掩盖失败: %d", got)
	}
}

func TestDisplayModeMalformedACKIsNotMaskedByParam(t *testing.T) {
	transport := &fakeHID{malformedNextCommand: true}
	driver := New("cooler-confirm-malformed", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	driver.realtime = false
	driver.realtimeKnown = true
	err := driver.setDisplayModeLocked(context.Background(), false, protocol.OfflineCommandSeq, true)
	driver.mu.Unlock()
	if err == nil {
		t.Fatal("损坏的显示模式 ACK 被错误接受")
	}
	if got := countReportsContaining(transport.writes, []byte("POST param 1")); got != 0 {
		t.Fatalf("损坏 ACK 后仍发送 param 并可能掩盖失败: %d", got)
	}
}

func TestDisplayModeSuccessfulACKSkipsRedundantParamConfirmation(t *testing.T) {
	transport := &fakeHID{}
	driver := New("cooler-confirm-ack", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	err := driver.setDisplayModeLocked(context.Background(), false, protocol.OfflineCommandSeq, true)
	driver.mu.Unlock()
	if err != nil {
		t.Fatalf("显示模式命令已经收到有效 ACK，却仍报告失败: %v", err)
	}
	transport.mu.Lock()
	writes := append([][]byte(nil), transport.writes...)
	transport.mu.Unlock()
	if got := countReportID(writes, protocol.RequestReportID); got != 1 {
		t.Fatalf("有效显示模式 ACK 后仍发送了冗余命令: got=%d want=1", got)
	}
	for _, report := range writes {
		if bytes.Contains(report, []byte("POST param 1")) {
			t.Fatal("有效显示模式 ACK 后不应再发送 param 状态查询")
		}
	}
}

func TestConnectTimeoutDoesNotInstallLateHIDHandleAndCanRetry(t *testing.T) {
	release := make(chan struct{})
	late := &trackedHID{fakeHID: &fakeHID{}, closed: make(chan struct{})}
	var openerCalls atomic.Int32
	driver := New("cooler-open-timeout", "冷龙影二代", [2]int{480, 480}, 4, nil, "")
	driver.ConnectDelay = 0
	driver.Opener = func() (HIDTransport, error) {
		openerCalls.Add(1)
		<-release
		return late, nil
	}
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		err := driver.Connect(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("第 %d 次打开超时未按上下文返回: %v", attempt, err)
		}
	}
	if driver.Connected() || driver.Transport != nil {
		t.Fatalf("超时会话被错误安装: connected=%v transport=%v", driver.Connected(), driver.Transport != nil)
	}
	if got := openerCalls.Load(); got != 1 {
		t.Fatalf("连续超时累积了多个 hidapi 打开协程: %d", got)
	}
	close(release)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatalf("设备恢复后未能重连: %v", err)
	}
	if !driver.Connected() || driver.Transport != late {
		t.Fatalf("恢复句柄未安装: connected=%v transport=%T", driver.Connected(), driver.Transport)
	}
	if got := openerCalls.Load(); got != 1 {
		t.Fatalf("复用 pending 结果时又重复打开: %d", got)
	}
	if err := driver.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-late.closed:
	case <-time.After(time.Second):
		t.Fatal("恢复后的 HID 句柄未在关闭时释放")
	}
}

func TestConcurrentConnectBroadcastsSingleOpenResult(t *testing.T) {
	release := make(chan struct{})
	opened := make(chan struct{})
	transport := &trackedHID{fakeHID: &fakeHID{}, closed: make(chan struct{})}
	var openerCalls atomic.Int32
	driver := New("cooler-open-broadcast", "冷龙影二代", [2]int{480, 480}, 4, nil, "")
	driver.ConnectDelay = 0
	driver.Opener = func() (HIDTransport, error) {
		if openerCalls.Add(1) == 1 {
			close(opened)
		}
		<-release
		return transport, nil
	}

	const callers = 6
	results := make(chan error, callers)
	for range callers {
		go func() { results <- driver.Connect(context.Background()) }()
	}
	select {
	case <-opened:
	case <-time.After(time.Second):
		t.Fatal("HID opener 未启动")
	}
	waitForPendingOpenWaiters(t, driver, callers)
	close(release)
	for index := 0; index < callers; index++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("并发 Connect 第 %d 个等待者失败: %v", index+1, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("并发 Connect 第 %d 个等待者未被广播唤醒", index+1)
		}
	}
	if got := openerCalls.Load(); got != 1 {
		t.Fatalf("并发 Connect 启动了多个 opener: %d", got)
	}
	if !driver.Connected() || driver.Transport != transport {
		t.Fatalf("广播完成后 HID 句柄未唯一安装: connected=%v transport=%T", driver.Connected(), driver.Transport)
	}
	if err := driver.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseWakesConnectAndBlocksReplacementUntilLateOpenerReturns(t *testing.T) {
	releaseFirst := make(chan struct{})
	firstStarted := make(chan struct{})
	late := &trackedHID{fakeHID: &fakeHID{}, closed: make(chan struct{})}
	fresh := &trackedHID{fakeHID: &fakeHID{}, closed: make(chan struct{})}
	var openerCalls atomic.Int32
	driver := New("cooler-open-close-waiter", "冷龙影二代", [2]int{480, 480}, 4, nil, "")
	driver.ConnectDelay = 0
	driver.Opener = func() (HIDTransport, error) {
		switch openerCalls.Add(1) {
		case 1:
			close(firstStarted)
			<-releaseFirst
			return late, nil
		case 2:
			return fresh, nil
		default:
			return nil, errors.New("启动了多余的 HID opener")
		}
	}

	firstResult := make(chan error, 1)
	go func() { firstResult <- driver.Connect(context.Background()) }()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("首个 HID opener 未启动")
	}
	waitForPendingOpenWaiters(t, driver, 1)
	if err := driver.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-firstResult:
		if !errors.Is(err, errTransportOpenCanceled) {
			t.Fatalf("Close 未以取消错误唤醒 Connect: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close 后 Connect 等待者仍阻塞")
	}

	// 旧 opener 仍未返回时，新 Connect 只能观察到已取消的同一 pending，
	// 不允许为同一物理设备再启动一个不可取消 opener。
	if err := driver.Connect(context.Background()); !errors.Is(err, errTransportOpenCanceled) {
		t.Fatalf("Close 后的 Connect 未复用已取消 pending: %v", err)
	}
	if got := openerCalls.Load(); got != 1 {
		t.Fatalf("旧 opener 未返回时启动了第二个 opener: %d", got)
	}

	close(releaseFirst)
	select {
	case <-late.closed:
	case <-time.After(time.Second):
		t.Fatal("Close 后迟到的 HID 句柄未释放")
	}
	waitForPendingOpenCleared(t, driver)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatalf("旧 opener 结束后未能启动新连接: %v", err)
	}
	if got := openerCalls.Load(); got != 2 || driver.Transport != fresh {
		t.Fatalf("旧 opener 结束后的新连接无效: calls=%d transport=%T", got, driver.Transport)
	}
	if err := driver.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseKeepsNeverReturningOpenerAsOnlyPendingTask(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var releaseOnce sync.Once
	var openerCalls atomic.Int32
	driver := New("cooler-open-stuck", "冷龙影二代", [2]int{480, 480}, 4, nil, "")
	driver.ConnectDelay = 0
	driver.Opener = func() (HIDTransport, error) {
		if openerCalls.Add(1) == 1 {
			close(started)
		}
		<-release
		return nil, errors.New("测试清理旧 opener")
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		waitForPendingOpenCleared(t, driver)
	})

	firstResult := make(chan error, 1)
	go func() { firstResult <- driver.Connect(context.Background()) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("HID opener 未启动")
	}
	waitForPendingOpenWaiters(t, driver, 1)
	if err := driver.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-firstResult:
		if !errors.Is(err, errTransportOpenCanceled) {
			t.Fatalf("永不返回 opener 的等待者未被 Close 取消: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close 未唤醒永不返回 opener 的等待者")
	}

	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := driver.Connect(ctx)
		cancel()
		if !errors.Is(err, errTransportOpenCanceled) {
			t.Fatalf("第 %d 次 Close 后连接没有复用已取消 pending: %v", attempt, err)
		}
	}
	if got := openerCalls.Load(); got != 1 {
		t.Fatalf("永不返回的旧 opener 累积了新任务: %d", got)
	}
	driver.mu.Lock()
	pending := driver.pendingOpen
	kept := pending != nil && pending.discarded && !pending.finished
	driver.mu.Unlock()
	if !kept {
		t.Fatal("已取消但未返回的 opener 没有继续占住唯一 pending 槽位")
	}
}

func TestCloseDiscardsPendingLateHIDHandle(t *testing.T) {
	release := make(chan struct{})
	late := &trackedHID{fakeHID: &fakeHID{}, closed: make(chan struct{})}
	driver := New("cooler-open-close", "冷龙影二代", [2]int{480, 480}, 4, nil, "")
	driver.ConnectDelay = 0
	driver.Opener = func() (HIDTransport, error) {
		<-release
		return late, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := driver.Connect(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("打开超时未返回: %v", err)
	}
	if err := driver.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-late.closed:
	case <-time.After(time.Second):
		t.Fatal("关闭驱动后迟到返回的 HID 句柄泄漏")
	}
}

func waitForPendingOpenWaiters(t *testing.T, driver *Driver, expected int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		driver.mu.Lock()
		pending := driver.pendingOpen
		actual := 0
		if pending != nil {
			actual = pending.waiters
		}
		driver.mu.Unlock()
		if actual == expected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending opener 等待者数量超时: got=%d want=%d", actual, expected)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForPendingOpenCleared(t *testing.T, driver *Driver) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		driver.mu.Lock()
		cleared := driver.pendingOpen == nil
		driver.mu.Unlock()
		if cleared {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("pending opener 未在旧调用返回后清理")
		}
		time.Sleep(time.Millisecond)
	}
}

func countReportID(reports [][]byte, reportID byte) int {
	count := 0
	for _, report := range reports {
		if len(report) > 0 && report[0] == reportID {
			count++
		}
	}
	return count
}

func solidJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	frame := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			frame.SetRGBA(x, y, color.RGBA{R: 40, G: 80, B: 120, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, frame, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func offlineTestManifest(id string) theme.Manifest {
	return theme.Manifest{
		ID: id, Name: id, Width: 480, Height: 480,
		Layers: []theme.Layer{{
			ID: "background", Type: theme.LayerBackgroundImage,
			Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
			Options: map[string]any{"color": "#123456ff"},
		}},
	}
}

func assertStateFileMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return
	} else if err != nil {
		t.Fatalf("检查失败传输状态: %v", err)
	}
	state, err := loadState(path)
	if err != nil {
		t.Fatalf("读取失败传输状态: %v", err)
	}
	for slot, stored := range state.Slots {
		if !stored.Uncertain {
			t.Fatalf("失败传输仍写入已验证主题摘要: slot=%d state=%+v", slot, stored)
		}
	}
}

func decodeJPEG(t *testing.T, data []byte) image.Image {
	t.Helper()
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("解码离线 JPEG: %v", err)
	}
	return decoded
}

func differentPixels(first, second image.Image) int {
	bounds := first.Bounds().Intersect(second.Bounds())
	different := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if color.NRGBAModel.Convert(first.At(x, y)) != color.NRGBAModel.Convert(second.At(x, y)) {
				different++
			}
		}
	}
	return different
}

func hasMediaDataKindReport(reports [][]byte, mediaDataKind byte) bool {
	for _, report := range reports {
		if len(report) > 9 && report[0] == protocol.DataReportID && report[9] == mediaDataKind {
			return true
		}
	}
	return false
}

func hasConsecutiveTimeouts(values []time.Duration, want time.Duration, count int) bool {
	if count <= 0 {
		return true
	}
	matched := 0
	for _, value := range values {
		if value == want {
			matched++
			if matched >= count {
				return true
			}
			continue
		}
		matched = 0
	}
	return false
}

func countReportsContaining(reports [][]byte, fragment []byte) int {
	count := 0
	for _, report := range reports {
		if bytes.Contains(report, fragment) {
			count++
		}
	}
	return count
}

func indexReportContaining(reports [][]byte, fragment []byte) int {
	for index, report := range reports {
		if bytes.Contains(report, fragment) {
			return index
		}
	}
	return -1
}

func fakeResponse(ack int) []byte {
	return fakeResponseStatus(ack, 200)
}

func fakeResponseStatus(ack, status int) []byte {
	return fakeResponseStatusBody(ack, status, nil)
}

func fakeSuspendResponse(ack int, mask []int) []byte {
	values := make([]string, len(mask))
	for index, value := range mask {
		values[index] = fmt.Sprint(value)
	}
	body := []byte(`{"suspendMediaActive":[` + strings.Join(values, ",") + `]}`)
	return fakeResponseStatusBody(ack, 200, body)
}

func fakeResponseStatusBody(ack, status int, body []byte) []byte {
	header := fmt.Sprintf("1 %d\r\nAckNumber=%d\r\n", status, ack)
	if len(body) > 0 {
		header += fmt.Sprintf("ContentType=json\r\nContentLength=%d\r\n", len(body))
	}
	payload := append([]byte(header+"\r\n"), body...)
	frameLength := len(payload) + 5
	inner := make([]byte, 0, frameLength)
	inner = append(inner, 0x5A, byte(frameLength>>8), byte(frameLength))
	inner = append(inner, payload...)
	var checksum byte
	for _, value := range inner[1:] {
		checksum += value
	}
	inner = append(inner, checksum, 0x5A)
	escaped := []byte{inner[0]}
	for _, value := range inner[1 : len(inner)-1] {
		switch value {
		case 0x5A:
			escaped = append(escaped, 0x5B, 0x01)
		case 0x5B:
			escaped = append(escaped, 0x5B, 0x02)
		default:
			escaped = append(escaped, value)
		}
	}
	escaped = append(escaped, inner[len(inner)-1])
	report := make([]byte, protocol.ReportSize)
	report[0] = protocol.ResponseReportID
	copy(report[1:], escaped)
	return report
}
