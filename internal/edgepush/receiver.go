package edgepush

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"casescreen/internal/edgesync"
	"casescreen/internal/lyrics"
)

const (
	defaultChallengeTTL  = time.Minute
	defaultSessionTTL    = 24 * time.Hour
	defaultStaleAfter    = 45 * time.Second
	defaultMaxChallenges = 64
	defaultMaxSessions   = 8
	responseWriteGrace   = 2 * time.Second
)

// Config 控制配对、防重放和状态失效边界。PairingKey 是每台屏幕独有的
// 256 位密钥；它只用于 HMAC，不会出现在 HTTP 请求或响应中。
type Config struct {
	PairingKey    string
	ChallengeTTL  time.Duration
	SessionTTL    time.Duration
	StaleAfter    time.Duration
	MaxChallenges int
	MaxSessions   int
	Now           func() time.Time
	Random        io.Reader
}

type challengeState struct {
	remote    string
	expiresAt time.Time
}

type sessionState struct {
	challenge string
	createdAt time.Time
	lastSeen  time.Time
	highest   uint64
	lastBody  [sha256.Size]byte
}

type playbackState struct {
	update     Update
	acceptedAt time.Time
	present    bool
}

// Receiver 是只保存易失性当前播放状态的 HTTP 接收器。它自然实现
// edge.MetadataClient 所需的 Current(context.Context) 方法，但不依赖 edge 包。
type Receiver struct {
	key           []byte
	challengeTTL  time.Duration
	sessionTTL    time.Duration
	staleAfter    time.Duration
	maxChallenges int
	maxSessions   int
	now           func() time.Time
	random        io.Reader
	handler       http.Handler

	mu            sync.Mutex
	challenges    map[string]challengeState
	sessions      map[string]*sessionState
	activeSession string
	playback      playbackState
}

var _ interface {
	Current(context.Context) (edgesync.Envelope, error)
} = (*Receiver)(nil)

// New 创建严格限定为三个固定路由的屏端接收器。
func New(config Config) (*Receiver, error) {
	key, err := decodePairingKey(config.PairingKey)
	if err != nil {
		return nil, err
	}
	config = normalizeConfig(config)
	if config.ChallengeTTL <= 0 || config.ChallengeTTL > 10*time.Minute {
		return nil, errors.New("挑战有效期必须在 0..10 分钟之间")
	}
	if config.SessionTTL <= 0 || config.SessionTTL > 7*24*time.Hour {
		return nil, errors.New("来源会话有效期必须在 0..7 天之间")
	}
	if config.StaleAfter <= 0 || config.StaleAfter > 24*time.Hour {
		return nil, errors.New("播放状态失效时间必须在 0..24 小时之间")
	}
	if config.MaxChallenges < 1 || config.MaxChallenges > 4096 {
		return nil, errors.New("挑战数量上限必须在 1..4096 之间")
	}
	if config.MaxSessions < 1 || config.MaxSessions > 256 {
		return nil, errors.New("来源会话数量上限必须在 1..256 之间")
	}
	receiver := &Receiver{
		key: append([]byte(nil), key...), challengeTTL: config.ChallengeTTL,
		sessionTTL: config.SessionTTL, staleAfter: config.StaleAfter,
		maxChallenges: config.MaxChallenges, maxSessions: config.MaxSessions,
		now: config.Now, random: config.Random,
		challenges: make(map[string]challengeState), sessions: make(map[string]*sessionState),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+ChallengePath, receiver.issueChallenge)
	mux.HandleFunc("POST "+SessionPath, receiver.openSession)
	mux.HandleFunc("PUT "+StatePath, receiver.acceptState)
	receiver.handler = securityHeaders(mux)
	return receiver, nil
}

