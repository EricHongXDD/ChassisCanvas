package edgedisplay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	protocol "casescreen/internal/protocol/waveshare"
)

func oneShotHostOpener(port io.ReadWriteCloser) hostPortOpener {
	var opened atomic.Bool
	return func() (io.ReadWriteCloser, error) {
		if opened.CompareAndSwap(false, true) {
			return port, nil
		}
		return nil, errors.New("测试主机端口已使用")
	}
}

func writeTestPacket(writer io.Writer, packet protocol.Packet) error {
	_, err := writeFullCount(writer, protocol.EncodePacket(packet))
	return err
}

func waitBridgeDone(t *testing.T, bridge *protocolBridge) {
	t.Helper()
	select {
	case <-bridge.Done():
	case <-time.After(time.Second):
		t.Fatal("协议桥没有及时停止")
	}
}

func TestProtocolBridgeForwardsHostCommandAndAck(t *testing.T) {
	bridgeDevice, factoryDevice := net.Pipe()
	bridgeHost, hostPC := net.Pipe()
	defer factoryDevice.Close()
	defer hostPC.Close()

	bridge, err := newProtocolBridge(
		bridgeDevice, oneShotHostOpener(bridgeHost),
		100*time.Millisecond, 500*time.Millisecond, 500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	bridge.StartHostIngress()

	factoryResult := make(chan error, 1)
	go func() {
		packet, readErr := protocol.ReadPacket(bufio.NewReader(factoryDevice))
		if readErr != nil {
			factoryResult <- readErr
			return
		}
		values, decodeErr := protocol.DecodeQStringMap(packet.Payload)
		if decodeErr != nil {
			factoryResult <- decodeErr
			return
		}
		if values["origin"] != "host" {
			factoryResult <- fmt.Errorf("电脑端命令没有透传: %#v", values)
			return
		}
		factoryResult <- writeTestPacket(factoryDevice, protocol.Packet{
			Type: protocol.PacketTypeCommandAck, Command: packet.Command,
			Payload: protocol.EncodeQStringMap(map[string]string{"origin": "host"}),
		})
	}()

	if err := writeTestPacket(hostPC, protocol.Packet{
		Type: protocol.PacketTypeCommand, Command: protocol.CommandGetThemes,
		Payload: protocol.EncodeQStringMap(map[string]string{"origin": "host"}),
	}); err != nil {
		t.Fatal(err)
	}
	_ = hostPC.SetReadDeadline(time.Now().Add(time.Second))
	ack, err := protocol.ReadPacket(bufio.NewReader(hostPC))
	if err != nil {
		t.Fatal(err)
	}
	values, err := protocol.DecodeQStringMap(ack.Payload)
	if err != nil || ack.Command != protocol.CommandGetThemes || values["origin"] != "host" {
		t.Fatalf("原厂 ACK 没有原样返回电脑端: packet=%+v values=%#v err=%v", ack, values, err)
	}
	if err := <-factoryResult; err != nil {
		t.Fatal(err)
	}
}

func TestProtocolBridgeSerializesConcurrentHostAndLocalCommands(t *testing.T) {
	bridgeDevice, factoryDevice := net.Pipe()
	bridgeHost, hostPC := net.Pipe()
	defer factoryDevice.Close()
	defer hostPC.Close()

	bridge, err := newProtocolBridge(
		bridgeDevice, oneShotHostOpener(bridgeHost),
		200*time.Millisecond, 500*time.Millisecond, 500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	bridge.StartHostIngress()

	factoryResult := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(factoryDevice)
		for index := 0; index < 2; index++ {
			packet, readErr := protocol.ReadPacket(reader)
			if readErr != nil {
				factoryResult <- readErr
				return
			}
			values, decodeErr := protocol.DecodeQStringMap(packet.Payload)
			if decodeErr != nil {
				factoryResult <- decodeErr
				return
			}
			origin := values["origin"]
			if origin != "host" && origin != "local" {
				factoryResult <- fmt.Errorf("未知事务来源: %#v", values)
				return
			}
			if writeErr := writeTestPacket(factoryDevice, protocol.Packet{
				Type: protocol.PacketTypeCommandAck, Command: packet.Command,
				Payload: protocol.EncodeQStringMap(map[string]string{"origin": origin}),
			}); writeErr != nil {
				factoryResult <- writeErr
				return
			}
		}
		factoryResult <- nil
	}()

	start := make(chan struct{})
	hostResult := make(chan protocol.Packet, 1)
	hostError := make(chan error, 1)
	go func() {
		<-start
		if writeErr := writeTestPacket(hostPC, protocol.Packet{
			Type: protocol.PacketTypeCommand, Command: protocol.CommandSystemData,
			Payload: protocol.EncodeQStringMap(map[string]string{"origin": "host"}),
		}); writeErr != nil {
			hostError <- writeErr
			return
		}
		_ = hostPC.SetReadDeadline(time.Now().Add(time.Second))
		packet, readErr := protocol.ReadPacket(bufio.NewReader(hostPC))
		if readErr != nil {
			hostError <- readErr
			return
		}
		hostResult <- packet
	}()

	localResult := make(chan protocol.Packet, 1)
	localError := make(chan error, 1)
	go func() {
		<-start
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		packet, callErr := bridge.Do(ctx, protocol.CommandSystemData,
			protocol.EncodeQStringMap(map[string]string{"origin": "local"}))
		if callErr != nil {
			localError <- callErr
			return
		}
		localResult <- packet
	}()
	close(start)

	var hostAck protocol.Packet
	select {
	case err := <-hostError:
		t.Fatal(err)
	case hostAck = <-hostResult:
	case <-time.After(2 * time.Second):
		t.Fatal("电脑端命令超时")
	}
	var localAck protocol.Packet
	select {
	case err := <-localError:
		t.Fatal(err)
	case localAck = <-localResult:
	case <-time.After(2 * time.Second):
		t.Fatal("本地注入命令超时")
	}

	hostValues, _ := protocol.DecodeQStringMap(hostAck.Payload)
	localValues, _ := protocol.DecodeQStringMap(localAck.Payload)
	if hostValues["origin"] != "host" || localValues["origin"] != "local" {
		t.Fatalf("并发 ACK 归属错误: host=%#v local=%#v", hostValues, localValues)
	}
	if err := <-factoryResult; err != nil {
		t.Fatal(err)
	}

	_ = hostPC.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, err := hostPC.Read(make([]byte, 1)); err == nil {
		t.Fatal("本地注入 ACK 泄漏到了电脑端")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("检查 ACK 泄漏时得到意外错误: %v", err)
	}
}

func TestProtocolBridgeKeepsFileTransferExclusiveAndAllowsSlowFileEndAck(t *testing.T) {
	bridgeDevice, factoryDevice := net.Pipe()
	bridgeHost, hostPC := net.Pipe()
	defer factoryDevice.Close()
	defer hostPC.Close()

	// 本地 ACK 只有 30ms；电脑端 ACK 独立为 300ms，验证慢 FileEnd 不会被本地阈值误杀。
	bridge, err := newProtocolBridge(
		bridgeDevice, oneShotHostOpener(bridgeHost),
		30*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	bridge.StartHostIngress()

	raw := []byte("theme-resource-bytes")
	rawReceived := make(chan struct{})
	nextPacket := make(chan protocol.Packet, 1)
	factoryResult := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(factoryDevice)
		startPacket, readErr := protocol.ReadPacket(reader)
		if readErr != nil {
			factoryResult <- readErr
			return
		}
		if startPacket.Command != protocol.CommandFileStart {
			factoryResult <- fmt.Errorf("首包不是 FileStart: %+v", startPacket)
			return
		}
		if writeErr := writeTestPacket(factoryDevice, protocol.Packet{
			Type: protocol.PacketTypeCommandAck, Command: protocol.CommandFileStart,
		}); writeErr != nil {
			factoryResult <- writeErr
			return
		}
		received := make([]byte, len(raw))
		if _, readErr = io.ReadFull(reader, received); readErr != nil {
			factoryResult <- readErr
			return
		}
		if string(received) != string(raw) {
			factoryResult <- fmt.Errorf("文件裸流不一致: %q", received)
			return
		}
		close(rawReceived)

		packet, readErr := protocol.ReadPacket(reader)
		if readErr != nil {
			factoryResult <- readErr
			return
		}
		nextPacket <- packet
		if packet.Command != protocol.CommandFileEnd {
			factoryResult <- fmt.Errorf("文件裸流后插入了非 FileEnd 命令: %+v", packet)
			return
		}
		timer := time.NewTimer(80 * time.Millisecond)
		<-timer.C
		if writeErr := writeTestPacket(factoryDevice, protocol.Packet{
			Type: protocol.PacketTypeCommandAck, Command: protocol.CommandFileEnd,
		}); writeErr != nil {
			factoryResult <- writeErr
			return
		}

		localPacket, readErr := protocol.ReadPacket(reader)
		if readErr != nil {
			factoryResult <- readErr
			return
		}
		if localPacket.Command != protocol.CommandSystemData {
			factoryResult <- fmt.Errorf("FileEnd 后没有恢复本地注入: %+v", localPacket)
			return
		}
		factoryResult <- writeTestPacket(factoryDevice, protocol.Packet{
			Type: protocol.PacketTypeCommandAck, Command: protocol.CommandSystemData,
		})
	}()

	hostReader := bufio.NewReader(hostPC)
	remotePath := "/data/theme-resource.png"
	if err := writeTestPacket(hostPC, protocol.Packet{
		Type: protocol.PacketTypeCommand, Command: protocol.CommandFileStart,
		Payload: protocol.EncodeQStringMap(map[string]string{remotePath: fmt.Sprint(len(raw))}),
	}); err != nil {
		t.Fatal(err)
	}
	_ = hostPC.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := protocol.ReadPacket(hostReader); err != nil {
		t.Fatal(err)
	}
	if _, err := writeFullCount(hostPC, raw); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rawReceived:
	case <-time.After(time.Second):
		t.Fatal("文件裸流没有到达原厂程序")
	}

	localRequest := bridgeRequest{
		ctx: context.Background(),
		packet: protocol.Packet{
			Type: protocol.PacketTypeCommand, Command: protocol.CommandSystemData,
			Payload: protocol.EncodeQStringMap(map[string]string{"origin": "local"}),
		},
		response: make(chan bridgeResponse, 1),
	}
	bridge.localCommands <- localRequest
	select {
	case packet := <-nextPacket:
		t.Fatalf("FileEnd 发送前原厂程序收到了命令: %+v", packet)
	case <-time.After(30 * time.Millisecond):
	}

	if err := writeTestPacket(hostPC, protocol.Packet{
		Type: protocol.PacketTypeCommand, Command: protocol.CommandFileEnd,
		Payload: protocol.EncodeQStringMap(map[string]string{remotePath: "0"}),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case packet := <-nextPacket:
		if packet.Command != protocol.CommandFileEnd {
			t.Fatalf("文件事务尾包错误: %+v", packet)
		}
	case <-time.After(time.Second):
		t.Fatal("FileEnd 没有到达原厂程序")
	}
	_ = hostPC.SetReadDeadline(time.Now().Add(time.Second))
	if ack, err := protocol.ReadPacket(hostReader); err != nil || ack.Command != protocol.CommandFileEnd {
		t.Fatalf("慢 FileEnd ACK 没有透传: packet=%+v err=%v", ack, err)
	}
	select {
	case response := <-localRequest.response:
		if response.err != nil {
			t.Fatal(response.err)
		}
	case <-time.After(time.Second):
		t.Fatal("文件事务结束后本地注入没有恢复")
	}
	if err := <-factoryResult; err != nil {
		t.Fatal(err)
	}
}

func TestProtocolBridgeTreatsSentCommandAckTimeoutAsFatal(t *testing.T) {
	bridgeDevice, factoryDevice := net.Pipe()
	defer factoryDevice.Close()
	bridge, err := newProtocolBridge(
		bridgeDevice, nil, 40*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}

	commandSeen := make(chan error, 1)
	go func() {
		_, readErr := protocol.ReadPacket(bufio.NewReader(factoryDevice))
		commandSeen <- readErr
	}()
	_, err = bridge.Do(context.Background(), protocol.CommandSystemData, protocol.EncodeQStringMap(map[string]string{"x": "1"}))
	if err == nil {
		t.Fatal("已发送命令缺少 ACK 时应失败")
	}
	if readErr := <-commandSeen; readErr != nil {
		t.Fatal(readErr)
	}
	waitBridgeDone(t, bridge)
	if bridge.Err() == nil || !strings.Contains(bridge.Err().Error(), "协议流已失步") {
		t.Fatalf("ACK 超时没有升级为致命失步: %v", bridge.Err())
	}
	_ = bridge.Close()
}

func TestProtocolBridgeTreatsDeviceEOFAsFatal(t *testing.T) {
	bridgeDevice, factoryDevice := net.Pipe()
	bridgeHost, hostPC := net.Pipe()
	defer hostPC.Close()
	bridge, err := newProtocolBridge(
		bridgeDevice, oneShotHostOpener(bridgeHost),
		50*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	bridge.StartHostIngress()

	go func() {
		_, _ = protocol.ReadPacket(bufio.NewReader(factoryDevice))
		_ = factoryDevice.Close()
	}()
	if err := writeTestPacket(hostPC, protocol.Packet{
		Type: protocol.PacketTypeCommand, Command: protocol.CommandGetThemes,
	}); err != nil {
		t.Fatal(err)
	}
	waitBridgeDone(t, bridge)
	if bridge.Err() == nil || !strings.Contains(bridge.Err().Error(), "ACK 未完成") {
		t.Fatalf("PTY EOF 没有升级为致命错误: %v", bridge.Err())
	}
	_ = bridge.Close()
}

func TestProtocolBridgeRawStreamIdleTimeoutIsFatal(t *testing.T) {
	bridgeDevice, factoryDevice := net.Pipe()
	bridgeHost, hostPC := net.Pipe()
	defer factoryDevice.Close()
	defer hostPC.Close()
	bridge, err := newProtocolBridge(
		bridgeDevice, oneShotHostOpener(bridgeHost),
		50*time.Millisecond, 200*time.Millisecond, 40*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	bridge.StartHostIngress()

	go func() {
		packet, readErr := protocol.ReadPacket(bufio.NewReader(factoryDevice))
		if readErr == nil {
			_ = writeTestPacket(factoryDevice, protocol.Packet{
				Type: protocol.PacketTypeCommandAck, Command: packet.Command,
			})
		}
	}()
	if err := writeTestPacket(hostPC, protocol.Packet{
		Type: protocol.PacketTypeCommand, Command: protocol.CommandFileStart,
		Payload: protocol.EncodeQStringMap(map[string]string{"/data/incomplete.bin": "1024"}),
	}); err != nil {
		t.Fatal(err)
	}
	_ = hostPC.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := protocol.ReadPacket(bufio.NewReader(hostPC)); err != nil {
		t.Fatal(err)
	}
	// 不发送声明的 1024 字节，桥必须由空闲超时打断，而不是永久卡在裸流读取。
	waitBridgeDone(t, bridge)
	if bridge.Err() == nil || !strings.Contains(bridge.Err().Error(), "文件裸流中断") {
		t.Fatalf("裸流超时没有升级为致命错误: %v", bridge.Err())
	}
	_ = bridge.Close()
}
