package edgerecognition

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	defaultArecordPath   = "/usr/bin/arecord"
	defaultCaptureDevice = "default"
	maximumCapture       = 15 * time.Second
)

type arecordCommand func(context.Context, string, []string, int) ([]byte, error)

// ArecordOptions 只选择 ALSA 设备和可执行文件；格式固定为 16 kHz mono S16_LE。
type ArecordOptions struct {
	Path   string
	Device string
}

// ArecordRecorder 通过 stdout 接收原始 PCM，不创建临时文件。
type ArecordRecorder struct {
	path   string
	device string
	run    arecordCommand
	now    func() time.Time
}

// NewArecordRecorder 创建 Linux ALSA 录音器。可执行文件会在首次采样时启动，
// 因而主机侧测试和 ARM 交叉编译不依赖本机安装 arecord。
func NewArecordRecorder(options ArecordOptions) (*ArecordRecorder, error) {
	return newArecordRecorder(options, runArecord, time.Now)
}

func newArecordRecorder(options ArecordOptions, run arecordCommand, now func() time.Time) (*ArecordRecorder, error) {
	path := strings.TrimSpace(options.Path)
	if path == "" {
		path = defaultArecordPath
	}
	device := strings.TrimSpace(options.Device)
	if device == "" {
		device = defaultCaptureDevice
	}
	if strings.ContainsRune(path, '\x00') || strings.ContainsRune(device, '\x00') || len(device) > 128 {
		return nil, errors.New("arecord 路径或 ALSA 设备名无效")
	}
	if run == nil || now == nil {
		return nil, errors.New("arecord 执行器或时钟不能为空")
	}
	return &ArecordRecorder{path: path, device: device, run: run, now: now}, nil
}

// Capture 只允许整秒短采样，并把文件名设为“-”让 arecord 输出到 stdout。
func (recorder *ArecordRecorder) Capture(ctx context.Context, duration time.Duration) (Capture, error) {
	if recorder == nil || recorder.run == nil {
		return Capture{}, ErrAudioUnavailable
	}
	if ctx == nil {
		return Capture{}, errors.New("录音上下文不能为空")
	}
	if duration < time.Second || duration > maximumCapture || duration%time.Second != 0 {
		return Capture{}, errors.New("录音时长必须是 1..15 秒内的整秒数")
	}
	seconds := int(duration / time.Second)
	expectedBytes := seconds * SampleRateHz * ChannelCount * BytesPerSample
	arguments := []string{
		"-q", "-D", recorder.device,
		"-t", "raw", "-f", "S16_LE",
		"-r", strconv.Itoa(SampleRateHz), "-c", strconv.Itoa(ChannelCount),
		"-d", strconv.Itoa(seconds), "-",
	}
	pcm, err := recorder.run(ctx, recorder.path, arguments, expectedBytes)
	if err != nil {
		return Capture{}, fmt.Errorf("%w: %v", ErrAudioUnavailable, err)
	}
	if len(pcm) < expectedBytes*3/4 || len(pcm) > expectedBytes+SampleRateHz*BytesPerSample/10 || len(pcm)%2 != 0 {
		clear(pcm)
		return Capture{}, errors.New("arecord 返回的 PCM 长度异常")
	}
	if len(pcm) > expectedBytes {
		clear(pcm[expectedBytes:])
		pcm = pcm[:expectedBytes]
	}
	return Capture{PCM: pcm, SampledAt: recorder.now(), Duration: duration}, nil
}

func runArecord(ctx context.Context, path string, arguments []string, maximumBytes int) ([]byte, error) {
	command := exec.CommandContext(ctx, path, arguments...)
	stdout := boundedOutputBuffer{limit: maximumBytes + SampleRateHz*BytesPerSample/10}
	stderr := boundedOutputBuffer{limit: 4096}
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		// 不回显设备输出，避免把环境细节带入长期日志。
		return nil, err
	}
	if stdout.Len() > maximumBytes+SampleRateHz*BytesPerSample/10 {
		return nil, errors.New("录音输出超过预期上限")
	}
	return stdout.Bytes(), nil
}

type boundedOutputBuffer struct {
	bytes.Buffer
	limit int
}

func (buffer *boundedOutputBuffer) Write(data []byte) (int, error) {
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		return 0, errors.New("命令输出超过内存上限")
	}
	if len(data) > remaining {
		written, _ := buffer.Buffer.Write(data[:remaining])
		return written, errors.New("命令输出超过内存上限")
	}
	return buffer.Buffer.Write(data)
}
