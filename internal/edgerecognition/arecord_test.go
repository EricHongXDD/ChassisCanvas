package edgerecognition

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestArecordUsesStdoutAndFixedPCMFormat(t *testing.T) {
	var gotPath string
	var gotArguments []string
	var gotMaximum int
	now := time.Unix(1_700_000_000, 0)
	runner := func(_ context.Context, path string, arguments []string, maximum int) ([]byte, error) {
		gotPath = path
		gotArguments = append([]string(nil), arguments...)
		gotMaximum = maximum
		return make([]byte, maximum), nil
	}
	recorder, err := newArecordRecorder(ArecordOptions{Path: "/bin/arecord", Device: "hw:2,0"}, runner, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	capture, err := recorder.Capture(context.Background(), 4*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	wantArguments := []string{"-q", "-D", "hw:2,0", "-t", "raw", "-f", "S16_LE", "-r", "16000", "-c", "1", "-d", "4", "-"}
	if gotPath != "/bin/arecord" || !slices.Equal(gotArguments, wantArguments) {
		t.Fatalf("arecord 参数错误: path=%q args=%#v", gotPath, gotArguments)
	}
	if gotMaximum != 128_000 || len(capture.PCM) != gotMaximum || capture.SampledAt != now {
		t.Fatalf("采样格式或时间错误: max=%d capture=%#v", gotMaximum, capture)
	}
	for _, argument := range gotArguments {
		if argument != "-" && (argument == "/tmp" || argument == "/data") {
			t.Fatalf("录音参数不应包含持久文件路径: %#v", gotArguments)
		}
	}
}

func TestArecordRejectsPartialOrUnboundedAudio(t *testing.T) {
	runner := func(_ context.Context, _ string, _ []string, maximum int) ([]byte, error) {
		return make([]byte, maximum/2), nil
	}
	recorder, err := newArecordRecorder(ArecordOptions{}, runner, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Capture(context.Background(), time.Second); err == nil {
		t.Fatal("过短的 PCM 应被拒绝")
	}
}
