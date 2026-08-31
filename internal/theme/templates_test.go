package theme

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBundledTemplatesAreValid(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "themes", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 4 {
		t.Fatalf("内置主题模板数量不足: %d", len(paths))
	}
	for _, path := range paths {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var manifest Manifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			if err := manifest.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
