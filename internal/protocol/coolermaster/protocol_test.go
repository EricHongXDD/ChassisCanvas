package coolermaster

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

func TestBuildDisplayModeRequest(t *testing.T) {
	report, err := BuildRequest(
		"realtimeDisplay",
		SetDisplayModeSeq,
		time.Unix(1_700_000_000, 0),
		map[string]bool{"enable": true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != ReportSize || report[0] != RequestReportID || report[1] != frameMarker {
		t.Fatalf("报告头无效: %s", hex.EncodeToString(report[:8]))
	}
	payload, err := decodeReport(RequestReportID, report)
	if err != nil {
		t.Fatal(err)
	}
	want := "POST realtimeDisplay 1\r\nSeqNumber=43\r\nDate=1700000000\r\nContentType=json\r\nContentLength=15\r\n\r\n{\"enable\":true}"
	if string(payload) != want {
		t.Fatalf("请求正文不同\n got: %q\nwant: %q", payload, want)
	}
	if !bytes.Equal(report[len(report)-16:], make([]byte, 16)) {
		t.Fatal("固定长度报告尾部没有补零")
	}
}

func TestBuildOriginalRealtimeKeepaliveRequests(t *testing.T) {
	capturedAt := time.Unix(1_700_000_000, 123_000_000)
	timestampReport, err := BuildRequest(
		"timestamp",
		RealtimeKeepaliveCommandSeq,
		capturedAt,
		RealtimeKeepaliveTimestampPayload(capturedAt),
	)
	if err != nil {
		t.Fatal(err)
	}
	timestampPayload, err := decodeReport(RequestReportID, timestampReport)
	if err != nil {
		t.Fatal(err)
	}
	wantTimestamp := "POST timestamp 1\r\nSeqNumber=42\r\nDate=1700000000\r\nContentType=json\r\nContentLength=27\r\n\r\n{\"timestamp\":1700000000123}"
	if string(timestampPayload) != wantTimestamp {
		t.Fatalf("timestamp 保活请求不同\n got: %q\nwant: %q", timestampPayload, wantTimestamp)
	}

	modeReport, err := BuildRequest(
		"realtimeDisplay",
		RealtimeKeepaliveCommandSeq,
		capturedAt,
		map[string]bool{"enable": true},
	)
	if err != nil {
		t.Fatal(err)
	}
	modePayload, err := decodeReport(RequestReportID, modeReport)
	if err != nil {
		t.Fatal(err)
	}
	wantMode := "POST realtimeDisplay 1\r\nSeqNumber=42\r\nDate=1700000000\r\nContentType=json\r\nContentLength=15\r\n\r\n{\"enable\":true}"
	if string(modePayload) != wantMode {
		t.Fatalf("realtimeDisplay 保活请求不同\n got: %q\nwant: %q", modePayload, wantMode)
	}
	ack := Response{JSON: map[string]any{"AckNumber": float64(43), "response": float64(200)}}
	if err := ValidateAck(ack, RealtimeKeepaliveCommandSeq); err != nil {
		t.Fatalf("原厂保活 ACK 43 未通过: %v", err)
	}
}

func TestOfflineReplacementStagesUseOriginalSequence(t *testing.T) {
	if TransportedCommandSeq != OfflineCommandSeq || OfflineDisplayModeSeq != OfflineCommandSeq {
		t.Fatal("离线替换阶段没有统一使用原厂 SeqNumber=42")
	}
	report, err := BuildRequest(
		"transported",
		TransportedCommandSeq,
		time.Unix(1_700_000_000, 0),
		TransportedPayload("suspend_4.jpg"),
	)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := decodeReport(RequestReportID, report)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte("SeqNumber=42")) {
		t.Fatalf("transported 未使用原厂 seq 0x2A: %q", payload)
	}
	commit := Response{JSON: map[string]any{"AckNumber": float64(43), "response": float64(200)}}
	if err := ValidateAck(commit, TransportedCommandSeq); err != nil {
		t.Fatalf("transported 原厂 ACK 43 未通过: %v", err)
	}
}

func TestBuildBrightnessRequest(t *testing.T) {
	payload, err := BrightnessPayload(0)
	if err != nil {
		t.Fatal(err)
	}
	report, err := BuildRequest(
		"brightness",
		BrightnessCommandSeq,
		time.Unix(1_700_000_000, 0),
		payload,
	)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeReport(RequestReportID, report)
	if err != nil {
		t.Fatal(err)
	}
	want := "POST brightness 1\r\nSeqNumber=44\r\nDate=1700000000\r\nContentType=json\r\nContentLength=11\r\n\r\n{\"value\":0}"
	if string(decoded) != want {
		t.Fatalf("亮度请求不同\n got: %q\nwant: %q", decoded, want)
	}
	for _, value := range []int{-1, 101} {
		if _, err := BrightnessPayload(value); err == nil {
			t.Fatalf("越界亮度 %d 被接受", value)
		}
	}
}

func TestFrameEscapingRoundTrip(t *testing.T) {
	payload := []byte{'A', frameMarker, 'B', escapeMarker, 'C'}
	report, err := encodeReport(ResponseReportID, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(report, []byte{escapeMarker, escapeFrameMarker}) ||
		!bytes.Contains(report, []byte{escapeMarker, escapeEscape}) {
		t.Fatalf("没有生成预期转义: %x", report[:20])
	}
	decoded, err := decodeReport(ResponseReportID, report)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("转义往返失败: %x != %x", decoded, payload)
	}
}

func TestDecodeResponseAndValidateAck(t *testing.T) {
	body := []byte(`{"AckNumber":44,"response":200,"enable":true}`)
	header := fmt.Sprintf("POST realtimeDisplay 1\r\nContentType=json\r\nContentLength=%d\r\n\r\n", len(body))
	payload := append([]byte(header), body...)
	report, err := encodeReport(ResponseReportID, payload)
	if err != nil {
		t.Fatal(err)
	}
	response, err := DecodeResponse(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAck(response, SetDisplayModeSeq); err != nil {
		t.Fatal(err)
	}
	if response.Envelope.StartLine != "POST realtimeDisplay 1" {
		t.Fatalf("起始行不同: %q", response.Envelope.StartLine)
	}
}

func TestDecodeBrightnessAck(t *testing.T) {
	report, err := encodeReport(ResponseReportID, []byte("1 200\r\nAckNumber=45\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := DecodeResponse(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAck(response, BrightnessCommandSeq); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeResponseRejectsChecksumDamage(t *testing.T) {
	report, err := encodeReport(ResponseReportID, []byte("x\r\n\r\n{}"))
	if err != nil {
		t.Fatal(err)
	}
	report[7] ^= 0x01
	if _, err := DecodeResponse(report); err == nil {
		t.Fatal("损坏校验和后仍被接受")
	}
}

func TestOfflineCommandPayloads(t *testing.T) {
	tests := []struct {
		action string
		body   any
		want   string
	}{
		{"delMedia", DeleteMediaPayload(MediaSuspend, "suspend_2.jpg"), `{"fileName":"suspend_2.jpg","type":"suspend"}`},
		{"transport", TransportPayload(MediaSuspend, "suspend_2.jpg", 12345), `{"fileName":"suspend_2.jpg","fileSize":12345,"type":"suspend"}`},
		{"transported", TransportedPayload("suspend_2.jpg"), `{"fileName":"suspend_2.jpg","md5":1111111}`},
		{"transport", TransportPayload(MediaSuspend, "suspend_4.jpg", 6789), `{"fileName":"suspend_4.jpg","fileSize":6789,"type":"suspend"}`},
	}
	for _, test := range tests {
		report, err := BuildRequest(test.action, OfflineCommandSeq, time.Unix(1_700_000_000, 0), test.body)
		if err != nil {
			t.Fatalf("%s: %v", test.action, err)
		}
		payload, err := decodeReport(RequestReportID, report)
		if err != nil {
			t.Fatalf("%s: %v", test.action, err)
		}
		if !bytes.HasSuffix(payload, []byte(test.want)) {
			t.Fatalf("%s JSON 不同: %q", test.action, payload)
		}
	}
}

func TestBuildDataReports(t *testing.T) {
	data := make([]byte, 2001)
	for index := range data {
		data[index] = byte(index)
	}
	reports, err := BuildDataReports(data, 23, MediaDataMP4)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 3 {
		t.Fatalf("数据块数量不同: %d", len(reports))
	}
	for index, report := range reports {
		if len(report) != ReportSize || report[0] != DataReportID || report[1] != dataPacketMarker {
			t.Fatalf("第 %d 块报告头无效: %x", index, report[:10])
		}
		if got := int(report[5])<<8 | int(report[6]); got != 3 {
			t.Fatalf("第 %d 块总数无效: %d", index, got)
		}
		if got := int(report[7])<<8 | int(report[8]); got != index {
			t.Fatalf("第 %d 块序号无效: %d", index, got)
		}
		if report[9] != MediaDataMP4 {
			t.Fatalf("第 %d 块媒体类型无效: %d", index, report[9])
		}
	}
	if got := int(reports[0][2])<<8 | int(reports[0][3]); got != 1020 {
		t.Fatalf("首块长度字段无效: %d", got)
	}
	if got := int(reports[2][2])<<8 | int(reports[2][3]); got != 21 {
		t.Fatalf("尾块长度字段无效: %d", got)
	}
	if !bytes.Equal(reports[0][DataPayloadOffset:], data[:DataChunkSize]) {
		t.Fatal("首块资源数据不同")
	}
	if reports[2][DataPayloadOffset] != data[2000] {
		t.Fatal("尾块资源数据不同")
	}
	if !bytes.Equal(reports[2][DataPayloadOffset+1:], make([]byte, ReportSize-DataPayloadOffset-1)) {
		t.Fatal("尾块没有补零")
	}
	jpegReports, err := BuildDataReports(data[:1], 23, MediaDataJPEG)
	if err != nil || len(jpegReports) != 1 || jpegReports[0][9] != MediaDataJPEG {
		t.Fatalf("JPG 媒体类型没有写入数据报告: reports=%d err=%v", len(jpegReports), err)
	}
	if _, err := BuildDataReports(data[:1], 23, 2); err == nil {
		t.Fatal("接受了原厂未定义的媒体数据类型")
	}
}

func TestBuildParamRequest(t *testing.T) {
	report, err := BuildParamRequest()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := decodeReport(RequestReportID, report)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "POST param 1" {
		t.Fatalf("param 请求不同: %q", payload)
	}
}

func TestDecodeHeaderOnlyResponse(t *testing.T) {
	report, err := encodeReport(ResponseReportID, []byte("1 200\r\nAckNumber=44\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := DecodeResponse(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAck(response, SetDisplayModeSeq); err != nil {
		t.Fatal(err)
	}
}
