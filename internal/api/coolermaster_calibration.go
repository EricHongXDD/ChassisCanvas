package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"casescreen/internal/config"
	"casescreen/internal/coolergeometry"
)

type coolerMasterCalibrationController interface {
	ShowOrientationCalibration(context.Context, string) error
	SetDisplayRotation(context.Context, string, int) error
	CancelOrientationCalibration(context.Context, string) error
}

type coolerMasterCalibrationStateController interface {
	OrientationCalibrationActive(string) bool
}

type coolerMasterCalibrationResponse struct {
	Target       string `json:"target"`
	Rotation     int    `json:"rotation"`
	Calibrated   bool   `json:"calibrated"`
	Profile      int    `json:"profile"`
	SelectedMark string `json:"selected_mark,omitempty"`
	Queued       bool   `json:"queued"`
	Warning      string `json:"warning,omitempty"`
	Active       bool   `json:"active"`
}

func (server *Server) coolerCalibrationActive(target string) bool {
	controller, ok := server.controller.(coolerMasterCalibrationStateController)
	return ok && controller.OrientationCalibrationActive(target)
}

// coolerCalibrationTargetLock 把同一台冷头屏的方向状态变更串行化。
// references 同时计入当前持有者和等待者，最后一个引用离开时从表中删除。
type coolerCalibrationTargetLock struct {
	token      chan struct{}
	references int
}

func newCoolerCalibrationTargetLock() *coolerCalibrationTargetLock {
	lock := &coolerCalibrationTargetLock{token: make(chan struct{}, 1)}
	lock.token <- struct{}{}
	return lock
}

func (server *Server) lockCoolerCalibrationTarget(ctx context.Context, target string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	target = strings.TrimSpace(target)
	server.coolerCalibrationMu.Lock()
	if server.coolerCalibrationLocks == nil {
		server.coolerCalibrationLocks = make(map[string]*coolerCalibrationTargetLock)
	}
	lock := server.coolerCalibrationLocks[target]
	if lock == nil {
		lock = newCoolerCalibrationTargetLock()
		server.coolerCalibrationLocks[target] = lock
	}
	lock.references++
	server.coolerCalibrationMu.Unlock()

	select {
	case <-lock.token:
		if err := ctx.Err(); err != nil {
			lock.token <- struct{}{}
			server.releaseCoolerCalibrationReference(target, lock)
			return nil, err
		}
		var once sync.Once
		return func() {
			once.Do(func() {
				lock.token <- struct{}{}
				server.releaseCoolerCalibrationReference(target, lock)
			})
		}, nil
	case <-ctx.Done():
		server.releaseCoolerCalibrationReference(target, lock)
		return nil, ctx.Err()
	}
}

func (server *Server) releaseCoolerCalibrationReference(target string, lock *coolerCalibrationTargetLock) {
	server.coolerCalibrationMu.Lock()
	defer server.coolerCalibrationMu.Unlock()
	lock.references--
	if lock.references == 0 && server.coolerCalibrationLocks[target] == lock {
		delete(server.coolerCalibrationLocks, target)
	}
}

