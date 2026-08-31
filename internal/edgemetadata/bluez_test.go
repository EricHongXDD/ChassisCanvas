package edgemetadata

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestNowPlayingFromBlueZPrefersPlayingAndMapsQQ(t *testing.T) {
	objects := bluezManagedObjects{
		"/org/bluez/hci0/dev_A/player1": {
			bluezMediaPlayerInterface: {
				"Name":     dbus.MakeVariant("网易云音乐"),
				"Status":   dbus.MakeVariant("paused"),
				"Position": dbus.MakeVariant(uint32(5000)),
				"Track": dbus.MakeVariant(map[string]dbus.Variant{
					"Title": dbus.MakeVariant("暂停歌曲"),
				}),
			},
		},
		"/org/bluez/hci0/dev_B/player0": {
			bluezMediaPlayerInterface: {
				"Name":     dbus.MakeVariant("QQMusic"),
				"Status":   dbus.MakeVariant("playing"),
				"Position": dbus.MakeVariant(uint32(12_345)),
				"Track": dbus.MakeVariant(map[string]dbus.Variant{
					"Title":    dbus.MakeVariant("正在播放"),
					"Artist":   dbus.MakeVariant("测试歌手"),
					"Album":    dbus.MakeVariant("测试专辑"),
					"Duration": dbus.MakeVariant(uint32(240_000)),
				}),
			},
		},
	}

	track, ok := nowPlayingFromBlueZ(objects)
	if !ok {
		t.Fatal("没有解析出蓝牙当前歌曲")
	}
	if track.Source != "qq" || track.Title != "正在播放" || track.Artist != "测试歌手" || !track.Playing {
		t.Fatalf("QQ 蓝牙元数据映射错误: %+v", track)
	}
	if !track.PositionKnown || track.Position != 12_345_000_000 || track.Duration != 240_000_000_000 {
		t.Fatalf("蓝牙时间轴映射错误: %+v", track)
	}
}

func TestNowPlayingFromBlueZMapsNeteaseAndRejectsEmptyTrack(t *testing.T) {
	objects := bluezManagedObjects{
		"/empty": {
			bluezMediaPlayerInterface: {
				"Name":   dbus.MakeVariant("QQMusic"),
				"Status": dbus.MakeVariant("playing"),
				"Track":  dbus.MakeVariant(map[string]dbus.Variant{}),
			},
		},
		"/netease": {
			bluezMediaPlayerInterface: {
				"Name":   dbus.MakeVariant("cloudmusic"),
				"Status": dbus.MakeVariant("paused"),
				"Track": dbus.MakeVariant(map[string]interface{}{
					"Title":  "网易云歌曲",
					"Artist": "网易云歌手",
				}),
			},
		},
	}

	track, ok := nowPlayingFromBlueZ(objects)
	if !ok || track.Source != "netease" || track.Title != "网易云歌曲" || track.Playing {
		t.Fatalf("网易云蓝牙元数据映射错误: ok=%v track=%+v", ok, track)
	}
}

func TestBluetoothUnknownPlayerKeepsGenericSource(t *testing.T) {
	if source := identifyBluetoothPlayer("Audio Player"); source != "bluetooth" {
		t.Fatalf("未知播放器来源错误: %q", source)
	}
}
