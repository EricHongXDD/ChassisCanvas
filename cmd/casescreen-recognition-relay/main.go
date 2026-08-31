package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"casescreen/internal/recognitionrelay"
)

const version = "recognition-v1"

type options struct {
	listen                  string
	tokenDigests            string
	nativeMappings          string
	autoPlatform            string
	provider                string
	providerTimeout         time.Duration
	maximumRequestBytes     int64
	minimumAudio            time.Duration
	maximumAudio            time.Duration
	requestsPerSecond       float64
	burst                   int
	maximumConcurrent       int
	allowPublicHTTP         bool
	acrFingerprintAlgorithm string
	acrFingerprintVersion   string
	check                   bool
	showVersion             bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "CaseScreen recognition relay:", err)
		os.Exit(1)
	}
}

func run(arguments []string, stdout, stderr io.Writer) error {
	options, err := parseOptions(arguments, stderr)
	if err != nil {
		return err
	}
	if options.showVersion {
		_, _ = fmt.Fprintln(stdout, version)
		return nil
	}
	verifier, err := recognitionrelay.LoadTokenVerifier(options.tokenDigests)
	if err != nil {
		return err
	}
	mapper, err := recognitionrelay.LoadStaticMapper(options.nativeMappings, options.autoPlatform)
	if err != nil {
		return err
	}
	provider, err := buildProvider(options)
	if err != nil {
		return err
	}
	server, err := recognitionrelay.New(recognitionrelay.Config{
		Tokens: verifier, Provider: provider, Mapper: mapper,
		MaximumRequestBytes: options.maximumRequestBytes, ProviderTimeout: options.providerTimeout,
		MinimumAudioDuration: options.minimumAudio, MaximumAudioDuration: options.maximumAudio,
		RequestsPerSecond: options.requestsPerSecond, Burst: options.burst,
		MaximumConcurrent: options.maximumConcurrent,
	})
	if err != nil {
		return err
	}
	if options.check {
		_, _ = fmt.Fprintln(stdout, "识歌中继配置检查通过")
		return nil
	}
	if !options.allowPublicHTTP && !loopbackListenAddress(options.listen) {
		return errors.New("内置 HTTP 监听默认只能绑定回环地址；反向代理同机部署，容器网络需显式 -allow-public-http")
	}
	listener, err := net.Listen("tcp", options.listen)
	if err != nil {
		return fmt.Errorf("监听识歌中继失败: %w", err)
	}
	defer listener.Close()
	logger := log.New(stderr, "[casescreen-recognition-relay] ", log.LstdFlags|log.LUTC)
	if options.provider == "deterministic" {
		logger.Print("警告：deterministic provider 仅用于协议端到端测试，不是真实识歌")
	}
	logger.Printf("已启动：listen=%s provider=%s protocol=%s", listener.Addr(), options.provider, version)
	ctx, cancel := signalContext()
	defer cancel()
	return server.Serve(ctx, listener)
}

func parseOptions(arguments []string, stderr io.Writer) (options, error) {
	var result options
	set := flag.NewFlagSet("casescreen-recognition-relay", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&result.listen, "listen", "127.0.0.1:19323", "反向代理后方的 HTTP 监听地址")
	set.StringVar(&result.tokenDigests, "token-digests", "", "设备令牌 SHA-256 清单 JSON（必填）")
	set.StringVar(&result.nativeMappings, "native-mappings", "", "可选 QQ/网易云原生 ID 强映射 JSON")
	set.StringVar(&result.autoPlatform, "auto-platform", "none", "auto 请求的显式平台策略：none、qq 或 netease")
	set.StringVar(&result.provider, "provider", "acrcloud", "识歌 provider：acrcloud 或 deterministic（仅测试）")
	set.DurationVar(&result.providerTimeout, "provider-timeout", 12*time.Second, "单次上游识歌超时（1..30 秒）")
	set.Int64Var(&result.maximumRequestBytes, "max-request-bytes", 2<<20, "屏端 JSON 请求大小上限")
	set.DurationVar(&result.minimumAudio, "min-audio", 5*time.Second, "允许的最短采样时长")
	set.DurationVar(&result.maximumAudio, "max-audio", 15*time.Second, "允许的最长采样时长")
	set.Float64Var(&result.requestsPerSecond, "requests-per-second", 1, "每台设备的持续请求速率")
	set.IntVar(&result.burst, "burst", 3, "每台设备的突发请求数")
	set.IntVar(&result.maximumConcurrent, "max-concurrent", 4, "上游识歌最大并发数")
	set.BoolVar(&result.allowPublicHTTP, "allow-public-http", false, "容器网络专用：允许非回环明文监听，公网 TLS 仍必须由反向代理终止")
	set.StringVar(&result.acrFingerprintAlgorithm, "acr-fingerprint-algorithm", "", "可选的屏端 ACRCloud 声纹算法标识")
	set.StringVar(&result.acrFingerprintVersion, "acr-fingerprint-version", "", "可选的屏端 ACRCloud 声纹格式版本")
	set.BoolVar(&result.check, "check", false, "只验证配置后退出")
	set.BoolVar(&result.showVersion, "version", false, "显示协议版本后退出")
	if err := set.Parse(arguments); err != nil {
		return options{}, err
	}
	if set.NArg() != 0 {
		return options{}, errors.New("不接受位置参数")
	}
	result.provider = strings.ToLower(strings.TrimSpace(result.provider))
	if result.showVersion {
		return result, nil
	}
	if strings.TrimSpace(result.tokenDigests) == "" {
		return options{}, errors.New("必须指定 -token-digests")
	}
	if result.provider != "acrcloud" && result.provider != "deterministic" {
		return options{}, errors.New("provider 必须是 acrcloud 或 deterministic")
	}
	return result, nil
}

func buildProvider(options options) (recognitionrelay.Provider, error) {
	switch options.provider {
	case "deterministic":
		return recognitionrelay.DeterministicProvider{}, nil
	case "acrcloud":
		return recognitionrelay.NewACRCloudProvider(recognitionrelay.ACRCloudConfig{
			Host: os.Getenv("ACRCLOUD_HOST"), AccessKey: os.Getenv("ACRCLOUD_ACCESS_KEY"),
			AccessSecret: os.Getenv("ACRCLOUD_ACCESS_SECRET"), Timeout: options.providerTimeout,
			FingerprintAlgorithm: options.acrFingerprintAlgorithm,
			FingerprintVersion:   options.acrFingerprintVersion,
		})
	default:
		return nil, errors.New("未知识歌 provider")
	}
}

func loopbackListenAddress(address string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
