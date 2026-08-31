//go:build windows

package lyrics

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/windows"
)

const (
	neteasePlayingListMaximumBytes     = 16 << 20
	neteasePlayingListMaximumItems     = 10_000
	neteasePlayingListMaximumDepth     = 64
	neteasePlayingListMaximumJSONText  = 64 << 10
	neteasePlayingListMaximumTitle     = 512
	neteasePlayingListMaximumArtist    = 256
	neteasePlayingListMaximumArtists   = 32
	neteasePlayingListMaximumAlbum     = 512
	neteasePlayingListMaximumIdentity  = 64
	neteasePlayingListReadBuffer       = 64 << 10
	neteaseEvidenceSecondReadTolerance = 2 * time.Second
)

type neteasePlayingListMetadata struct {
	TrackID  string
	Title    string
	Artist   string
	Album    string
	Duration time.Duration
}

type neteasePlayingListCache struct {
	metadata neteasePlayingListMetadata
	info     os.FileInfo
}

type neteasePlayingListResolver struct {
	mu    sync.Mutex
	cache neteasePlayingListCache

	// pathForTest 只供单测把固定 Known Folder 路径替换成临时文件。
	// 生产实例始终调用 SHGetKnownFolderPath，不读取环境变量或外部配置。
	pathForTest func() (string, error)
}

type neteasePlayingListItem struct {
	ID    string `json:"id"`
	Track struct {
		ID       string      `json:"id"`
		Name     string      `json:"name"`
		Duration json.Number `json:"duration"`
		Artists  []struct {
			Name string `json:"name"`
		} `json:"artists"`
		Album struct {
			Name string `json:"name"`
		} `json:"album"`
	} `json:"track"`
}

type neteaseNowPlayingSource struct {
	mu       sync.Mutex
	memory   *neteaseProcessMemorySource
	metadata *neteasePlayingListResolver
	smtc     NowPlayingSource
}

type windowsNowPlayingSource struct {
	netease  *neteaseNowPlayingSource
	fallback *fallbackNowPlayingSource
}

func newNeteaseNowPlayingSource(memory *neteaseProcessMemorySource) *neteaseNowPlayingSource {
	if memory == nil {
		memory = newNeteaseProcessMemorySource()
	}
	return &neteaseNowPlayingSource{
		memory:   memory,
		metadata: &neteasePlayingListResolver{},
		smtc:     &smtcSource{preferredSource: "netease"},
	}
}

func (source *windowsNowPlayingSource) Current(ctx context.Context) (NowPlaying, error) {
	if source == nil || source.netease == nil || source.fallback == nil {
		return NowPlaying{}, errors.New("Windows 当前曲目源未初始化")
	}
	neteaseTrack, neteaseErr := source.netease.Current(ctx)
	if neteaseErr == nil && neteaseTrack.Valid() && neteaseTrack.Playing {
		return neteaseTrack, nil
	}
	fallbackTrack, fallbackErr := source.fallback.Current(ctx)
	if neteaseErr == nil && neteaseTrack.Valid() {
		// 网易云暂停时允许另一个明确正在播放的媒体会话接管；
		// 否则保留网易云原生暂停锚点，使歌词同步停止前进。
		if fallbackErr == nil && fallbackTrack.Valid() && fallbackTrack.Playing &&
			sourceProviderID(fallbackTrack.Source) != "netease" {
			return fallbackTrack, nil
		}
		return neteaseTrack, nil
	}
	if fallbackErr != nil {
		if neteaseErr != nil {
			return NowPlaying{}, errors.Join(neteaseErr, fallbackErr)
		}
		return NowPlaying{}, fallbackErr
	}
	if fallbackTrack.Valid() {
		// 原生网易云核验报错时，不能退回只有窗口标题的网易云结果。
		if neteaseErr != nil && sourceProviderID(fallbackTrack.Source) == "netease" {
			return NowPlaying{}, neteaseErr
		}
		return fallbackTrack, nil
	}
	if neteaseErr != nil {
		return NowPlaying{}, neteaseErr
	}
	return fallbackTrack, nil
}

func (source *windowsNowPlayingSource) Close() error {
	if source == nil {
		return nil
	}
	var failures []error
	if source.netease != nil {
		failures = append(failures, source.netease.Close())
	}
	if source.fallback != nil {
		failures = append(failures, source.fallback.Close())
	}
	return errors.Join(failures...)
}

