package waveshare

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestCommandAckGateSkipsStaleTelemetryAck(t *testing.T) {
	stream := bytes.NewBufferString("noise")
	stream.Write(packetBytes(Packet{Type: PacketTypeCommandAck, Command: CommandSystemData}))
	stream.Write(packetBytes(Packet{Type: PacketTypeCommandAck, Command: CommandFileStart}))
	gate := NewCommandAckGate(stream)
	packet, err := gate.Wait(context.Background(), CommandFileStart, "/data/test.Theme")
	if err != nil {
		t.Fatal(err)
	}
	if packet.Command != CommandFileStart || packet.Type != PacketTypeCommandAck {
		t.Fatalf("得到错误 ACK: %+v", packet)
	}
}

func TestCapturedFileEndAck(t *testing.T) {
	remotePath := "/data/theme/1024x600/070-Car.Theme"
	payload := EncodeQStringMap(map[string]string{"res": "1", "fileName": remotePath})
	encoded := EncodePacket(Packet{Type: PacketTypeCommandAck, Command: CommandFileEnd, Payload: payload})
	packet, err := ReadPacket(bufio.NewReader(bytes.NewReader(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	if !IsSuccessfulFileAck(packet, remotePath) {
		t.Fatalf("未识别成功文件 ACK: %+v", packet)
	}
}

func TestCommandAckGateCancellationInterruptsBlockedRead(t *testing.T) {
	reader, writer := net.Pipe()
	defer reader.Close()
	defer writer.Close()
	gate := NewCommandAckGate(reader)
	gate.Timeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := gate.Wait(ctx, CommandGetThemes, "")
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("阻塞读取取消错误: %v", err)
		}
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			t.Fatalf("取消没有及时打断阻塞读取: %v", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("取消后 ACK 读取仍在阻塞")
	}

	// 取消结束时必须恢复底层 deadline，同一个 gate 随后仍能正常收包。
	go func() {
		_, _ = writer.Write(packetBytes(Packet{Type: PacketTypeCommandAck, Command: CommandSystemData}))
	}()
	packet, err := gate.Wait(context.Background(), CommandSystemData, "")
	if err != nil || packet.Command != CommandSystemData {
		t.Fatalf("取消后的 ACK gate 无法复用: packet=%+v err=%v", packet, err)
	}
}
