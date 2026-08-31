package edgesetup

import (
	"strings"
	"testing"
)

func TestListenRejectsWildcardByDefault(t *testing.T) {
	if _, err := Listen(ListenOptions{Address: "0.0.0.0:0"}); err == nil || !strings.Contains(err.Error(), "通配") {
		t.Fatalf("通配监听没有默认失败关闭: %v", err)
	}
	if _, err := Listen(ListenOptions{Address: "setup.local:19324"}); err == nil {
		t.Fatal("监听入口接受了主机名")
	}
}

func TestListenAcceptsExplicitLoopback(t *testing.T) {
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if !strings.HasPrefix(listener.Addr().String(), "127.0.0.1:") {
		t.Fatalf("监听地址错误: %s", listener.Addr())
	}
}
