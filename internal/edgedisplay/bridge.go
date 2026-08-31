package edgedisplay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	protocol "casescreen/internal/protocol/waveshare"
)

type hostPortOpener func() (io.ReadWriteCloser, error)

type bridgeRequest struct {
	ctx      context.Context
	packet   protocol.Packet
	response chan bridgeResponse
}

type bridgeResponse struct {
	packet protocol.Packet
	err    error
}

type hostCommand struct {
	port     io.ReadWriteCloser
	packet   protocol.Packet
	rawSize  int64
	raw      io.Reader
	complete chan error
}

// protocolBridge 在真实 USB gadget 与 PCMonitorLinux 的 PTY 之间做协议级复用。
// 微雪协议没有事务 ID，因此任一时刻只能有一个已发送且等待 ACK 的命令。
type protocolBridge struct {
	device              io.ReadWriteCloser
	openHost            hostPortOpener
	localAckTimeout     time.Duration
	hostAckTimeout      time.Duration
	transferIdleTimeout time.Duration
	retryEvery          time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	localCommands chan bridgeRequest
	hostCommands  chan hostCommand
	hostClosed    chan struct{}
	done          chan struct{}

	deviceReader *bufio.Reader

	hostMu      sync.Mutex
	currentHost io.ReadWriteCloser

	errMu sync.RWMutex
	err   error

	hostStartOnce sync.Once
	workers       sync.WaitGroup
	closeOnce     sync.Once
}

