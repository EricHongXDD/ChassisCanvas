//go:build casescreen_legacy_netease_cdp

// Package playerhost 管理音乐客户端的本机软件同步启动入口。
// 它不读取账号、Cookie、歌单或音频，也不控制任何副屏硬件。
package playerhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	NeteasePrivatePipeChannel = "private_pipe"

	NeteaseStateReady              = "ready"
	NeteaseStateStopped            = "stopped"
	NeteaseStateRunningWithoutPipe = "running_without_pipe"
	NeteaseStateNotInstalled       = "not_installed"
	NeteaseStateStarting           = "starting"
	NeteaseStateError              = "error"

	maximumNeteaseCDPFrameBytes   = 64 << 10
	maximumNeteaseCDPRequestBytes = 16 << 10
	maximumNeteaseSnapshotBytes   = 16 << 10
	maximumNeteaseCDPReplies      = 16
	maximumNeteaseCDPTargets      = 64
	neteaseLaunchWait             = 7 * time.Second
	neteaseLaunchPoll             = 100 * time.Millisecond
	neteasePlaybackRequestTimeout = 900 * time.Millisecond
)

var errNeteaseNotInstalled = errors.New("未在 Windows App Paths 中找到 cloudmusic.exe")
var errNeteaseTargetNotReady = errors.New("网易云 orpheus 页面尚未创建")

// neteasePlaybackExpression 只读取官方客户端主动暴露的当前播放接口。
// 不读取 Cookie、账号、歌单、历史或播放队列，也不向客户端写入状态。
// 前后各采样一次歌曲和状态，可拒绝恰好发生在切歌/暂停边界的混合快照。
const neteasePlaybackExpression = `(()=>{const a=window.__autotest__;if(!a)return{error:"autotest_unavailable"};const i1=a.playingInfo;if(!i1)return{empty:true,sampledAtMs:String(Date.now())};const s1=a.playingStatus;const p=a.playingProgress;const i2=a.playingInfo;const s2=a.playingStatus;const text=v=>typeof v==="string"?v.slice(0,512):"";const artists=Array.isArray(i1.artists)?i1.artists.slice(0,16).map(v=>typeof v==="string"?v.slice(0,256):(v&&typeof v.name==="string"?v.name.slice(0,256):"")):[];return{stable:Boolean(i2&&String(i1.resourceId)===String(i2.resourceId)&&String(i1.resourceType)===String(i2.resourceType)&&Number(i1.duration)===Number(i2.duration)&&s1===s2),sampledAtMs:String(Date.now()),status:s2,info:{name:text(i1.name),artists:artists,album:text(i1.album),resourceType:text(i1.resourceType),resourceId:String(i1.resourceId).slice(0,32),duration:i1.duration},progress:p?{current:p.current,duration:p.duration}:null}})()`

// NeteaseStatus 是可以安全返回给本机工作台的脱敏状态。
// 它不包含可执行文件路径、管道句柄、调试 target、账号或客户端内容。
type NeteaseStatus struct {
	Platform         string `json:"platform"`
	State            string `json:"state"`
	Installed        bool   `json:"installed"`
	Running          bool   `json:"running"`
	Connected        bool   `json:"connected"`
	CanLaunch        bool   `json:"can_launch"`
	RequiresExitOnce bool   `json:"requires_exit_once"`
	Channel          string `json:"channel"`
	Message          string `json:"message"`
}

// NeteaseController 供本机 API 使用，也便于 API 单测注入完全离线的 fake。
type NeteaseController interface {
	Status(context.Context) NeteaseStatus
	StartAndConnect(context.Context) NeteaseStatus
}

// NeteasePlaybackReader 是歌词模块唯一可见的窄接口。实现方只能返回固定
// window.__autotest__ 表达式的结果，调用者不能提交任意 CDP 方法或脚本。
type NeteasePlaybackReader interface {
	ReadPlayback(context.Context) (json.RawMessage, error)
}

type neteaseDependencies struct {
	locateExecutable func() (string, error)
	processRunning   func() (bool, error)
	launch           func(string) (io.ReadWriteCloser, error)
}

type neteaseInspection struct {
	status         NeteaseStatus
	executablePath string
}

// NeteaseHost 同时拥有客户端启动权和私有管道。管道从不监听 TCP，也不会
// 暴露给 API；同一实例应同时交给 API 状态入口和歌词状态源。
type NeteaseHost struct {
	mu      sync.Mutex
	deps    neteaseDependencies
	session *neteasePipeSession
}

