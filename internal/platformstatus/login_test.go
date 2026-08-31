package platformstatus

import "testing"

func TestRequiresLoginRecognizesHTTP200PlatformErrors(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`{"code":301,"message":"需要登录"}`),
		[]byte(`{"code":200,"needVip":true}`),
		[]byte(`{"code":0,"data":{"error":{"msg":"会员权限不足"}}}`),
		[]byte(`{"status":"403","reason":"forbidden"}`),
	} {
		if !RequiresLogin(data) {
			t.Fatalf("没有识别登录/会员错误: %s", data)
		}
	}
}

func TestRequiresLoginDoesNotInspectLyricsBody(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`{"code":0,"lyric":"[00:00.00]登录你的世界"}`),
		[]byte(`{"code":200,"lrc":{"lyric":"会员的日常"}}`),
		[]byte(`{"code":0,"message":"success"}`),
	} {
		if RequiresLogin(data) {
			t.Fatalf("正常歌词被误判为登录错误: %s", data)
		}
	}
}
