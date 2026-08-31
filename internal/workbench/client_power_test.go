package workbench

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	localapi "casescreen/internal/api"
)

func TestClientSetPoweredOffDecodesMultiStatus(t *testing.T) {
	var received struct {
		Target     string `json:"target"`
		PoweredOff bool   `json:"powered_off"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/session":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"token": "power-token", "protocol_version": localapi.ProtocolVersion,
			})
		case "/api/power":
			if request.Method != http.MethodPut || request.Header.Get("X-CaseScreen-Token") != "power-token" {
				t.Fatalf("关屏请求方法/令牌错误: %s %q", request.Method, request.Header.Get("X-CaseScreen-Token"))
			}
			if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
				t.Fatal(err)
			}
			writer.WriteHeader(http.StatusMultiStatus)
			_ = json.NewEncoder(writer).Encode(PowerResult{
				Target: "all", PoweredOff: true, Successes: 1, Failures: 1,
				Results: map[string]string{"waveshare-7": "ok", "coolermaster": "HID 超时"},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := NewAPI(server.URL)
	result, err := client.SetPoweredOff("all", true)
	if err != nil {
		t.Fatal(err)
	}
	if received.Target != "all" || !received.PoweredOff || result.Successes != 1 || result.Failures != 1 || result.Results["coolermaster"] != "HID 超时" {
		t.Fatalf("部分失败响应丢失: received=%+v result=%+v", received, result)
	}
}

func TestClientSetPoweredOffRejectsEmptyTarget(t *testing.T) {
	client := NewAPI("http://127.0.0.1:1")
	if _, err := client.SetPoweredOff(" ", true); err == nil {
		t.Fatal("空目标应在访问服务前被拒绝")
	}
}

func TestClientPowerUsesDedicatedLongTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/session":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"token": "power-token", "protocol_version": localapi.ProtocolVersion,
			})
		case "/api/power":
			time.Sleep(20 * time.Millisecond)
			_ = json.NewEncoder(writer).Encode(PowerResult{
				Target: "all", PoweredOff: true, Successes: 1,
				Results: map[string]string{"waveshare-7": "ok"},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := NewAPI(server.URL)
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	// 普通状态请求的短超时不应截断三屏串行亮度命令。
	client.Client.Timeout = time.Millisecond
	result, err := client.SetPoweredOff("all", true)
	if err != nil || result.Successes != 1 {
		t.Fatalf("关屏专用超时未生效: result=%+v err=%v", result, err)
	}
}
