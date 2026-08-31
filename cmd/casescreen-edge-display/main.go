package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"casescreen/internal/edgedisplay"
)

type inputMessage struct {
	Values        map[string]string `json:"values"`
	Source        *string           `json:"source,omitempty"`
	TrackID       *string           `json:"track_id,omitempty"`
	Position      *int64            `json:"position,omitempty"`
	Playing       *bool             `json:"playing,omitempty"`
	LyricMode     *string           `json:"lyric_mode,omitempty"`
	LyricsCurrent *string           `json:"lyrics_current,omitempty"`
	LyricsNext    *string           `json:"lyrics_next,omitempty"`
}

type outputMessage struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func main() {
	var options edgedisplay.Options
	flag.StringVar(&options.DevicePath, "device", "/dev/ttyGS0", "原厂程序固定打开的设备节点")
	flag.StringVar(&options.BackupPath, "backup", "/dev/ttyGS0.casescreen-real", "原设备节点的临时备份路径")
	flag.StringVar(&options.LockPath, "lock", "/run/casescreen-edge-display.lock", "防止重复接管的锁文件")
	flag.StringVar(&options.ProgramPath, "program", "/data/PCMonitorLinux", "原厂 PCMonitorLinux 路径")
	flag.StringVar(&options.WorkingDirectory, "workdir", "/data", "原厂程序工作目录")
	flag.DurationVar(&options.ReadyTimeout, "ready-timeout", 10*time.Second, "等待原厂程序串口握手的最长时间")
	flag.DurationVar(&options.AckTimeout, "ack-timeout", 2*time.Second, "屏端本地注入命令 ACK 超时")
	flag.DurationVar(&options.HostAckTimeout, "host-ack-timeout", 10*time.Second, "电脑端原协议命令 ACK 超时（至少 5 秒）")
	flag.DurationVar(&options.TransferIdleTimeout, "transfer-idle-timeout", 15*time.Second, "电脑端文件裸流单次读写空闲超时")
	flag.DurationVar(&options.StopTimeout, "stop-timeout", 3*time.Second, "退出时等待原厂程序停止的时间")
	flag.Parse()

	options.ProgramArgs = flag.Args()
	options.Stdout = os.Stderr
	options.Stderr = os.Stderr

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	proxy, err := edgedisplay.Start(ctx, options)
	if err != nil {
		log.Fatalf("启动屏端显示注入层失败: %v", err)
	}
	log.Printf("屏端显示注入层已就绪: %v", proxy.DeviceInfo())

	waitResult := make(chan error, 1)
	go func() { waitResult <- proxy.Wait(ctx) }()
	messageChannel, inputResult := readMessages(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)

	for {
		select {
		case <-ctx.Done():
			err = <-waitResult
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("屏端显示注入层退出: %v", err)
			}
			return
		case err = <-waitResult:
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Fatalf("原厂 PCMonitorLinux 已退出: %v", err)
			}
			return
		case err = <-inputResult:
			closeErr := proxy.Close()
			if err != nil {
				log.Printf("读取注入数据失败: %v", err)
			}
			if closeErr != nil {
				log.Printf("清理屏端显示注入层失败: %v", closeErr)
			}
			return
		case message, open := <-messageChannel:
			if !open {
				messageChannel = nil
				continue
			}
			values := message.Values
			if values == nil {
				values = make(map[string]string, 8)
			}
			if hasLyricsUpdate(message) {
				update := lyricsUpdate(message)
				lyricsValues, valueErr := edgedisplay.LyricsValues(update)
				if valueErr != nil {
					_ = encoder.Encode(outputMessage{OK: false, Error: valueErr.Error()})
					continue
				}
				for key, value := range lyricsValues {
					values[key] = value
				}
			}
			sendErr := proxy.SendSystemData(ctx, values)
			response := outputMessage{OK: sendErr == nil}
			if sendErr != nil {
				response.Error = sendErr.Error()
			}
			if encodeErr := encoder.Encode(response); encodeErr != nil {
				log.Printf("输出注入结果失败: %v", encodeErr)
			}
		}
	}
}

func hasLyricsUpdate(message inputMessage) bool {
	return message.Source != nil || message.TrackID != nil || message.Position != nil ||
		message.Playing != nil || message.LyricMode != nil ||
		message.LyricsCurrent != nil || message.LyricsNext != nil
}

func lyricsUpdate(message inputMessage) edgedisplay.LyricsUpdate {
	var update edgedisplay.LyricsUpdate
	if message.Source != nil {
		update.Source = *message.Source
	}
	if message.TrackID != nil {
		update.TrackID = *message.TrackID
	}
	if message.Position != nil {
		update.Position = *message.Position
	}
	if message.Playing != nil {
		update.Playing = *message.Playing
	}
	if message.LyricMode != nil {
		update.LyricMode = edgedisplay.LyricMode(*message.LyricMode)
	}
	if message.LyricsCurrent != nil {
		update.Current = *message.LyricsCurrent
	}
	if message.LyricsNext != nil {
		update.Next = *message.LyricsNext
	}
	return update
}

func readMessages(reader io.Reader) (<-chan inputMessage, <-chan error) {
	messages := make(chan inputMessage)
	result := make(chan error, 1)
	go func() {
		defer close(messages)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64<<10), 2<<20)
		for scanner.Scan() {
			var message inputMessage
			if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
				result <- fmt.Errorf("解析 JSON 行: %w", err)
				return
			}
			messages <- message
		}
		result <- scanner.Err()
	}()
	return messages, result
}
