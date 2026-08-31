package lyrics

import "testing"

func TestParseWindowTitle(t *testing.T) {
	track := parseWindowTitle("netease", "瓷意千年 - Jason嘉/赵方婧 - 网易云音乐")
	if track.Title != "瓷意千年" || track.Artist != "Jason嘉/赵方婧" || track.Source != "netease" || track.Playing {
		t.Fatalf("窗口标题解析错误: %#v", track)
	}
	if parseWindowTitle("qq", "QQ音乐").Valid() {
		t.Fatal("播放器空闲标题不应被当作歌曲")
	}
	sogou := parseWindowTitle("sogou", "夜曲 - 周杰伦 - 搜狗音乐")
	if sogou.Title != "夜曲" || sogou.Artist != "周杰伦" || sogou.Source != "sogou" {
		t.Fatalf("搜狗音乐窗口标题解析错误: %#v", sogou)
	}
	if parseWindowTitle("kuwo", "酷我音乐").Valid() {
		t.Fatal("酷我音乐空闲标题不应被当作歌曲")
	}
}
