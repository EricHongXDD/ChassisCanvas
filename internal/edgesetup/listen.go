package edgesetup

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

const defaultListenAddress = "127.0.0.1:19324"

// ListenOptions 控制本地门户的监听范围。AllowWildcard 必须显式设置，
// 默认拒绝 0.0.0.0 和 [::]，避免意外暴露到所有网络。
type ListenOptions struct {
	Address       string
	AllowWildcard bool
}

// Listen 只接受 localhost 或数字 IP，默认仅监听 loopback。
func Listen(options ListenOptions) (net.Listener, error) {
	address := strings.TrimSpace(options.Address)
	if address == "" {
		address = defaultListenAddress
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("设置门户监听地址必须是显式 IP:端口")
	}
	if host != "localhost" {
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if ip == nil {
			return nil, errors.New("设置门户监听地址不接受主机名")
		}
		if ip.IsUnspecified() && !options.AllowWildcard {
			return nil, errors.New("设置门户默认拒绝通配监听地址")
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return nil, errors.New("设置门户监听端口无效")
	}
	return net.Listen("tcp", address)
}
