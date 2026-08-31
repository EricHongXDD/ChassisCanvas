package service

import (
	"testing"

	"casescreen/internal/device"
)

func TestSortDeviceStatusesUsesSupportedScreenOrder(t *testing.T) {
	statuses := []DeviceStatus{
		{Info: device.Info{ID: "coolermaster-atmos-v2", Name: "酷冷至尊冷龙影二代", Resolution: [2]int{480, 480}, Capabilities: device.Capabilities{DeliveryMode: device.DeliveryHostFrame}}},
		{Info: device.Info{ID: "waveshare-generic", Name: "微雪 USB Monitor", Resolution: [2]int{800, 480}, Capabilities: device.Capabilities{DeliveryMode: device.DeliveryStoredTheme}}},
		{Info: device.Info{ID: "waveshare-8.8", Name: "微雪 8.8 英寸", Resolution: [2]int{480, 1920}, Capabilities: device.Capabilities{DeliveryMode: device.DeliveryStoredTheme}}},
		{Info: device.Info{ID: "waveshare-7", Name: "微雪 7 英寸", Resolution: [2]int{1024, 600}, Capabilities: device.Capabilities{DeliveryMode: device.DeliveryStoredTheme}}},
	}
	sortDeviceStatuses(statuses)
	want := []string{"waveshare-7", "waveshare-8.8", "waveshare-generic", "coolermaster-atmos-v2"}
	for index, id := range want {
		if statuses[index].Info.ID != id {
			t.Fatalf("API 设备顺序错误: got=%+v want=%v", statuses, want)
		}
	}
}
