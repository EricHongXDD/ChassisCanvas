package main

import (
	"path/filepath"
	"testing"
)

func TestRequestedTargetsCanBuildBothWaveshareSizes(t *testing.T) {
	directory := t.TempDir()
	targets, err := requestedTargets("", directory, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0].width != 1024 || targets[0].height != 600 || targets[1].width != 480 || targets[1].height != 1920 {
		t.Fatalf("批量目标错误: %+v", targets)
	}
	if targets[0].path != filepath.Join(directory, "casescreen-lyrics-1024x600.Theme") {
		t.Fatalf("横屏输出路径错误: %s", targets[0].path)
	}
}

func TestRequestedTargetsRejectsAmbiguousOutput(t *testing.T) {
	if _, err := requestedTargets("one.Theme", "themes", 1024, 600); err == nil {
		t.Fatal("同时指定两种输出方式未被拒绝")
	}
}
