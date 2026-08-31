package coolermaster

import (
	"context"
	"errors"
	"testing"
	"time"

	"casescreen/internal/device"
	protocol "casescreen/internal/protocol/coolermaster"
)

type failedHealthHID struct {
	closed bool
}

type timeoutHealthHID struct {
	writes int
	closed bool
}

type deadlineHealthHID struct {
	writes int
	closed bool
}

type malformedHealthHID struct {
	closed bool
}

func (*malformedHealthHID) Write(_ context.Context, data []byte, _ time.Duration) (int, error) {
	return len(data), nil
}

func (*malformedHealthHID) Read(_ context.Context, data []byte, _ time.Duration) (int, error) {
	data[0] = protocol.ResponseReportID
	return len(data), nil
}

func (transport *malformedHealthHID) Close() error {
	transport.closed = true
	return nil
}

func (transport *timeoutHealthHID) Write(_ context.Context, data []byte, _ time.Duration) (int, error) {
	transport.writes++
	return len(data), nil
}

func (*timeoutHealthHID) Read(context.Context, []byte, time.Duration) (int, error) {
	return 0, context.DeadlineExceeded
}

func (transport *timeoutHealthHID) Close() error {
	transport.closed = true
	return nil
}

func (transport *deadlineHealthHID) Write(_ context.Context, data []byte, _ time.Duration) (int, error) {
	transport.writes++
	return len(data), nil
}

func (*deadlineHealthHID) Read(ctx context.Context, _ []byte, _ time.Duration) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func (transport *deadlineHealthHID) Close() error {
	transport.closed = true
	return nil
}

func (*failedHealthHID) Write(_ context.Context, data []byte, _ time.Duration) (int, error) {
	return len(data), nil
}

func (*failedHealthHID) Read(context.Context, []byte, time.Duration) (int, error) {
	return 0, errors.New("HID 已拔出")
}

func (transport *failedHealthHID) Close() error {
	transport.closed = true
	return nil
}

func TestHealthCheckConfirmsPhysicalCoolerMasterSession(t *testing.T) {
	transport := &fakeHID{}
	driver := New("cooler-health", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.CheckConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countReportID(transport.writes, protocol.RequestReportID) != 1 || !driver.Connected() {
		t.Fatalf("健康检查没有发送单次 param: writes=%d connected=%v", len(transport.writes), driver.Connected())
	}
}

func TestHealthCheckDisconnectsFailedCoolerMasterSession(t *testing.T) {
	transport := &failedHealthHID{}
	driver := New("cooler-health", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.CheckConnection(context.Background()); err == nil {
		t.Fatal("拔出后健康检查错误地成功")
	}
	if driver.Connected() || !transport.closed || driver.Transport != nil {
		t.Fatalf("失败会话没有释放: connected=%v closed=%v", driver.Connected(), transport.closed)
	}
}

func TestHealthCheckTimeoutKeepsRecoverableCoolerMasterSession(t *testing.T) {
	transport := &timeoutHealthHID{}
	driver := New("cooler-health-timeout", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		err := driver.CheckConnection(context.Background())
		if !errors.Is(err, device.ErrHealthCheckBusy) {
			t.Fatalf("第 %d 次无响应没有按可恢复探测处理: %v", attempt+1, err)
		}
		if !driver.Connected() || driver.Transport == nil || transport.closed {
			t.Fatalf("第 %d 次 param 超时错误释放了可写 HID 会话", attempt+1)
		}
	}
	if transport.writes != 3 {
		t.Fatalf("超时后没有继续周期探测: writes=%d", transport.writes)
	}
}

func TestHealthCheckCallerDeadlineDoesNotDisconnectWritableSession(t *testing.T) {
	transport := &deadlineHealthHID{}
	driver := New("cooler-health-deadline", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := driver.CheckConnection(ctx)
	cancel()
	if !errors.Is(err, device.ErrHealthCheckBusy) {
		t.Fatalf("健康检查调用方超时被误判为断线: %v", err)
	}
	if transport.writes != 1 || transport.closed || !driver.Connected() || driver.Transport == nil {
		t.Fatalf("调用方超时破坏了可写会话: writes=%d closed=%v connected=%v", transport.writes, transport.closed, driver.Connected())
	}
}

func TestHealthCheckMalformedResponseDoesNotCloseWritableSession(t *testing.T) {
	transport := &malformedHealthHID{}
	driver := New("cooler-health-malformed", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.CheckConnection(context.Background()); err == nil {
		t.Fatal("损坏健康检查回包被错误接受")
	}
	if !driver.Connected() || driver.Transport == nil || transport.closed {
		t.Fatalf("单次损坏回包关闭了可写会话: connected=%v closed=%v", driver.Connected(), transport.closed)
	}
}

func TestHealthCheckBusyDoesNotDisconnectCoolerMaster(t *testing.T) {
	driver := New("cooler-health", "冷龙影二代", [2]int{480, 480}, 4, &fakeHID{}, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	err := driver.CheckConnection(ctx)
	cancel()
	driver.mu.Unlock()
	if !errors.Is(err, device.ErrHealthCheckBusy) || !driver.Connected() {
		t.Fatalf("设备繁忙被误判为断开: err=%v connected=%v", err, driver.Connected())
	}
}

func TestHealthCheckUsesRecentRealtimeWriteAsOnlineEvidence(t *testing.T) {
	transport := &fakeHID{}
	driver := New("cooler-health-realtime", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	driver.realtime = true
	driver.realtimeKnown = true
	driver.lastSuccessfulRealtimeIO = time.Now()
	driver.mu.Unlock()

	if err := driver.CheckConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := countReportID(transport.writes, protocol.RequestReportID); got != 0 {
		t.Fatalf("近期实时帧成功后仍发送 param: %d", got)
	}
}

func TestHealthCheckProbesAfterRealtimeWritesBecomeStale(t *testing.T) {
	transport := &fakeHID{}
	driver := New("cooler-health-stale", "冷龙影二代", [2]int{480, 480}, 4, transport, "")
	driver.pause = func(context.Context, time.Duration) error { return nil }
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	driver.realtime = true
	driver.realtimeKnown = true
	driver.lastSuccessfulRealtimeIO = time.Now().Add(-recentRealtimeIOHealthWindow - time.Second)
	driver.mu.Unlock()

	if err := driver.CheckConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := countReportID(transport.writes, protocol.RequestReportID); got != 1 {
		t.Fatalf("实时帧长时间未成功时没有恢复 param 探测: %d", got)
	}
}