func normalizeConfig(config Config) Config {
	if config.ChallengeTTL == 0 {
		config.ChallengeTTL = defaultChallengeTTL
	}
	if config.SessionTTL == 0 {
		config.SessionTTL = defaultSessionTTL
	}
	if config.StaleAfter == 0 {
		config.StaleAfter = defaultStaleAfter
	}
	if config.MaxChallenges == 0 {
		config.MaxChallenges = defaultMaxChallenges
	}
	if config.MaxSessions == 0 {
		config.MaxSessions = defaultMaxSessions
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	return config
}

// Handler 返回仅含 challenge、session 和 source-state 的 HTTP 处理器。
func (receiver *Receiver) Handler() http.Handler {
	if receiver == nil {
		return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeError(writer, http.StatusServiceUnavailable, "播放来源接收器尚未初始化")
		})
	}
	return receiver.handler
}

// Serve 在指定监听器上运行，并在上下文取消时有界关闭。
func (receiver *Receiver) Serve(ctx context.Context, listener net.Listener) error {
	if receiver == nil || receiver.handler == nil {
		return errors.New("播放来源接收器尚未初始化")
	}
	if ctx == nil {
		return errors.New("播放来源接收器上下文不能为空")
	}
	if listener == nil {
		return errors.New("播放来源接收器监听器不能为空")
	}
	server := &http.Server{
		Handler: receiver.handler, ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5*time.Second + responseWriteGrace,
		IdleTimeout: 15 * time.Second, MaxHeaderBytes: 8 << 10,
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = server.Shutdown(shutdownContext)
			cancel()
		case <-done:
		}
	}()
	err := server.Serve(listener)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Current 返回适配 edgesync.Envelope 的当前状态。播放中时位置从本机接收时刻
// 单调推进，因此 Runner 重复轮询同一次来源心跳也不会把歌词锚点冻结在旧位置。
func (receiver *Receiver) Current(ctx context.Context) (edgesync.Envelope, error) {
	if receiver == nil {
		return edgesync.Envelope{}, errors.New("播放来源接收器尚未初始化")
	}
	if ctx == nil {
		return edgesync.Envelope{}, errors.New("播放来源接收器上下文不能为空")
	}
	select {
	case <-ctx.Done():
		return edgesync.Envelope{}, ctx.Err()
	default:
	}
	now := receiver.now()
	receiver.mu.Lock()
	state := receiver.playback
	receiver.mu.Unlock()
	envelope := edgesync.Envelope{
		ProtocolVersion: edgesync.ProtocolVersion,
		SampledAtUnixMS: now.UnixMilli(),
		AuthRequired:    true,
		Metadata:        edgesync.Metadata{LyricMode: edgesync.LyricModeUnknown},
	}
	if !state.present || !state.update.Available || state.acceptedAt.IsZero() ||
		(!now.Before(state.acceptedAt) && now.Sub(state.acceptedAt) > receiver.staleAfter) {
		return envelope, nil
	}
	position := time.Duration(state.update.PositionMS) * time.Millisecond
	duration := time.Duration(state.update.DurationMS) * time.Millisecond
	if state.update.Playing && state.update.PositionKnown && now.After(state.acceptedAt) {
		position += now.Sub(state.acceptedAt)
	}
	if duration > 0 && position > duration {
		position = duration
	}
	envelope.Available = true
	envelope.Metadata.Source = state.update.Source
	envelope.Metadata.TrackID = state.update.TrackID
	envelope.Track = lyrics.NowPlaying{
		Position: int64(position), Duration: int64(duration),
		PositionKnown: state.update.PositionKnown, Playing: state.update.Playing,
	}
	return envelope, nil
}