// Current 以网易云进程内的原生 ID、播放状态和时间轴为事实源，
// playingList 只在相同 ID 下补齐标题、全部歌手和专辑。取得元数据后
// 再读一轮进程；切歌、暂停或进度不一致时整轮失败关闭。
func (source *neteaseNowPlayingSource) Current(ctx context.Context) (NowPlaying, error) {
	if source == nil || source.memory == nil || source.metadata == nil || ctx == nil {
		return NowPlaying{}, errors.New("网易云只读当前曲目源未初始化")
	}
	if err := ctx.Err(); err != nil {
		return NowPlaying{}, err
	}
	source.mu.Lock()
	defer source.mu.Unlock()

	first, err := source.memory.currentEvidence(ctx)
	if errors.Is(err, errNeteaseNoPlaybackEvidence) {
		source.metadata.clear()
		return NowPlaying{}, nil
	}
	if err != nil {
		return NowPlaying{}, err
	}
	metadata, err := source.metadata.resolve(ctx, first.TrackID)
	if err != nil {
		return NowPlaying{}, fmt.Errorf("读取网易云当前曲目元数据: %w", err)
	}
	if !neteasePlayingListMetadataMatchesEvidence(metadata, first) {
		return NowPlaying{}, errors.New("网易云 playingList 时长与原生播放结构不一致")
	}
	second, err := source.memory.currentEvidence(ctx)
	if err != nil {
		return NowPlaying{}, fmt.Errorf("网易云元数据读取后重验播放结构: %w", err)
	}
	if !neteaseMemoryEvidenceSamePlayback(first, second) ||
		!neteasePlayingListMetadataMatchesEvidence(metadata, second) {
		return NowPlaying{}, errors.New("网易云在元数据读取期间发生切歌或时间轴变化异常")
	}

	track := NowPlaying{
		Source:            "netease",
		TrackID:           second.TrackID,
		TrackIDOrigin:     TrackIDOriginSource,
		Title:             metadata.Title,
		Artist:            metadata.Artist,
		Album:             metadata.Album,
		Position:          int64(second.Position),
		Duration:          int64(second.Duration),
		PositionKnown:     true,
		Playing:           second.playing(),
		PositionUpdatedAt: second.SampledAt,
	}
	if source.smtc == nil {
		return track, nil
	}
	smtcTrack, err := source.smtc.Current(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return NowPlaying{}, ctxErr
		}
		// SMTC 不是网易云 3.x 的身份来源；部分版本没有注册会话，
		// COM 查询失败也不能抹掉已经完整闭环的原生内存+playingList 证据。
		return track, nil
	}
	if sourceProviderID(smtcTrack.Source) != "netease" || !neteaseSMTCEvidenceComplete(smtcTrack) {
		// 网易云 3.x 在部分机器上没有注册 SMTC；这不是身份降级，
		// 因为原生 ID、状态、进度和时长均来自同一个只读进程结构。
		return track, nil
	}
	if normalizeText(smtcTrack.Title) != normalizeText(metadata.Title) ||
		normalizeText(smtcTrack.Artist) != normalizeText(metadata.Artist) ||
		normalizeText(smtcTrack.Album) != normalizeText(metadata.Album) ||
		!neteaseMemoryEvidenceMatchesCurrent(second, smtcTrack, second.SampledAt) {
		return NowPlaying{}, errors.New("网易云 SMTC 与原生 ID 元数据或时间轴冲突")
	}
	return track, nil
}

func (source *neteaseNowPlayingSource) Close() error {
	if source == nil {
		return nil
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.memory != nil {
		source.memory.closeAll()
	}
	if source.metadata != nil {
		source.metadata.clear()
	}
	return nil
}

func neteasePlayingListPath() (string, error) {
	localAppData, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", fmt.Errorf("读取 LocalAppData Known Folder: %w", err)
	}
	if strings.TrimSpace(localAppData) == "" {
		return "", errors.New("LocalAppData Known Folder 为空")
	}
	return filepath.Join(localAppData, "NetEase", "CloudMusic", "WebData", "file", "playingList"), nil
}

