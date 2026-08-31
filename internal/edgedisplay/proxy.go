package edgedisplay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	protocol "casescreen/internal/protocol/waveshare"
	"casescreen/internal/telemetry"
)

const (
	maxSystemDataPayload = 1 << 20
	maxLyricLineBytes    = 4 << 10
	maxThemePathBytes    = 4 << 10

	// PlainLyricsNotice 是纯文本歌词进入屏幕渲染层时显示的固定提示。
	PlainLyricsNotice = "该首歌的歌词暂不支持滚动"
)

// LyricMode 描述歌词是否含有可滚动时间轴。
type LyricMode string

const (
	LyricModeUnknown LyricMode = "unknown"
	LyricModeTimed   LyricMode = "timed"
	LyricModePlain   LyricMode = "plain"
)

// LyricsUpdate 是屏端歌词核心交给原厂渲染器的最小状态。
// Position 使用纳秒；完整歌词由屏端按 Source + TrackID 缓存，不通过此结构传输。
type LyricsUpdate struct {
	Source    string
	TrackID   string
	Position  int64
	Playing   bool
	LyricMode LyricMode
	Current   string
	Next      string
}

// Options 描述屏端代理如何接管原厂 PCMonitorLinux 的串口入口。
type Options struct {
	DevicePath          string
	BackupPath          string
	LockPath            string
	ProgramPath         string
	ProgramArgs         []string
	WorkingDirectory    string
	Environment         []string
	ReadyTimeout        time.Duration
	AckTimeout          time.Duration
	HostAckTimeout      time.Duration
	TransferIdleTimeout time.Duration
	StopTimeout         time.Duration
	Stdout              io.Writer
	Stderr              io.Writer
}

// Proxy 通过 PTY 主端向原厂 PCMonitorLinux 发送原有微雪协议。
// Proxy 提供小型 SystemData 注入和已驻留主题切换，不提供文件或固件写入能力。
type Proxy struct {
	options  Options
	endpoint platformEndpoint
	command  *exec.Cmd
	bridge   *protocolBridge

	childDone chan struct{}
	childMu   sync.RWMutex
	childErr  error

	restoreOnce sync.Once
	restoreErr  error
	closeOnce   sync.Once
	closeErr    error

	deviceInfo map[string]string
}

// Start 创建 PTY、暂时替换 ttyGS0、启动原厂程序并完成发现命令握手。
func Start(ctx context.Context, options Options) (*Proxy, error) {
	options = normalizeOptions(options)
	endpoint, err := openPlatformEndpoint(options)
	if err != nil {
		return nil, err
	}
	bridge, err := newProtocolBridge(
		endpoint, endpoint.HostOpener(), options.AckTimeout,
		options.HostAckTimeout, options.TransferIdleTimeout,
	)
	if err != nil {
		return nil, errors.Join(err, endpoint.Close(), endpoint.Restore())
	}

	command := exec.Command(options.ProgramPath, options.ProgramArgs...)
	command.Dir = options.WorkingDirectory
	command.Env = append(os.Environ(), options.Environment...)
	command.Stdout = options.Stdout
	command.Stderr = options.Stderr
	configureChildProcess(command)
	if err := command.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("启动原厂 PCMonitorLinux: %w", err), bridge.Close(), endpoint.Restore())
	}

	proxy := &Proxy{
		options: options, endpoint: endpoint, command: command,
		bridge: bridge, childDone: make(chan struct{}),
	}
	go proxy.waitChild()
	go proxy.watchBridge()

	readyContext, cancel := context.WithTimeout(ctx, options.ReadyTimeout)
	defer cancel()
	packet, err := proxy.waitReady(readyContext)
	if err != nil {
		closeErr := proxy.Close()
		return nil, errors.Join(fmt.Errorf("等待原厂 PCMonitorLinux 串口就绪: %w", err), closeErr)
	}
	values, err := protocol.DecodeQStringMap(packet.Payload)
	if err != nil {
		closeErr := proxy.Close()
		return nil, errors.Join(fmt.Errorf("解析原厂程序设备信息: %w", err), closeErr)
	}
	proxy.deviceInfo = cloneValues(values)
	proxy.bridge.StartHostIngress()
	return proxy, nil
}

func normalizeOptions(options Options) Options {
	if options.DevicePath == "" {
		options.DevicePath = "/dev/ttyGS0"
	}
	if options.BackupPath == "" {
		options.BackupPath = options.DevicePath + ".casescreen-real"
	}
	if options.LockPath == "" {
		options.LockPath = "/run/casescreen-edge-display.lock"
	}
	if options.ProgramPath == "" {
		options.ProgramPath = "/data/PCMonitorLinux"
	}
	if options.WorkingDirectory == "" {
		options.WorkingDirectory = filepath.Dir(options.ProgramPath)
	}
	if options.ReadyTimeout <= 0 {
		options.ReadyTimeout = 10 * time.Second
	}
	if options.AckTimeout <= 0 {
		options.AckTimeout = 2 * time.Second
	}
	if options.HostAckTimeout < 5*time.Second {
		options.HostAckTimeout = 10 * time.Second
	}
	if options.TransferIdleTimeout <= 0 {
		options.TransferIdleTimeout = 15 * time.Second
	}
	if options.StopTimeout <= 0 {
		options.StopTimeout = 3 * time.Second
	}
	return options
}

