package waveshare

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"testing"
	"time"

	"casescreen/internal/telemetry"
)

func TestCapturedSystemDataPacket(t *testing.T) {
	want := []byte{
		0x41, 0x41, 0x35, 0x35, 0x31, 0x32, 0x33, 0x34, 0x20, 0x46, 0x49, 0x58, 0x45, 0x44, 0x43, 0x4d,
		0x44, 0x48, 0x45, 0x41, 0x44, 0x20, 0, 0, 0, 0, 1, 0, 0, 0, 0x34, 0, 0, 0, 0x0a, 0x7c, 0x1b, 0xc5,
		0, 0, 0, 3, 0, 0, 0, 6, 0, 0x37, 0, 0x38, 0, 0x39, 0, 0, 0, 2, 0, 0x63,
		0, 0, 0, 6, 0, 0x34, 0, 0x35, 0, 0x36, 0, 0, 0, 2, 0, 0x62,
		0, 0, 0, 6, 0, 0x31, 0, 0x32, 0, 0x33, 0, 0, 0, 2, 0, 0x61,
	}
	got := SystemDataPacket(map[string]string{"123": "a", "456": "b", "789": "c"})
	if !bytes.Equal(got, want) {
		t.Fatalf("编码结果与原厂抓包不同\n got: % x\nwant: % x", got, want)
	}
	packet, err := DecodePacket(got)
	if err != nil {
		t.Fatal(err)
	}
	values, err := DecodeQStringMap(packet.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if values["123"] != "a" || values["789"] != "c" {
		t.Fatalf("键值回读失败: %#v", values)
	}
	if packet.Type != PacketTypeCommand {
		t.Fatalf("包类型=%d", packet.Type)
	}
}

func TestTypicalTelemetryPacketStaysSmall(t *testing.T) {
	gpuLoad := 67.0
	gpuTemperature := 71.0
	snapshot := telemetry.Snapshot{
		CapturedAt: time.Date(2026, 8, 24, 12, 34, 56, 0, time.Local),
		CPUPercent: 42, MemoryPercent: 63, MemoryUsedGiB: 20.3, MemoryTotalGiB: 32,
		CPUModel: "AMD Ryzen 9", GPUPercent: &gpuLoad, GPUTemperature: &gpuTemperature,
		CurrentDecibels: 48, AverageDecibels: 41, MaximumDecibels: 73, Volume: 62,
	}
	packet := SystemDataPacket(snapshot.Values())
	if len(packet) > 1024 {
		t.Fatalf("典型遥测包超过 1 KiB: %d 字节", len(packet))
	}
	t.Logf("典型完整遥测包为 %d 字节", len(packet))
}

func TestUploadMatchesCapturedChunkingAndCRC(t *testing.T) {
	source := bytes.NewReader(bytes.Repeat([]byte{0x5a}, 3629044))
	var output bytes.Buffer
	uploader := Uploader{Writer: &output, Gate: DelayGate{}, ChunkSize: DefaultFileChunkSize, BytesPerSecond: -1}
	checksum, err := uploader.Upload(context.Background(), "/data/theme/1024x600/test.Theme", source)
	if err != nil {
		t.Fatal(err)
	}
	if checksum != crc32.ChecksumIEEE(bytes.Repeat([]byte{0x5a}, 3629044)) {
		t.Fatal("CRC32 不一致")
	}
	startLength := int(binary.LittleEndian.Uint32(output.Bytes()[30:34])) + packetHeaderSize
	start, err := DecodePacket(output.Bytes()[:startLength])
	if err != nil || start.Command != CommandFileStart {
		t.Fatalf("文件开始命令无效: %v", err)
	}
	endBytes := output.Bytes()[startLength+3629044:]
	end, err := DecodePacket(endBytes)
	if err != nil || end.Command != CommandFileEnd {
		t.Fatalf("文件结束命令无效: %v", err)
	}
	rawSize := output.Len() - startLength - len(endBytes)
	if rawSize != 3629044 {
		t.Fatalf("原始文件长度=%d", rawSize)
	}
}

func TestUploadPacingUsesBoundedThroughputBudget(t *testing.T) {
	startedAt := time.Date(2026, 8, 28, 1, 2, 3, 0, time.Local)
	deadline := uploadPaceDeadline(startedAt, 3*DefaultFileBytesPerSecond/2, DefaultFileBytesPerSecond)
	if got := deadline.Sub(startedAt); got != 1500*time.Millisecond {
		t.Fatalf("上传速率预算错误: %v", got)
	}
	if got := uploadPaceDeadline(startedAt, 4095, -1); !got.Equal(startedAt) {
		t.Fatalf("显式关闭节流时仍延迟: %v", got)
	}
}
