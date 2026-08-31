package lyrics

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maximumNeteaseDBTrackJSONBytes = 512 << 10
	maximumNeteaseDBTextBytes      = 8 << 10
	neteasePlaybackEvidenceMaxAge  = 30 * time.Second
	neteasePlaybackClockSkew       = 3 * time.Second
	neteasePlaybackPositionSlack   = 5 * time.Second
	neteaseTrackDurationSlack      = 2 * time.Second
)

type neteaseWebDBPlaybackEvidence struct {
	ResourceID   string
	ResourceType string
	PlayDuration int64
	UpdateTime   int64
	TrackJSON    []byte
}

type neteaseWebDBTrack struct {
	ID       string
	Title    string
	Artists  []string
	Album    string
	Duration time.Duration
}

// resolveNeteaseWebDBTrackID 只在本地播放计数、dbTrack 和 SMTC 四项媒体
// 信息形成同一条新鲜证据链时返回 ID。任何缺项或近似歌曲都会失败关闭。
func resolveNeteaseWebDBTrackID(evidence neteaseWebDBPlaybackEvidence, current NowPlaying, now time.Time) string {
	if sourceProviderID(current.Source) != "netease" ||
		!strings.EqualFold(strings.TrimSpace(evidence.ResourceType), "track") || now.IsZero() {
		return ""
	}
	resourceID := strings.TrimSpace(evidence.ResourceID)
	if _, valid := parseNeteaseSongID(resourceID); !valid {
		return ""
	}
	updatedAt, valid := parseNeteaseWebDBUpdateTime(evidence.UpdateTime)
	if !valid {
		return ""
	}
	age := now.Sub(updatedAt)
	if age < -neteasePlaybackClockSkew || age > neteasePlaybackEvidenceMaxAge {
		return ""
	}
	if age < 0 {
		age = 0
	}
	metadata, valid := parseNeteaseWebDBTrack(evidence.TrackJSON)
	if !valid || metadata.ID != resourceID {
		return ""
	}
	if !neteaseWebDBMetadataMatches(metadata, current) {
		return ""
	}
	if !neteaseWebDBPositionMatches(evidence.PlayDuration, current, age) {
		return ""
	}
	return resourceID
}

func parseNeteaseWebDBUpdateTime(value int64) (time.Time, bool) {
	switch {
	case value >= 1_000_000_000 && value < 100_000_000_000:
		return time.Unix(value, 0), true
	case value >= 1_000_000_000_000 && value < 100_000_000_000_000:
		return time.UnixMilli(value), true
	default:
		return time.Time{}, false
	}
}

func neteaseWebDBMetadataMatches(candidate neteaseWebDBTrack, current NowPlaying) bool {
	if normalizeText(candidate.Title) == "" || normalizeText(candidate.Title) != normalizeText(current.Title) {
		return false
	}
	if len(candidate.Artists) == 0 || normalizeText(current.Artist) == "" ||
		normalizeText(strings.Join(candidate.Artists, "/")) != normalizeText(current.Artist) {
		return false
	}
	if normalizeText(candidate.Album) == "" || normalizeText(current.Album) == "" ||
		normalizeText(candidate.Album) != normalizeText(current.Album) {
		return false
	}
	if candidate.Duration <= 0 || current.Duration <= 0 {
		return false
	}
	difference := candidate.Duration - time.Duration(current.Duration)
	if difference < 0 {
		difference = -difference
	}
	return difference <= neteaseTrackDurationSlack
}

func neteaseWebDBPositionMatches(rawDuration int64, current NowPlaying, evidenceAge time.Duration) bool {
	if rawDuration < 0 || !current.PositionKnown || current.Position < 0 || current.Duration <= 0 {
		return false
	}
	position := time.Duration(current.Position)
	duration := time.Duration(current.Duration)
	if position > duration+neteasePlaybackPositionSlack {
		return false
	}
	candidates := make([]time.Duration, 0, 2)
	if rawDuration <= int64((duration+neteasePlaybackPositionSlack)/time.Second)+1 {
		candidates = append(candidates, time.Duration(rawDuration)*time.Second)
	}
	if rawDuration <= int64((duration+neteasePlaybackPositionSlack)/time.Millisecond)+1 {
		candidates = append(candidates, time.Duration(rawDuration)*time.Millisecond)
	}
	for _, stored := range candidates {
		for _, expected := range []time.Duration{stored, stored + playbackProjection(current.Playing, evidenceAge)} {
			difference := expected - position
			if difference < 0 {
				difference = -difference
			}
			if difference <= neteasePlaybackPositionSlack {
				return true
			}
		}
	}
	return false
}

