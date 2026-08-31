package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"casescreen/internal/api"
)

type buildTarget struct {
	width  int
	height int
	path   string
}

func main() {
	ffmpegPath := flag.String("ffmpeg", "", "ffmpeg/ffmpeg.exe 路径；留空时从 PATH 查找")
	outputPath := flag.String("output", "", "单个 .Theme 输出路径")
	outputDirectory := flag.String("output-dir", "", "同时生成 1024x600 与 480x1920 主题的目录")
	width := flag.Int("width", 1024, "单个主题宽度")
	height := flag.Int("height", 600, "单个主题高度")
	flag.Parse()

	targets, err := requestedTargets(*outputPath, *outputDirectory, *width, *height)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for _, target := range targets {
		data, buildErr := api.BuildWaveshareStandaloneLyricsTheme(ctx, *ffmpegPath, target.width, target.height)
		if buildErr != nil {
			fmt.Fprintf(os.Stderr, "生成 %dx%d 微雪歌词主题失败: %v\n", target.width, target.height, buildErr)
			os.Exit(1)
		}
		if writeErr := writeTheme(target.path, data); writeErr != nil {
			fmt.Fprintf(os.Stderr, "写入 %s 失败: %v\n", target.path, writeErr)
			os.Exit(1)
		}
		digest := sha256.Sum256(data)
		fmt.Printf("%s\t%d bytes\tsha256=%x\n", target.path, len(data), digest)
	}
}

func requestedTargets(outputPath, outputDirectory string, width, height int) ([]buildTarget, error) {
	if outputPath != "" && outputDirectory != "" {
		return nil, fmt.Errorf("--output 与 --output-dir 不能同时使用")
	}
	if outputDirectory != "" {
		return []buildTarget{
			{width: 1024, height: 600, path: filepath.Join(outputDirectory, "casescreen-lyrics-1024x600.Theme")},
			{width: 480, height: 1920, path: filepath.Join(outputDirectory, "casescreen-lyrics-480x1920.Theme")},
		}, nil
	}
	if outputPath == "" {
		return nil, fmt.Errorf("请指定 --output 或 --output-dir")
	}
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("--width 与 --height 必须大于 0")
	}
	return []buildTarget{{width: width, height: height, path: outputPath}}, nil
}

func writeTheme(outputPath string, data []byte) error {
	if extension := filepath.Ext(outputPath); extension != ".Theme" && extension != ".theme" {
		return fmt.Errorf("输出文件必须使用 .Theme 扩展名")
	}
	directory := filepath.Dir(outputPath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	return os.WriteFile(outputPath, data, 0o600)
}
