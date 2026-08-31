//go:build casescreen_legacy_netease_cdp

package playerhost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNeteaseHostRefusesToRestartRunningClientWithoutPrivatePipe(t *testing.T) {
	launches := 0
	host := newNeteaseHost(neteaseDependencies{
		locateExecutable: func() (string, error) { return `C:\Music\cloudmusic.exe`, nil },
		processRunning:   func() (bool, error) { return true, nil },
		launch: func(string) (io.ReadWriteCloser, error) {
			launches++
			return nil, nil
		},
	})
	status := host.StartAndConnect(context.Background())
	if status.State != NeteaseStateRunningWithoutPipe || !status.Running || status.Connected ||
		!status.RequiresExitOnce || status.CanLaunch || launches != 0 ||
		!strings.Contains(status.Message, "请完全退出网易云") ||
		!strings.Contains(status.Message, "本机播放信息同步") {
		t.Fatalf("已运行客户端没有严格拒绝重启: status=%+v launches=%d", status, launches)
	}
}

func TestNeteaseHostStartsAndSharesFixedPrivatePipe(t *testing.T) {
	var running atomic.Bool
	var launches atomic.Int32
	var targetRequests atomic.Int32
	var serverDone <-chan error
	snapshot := json.RawMessage(`{"stable":true,"sampledAtMs":"1770000000000","status":"Playing","info":{"name":"歌","artists":["歌手"],"album":"专辑","resourceType":"track","resourceId":"123","duration":180000},"progress":{"current":12.5,"duration":180000}}`)
	host := newNeteaseHost(neteaseDependencies{
		locateExecutable: func() (string, error) { return `C:\Music\cloudmusic.exe`, nil },
		processRunning:   func() (bool, error) { return running.Load(), nil },
		launch: func(path string) (io.ReadWriteCloser, error) {
			if path != `C:\Music\cloudmusic.exe` {
				return nil, fmt.Errorf("启动路径被改写: %q", path)
			}
			launches.Add(1)
			running.Store(true)
			client, browser := net.Pipe()
			serverDone = serveFakeNeteasePipe(browser, 2, snapshot, &targetRequests)
			return client, nil
		},
	})
	status := host.StartAndConnect(context.Background())
	if status.State != NeteaseStateReady || !status.Connected || status.Channel != NeteasePrivatePipeChannel ||
		status.Message != "网易云播放信息同步已就绪" || launches.Load() != 1 {
		t.Fatalf("停止状态没有启动并连接私有管道: status=%+v launches=%d", status, launches.Load())
	}
	if targetRequests.Load() != 3 {
		t.Fatalf("orpheus 页面延迟出现时没有只重试空 target: requests=%d", targetRequests.Load())
	}
	payload, err := host.ReadPlayback(context.Background())
	if err != nil || string(payload) != string(snapshot) {
		t.Fatalf("共享主机没有返回固定播放快照: payload=%s err=%v", payload, err)
	}
	status = host.StartAndConnect(context.Background())
	if !status.Connected || launches.Load() != 1 {
		t.Fatalf("已连接状态重复启动: status=%+v launches=%d", status, launches.Load())
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	assertFakeNeteasePipeDone(t, serverDone)
}

func TestNeteaseHostFailsClosedWhenProcessInspectionFails(t *testing.T) {
	launches := 0
	host := newNeteaseHost(neteaseDependencies{
		locateExecutable: func() (string, error) { return `C:\Music\cloudmusic.exe`, nil },
		processRunning:   func() (bool, error) { return false, errors.New("snapshot denied") },
		launch: func(string) (io.ReadWriteCloser, error) {
			launches++
			return nil, nil
		},
	})
	status := host.StartAndConnect(context.Background())
	if status.State != NeteaseStateError || launches != 0 || status.CanLaunch {
		t.Fatalf("进程状态未知时没有失败关闭: status=%+v launches=%d", status, launches)
	}
}

func TestNeteaseStatusDoesNotExposeExecutableTargetOrLaunchError(t *testing.T) {
	host := newNeteaseHost(neteaseDependencies{
		locateExecutable: func() (string, error) { return `C:\Secret\cloudmusic.exe`, nil },
		processRunning:   func() (bool, error) { return false, nil },
		launch: func(string) (io.ReadWriteCloser, error) {
			return nil, errors.New(`CreateProcess C:\Secret\cloudmusic.exe: access denied`)
		},
	})
	status := host.StartAndConnect(context.Background())
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(string(data))
	for _, forbidden := range []string{"secret", "devtools/page", "cookie", "127.0.0.1", "19322", "endpoint"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("播放器状态泄漏了本机路径、端口、target 或登录信息 %q: %s", forbidden, data)
		}
	}
}

func TestNeteasePipeRejectsMultipleOrpheusTargets(t *testing.T) {
	client, browser := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer browser.Close()
		request, err := readNULJSON(browser)
		if err != nil {
			return
		}
		var envelope struct {
			ID int `json:"id"`
		}
		_ = json.Unmarshal(request, &envelope)
		_ = writeNULJSON(browser, map[string]any{
			"id": envelope.ID,
			"result": map[string]any{"targetInfos": []map[string]any{
				{"targetId": "one", "type": "page", "url": "orpheus://orpheus/pub/core.html"},
				{"targetId": "two", "type": "page", "url": "orpheus://orpheus/pub/core.html"},
			}},
		})
		// 保持真实浏览器式长连接，等待被测会话因校验失败主动关闭。
		_, _ = browser.Read(make([]byte, 1))
	}()
	session := newNeteasePipeSession(client)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := session.connect(ctx); err == nil || !strings.Contains(err.Error(), "多个") {
		t.Fatalf("多个 orpheus target 没有失败关闭: %v", err)
	}
	_ = session.Close()
	<-done
}

