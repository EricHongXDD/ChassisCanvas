//go:build windows

package lyrics

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type neteaseTestNowPlayingSource func(context.Context) (NowPlaying, error)

func (source neteaseTestNowPlayingSource) Current(ctx context.Context) (NowPlaying, error) {
	return source(ctx)
}

func neteaseTestPlayingListItem(id, title string, artists []string,
	album string, durationMilliseconds int64) map[string]any {
	artistValues := make([]map[string]any, 0, len(artists))
	for _, artist := range artists {
		artistValues = append(artistValues, map[string]any{"name": artist})
	}
	return map[string]any{
		"id": id,
		"track": map[string]any{
			"id": id, "name": title, "duration": durationMilliseconds,
			"artists": artistValues, "album": map[string]any{"name": album},
		},
	}
}

func marshalNeteaseTestPlayingList(t *testing.T, items ...any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"list": items})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseNeteasePlayingListRequiresExactNativeID(t *testing.T) {
	item := neteaseTestPlayingListItem("757567", "1967", []string{"ゴンチチ"},
		"humble music", 144_172)
	metadata, err := parseNeteasePlayingList(context.Background(),
		marshalNeteaseTestPlayingList(t, item), "757567")
	if err != nil || metadata.TrackID != "757567" || metadata.Title != "1967" ||
		metadata.Artist != "ゴンチチ" || metadata.Album != "humble music" ||
		metadata.Duration != 144_172*time.Millisecond {
		t.Fatalf("精确 ID 元数据解析失败: metadata=%+v err=%v", metadata, err)
	}

	consistent := neteaseTestPlayingListItem("757567", " 1967 ", []string{" ゴンチチ "},
		" humble music ", 144_172)
	if _, err := parseNeteasePlayingList(context.Background(),
		marshalNeteaseTestPlayingList(t, item, consistent), "757567"); err != nil {
		t.Fatalf("完全一致的重复队列项应安全合并: %v", err)
	}
	conflict := neteaseTestPlayingListItem("757567", "同 ID 冲突标题", []string{"ゴンチチ"},
		"humble music", 144_172)
	if _, err := parseNeteasePlayingList(context.Background(),
		marshalNeteaseTestPlayingList(t, item, conflict), "757567"); err == nil {
		t.Fatal("同一 ID 的冲突元数据没有失败关闭")
	}
	padded := neteaseTestPlayingListItem(" 757567 ", "1967", []string{"ゴンチチ"},
		"humble music", 144_172)
	if _, err := parseNeteasePlayingList(context.Background(),
		marshalNeteaseTestPlayingList(t, padded), "757567"); err == nil {
		t.Fatal("带空格的非精确 ID 被接受")
	}
}

