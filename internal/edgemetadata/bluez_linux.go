//go:build linux

package edgemetadata

import (
	"context"
	"errors"
	"fmt"

	"casescreen/internal/lyrics"
	"github.com/godbus/dbus/v5"
)

// BlueZSource 从蓝牙 AVRCP 的 MediaPlayer1 读取歌曲元数据。
// 它只读取 D-Bus 属性，不打开声卡、麦克风或任何录音设备。
type BlueZSource struct {
	connect func() (*dbus.Conn, error)
}

func NewBlueZSource() *BlueZSource {
	return &BlueZSource{connect: func() (*dbus.Conn, error) {
		return dbus.SystemBusPrivate()
	}}
}

func (source *BlueZSource) Current(ctx context.Context) (lyrics.NowPlaying, error) {
	if source == nil || source.connect == nil {
		return lyrics.NowPlaying{}, errors.New("BlueZ 元数据源未初始化")
	}
	connection, err := source.connect()
	if err != nil {
		return lyrics.NowPlaying{}, fmt.Errorf("连接系统 D-Bus 失败: %w", err)
	}
	defer connection.Close()
	if err := connection.Auth(nil); err != nil {
		return lyrics.NowPlaying{}, fmt.Errorf("系统 D-Bus 认证失败: %w", err)
	}
	if err := connection.Hello(); err != nil {
		return lyrics.NowPlaying{}, fmt.Errorf("系统 D-Bus Hello 失败: %w", err)
	}
	var objects bluezManagedObjects
	call := connection.Object("org.bluez", dbus.ObjectPath("/")).CallWithContext(
		ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0,
	)
	if call.Err != nil {
		return lyrics.NowPlaying{}, fmt.Errorf("读取 BlueZ 播放器失败: %w", call.Err)
	}
	if err := call.Store(&objects); err != nil {
		return lyrics.NowPlaying{}, fmt.Errorf("解析 BlueZ 播放器失败: %w", err)
	}
	track, ok := nowPlayingFromBlueZ(objects)
	if !ok {
		return lyrics.NowPlaying{}, nil
	}
	return track, nil
}