func (resolver *neteasePlayingListResolver) resolve(ctx context.Context,
	trackID string) (neteasePlayingListMetadata, error) {
	if resolver == nil || ctx == nil || !validPlatformTrackID("netease", trackID) {
		return neteasePlayingListMetadata{}, errors.New("网易云 playingList 查询参数无效")
	}
	if err := ctx.Err(); err != nil {
		return neteasePlayingListMetadata{}, err
	}
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if resolver.cache.metadata.TrackID != trackID {
		resolver.cache = neteasePlayingListCache{}
	}
	pathFunction := resolver.pathForTest
	if pathFunction == nil {
		pathFunction = neteasePlayingListPath
	}
	path, err := pathFunction()
	if err != nil {
		return neteasePlayingListMetadata{}, err
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		return neteasePlayingListMetadata{}, err
	}
	if err := validateNeteasePlayingListFile(pathInfo); err != nil {
		return neteasePlayingListMetadata{}, err
	}
	if resolver.cache.metadata.TrackID == trackID && resolver.cache.info != nil &&
		os.SameFile(resolver.cache.info, pathInfo) && sameNeteasePlayingListFingerprint(resolver.cache.info, pathInfo) {
		return resolver.cache.metadata, nil
	}

	data, stableInfo, err := readStableNeteasePlayingList(ctx, path, pathInfo)
	if err != nil {
		return neteasePlayingListMetadata{}, err
	}
	metadata, err := parseNeteasePlayingList(ctx, data, trackID)
	if err != nil {
		return neteasePlayingListMetadata{}, err
	}
	resolver.cache = neteasePlayingListCache{metadata: metadata, info: stableInfo}
	return metadata, nil
}

func (resolver *neteasePlayingListResolver) clear() {
	if resolver == nil {
		return
	}
	resolver.mu.Lock()
	resolver.cache = neteasePlayingListCache{}
	resolver.mu.Unlock()
}

func validateNeteasePlayingListFile(info os.FileInfo) error {
	if info == nil || !info.Mode().IsRegular() {
		return errors.New("网易云 playingList 不是普通文件")
	}
	if info.Size() <= 0 || info.Size() > neteasePlayingListMaximumBytes {
		return fmt.Errorf("网易云 playingList 大小 %d 超过范围", info.Size())
	}
	return nil
}

func sameNeteasePlayingListFingerprint(left, right os.FileInfo) bool {
	return left != nil && right != nil && left.Size() == right.Size() &&
		left.ModTime().Equal(right.ModTime())
}

func readStableNeteasePlayingList(ctx context.Context, path string,
	pathInfo os.FileInfo) ([]byte, os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if err := validateNeteasePlayingListFile(before); err != nil {
		return nil, nil, err
	}
	if pathInfo == nil || !os.SameFile(pathInfo, before) ||
		!sameNeteasePlayingListFingerprint(pathInfo, before) {
		return nil, nil, errors.New("网易云 playingList 在打开前已被替换")
	}
	limited := io.LimitReader(bufio.NewReaderSize(file, neteasePlayingListReadBuffer),
		neteasePlayingListMaximumBytes+1)
	data := make([]byte, 0, int(before.Size()))
	buffer := make([]byte, neteasePlayingListReadBuffer)
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		count, readErr := limited.Read(buffer)
		if count > 0 {
			data = append(data, buffer[:count]...)
			if len(data) > neteasePlayingListMaximumBytes {
				return nil, nil, errors.New("网易云 playingList 读取超过上限")
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, nil, readErr
		}
	}
	after, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	pathAfter, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(before, after) || !os.SameFile(after, pathAfter) ||
		!sameNeteasePlayingListFingerprint(before, after) ||
		!sameNeteasePlayingListFingerprint(after, pathAfter) || int64(len(data)) != after.Size() {
		return nil, nil, errors.New("网易云 playingList 在读取期间发生变化")
	}
	return data, after, nil
}

