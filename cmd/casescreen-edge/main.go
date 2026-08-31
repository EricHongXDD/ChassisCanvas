package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"casescreen/internal/edge"
	"casescreen/internal/edgedisplay"
	"casescreen/internal/edgelyrics"
	"casescreen/internal/edgemetadata"
	"casescreen/internal/edgeprobe"
	"casescreen/internal/edgepush"
	"casescreen/internal/edgerecognition"
	"casescreen/internal/edgesync"
	"casescreen/internal/platformauth"
)

type commandOptions struct {
	metadataMode                    string
	syncURL                         string
	tokenFile                       string
	lyricsCache                     string
	lyricsMode                      string
	authDirectory                   string
	identityCache                   string
	standalonePlatform              string
	identityRetry                   time.Duration
	recognitionRelayURL             string
	recognitionTokenFile            string
	recognitionPCM                  string
	recognitionArecord              string
	recognitionAllowHTTP            bool
	recognitionChunk                time.Duration
	recognitionWindow               time.Duration
	recognitionSearchInterval       time.Duration
	recognitionSteadyInterval       time.Duration
	recognitionTimeout              time.Duration
	recognitionMinConfidence        float64
	recognitionConfirmations        int
	recognitionPauseSilent          int
	recognitionResumeActive         int
	recognitionPreferredPlatform    string
	recognitionFingerprintExtractor string
	recognitionFingerprintAlgorithm string
	recognitionFingerprintVersion   string
	recognitionConfigPath           string
	sourceListen                    string
	sourceKeyFile                   string
	sourceStaleAfter                time.Duration
	qqCookieFile                    string
	neteaseCookieFile               string
	caBundle                        string
	hardwareReport                  string
	startupTheme                    string
	pollInterval                    time.Duration
	renderInterval                  time.Duration
	syncTimeout                     time.Duration
	lyricsTimeout                   time.Duration
	lyricsRetryMin                  time.Duration
	lyricsRetryMax                  time.Duration
	display                         edgedisplay.Options
}