// newProtocolBridge 只启动协议协调器。调用方应在 PCMonitorLinux 完成发现握手后
// 再调用 StartHostIngress，避免程序尚未打开 PTY 时电脑端命令抢先进入。
func newProtocolBridge(
	device io.ReadWriteCloser,
	openHost hostPortOpener,
	localAckTimeout time.Duration,
	hostAckTimeout time.Duration,
	transferIdleTimeout time.Duration,
) (*protocolBridge, error) {
	if device == nil {
		return nil, errors.New("PTY 协议桥缺少原厂程序端口")
	}
	if localAckTimeout <= 0 {
		localAckTimeout = 2 * time.Second
	}
	if hostAckTimeout <= 0 {
		hostAckTimeout = 10 * time.Second
	}
	if transferIdleTimeout <= 0 {
		transferIdleTimeout = 15 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	bridge := &protocolBridge{
		device:              device,
		openHost:            openHost,
		localAckTimeout:     localAckTimeout,
		hostAckTimeout:      hostAckTimeout,
		transferIdleTimeout: transferIdleTimeout,
		retryEvery:          500 * time.Millisecond,
		ctx:                 ctx,
		cancel:              cancel,
		localCommands:       make(chan bridgeRequest, 16),
		hostCommands:        make(chan hostCommand),
		hostClosed:          make(chan struct{}),
		done:                make(chan struct{}),
		deviceReader:        bufio.NewReaderSize(device, 16<<10),
	}
	bridge.workers.Add(1)
	go bridge.run()
	return bridge, nil
}

// StartHostIngress 开始接收电脑端真实 ttyGS0 命令。无真实 gadget 时为空操作。
func (bridge *protocolBridge) StartHostIngress() {
	if bridge == nil || bridge.openHost == nil {
		return
	}
	bridge.hostStartOnce.Do(func() {
		select {
		case <-bridge.done:
			return
		default:
		}
		bridge.workers.Add(1)
		go bridge.runHostIngress()
	})
}

func (bridge *protocolBridge) Done() <-chan struct{} {
	if bridge == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return bridge.done
}

func (bridge *protocolBridge) Err() error {
	if bridge == nil {
		return errors.New("PTY 协议桥未初始化")
	}
	bridge.errMu.RLock()
	defer bridge.errMu.RUnlock()
	return bridge.err
}

func (bridge *protocolBridge) Do(ctx context.Context, command protocol.Command, payload []byte) (protocol.Packet, error) {
	if bridge == nil {
		return protocol.Packet{}, errors.New("PTY 协议桥未初始化")
	}
	if ctx == nil {
		return protocol.Packet{}, errors.New("PTY 本地命令上下文不能为空")
	}
	request := bridgeRequest{
		ctx: ctx,
		packet: protocol.Packet{
			Type: protocol.PacketTypeCommand, Command: command,
			Payload: append([]byte(nil), payload...),
		},
		response: make(chan bridgeResponse, 1),
	}
	select {
	case <-ctx.Done():
		return protocol.Packet{}, ctx.Err()
	case <-bridge.done:
		return protocol.Packet{}, bridge.resultError()
	case bridge.localCommands <- request:
	}
	select {
	case <-ctx.Done():
		return protocol.Packet{}, ctx.Err()
	case <-bridge.done:
		select {
		case response := <-request.response:
			return response.packet, response.err
		default:
			return protocol.Packet{}, bridge.resultError()
		}
	case response := <-request.response:
		return response.packet, response.err
	}
}

func (bridge *protocolBridge) run() {
	defer bridge.workers.Done()
	defer close(bridge.done)
	fileTransfer := false
	for {
		if fileTransfer {
			select {
			case <-bridge.ctx.Done():
				return
			case <-bridge.hostClosed:
				bridge.fail(errors.New("主机在 FileStart 与 FileEnd 之间断开，协议流已失步"))
				return
			case command := <-bridge.hostCommands:
				if command.packet.Command != protocol.CommandFileEnd {
					err := errors.New("文件传输期间收到非 FileEnd 命令，协议流已失步")
					command.complete <- err
					bridge.fail(err)
					return
				}
				active, fatal := bridge.handleHostCommand(command, true)
				fileTransfer = active
				if fatal != nil {
					bridge.fail(fatal)
					return
				}
			}
			continue
		}

		select {
		case <-bridge.ctx.Done():
			return
		case <-bridge.hostClosed:
			// 普通命令之间断线不会改变 PCMonitorLinux 的协议状态，可等待重连。
		case command := <-bridge.hostCommands:
			active, fatal := bridge.handleHostCommand(command, false)
			fileTransfer = active
			if fatal != nil {
				bridge.fail(fatal)
				return
			}
		case request := <-bridge.localCommands:
			if fatal := bridge.handleLocalCommand(request); fatal != nil {
				bridge.fail(fatal)
				return
			}
		}
	}
}

func (bridge *protocolBridge) handleLocalCommand(request bridgeRequest) error {
	if err := request.ctx.Err(); err != nil {
		request.response <- bridgeResponse{err: err}
		return nil
	}
	written, err := writePacketWithDeadline(request.ctx, bridge.device, request.packet, bridge.localAckTimeout)
	if err != nil {
		wrapped := fmt.Errorf("写入本地 PTY 命令: %w", err)
		request.response <- bridgeResponse{err: wrapped}
		if written == 0 {
			// PCMonitorLinux 启动早期尚未打开 PTY 时，零字节写入失败可以安全重试。
			return nil
		}
		return fmt.Errorf("本地命令只写入 %d 字节，协议流已失步: %w", written, err)
	}
	response, _, deviceErr := bridge.waitForAck(request.ctx, request.packet.Command, nil, bridge.localAckTimeout)
	if deviceErr != nil {
		wrapped := fmt.Errorf("等待本地命令 %d ACK: %w", request.packet.Command, deviceErr)
		request.response <- bridgeResponse{err: wrapped}
		return fmt.Errorf("本地命令已发送但 ACK 未完成，协议流已失步: %w", deviceErr)
	}
	request.response <- bridgeResponse{packet: response}
	return nil
}

func (bridge *protocolBridge) handleHostCommand(command hostCommand, fileTransfer bool) (bool, error) {
	ctx, cancel := context.WithTimeout(bridge.ctx, bridge.hostAckTimeout)
	defer cancel()
	written, err := writePacketWithDeadline(ctx, bridge.device, command.packet, bridge.hostAckTimeout)
	if err != nil {
		wrapped := fmt.Errorf("向原厂程序转发主机命令: %w", err)
		command.complete <- wrapped
		if written == 0 {
			return false, nil
		}
		return false, fmt.Errorf("主机命令只写入 %d 字节，协议流已失步: %w", written, err)
	}

	_, hostErr, deviceErr := bridge.waitForAck(ctx, command.packet.Command, command.port, bridge.hostAckTimeout)
	if deviceErr != nil {
		wrapped := fmt.Errorf("等待主机命令 %d ACK: %w", command.packet.Command, deviceErr)
		command.complete <- wrapped
		return false, fmt.Errorf("主机命令已发送但 ACK 未完成，协议流已失步: %w", deviceErr)
	}
	if hostErr != nil && command.packet.Command == protocol.CommandFileStart {
		wrapped := fmt.Errorf("FileStart ACK 无法返回主机: %w", hostErr)
		command.complete <- wrapped
		return false, fmt.Errorf("原厂程序已进入裸流接收但主机未收到 ACK，协议流已失步: %w", hostErr)
	}

	if command.packet.Command == protocol.CommandFileStart {
		if command.rawSize < 0 || command.raw == nil {
			err := errors.New("FileStart 缺少有效原始文件长度")
			command.complete <- err
			return false, fmt.Errorf("原厂程序已进入裸流接收但长度无效，协议流已失步: %w", err)
		}
		copied, copyErr := bridge.copyRawStream(command)
		if copyErr != nil {
			wrapped := fmt.Errorf("桥接主机文件内容（%d/%d 字节）: %w", copied, command.rawSize, copyErr)
			command.complete <- wrapped
			return false, fmt.Errorf("文件裸流中断，协议流已失步: %w", wrapped)
		}
		command.complete <- nil
		return true, nil
	}

	command.complete <- hostErr
	if command.packet.Command == protocol.CommandFileEnd {
		return false, nil
	}
	return fileTransfer, nil
}

// waitForAck 读取到匹配 ACK 前持续排空响应。host 写失败不会中断读取，
// 因为必须先把原厂程序侧重新同步到命令边界，才能安全处理下一条本地命令。
func (bridge *protocolBridge) waitForAck(
	ctx context.Context,
	command protocol.Command,
	host io.Writer,
	timeout time.Duration,
) (protocol.Packet, error, error) {
	deadline := time.Now().Add(timeout)
	if contextDeadline, exists := ctx.Deadline(); exists && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if reader, supported := bridge.device.(interface{ SetReadDeadline(time.Time) error }); supported {
		if err := reader.SetReadDeadline(deadline); err != nil {
			return protocol.Packet{}, nil, err
		}
		defer reader.SetReadDeadline(time.Time{})
	}
	var hostErr error
	for {
		if err := ctx.Err(); err != nil {
			return protocol.Packet{}, hostErr, err
		}
		packet, err := protocol.ReadPacket(bridge.deviceReader)
		if err != nil {
			return protocol.Packet{}, hostErr, fmt.Errorf("读取原厂程序 ACK: %w", err)
		}
		if host != nil && hostErr == nil {
			_, err = writePacketWithDeadline(bridge.ctx, host, packet, timeout)
			if err != nil {
				hostErr = fmt.Errorf("向主机返回原厂响应: %w", err)
			}
		}
		if packet.Type == protocol.PacketTypeCommandAck && packet.Command == command {
			return packet, hostErr, nil
		}
		// 本地事务没有对应主机请求；非匹配的陈旧或无关 ACK 不向主机泄漏。
	}
}

func (bridge *protocolBridge) copyRawStream(command hostCommand) (int64, error) {
	remaining := command.rawSize
	buffer := make([]byte, 64<<10)
	var copied int64
	defer clearDeadlines(command.port, bridge.device)
	for remaining > 0 {
		select {
		case <-bridge.ctx.Done():
			return copied, bridge.ctx.Err()
		default:
		}
		deadline := time.Now().Add(bridge.transferIdleTimeout)
		if setter, supported := command.port.(interface{ SetReadDeadline(time.Time) error }); supported {
			if err := setter.SetReadDeadline(deadline); err != nil {
				return copied, err
			}
		}
		if setter, supported := bridge.device.(interface{ SetWriteDeadline(time.Time) error }); supported {
			if err := setter.SetWriteDeadline(deadline); err != nil {
				return copied, err
			}
		}
		chunk := int64(len(buffer))
		if remaining < chunk {
			chunk = remaining
		}
		read, readErr := command.raw.Read(buffer[:int(chunk)])
		if read > 0 {
			written, writeErr := writeFullCount(bridge.device, buffer[:read])
			copied += int64(written)
			remaining -= int64(written)
			if writeErr != nil {
				return copied, writeErr
			}
			if written != read {
				return copied, io.ErrShortWrite
			}
		}
		if readErr != nil {
			return copied, readErr
		}
		if read == 0 {
			return copied, io.ErrNoProgress
		}
	}
	return copied, nil
}

func clearDeadlines(host io.Reader, device io.Writer) {
	if setter, supported := host.(interface{ SetReadDeadline(time.Time) error }); supported {
		_ = setter.SetReadDeadline(time.Time{})
	}
	if setter, supported := device.(interface{ SetWriteDeadline(time.Time) error }); supported {
		_ = setter.SetWriteDeadline(time.Time{})
	}
}

func (bridge *protocolBridge) runHostIngress() {
	defer bridge.workers.Done()
	for {
		select {
		case <-bridge.ctx.Done():
			return
		default:
		}
		port, err := bridge.openHost()
		if err != nil {
			if !bridge.waitRetry() {
				return
			}
			continue
		}
		bridge.setCurrentHost(port)
		_ = bridge.serveHostPort(port)
		bridge.clearCurrentHost(port)
		_ = port.Close()
		select {
		case <-bridge.ctx.Done():
			return
		case bridge.hostClosed <- struct{}{}:
		}
		if !bridge.waitRetry() {
			return
		}
	}
}

func (bridge *protocolBridge) serveHostPort(port io.ReadWriteCloser) error {
	reader := bufio.NewReaderSize(port, 16<<10)
	for {
		packet, err := protocol.ReadPacket(reader)
		if err != nil {
			return err
		}
		if packet.Type != protocol.PacketTypeCommand {
			return fmt.Errorf("主机发送了不支持的包类型 %d", packet.Type)
		}
		command := hostCommand{
			port: port, packet: packet, raw: reader, rawSize: -1,
			complete: make(chan error, 1),
		}
		if packet.Command == protocol.CommandFileStart {
			command.rawSize, err = fileStartSize(packet.Payload)
			if err != nil {
				return err
			}
		}
		select {
		case <-bridge.ctx.Done():
			return bridge.ctx.Err()
		case bridge.hostCommands <- command:
		}
		select {
		case <-bridge.ctx.Done():
			return bridge.ctx.Err()
		case err := <-command.complete:
			if err != nil {
				return err
			}
		}
	}
}

func fileStartSize(payload []byte) (int64, error) {
	values, err := protocol.DecodeQStringMap(payload)
	if err != nil {
		return 0, fmt.Errorf("解析 FileStart: %w", err)
	}
	if len(values) != 1 {
		return 0, errors.New("FileStart 必须且只能声明一个文件")
	}
	for _, text := range values {
		size, err := strconv.ParseInt(text, 10, 64)
		if err != nil || size < 0 {
			return 0, errors.New("FileStart 文件长度无效")
		}
		return size, nil
	}
	return 0, errors.New("FileStart 未声明文件")
}

func writePacketWithDeadline(
	ctx context.Context,
	writer io.Writer,
	packet protocol.Packet,
	timeout time.Duration,
) (int, error) {
	if setter, supported := writer.(interface{ SetWriteDeadline(time.Time) error }); supported {
		deadline := time.Now().Add(timeout)
		if contextDeadline, exists := ctx.Deadline(); exists && contextDeadline.Before(deadline) {
			deadline = contextDeadline
		}
		if err := setter.SetWriteDeadline(deadline); err != nil {
			return 0, err
		}
		defer setter.SetWriteDeadline(time.Time{})
	}
	return writeFullCount(writer, protocol.EncodePacket(packet))
}

func writeFullCount(writer io.Writer, data []byte) (int, error) {
	total := 0
	for len(data) > 0 {
		written, err := writer.Write(data)
		total += written
		if err != nil {
			return total, err
		}
		if written <= 0 {
			return total, io.ErrShortWrite
		}
		data = data[written:]
	}
	return total, nil
}

func (bridge *protocolBridge) waitRetry() bool {
	timer := time.NewTimer(bridge.retryEvery)
	defer timer.Stop()
	select {
	case <-bridge.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (bridge *protocolBridge) setCurrentHost(port io.ReadWriteCloser) {
	bridge.hostMu.Lock()
	bridge.currentHost = port
	bridge.hostMu.Unlock()
}

func (bridge *protocolBridge) clearCurrentHost(port io.ReadWriteCloser) {
	bridge.hostMu.Lock()
	if bridge.currentHost == port {
		bridge.currentHost = nil
	}
	bridge.hostMu.Unlock()
}

func (bridge *protocolBridge) closeCurrentHost() {
	bridge.hostMu.Lock()
	port := bridge.currentHost
	bridge.currentHost = nil
	bridge.hostMu.Unlock()
	if port != nil {
		_ = port.Close()
	}
}

func (bridge *protocolBridge) fail(err error) {
	bridge.setError(err)
	bridge.cancel()
	bridge.closeCurrentHost()
	_ = bridge.device.Close()
}

func (bridge *protocolBridge) setError(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	bridge.errMu.Lock()
	if bridge.err == nil {
		bridge.err = err
	}
	bridge.errMu.Unlock()
}

func (bridge *protocolBridge) resultError() error {
	if err := bridge.Err(); err != nil {
		return err
	}
	return errors.New("PTY 协议桥已关闭")
}

func (bridge *protocolBridge) Close() error {
	if bridge == nil {
		return nil
	}
	bridge.closeOnce.Do(func() {
		bridge.cancel()
		bridge.closeCurrentHost()
		_ = bridge.device.Close()
		bridge.workers.Wait()
	})
	return bridge.Err()
}
