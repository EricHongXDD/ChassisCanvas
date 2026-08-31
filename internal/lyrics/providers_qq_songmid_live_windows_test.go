//go:build windows

package lyrics

import (
	"context"
	"os"
	"testing"
	"time"
)

// QQ 数值 songId 来自 Windows SMTC ID 通道，因此对应在线映射测试也只在
// Windows 构建，避免跨平台测试包引用 Windows 专属适配器。
func TestQQSongIDToMIDLiveSmoke(t *testing.T) {
	if os.Getenv("CASESCREEN_LYRICS_LIVE") != "1" {
		t.Skip("设置 CASESCREEN_LYRICS_LIVE=1 后执行 QQ songId 到 songmid 在线测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	detail, err := (&qqSongMIDClient{}).ResolveSongMID(ctx, 97773)
	if err != nil {
		t.Fatalf("QQ songId 精确转换失败: %v", err)
	}
	if detail.SongID != 97773 || detail.SongMID != "0039MnYb0qxYhV" ||
		detail.Title != "晴天" || detail.Artist != "周杰伦" || detail.Album != "叶惠美" {
		t.Fatalf("QQ songId 返回的歌曲身份不一致: %+v", detail)
	}
	t.Logf("QQ songId %d 精确映射为 songmid %s", detail.SongID, detail.SongMID)
}
