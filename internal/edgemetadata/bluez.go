package edgemetadata

import (
	"sort"
	"strings"

	"casescreen/internal/lyrics"
	"github.com/godbus/dbus/v5"
)

const bluezMediaPlayerInterface = "org.bluez.MediaPlayer1"

// bluezManagedObjects 对应 BlueZ ObjectManager.GetManagedObjects 的返回值。
// 把解析放在跨平台文件中，Windows 构建也能用构造数据验证 AVRCP 元数据映射。
type bluezManagedObjects map[dbus.ObjectPath]map[string]map[string]dbus.Variant

type bluezCandidate struct {
	path   string
	status string
	track  lyrics.NowPlaying
}

func nowPlayingFromBlueZ(objects bluezManagedObjects) (lyrics.NowPlaying, bool) {
	candidates := make([]bluezCandidate, 0)
	for path, interfaces := range objects {
		properties, exists := interfaces[bluezMediaPlayerInterface]
		if !exists {
			continue
		}
		track, valid := bluezTrack(properties)
		if !valid {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(variantString(properties["Status"])))
		track.Playing = status == "playing" || status == "forward-seek" || status == "reverse-seek"
		candidates = append(candidates, bluezCandidate{path: string(path), status: status, track: track})
	}
	if len(candidates) == 0 {
		return lyrics.NowPlaying{}, false
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		leftRank := bluezStatusRank(candidates[left].status)
		rightRank := bluezStatusRank(candidates[right].status)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return candidates[left].path < candidates[right].path
	})
	return candidates[0].track, true
}

func bluezTrack(properties map[string]dbus.Variant) (lyrics.NowPlaying, bool) {
	trackValues := variantMap(properties["Track"])
	title := strings.TrimSpace(variantString(trackValues["Title"]))
	if title == "" {
		return lyrics.NowPlaying{}, false
	}
	name := strings.TrimSpace(variantString(properties["Name"]))
	track := lyrics.NowPlaying{
		Source: identifyBluetoothPlayer(name),
		Title:  title,
		Artist: strings.TrimSpace(variantString(trackValues["Artist"])),
		Album:  strings.TrimSpace(variantString(trackValues["Album"])),
	}
	if durationMS, ok := variantUint64(trackValues["Duration"]); ok {
		track.Duration = int64(durationMS) * 1_000_000
	}
	if positionMS, ok := variantUint64(properties["Position"]); ok {
		track.Position = int64(positionMS) * 1_000_000
		track.PositionKnown = true
	}
	return track, true
}

func bluezStatusRank(status string) int {
	switch status {
	case "playing", "forward-seek", "reverse-seek":
		return 0
	case "paused":
		return 1
	default:
		return 2
	}
}

func identifyBluetoothPlayer(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.Contains(normalized, "qqmusic"), strings.Contains(normalized, "qq music"), strings.Contains(normalized, "qq音乐"):
		return "qq"
	case strings.Contains(normalized, "cloudmusic"), strings.Contains(normalized, "netease"), strings.Contains(normalized, "网易云"):
		return "netease"
	default:
		return "bluetooth"
	}
}

func variantMap(value dbus.Variant) map[string]dbus.Variant {
	switch typed := value.Value().(type) {
	case map[string]dbus.Variant:
		return typed
	case map[string]interface{}:
		output := make(map[string]dbus.Variant, len(typed))
		for key, item := range typed {
			output[key] = dbus.MakeVariant(item)
		}
		return output
	default:
		return map[string]dbus.Variant{}
	}
}

func variantString(value dbus.Variant) string {
	text, _ := value.Value().(string)
	return text
}

func variantUint64(value dbus.Variant) (uint64, bool) {
	switch number := value.Value().(type) {
	case uint8:
		return uint64(number), true
	case uint16:
		return uint64(number), true
	case uint32:
		return uint64(number), true
	case uint64:
		return number, true
	case int:
		if number >= 0 {
			return uint64(number), true
		}
	case int32:
		if number >= 0 {
			return uint64(number), true
		}
	case int64:
		if number >= 0 {
			return uint64(number), true
		}
	}
	return 0, false
}
