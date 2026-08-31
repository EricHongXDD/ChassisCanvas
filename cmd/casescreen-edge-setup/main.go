package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"casescreen/internal/edgesetup"
)

type commandOptions struct {
	listenAddress    string
	publicURL        string
	allowWildcard    bool
	sessionTTL       time.Duration
	completionMarker string
	wifiConfigPath   string
	wifiInterface    string
	wpaCLI           string
	wpaControlPath   string
	dateBinary       string
	hwclockBinary    string
	rtcPath          string
	caBundlePath     string
	qmlscene         string
	qmlFile          string
}

type portalExitReason uint8

const (
	portalExitCanceled portalExitReason = iota
	portalExitExpired
	portalExitCompleted
	portalExitServiceError
	portalExitDisplayError
)

func main() {
	ctx, stop := signalContext()
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		log.Printf("CaseScreen 一次性设置门户退出: %v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, stderr io.Writer) error {
	options, err := parseFlags(arguments, stderr)
	if err != nil {
		return err
	}
	listener, err := edgesetup.Listen(edgesetup.ListenOptions{
		Address: options.listenAddress, AllowWildcard: options.allowWildcard,
	})
	if err != nil {
		return err
	}
	defer listener.Close()
	if options.publicURL == "" {
		options.publicURL = publicURLForListener(listener)
	}

	var wifi edgesetup.WiFiController
	if options.wifiConfigPath != "" {
		store := &edgesetup.WPAConfigStore{
			Path: options.wifiConfigPath, Interface: options.wifiInterface,
		}
		store.Apply = func(applyContext context.Context, _ string) error {
			return runWPACommand(applyContext, options, "reconfigure")
		}
		store.IsConnected = func(statusContext context.Context) bool {
			command := exec.CommandContext(statusContext, options.wpaCLI,
				"-i", options.wifiInterface, "-p", options.wpaControlPath, "status")
			output, commandErr := command.Output()
			return commandErr == nil && strings.Contains(string(output), "wpa_state=COMPLETED")
		}
		wifi = store
	}
	clock := edgesetup.CommandClock{
		DateBinary: options.dateBinary, HWClockBinary: options.hwclockBinary,
		RTCPath: options.rtcPath,
	}
	portal, err := edgesetup.New(edgesetup.Config{
		PublicURL: options.publicURL, SessionTTL: options.sessionTTL, WiFi: wifi,
		Clock: clock, TLS: edgesetup.FileTLSProbe{CABundlePath: options.caBundlePath, Clock: clock},
	})
	if err != nil {
		return err
	}

	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	displayDone := make(chan error, 1)
	if strings.TrimSpace(options.qmlscene) != "" {
		go func() {
			displayDone <- runDisplay(runContext, options, portal)
		}()
	} else {
		displayDone = nil
	}
	server := &http.Server{
		Handler: portal.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	serverDone := make(chan error, 1)
	go func() {
		serveErr := server.Serve(listener)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		serverDone <- serveErr
	}()
	fmt.Fprintf(stderr, "一次性设置门户已在 %s 就绪；设置码只显示在屏幕，不写入日志。\n", portal.PublicURL())

	timer := time.NewTimer(time.Until(portal.ExpiresAt()))
	defer timer.Stop()
	var result error
	exitReason := portalExitCanceled
	select {
	case <-ctx.Done():
	case <-timer.C:
		exitReason = portalExitExpired
		fmt.Fprintln(stderr, "一次性设置窗口已到期。")
	case <-portal.Done():
		exitReason = portalExitCompleted
		fmt.Fprintln(stderr, "一次性设置已完成。")
	case result = <-serverDone:
		exitReason = portalExitServiceError
		serverDone = nil
	case displayErr := <-displayDone:
		exitReason = portalExitDisplayError
		displayDone = nil
		if displayErr != nil && !errors.Is(displayErr, context.Canceled) {
			result = fmt.Errorf("本地设置页退出: %w", displayErr)
		}
	}
	cancel()
	shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()
	shutdownErr := server.Shutdown(shutdownContext)
	if serverDone != nil {
		if serveErr := <-serverDone; result == nil {
			result = serveErr
		}
	}
	if displayDone != nil {
		select {
		case displayErr := <-displayDone:
			if displayErr != nil && !errors.Is(displayErr, context.Canceled) {
				result = errors.Join(result, fmt.Errorf("本地设置页退出: %w", displayErr))
			}
		case <-shutdownContext.Done():
			result = errors.Join(result, errors.New("本地设置页停止超时"))
		}
	}
	result = errors.Join(result, shutdownErr)
	return finalizeCompletionMarker(ctx, options.completionMarker, exitReason, result)
}

func parseFlags(arguments []string, stderr io.Writer) (commandOptions, error) {
	var options commandOptions
	set := flag.NewFlagSet("casescreen-edge-setup", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&options.listenAddress, "listen", "127.0.0.1:19324", "设置门户监听的 localhost 或显式数字 IP:端口")
	set.StringVar(&options.publicURL, "public-url", "", "屏幕显示的 HTTP URL；默认从实际监听地址生成")
	set.BoolVar(&options.allowWildcard, "allow-wildcard", false, "显式允许 0.0.0.0/[::]；通常不应启用")
	set.DurationVar(&options.sessionTTL, "session-ttl", 10*time.Minute, "一次性设置窗口时长（1 分钟..1 小时）")
	set.StringVar(&options.completionMarker, "completion-marker", "", "仅在已认证用户完成设置后原子写入的 0600 标记；空值禁用")
	set.StringVar(&options.wifiConfigPath, "wifi-config", "", "原子写入的 wpa_supplicant 配置；空值表示无线能力未确认")
	set.StringVar(&options.wifiInterface, "wifi-interface", "wlan0", "真机确认后的 Wi-Fi 接口")
	set.StringVar(&options.wpaCLI, "wpa-cli", "/usr/sbin/wpa_cli", "wpa_cli 路径")
	set.StringVar(&options.wpaControlPath, "wpa-control", "/etc/wifi/sockets", "wpa_supplicant 控制目录")
	set.StringVar(&options.dateBinary, "date", "/bin/date", "BusyBox date 路径")
	set.StringVar(&options.hwclockBinary, "hwclock", "/sbin/hwclock", "可选 RTC 写入命令")
	set.StringVar(&options.rtcPath, "rtc", "/dev/rtc0", "可选 RTC 设备；不存在时仅设置系统时间")
	set.StringVar(&options.caBundlePath, "ca-bundle", "/etc/ssl/certs/ca-certificates.crt", "平台 HTTPS 使用的 CA bundle；缺失时授权能力保持关闭")
	set.StringVar(&options.qmlscene, "qmlscene", "/arm-qt/bin/qmlscene", "本地 linuxfb 引导页程序；空值禁用本地显示")
	set.StringVar(&options.qmlFile, "qml", "/data/casescreen-setup.qml", "本地设置引导 QML")
	if err := set.Parse(arguments); err != nil {
		return commandOptions{}, err
	}
	if set.NArg() != 0 {
		return commandOptions{}, errors.New("一次性设置门户不接受位置参数")
	}
	if options.wifiConfigPath != "" &&
		(strings.TrimSpace(options.wifiInterface) == "" || strings.TrimSpace(options.wpaCLI) == "" ||
			strings.TrimSpace(options.wpaControlPath) == "") {
		return commandOptions{}, errors.New("启用 Wi-Fi 配置时必须指定接口、wpa_cli 和控制目录")
	}
	if strings.TrimSpace(options.qmlscene) != "" && strings.TrimSpace(options.qmlFile) == "" {
		return commandOptions{}, errors.New("启用本地设置页时必须指定 QML 文件")
	}
	return options, nil
}

func publicURLForListener(listener net.Listener) string {
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return ""
	}
	return "http://" + net.JoinHostPort(host, port)
}

func runWPACommand(ctx context.Context, options commandOptions, operation string) error {
	command := exec.CommandContext(ctx, options.wpaCLI,
		"-i", options.wifiInterface, "-p", options.wpaControlPath, operation)
	if err := command.Run(); err != nil {
		return errors.New("wpa_supplicant 操作失败")
	}
	return nil
}

func runDisplay(ctx context.Context, options commandOptions, portal *edgesetup.Portal) error {
	command := exec.CommandContext(ctx, options.qmlscene, options.qmlFile, "--",
		portal.PublicURL(), portal.BootstrapCode(), portal.ExpiresAt().Local().Format("15:04:05"))
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("qmlscene 无法显示一次性设置入口")
	}
	return nil
}