func NewNeteaseHost() *NeteaseHost {
	return newNeteaseHost(newPlatformNeteaseDependencies())
}

func newNeteaseHost(dependencies neteaseDependencies) *NeteaseHost {
	return &NeteaseHost{deps: dependencies}
}

func (host *NeteaseHost) Status(ctx context.Context) NeteaseStatus {
	if host == nil {
		return neteaseErrorStatus("网易云播放信息同步尚未初始化")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	return host.inspectLocked(ctx).status
}

// StartAndConnect 只有在 cloudmusic.exe 确认没有运行时才启动客户端。
// 已运行但没有 CaseScreen 持有的私有管道时绝不杀进程、关闭或重启。
func (host *NeteaseHost) StartAndConnect(ctx context.Context) NeteaseStatus {
	if host == nil {
		return neteaseErrorStatus("网易云播放信息同步尚未初始化")
	}
	host.mu.Lock()
	defer host.mu.Unlock()

	initial := host.inspectLocked(ctx)
	if initial.status.Connected || initial.status.State != NeteaseStateStopped {
		return initial.status
	}
	// 把“未运行”检查尽量贴近 CreateProcess，缩小用户同时手动启动造成的竞态窗口。
	running, err := host.deps.processRunning()
	if err != nil {
		return neteaseErrorStatus("无法确认网易云音乐是否正在运行，已拒绝自动启动")
	}
	if running {
		return neteaseNeedsExitStatus(initial.status.Installed)
	}
	transport, err := host.deps.launch(initial.executablePath)
	if err != nil {
		// 具体系统错误可能包含用户目录；脱敏状态不向 API/UI 返回路径。
		log.Printf("网易云私有管道启动失败: %v", err)
		return neteaseErrorStatus("启动网易云音乐失败；未建立本机播放信息同步，请查看本机日志")
	}
	session := newNeteasePipeSession(transport)
	connectContext, cancel := context.WithTimeout(ctx, neteaseLaunchWait)
	for {
		err = session.connect(connectContext)
		if !errors.Is(err, errNeteaseTargetNotReady) {
			break
		}
		timer := time.NewTimer(neteaseLaunchPoll)
		select {
		case <-connectContext.Done():
			timer.Stop()
			err = connectContext.Err()
		case <-timer.C:
			continue
		}
		break
	}
	cancel()
	if err != nil {
		_ = session.Close()
		log.Printf("网易云私有管道握手失败: %v", err)
		status := neteaseErrorStatus("网易云已启动，但本机播放信息同步失败；请完全退出客户端后重试")
		status.Installed = true
		status.Running = true
		status.RequiresExitOnce = true
		return status
	}
	host.session = session
	status := baseNeteaseStatus()
	status.State = NeteaseStateReady
	status.Installed = true
	status.Running = true
	status.Connected = true
	status.Message = "网易云播放信息同步已就绪"
	return status
}

// ReadPlayback 只执行编译期固定表达式。任何超时、断管、协议错误或脚本异常
// 都立即销毁该会话；之后不会回退到端口、窗口标题或历史数据库。
func (host *NeteaseHost) ReadPlayback(ctx context.Context) (json.RawMessage, error) {
	if host == nil {
		return nil, errors.New("网易云私有管道尚未初始化")
	}
	host.mu.Lock()
	session := host.session
	if session != nil && session.closed() {
		host.session = nil
		session = nil
	}
	host.mu.Unlock()
	if session == nil {
		return nil, errors.New("网易云私有管道未连接")
	}
	readContext, cancel := context.WithTimeout(ctx, neteasePlaybackRequestTimeout)
	defer cancel()
	payload, err := session.evaluatePlayback(readContext)
	if err == nil {
		return payload, nil
	}
	host.mu.Lock()
	if host.session == session {
		host.session = nil
	}
	host.mu.Unlock()
	_ = session.Close()
	return nil, fmt.Errorf("读取网易云私有管道当前播放状态: %w", err)
}

// Close 只关闭 CaseScreen 自己持有的私有管道，不终止网易云客户端。
func (host *NeteaseHost) Close() error {
	if host == nil {
		return nil
	}
	host.mu.Lock()
	session := host.session
	host.session = nil
	host.mu.Unlock()
	if session != nil {
		return session.Close()
	}
	return nil
}

func (host *NeteaseHost) inspectLocked(ctx context.Context) neteaseInspection {
	status := baseNeteaseStatus()
	if err := ctx.Err(); err != nil {
		status.State = NeteaseStateError
		status.Message = "检测网易云主机状态超时；未执行任何客户端操作"
		return neteaseInspection{status: status}
	}
	if host.deps.locateExecutable == nil || host.deps.processRunning == nil || host.deps.launch == nil {
		status.State = NeteaseStateError
		status.Message = "当前系统不支持网易云播放信息同步启动"
		return neteaseInspection{status: status}
	}
	executablePath, locateErr := host.deps.locateExecutable()
	if locateErr == nil {
		status.Installed = true
	}
	running, runningErr := host.deps.processRunning()
	if runningErr != nil {
		status.State = NeteaseStateError
		status.Message = "无法只读检测网易云音乐进程，已拒绝自动启动"
		return neteaseInspection{status: status}
	}
	status.Running = running
	if running {
		if host.session != nil && !host.session.closed() {
			status.State = NeteaseStateReady
			status.Connected = true
			status.Message = "网易云播放信息同步已就绪"
			return neteaseInspection{status: status, executablePath: executablePath}
		}
		if host.session != nil {
			_ = host.session.Close()
			host.session = nil
		}
		return neteaseInspection{status: neteaseNeedsExitStatus(status.Installed), executablePath: executablePath}
	}
	if host.session != nil {
		_ = host.session.Close()
		host.session = nil
	}
	if locateErr != nil {
		if errors.Is(locateErr, errNeteaseNotInstalled) {
			status.State = NeteaseStateNotInstalled
			status.Message = "未在 Windows App Paths 中找到网易云音乐，请先安装官方客户端"
			return neteaseInspection{status: status}
		}
		status.State = NeteaseStateError
		status.Message = "读取网易云音乐 App Paths 失败，已拒绝自动启动"
		return neteaseInspection{status: status}
	}
	status.State = NeteaseStateStopped
	status.CanLaunch = true
	status.Message = "网易云音乐未运行，可由 CaseScreen 启动网易云并同步播放信息"
	return neteaseInspection{status: status, executablePath: executablePath}
}

func baseNeteaseStatus() NeteaseStatus {
	return NeteaseStatus{Platform: "netease", Channel: NeteasePrivatePipeChannel}
}

func neteaseNeedsExitStatus(installed bool) NeteaseStatus {
	status := baseNeteaseStatus()
	status.State = NeteaseStateRunningWithoutPipe
	status.Installed = installed
	status.Running = true
	status.RequiresExitOnce = true
	status.Message = "网易云已在运行但未建立本机播放信息同步；请完全退出网易云音乐后再点击“启动网易云并同步播放信息”，CaseScreen 不会自动关闭或重启客户端"
	return status
}

func neteaseErrorStatus(message string) NeteaseStatus {
	status := baseNeteaseStatus()
	status.State = NeteaseStateError
	status.Message = message
	return status
}

type neteasePipeSession struct {
	transport io.ReadWriteCloser

	requestMu sync.Mutex
	stateMu   sync.Mutex
	nextID    int
	sessionID string
	readErr   error

	frames    chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

func newNeteasePipeSession(transport io.ReadWriteCloser) *neteasePipeSession {
	session := &neteasePipeSession{
		transport: transport,
		frames:    make(chan []byte, maximumNeteaseCDPReplies),
		done:      make(chan struct{}),
	}
	go session.readFrames()
	return session
}

func (session *neteasePipeSession) connect(ctx context.Context) error {
	if session == nil || session.transport == nil {
		return errors.New("网易云私有管道为空")
	}
	result, err := session.call(ctx, "Target.getTargets", nil, "")
	if err != nil {
		return err
	}
	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			URL      string `json:"url"`
		} `json:"targetInfos"`
	}
	if err := json.Unmarshal(result, &targets); err != nil || len(targets.TargetInfos) > maximumNeteaseCDPTargets {
		return errors.New("网易云私有管道返回的 target 列表无效")
	}
	var targetID string
	matchingTargets := 0
	for _, target := range targets.TargetInfos {
		if target.Type == "page" && isNeteaseOrpheusURL(target.URL) && validCDPIdentifier(target.TargetID) {
			targetID = target.TargetID
			matchingTargets++
		}
	}
	if matchingTargets == 0 {
		return errNeteaseTargetNotReady
	}
	if matchingTargets != 1 {
		return errors.New("网易云私有管道返回多个 orpheus 页面，已拒绝猜测")
	}
	params, _ := json.Marshal(struct {
		TargetID string `json:"targetId"`
		Flatten  bool   `json:"flatten"`
	}{TargetID: targetID, Flatten: true})
	result, err = session.call(ctx, "Target.attachToTarget", params, "")
	if err != nil {
		return err
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(result, &attached) != nil || !validCDPIdentifier(attached.SessionID) {
		return errors.New("网易云私有管道返回的 sessionId 无效")
	}
	session.stateMu.Lock()
	session.sessionID = attached.SessionID
	session.stateMu.Unlock()
	return nil
}

func (session *neteasePipeSession) evaluatePlayback(ctx context.Context) (json.RawMessage, error) {
	if session == nil {
		return nil, errors.New("网易云私有管道会话为空")
	}
	session.stateMu.Lock()
	sessionID := session.sessionID
	session.stateMu.Unlock()
	if !validCDPIdentifier(sessionID) {
		return nil, errors.New("网易云私有管道尚未附加页面")
	}
	params, _ := json.Marshal(struct {
		Expression    string `json:"expression"`
		ReturnByValue bool   `json:"returnByValue"`
		AwaitPromise  bool   `json:"awaitPromise"`
	}{Expression: neteasePlaybackExpression, ReturnByValue: true})
	result, err := session.call(ctx, "Runtime.evaluate", params, sessionID)
	if err != nil {
		return nil, err
	}
	var evaluated struct {
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
		Result           struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	}
	if json.Unmarshal(result, &evaluated) != nil {
		return nil, errors.New("网易云私有管道 Runtime.evaluate 响应无效")
	}
	exception := bytes.TrimSpace(evaluated.ExceptionDetails)
	if len(exception) != 0 && !bytes.Equal(exception, []byte("null")) {
		return nil, errors.New("网易云只读表达式执行失败")
	}
	if evaluated.Result.Type != "object" {
		return nil, errors.New("网易云只读表达式返回类型无效")
	}
	value := bytes.TrimSpace(evaluated.Result.Value)
	if len(value) == 0 || len(value) > maximumNeteaseSnapshotBytes {
		return nil, errors.New("网易云播放快照为空或超过长度限制")
	}
	return append(json.RawMessage(nil), value...), nil
}

func (session *neteasePipeSession) call(ctx context.Context, method string, params json.RawMessage, sessionID string) (json.RawMessage, error) {
	if method != "Target.getTargets" && method != "Target.attachToTarget" && method != "Runtime.evaluate" {
		return nil, errors.New("拒绝执行非固定网易云 CDP 方法")
	}
	if method == "Runtime.evaluate" && !validCDPIdentifier(sessionID) {
		return nil, errors.New("Runtime.evaluate 缺少可信 sessionId")
	}
	if method != "Runtime.evaluate" && sessionID != "" {
		return nil, errors.New("浏览器级 CDP 方法不能携带 sessionId")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	session.requestMu.Lock()
	defer session.requestMu.Unlock()
	// 等待前一请求释放串行锁期间，调用者的期限可能已经到达。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if session.closed() {
		return nil, session.failure()
	}
	session.nextID++
	requestID := session.nextID
	request := struct {
		ID        int             `json:"id"`
		Method    string          `json:"method"`
		Params    json.RawMessage `json:"params,omitempty"`
		SessionID string          `json:"sessionId,omitempty"`
	}{ID: requestID, Method: method, Params: params, SessionID: sessionID}
	message, err := json.Marshal(request)
	if err != nil || len(message) > maximumNeteaseCDPRequestBytes {
		return nil, errors.New("网易云私有管道请求无效或超过长度限制")
	}
	message = append(message, 0)
	writeResult := make(chan error, 1)
	go func() {
		writeResult <- writeAll(session.transport, message)
	}()
	select {
	case <-ctx.Done():
		// 关闭父端可解除同步匿名管道上可能阻塞的 Write。
		session.fail(ctx.Err())
		return nil, ctx.Err()
	case <-session.done:
		return nil, session.failure()
	case err := <-writeResult:
		if err != nil {
			session.fail(fmt.Errorf("写入网易云私有管道: %w", err))
			return nil, session.failure()
		}
	}
	for replyIndex := 0; replyIndex < maximumNeteaseCDPReplies; replyIndex++ {
		select {
		case <-ctx.Done():
			session.fail(ctx.Err())
			return nil, ctx.Err()
		case <-session.done:
			return nil, session.failure()
		case frame := <-session.frames:
			var reply struct {
				ID     *int            `json:"id"`
				Method string          `json:"method,omitempty"`
				Result json.RawMessage `json:"result,omitempty"`
				Error  *struct {
					Code int `json:"code"`
				} `json:"error,omitempty"`
			}
			if json.Unmarshal(frame, &reply) != nil {
				session.fail(errors.New("网易云私有管道返回无效 JSON"))
				return nil, session.failure()
			}
			if reply.ID == nil {
				// CDP 事件没有请求 ID；未启用任何事件域，最多跳过有界数量。
				if strings.TrimSpace(reply.Method) == "" {
					session.fail(errors.New("网易云私有管道返回无 ID、无方法的消息"))
					return nil, session.failure()
				}
				continue
			}
			if *reply.ID != requestID {
				session.fail(errors.New("网易云私有管道响应 ID 与当前请求不一致"))
				return nil, session.failure()
			}
			if reply.Error != nil {
				return nil, fmt.Errorf("网易云 CDP %s 失败: code=%d", method, reply.Error.Code)
			}
			result := bytes.TrimSpace(reply.Result)
			if len(result) == 0 || bytes.Equal(result, []byte("null")) {
				return nil, errors.New("网易云私有管道响应缺少 result")
			}
			return append(json.RawMessage(nil), result...), nil
		}
	}
	session.fail(errors.New("网易云私有管道事件数量超过限制"))
	return nil, session.failure()
}

func (session *neteasePipeSession) readFrames() {
	if session == nil || session.transport == nil {
		return
	}
	buffer := make([]byte, 4096)
	pending := make([]byte, 0, 4096)
	for {
		count, err := session.transport.Read(buffer)
		if count > 0 {
			pending = append(pending, buffer[:count]...)
			for {
				terminator := bytes.IndexByte(pending, 0)
				if terminator < 0 {
					break
				}
				if terminator == 0 || terminator > maximumNeteaseCDPFrameBytes {
					session.fail(errors.New("网易云私有管道返回空消息或超长消息"))
					return
				}
				frame := append([]byte(nil), pending[:terminator]...)
				pending = pending[terminator+1:]
				select {
				case session.frames <- frame:
				case <-session.done:
					return
				default:
					session.fail(errors.New("网易云私有管道未消费消息超过限制"))
					return
				}
			}
			if len(pending) > maximumNeteaseCDPFrameBytes {
				session.fail(errors.New("网易云私有管道消息超过长度限制"))
				return
			}
		}
		if err != nil {
			session.fail(fmt.Errorf("读取网易云私有管道: %w", err))
			return
		}
		if count == 0 {
			session.fail(io.ErrNoProgress)
			return
		}
	}
}

func (session *neteasePipeSession) fail(err error) {
	if err == nil {
		err = errors.New("网易云私有管道已关闭")
	}
	session.closeOnce.Do(func() {
		session.stateMu.Lock()
		session.readErr = err
		session.stateMu.Unlock()
		close(session.done)
		if session.transport != nil {
			_ = session.transport.Close()
		}
	})
}

func (session *neteasePipeSession) failure() error {
	if session == nil {
		return errors.New("网易云私有管道会话为空")
	}
	session.stateMu.Lock()
	defer session.stateMu.Unlock()
	if session.readErr != nil {
		return session.readErr
	}
	return errors.New("网易云私有管道已关闭")
}

func (session *neteasePipeSession) closed() bool {
	if session == nil {
		return true
	}
	select {
	case <-session.done:
		return true
	default:
		return false
	}
}

func (session *neteasePipeSession) Close() error {
	if session == nil {
		return nil
	}
	session.fail(errors.New("网易云私有管道已由 CaseScreen 关闭"))
	return nil
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

func validCDPIdentifier(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

func isNeteaseOrpheusURL(value string) bool {
	// CEF target 必须是官方客户端当前版本已知的唯一规范 URL。这里直接比较
	// 原始字符串，避免接受空查询、空片段或 percent-encoding 等等价但非规范形式。
	return value == "orpheus://orpheus/pub/core.html"
}
