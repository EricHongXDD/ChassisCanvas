package recognitionrelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"casescreen/internal/edgerecognition"
	"casescreen/internal/platformid"
)

// StaticMappingFile 是可审计的平台 ID 强映射文件。它没有标题或歌手字段，
// 从数据结构上禁止“按标题猜测歌曲 ID”。
type StaticMappingFile struct {
	Version int                   `json:"version"`
	Records []StaticMappingRecord `json:"records"`
}

// StaticMappingRecord 必须提供 stable_id 或 ISRC 证据，并至少映射一个平台。
type StaticMappingRecord struct {
	StableID       string `json:"stable_id,omitempty"`
	ISRC           string `json:"isrc,omitempty"`
	QQTrackID      string `json:"qq_track_id,omitempty"`
	NeteaseTrackID string `json:"netease_track_id,omitempty"`
}

type mappingRecord struct {
	stableID       string
	isrc           string
	qqTrackID      string
	neteaseTrackID string
}

// StaticMapper 同时建立 stable_id 与 ISRC 精确索引；冲突在启动阶段即失败。
type StaticMapper struct {
	byStableID   map[string]mappingRecord
	byISRC       map[string]mappingRecord
	autoPlatform string
}

// LoadStaticMapper 从 JSON 加载强映射。autoPlatform 为空或 none 时，auto 请求
// 不会选择任何平台；设置 qq/netease 才代表管理员的显式来源策略。
func LoadStaticMapper(path, autoPlatform string) (*StaticMapper, error) {
	if strings.TrimSpace(path) == "" {
		return NewStaticMapper(StaticMappingFile{Version: 1}, autoPlatform)
	}
	data, err := readBoundedRegularFile(path, maximumMappingFile, false)
	if err != nil {
		return nil, fmt.Errorf("读取平台 ID 映射文件失败: %w", err)
	}
	defer clear(data)
	var document StaticMappingFile
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("平台 ID 映射文件不是有效 JSON")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, errors.New("平台 ID 映射文件包含多余内容")
	}
	return NewStaticMapper(document, autoPlatform)
}

// NewStaticMapper 验证所有强证据和平台原生 ID。
func NewStaticMapper(document StaticMappingFile, autoPlatform string) (*StaticMapper, error) {
	auto, err := normalizeAutoPlatform(autoPlatform)
	if err != nil {
		return nil, err
	}
	if document.Version != 1 {
		return nil, errors.New("平台 ID 映射文件版本必须是 1")
	}
	if len(document.Records) > 100_000 {
		return nil, errors.New("平台 ID 映射记录超过 100000 条上限")
	}
	mapper := &StaticMapper{
		byStableID: make(map[string]mappingRecord, len(document.Records)),
		byISRC:     make(map[string]mappingRecord, len(document.Records)), autoPlatform: auto,
	}
	for _, raw := range document.Records {
		record := mappingRecord{
			stableID: strings.TrimSpace(raw.StableID), isrc: normalizeISRC(raw.ISRC),
			qqTrackID: strings.TrimSpace(raw.QQTrackID), neteaseTrackID: strings.TrimSpace(raw.NeteaseTrackID),
		}
		if record.stableID == "" && record.isrc == "" {
			return nil, errors.New("平台 ID 映射必须包含 stable_id 或 ISRC")
		}
		if record.stableID != "" && !validStableID(record.stableID) {
			return nil, errors.New("平台 ID 映射的 stable_id 无效")
		}
		if record.isrc != "" && !validISRC(record.isrc) {
			return nil, errors.New("平台 ID 映射的 ISRC 无效")
		}
		if record.qqTrackID == "" && record.neteaseTrackID == "" {
			return nil, errors.New("平台 ID 映射至少需要一个 QQ 或网易云原生 ID")
		}
		if record.qqTrackID != "" && !platformid.QQ(record.qqTrackID) {
			return nil, errors.New("平台 ID 映射中的 QQ songmid 无效")
		}
		if record.neteaseTrackID != "" && !platformid.Netease(record.neteaseTrackID) {
			return nil, errors.New("平台 ID 映射中的网易云歌曲 ID 无效")
		}
		if record.stableID != "" {
			if previous, exists := mapper.byStableID[record.stableID]; exists && previous != record {
				return nil, errors.New("同一 stable_id 存在冲突的平台 ID 映射")
			}
			mapper.byStableID[record.stableID] = record
		}
		if record.isrc != "" {
			if previous, exists := mapper.byISRC[record.isrc]; exists && previous != record {
				return nil, errors.New("同一 ISRC 存在冲突的平台 ID 映射")
			}
			mapper.byISRC[record.isrc] = record
		}
	}
	return mapper, nil
}

