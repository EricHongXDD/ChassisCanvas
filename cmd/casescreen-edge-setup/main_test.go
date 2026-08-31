package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseFlagsDefaultsFailClosed(t *testing.T) {
	options, err := parseFlags(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if options.listenAddress != "127.0.0.1:19324" || options.allowWildcard ||
		options.wifiConfigPath != "" || options.completionMarker != "" {
		t.Fatalf("默认设置入口范围错误: %#v", options)
	}
}

func TestParseFlagsRequiresCompleteWiFiAdapter(t *testing.T) {
	_, err := parseFlags([]string{"-wifi-config", "/tmp/wpa.conf", "-wifi-interface", ""}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "必须指定") {
		t.Fatalf("不完整 Wi-Fi 适配器没有失败关闭: %v", err)
	}
}

func TestHeadlessRunDoesNotPrintBootstrapSecret(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), "complete")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr bytes.Buffer
	err := run(ctx, []string{
		"-listen", "127.0.0.1:0", "-qmlscene", "", "-session-ttl", "1m",
		"-completion-marker", markerPath,
	}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	output := stderr.String()
	if strings.Contains(output, "#setup_token=") || strings.Contains(output, "设置码=") {
		t.Fatalf("日志泄露一次性启动材料: %s", output)
	}
	if _, statErr := os.Stat(markerPath); !os.IsNotExist(statErr) {
		t.Fatalf("取消路径写入了完成标记: %v", statErr)
	}
}

func TestCompletionMarkerWrittenOnlyAfterPortalDone(t *testing.T) {
	directory := t.TempDir()
	markerPath := filepath.Join(directory, "setup.complete")
	if err := os.WriteFile(markerPath, []byte("stale\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := finalizeCompletionMarker(context.Background(), markerPath, portalExitCompleted, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != completionMarkerContent {
		t.Fatalf("完成标记内容=%q want=%q", data, completionMarkerContent)
	}
	info, err := os.Stat(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("完成标记权限=%#o want=0600", info.Mode().Perm())
	}
	leftovers, err := filepath.Glob(filepath.Join(directory, ".casescreen-complete-*.tmp"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("存在完成标记临时文件残留: %v %v", leftovers, err)
	}
}

func TestCompletionMarkerNotWrittenOnTimeoutOrCancellation(t *testing.T) {
	tests := []struct {
		name       string
		context    context.Context
		exitReason portalExitReason
		runErr     error
	}{
		{name: "超时", context: context.Background(), exitReason: portalExitExpired},
		{name: "取消", context: canceledContext(), exitReason: portalExitCompleted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			markerPath := filepath.Join(t.TempDir(), "setup.complete")
			err := finalizeCompletionMarker(test.context, markerPath, test.exitReason, test.runErr)
			if !errors.Is(err, test.runErr) {
				t.Fatalf("返回错误=%v want=%v", err, test.runErr)
			}
			if _, statErr := os.Stat(markerPath); !os.IsNotExist(statErr) {
				t.Fatalf("%s路径写入了完成标记: %v", test.name, statErr)
			}
		})
	}
}

func TestCompletionMarkerNotWrittenWhenServiceFailsAfterDone(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), "setup.complete")
	serviceErr := errors.New("service failed")
	if err := finalizeCompletionMarker(context.Background(), markerPath, portalExitCompleted, serviceErr); !errors.Is(err, serviceErr) {
		t.Fatalf("服务错误丢失: %v", err)
	}
	if _, statErr := os.Stat(markerPath); !os.IsNotExist(statErr) {
		t.Fatalf("服务错误路径写入了完成标记: %v", statErr)
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