func parseNeteasePlayingList(ctx context.Context, data []byte,
	trackID string) (neteasePlayingListMetadata, error) {
	if ctx == nil || ctx.Err() != nil || len(data) == 0 ||
		len(data) > neteasePlayingListMaximumBytes || !utf8.Valid(data) ||
		!neteaseJSONWithinBounds(data) {
		return neteasePlayingListMetadata{}, errors.New("网易云 playingList JSON 无效或超过结构上限")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return neteasePlayingListMetadata{}, errors.New("网易云 playingList 根对象无效")
	}
	seenList := false
	matched := 0
	var selected neteasePlayingListMetadata
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return neteasePlayingListMetadata{}, err
		}
		keyToken, err := decoder.Token()
		if err != nil {
			return neteasePlayingListMetadata{}, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return neteasePlayingListMetadata{}, errors.New("网易云 playingList 根键无效")
		}
		if key != "list" {
			if err := skipNeteaseJSONValue(decoder, 0); err != nil {
				return neteasePlayingListMetadata{}, err
			}
			continue
		}
		if seenList {
			return neteasePlayingListMetadata{}, errors.New("网易云 playingList 包含重复 list 字段")
		}
		seenList = true
		listStart, err := decoder.Token()
		if err != nil || listStart != json.Delim('[') {
			return neteasePlayingListMetadata{}, errors.New("网易云 playingList.list 不是数组")
		}
		items := 0
		for decoder.More() {
			if err := ctx.Err(); err != nil {
				return neteasePlayingListMetadata{}, err
			}
			items++
			if items > neteasePlayingListMaximumItems {
				return neteasePlayingListMetadata{}, errors.New("网易云 playingList 项目数量超过上限")
			}
			var item *neteasePlayingListItem
			if err := decoder.Decode(&item); err != nil {
				return neteasePlayingListMetadata{}, err
			}
			if item == nil {
				return neteasePlayingListMetadata{}, errors.New("网易云 playingList 项目不是对象")
			}
			if err := validateNeteasePlayingListItemBounds(*item); err != nil {
				return neteasePlayingListMetadata{}, err
			}
			if item.ID != trackID {
				continue
			}
			metadata, err := metadataFromNeteasePlayingListItem(*item, trackID)
			if err != nil {
				return neteasePlayingListMetadata{}, err
			}
			matched++
			if matched == 1 {
				selected = metadata
			} else if !sameNeteasePlayingListMetadata(selected, metadata) {
				return neteasePlayingListMetadata{}, errors.New("网易云 playingList 同一 ID 存在冲突元数据")
			}
		}
		if listEnd, err := decoder.Token(); err != nil || listEnd != json.Delim(']') {
			return neteasePlayingListMetadata{}, errors.New("网易云 playingList.list 结尾无效")
		}
	}
	if rootEnd, err := decoder.Token(); err != nil || rootEnd != json.Delim('}') {
		return neteasePlayingListMetadata{}, errors.New("网易云 playingList 根对象结尾无效")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return neteasePlayingListMetadata{}, errors.New("网易云 playingList 根对象后有多余内容")
	}
	if !seenList || matched == 0 {
		return neteasePlayingListMetadata{}, fmt.Errorf("网易云 playingList 没有原生 ID %s", trackID)
	}
	return selected, nil
}

func validateNeteasePlayingListItemBounds(item neteasePlayingListItem) error {
	if len(item.ID) > neteasePlayingListMaximumIdentity ||
		len(item.Track.ID) > neteasePlayingListMaximumIdentity ||
		!boundedNeteaseText(item.Track.Name, neteasePlayingListMaximumTitle, true) ||
		!boundedNeteaseText(item.Track.Album.Name, neteasePlayingListMaximumAlbum, true) ||
		len(item.Track.Artists) > neteasePlayingListMaximumArtists {
		return errors.New("网易云 playingList 项目字段超过上限")
	}
	for _, artist := range item.Track.Artists {
		if !boundedNeteaseText(artist.Name, neteasePlayingListMaximumArtist, true) {
			return errors.New("网易云 playingList 歌手字段超过上限")
		}
	}
	return nil
}

