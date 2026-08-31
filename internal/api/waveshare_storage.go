package api

import (
	"context"
	"net/http"

	"casescreen/internal/device"
)

// waveshareStorageRefresher 是容量主动刷新 API 的可选控制器能力，
// 避免影响只实现基础 Controller 的测试替身和其他调用方。
type waveshareStorageRefresher interface {
	RefreshDeviceStorage(context.Context, string) (device.StorageInfo, error)
}

func (server *Server) refreshWaveshareStorage(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	target, err := server.validateWaveshareLibraryTarget(request.URL.Query().Get("target"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	refresher, ok := server.controller.(waveshareStorageRefresher)
	if !ok {
		writeError(writer, http.StatusNotImplemented, "当前服务不支持主动刷新设备容量")
		return
	}
	storage, err := refresher.RefreshDeviceStorage(request.Context(), target)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "读取微雪 TF 卡容量失败: "+err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"target": target, "storage": storage})
}
