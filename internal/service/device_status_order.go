package service

import (
	"sort"
	"strings"

	"casescreen/internal/device"
)

// sortDeviceStatuses 使用握手后的真实设备信息排序，因此即使两块微雪在发现时
// 都只有通用 1024×600 初值，分辨率确认后 API 仍固定为 7 英寸、8.8 英寸、酷冷。
func sortDeviceStatuses(statuses []DeviceStatus) {
	sort.SliceStable(statuses, func(left, right int) bool {
		return deviceStatusRank(statuses[left].Info) < deviceStatusRank(statuses[right].Info)
	})
}

func deviceStatusRank(info device.Info) int {
	identity := strings.ToLower(strings.TrimSpace(info.ID + " " + info.Name))
	isWaveshare := info.Capabilities.DeliveryMode == device.DeliveryStoredTheme ||
		strings.Contains(identity, "waveshare") || strings.Contains(identity, "微雪")
	if isWaveshare {
		if strings.Contains(identity, "8.8") || (info.Resolution[1] > info.Resolution[0] && info.Resolution[1] >= 1000) {
			return 1
		}
		if strings.Contains(identity, "7") || info.Resolution == [2]int{1024, 600} {
			return 0
		}
		return 2
	}
	if info.Capabilities.DeliveryMode == device.DeliveryHostFrame ||
		strings.Contains(identity, "coolermaster") || strings.Contains(identity, "酷冷") {
		return 3
	}
	return 4
}