func main() {
	ctx, stop := signalContext()
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		log.Printf("CaseScreen 屏端服务退出: %v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer) (runError error) {
	options, programArguments, err := parseFlags(arguments, stderr)
	if err != nil {
		return err
	}
	recognitionStage := ""
	var recognitionReport edgeprobe.Report
	defer func() {
		if runError == nil || options.metadataMode != "recognition" || recognitionStage != "initializing" {
			return
		}
		// 启动前门禁已经通过，但登录态、CA、令牌、声纹程序或显示接管仍可能
		// 初始化失败。将这一阶段失败写回同一份脱敏报告，避免 TF 上残留“就绪”。
		markRecognitionStartupFailure(&recognitionReport)
		if writeErr := writeRecognitionReport(options.hardwareReport, recognitionReport); writeErr != nil {
			runError = errors.Join(runError, fmt.Errorf("写入识歌初始化失败报告: %w", writeErr))
		}
	}()

	idleNotice := ""
	if options.metadataMode == "recognition" {
		recognitionStage = "preflight"
		report, probeErr := collectRecognitionReadiness(ctx, options)
		recognitionReport = report
		if writeErr := writeRecognitionReport(options.hardwareReport, report); writeErr != nil {
			return fmt.Errorf("写入独立识歌启动前报告: %w", writeErr)
		}
		if probeErr != nil {
			return fmt.Errorf("独立识歌启动前预检失败: %w", probeErr)
		}
		if readyErr := recognitionReadinessError(report, "独立识歌启动前未就绪"); readyErr != nil {
			return readyErr
		}
		idleNotice = edgeprobe.IdleNotice(report)
		recognitionStage = "initializing"
		fmt.Fprintln(stderr, "独立识歌启动前预检通过：已留下脱敏 readiness 报告")
	}
	if err := preparePrivateDirectory(options.lyricsCache); err != nil {
		return fmt.Errorf("准备歌词缓存: %w", err)
	}
	if options.metadataMode != "recognition" && strings.TrimSpace(options.hardwareReport) != "" {
		report, reportErr := edgeprobe.Collect("/", time.Now())
		if reportErr != nil {
			fmt.Fprintf(stderr, "屏端硬件预检失败：%v\n", reportErr)
		} else {
			idleNotice = edgeprobe.IdleNotice(report)
			if writeErr := edgeprobe.WriteAtomic(options.hardwareReport, report); writeErr != nil {
				fmt.Fprintf(stderr, "写入屏端硬件报告失败：%v\n", writeErr)
			} else {
				fmt.Fprintf(stderr, "屏端硬件报告已写入 %s：%s\n", options.hardwareReport, report)
			}
		}
	}

	var authStore *platformauth.Store
	var authSession edgelyrics.AuthSession
	if options.lyricsMode == "direct" {
		if strings.TrimSpace(options.authDirectory) == "" {
			if options.metadataMode == "bluez" || options.metadataMode == "source-push" || options.metadataMode == "recognition" {
				return errors.New("独立元数据模式必须通过 -auth-dir 启用平台登录态存储")
			}
		} else {
			authStore, err = platformauth.Open(options.authDirectory)
			if err != nil {
				return fmt.Errorf("打开 QQ/网易云登录态: %w", err)
			}
			authSession = authStore
			if err := importConfiguredPlatformCookies(authStore, options, stderr); err != nil {
				return err
			}
			fmt.Fprintf(stderr, "屏端平台登录态：%s\n", authStore)
		}
	} else if strings.TrimSpace(options.qqCookieFile) != "" || strings.TrimSpace(options.neteaseCookieFile) != "" {
		return errors.New("登录态导入文件只能与 -lyrics-mode direct 一起使用")
	}

	var metadataClient edge.MetadataClient
	var coreFactory edge.CoreFactory
	var sourceReceiver *edgepush.Receiver
	var sourceListener net.Listener
	loginNotice := edge.PlatformLoginNotice
	runnerSyncTimeout := options.syncTimeout
	directHTTPClient, err := newDirectHTTPClient(options.caBundle, maxDuration(options.lyricsTimeout, options.recognitionTimeout))
	if err != nil && options.lyricsMode == "direct" {
		return err
	}
	switch options.metadataMode {
	case "relay":
		if strings.TrimSpace(options.syncURL) == "" {
			return errors.New("relay 元数据模式必须通过 -sync-url 指定主机 now-playing 同步地址")
		}
		token, tokenErr := readTokenFile(options.tokenFile)
		if tokenErr != nil {
			return tokenErr
		}
		// now-playing 由 Runner 的短上下文限制；同一个安全客户端还要允许低频
		// `/v1/lyrics` 使用较长超时，因此底层 HTTP 上限取两者较大值。
		syncClient, clientErr := edgesync.NewClient(options.syncURL, token, &http.Client{
			Timeout: maxDuration(options.syncTimeout, options.lyricsTimeout),
		})
		if clientErr != nil {
			return fmt.Errorf("初始化 now-playing 同步客户端: %w", clientErr)
		}
		metadataClient = syncClient
		switch options.lyricsMode {
		case "relay":
			coreFactory = func() edge.LyricsEngine {
				return edge.NewRelayLyricsCore(options.lyricsCache, syncClient)
			}
			fmt.Fprintln(stderr, "歌词模式：主机 relay（副屏不直接访问 QQ 音乐或网易云音乐）")
		case "direct":
			coreFactory = func() edge.LyricsEngine {
				return edgelyrics.NewWithAuth(options.lyricsCache, directHTTPClient, authSession)
			}
			fmt.Fprintln(stderr, "歌词模式：direct（副屏直接访问平台，已显式启用）")
		}
	case "bluez":
		if options.lyricsMode != "direct" {
			return errors.New("BlueZ 独立元数据模式必须使用 -lyrics-mode direct")
		}
		if err := preparePrivateDirectory(options.identityCache); err != nil {
			return fmt.Errorf("准备歌曲身份缓存: %w", err)
		}
		metadataClient, coreFactory, err = edge.NewStandalonePipeline(edge.StandaloneOptions{
			Source: edgemetadata.NewBlueZSource(), Platform: options.standalonePlatform,
			IdentityCacheDir: options.identityCache, LyricsCacheDir: options.lyricsCache,
			HTTPClient: directHTTPClient, Session: authSession,
			IdentityRetry: options.identityRetry,
		})
		if err != nil {
			return fmt.Errorf("初始化 BlueZ 独立歌曲链路: %w", err)
		}
		loginNotice = edge.StandalonePlatformLoginNotice
		// 独立元数据客户端在切歌时还会执行一次同平台身份确认；该请求不能
		// 被 relay 默认的 5 秒轮询窗口提前取消，显示滚动仍由独立 goroutine 驱动。
		runnerSyncTimeout = maxDuration(options.syncTimeout, options.lyricsTimeout)
		fmt.Fprintln(stderr, "实验独立模式：BlueZ AVRCP 元数据 + 严格同平台匹配 + TF 歌词缓存（AVRCP 不提供原生歌曲 ID）")
	case "recognition":
		if options.lyricsMode != "direct" {
			return errors.New("屏端识歌模式必须使用 -lyrics-mode direct")
		}
		if strings.TrimSpace(options.recognitionRelayURL) == "" {
			return errors.New("屏端识歌模式必须通过 -recognition-relay-url 指定识歌中继")
		}
		recognitionToken, tokenErr := readTokenFile(options.recognitionTokenFile)
		if tokenErr != nil {
			return fmt.Errorf("读取识歌中继令牌: %w", tokenErr)
		}
		recorder, recorderErr := edgerecognition.NewArecordRecorder(edgerecognition.ArecordOptions{
			Path: options.recognitionArecord, Device: options.recognitionPCM,
		})
		if recorderErr != nil {
			return fmt.Errorf("初始化屏端 ALSA 录音: %w", recorderErr)
		}
		provider, providerErr := edgerecognition.NewHTTPProvider(edgerecognition.HTTPConfig{
			Endpoint: options.recognitionRelayURL, Token: recognitionToken,
			Client: directHTTPClient, RequestTimeout: options.recognitionTimeout,
			AllowInsecureHTTP: options.recognitionAllowHTTP,
			PreferredPlatform: options.recognitionPreferredPlatform,
		})
		if providerErr != nil {
			return fmt.Errorf("初始化识歌中继: %w", providerErr)
		}
		var fingerprintExtractor edgerecognition.FingerprintExtractor
		if strings.TrimSpace(options.recognitionFingerprintExtractor) != "" {
			fingerprintExtractor, err = edgerecognition.NewCommandFingerprintExtractor(edgerecognition.CommandFingerprintOptions{
				Path:      options.recognitionFingerprintExtractor,
				Algorithm: options.recognitionFingerprintAlgorithm,
				Version:   options.recognitionFingerprintVersion,
			})
			if err != nil {
				return fmt.Errorf("初始化屏端声纹提取器: %w", err)
			}
		} else {
			fmt.Fprintf(stderr, "识歌带宽提示：未配置 ARMv7 声纹提取器，将回退为低频 %v PCM JSON 上传\n",
				options.recognitionWindow)
		}
		metadataClient, err = edgerecognition.NewClient(edgerecognition.Config{
			Recorder: recorder, Extractor: fingerprintExtractor, Provider: provider,
			ChunkDuration:      options.recognitionChunk,
			WindowDuration:     options.recognitionWindow,
			SearchInterval:     options.recognitionSearchInterval,
			SteadyInterval:     options.recognitionSteadyInterval,
			RecognitionTimeout: options.recognitionTimeout,
			MinimumConfidence:  options.recognitionMinConfidence,
			Confirmations:      options.recognitionConfirmations,
			PauseAfterSilent:   options.recognitionPauseSilent,
			ResumeAfterActive:  options.recognitionResumeActive,
			PreferredPlatform:  options.recognitionPreferredPlatform,
			ErrorSink: func(message string) {
				fmt.Fprintf(stderr, "[recognition] %s\n", message)
			},
		})
		if err != nil {
			return fmt.Errorf("初始化屏端识歌状态机: %w", err)
		}
		coreFactory = func() edge.LyricsEngine {
			return edgelyrics.NewWithAuth(options.lyricsCache, directHTTPClient, authSession)
		}
		loginNotice = edge.StandalonePlatformLoginNotice
		// HTTP/声纹提取在独立 goroutine 中有界运行；Runner 只需覆盖一个短块录音。
		runnerSyncTimeout = maxDuration(options.syncTimeout, options.recognitionChunk+2*time.Second)
		if options.pollInterval > 250*time.Millisecond {
			options.pollInterval = 250 * time.Millisecond
		}
		fmt.Fprintf(stderr, "屏端识歌模式：%v ALSA 短块 + %v RAM 环 + 本地静音门控 + 异步 HTTPS 中继\n",
			options.recognitionChunk, options.recognitionWindow)
		fmt.Fprintln(stderr, "只有中继显式验证的 QQ/网易云原生歌曲 ID 才会用于 direct 取词；不会按标题猜测 ID")
	case "source-push":
		pairingKey, keyErr := readTokenFile(options.sourceKeyFile)
		if keyErr != nil {
			return fmt.Errorf("读取播放来源配对密钥: %w", keyErr)
		}
		sourceReceiver, err = edgepush.New(edgepush.Config{
			PairingKey: pairingKey, StaleAfter: options.sourceStaleAfter,
		})
		if err != nil {
			return fmt.Errorf("初始化播放来源接收器: %w", err)
		}
		metadataClient = sourceReceiver
		switch options.lyricsMode {
		case "relay":
			if strings.TrimSpace(options.syncURL) == "" {
				return errors.New("source-push 的 relay 歌词模式必须通过 -sync-url 指定电脑端 EdgeSync 地址")
			}
			token, tokenErr := readTokenFile(options.tokenFile)
			if tokenErr != nil {
				return tokenErr
			}
			lyricsRelay, clientErr := edgesync.NewClient(options.syncURL, token, &http.Client{
				Timeout: maxDuration(options.syncTimeout, options.lyricsTimeout),
			})
			if clientErr != nil {
				return fmt.Errorf("初始化电脑端精确歌词客户端: %w", clientErr)
			}
			coreFactory = func() edge.LyricsEngine {
				return edge.NewRelayLyricsCore(options.lyricsCache, lyricsRelay)
			}
			fmt.Fprintln(stderr, "准确来源歌词：电脑端同平台按 ID 获取，TF 未命中时仅传一次完整歌词")
		case "direct":
			coreFactory = func() edge.LyricsEngine {
				return edgelyrics.NewWithAuth(options.lyricsCache, directHTTPClient, authSession)
			}
			loginNotice = edge.StandalonePlatformLoginNotice
			fmt.Fprintln(stderr, "准确来源歌词：副屏 direct 直连同平台（需要外网、CA 与屏端登录态）")
		}
		sourceListener, err = net.Listen("tcp", options.sourceListen)
		if err != nil {
			return fmt.Errorf("监听播放来源小包地址 %s: %w", options.sourceListen, err)
		}
		fmt.Fprintf(stderr, "准确来源模式：监听 %s，只接收已配对来源发送的 QQ/网易云原生歌曲 ID 小包\n",
			sourceListener.Addr())
	}
	if sourceListener != nil {
		defer sourceListener.Close()
	}

	options.display.ProgramArgs = programArguments
	options.display.Stdout = stderr
	options.display.Stderr = stderr
	display, err := edgedisplay.Start(ctx, options.display)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "原厂显示渲染器已就绪：%v\n", display.DeviceInfo())
	if options.startupTheme != "" {
		themeContext, themeCancel := context.WithTimeout(ctx, options.display.AckTimeout)
		themeErr := display.ReloadTheme(themeContext, options.startupTheme)
		themeCancel()
		if themeErr != nil {
			return errors.Join(fmt.Errorf("切换屏端驻留主题: %w", themeErr), display.Close())
		}
		fmt.Fprintf(stderr, "已切换屏端驻留主题：%s\n", options.startupTheme)
	}
	if options.metadataMode == "recognition" {
		recognitionStage = "runtime_gate"
		// 必须在原厂渲染器和驻留主题均启动后再试录，才能发现主题音频控件
		// 与识歌进程争用同一 ALSA capture 设备的真实运行时冲突。
		report, probeErr := collectRecognitionReadiness(ctx, options)
		recognitionReport = report
		if writeErr := writeRecognitionReport(options.hardwareReport, report); writeErr != nil {
			return errors.Join(fmt.Errorf("写入独立识歌运行时报告: %w", writeErr), display.Close())
		}
		if probeErr != nil {
			return errors.Join(fmt.Errorf("独立识歌运行时预检失败: %w", probeErr), display.Close())
		}
		idleNotice = edgeprobe.IdleNotice(report)
		if readyErr := recognitionReadinessError(report, "独立识歌运行时未就绪"); readyErr != nil {
			return errors.Join(readyErr, display.Close())
		}
		recognitionStage = "running"
		fmt.Fprintln(stderr, "独立识歌运行时预检通过：ALSA、独立网络、DNS/CA、可信时间与中继令牌均就绪")
	}

	runner, err := edge.NewRunner(edge.Config{
		PollInterval: options.pollInterval, RenderInterval: options.renderInterval,
		SyncTimeout: runnerSyncTimeout, LyricsRetryMin: options.lyricsRetryMin,
		LyricsRetryMax:      options.lyricsRetryMax,
		PlatformLoginNotice: loginNotice,
		IdleNotice:          idleNotice,
		ErrorSink: func(scope, message string) {
			fmt.Fprintf(stderr, "[%s] %s\n", scope, message)
		},
	}, metadataClient, coreFactory, display)
	if err != nil {
		return errors.Join(err, display.Close())
	}

	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	runnerDone := make(chan error, 1)
	displayDone := make(chan error, 1)
	var sourceDone chan error
	go func() { runnerDone <- runner.Run(runContext) }()
	go func() { displayDone <- display.Wait(runContext) }()
	if sourceReceiver != nil {
		sourceDone = make(chan error, 1)
		go func() { sourceDone <- sourceReceiver.Serve(runContext, sourceListener) }()
	}

	var result error
	select {
	case <-ctx.Done():
		cancel()
	case err := <-displayDone:
		if err == nil {
			err = errors.New("原厂 PCMonitorLinux 已退出")
		}
		result = err
		cancel()
		displayDone = nil
	case err := <-runnerDone:
		if err == nil {
			err = errors.New("屏端歌词编排循环意外退出")
		}
		result = err
		cancel()
		runnerDone = nil
	case err := <-sourceDone:
		if err == nil {
			err = errors.New("播放来源小包接收服务意外退出")
		}
		result = err
		cancel()
		sourceDone = nil
	}
	if runnerDone != nil {
		if err := <-runnerDone; result == nil && err != nil && !errors.Is(err, context.Canceled) {
			result = err
		}
	}
	if displayDone != nil {
		if err := <-displayDone; result == nil && err != nil && !errors.Is(err, context.Canceled) {
			result = err
		}
	}
	if sourceDone != nil {
		if err := <-sourceDone; result == nil && err != nil && !errors.Is(err, context.Canceled) {
			result = err
		}
	}
	closeErr := display.Close()
	if ctx.Err() != nil {
		result = nil
	}
	return errors.Join(result, closeErr)
}

