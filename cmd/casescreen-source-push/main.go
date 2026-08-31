// casescreen-source-push 是平台官方适配器与屏端 edgepush 接收器之间的
// 单次发送桥梁。调用方必须显式提供当前播放对象中的平台原生歌曲 ID；本命令
// 不读取 SMTC、窗口标题或音频，也不通过搜索推断歌曲 ID。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"casescreen/internal/edgepush"
)

const (
	defaultTimeout       = 5 * time.Second
	maximumTimeout       = 30 * time.Second
	maximumKeyFileBytes  = 1 << 10
	maximumTrackTimeInMS = int64((24 * time.Hour) / time.Millisecond)
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "发送失败:", err)
		os.Exit(1)
	}
}

func run(parent context.Context, args []string, stdout, stderr io.Writer) error {
	if parent == nil {
		return errors.New("发送上下文不能为空")
	}
	flags := flag.NewFlagSet("casescreen-source-push", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var (
		screenURL  string
		keyFile    string
		source     string
		trackID    string
		positionMS int64
		durationMS int64
		playing    bool
		clearState bool
		timeout    time.Duration
	)
	flags.StringVar(&screenURL, "screen-url", "", "屏端 edgepush 服务基础地址")
	flags.StringVar(&keyFile, "key-file", "", "64 位十六进制配对密钥文件")
	flags.StringVar(&source, "source", "", "原生 ID 所属平台：qq 或 netease")
	flags.StringVar(&trackID, "track-id", "", "平台当前播放对象直接提供的原生歌曲 ID")
	flags.Int64Var(&positionMS, "position-ms", 0, "当前播放位置（毫秒）")
	flags.Int64Var(&durationMS, "duration-ms", 0, "歌曲总时长（毫秒，未知可为 0）")
	flags.BoolVar(&playing, "playing", false, "当前是否正在播放")
	flags.BoolVar(&clearState, "clear", false, "清除屏端当前播放状态")
	flags.DurationVar(&timeout, "timeout", defaultTimeout, "本次发送超时")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("不接受位置参数")
	}
	if strings.TrimSpace(screenURL) == "" {
		return errors.New("必须提供 -screen-url")
	}
	if strings.TrimSpace(keyFile) == "" {
		return errors.New("必须提供 -key-file")
	}
	if timeout <= 0 || timeout > maximumTimeout {
		return errors.New("-timeout 必须在 0..30 秒之间")
	}
	if err := validateMilliseconds(positionMS, durationMS); err != nil {
		return err
	}
	if clearState {
		if source != "" || trackID != "" || positionMS != 0 || durationMS != 0 || playing {
			return errors.New("-clear 不能同时携带歌曲或播放字段")
		}
	} else if source == "" || trackID == "" {
		return errors.New("发送播放状态必须提供 -source 和 -track-id")
	}

	pairingKey, err := readPairingKey(keyFile)
	if err != nil {
		return err
	}
	publisher, err := edgepush.NewPublisher(edgepush.PublisherConfig{
		BaseURL: screenURL, PairingKey: pairingKey,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if clearState {
		if err := publisher.Clear(ctx); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(stdout, "播放清空状态已发送")
		return nil
	}

	position := time.Duration(positionMS) * time.Millisecond
	duration := time.Duration(durationMS) * time.Millisecond
	var playback edgepush.NativePlayback
	switch source {
	case "qq":
		playback, err = edgepush.QQNativePlayback(trackID, position, duration, playing)
	case "netease":
		playback, err = edgepush.NeteaseNativePlayback(trackID, position, duration, playing)
	default:
		return errors.New("-source 仅支持 qq 或 netease")
	}
	if err != nil {
		return err
	}
	if err := publisher.Send(ctx, playback); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, "播放状态已发送")
	return nil
}

func validateMilliseconds(position, duration int64) error {
	if position < 0 || position > maximumTrackTimeInMS {
		return errors.New("-position-ms 必须在 0..86400000 之间")
	}
	if duration < 0 || duration > maximumTrackTimeInMS {
		return errors.New("-duration-ms 必须在 0..86400000 之间")
	}
	if duration > 0 && position > duration {
		return errors.New("-position-ms 不能超过 -duration-ms")
	}
	return nil
}

func readPairingKey(path string) (string, error) {
	if path != strings.TrimSpace(path) || path == "" {
		return "", errors.New("配对密钥文件路径无效")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.New("无法读取配对密钥文件")
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumKeyFileBytes {
		return "", errors.New("配对密钥文件必须是 1 KiB 以内的普通文件")
	}
	// Unix 上拒绝组或其他用户可读写的密钥；Windows ACL 不映射为这些位。
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("配对密钥文件权限过宽，请设置为 0600")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("无法读取配对密钥文件")
	}
	value := string(data)
	if strings.HasSuffix(value, "\r\n") {
		value = strings.TrimSuffix(value, "\r\n")
	} else if strings.HasSuffix(value, "\n") {
		value = strings.TrimSuffix(value, "\n")
	}
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\r\n\t ") {
		return "", errors.New("配对密钥文件格式无效")
	}
	return value, nil
}