func (receiver *Receiver) issueChallenge(writer http.ResponseWriter, request *http.Request) {
	unexpectedBody, readErr := io.ReadAll(io.LimitReader(request.Body, 1))
	if readErr != nil || request.ContentLength > 0 || len(unexpectedBody) > 0 {
		writeError(writer, http.StatusBadRequest, "挑战请求不能携带正文")
		return
	}
	random := make([]byte, challengeBytes)
	if _, err := io.ReadFull(receiver.random, random); err != nil {
		writeError(writer, http.StatusServiceUnavailable, "暂时无法生成来源挑战")
		return
	}
	now := receiver.now()
	challenge := base64.RawURLEncoding.EncodeToString(random)
	receiver.mu.Lock()
	receiver.pruneLocked(now)
	for len(receiver.challenges) >= receiver.maxChallenges {
		receiver.evictOldestChallengeLocked()
	}
	receiver.challenges[challenge] = challengeState{
		remote: remoteClient(request.RemoteAddr), expiresAt: now.Add(receiver.challengeTTL),
	}
	receiver.mu.Unlock()
	writeJSON(writer, http.StatusOK, ChallengeResponse{
		Version: ProtocolVersion, Challenge: challenge,
		ExpiresAtUnixMS: now.Add(receiver.challengeTTL).UnixMilli(),
	})
}

func (receiver *Receiver) openSession(writer http.ResponseWriter, request *http.Request) {
	body, status, err := readStrictJSON(request)
	if err != nil {
		writeError(writer, status, err.Error())
		return
	}
	var command SessionRequest
	if err := decodeStrict(body, &command); err != nil {
		writeError(writer, http.StatusBadRequest, "会话请求不是有效的严格 JSON")
		return
	}
	if command.Version != ProtocolVersion || !validChallenge(command.Challenge) || !validSessionID(command.Session) {
		writeError(writer, http.StatusBadRequest, "来源会话参数无效")
		return
	}
	if !verifySignature(receiver.key, sessionCanonical(body), request.Header.Get(HeaderMAC)) {
		writeError(writer, http.StatusUnauthorized, "来源认证失败")
		return
	}
	now := receiver.now()
	remote := remoteClient(request.RemoteAddr)
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	receiver.pruneLocked(now)
	if existing, exists := receiver.sessions[command.Session]; exists {
		if existing.challenge == command.Challenge {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(writer, http.StatusConflict, "来源会话 ID 已被使用")
		return
	}
	challenge, exists := receiver.challenges[command.Challenge]
	if !exists || now.After(challenge.expiresAt) || challenge.remote != remote {
		writeError(writer, http.StatusConflict, "来源挑战不存在、已过期或来源不匹配")
		return
	}
	delete(receiver.challenges, command.Challenge)
	for len(receiver.sessions) >= receiver.maxSessions {
		receiver.evictOldestSessionLocked()
	}
	receiver.sessions[command.Session] = &sessionState{
		challenge: command.Challenge, createdAt: now, lastSeen: now,
	}
	receiver.activeSession = command.Session
	// 新会话接管后立即丢弃上一来源的状态，避免短暂显示旧设备的歌曲。
	receiver.playback = playbackState{}
	writer.WriteHeader(http.StatusNoContent)
}

func (receiver *Receiver) acceptState(writer http.ResponseWriter, request *http.Request) {
	body, status, err := readStrictJSON(request)
	if err != nil {
		writeError(writer, status, err.Error())
		return
	}
	var update Update
	if err := decodeStrict(body, &update); err != nil {
		writeError(writer, http.StatusBadRequest, "播放状态不是有效的严格 JSON")
		return
	}
	if err := update.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	session := request.Header.Get(HeaderSession)
	if !validSessionID(session) {
		writeError(writer, http.StatusUnauthorized, "来源认证失败")
		return
	}
	sequenceText := request.Header.Get(HeaderSequence)
	sequence, parseErr := strconv.ParseUint(sequenceText, 10, 64)
	if parseErr != nil || sequence == 0 || strconv.FormatUint(sequence, 10) != sequenceText {
		writeError(writer, http.StatusBadRequest, "来源序号无效")
		return
	}
	if !verifySignature(receiver.key, stateCanonical(session, sequence, body), request.Header.Get(HeaderMAC)) {
		writeError(writer, http.StatusUnauthorized, "来源认证失败")
		return
	}
	now := receiver.now()
	bodyDigest := sha256.Sum256(body)
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	receiver.pruneLocked(now)
	state, exists := receiver.sessions[session]
	if !exists {
		writeError(writer, http.StatusUnauthorized, "来源会话不存在或已过期")
		return
	}
	if receiver.activeSession != session {
		writeError(writer, http.StatusConflict, "来源会话已被新的会话替换")
		return
	}
	if sequence < state.highest {
		writeError(writer, http.StatusConflict, "来源序号已过期")
		return
	}
	if sequence == state.highest {
		if sequence > 0 && hmacDigestEqual(bodyDigest, state.lastBody) {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(writer, http.StatusConflict, "相同来源序号携带了不同状态")
		return
	}
	state.highest = sequence
	state.lastBody = bodyDigest
	state.lastSeen = now
	receiver.playback = playbackState{update: update, acceptedAt: now, present: true}
	writer.WriteHeader(http.StatusNoContent)
}

func readStrictJSON(request *http.Request) ([]byte, int, error) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, http.StatusUnsupportedMediaType, errors.New("请求必须使用 application/json")
	}
	data, err := io.ReadAll(io.LimitReader(request.Body, maximumRequestBytes+1))
	if err != nil {
		return nil, http.StatusBadRequest, errors.New("无法读取请求正文")
	}
	if len(data) > maximumRequestBytes {
		return nil, http.StatusRequestEntityTooLarge, errors.New("请求正文超过 1 KiB")
	}
	if len(data) == 0 {
		return nil, http.StatusBadRequest, errors.New("请求正文不能为空")
	}
	return data, http.StatusOK, nil
}

