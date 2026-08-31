package serialport

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeName(t *testing.T) {
	actual, err := normalizeName(" com17 ")
	if err != nil {
		t.Fatal(err)
	}
	if actual != `\\.\COM17` {
		t.Fatalf("路径=%q", actual)
	}
	if _, err := normalizeName(`C:\temp`); err == nil {
		t.Fatal("不应接受任意设备路径")
	}
}

func TestTransportErrorKeepsStableKindAndCause(t *testing.T) {
	cause := errors.New("底层错误")
	err := &TransportError{Port: "COM3", Operation: "写入", Kind: ErrorKindDisconnected, Err: cause}
	if kind, ok := KindOf(err); !ok || kind != ErrorKindDisconnected {
		t.Fatalf("串口错误分类丢失: kind=%q ok=%v", kind, ok)
	}
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "COM3") || !strings.Contains(err.Error(), "USB 总线断开") {
		t.Fatalf("串口诊断信息不完整: %v", err)
	}
	if !err.Temporary() || err.Timeout() {
		t.Fatalf("掉线错误属性错误: temporary=%v timeout=%v", err.Temporary(), err.Timeout())
	}
}
