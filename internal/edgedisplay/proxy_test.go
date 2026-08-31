package edgedisplay

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	protocol "casescreen/internal/protocol/waveshare"
	"casescreen/internal/telemetry"
)

type memoryEndpoint struct {
	input  *bytes.Reader
	output bytes.Buffer
}

func (endpoint *memoryEndpoint) Read(buffer []byte) (int, error) {
	return endpoint.input.Read(buffer)
}

func (endpoint *memoryEndpoint) Write(buffer []byte) (int, error) {
	return endpoint.output.Write(buffer)
}

func (*memoryEndpoint) Close() error               { return nil }
func (*memoryEndpoint) Restore() error             { return nil }
func (*memoryEndpoint) HostOpener() hostPortOpener { return nil }

func TestSendLyricsUsesResidentThemeKeysAndWaitsForAck(t *testing.T) {
	ack := protocol.EncodePacket(protocol.Packet{
		Type: protocol.PacketTypeCommandAck, Command: protocol.CommandSystemData,
	})
	endpoint := &memoryEndpoint{input: bytes.NewReader(ack)}
	bridge, err := newProtocolBridge(endpoint, nil, time.Second, 5*time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	proxy := &Proxy{
		bridge:    bridge,
		childDone: make(chan struct{}),
	}
	if err := proxy.SendLyrics(context.Background(), "第一句", "第二句"); err != nil {
		t.Fatal(err)
	}
	packet, err := protocol.ReadPacket(bufio.NewReader(bytes.NewReader(endpoint.output.Bytes())))
	if err != nil {
		t.Fatal(err)
	}
	if packet.Command != protocol.CommandSystemData || packet.Type != protocol.PacketTypeCommand {
		t.Fatalf("发送了错误的微雪命令: %+v", packet)
	}
	values, err := protocol.DecodeQStringMap(packet.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if values["CaseScreen_LyricsCurrent"] != "第一句" || values["CaseScreen_LyricsNext"] != "第二句" {
		t.Fatalf("歌词键值错误: %#v", values)
	}
}

func TestReloadThemeUsesResidentPathAndWaitsForAck(t *testing.T) {
	ack := protocol.EncodePacket(protocol.Packet{
		Type: protocol.PacketTypeCommandAck, Command: protocol.CommandReloadTheme,
	})
	endpoint := &memoryEndpoint{input: bytes.NewReader(ack)}
	bridge, err := newProtocolBridge(endpoint, nil, time.Second, 5*time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	proxy := &Proxy{bridge: bridge, childDone: make(chan struct{})}
	remotePath := "/data/theme/1024x600/casescreen-standalone.Theme"
	if err := proxy.ReloadTheme(context.Background(), remotePath); err != nil {
		t.Fatal(err)
	}
	packet, err := protocol.ReadPacket(bufio.NewReader(bytes.NewReader(endpoint.output.Bytes())))
	if err != nil {
		t.Fatal(err)
	}
	if packet.Command != protocol.CommandReloadTheme || string(packet.Payload) != remotePath {
		t.Fatalf("驻留主题切换命令错误: %+v", packet)
	}
}

func TestReloadThemeRejectsNonResidentPath(t *testing.T) {
	proxy := &Proxy{}
	for _, remotePath := range []string{"", "/tmp/theme.Theme", "/data/theme/1024x600/theme.json"} {
		if err := proxy.ReloadTheme(context.Background(), remotePath); err == nil {
			t.Fatalf("应拒绝无效驻留主题路径: %q", remotePath)
		}
	}
}

func TestSendLyricsUpdateCarriesOnlySmallPlaybackState(t *testing.T) {
	values, err := LyricsValues(LyricsUpdate{
		Source: " QQ ", TrackID: "0039MnYb0qxYhV", Position: 12_345_000_000,
		Playing: true, LyricMode: LyricModeTimed, Current: "当前句", Next: "下一句",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"source": "qq", "track_id": "0039MnYb0qxYhV", "position": "12345000000",
		"playing": "true", "lyric_mode": "timed",
		"CaseScreen_LyricsCurrent": "当前句", "CaseScreen_LyricsNext": "下一句",
		telemetry.LyricsVisibleKey: "1",
	}
	if len(values) != len(want) {
		t.Fatalf("小包包含非必要字段: %#v", values)
	}
	for key, expected := range want {
		if values[key] != expected {
			t.Errorf("字段 %s: got=%q want=%q", key, values[key], expected)
		}
	}
}

func TestPlainLyricsShowsNoticeInsteadOfWholeText(t *testing.T) {
	values, err := LyricsValues(LyricsUpdate{
		Source: "netease", TrackID: "19723756", LyricMode: LyricModePlain,
		Current: "这里即使误传了整段纯文本，也不应进入显示包", Next: "不应显示",
	})
	if err != nil {
		t.Fatal(err)
	}
	if values["CaseScreen_LyricsCurrent"] != PlainLyricsNotice || values["CaseScreen_LyricsNext"] != "" {
		t.Fatalf("纯文本歌词没有降级为固定提示: %#v", values)
	}
	if values[telemetry.LyricsVisibleKey] != "1" {
		t.Fatalf("纯文本歌词提示被错误隐藏: %#v", values)
	}
}

func TestLyricsValuesHidesWaitingLayerAndDropsOrphanNextLine(t *testing.T) {
	values, err := LyricsValues(LyricsUpdate{Playing: true, LyricMode: LyricModeTimed, Next: "不应单独显示"})
	if err != nil {
		t.Fatal(err)
	}
	if values[telemetry.LyricsVisibleKey] != "0" || values["CaseScreen_LyricsCurrent"] != "" || values["CaseScreen_LyricsNext"] != "" {
		t.Fatalf("等待歌词时没有隐藏整个图层: %#v", values)
	}
	if values[telemetry.LyricsProgressFineKey] != "0" || values[telemetry.LyricsProgressLegacyKey] != "0" {
		t.Fatalf("等待歌词时没有清掉上一次进度前景: %#v", values)
	}
}

func TestLyricsValuesRejectsWholeLyricsInHighFrequencyPacket(t *testing.T) {
	_, err := LyricsValues(LyricsUpdate{
		Source: "qq", TrackID: "id", LyricMode: LyricModeTimed,
		Current: strings.Repeat("整", maxLyricLineBytes+1),
	})
	if err == nil || !strings.Contains(err.Error(), "禁止携带整份歌词") {
		t.Fatalf("应拒绝疑似整份歌词的高频包: %v", err)
	}
}

func TestSendSystemDataRejectsOversizedPayloadBeforeWriting(t *testing.T) {
	proxy := &Proxy{}
	err := proxy.SendSystemData(context.Background(), map[string]string{"large": strings.Repeat("x", maxSystemDataPayload)})
	if err == nil || !strings.Contains(err.Error(), "载荷过大") {
		t.Fatalf("超大载荷应在写入前被拒绝: %v", err)
	}
}

func TestDeviceInfoReturnsIndependentCopy(t *testing.T) {
	proxy := &Proxy{deviceInfo: map[string]string{"screen_width": "1024"}}
	values := proxy.DeviceInfo()
	values["screen_width"] = "1"
	if proxy.DeviceInfo()["screen_width"] != "1024" {
		t.Fatal("调用方不应能修改代理内部设备信息")
	}
}
