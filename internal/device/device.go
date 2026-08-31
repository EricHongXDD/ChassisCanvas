package device

import (
	"context"
	"errors"
	"image"
	"time"

	"casescreen/internal/telemetry"
	"casescreen/internal/theme"
)

// ErrRealtimeFrameDropped 表示高频实时帧因为设备正处理更重要的命令而被主动丢弃。
// 这是预期的背压，不代表设备断开；调用方不得把该帧记为已经 ACK。
var ErrRealtimeFrameDropped = errors.New("实时帧因设备繁忙而丢弃")

// ErrHealthCheckBusy 表示驱动正在执行主题传输或实时帧传输，
// 本次健康检查主动跳过。它不能被解读为设备断开。
var ErrHealthCheckBusy = errors.New("设备正忙，跳过健康检查")

// ErrBrightnessUnsupported 表示驱动没有安全、已确认的亮度控制协议。
// 服务会把它作为能力错误返回，而不是猜测命令或把黑色画面冒充关屏。
var ErrBrightnessUnsupported = errors.New("设备不支持亮度控制")

type DeliveryMode string

const (
	DeliveryStoredTheme DeliveryMode = "stored_theme"
	DeliveryHostFrame   DeliveryMode = "host_frame"
)

type Capabilities struct {
	DeliveryMode       DeliveryMode `json:"delivery_mode"`
	StoresResources    bool         `json:"stores_resources"`
	SmallTelemetry     bool         `json:"small_telemetry"`
	HostRenderedFrames bool         `json:"host_rendered_frames"`
	MaxFrameBytes      int          `json:"max_frame_bytes,omitempty"`
}

type Info struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Resolution [2]int `json:"resolution"`
	// DisplayShape 描述编辑器应展示的逻辑可视边界，而不是像素缓冲区外框。
	DisplayShape    string       `json:"display_shape,omitempty"`
	DisplayRotation int          `json:"display_rotation,omitempty"`
	Capabilities    Capabilities `json:"capabilities"`
}

// StorageInfo 是设备本地存储介质的快照。容量单位都是字节。
// Error 只表示最近一次刷新失败，不会使设备断开。
type StorageInfo struct {
	Supported      bool      `json:"supported"`
	TotalBytes     uint64    `json:"total_bytes,omitempty"`
	AvailableBytes uint64    `json:"available_bytes,omitempty"`
	UsedBytes      uint64    `json:"used_bytes,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
	Error          string    `json:"error,omitempty"`
}

// Driver 是后台服务唯一依赖的设备边界。
type Driver interface {
	Info() Info
	Connect(context.Context) error
	ApplyTheme(context.Context, theme.Manifest) error
	SendTelemetry(context.Context, telemetry.Snapshot) error
	Close() error
}

// BrightnessDriver 由具备已确认亮度协议的设备实现。
// 亮度 0 用作关屏意图；恢复值由 DefaultBrightness 返回，不能在服务层猜测。
type BrightnessDriver interface {
	Driver
	SetBrightness(context.Context, int) error
	DefaultBrightness() int
}

// ConnectionStateDriver 让服务在主题传输失败后判断底层句柄是否已经断开。
type ConnectionStateDriver interface {
	Driver
	Connected() bool
}

// HealthCheckDriver 由能向物理设备发送无副作用探测命令的驱动实现。
// 这个边界用来区分“后台还保留旧句柄”和“物理设备真正在线”。
type HealthCheckDriver interface {
	Driver
	CheckConnection(context.Context) error
}

// StorageReporter 由能查询设备本地存储介质的驱动可选实现。
// 刷新失败不代表设备的其他功能不可用。
type StorageReporter interface {
	RefreshStorage(context.Context) error
	StorageInfo() StorageInfo
}

// HostFrameDriver 由不具备屏内主题渲染器的设备实现。
type HostFrameDriver interface {
	Driver
	SendJPEG(context.Context, []byte) error
}

// HostFrameTransformer 在 JPEG 编码前统一处理设备外形遮罩和安装方向。
// TransformKey 必须随变换参数变化，防止不同设备错误复用同一编码帧。
type HostFrameTransformer interface {
	TransformHostFrame(image.Image) image.Image
	HostFrameTransformKey() string
}

// OrientationCalibrationDriver 由支持一次性物理安装方向校准的设备实现。
// 校准图必须绕过当前方向变换，才能让用户看到真实像素方向上的标识。
type OrientationCalibrationDriver interface {
	Driver
	SetDisplayRotation(int) error
	DisplayRotation() int
	ShowOrientationCalibration(context.Context) error
}

// OrientationCalibrationKeepaliveDriver 由方向测试图会被设备端看门狗
// 自动退出的设备实现。服务只在方向校准会话存续期间按驱动给出的周期
// 刷新测试画面。驱动可以发送小型保活包，也可以重发缓存的同一测试帧，
// 但不得修改离线主题或切回普通实时主题。
type OrientationCalibrationKeepaliveDriver interface {
	OrientationCalibrationDriver
	OrientationCalibrationKeepaliveInterval() time.Duration
	SendOrientationCalibrationKeepalive(context.Context) error
}

// HybridFrameDriver 可在设备端离线资源和主机实时帧之间按主题切换。
type HybridFrameDriver interface {
	HostFrameDriver
	UseHostFrames(theme.Manifest) bool
	ApplyHostTheme(context.Context, theme.Manifest) error
}
