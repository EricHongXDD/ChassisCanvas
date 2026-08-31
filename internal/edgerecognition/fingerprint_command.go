package edgerecognition

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

const (
	maximumFingerprintBytes           = 64 << 10
	maximumFingerprintAlgorithmLength = 64
	maximumFingerprintVersionLength   = 32
)

type fingerprintCommand func(context.Context, string, []string, []byte, int) ([]byte, error)

// CommandFingerprintOptions 定义外部 ARM32 声纹提取器契约。程序从 stdin 读取
// 固定格式 PCM，从 stdout 只输出声纹二进制；不使用任何临时文件。
type CommandFingerprintOptions struct {
	Path      string
	Algorithm string
	Version   string
}

type CommandFingerprintExtractor struct {
	path      string
	algorithm string
	version   string
	run       fingerprintCommand
}

// NewCommandFingerprintExtractor 创建外部声纹适配器。该函数不声称固件已经
// 自带第三方 SDK；镜像必须另外部署与目标中继匹配的 ARMv7 提取器。
func NewCommandFingerprintExtractor(options CommandFingerprintOptions) (*CommandFingerprintExtractor, error) {
	return newCommandFingerprintExtractor(options, runFingerprintCommand)
}

func newCommandFingerprintExtractor(options CommandFingerprintOptions, run fingerprintCommand) (*CommandFingerprintExtractor, error) {
	path := strings.TrimSpace(options.Path)
	algorithm := strings.TrimSpace(options.Algorithm)
	version := strings.TrimSpace(options.Version)
	if path == "" || strings.ContainsRune(path, '\x00') {
		return nil, errors.New("声纹提取器路径无效")
	}
	if !safeProtocolLabel(algorithm, maximumFingerprintAlgorithmLength) ||
		!safeProtocolLabel(version, maximumFingerprintVersionLength) {
		return nil, errors.New("声纹算法或版本标识无效")
	}
	if run == nil {
		return nil, errors.New("声纹提取命令执行器不能为空")
	}
	return &CommandFingerprintExtractor{path: path, algorithm: algorithm, version: version, run: run}, nil
}

func (extractor *CommandFingerprintExtractor) Extract(ctx context.Context, capture Capture) (Fingerprint, error) {
	if extractor == nil || extractor.run == nil {
		return Fingerprint{}, errors.New("声纹提取器尚未初始化")
	}
	if ctx == nil || len(capture.PCM) == 0 || len(capture.PCM)%BytesPerSample != 0 {
		return Fingerprint{}, errors.New("声纹提取输入无效")
	}
	arguments := []string{
		"--format", AudioFormat,
		"--sample-rate", strconv.Itoa(SampleRateHz),
		"--channels", strconv.Itoa(ChannelCount),
		"--duration-ms", strconv.FormatInt(capture.Duration.Milliseconds(), 10),
	}
	data, err := extractor.run(ctx, extractor.path, arguments, capture.PCM, maximumFingerprintBytes)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("声纹提取失败: %w", err)
	}
	if len(data) == 0 || len(data) > maximumFingerprintBytes {
		clear(data)
		return Fingerprint{}, errors.New("声纹提取器输出大小无效")
	}
	return Fingerprint{Algorithm: extractor.algorithm, Version: extractor.version, Data: data}, nil
}

func runFingerprintCommand(ctx context.Context, path string, arguments []string, pcm []byte, maximum int) ([]byte, error) {
	command := exec.CommandContext(ctx, path, arguments...)
	command.Stdin = bytes.NewReader(pcm)
	stdout := boundedOutputBuffer{limit: maximum}
	command.Stdout = &stdout
	// 不记录第三方工具 stderr，防止意外把凭据或音频诊断写入长期日志。
	command.Stderr = nil
	if err := command.Run(); err != nil {
		return nil, err
	}
	if stdout.Len() > maximum {
		return nil, errors.New("声纹输出超过上限")
	}
	return stdout.Bytes(), nil
}

func safeProtocolLabel(value string, maximum int) bool {
	if value == "" || maximum < 1 || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.') {
			return false
		}
	}
	return true
}