func metadataFromNeteasePlayingListItem(item neteasePlayingListItem,
	trackID string) (neteasePlayingListMetadata, error) {
	if item.Track.ID != trackID || len(item.Track.Artists) == 0 {
		return neteasePlayingListMetadata{}, errors.New("网易云 playingList 当前项身份或歌手为空")
	}
	title := strings.TrimSpace(item.Track.Name)
	album := strings.TrimSpace(item.Track.Album.Name)
	if title == "" || album == "" {
		return neteasePlayingListMetadata{}, errors.New("网易云 playingList 当前项标题或专辑为空")
	}
	artists := make([]string, 0, len(item.Track.Artists))
	for _, itemArtist := range item.Track.Artists {
		artist := strings.TrimSpace(itemArtist.Name)
		if artist == "" {
			return neteasePlayingListMetadata{}, errors.New("网易云 playingList 当前项包含空歌手")
		}
		artists = append(artists, artist)
	}
	durationMilliseconds, err := strconv.ParseInt(item.Track.Duration.String(), 10, 64)
	if err != nil || durationMilliseconds <= 0 ||
		durationMilliseconds > int64(neteaseMemoryMaximumDuration/time.Millisecond) {
		return neteasePlayingListMetadata{}, errors.New("网易云 playingList 当前项时长无效")
	}
	return neteasePlayingListMetadata{
		TrackID:  trackID,
		Title:    title,
		Artist:   strings.Join(artists, " / "),
		Album:    album,
		Duration: time.Duration(durationMilliseconds) * time.Millisecond,
	}, nil
}

func boundedNeteaseText(value string, maximum int, allowEmpty bool) bool {
	if !utf8.ValidString(value) || len(value) > maximum*4 || utf8.RuneCountInString(value) > maximum {
		return false
	}
	return allowEmpty || strings.TrimSpace(value) != ""
}

func sameNeteasePlayingListMetadata(left, right neteasePlayingListMetadata) bool {
	return left.TrackID == right.TrackID && left.Duration == right.Duration &&
		normalizeText(left.Title) == normalizeText(right.Title) &&
		normalizeText(left.Artist) == normalizeText(right.Artist) &&
		normalizeText(left.Album) == normalizeText(right.Album)
}

func neteasePlayingListMetadataMatchesEvidence(metadata neteasePlayingListMetadata,
	evidence neteaseMemoryEvidence) bool {
	if metadata.TrackID != evidence.TrackID || !validPlatformTrackID("netease", metadata.TrackID) ||
		strings.TrimSpace(metadata.Title) == "" || strings.TrimSpace(metadata.Artist) == "" ||
		strings.TrimSpace(metadata.Album) == "" || metadata.Duration <= 0 {
		return false
	}
	difference := metadata.Duration - evidence.Duration
	if difference < 0 {
		difference = -difference
	}
	return difference <= neteaseMemoryDurationSlack
}

func neteaseMemoryEvidenceSamePlayback(first, second neteaseMemoryEvidence) bool {
	if first.TrackID != second.TrackID || first.Status != second.Status ||
		first.Duration != second.Duration || first.SampledAt.IsZero() || second.SampledAt.IsZero() ||
		second.SampledAt.Before(first.SampledAt) ||
		(first.ProcessID != 0 && second.ProcessID != 0 && first.ProcessID != second.ProcessID) {
		return false
	}
	expectedPosition := first.Position
	if first.playing() {
		expectedPosition += second.SampledAt.Sub(first.SampledAt)
	}
	difference := second.Position - expectedPosition
	if difference < 0 {
		difference = -difference
	}
	return difference <= neteaseEvidenceSecondReadTolerance
}

func neteaseJSONWithinBounds(data []byte) bool {
	depth := 0
	inString := false
	escaped := false
	stringBytes := 0
	for _, value := range data {
		if inString {
			if escaped {
				stringBytes++
				if stringBytes > neteasePlayingListMaximumJSONText {
					return false
				}
				escaped = false
				continue
			}
			if value == '\\' {
				stringBytes++
				if stringBytes > neteasePlayingListMaximumJSONText {
					return false
				}
				escaped = true
				continue
			}
			if value == '"' {
				inString = false
				continue
			}
			stringBytes++
			if stringBytes > neteasePlayingListMaximumJSONText {
				return false
			}
			continue
		}
		switch value {
		case '"':
			inString = true
			stringBytes = 0
		case '{', '[':
			depth++
			if depth > neteasePlayingListMaximumDepth {
				return false
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return !inString && !escaped && depth == 0 && json.Valid(data)
}

func skipNeteaseJSONValue(decoder *json.Decoder, depth int) error {
	if decoder == nil || depth > neteasePlayingListMaximumDepth {
		return errors.New("网易云 playingList JSON 深度超过上限")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		for decoder.More() {
			if _, err := decoder.Token(); err != nil {
				return err
			}
			if err := skipNeteaseJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := skipNeteaseJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("网易云 playingList JSON 分隔符无效")
	}
}