func parseFlags(arguments []string, stderr io.Writer) (commandOptions, []string, error) {
	var options commandOptions
	set := flag.NewFlagSet("casescreen-edge", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&options.metadataMode, "metadata-mode", "relay", "播放信息来源：relay、bluez（实验）、recognition（屏端识歌）或 source-push（来源原生 ID）")
	set.StringVar(&options.syncURL, "sync-url", "", "主机 CaseScreen Edge Sync 地址，例如 http://192.168.1.2:19322")
	set.StringVar(&options.tokenFile, "token-file", "/data/casescreen/sync.token", "保存 64 位十六进制配对令牌的 0600 文件")
	set.StringVar(&options.lyricsCache, "lyrics-cache", "/data/casescreen/lyrics-cache", "按平台歌曲 ID 保存的歌词缓存目录")
	set.StringVar(&options.lyricsMode, "lyrics-mode", "relay", "歌词来源：relay（默认，由主机获取）或 direct（屏端直连平台）")
	set.StringVar(&options.authDirectory, "auth-dir", "/data/casescreen/platform-auth", "仅 direct 模式使用的 QQ/网易云加密登录态目录；空值禁用")
	set.StringVar(&options.identityCache, "identity-cache", "/data/casescreen/identity-cache", "BlueZ 独立模式按歌曲信息保存的平台 ID 缓存目录")
	set.StringVar(&options.standalonePlatform, "standalone-platform", "auto", "BlueZ 播放器平台：auto、qq 或 netease")
	set.DurationVar(&options.identityRetry, "identity-retry", 30*time.Second, "同平台歌曲 ID 查询失败后的重试间隔")
	set.StringVar(&options.recognitionRelayURL, "recognition-relay-url", "", "屏端识歌 HTTPS JSON 中继地址")
	set.StringVar(&options.recognitionTokenFile, "recognition-token-file", "/data/casescreen/recognition.token", "保存可撤销识歌中继令牌的 0600 文件")
	set.StringVar(&options.recognitionPCM, "recognition-pcm", "plughw:audiocodec", "arecord 使用的 ALSA PCM 设备；plughw 可把原生 3 声道转换为单声道")
	set.StringVar(&options.recognitionArecord, "recognition-arecord", "/usr/bin/arecord", "屏端 arecord 可执行文件")
	set.BoolVar(&options.recognitionAllowHTTP, "recognition-allow-http", false, "仅本地调试：允许明文 HTTP 识歌中继")
	set.DurationVar(&options.recognitionChunk, "recognition-chunk", time.Second, "本地 VAD 使用的短块采样时长（1..2 秒）")
	set.DurationVar(&options.recognitionWindow, "recognition-capture", 10*time.Second, "识歌使用的 RAM 音频窗口时长")
	set.DurationVar(&options.recognitionSearchInterval, "recognition-search-interval", 2*time.Second, "未确认歌曲时两次短采样之间的间隔")
	set.DurationVar(&options.recognitionSteadyInterval, "recognition-steady-interval", 15*time.Second, "歌曲稳定后两次复核采样之间的间隔")
	set.DurationVar(&options.recognitionTimeout, "recognition-timeout", 12*time.Second, "单次识歌中继请求超时")
	set.Float64Var(&options.recognitionMinConfidence, "recognition-min-confidence", 0.78, "接受识别结果的最低置信度（0..1）")
	set.IntVar(&options.recognitionConfirmations, "recognition-confirmations", 2, "首次识别或切歌需要的连续相同结果次数")
	set.IntVar(&options.recognitionPauseSilent, "recognition-pause-silent", 2, "连续静音采样达到该次数后暂停歌词时间轴")
	set.IntVar(&options.recognitionResumeActive, "recognition-resume-active", 2, "连续有声采样达到该次数后先恢复本地歌词时间轴")
	set.StringVar(&options.recognitionPreferredPlatform, "recognition-preferred-platform", "auto", "中继原生歌曲 ID 映射偏好：auto、qq 或 netease")
	set.StringVar(&options.recognitionFingerprintExtractor, "recognition-fingerprint-extractor", "", "可选 ARMv7 声纹提取程序；stdin 读 PCM、stdout 写声纹")
	set.StringVar(&options.recognitionFingerprintAlgorithm, "recognition-fingerprint-algorithm", "", "外部声纹算法标识，需与中继一致")
	set.StringVar(&options.recognitionFingerprintVersion, "recognition-fingerprint-version", "", "外部声纹格式版本，需与中继一致")
	set.StringVar(&options.recognitionConfigPath, "recognition-config", "/data/casescreen/recognition.env", "独立识歌 readiness gate 使用的 0600 配置文件")
	set.StringVar(&options.sourceListen, "source-listen", "0.0.0.0:19323", "source-push 的 HMAC 小包监听地址")
	set.StringVar(&options.sourceKeyFile, "source-key-file", "/data/casescreen/source-push.key", "source-push 每屏独有的 64 位十六进制配对密钥文件")
	set.DurationVar(&options.sourceStaleAfter, "source-stale-after", 45*time.Second, "来源心跳停止后清空旧歌曲的时间")
	set.StringVar(&options.qqCookieFile, "qq-cookie-file", "", "一次性导入 QQ 音乐 Netscape cookie.txt；成功后删除明文文件")
	set.StringVar(&options.neteaseCookieFile, "netease-cookie-file", "", "一次性导入网易云 Netscape cookie.txt；成功后删除明文文件")
	set.StringVar(&options.caBundle, "ca-bundle", "", "direct 模式使用的 PEM 根证书包；空值使用系统根证书")
	set.StringVar(&options.hardwareReport, "hardware-report", "", "将不含 SSID、密码或登录态的硬件预检写入 JSON")
	set.StringVar(&options.startupTheme, "startup-theme", "", "启动后切换到 /data/theme 下已经驻留的 .Theme；不会上传文件")
	set.DurationVar(&options.pollInterval, "poll-interval", time.Second, "now-playing 轮询间隔（250ms..30s）")
	set.DurationVar(&options.renderInterval, "render-interval", 500*time.Millisecond, "本地歌词注入间隔（250ms..1s）")
	set.DurationVar(&options.syncTimeout, "sync-timeout", 5*time.Second, "单次 now-playing 请求超时")
	set.DurationVar(&options.lyricsTimeout, "lyrics-timeout", 20*time.Second, "切歌时精确歌词请求超时")
	set.DurationVar(&options.lyricsRetryMin, "lyrics-retry-min", 5*time.Second, "歌词联网失败后的首次重试间隔")
	set.DurationVar(&options.lyricsRetryMax, "lyrics-retry-max", time.Minute, "歌词联网失败的最大退避")
	set.StringVar(&options.display.DevicePath, "device", "/dev/ttyGS0", "原厂程序固定打开的设备节点")
	set.StringVar(&options.display.BackupPath, "backup", "/dev/ttyGS0.casescreen-real", "原设备节点的临时备份路径")
	set.StringVar(&options.display.LockPath, "lock", "/run/casescreen-edge-display.lock", "防止重复接管 ttyGS0 的锁文件")
	set.StringVar(&options.display.ProgramPath, "program", "/data/PCMonitorLinux", "原厂 PCMonitorLinux 路径；进程继承已加载的 qt_env.sh 环境")
	set.StringVar(&options.display.WorkingDirectory, "workdir", "/data", "原厂程序工作目录")
	set.DurationVar(&options.display.ReadyTimeout, "ready-timeout", 10*time.Second, "等待原厂串口握手的最长时间")
	set.DurationVar(&options.display.AckTimeout, "ack-timeout", 2*time.Second, "屏端本地注入命令 ACK 超时")
	set.DurationVar(&options.display.HostAckTimeout, "host-ack-timeout", 10*time.Second, "电脑端原协议命令 ACK 超时（至少 5 秒）")
	set.DurationVar(&options.display.TransferIdleTimeout, "transfer-idle-timeout", 15*time.Second, "电脑端文件裸流单次读写空闲超时")
	set.DurationVar(&options.display.StopTimeout, "stop-timeout", 3*time.Second, "退出时等待原厂程序停止的时间")
	if err := set.Parse(arguments); err != nil {
		return commandOptions{}, nil, err
	}
	if options.lyricsTimeout <= 0 || options.lyricsTimeout > time.Minute {
		return commandOptions{}, nil, errors.New("歌词请求超时必须在 0..60 秒之间")
	}
	options.metadataMode = strings.ToLower(strings.TrimSpace(options.metadataMode))
	if options.metadataMode != "relay" && options.metadataMode != "bluez" && options.metadataMode != "recognition" && options.metadataMode != "source-push" {
		return commandOptions{}, nil, errors.New("播放信息来源必须是 relay、bluez、recognition 或 source-push")
	}
	options.lyricsMode = strings.ToLower(strings.TrimSpace(options.lyricsMode))
	if options.lyricsMode != "relay" && options.lyricsMode != "direct" {
		return commandOptions{}, nil, errors.New("歌词模式必须是 relay 或 direct")
	}
	options.standalonePlatform = strings.ToLower(strings.TrimSpace(options.standalonePlatform))
	if options.standalonePlatform != "auto" && options.standalonePlatform != "qq" && options.standalonePlatform != "netease" {
		return commandOptions{}, nil, errors.New("BlueZ 播放器平台必须是 auto、qq 或 netease")
	}
	if options.identityRetry < time.Second || options.identityRetry > 24*time.Hour {
		return commandOptions{}, nil, errors.New("歌曲身份重试间隔必须在 1 秒..24 小时之间")
	}
	if options.recognitionChunk < time.Second || options.recognitionChunk > 2*time.Second || options.recognitionChunk%time.Second != 0 {
		return commandOptions{}, nil, errors.New("识歌短块必须是 1..2 秒内的整秒数")
	}
	if options.recognitionWindow < 5*time.Second || options.recognitionWindow > 30*time.Second ||
		options.recognitionWindow%options.recognitionChunk != 0 {
		return commandOptions{}, nil, errors.New("识歌 RAM 窗口必须是 5..30 秒且为短块整数倍")
	}
	if options.recognitionSearchInterval < 250*time.Millisecond || options.recognitionSearchInterval > 24*time.Hour ||
		options.recognitionSteadyInterval < 250*time.Millisecond || options.recognitionSteadyInterval > 24*time.Hour {
		return commandOptions{}, nil, errors.New("识歌采样间隔必须在 250 毫秒..24 小时")
	}
	if options.recognitionTimeout < time.Second || options.recognitionTimeout > 30*time.Second {
		return commandOptions{}, nil, errors.New("识歌中继请求超时必须在 1..30 秒")
	}
	if options.recognitionMinConfidence <= 0 || options.recognitionMinConfidence > 1 ||
		options.recognitionConfirmations < 1 || options.recognitionConfirmations > 5 ||
		options.recognitionPauseSilent < 1 || options.recognitionPauseSilent > 10 ||
		options.recognitionResumeActive < 1 || options.recognitionResumeActive > 10 {
		return commandOptions{}, nil, errors.New("识歌置信度、确认次数或静音迟滞参数无效")
	}
	preferred := strings.ToLower(strings.TrimSpace(options.recognitionPreferredPlatform))
	if preferred == "qqmusic" {
		preferred = "qq"
	}
	if preferred == "cloudmusic" || preferred == "163" {
		preferred = "netease"
	}
	if preferred != "auto" && preferred != "qq" && preferred != "netease" {
		return commandOptions{}, nil, errors.New("识歌偏好平台必须是 auto、qq 或 netease")
	}
	options.recognitionPreferredPlatform = preferred
	hasExtractor := strings.TrimSpace(options.recognitionFingerprintExtractor) != ""
	hasAlgorithm := strings.TrimSpace(options.recognitionFingerprintAlgorithm) != ""
	hasVersion := strings.TrimSpace(options.recognitionFingerprintVersion) != ""
	if hasExtractor != hasAlgorithm || hasExtractor != hasVersion {
		return commandOptions{}, nil, errors.New("外部声纹提取器、算法和版本必须一起配置")
	}
	options.recognitionConfigPath = filepath.Clean(strings.TrimSpace(options.recognitionConfigPath))
	if options.recognitionConfigPath == "" || options.recognitionConfigPath == "." ||
		!strings.HasPrefix(filepath.ToSlash(options.recognitionConfigPath), "/") {
		return commandOptions{}, nil, errors.New("独立识歌配置文件必须是屏端绝对路径")
	}
	if options.sourceStaleAfter < 5*time.Second || options.sourceStaleAfter > 10*time.Minute {
		return commandOptions{}, nil, errors.New("来源播放状态失效时间必须在 5 秒..10 分钟之间")
	}
	if options.pollInterval < 250*time.Millisecond || options.pollInterval > 30*time.Second {
		return commandOptions{}, nil, errors.New("now-playing 轮询间隔必须在 250 毫秒..30 秒之间")
	}
	if options.renderInterval < 250*time.Millisecond || options.renderInterval > time.Second {
		return commandOptions{}, nil, errors.New("屏端歌词注入间隔必须在 250..1000 毫秒之间")
	}
	if options.syncTimeout <= 0 || options.syncTimeout > 30*time.Second {
		return commandOptions{}, nil, errors.New("now-playing 请求超时必须在 0..30 秒之间")
	}
	if options.display.HostAckTimeout < 5*time.Second || options.display.HostAckTimeout > 2*time.Minute {
		return commandOptions{}, nil, errors.New("电脑端原协议 ACK 超时必须在 5 秒..2 分钟之间")
	}
	if options.display.TransferIdleTimeout < time.Second || options.display.TransferIdleTimeout > 5*time.Minute {
		return commandOptions{}, nil, errors.New("文件裸流空闲超时必须在 1 秒..5 分钟之间")
	}
	if options.lyricsRetryMin <= 0 || options.lyricsRetryMax < options.lyricsRetryMin || options.lyricsRetryMax > 24*time.Hour {
		return commandOptions{}, nil, errors.New("歌词重试退避参数无效")
	}
	options.startupTheme = strings.TrimSpace(options.startupTheme)
	if options.startupTheme != "" &&
		(!strings.HasPrefix(options.startupTheme, "/data/theme/") ||
			!strings.EqualFold(filepath.Ext(options.startupTheme), ".Theme")) {
		return commandOptions{}, nil, errors.New("启动主题必须是 /data/theme 下的 .Theme 文件")
	}
	return options, set.Args(), nil
}