// DeviceInfo 返回握手时由原厂程序报告的屏幕信息副本。
func (proxy *Proxy) DeviceInfo() map[string]string {
	return cloneValues(proxy.deviceInfo)
}

// SendSystemData 向原厂主题控件注入一组 QStringMap 小数据。
func (proxy *Proxy) SendSystemData(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	payload := protocol.EncodeQStringMap(values)
	if len(payload) > maxSystemDataPayload {
		return fmt.Errorf("SystemData 载荷过大: %d 字节（上限 %d）", len(payload), maxSystemDataPayload)
	}
	_, err := proxy.sendCommand(ctx, protocol.CommandSystemData, payload)
	return err
}

// ReloadTheme 让原厂渲染器切换到已经驻留在 TF 卡上的主题。
// 该方法不会上传主题文件，适合开机时只发送一次很小的路径命令。
func (proxy *Proxy) ReloadTheme(ctx context.Context, remotePath string) error {
	remotePath = strings.TrimSpace(remotePath)
	if remotePath == "" {
		return errors.New("驻留主题路径不能为空")
	}
	if len(remotePath) > maxThemePathBytes || strings.ContainsRune(remotePath, '\x00') {
		return errors.New("驻留主题路径无效")
	}
	if !strings.HasPrefix(remotePath, "/data/theme/") || !strings.EqualFold(filepath.Ext(remotePath), ".Theme") {
		return errors.New("驻留主题必须是 /data/theme 下的 .Theme 文件")
	}
	_, err := proxy.sendCommand(ctx, protocol.CommandReloadTheme, []byte(remotePath))
	return err
}

// SendLyrics 使用当前微雪主题编译器约定的键名发送双行歌词。
func (proxy *Proxy) SendLyrics(ctx context.Context, current, next string) error {
	visible := strings.TrimSpace(current) != ""
	if !visible {
		current, next = "", ""
	}
	visibleValue := "0"
	if visible {
		visibleValue = "1"
	}
	values := map[string]string{
		"CaseScreen_LyricsCurrent": current,
		"CaseScreen_LyricsNext":    next,
		telemetry.LyricsVisibleKey: visibleValue,
	}
	if !visible {
		// 隐藏状态同时清掉曾经留下的进度前景。这里只在歌词状态变化时
		// 增加两个短值，不进入播放期间的高频小包。
		values[telemetry.LyricsProgressFineKey] = "0"
		values[telemetry.LyricsProgressLegacyKey] = "0"
	}
	return proxy.SendSystemData(ctx, values)
}

// SendLyricsUpdate 注入歌曲身份、播放位置和双行歌词。纯文本歌词不会被塞进
// 高频包；LyricModePlain 时固定显示“不支持滚动”提示并清空下一句。
func (proxy *Proxy) SendLyricsUpdate(ctx context.Context, update LyricsUpdate) error {
	values, err := LyricsValues(update)
	if err != nil {
		return err
	}
	return proxy.SendSystemData(ctx, values)
}

// LyricsValues 生成 PCMonitorLinux 可直接消费的 QStringMap。
func LyricsValues(update LyricsUpdate) (map[string]string, error) {
	update.Source = strings.ToLower(strings.TrimSpace(update.Source))
	update.TrackID = strings.TrimSpace(update.TrackID)
	if update.Position < 0 {
		return nil, errors.New("歌词播放位置不能为负数")
	}
	switch update.LyricMode {
	case "":
		update.LyricMode = LyricModeUnknown
	case LyricModeUnknown, LyricModeTimed, LyricModePlain:
	default:
		return nil, fmt.Errorf("未知歌词模式: %q", update.LyricMode)
	}
	if len(update.Source) > 64 || len(update.TrackID) > 512 {
		return nil, errors.New("歌词平台或歌曲 ID 过长")
	}
	if len(update.Current) > maxLyricLineBytes || len(update.Next) > maxLyricLineBytes {
		return nil, errors.New("当前句或下一句过长；高频包禁止携带整份歌词")
	}
	if update.LyricMode == LyricModePlain {
		update.Current = PlainLyricsNotice
		update.Next = ""
	}
	visible := strings.TrimSpace(update.Current) != ""
	if !visible {
		// 下一句不能单独唤醒歌词层；等待当前句时清空两行，避免驻留
		// QStringMap 继续保留上一首歌。
		update.Current = ""
		update.Next = ""
	}
	visibleValue := "0"
	if visible {
		visibleValue = "1"
	}
	values := map[string]string{
		"source":                   update.Source,
		"track_id":                 update.TrackID,
		"position":                 strconv.FormatInt(update.Position, 10),
		"playing":                  strconv.FormatBool(update.Playing),
		"lyric_mode":               string(update.LyricMode),
		"CaseScreen_LyricsCurrent": update.Current,
		"CaseScreen_LyricsNext":    update.Next,
		telemetry.LyricsVisibleKey: visibleValue,
	}
	if !visible {
		// 防止上一句非零 LFP 在歌词面板隐藏后仍留下孤立的进度前景。
		values[telemetry.LyricsProgressFineKey] = "0"
		values[telemetry.LyricsProgressLegacyKey] = "0"
	}
	return values, nil
}