func decodeStrict(data []byte, destination any) error {
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("JSON 正文尾部存在多余内容")
	}
	return nil
}

// rejectDuplicateKeys 防止不同 JSON 实现对重复字段采用不同解释。当前协议只有
// 顶层标量字段，因此只需检查顶层对象即可覆盖全部请求结构。
func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' {
		return errors.New("JSON 正文必须是对象")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("JSON 字段名称无效")
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("JSON 字段 %q 重复", key)
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return errors.New("JSON 对象没有正确结束")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("JSON 正文尾部存在多余内容")
	}
	return nil
}

func (receiver *Receiver) pruneLocked(now time.Time) {
	for challenge, state := range receiver.challenges {
		if now.After(state.expiresAt) {
			delete(receiver.challenges, challenge)
		}
	}
	for session, state := range receiver.sessions {
		if !now.Before(state.lastSeen) && now.Sub(state.lastSeen) > receiver.sessionTTL {
			delete(receiver.sessions, session)
			if receiver.activeSession == session {
				receiver.activeSession = ""
			}
		}
	}
}

func (receiver *Receiver) evictOldestChallengeLocked() {
	oldestKey := ""
	var oldest time.Time
	for key, state := range receiver.challenges {
		if oldestKey == "" || state.expiresAt.Before(oldest) {
			oldestKey, oldest = key, state.expiresAt
		}
	}
	delete(receiver.challenges, oldestKey)
}

func (receiver *Receiver) evictOldestSessionLocked() {
	oldestKey := ""
	var oldest time.Time
	for key, state := range receiver.sessions {
		if oldestKey == "" || state.lastSeen.Before(oldest) {
			oldestKey, oldest = key, state.lastSeen
		}
	}
	delete(receiver.sessions, oldestKey)
	if receiver.activeSession == oldestKey {
		receiver.activeSession = ""
	}
}

func remoteClient(remoteAddress string) string {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err == nil && host != "" {
		return host
	}
	if remoteAddress == "" {
		return "unknown"
	}
	return remoteAddress
}

func hmacDigestEqual(left, right [sha256.Size]byte) bool {
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(writer, request)
	})
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func (receiver *Receiver) String() string {
	if receiver == nil {
		return "edgepush.Receiver<nil>"
	}
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	return fmt.Sprintf("edgepush.Receiver{sessions:%d state:%t}", len(receiver.sessions), receiver.playback.present)
}
