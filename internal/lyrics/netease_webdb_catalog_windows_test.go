//go:build windows

package lyrics

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	errNeteaseCatalogNotReadOnly = errors.New("网易云数据库结构查询未处于只读模式")
	errNeteaseCatalogRowInvalid  = errors.New("网易云数据库结构记录无效")
)

const neteaseSQLiteCatalogSQL = `
SELECT name, coalesce(sql, '')
FROM sqlite_master
WHERE type IN ('table', 'view')
ORDER BY name`

// TestNeteaseWebDBCatalogLive 仅用于人工兼容性诊断。它以只读方式列出
// 网易云本地数据库的表结构，不读取用户歌单、账号或 Cookie 内容。
func TestNeteaseWebDBCatalogLive(t *testing.T) {
	if os.Getenv("CASESCREEN_NETEASE_WEBDB_CATALOG_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_NETEASE_WEBDB_CATALOG_LIVE=1 后只读列出本机数据库结构")
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal("无法定位本机网易云数据库")
	}
	path := filepath.Join(cacheDir, "NetEase", "CloudMusic", "Library", "webdb.dat")
	rows, err := readNeteaseSQLiteCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		t.Logf("name=%q schema=%q", row[0], row[1])
	}
}

func TestNeteaseWebDBStateRowsLive(t *testing.T) {
	if os.Getenv("CASESCREEN_NETEASE_WEBDB_STATE_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_NETEASE_WEBDB_STATE_LIVE=1 后只读列出播放状态候选记录")
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal("无法定位本机网易云数据库")
	}
	path := filepath.Join(cacheDir, "NetEase", "CloudMusic", "Library", "webdb.dat")
	queries := []struct {
		name    string
		query   string
		columns int
	}{
		{
			name: "historyTracks",
			query: `SELECT id, printf('%lld', playtime), printf('%d', length(jsonStr))
				FROM historyTracks ORDER BY playtime DESC LIMIT 12`,
			columns: 3,
		},
		{
			name: "persistentModel",
			query: `SELECT uniKey, printf('%lld', time), printf('%lld', clearTime),
				printf('%d', length(jsonStr)) FROM persistentModel ORDER BY time DESC LIMIT 40`,
			columns: 4,
		},
		{
			name: "tsPlayingCount",
			query: `SELECT resourceId, printf('%lld', playDuration), printf('%lld', updateTime),
				resourceType, printf('%d', length(jsonStr)) FROM tsPlayingCount
				ORDER BY updateTime DESC LIMIT 12`,
			columns: 5,
		},
	}
	for _, candidate := range queries {
		rows, queryErr := readNeteaseSQLiteTextQuery(path, candidate.query, candidate.columns)
		if queryErr != nil {
			t.Fatalf("查询 %s 失败: %v", candidate.name, queryErr)
		}
		for _, row := range rows {
			t.Logf("table=%s row=%q", candidate.name, row)
		}
	}
}

func readNeteaseSQLiteCatalog(path string) (rows [][2]string, resultErr error) {
	if err := loadNeteaseSQLite(); err != nil {
		return nil, err
	}
	pathBytes, err := windows.BytePtrFromString(path)
	if err != nil {
		return nil, err
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
				rows = nil
				resultErr = neteaseSQLiteError("关闭网易云数据库结构查询", closeCode)
			}
		}()
	}
	if code != neteaseSQLiteOK || database == 0 {
		return nil, neteaseSQLiteError("只读打开网易云数据库结构", code)
	}
	mainName, _ := windows.BytePtrFromString("main")
	readOnly, _, _ := neteaseSQLiteDBReadOnly.Call(database, uintptr(unsafe.Pointer(mainName)))
	if int32(readOnly) != 1 {
		return nil, errNeteaseCatalogNotReadOnly
	}

	queryBytes, _ := windows.BytePtrFromString(neteaseSQLiteCatalogSQL)
	var statement uintptr
	code, _, _ = neteaseSQLitePrepareV2.Call(
		database, uintptr(unsafe.Pointer(queryBytes)), uintptr(len(neteaseSQLiteCatalogSQL)),
		uintptr(unsafe.Pointer(&statement)), 0,
	)
	if statement != 0 {
		defer func() {
			finalizeCode, _, _ := neteaseSQLiteFinalize.Call(statement)
			if finalizeCode != neteaseSQLiteOK && resultErr == nil {
				rows = nil
				resultErr = neteaseSQLiteError("释放网易云数据库结构查询", finalizeCode)
			}
		}()
	}
	if code != neteaseSQLiteOK || statement == 0 {
		return nil, neteaseSQLiteError("准备网易云数据库结构查询", code)
	}

	for {
		stepCode, _, _ := neteaseSQLiteStep.Call(statement)
		if stepCode == neteaseSQLiteDone {
			return rows, nil
		}
		if stepCode != neteaseSQLiteRow {
			return nil, neteaseSQLiteError("读取网易云数据库结构", stepCode)
		}
		name, nameErr := readNeteaseSQLiteText(statement, 0, 256)
		schema, schemaErr := readNeteaseSQLiteText(statement, 1, 64<<10)
		if nameErr != nil || schemaErr != nil {
			return nil, errNeteaseCatalogRowInvalid
		}
		rows = append(rows, [2]string{name, schema})
	}
}

func readNeteaseSQLiteTextQuery(path, query string, columns int) (rows [][]string, resultErr error) {
	if columns <= 0 || columns > 16 {
		return nil, errNeteaseCatalogRowInvalid
	}
	if err := loadNeteaseSQLite(); err != nil {
		return nil, err
	}
	pathBytes, err := windows.BytePtrFromString(path)
	if err != nil {
		return nil, err
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
				rows = nil
				resultErr = neteaseSQLiteError("关闭网易云状态查询", closeCode)
			}
		}()
	}
	if code != neteaseSQLiteOK || database == 0 {
		return nil, neteaseSQLiteError("只读打开网易云状态数据库", code)
	}
	mainName, _ := windows.BytePtrFromString("main")
	readOnly, _, _ := neteaseSQLiteDBReadOnly.Call(database, uintptr(unsafe.Pointer(mainName)))
	if int32(readOnly) != 1 {
		return nil, errNeteaseCatalogNotReadOnly
	}

	queryBytes, err := windows.BytePtrFromString(query)
	if err != nil {
		return nil, err
	}
	var statement uintptr
	code, _, _ = neteaseSQLitePrepareV2.Call(
		database, uintptr(unsafe.Pointer(queryBytes)), uintptr(len(query)),
		uintptr(unsafe.Pointer(&statement)), 0,
	)
	if statement != 0 {
		defer func() {
			finalizeCode, _, _ := neteaseSQLiteFinalize.Call(statement)
			if finalizeCode != neteaseSQLiteOK && resultErr == nil {
				rows = nil
				resultErr = neteaseSQLiteError("释放网易云状态查询", finalizeCode)
			}
		}()
	}
	if code != neteaseSQLiteOK || statement == 0 {
		return nil, neteaseSQLiteError("准备网易云状态查询", code)
	}

	for {
		stepCode, _, _ := neteaseSQLiteStep.Call(statement)
		if stepCode == neteaseSQLiteDone {
			return rows, nil
		}
		if stepCode != neteaseSQLiteRow {
			return nil, neteaseSQLiteError("读取网易云状态查询", stepCode)
		}
		row := make([]string, columns)
		for column := range columns {
			value, valueErr := readNeteaseSQLiteText(statement, column, 64<<10)
			if valueErr != nil {
				return nil, errNeteaseCatalogRowInvalid
			}
			row[column] = value
		}
		rows = append(rows, row)
	}
}