const maximumCookieImportSize = 1 << 20

func importConfiguredPlatformCookies(store *platformauth.Store, options commandOptions, stderr io.Writer) error {
	for _, item := range []struct {
		platform string
		path     string
		label    string
	}{
		{platform: "qq", path: options.qqCookieFile, label: "QQ 音乐"},
		{platform: "netease", path: options.neteaseCookieFile, label: "网易云音乐"},
	} {
		result, imported, err := importPlatformCookieFile(store, item.platform, item.path)
		if err != nil {
			return fmt.Errorf("导入%s登录态失败: %w", item.label, err)
		}
		if imported {
			fmt.Fprintf(stderr, "%s登录态已加密保存：接受 %d 项，忽略 %d 项；明文导入文件已删除\n",
				item.label, result.Accepted, result.Ignored)
		}
	}
	return nil
}

func importPlatformCookieFile(store *platformauth.Store, platform, path string) (platformauth.ImportResult, bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return platformauth.ImportResult{}, false, nil
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return platformauth.ImportResult{}, false, nil
	}
	if err != nil {
		return platformauth.ImportResult{}, false, errors.New("无法读取登录态导入文件")
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumCookieImportSize {
		return platformauth.ImportResult{}, false, errors.New("登录态导入文件必须是 1 MiB 内的非空普通文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return platformauth.ImportResult{}, false, errors.New("无法读取登录态导入文件")
	}
	result, err := store.ImportNetscape(platform, string(data))
	for index := range data {
		data[index] = 0
	}
	if err != nil {
		return platformauth.ImportResult{}, false, err
	}
	if err := os.Remove(path); err != nil {
		return platformauth.ImportResult{}, false, errors.New("登录态已加密保存，但无法删除明文导入文件")
	}
	return result, true, nil
}

func readTokenFile(path string) (string, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || path == "." {
		return "", errors.New("配对令牌文件路径无效")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", errors.New("无法读取配对令牌文件")
	}
	if !info.Mode().IsRegular() || info.Size() > 1024 {
		return "", errors.New("配对令牌文件必须是小型普通文件")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("配对令牌文件权限过宽，必须设置为 0600")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("无法读取配对令牌文件")
	}
	text := strings.TrimSuffix(string(data), "\n")
	text = strings.TrimSuffix(text, "\r")
	if strings.ContainsAny(text, "\r\n\t ") {
		return "", errors.New("配对令牌文件格式无效")
	}
	return text, nil
}

func preparePrivateDirectory(path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || path == "." {
		return errors.New("目录路径无效")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
}

func collectRecognitionReadiness(ctx context.Context, options commandOptions) (edgeprobe.Report, error) {
	probeTimeout := options.recognitionChunk + 4*time.Second
	probeContext, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	report, err := edgeprobe.CollectWithOptions(probeContext, "/", time.Now(), edgeprobe.Options{
		PCMDevice:             options.recognitionPCM,
		ArecordPath:           options.recognitionArecord,
		CABundlePath:          options.caBundle,
		SampleDuration:        options.recognitionChunk,
		SampleTimeout:         probeTimeout,
		TemporaryDirectory:    "/tmp",
		RecognitionConfigPath: options.recognitionConfigPath,
	})
	if err != nil && len(report.StandaloneRecognition.Reasons) == 0 {
		report.Checks.StandaloneRecognitionReady = false
		report.StandaloneRecognition = edgeprobe.RecognitionReadiness{
			Ready: false, Reasons: []string{"独立识歌动态门禁执行失败"},
		}
	}
	return report, err
}

func writeRecognitionReport(path string, report edgeprobe.Report) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	return edgeprobe.WriteAtomic(path, report)
}

func recognitionReadinessError(report edgeprobe.Report, prefix string) error {
	if report.StandaloneRecognition.Ready {
		return nil
	}
	reasons := strings.Join(report.StandaloneRecognition.Reasons, "；")
	if reasons == "" {
		reasons = "未知 readiness 条件未满足"
	}
	return fmt.Errorf("%s: %s", prefix, reasons)
}

func markRecognitionStartupFailure(report *edgeprobe.Report) {
	if report == nil {
		return
	}
	const reason = "识歌服务依赖或显示接管初始化失败；请查看本机服务日志"
	report.Checks.StandaloneRecognitionReady = false
	report.StandaloneRecognition.Ready = false
	for _, existing := range report.StandaloneRecognition.Reasons {
		if existing == reason {
			return
		}
	}
	report.StandaloneRecognition.Reasons = append([]string{reason}, report.StandaloneRecognition.Reasons...)
}

func newDirectHTTPClient(caBundle string, timeout time.Duration) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	caBundle = filepath.Clean(strings.TrimSpace(caBundle))
	if caBundle != "" && caBundle != "." {
		info, err := os.Stat(caBundle)
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 4<<20 {
			return nil, errors.New("direct 模式的 CA 根证书包无效")
		}
		data, err := os.ReadFile(caBundle)
		if err != nil {
			return nil, errors.New("无法读取 direct 模式的 CA 根证书包")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			return nil, errors.New("direct 模式的 CA 根证书包不含有效 PEM 证书")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Timeout: timeout, Transport: transport}, nil
}
