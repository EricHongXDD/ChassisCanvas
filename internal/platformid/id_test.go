package platformid

import "testing"

func TestQQAndNeteaseIDFormats(t *testing.T) {
	if !QQ("0039MnYb0qxYhV") || !QQ("1234567890") || QQ("0039-MnYb0qxYhV") {
		t.Fatal("QQ songmid 校验错误")
	}
	if !Netease("186016") || !Netease("9223372036854775807") ||
		Netease("0186016") || Netease("9223372036854775808") || Netease("0039MnYb0qxYhV") {
		t.Fatal("网易云歌曲 ID 校验错误")
	}
}
