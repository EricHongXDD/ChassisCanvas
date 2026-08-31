//go:build windows

package coolermaster

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"

	protocol "casescreen/internal/protocol/coolermaster"
	"casescreen/internal/theme"
	"casescreen/internal/usb/hidapi"
)

func TestHardwareRealtimeFrame(t *testing.T) {
	dllPath := os.Getenv("CASESCREEN_COOLER_HIDAPI")
	if dllPath == "" {
		t.Skip("设置 CASESCREEN_COOLER_HIDAPI 后执行酷冷至尊真机实时帧测试")
	}
	transport, err := hidapi.Open(dllPath, 0x2516, 0x0222, 0)
	if err != nil {
		t.Fatal(err)
	}
	driver := New("cooler-hardware", "酷冷至尊真机", [2]int{480, 480}, 1, transport, "")
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer driver.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := driver.SetRealtimeEnabled(ctx, false); err != nil {
			t.Errorf("恢复酷冷至尊 Offline Mode: %v", err)
		}
	}()

	frame := image.NewRGBA(image.Rect(0, 0, 480, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 480; x++ {
			frame.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 255 / 479),
				G: uint8(y * 255 / 479),
				B: uint8((x + y) * 255 / 958),
				A: 255,
			})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, frame, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := driver.SendJPEG(ctx, encoded.Bytes()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(750 * time.Millisecond)
}

