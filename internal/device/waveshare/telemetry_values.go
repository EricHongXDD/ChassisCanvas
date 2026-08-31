package waveshare

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"casescreen/internal/device"
	protocol "casescreen/internal/protocol/waveshare"
	"casescreen/internal/telemetry"
)

const waveshareSpectrumBands = 48

const (
	// 屏端 QSerialPort 按 115200 配置。实时频谱采用 TryLock 丢弃旧帧，
	// 遥测和歌词会自然抢占串口，因此可以使用 85% 的瞬时线速预算；
	// 仍保留 15% 给 ACK 和调度抖动，避免在慢设备上形成待发送队列。
	waveshareConfiguredBaudRate = 115200
	waveshareSerialBitsPerByte  = 10
	waveshareSpectrumLineShare  = 0.85
	waveshareSpectrumACKBytes   = 38
	waveshareSpectrumFloor      = 100 * time.Millisecond
)

// waveshareTelemetryValues 同时保留 CaseScreen 的规范键和 PCMonitorLinux
// 主题实际使用的 system_data_name。屏端不会做中英文键名转换，而是直接查表。
func waveshareTelemetryValues(snapshot telemetry.Snapshot) map[string]string {
	if !snapshot.OmitHardwareMetrics {
		snapshot = enrichWavesharePlatformTelemetry(snapshot)
	}
	values := snapshot.Values()
	aliases := map[string]string{
		"时间":     "time",
		"日期":     "date",
		"星期":     "week",
		"年":      "year",
		"月":      "month",
		"日":      "day",
		"时":      "hour",
		"分":      "minute",
		"秒":      "second",
		"Cpu使用率": "Cpu_Freq",
		"CPU使用率": "Cpu_Freq",
		"Cpu型号":  "Cpu_Model",
		"显卡利用率":  "Gpu_Load",
		"显卡型号":   "Gpu_Model",
		"显卡温度":   "GPU_Temp",
		"显卡热点温度": "GPU_Hotspot_Temp",
		"显存温度":   "GPU_Memory_Temp",
		"内存利用率":  "Memory_Load",
		"已用内存":   "Memory_Used",
		"可用内存":   "Memory_Available",
	}
	for alias, source := range aliases {
		if value, ok := values[source]; ok {
			values[alias] = value
		}
	}
	unitlessAliases := map[string]string{
		"Cpu使用率_数值": "Cpu_Freq",
		"显卡利用率_数值":  "Gpu_Load",
		"内存利用率_数值":  "Memory_Load",
		"显卡温度_数值":   "GPU_Temp",
		"显卡热点温度_数值": "GPU_Hotspot_Temp",
		"显存温度_数值":   "GPU_Memory_Temp",
		"已用内存_数值":   "Memory_Used",
		"可用内存_数值":   "Memory_Available",
	}
	for alias, source := range unitlessAliases {
		if value, ok := values[source]; ok {
			if numeric, valid := waveshareUnitlessValue(value); valid {
				values[alias] = numeric
			}
		}
	}
	appendWaveshareSpectrum(values, snapshot.AudioSpectrum)
	return values
}

// waveshareUnitlessValue 为不显示单位的主题控件生成纯数值字段。
func waveshareUnitlessValue(value string) (string, bool) {
	numeric := strings.TrimSpace(value)
	if numeric == "--" {
		// 屏端进度条和仪表只接受数值。规范键的缺失标记仍保留为
		// "--"，对应纯数值别名显式归零，避免驻留控件保留旧进度。
		return "0", true
	}
	for _, unit := range []string{"GB", "%", "℃"} {
		numeric = strings.TrimSuffix(numeric, unit)
	}
	numeric = strings.TrimSpace(numeric)
	if numeric == "" {
		return "", false
	}
	if _, err := strconv.ParseFloat(numeric, 64); err != nil {
		return "", false
	}
	return numeric, true
}