func TestParseNeteasePlayingListEnforcesBounds(t *testing.T) {
	valid := neteaseTestPlayingListItem("757567", "1967", []string{"ゴンチチ"},
		"humble music", 144_172)
	tests := []struct {
		name string
		data func() []byte
	}{
		{name: "非法 UTF-8", data: func() []byte { return []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'} }},
		{name: "过深", data: func() []byte {
			return []byte(`{"list":[],"x":` + strings.Repeat("[", neteasePlayingListMaximumDepth+1) +
				strings.Repeat("]", neteasePlayingListMaximumDepth+1) + `}`)
		}},
		{name: "转义字符串过长", data: func() []byte {
			data, _ := json.Marshal(map[string]any{"list": []any{valid},
				"x": strings.Repeat("\\", neteasePlayingListMaximumJSONText+1)})
			return data
		}},
		{name: "空项目", data: func() []byte { return marshalNeteaseTestPlayingList(t, nil) }},
		{name: "歌手过多", data: func() []byte {
			artists := make([]string, neteasePlayingListMaximumArtists+1)
			for index := range artists {
				artists[index] = "歌手"
			}
			return marshalNeteaseTestPlayingList(t,
				neteaseTestPlayingListItem("757567", "1967", artists, "humble music", 144_172))
		}},
		{name: "字段过长", data: func() []byte {
			return marshalNeteaseTestPlayingList(t,
				neteaseTestPlayingListItem("757567", strings.Repeat("曲", neteasePlayingListMaximumTitle+1),
					[]string{"ゴンチチ"}, "humble music", 144_172))
		}},
		{name: "项目过多", data: func() []byte {
			items := make([]any, neteasePlayingListMaximumItems+1)
			for index := range items {
				items[index] = neteaseTestPlayingListItem("999999", "其他", []string{"歌手"}, "专辑", 1_000)
			}
			return marshalNeteaseTestPlayingList(t, items...)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseNeteasePlayingList(context.Background(), test.data(), "757567"); err == nil {
				t.Fatal("越界 playingList 被接受")
			}
		})
	}
}

func TestNeteasePlayingListResolverUsesStableFingerprintAndClearsOnTrackChange(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "playingList")
	write := func(item map[string]any) {
		t.Helper()
		if err := os.WriteFile(path, marshalNeteaseTestPlayingList(t, item), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(neteaseTestPlayingListItem("757567", "1967", []string{"ゴンチチ"},
		"humble music", 144_172))
	resolver := &neteasePlayingListResolver{pathForTest: func() (string, error) { return path, nil }}
	metadata, err := resolver.resolve(context.Background(), "757567")
	if err != nil || metadata.Title != "1967" || resolver.cache.info == nil {
		t.Fatalf("首次稳定快照解析失败: metadata=%+v err=%v", metadata, err)
	}
	write(neteaseTestPlayingListItem("757567", "1967 remaster", []string{"ゴンチチ"},
		"humble music", 144_172))
	metadata, err = resolver.resolve(context.Background(), "757567")
	if err != nil || metadata.Title != "1967 remaster" {
		t.Fatalf("文件指纹变化后沿用了旧缓存: metadata=%+v err=%v", metadata, err)
	}
	if _, err := resolver.resolve(context.Background(), "2080326"); err == nil ||
		resolver.cache.metadata.TrackID != "" {
		t.Fatalf("切换 ID 失败后仍保留旧元数据: cache=%+v err=%v", resolver.cache, err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolver.resolve(canceled, "757567"); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文没有原样返回: %v", err)
	}
}

func TestReadStableNeteasePlayingListRejectsInvalidFiles(t *testing.T) {
	directory := t.TempDir()
	if _, _, err := readStableNeteasePlayingList(context.Background(), directory, nil); err == nil {
		t.Fatal("目录被当作 playingList 普通文件")
	}
	path := filepath.Join(directory, "oversize")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(neteasePlayingListMaximumBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := readStableNeteasePlayingList(context.Background(), path, info); err == nil {
		t.Fatal("超限 playingList 被读取")
	}

	stablePath := filepath.Join(directory, "stable")
	if err := os.WriteFile(stablePath, []byte(`{"list":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	staleInfo, err := os.Stat(stablePath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(stablePath, []byte(`{"list":[null]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readStableNeteasePlayingList(context.Background(), stablePath, staleInfo); err == nil {
		t.Fatal("打开前文件指纹变化没有失败关闭")
	}
}

func TestNeteaseNowPlayingSourceBuildsFullNativeTrackAndRechecksSwitch(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "playingList")
	if err := os.WriteFile(path, marshalNeteaseTestPlayingList(t,
		neteaseTestPlayingListItem("757567", "1967", []string{"ゴンチチ"},
			"humble music", 144_172)), 0o600); err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_900_000_000, 0)
	first := neteaseMemoryEvidence{
		TrackID: "757567", Status: 2, Duration: 144_172 * time.Millisecond,
		Position: 42 * time.Second, SampledAt: base,
	}
	newSource := func(second neteaseMemoryEvidence, smtc NowPlayingSource) *neteaseNowPlayingSource {
		memory := newNeteaseProcessMemorySource()
		probe := &neteaseProcessProbe{}
		memory.processIDs = func() ([]uint32, error) { return []uint32{101}, nil }
		memory.openProbe = func(context.Context, uint32) (*neteaseProcessProbe, error) { return probe, nil }
		calls := 0
		memory.readEvidence = func(context.Context, *neteaseProcessProbe) (neteaseMemoryEvidence, error) {
			calls++
			if calls == 1 {
				return first, nil
			}
			return second, nil
		}
		return &neteaseNowPlayingSource{
			memory: memory,
			metadata: &neteasePlayingListResolver{
				pathForTest: func() (string, error) { return path, nil },
			},
			smtc: smtc,
		}
	}
	second := first
	second.SampledAt = base.Add(100 * time.Millisecond)
	source := newSource(second, neteaseTestNowPlayingSource(func(context.Context) (NowPlaying, error) {
		return NowPlaying{}, errors.New("本机没有网易云 SMTC")
	}))
	track, err := source.Current(context.Background())
	if err != nil || track.SourceTrackID() != "757567" || track.Title != "1967" ||
		track.Artist != "ゴンチチ" || track.Album != "humble music" ||
		!track.PositionKnown || track.Playing || track.PositionUpdatedAt != second.SampledAt {
		t.Fatalf("原生完整 NowPlaying 构造失败: track=%+v err=%v", track, err)
	}

	switched := second
	switched.TrackID = "2080326"
	switched.SampledAt = base.Add(100 * time.Millisecond)
	if _, err := newSource(switched, nil).Current(context.Background()); err == nil {
		t.Fatal("读取 playingList 后发生切歌仍返回了旧曲目")
	}

	conflictingSMTC := NowPlaying{
		Source: "netease", Title: "错误标题", Artist: "ゴンチチ", Album: "humble music",
		Position: int64(second.Position), Duration: int64(second.Duration), PositionKnown: true,
		Playing: false, PositionUpdatedAt: second.SampledAt,
	}
	if _, err := newSource(second, neteaseTestNowPlayingSource(func(context.Context) (NowPlaying, error) {
		return conflictingSMTC, nil
	})).Current(context.Background()); err == nil {
		t.Fatal("有效网易云 SMTC 元数据冲突没有失败关闭")
	}
}