func TestHardwareOfflineJPEGSlot4(t *testing.T) {
	if os.Getenv("CASESCREEN_COOLER_OFFLINE_TEST") != "1" {
		t.Skip("设置 CASESCREEN_COOLER_OFFLINE_TEST=1 后执行酷冷至尊槽位 4 离线 JPG 测试")
	}
	dllPath := os.Getenv("CASESCREEN_COOLER_HIDAPI")
	if dllPath == "" {
		t.Skip("设置 CASESCREEN_COOLER_HIDAPI 后执行酷冷至尊真机离线 JPG 测试")
	}
	transport, err := hidapi.Open(dllPath, 0x2516, 0x0222, 0)
	if err != nil {
		t.Fatal(err)
	}
	driver := New(
		"cooler-offline-hardware", "酷冷至尊真机", [2]int{480, 480}, 4,
		transport, t.TempDir()+"/state.json",
	)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer driver.Close()

	nonce := time.Now().Format("15:04:05")
	manifest := theme.Manifest{
		ID:    "cooler-slot-4-test-" + nonce,
		Name:  "酷冷槽位 4 真机测试",
		Width: 480, Height: 480,
		Layers: []theme.Layer{
			{ID: "canvas", Type: theme.LayerCanvas, Bounds: theme.Rect{Width: 1, Height: 1}, Opacity: 1,
				Options: map[string]any{"color": "#101820ff"}},
			{ID: "red", Type: theme.LayerShape, Bounds: theme.Rect{X: 0, Y: 0, Width: .5, Height: .5}, Opacity: 1,
				Options: map[string]any{"shape": "rect", "fill_color": "#ff1744ff", "stroke_width": 0}},
			{ID: "green", Type: theme.LayerShape, Bounds: theme.Rect{X: .5, Y: 0, Width: .5, Height: .5}, Opacity: 1,
				Options: map[string]any{"shape": "rect", "fill_color": "#00e676ff", "stroke_width": 0}},
			{ID: "blue", Type: theme.LayerShape, Bounds: theme.Rect{X: 0, Y: .5, Width: .5, Height: .5}, Opacity: 1,
				Options: map[string]any{"shape": "rect", "fill_color": "#2979ffff", "stroke_width": 0}},
			{ID: "yellow", Type: theme.LayerShape, Bounds: theme.Rect{X: .5, Y: .5, Width: .5, Height: .5}, Opacity: 1,
				Options: map[string]any{"shape": "rect", "fill_color": "#ffea00ff", "stroke_width": 0}},
			{ID: "label", Type: theme.LayerText, Bounds: theme.Rect{X: .05, Y: .31, Width: .9, Height: .38}, Opacity: 1,
				Options: map[string]any{
					"text": fmt.Sprintf("CASESCREEN\nSLOT 4\n%s", nonce), "color": "#ffffffff",
					"font_size": 64, "font_weight": 900, "align": "center", "vertical_align": "middle",
					"outline_color": "#000000ff", "outline_width": 5,
				}},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := driver.ApplyTheme(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	t.Logf("槽位 4 离线 JPG 已提交，画面 nonce=%s", nonce)
	// 原厂独立 GetSuspend 会发送一次 param，随后最多读取五帧，
	// 用于观察设备当前报告的 suspendMediaActive。这里把原始状态
	// 留在真机测试日志中，区分“文件已提交”和“播放器已激活”。
	driver.mu.Lock()
	paramReport, paramErr := protocol.BuildParamRequest()
	if paramErr == nil {
		driver.drainResponsesLocked(ctx)
		paramErr = driver.writeReportLocked(ctx, paramReport)
	}
	if paramErr == nil {
		paramErr = driver.pause(ctx, 60*time.Millisecond)
	}
	for read := 1; read <= 5 && paramErr == nil; read++ {
		response, readErr := driver.readResponseLockedTimeout(ctx, offlineResponseTimeout)
		if readErr != nil {
			paramErr = readErr
			break
		}
		t.Logf("param 状态第 %d 帧: %+v", read, response.JSON)
		if _, ok := response.JSON["suspendMediaActive"]; ok {
			break
		}
	}
	driver.mu.Unlock()
	if paramErr != nil {
		t.Logf("param 状态未返回（不否定 transported 提交）: %v", paramErr)
	}
	time.Sleep(2 * time.Second)
}

func TestHardwareCleanupLegacySlot5(t *testing.T) {
	if os.Getenv("CASESCREEN_COOLER_DELETE_LEGACY_SLOT5") != "DELETE_PHYSICAL_SLOT_5" {
		t.Skip("设置 CASESCREEN_COOLER_DELETE_LEGACY_SLOT5=DELETE_PHYSICAL_SLOT_5 后才允许删除旧物理槽位 5")
	}
	if err := CheckOEMControlConflict(); err != nil {
		t.Fatalf("原厂控制程序仍在运行，拒绝迁移: %v", err)
	}
	dllPath := os.Getenv("CASESCREEN_COOLER_HIDAPI")
	if dllPath == "" {
		t.Fatal("必须显式设置 CASESCREEN_COOLER_HIDAPI")
	}
	statePath := os.Getenv("CASESCREEN_COOLER_STATE_PATH")
	if statePath == "" || !filepath.IsAbs(statePath) {
		t.Fatal("必须用 CASESCREEN_COOLER_STATE_PATH 指定现有酷冷状态文件的绝对路径")
	}
	state, err := loadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	slot, exists := state.Slots[4]
	if !exists || !isVerifiedSlot4Target(slot) {
		t.Fatalf("状态文件没有已验证的物理槽位 4 新资源，拒绝删除槽位 5: %+v", slot)
	}
	if slot.LegacyAliasMigration == legacySlot5MigrationComplete {
		t.Skip("旧物理槽位 5 已完成迁移")
	}
	// 强确认门控本身代表用户批准本次一次性迁移。先持久化 approved，
	// 即使测试中途退出，下次也只会续做小型删除/状态查询，不会重传大文件。
	slot.LegacyAliasMigration = legacySlot5MigrationApproved
	state.Slots[4] = slot
	if err := saveState(statePath, state); err != nil {
		t.Fatalf("写入迁移 approved 标记: %v", err)
	}

	transport, err := hidapi.Open(dllPath, 0x2516, 0x0222, 0)
	if err != nil {
		t.Fatal(err)
	}
	driver := New("cooler-legacy-slot5-cleanup", "酷冷至尊真机", [2]int{480, 480}, 4, transport, statePath)
	if err := driver.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer driver.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	driver.mu.Lock()
	err = driver.migrateLegacySlot5IfNeededLocked(ctx, &state)
	driver.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	verified, err := loadState(statePath)
	if err != nil || verified.Slots[4].LegacyAliasMigration != legacySlot5MigrationComplete {
		t.Fatalf("真机迁移完成后状态未落盘: state=%+v err=%v", verified, err)
	}
	t.Log("真机已验证：物理槽位 4 活动、旧物理槽位 5 不活动；迁移标记 complete")
}
