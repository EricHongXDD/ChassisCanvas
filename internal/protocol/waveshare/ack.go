package waveshare

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

const maxResponsePayload = 16 << 20

type deadlineReader interface {
	SetReadDeadline(time.Time) error
}

// CommandAckGate 从同一串口读取屏端 CMD_ACK，并跳过之前未消费的普通遥测 ACK。
type CommandAckGate struct {
	Reader  io.Reader
	Timeout time.Duration
	buffer  *bufio.Reader
}

func NewCommandAckGate(reader io.Reader) *CommandAckGate {
	return &CommandAckGate{Reader: reader, Timeout: 3 * time.Second}
}

func (gate *CommandAckGate) Wait(ctx context.Context, command Command, _ string) (Packet, error) {
	if gate == nil || gate.Reader == nil {
		return Packet{}, errors.New("ACK 读取器未配置")
	}
	if gate.buffer == nil {
		gate.buffer = bufio.NewReaderSize(gate.Reader, 16<<10)
	}
	deadline := time.Now().Add(gate.Timeout)
	if gate.Timeout <= 0 {
		deadline = time.Now().Add(3 * time.Second)
	}
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if reader, ok := gate.Reader.(deadlineReader); ok {
		if err := reader.SetReadDeadline(deadline); err != nil {
			return Packet{}, err
		}
		// SetReadDeadline 可以安全打断已经进入的阻塞 Read。单纯检查
		// ctx.Err() 只能在下一次读取前生效，会让暂停歌词被容量查询挡住。
		stopInterrupt := make(chan struct{})
		interruptDone := make(chan struct{})
		go func() {
			defer close(interruptDone)
			select {
			case <-ctx.Done():
				_ = reader.SetReadDeadline(time.Now())
			case <-stopInterrupt:
			}
		}()
		defer func() {
			close(stopInterrupt)
			<-interruptDone
			_ = reader.SetReadDeadline(time.Time{})
		}()
	}
	for {
		if err := ctx.Err(); err != nil {
			return Packet{}, err
		}
		packet, err := ReadPacket(gate.buffer)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return Packet{}, contextErr
			}
			return Packet{}, err
		}
		if packet.Type != PacketTypeCommandAck || packet.Command != command {
			continue
		}
		return packet, nil
	}
}

// ReadPacket 从字节流中寻找固定头，再读取一个完整包；可从串口噪声后重新同步。
func ReadPacket(reader *bufio.Reader) (Packet, error) {
	matched := 0
	for matched < len(fixedHeader) {
		value, err := reader.ReadByte()
		if err != nil {
			return Packet{}, err
		}
		if value == fixedHeader[matched] {
			matched++
			continue
		}
		if value == fixedHeader[0] {
			matched = 1
		} else {
			matched = 0
		}
	}
	rest := make([]byte, packetHeaderSize-len(fixedHeader))
	if _, err := io.ReadFull(reader, rest); err != nil {
		return Packet{}, err
	}
	payloadLength := binary.LittleEndian.Uint32(rest[8:12])
	if payloadLength > maxResponsePayload {
		return Packet{}, fmt.Errorf("微雪回应载荷过大: %d", payloadLength)
	}
	payload := make([]byte, payloadLength)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return Packet{}, err
	}
	encoded := make([]byte, 0, packetHeaderSize+len(payload))
	encoded = append(encoded, fixedHeader...)
	encoded = append(encoded, rest...)
	encoded = append(encoded, payload...)
	return DecodePacket(encoded)
}

func IsSuccessfulFileAck(packet Packet, remotePath string) bool {
	if packet.Type != PacketTypeCommandAck || packet.Command != CommandFileEnd {
		return false
	}
	values, err := DecodeQStringMap(packet.Payload)
	return err == nil && values["res"] == "1" && values["fileName"] == remotePath
}

func packetBytes(packet Packet) []byte {
	return bytes.Clone(EncodePacket(packet))
}