func playbackProjection(playing bool, age time.Duration) time.Duration {
	if !playing {
		return 0
	}
	return age
}

type neteaseWebDBTrackJSON struct {
	ID       json.RawMessage        `json:"id"`
	Name     string                 `json:"name"`
	Duration json.RawMessage        `json:"duration"`
	DT       json.RawMessage        `json:"dt"`
	Artists  []neteaseWebDBArtist   `json:"artists"`
	AR       []neteaseWebDBArtist   `json:"ar"`
	Album    neteaseWebDBAlbum      `json:"album"`
	AL       neteaseWebDBAlbum      `json:"al"`
	Track    *neteaseWebDBTrackJSON `json:"track"`
	Song     *neteaseWebDBTrackJSON `json:"song"`
}

type neteaseWebDBArtist struct {
	Name string `json:"name"`
}

type neteaseWebDBAlbum struct {
	Name string `json:"name"`
}

func parseNeteaseWebDBTrack(data []byte) (neteaseWebDBTrack, bool) {
	if len(data) == 0 || len(data) > maximumNeteaseDBTrackJSONBytes || !utf8.Valid(data) {
		return neteaseWebDBTrack{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root neteaseWebDBTrackJSON
	if err := decoder.Decode(&root); err != nil {
		return neteaseWebDBTrack{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return neteaseWebDBTrack{}, false
	}
	candidates := []*neteaseWebDBTrackJSON{&root, root.Track, root.Song}
	for _, candidate := range candidates {
		if parsed, ok := convertNeteaseWebDBTrack(candidate); ok {
			return parsed, true
		}
	}
	return neteaseWebDBTrack{}, false
}

func convertNeteaseWebDBTrack(value *neteaseWebDBTrackJSON) (neteaseWebDBTrack, bool) {
	if value == nil || !boundedNeteaseDBText(value.Name) {
		return neteaseWebDBTrack{}, false
	}
	id, valid := parseNeteaseJSONPositiveInteger(value.ID)
	if !valid {
		return neteaseWebDBTrack{}, false
	}
	durationMS, valid := parseNeteaseJSONPositiveInteger(value.Duration)
	if !valid {
		durationMS, valid = parseNeteaseJSONPositiveInteger(value.DT)
	}
	if !valid || durationMS > int64((48*time.Hour)/time.Millisecond) {
		return neteaseWebDBTrack{}, false
	}
	artists := value.Artists
	if len(artists) == 0 {
		artists = value.AR
	}
	if len(artists) == 0 || len(artists) > 64 {
		return neteaseWebDBTrack{}, false
	}
	names := make([]string, 0, len(artists))
	for _, artist := range artists {
		if !boundedNeteaseDBText(artist.Name) {
			return neteaseWebDBTrack{}, false
		}
		names = append(names, strings.TrimSpace(artist.Name))
	}
	album := value.Album.Name
	if strings.TrimSpace(album) == "" {
		album = value.AL.Name
	}
	if !boundedNeteaseDBText(album) {
		return neteaseWebDBTrack{}, false
	}
	return neteaseWebDBTrack{
		ID: strconv.FormatInt(id, 10), Title: strings.TrimSpace(value.Name),
		Artists: names, Album: strings.TrimSpace(album), Duration: time.Duration(durationMS) * time.Millisecond,
	}, true
}

func parseNeteaseJSONPositiveInteger(raw json.RawMessage) (int64, bool) {
	value := strings.TrimSpace(string(raw))
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		unquoted, err := strconv.Unquote(value)
		if err != nil {
			return 0, false
		}
		value = unquoted
	}
	if value == "" || len(value) > 20 || strings.HasPrefix(value, "+") || (len(value) > 1 && value[0] == '0') {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	return parsed, err == nil && parsed > 0
}

func boundedNeteaseDBText(value string) bool {
	trimmed := strings.TrimSpace(value)
	return trimmed != "" && len(trimmed) <= maximumNeteaseDBTextBytes
}
