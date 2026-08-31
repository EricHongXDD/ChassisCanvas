package lyrics

import (
	"strings"
	"testing"
	"time"
)

func TestParseAndSelectLRC(t *testing.T) {
	document, err := ParseLRC(strings.NewReader("\ufeff[ti:测试歌曲]\n[ar:测试歌手]\n[offset:100]\n[00:01.00]第一句\n[00:02.500][00:03.50]第二句\n"))
	if err != nil {
		t.Fatal(err)
	}
	if document.Title != "测试歌曲" || len(document.Lines) != 3 {
		t.Fatalf("解析结果异常: %#v", document)
	}
	current, next, progress, ok := document.Current(2100 * time.Millisecond)
	if !ok || current.Text != "第一句" || next.Text != "第二句" {
		t.Fatalf("当前歌词选择异常: %#v %#v", current, next)
	}
	if progress < 0.64 || progress > 0.69 {
		t.Fatalf("歌词进度异常: %.3f", progress)
	}
}