func TestNeteaseOrpheusURLIsExactOfficialCorePage(t *testing.T) {
	if !isNeteaseOrpheusURL("orpheus://orpheus/pub/core.html") {
		t.Fatal("官方 orpheus core 页面被拒绝")
	}
	for _, invalid := range []string{
		"orpheus://evil/pub/core.html",
		"orpheus://orpheus:80/pub/core.html",
		"orpheus://user@orpheus/pub/core.html",
		"orpheus://orpheus/other.html",
		"orpheus://orpheus/pub/core.html?debug=1",
		"orpheus://orpheus/pub/core.html?",
		"orpheus://orpheus/pub/core.html#fragment",
		"orpheus://orpheus/pub/core.html#",
		"orpheus://orpheus/pub/%63ore.html",
		" orpheus://orpheus/pub/core.html",
		"https://orpheus/pub/core.html",
	} {
		if isNeteaseOrpheusURL(invalid) {
			t.Fatalf("非官方固定 core 页面被接受: %q", invalid)
		}
	}
}

func TestNeteasePipeTimeoutClosesBlockedTransport(t *testing.T) {
	transport := newBlockingTransport()
	session := newNeteasePipeSession(transport)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := session.call(ctx, "Target.getTargets", nil, "")
	if err == nil || time.Since(started) > 250*time.Millisecond || !session.closed() || !transport.closed() {
		t.Fatalf("阻塞写没有被期限解除: elapsed=%s closed=%v err=%v", time.Since(started), transport.closed(), err)
	}
}

func TestNeteasePipeRejectsArbitraryCDPMethod(t *testing.T) {
	transport := newBlockingTransport()
	session := newNeteasePipeSession(transport)
	defer session.Close()
	if _, err := session.call(context.Background(), "Network.getAllCookies", nil, ""); err == nil {
		t.Fatal("任意 CDP 方法被私有管道接受")
	}
}

func serveFakeNeteasePipe(connection net.Conn, emptyTargets int, snapshot json.RawMessage,
	targetRequests *atomic.Int32) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer connection.Close()
		reader := bufio.NewReader(connection)
		for {
			message, err := reader.ReadBytes(0)
			if err != nil {
				if errors.Is(err, io.EOF) || strings.Contains(strings.ToLower(err.Error()), "closed") {
					done <- nil
				} else {
					done <- err
				}
				return
			}
			message = message[:len(message)-1]
			var request struct {
				ID        int             `json:"id"`
				Method    string          `json:"method"`
				Params    json.RawMessage `json:"params"`
				SessionID string          `json:"sessionId"`
			}
			if err := json.Unmarshal(message, &request); err != nil {
				done <- err
				return
			}
			var result any
			switch request.Method {
			case "Target.getTargets":
				count := int(targetRequests.Add(1))
				targets := []map[string]any{}
				if count > emptyTargets {
					targets = append(targets, map[string]any{
						"targetId": "orpheus-page", "type": "page", "url": "orpheus://orpheus/pub/core.html",
					})
				}
				result = map[string]any{"targetInfos": targets}
			case "Target.attachToTarget":
				var params struct {
					TargetID string `json:"targetId"`
					Flatten  bool   `json:"flatten"`
				}
				if json.Unmarshal(request.Params, &params) != nil || params.TargetID != "orpheus-page" || !params.Flatten || request.SessionID != "" {
					done <- errors.New("Target.attachToTarget 参数不是固定页面/flatten")
					return
				}
				result = map[string]any{"sessionId": "private-session"}
			case "Runtime.evaluate":
				var params struct {
					Expression    string `json:"expression"`
					ReturnByValue bool   `json:"returnByValue"`
				}
				if json.Unmarshal(request.Params, &params) != nil || request.SessionID != "private-session" ||
					params.Expression != neteasePlaybackExpression || !params.ReturnByValue {
					done <- errors.New("Runtime.evaluate 不是固定只读表达式或 session")
					return
				}
				lowerExpression := strings.ToLower(params.Expression)
				for _, forbidden := range []string{"cookie", "playlist", "history", "account", "dispatch("} {
					if strings.Contains(lowerExpression, forbidden) {
						done <- fmt.Errorf("固定表达式读取了禁止字段 %q", forbidden)
						return
					}
				}
				result = map[string]any{"result": map[string]any{"type": "object", "value": snapshot}}
			default:
				done <- fmt.Errorf("收到非固定 CDP 方法 %q", request.Method)
				return
			}
			if err := writeNULJSON(connection, map[string]any{"id": request.ID, "result": result}); err != nil {
				done <- err
				return
			}
		}
	}()
	return done
}

func readNULJSON(reader io.Reader) ([]byte, error) {
	message, err := bufio.NewReader(reader).ReadBytes(0)
	if err != nil {
		return nil, err
	}
	return message[:len(message)-1], nil
}

func writeNULJSON(writer io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeAll(writer, append(data, 0))
}

func assertFakeNeteasePipeDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fake 网易云私有管道没有退出")
	}
}

type blockingTransport struct {
	done chan struct{}
	once sync.Once
}

func newBlockingTransport() *blockingTransport {
	return &blockingTransport{done: make(chan struct{})}
}

func (transport *blockingTransport) Read([]byte) (int, error) {
	<-transport.done
	return 0, io.ErrClosedPipe
}

func (transport *blockingTransport) Write([]byte) (int, error) {
	<-transport.done
	return 0, io.ErrClosedPipe
}

func (transport *blockingTransport) Close() error {
	transport.once.Do(func() { close(transport.done) })
	return nil
}

func (transport *blockingTransport) closed() bool {
	select {
	case <-transport.done:
		return true
	default:
		return false
	}
}