// appendWaveshareSpectrum 把主机 FFT 频带展开为普通标量键。主题可在 TF 卡中
// 驻留多个进度控件，运行时只需传这些很小的 0..100 数值。
func appendWaveshareSpectrum(values map[string]string, encoded string) {
	if strings.TrimSpace(encoded) == "" {
		return
	}
	parts := strings.Split(encoded, ",")
	peak := 0
	squareSum := 0.0
	for index := 0; index < waveshareSpectrumBands; index++ {
		value := 0
		if index < len(parts) {
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(parts[index]), 64); err == nil {
				value = int(math.Round(math.Max(0, math.Min(100, parsed))))
			}
		}
		values[fmt.Sprintf("Audio_%02d", index)] = strconv.Itoa(value)
		if value > peak {
			peak = value
		}
		squareSum += float64(value * value)
	}
	values["Audio_Level"] = strconv.Itoa(int(math.Round(math.Sqrt(squareSum / waveshareSpectrumBands))))
	values["Audio_Peak"] = strconv.Itoa(peak)
}

// SendSpectrum 高频发送主机生成的频带值。它与主题上传、每秒遥测共用 Driver.mu，
// 因而不会在同一串口上交错写包。
func (driver *Driver) SendSpectrum(ctx context.Context, bars []float64) error {
	values := make(map[string]string, waveshareSpectrumBands+2)
	peak := 0
	squareSum := 0.0
	for index := 0; index < waveshareSpectrumBands; index++ {
		value := 0
		if index < len(bars) {
			value = int(math.Round(math.Max(0, math.Min(1, bars[index])) * 100))
		}
		values[fmt.Sprintf("Audio_%02d", index)] = strconv.Itoa(value)
		if value > peak {
			peak = value
		}
		squareSum += float64(value * value)
	}
	values["Audio_Level"] = strconv.Itoa(int(math.Round(math.Sqrt(squareSum / waveshareSpectrumBands))))
	values["Audio_Peak"] = strconv.Itoa(peak)
	return driver.SendSpectrumValues(ctx, values)
}

// SendSpectrumValues 把多个微雪音频图层的独立频带合并为一个 QStringMap。
// 包内只有短键和 0..100 数值，不包含 JPEG、视频或其他媒体资源。
func (driver *Driver) SendSpectrumValues(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	payload := protocol.EncodeQStringMap(values)
	now := driver.currentSpectrumTime()
	equal, rateLimited := driver.spectrumPayloadState(payload, now)
	if equal {
		return nil
	}
	if rateLimited {
		return device.ErrRealtimeFrameDropped
	}
	// 采样循环会持续产生新帧；串口正在发歌词、主题或查容量时
	// 直接丢弃当前帧，避免旧帧排队并延迟暂停包。
	if !driver.mu.TryLock() {
		return device.ErrRealtimeFrameDropped
	}
	defer driver.mu.Unlock()
	if !driver.connected || driver.Port == nil {
		return fmt.Errorf("微雪屏幕未连接")
	}
	now = driver.currentSpectrumTime()
	equal, rateLimited = driver.spectrumPayloadState(payload, now)
	if equal {
		return nil
	}
	if rateLimited {
		return device.ErrRealtimeFrameDropped
	}
	// 线速预算已经包含完整命令和 ACK 的串口占用时间，因此限速必须按
	// 相邻发送起点计算；若从 ACK 完成后再等待，会把约 58 ms 实机传输
	// 时间重复计算，110 ms 采样会固定丢掉每隔一帧。
	startedAt := now
	_, err := driver.sendAndWaitLocked(ctx, protocol.CommandSystemData, payload)
	if err == nil {
		driver.storeSpectrumPayload(payload, startedAt)
	}
	return err
}

// waveshareSpectrumMinimumInterval 按完整命令包、最小 ACK 和 8N1 线编码
// 估算相邻两帧发送起点的最小间隔。载荷越大帧率越低，而不是继续积压旧帧。
func waveshareSpectrumMinimumInterval(payloadBytes int) time.Duration {
	if payloadBytes < 0 {
		payloadBytes = 0
	}
	onWireBytes := 38 + payloadBytes + waveshareSpectrumACKBytes
	seconds := float64(onWireBytes*waveshareSerialBitsPerByte) /
		(float64(waveshareConfiguredBaudRate) * waveshareSpectrumLineShare)
	interval := time.Duration(math.Ceil(seconds * float64(time.Second)))
	if interval < waveshareSpectrumFloor {
		return waveshareSpectrumFloor
	}
	return interval
}