func (server *Server) getCoolerMasterCalibration(writer http.ResponseWriter, request *http.Request) {
	target, err := coolerMasterCalibrationTarget(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	unlock, err := server.lockCoolerCalibrationTarget(request.Context(), target)
	if err != nil {
		writeError(writer, http.StatusRequestTimeout, "等待酷冷至尊方向操作失败: "+err.Error())
		return
	}
	defer unlock()
	target, configured, err := server.configuredCoolerMaster(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, coolerMasterCalibrationResponse{
		Target: target, Rotation: configured.DisplayRotation,
		Calibrated: configured.OrientationCalibrated, Profile: configured.OrientationProfile,
		Active: server.coolerCalibrationActive(target),
	})
}

func (server *Server) showCoolerMasterCalibration(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	target, err := coolerMasterCalibrationTarget(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	operationContext, cancel := server.detachedOperationContext(request.Context(), 30*time.Second)
	defer cancel()
	unlock, err := server.lockCoolerCalibrationTarget(operationContext, target)
	if err != nil {
		writeError(writer, http.StatusGatewayTimeout, "等待酷冷至尊方向操作失败: "+err.Error())
		return
	}
	defer unlock()
	target, _, err = server.configuredCoolerMaster(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	controller, ok := server.controller.(coolerMasterCalibrationController)
	if !ok {
		writeError(writer, http.StatusNotImplemented, "当前服务不支持酷冷至尊方向校准")
		return
	}
	if err := controller.ShowOrientationCalibration(operationContext, target); err != nil {
		writeError(writer, http.StatusBadGateway, "显示方向测试图失败: "+err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"target":  target,
		"message": "测试图已显示；请选择位于冷头外壳下方尖角处的数字",
		"active":  true,
	})
}

func (server *Server) saveCoolerMasterCalibration(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	target, err := coolerMasterCalibrationTarget(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Marker string `json:"marker"`
	}
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	rotation, err := coolerRotationForMarker(body.Marker)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	operationContext, cancel := server.detachedOperationContext(request.Context(), 15*time.Minute)
	defer cancel()
	unlock, err := server.lockCoolerCalibrationTarget(operationContext, target)
	if err != nil {
		writeError(writer, http.StatusGatewayTimeout, "等待酷冷至尊方向操作失败: "+err.Error())
		return
	}
	defer unlock()
	target, _, err = server.configuredCoolerMaster(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	controller, ok := server.controller.(coolerMasterCalibrationController)
	if !ok {
		writeError(writer, http.StatusNotImplemented, "当前服务不支持酷冷至尊方向校准")
		return
	}
	if err := server.store.Update(func(cfg *config.Config) error {
		for index := range cfg.Devices {
			if cfg.Devices[index].ID == target && cfg.Devices[index].Kind == "coolermaster" {
				cfg.Devices[index].DisplayRotation = rotation
				cfg.Devices[index].OrientationCalibrated = true
				cfg.Devices[index].OrientationProfile = 1
				return nil
			}
		}
		return fmt.Errorf("未找到酷冷至尊设备 %q", target)
	}); err != nil {
		writeError(writer, http.StatusInternalServerError, "保存方向校准失败: "+err.Error())
		return
	}
	response := coolerMasterCalibrationResponse{
		Target: target, Rotation: rotation, Calibrated: true, Profile: 1,
		SelectedMark: strings.TrimSpace(body.Marker),
	}
	if applyErr := controller.SetDisplayRotation(operationContext, target, rotation); applyErr != nil {
		response.Active = server.coolerCalibrationActive(target)
		response.Queued = true
		response.Warning = "方向已保存；当前设备未完成恢复，将在重连并重新应用主题后生效: " + applyErr.Error()
		writeJSON(writer, http.StatusAccepted, response)
		return
	}
	response.Active = server.coolerCalibrationActive(target)
	writeJSON(writer, http.StatusOK, response)
}

func (server *Server) cancelCoolerMasterCalibration(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	target, err := coolerMasterCalibrationTarget(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	operationContext, cancel := server.detachedOperationContext(request.Context(), 15*time.Minute)
	defer cancel()
	unlock, err := server.lockCoolerCalibrationTarget(operationContext, target)
	if err != nil {
		writeError(writer, http.StatusGatewayTimeout, "等待酷冷至尊方向操作失败: "+err.Error())
		return
	}
	defer unlock()
	target, configured, err := server.configuredCoolerMaster(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	controller, ok := server.controller.(coolerMasterCalibrationController)
	if !ok {
		writeError(writer, http.StatusNotImplemented, "当前服务不支持酷冷至尊方向校准")
		return
	}
	if err := controller.CancelOrientationCalibration(operationContext, target); err != nil {
		writeError(writer, http.StatusBadGateway, "恢复当前主题失败: "+err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, coolerMasterCalibrationResponse{
		Target: target, Rotation: configured.DisplayRotation,
		Calibrated: configured.OrientationCalibrated, Profile: configured.OrientationProfile,
		Active: server.coolerCalibrationActive(target),
	})
}

func isConfiguredCoolerMasterTarget(cfg config.Config, target string) bool {
	target = strings.TrimSpace(target)
	for _, configured := range cfg.Devices {
		if configured.ID == target {
			return configured.Kind == "coolermaster"
		}
	}
	return false
}

func (server *Server) configuredCoolerMaster(request *http.Request) (string, config.Device, error) {
	target, err := coolerMasterCalibrationTarget(request)
	if err != nil {
		return "", config.Device{}, err
	}
	for _, configured := range server.store.Get().Devices {
		if configured.ID == target {
			if configured.Kind != "coolermaster" {
				return "", config.Device{}, fmt.Errorf("目标 %q 不是酷冷至尊副屏", target)
			}
			return target, configured, nil
		}
	}
	return "", config.Device{}, fmt.Errorf("未找到酷冷至尊副屏 %q", target)
}

func coolerMasterCalibrationTarget(request *http.Request) (string, error) {
	target := strings.TrimSpace(request.URL.Query().Get("target"))
	if target == "" || target == "all" {
		return "", fmt.Errorf("必须指定一台酷冷至尊副屏")
	}
	return target, nil
}

// coolerRotationForMarker 使用钟面标识：12 在上、3 在右、6 在下、9 在左。
// 用户选择物理下方尖角旁的数字后，把逻辑画布的下方向旋到该像素方向。
func coolerRotationForMarker(marker string) (int, error) {
	return coolergeometry.RotationForMarker(marker)
}
