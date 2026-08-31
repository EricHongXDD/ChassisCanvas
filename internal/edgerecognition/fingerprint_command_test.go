package edgerecognition

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCommandFingerprintExtractorUsesStdinStdoutContract(t *testing.T) {
	var gotPath string
	var gotArguments []string
	var gotPCM []byte
	runner := func(_ context.Context, path string, arguments []string, pcm []byte, maximum int) ([]byte, error) {
		gotPath = path
		gotArguments = append([]string(nil), arguments...)
		gotPCM = append([]byte(nil), pcm...)
		if maximum != maximumFingerprintBytes {
			t.Fatalf("声纹输出上限错误: %d", maximum)
		}
		return []byte{4, 3, 2, 1}, nil
	}
	extractor, err := newCommandFingerprintExtractor(CommandFingerprintOptions{
		Path: "/data/casescreen/fingerprint-extractor", Algorithm: "vendor-acr", Version: "1",
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	pcm := []byte{1, 2, 3, 4}
	fingerprint, err := extractor.Extract(context.Background(), Capture{
		PCM: pcm, Duration: 10 * time.Second, SampledAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantArguments := []string{
		"--format", "pcm_s16le", "--sample-rate", "16000", "--channels", "1", "--duration-ms", "10000",
	}
	if gotPath != "/data/casescreen/fingerprint-extractor" || !slices.Equal(gotArguments, wantArguments) ||
		!slices.Equal(gotPCM, pcm) || !slices.Equal(fingerprint.Data, []byte{4, 3, 2, 1}) {
		t.Fatalf("声纹命令契约错误: path=%q args=%#v pcm=%#v result=%#v",
			gotPath, gotArguments, gotPCM, fingerprint)
	}
}

func TestCommandFingerprintExtractorRequiresSafeAlgorithmVersion(t *testing.T) {
	for _, options := range []CommandFingerprintOptions{
		{Path: "/data/fp", Algorithm: "", Version: "1"},
		{Path: "/data/fp", Algorithm: "bad label", Version: "1"},
		{Path: "/data/fp", Algorithm: "vendor", Version: strings.Repeat("v", 33)},
		{Path: "", Algorithm: "vendor", Version: "1"},
	} {
		if _, err := NewCommandFingerprintExtractor(options); err == nil {
			t.Fatalf("无效声纹提取配置应被拒绝: %#v", options)
		}
	}
}
