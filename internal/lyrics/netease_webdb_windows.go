//go:build windows

package lyrics

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	neteaseSQLiteOK           = 0
	neteaseSQLiteRow          = 100
	neteaseSQLiteDone         = 101
	neteaseSQLiteInteger      = 1
	neteaseSQLiteText         = 3
	neteaseSQLiteOpenReadOnly = 0x00000001
	neteaseSQLitePrivateCache = 0x00040000
)

const neteaseLatestPlaybackSQL = `
SELECT pc.resourceId, pc.playDuration, pc.updateTime, pc.resourceType, dt.jsonStr
FROM playingCount AS pc
JOIN dbTrack AS dt ON dt.id = pc.resourceId
WHERE pc.resourceType = ?1 AND length(dt.jsonStr) BETWEEN 2 AND ?2
ORDER BY pc.updateTime DESC
LIMIT 2`

var (
	neteaseSQLiteDLL             = windows.NewLazySystemDLL("winsqlite3.dll")
	neteaseSQLiteOpenV2          = neteaseSQLiteDLL.NewProc("sqlite3_open_v2")
	neteaseSQLiteCloseV2         = neteaseSQLiteDLL.NewProc("sqlite3_close_v2")
	neteaseSQLiteDBReadOnly      = neteaseSQLiteDLL.NewProc("sqlite3_db_readonly")
	neteaseSQLitePrepareV2       = neteaseSQLiteDLL.NewProc("sqlite3_prepare_v2")
	neteaseSQLiteBindText        = neteaseSQLiteDLL.NewProc("sqlite3_bind_text")
	neteaseSQLiteBindInt64       = neteaseSQLiteDLL.NewProc("sqlite3_bind_int64")
	neteaseSQLiteStep            = neteaseSQLiteDLL.NewProc("sqlite3_step")
	neteaseSQLiteFinalize        = neteaseSQLiteDLL.NewProc("sqlite3_finalize")
	neteaseSQLiteColumnType      = neteaseSQLiteDLL.NewProc("sqlite3_column_type")
	neteaseSQLiteColumnInt64     = neteaseSQLiteDLL.NewProc("sqlite3_column_int64")
	neteaseSQLiteColumnText      = neteaseSQLiteDLL.NewProc("sqlite3_column_text")
	neteaseSQLiteColumnBytes     = neteaseSQLiteDLL.NewProc("sqlite3_column_bytes")
	neteaseSQLiteRequiredExports = []*windows.LazyProc{
		neteaseSQLiteOpenV2, neteaseSQLiteCloseV2, neteaseSQLiteDBReadOnly,
		neteaseSQLitePrepareV2, neteaseSQLiteBindText, neteaseSQLiteBindInt64,
		neteaseSQLiteStep, neteaseSQLiteFinalize, neteaseSQLiteColumnType,
		neteaseSQLiteColumnInt64, neteaseSQLiteColumnText, neteaseSQLiteColumnBytes,
	}
)