func (proxy *Proxy) sendCommand(ctx context.Context, command protocol.Command, payload []byte) (protocol.Packet, error) {
	if ctx == nil {
		return protocol.Packet{}, errors.New("PTY 命令上下文不能为空")
	}
	select {
	case <-proxy.childDone:
		if err := proxy.childResult(); err != nil {
			return protocol.Packet{}, fmt.Errorf("原厂 PCMonitorLinux 已退出: %w", err)
		}
		return protocol.Packet{}, errors.New("原厂 PCMonitorLinux 已退出")
	default:
	}
	if err := ctx.Err(); err != nil {
		return protocol.Packet{}, err
	}
	if proxy.bridge == nil {
		return protocol.Packet{}, errors.New("PTY 协议桥未初始化")
	}
	response, err := proxy.bridge.Do(ctx, command, payload)
	if err != nil {
		return protocol.Packet{}, fmt.Errorf("等待命令 %d ACK: %w", command, err)
	}
	return response, nil
}

func (proxy *Proxy) waitReady(ctx context.Context) (protocol.Packet, error) {
	var lastError error
	for {
		attemptTimeout := proxy.options.AckTimeout
		if attemptTimeout > time.Second {
			attemptTimeout = time.Second
		}
		attemptContext, cancel := context.WithTimeout(ctx, attemptTimeout)
		packet, err := proxy.sendCommand(attemptContext, protocol.CommandFindDevice, nil)
		cancel()
		if err == nil {
			return packet, nil
		}
		lastError = err
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return protocol.Packet{}, errors.Join(ctx.Err(), lastError)
		case <-proxy.childDone:
			stopTimer(timer)
			return protocol.Packet{}, fmt.Errorf("原厂 PCMonitorLinux 在握手前退出: %v", proxy.childResult())
		case <-timer.C:
		}
	}
}

// Wait 等待原厂程序退出；上下文取消时会关闭代理并恢复 ttyGS0。
func (proxy *Proxy) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return errors.Join(ctx.Err(), proxy.Close())
	case <-proxy.childDone:
		proxy.restore()
		return errors.Join(proxy.childResult(), proxy.restoreErr)
	}
}

// Close 终止原厂程序、关闭 PTY，并尽力恢复原 ttyGS0 节点。
func (proxy *Proxy) Close() error {
	proxy.closeOnce.Do(func() {
		if !isClosed(proxy.childDone) {
			_ = signalChild(proxy.command)
			timer := time.NewTimer(proxy.options.StopTimeout)
			select {
			case <-proxy.childDone:
				stopTimer(timer)
			case <-timer.C:
				_ = killChild(proxy.command)
				<-proxy.childDone
			}
		}
		proxy.restore()
		proxy.closeErr = proxy.restoreErr
	})
	return proxy.closeErr
}

func (proxy *Proxy) waitChild() {
	err := proxy.command.Wait()
	proxy.childMu.Lock()
	proxy.childErr = err
	proxy.childMu.Unlock()
	close(proxy.childDone)
	proxy.restore()
}

// watchBridge 在协议失步时终止原厂进程，让上层监督器重启整个接管流程。
// 不能在无事务 ID 的字节流上继续猜测迟到 ACK 的归属。
func (proxy *Proxy) watchBridge() {
	<-proxy.bridge.Done()
	if proxy.bridge.Err() == nil || isClosed(proxy.childDone) {
		return
	}
	_ = signalChild(proxy.command)
	timer := time.NewTimer(proxy.options.StopTimeout)
	defer timer.Stop()
	select {
	case <-proxy.childDone:
	case <-timer.C:
		_ = killChild(proxy.command)
	}
}

func (proxy *Proxy) childResult() error {
	proxy.childMu.RLock()
	defer proxy.childMu.RUnlock()
	return proxy.childErr
}

func (proxy *Proxy) restore() {
	proxy.restoreOnce.Do(func() {
		if proxy.bridge != nil {
			proxy.restoreErr = errors.Join(proxy.bridge.Close(), proxy.endpoint.Restore())
			return
		}
		proxy.restoreErr = errors.Join(proxy.endpoint.Close(), proxy.endpoint.Restore())
	})
}

func cloneValues(values map[string]string) map[string]string {
	copy := make(map[string]string, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}

func isClosed(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

func stopTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}
