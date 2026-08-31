package discovery

type Device struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`
	Port    string `json:"port,omitempty"`
	// StableID 是设备固件提供的强物理身份，与当前 COM 号分离。
	// Windows 上只有通过 USB UniqueID 能力确认的序列号会进入此字段。
	StableID    string `json:"stable_id,omitempty"`
	LibraryPath string `json:"library_path,omitempty"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Slot        *int   `json:"slot,omitempty"`
}

func Find() []Device {
	return findPlatformDevices()
}