// readLatestNeteaseWebDBPlayback 使用 Windows 自带 winsqlite3.dll 直接只读
// 查询原数据库。SQL 完全固定，resourceType 与长度限制均使用绑定参数。
func readLatestNeteaseWebDBPlayback(path string) (result neteaseWebDBPlaybackEvidence, resultErr error) {
	path = strings.TrimSpace(path)
	if path == "" || len(path) > 64<<10 || strings.IndexByte(path, 0) >= 0 {
		return neteaseWebDBPlaybackEvidence{}, errors.New("网易云播放数据库路径无效")
	}
	if err := loadNeteaseSQLite(); err != nil {
		return neteaseWebDBPlaybackEvidence{}, err
	}
	pathBytes, err := windows.BytePtrFromString(path)
	if err != nil {
		return neteaseWebDBPlaybackEvidence{}, errors.New("网易云播放数据库路径编码失败")
	}
	var database uintptr
	code, _, _ := neteaseSQLiteOpenV2.Call(
		uintptr(unsafe.Pointer(pathBytes)), uintptr(unsafe.Pointer(&database)),
		neteaseSQLiteOpenReadOnly|neteaseSQLitePrivateCache, 0,
	)
	if database != 0 {
		defer func() {
			closeCode, _, _ := neteaseSQLiteCloseV2.Call(database)
			if closeCode != neteaseSQLiteOK && resultErr == nil {
				result = neteaseWebDBPlaybackEvidence{}
				resultErr = neteaseSQLiteError("关闭网易云播放数据库", closeCode)
			}
		}()
	}
	if code != neteaseSQLiteOK || database == 0 {
		return neteaseWebDBPlaybackEvidence{}, neteaseSQLiteError("只读打开网易云播放数据库", code)
	}
	mainName, _ := windows.BytePtrFromString("main")
	readOnly, _, _ := neteaseSQLiteDBReadOnly.Call(database, uintptr(unsafe.Pointer(mainName)))
	if int32(readOnly) != 1 {
		return neteaseWebDBPlaybackEvidence{}, errors.New("网易云播放数据库未处于只读模式")
	}

	queryBytes, _ := windows.BytePtrFromString(neteaseLatestPlaybackSQL)
	var statement uintptr
	code, _, _ = neteaseSQLitePrepareV2.Call(
		database, uintptr(unsafe.Pointer(queryBytes)), uintptr(len(neteaseLatestPlaybackSQL)),
		uintptr(unsafe.Pointer(&statement)), 0,
	)
	if statement != 0 {
		defer func() {
			finalizeCode, _, _ := neteaseSQLiteFinalize.Call(statement)
			if finalizeCode != neteaseSQLiteOK && resultErr == nil {
				result = neteaseWebDBPlaybackEvidence{}
				resultErr = neteaseSQLiteError("释放网易云播放证据查询", finalizeCode)
			}
		}()
	}
	if code != neteaseSQLiteOK || statement == 0 {
		return neteaseWebDBPlaybackEvidence{}, neteaseSQLiteError("准备网易云播放证据查询", code)
	}

	resourceType := []byte("track")
	code, _, _ = neteaseSQLiteBindText.Call(
		statement, 1, uintptr(unsafe.Pointer(&resourceType[0])), uintptr(len(resourceType)), ^uintptr(0),
	)
	if code != neteaseSQLiteOK {
		return neteaseWebDBPlaybackEvidence{}, neteaseSQLiteError("绑定网易云资源类型", code)
	}
	code, _, _ = neteaseSQLiteBindInt64.Call(statement, 2, maximumNeteaseDBTrackJSONBytes)
	if code != neteaseSQLiteOK {
		return neteaseWebDBPlaybackEvidence{}, neteaseSQLiteError("绑定网易云元数据长度", code)
	}
	runtime.KeepAlive(resourceType)

	stepCode, _, _ := neteaseSQLiteStep.Call(statement)
	if stepCode == neteaseSQLiteDone {
		return neteaseWebDBPlaybackEvidence{}, errors.New("网易云播放数据库没有 track 证据")
	}
	if stepCode != neteaseSQLiteRow {
		return neteaseWebDBPlaybackEvidence{}, neteaseSQLiteError("读取网易云播放证据", stepCode)
	}
	first, err := readNeteaseSQLiteEvidenceRow(statement)
	if err != nil {
		return neteaseWebDBPlaybackEvidence{}, err
	}
	stepCode, _, _ = neteaseSQLiteStep.Call(statement)
	if stepCode == neteaseSQLiteRow {
		secondID, secondIDErr := readNeteaseSQLiteResourceID(statement, 0)
		secondUpdate, secondUpdateErr := readNeteaseSQLiteInteger(statement, 2)
		if secondIDErr != nil || secondUpdateErr != nil {
			return neteaseWebDBPlaybackEvidence{}, errors.New("网易云次新播放证据无效")
		}
		if secondUpdate == first.UpdateTime && secondID != first.ResourceID {
			return neteaseWebDBPlaybackEvidence{}, errors.New("网易云最新播放证据存在并列歌曲")
		}
	} else if stepCode != neteaseSQLiteDone {
		return neteaseWebDBPlaybackEvidence{}, neteaseSQLiteError("结束网易云播放证据查询", stepCode)
	}
	return first, nil
}

func loadNeteaseSQLite() error {
	if err := neteaseSQLiteDLL.Load(); err != nil {
		return errors.New("Windows winsqlite3.dll 不可用")
	}
	for _, procedure := range neteaseSQLiteRequiredExports {
		if err := procedure.Find(); err != nil {
			return errors.New("Windows winsqlite3.dll 缺少所需只读接口")
		}
	}
	return nil
}