// Resolve 只做精确索引匹配。若一条记录同时包含 stable_id 与 ISRC，则二者
// 与识别结果不一致时拒绝映射，绝不降级到标题/歌手匹配。
func (mapper *StaticMapper) Resolve(_ context.Context, result edgerecognition.Result, preferred string) (NativeIdentity, bool, error) {
	if mapper == nil {
		return NativeIdentity{}, false, nil
	}
	stableID := strings.TrimSpace(result.StableID)
	isrc := normalizeISRC(result.ISRC)
	stableRecord, stableFound := mapper.byStableID[stableID]
	isrcRecord, isrcFound := mappingRecord{}, false
	if isrc != "" {
		isrcRecord, isrcFound = mapper.byISRC[isrc]
	}
	// stable_id 与 ISRC 都是强证据。即使映射文件把两种证据拆在两条记录中，
	// 同一次识别若分别命中不同记录也必须失败关闭，不能静默偏向 stable_id。
	if stableFound && isrcFound && stableRecord != isrcRecord {
		return NativeIdentity{}, false, ErrMappingConflict
	}
	record, found := stableRecord, stableFound
	if stableFound {
		if stableRecord.isrc != "" && isrc != "" && stableRecord.isrc != isrc {
			return NativeIdentity{}, false, ErrMappingConflict
		}
	} else if isrcFound {
		record, found = isrcRecord, true
		if isrcRecord.stableID != "" && stableID != "" && isrcRecord.stableID != stableID {
			return NativeIdentity{}, false, ErrMappingConflict
		}
	}
	if !found {
		return NativeIdentity{}, false, nil
	}
	platform := strings.ToLower(strings.TrimSpace(preferred))
	if platform == "auto" || platform == "" {
		platform = mapper.autoPlatform
	}
	switch platform {
	case "qq":
		if record.qqTrackID == "" {
			return NativeIdentity{}, false, nil
		}
		return NativeIdentity{Source: "qq", TrackID: record.qqTrackID}, true, nil
	case "netease":
		if record.neteaseTrackID == "" {
			return NativeIdentity{}, false, nil
		}
		return NativeIdentity{Source: "netease", TrackID: record.neteaseTrackID}, true, nil
	case "none":
		return NativeIdentity{}, false, nil
	default:
		return NativeIdentity{}, false, errors.New("请求的平台偏好无效")
	}
}

func normalizeAutoPlatform(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "", "none":
		return "none", nil
	case "qq", "netease":
		return normalized, nil
	default:
		return "", errors.New("auto 平台策略必须是 none、qq 或 netease")
	}
}

func normalizeISRC(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "")
	value = strings.ReplaceAll(value, " ", "")
	return value
}

func validISRC(value string) bool {
	if len(value) != 12 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if index < 2 {
			if character < 'A' || character > 'Z' {
				return false
			}
			continue
		}
		if index < 5 {
			if !((character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9')) {
				return false
			}
			continue
		}
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validStableID(value string) bool {
	if len(value) < 3 || len(value) > 256 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character < 0x21 || character > 0x7e || character == '\\' || character == '"' {
			return false
		}
	}
	return true
}
