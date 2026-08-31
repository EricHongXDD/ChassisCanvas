package api

import (
	"net/http"
	"strings"

	"casescreen/internal/platformauth"
)

const (
	// JSON 转义和字段名称会比原 cookie.txt 稍大，因此请求上限需高于正文上限。
	maximumPlatformAuthImportBytes = 384 << 10
	maximumPlatformAuthTextBytes   = 256 << 10
)

type platformAuthImportRequest struct {
	Platform string                     `json:"platform"`
	Format   string                     `json:"format"`
	Text     string                     `json:"text,omitempty"`
	Cookies  []platformauth.CookieInput `json:"cookies,omitempty"`
}

func preventPlatformAuthCaching(writer http.ResponseWriter) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
}

// getPlatformAuth 只返回登录态摘要，不包含 Cookie 名称和值。
func (server *Server) getPlatformAuth(writer http.ResponseWriter, _ *http.Request) {
	preventPlatformAuthCaching(writer)
	writeJSON(writer, http.StatusOK, map[string]any{"sessions": server.platformAuth.StatusAll()})
}

// importPlatformAuth 接受用户从已登录平台导出的会话，不接收账号或密码。
func (server *Server) importPlatformAuth(writer http.ResponseWriter, request *http.Request) {
	preventPlatformAuthCaching(writer)
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	var body platformAuthImportRequest
	if err := decodeJSONLimit(writer, request, &body, maximumPlatformAuthImportBytes); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	var (
		result platformauth.ImportResult
		err    error
	)
	switch strings.ToLower(strings.TrimSpace(body.Format)) {
	case "netscape", "cookie.txt":
		if len(body.Text) == 0 || len(body.Text) > maximumPlatformAuthTextBytes {
			writeError(writer, http.StatusBadRequest, "登录态文件必须大于 0 且不超过 256 KiB")
			return
		}
		result, err = server.platformAuth.ImportNetscape(body.Platform, body.Text)
	case "structured", "json":
		result, err = server.platformAuth.ImportStructured(body.Platform, body.Cookies)
	default:
		writeError(writer, http.StatusBadRequest, "登录态格式仅支持 netscape 或 structured")
		return
	}
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) clearPlatformAuth(writer http.ResponseWriter, request *http.Request) {
	preventPlatformAuthCaching(writer)
	if !server.authorized(request) {
		writeError(writer, http.StatusForbidden, "本地控制令牌无效")
		return
	}
	platform := strings.TrimSpace(request.URL.Query().Get("platform"))
	if err := server.platformAuth.Clear(platform); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	status, err := server.platformAuth.StatusFor(platform)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": status})
}
