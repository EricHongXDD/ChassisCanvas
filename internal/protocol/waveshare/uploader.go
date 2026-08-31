package waveshare

import (
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
	"time"
)

const DefaultFileChunkSize = 4095

// DefaultFileBytesPerSecond 限制一次性 TF 资源上传的 USB CDC 突发速率。
// 资源 CRC 未变化时不会进入上传路径，因此该限制只影响首次或变更后的等待时间。
const DefaultFileBytesPerSecond int64 = 512 * 1024

// Phase 表示需要等待屏端回应的上传阶段。
type Phase string

const (
	PhaseFileStart Phase = "file_start"
	PhaseFileEnd   Phase = "file_end"
)

// AckGate 隔离串口 ACK 解析；真实设备模式下不允许省略。
type AckGate interface {
	Wait(context.Context, Command, string) (Packet, error)
}

// Uploader 实现已抓包确认的命令 6 + 原始文件分块 + 命令 7 流程。
type Uploader struct {
	Writer    io.Writer
	Gate      AckGate
	ChunkSize int
	// BytesPerSecond 为 0 时采用安全默认值；负数仅供离线测试关闭节流。
	BytesPerSecond int64
}

// Upload 上传单个资源，返回文件的 IEEE CRC32。
func (u *Uploader) Upload(ctx context.Context, remotePath string, source io.ReadSeeker) (uint32, error) {
	if u.Writer == nil {
		return 0, fmt.Errorf("微雪上传器缺少串口写入器")
	}
	if u.Gate == nil {
		return 0, fmt.Errorf("真实文件上传必须配置屏端 ACK 门控")
	}
	size, checksum, err := InspectSource(source)
	if err != nil {
		return 0, err
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}

	startPayload := EncodeQStringMap(map[string]string{remotePath: strconv.FormatInt(size, 10)})
	if err := writeAll(u.Writer, EncodePacket(Packet{Type: PacketTypeCommand, Command: CommandFileStart, Payload: startPayload})); err != nil {
		return 0, fmt.Errorf("发送文件开始命令: %w", err)
	}
	if _, err := u.Gate.Wait(ctx, CommandFileStart, remotePath); err != nil {
		return 0, fmt.Errorf("等待文件开始 ACK: %w", err)
	}

	chunkSize := u.ChunkSize
	if chunkSize <= 0 {
		chunkSize = DefaultFileChunkSize
	}
	buffer := make([]byte, chunkSize)
	bytesPerSecond := u.BytesPerSecond
	if bytesPerSecond == 0 {
		bytesPerSecond = DefaultFileBytesPerSecond
	}
	startedAt := time.Now()
	var sentBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			if err := writeAll(u.Writer, buffer[:read]); err != nil {
				return 0, fmt.Errorf("发送文件分块: %w", err)
			}
			sentBytes += int64(read)
			if bytesPerSecond > 0 {
				deadline := uploadPaceDeadline(startedAt, sentBytes, bytesPerSecond)
				if delay := time.Until(deadline); delay > 0 {
					timer := time.NewTimer(delay)
					select {
					case <-ctx.Done():
						timer.Stop()
						return 0, ctx.Err()
					case <-timer.C:
					}
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, readErr
		}
	}

	endPayload := EncodeQStringMap(map[string]string{remotePath: strconv.FormatUint(uint64(checksum), 10)})
	if err := writeAll(u.Writer, EncodePacket(Packet{Type: PacketTypeCommand, Command: CommandFileEnd, Payload: endPayload})); err != nil {
		return 0, fmt.Errorf("发送文件结束命令: %w", err)
	}
	ack, err := u.Gate.Wait(ctx, CommandFileEnd, remotePath)
	if err != nil {
		return 0, fmt.Errorf("等待文件结束 ACK: %w", err)
	}
	result, err := DecodeQStringMap(ack.Payload)
	if err != nil {
		return 0, fmt.Errorf("文件结束 ACK 载荷无效: %w", err)
	}
	if result["res"] != "1" || result["fileName"] != remotePath {
		return 0, fmt.Errorf("屏端拒绝文件或路径不匹配: res=%q fileName=%q", result["res"], result["fileName"])
	}
	return checksum, nil
}

func uploadPaceDeadline(startedAt time.Time, sentBytes, bytesPerSecond int64) time.Time {
	if bytesPerSecond <= 0 || sentBytes <= 0 {
		return startedAt
	}
	seconds := float64(sentBytes) / float64(bytesPerSecond)
	return startedAt.Add(time.Duration(seconds * float64(time.Second)))
}

func InspectSource(source io.ReadSeeker) (int64, uint32, error) {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return 0, 0, err
	}
	digest := crc32.NewIEEE()
	size, err := io.Copy(digest, source)
	if err != nil {
		return 0, 0, err
	}
	return size, digest.Sum32(), nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

// DelayGate 只用于离线回放和测试，真实串口适配器需替换为 ACK 解析器。
type DelayGate struct {
	Delay time.Duration
}

func (gate DelayGate) Wait(ctx context.Context, command Command, remotePath string) (Packet, error) {
	timer := time.NewTimer(gate.Delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return Packet{}, ctx.Err()
	case <-timer.C:
		payload := []byte(nil)
		if command == CommandFileEnd {
			payload = EncodeQStringMap(map[string]string{"res": "1", "fileName": remotePath})
		}
		return Packet{Type: PacketTypeCommandAck, Command: command, Payload: payload}, nil
	}
}