func readNeteaseSQLiteEvidenceRow(statement uintptr) (neteaseWebDBPlaybackEvidence, error) {
	resourceID, err := readNeteaseSQLiteResourceID(statement, 0)
	if err != nil {
		return neteaseWebDBPlaybackEvidence{}, err
	}
	playDuration, err := readNeteaseSQLiteInteger(statement, 1)
	if err != nil {
		return neteaseWebDBPlaybackEvidence{}, err
	}
	updateTime, err := readNeteaseSQLiteInteger(statement, 2)
	if err != nil {
		return neteaseWebDBPlaybackEvidence{}, err
	}
	resourceType, err := readNeteaseSQLiteText(statement, 3, 32)
	if err != nil {
		return neteaseWebDBPlaybackEvidence{}, err
	}
	if !strings.EqualFold(resourceType, "track") {
		return neteaseWebDBPlaybackEvidence{}, errors.New("网易云播放证据资源类型无效")
	}
	trackJSONType, _, _ := neteaseSQLiteColumnType.Call(statement, 4)
	if trackJSONType != neteaseSQLiteText {
		return neteaseWebDBPlaybackEvidence{}, errors.New("网易云 dbTrack JSON 类型无效")
	}
	trackJSON, err := readNeteaseSQLiteBytes(statement, 4, maximumNeteaseDBTrackJSONBytes)
	if err != nil {
		return neteaseWebDBPlaybackEvidence{}, err
	}
	if !utf8.Valid(trackJSON) {
		return neteaseWebDBPlaybackEvidence{}, errors.New("网易云 dbTrack JSON 编码无效")
	}
	return neteaseWebDBPlaybackEvidence{
		ResourceID: resourceID, ResourceType: resourceType, PlayDuration: playDuration,
		UpdateTime: updateTime, TrackJSON: trackJSON,
	}, nil
}

func readNeteaseSQLiteResourceID(statement uintptr, column int) (string, error) {
	columnType, _, _ := neteaseSQLiteColumnType.Call(statement, uintptr(column))
	switch columnType {
	case neteaseSQLiteInteger:
		value, err := readNeteaseSQLiteInteger(statement, column)
		if err != nil || value <= 0 {
			return "", errors.New("网易云播放证据歌曲 ID 无效")
		}
		return strconv.FormatInt(value, 10), nil
	case neteaseSQLiteText:
		value, err := readNeteaseSQLiteText(statement, column, 32)
		if err != nil {
			return "", err
		}
		if _, valid := parseNeteaseSongID(value); !valid {
			return "", errors.New("网易云播放证据歌曲 ID 无效")
		}
		return value, nil
	default:
		return "", errors.New("网易云播放证据歌曲 ID 类型无效")
	}
}

func readNeteaseSQLiteInteger(statement uintptr, column int) (int64, error) {
	columnType, _, _ := neteaseSQLiteColumnType.Call(statement, uintptr(column))
	if columnType != neteaseSQLiteInteger {
		return 0, errors.New("网易云播放证据整数类型无效")
	}
	value, _, _ := neteaseSQLiteColumnInt64.Call(statement, uintptr(column))
	return int64(value), nil
}

func readNeteaseSQLiteText(statement uintptr, column, maximum int) (string, error) {
	columnType, _, _ := neteaseSQLiteColumnType.Call(statement, uintptr(column))
	if columnType != neteaseSQLiteText {
		return "", errors.New("网易云播放证据文本类型无效")
	}
	data, err := readNeteaseSQLiteBytes(statement, column, maximum)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", errors.New("网易云播放证据文本编码无效")
	}
	return string(data), nil
}

func readNeteaseSQLiteBytes(statement uintptr, column, maximum int) ([]byte, error) {
	pointer, _, _ := neteaseSQLiteColumnText.Call(statement, uintptr(column))
	if pointer == 0 {
		return nil, errors.New("网易云播放证据文本为空")
	}
	lengthValue, _, _ := neteaseSQLiteColumnBytes.Call(statement, uintptr(column))
	length := int64(lengthValue)
	if length <= 0 || length > int64(maximum) {
		return nil, errors.New("网易云播放证据文本长度无效")
	}
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(pointer)), int(length))...), nil
}

func neteaseSQLiteError(operation string, code uintptr) error {
	return fmt.Errorf("%s失败（SQLite code=%d）", operation, uint32(code))
}
