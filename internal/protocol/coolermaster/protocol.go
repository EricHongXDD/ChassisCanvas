package coolermaster

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	ReportSize        = 1024
	RequestReportID   = 0x1E
	DataReportID      = 0x1F
	ResponseReportID  = 0x20
	dataPacketMarker  = 0x5C
	DataChunkSize     = 1000
	DataPayloadOffset = 24
	frameMarker       = 0x5A
	escapeMarker      = 0x5B
	escapeFrameMarker = 0x01
	escapeEscape      = 0x02
	OfflineCommandSeq = 0x2A
	// 原厂 SendKeepAlive 的 timestamp 与 realtimeDisplay 两条命令
	// 都固定使用 SeqNumber=42，设备逐条返回 AckNumber=43。
	RealtimeKeepaliveCommandSeq = OfflineCommandSeq
	// 原厂 LCDATMOSV2SSRPlayer::SSRPostCommand 对 delMedia、
	// transport、transported 和最终 realtimeDisplay 都固定写入
	// SeqNumber=42。真机可以给任意序号回 200，但并不保证执行
	// 离线文件替换，因此离线链路必须保持原厂序号。
	TransportedCommandSeq = OfflineCommandSeq
	OfflineDisplayModeSeq = OfflineCommandSeq
	SetDisplayModeSeq     = 0x2B
	BrightnessCommandSeq  = 0x2C
	BrightnessMin         = 0
	BrightnessMax         = 100
	DefaultBrightness     = 80
	TransportedMD5        = 1111111
	// 原厂 FlashUpdate 的数据包第 10 字节不是“离线/实时”标志，
	// 而是媒体编码：MP4 写 0，JPG（包括实时 JPEG）写 1。
	MediaDataMP4  byte = 0
	MediaDataJPEG byte = 1
)

type Envelope struct {
	StartLine string
	Headers   map[string]string
	Body      []byte
}

type Response struct {
	Envelope Envelope
	JSON     map[string]any
}

type MediaType string

const (
	MediaSuspend MediaType = "suspend"
	MediaLogo    MediaType = "logo"
)

func DeleteMediaPayload(mediaType MediaType, fileName string) map[string]any {
	return map[string]any{
		"type":     string(mediaType),
		"fileName": fileName,
	}
}

func TransportPayload(mediaType MediaType, fileName string, fileSize int) map[string]any {
	return map[string]any{
		"type":     string(mediaType),
		"fileName": fileName,
		"fileSize": fileSize,
	}
}

func TransportedPayload(fileName string) map[string]any {
	return map[string]any{
		"fileName": fileName,
		"md5":      TransportedMD5,
	}
}

// BrightnessPayload 对齐原厂 ATMOS V2 的 brightness 命令。
// 原厂能力清单与滑块都把有效范围限定为 0..100。
func BrightnessPayload(value int) (map[string]int, error) {
	if value < BrightnessMin || value > BrightnessMax {
		return nil, fmt.Errorf("酷冷至尊亮度必须在 %d..%d 之间", BrightnessMin, BrightnessMax)
	}
	return map[string]int{"value": value}, nil
}

// RealtimeKeepaliveTimestampPayload 对齐原厂以 Unix 毫秒发送的
// timestamp JSON；请求头 Date 仍由 BuildRequest 写入 Unix 秒。
func RealtimeKeepaliveTimestampPayload(capturedAt time.Time) map[string]int64 {
	if capturedAt.IsZero() {
		capturedAt = time.Now()
	}
	return map[string]int64{"timestamp": capturedAt.UnixMilli()}
}

// BuildDataReports 按原厂协议把资源或实时 JPEG 拆成 1000 字节数据块。
// transferID 对齐原厂每次传输随机生成的 0～59；mediaDataKind
// 必须按媒体编码填写，不能用离线/实时模式代替。
func BuildDataReports(data []byte, transferID, mediaDataKind byte) ([][]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("酷冷离线资源不能为空")
	}
	if transferID > 59 {
		return nil, fmt.Errorf("酷冷传输 ID 超出原厂范围: %d", transferID)
	}
	if mediaDataKind != MediaDataMP4 && mediaDataKind != MediaDataJPEG {
		return nil, fmt.Errorf("酷冷媒体数据类型无效: %d", mediaDataKind)
	}
	total := (len(data) + DataChunkSize - 1) / DataChunkSize
	if total > int(^uint16(0)) {
		return nil, fmt.Errorf("酷冷离线资源数据块过多: %d", total)
	}
	reports := make([][]byte, 0, total)
	for index, offset := 0, 0; offset < len(data); index, offset = index+1, offset+DataChunkSize {
		end := min(offset+DataChunkSize, len(data))
		chunk := data[offset:end]
		report := make([]byte, ReportSize)
		report[0] = DataReportID
		report[1] = dataPacketMarker
		binary.BigEndian.PutUint16(report[2:4], uint16(len(chunk)+20))
		report[4] = transferID
		binary.BigEndian.PutUint16(report[5:7], uint16(total))
		binary.BigEndian.PutUint16(report[7:9], uint16(index))
		report[9] = mediaDataKind
		copy(report[DataPayloadOffset:], chunk)
		reports = append(reports, report)
	}
	return reports, nil
}

// MarshalCompactJSON 对齐原厂 nlohmann::json::dump() 的紧凑输出方式。
func MarshalCompactJSON(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(output.Bytes(), []byte{'\n'}), nil
}

func BuildRequest(action string, sequence int, capturedAt time.Time, body any) ([]byte, error) {
	encodedBody, err := MarshalCompactJSON(body)
	if err != nil {
		return nil, fmt.Errorf("编码酷冷 JSON: %w", err)
	}
	return BuildRequestJSON(action, sequence, capturedAt, encodedBody)
}

func BuildRequestJSON(action string, sequence int, capturedAt time.Time, body []byte) ([]byte, error) {
	if action == "" || strings.ContainsAny(action, " \t\r\n") {
		return nil, errors.New("酷冷命令名称无效")
	}
	if sequence < 0 {
		return nil, errors.New("酷冷命令序号不能为负数")
	}
	if !json.Valid(body) {
		return nil, errors.New("酷冷命令正文不是有效 JSON")
	}
	if capturedAt.IsZero() {
		capturedAt = time.Now()
	}
	payload := []byte(fmt.Sprintf(
		"POST %s 1\r\nSeqNumber=%d\r\nDate=%d\r\nContentType=json\r\nContentLength=%d\r\n\r\n%s",
		action, sequence, capturedAt.Unix(), len(body), body,
	))
	return encodeReport(RequestReportID, payload)
}

func BuildParamRequest() ([]byte, error) {
	return encodeReport(RequestReportID, []byte("POST param 1"))
}

func DecodeResponse(report []byte) (Response, error) {
	payload, err := decodeReport(ResponseReportID, report)
	if err != nil {
		return Response{}, err
	}
	envelope, err := ParseEnvelope(payload)
	if err != nil {
		return Response{}, err
	}
	value := make(map[string]any)
	if len(envelope.Body) > 0 {
		if err := json.Unmarshal(envelope.Body, &value); err != nil {
			return Response{Envelope: envelope}, fmt.Errorf(
				"解析酷冷响应 JSON: %w（起始行=%q，头=%v，正文=%q）",
				err, envelope.StartLine, envelope.Headers, envelope.Body,
			)
		}
	}
	for key, text := range envelope.Headers {
		if _, exists := value[key]; exists {
			continue
		}
		if number, err := strconv.ParseInt(text, 10, 64); err == nil {
			value[key] = number
		} else {
			value[key] = text
		}
	}
	if _, exists := value["response"]; !exists {
		fields := strings.Fields(envelope.StartLine)
		if len(fields) >= 2 {
			if status, err := strconv.ParseInt(fields[len(fields)-1], 10, 64); err == nil {
				value["response"] = status
			}
		}
	}
	return Response{Envelope: envelope, JSON: value}, nil
}

func ParseEnvelope(payload []byte) (Envelope, error) {
	headerEnd := bytes.Index(payload, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		return Envelope{}, errors.New("酷冷消息缺少头部结束标记")
	}
	lines := strings.Split(string(payload[:headerEnd]), "\r\n")
	if len(lines) == 0 || lines[0] == "" {
		return Envelope{}, errors.New("酷冷消息起始行为空")
	}
	envelope := Envelope{StartLine: lines[0], Headers: make(map[string]string)}
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return Envelope{}, fmt.Errorf("酷冷消息头无效: %q", line)
		}
		envelope.Headers[key] = value
	}
	body := payload[headerEnd+4:]
	if text := envelope.Headers["ContentLength"]; text != "" {
		length, err := strconv.Atoi(text)
		if err != nil || length < 0 || length > len(body) {
			return Envelope{}, fmt.Errorf("酷冷 ContentLength 无效: %q", text)
		}
		body = body[:length]
	}
	envelope.Body = append([]byte(nil), body...)
	return envelope, nil
}

func ValidateAck(response Response, sequence int) error {
	ack, ok := integerField(response.JSON, "AckNumber")
	if !ok || ack != int64(sequence+1) {
		return fmt.Errorf("酷冷 ACK 序号无效: got=%v want=%d", response.JSON["AckNumber"], sequence+1)
	}
	status, ok := integerField(response.JSON, "response")
	if !ok || status != 200 {
		return fmt.Errorf("酷冷响应状态无效: %v", response.JSON["response"])
	}
	return nil
}

func ValidateStatus(response Response) error {
	status, ok := integerField(response.JSON, "response")
	if !ok || status != 200 {
		return fmt.Errorf("酷冷响应状态无效: %v", response.JSON["response"])
	}
	return nil
}

func encodeReport(reportID byte, payload []byte) ([]byte, error) {
	if len(payload)+5 > int(^uint16(0)) {
		return nil, errors.New("酷冷消息超过 16 位帧长度")
	}
	frameLength := len(payload) + 5
	inner := make([]byte, 0, frameLength)
	inner = append(inner, frameMarker, byte(frameLength>>8), byte(frameLength))
	inner = append(inner, payload...)
	checksum := sum8(inner[1:])
	inner = append(inner, checksum, frameMarker)
	escaped := escapeFrame(inner)
	if len(escaped)+1 > ReportSize {
		return nil, fmt.Errorf("酷冷消息编码后超过 %d 字节: %d", ReportSize, len(escaped)+1)
	}
	report := make([]byte, ReportSize)
	report[0] = reportID
	copy(report[1:], escaped)
	return report, nil
}

func decodeReport(expectedID byte, report []byte) ([]byte, error) {
	if len(report) < 7 {
		return nil, errors.New("酷冷报告过短")
	}
	if report[0] != expectedID {
		return nil, fmt.Errorf("酷冷报告 ID 无效: got=0x%02X want=0x%02X", report[0], expectedID)
	}
	if report[1] != frameMarker {
		return nil, errors.New("酷冷报告缺少起始标记")
	}
	end := findFrameEnd(report[1:])
	if end < 0 {
		return nil, errors.New("酷冷报告缺少结束标记")
	}
	inner, err := unescapeFrame(report[1 : end+2])
	if err != nil {
		return nil, err
	}
	if len(inner) < 5 || inner[0] != frameMarker || inner[len(inner)-1] != frameMarker {
		return nil, errors.New("酷冷帧边界无效")
	}
	declared := int(binary.BigEndian.Uint16(inner[1:3]))
	if declared != len(inner) {
		return nil, fmt.Errorf("酷冷帧长度无效: got=%d want=%d", len(inner), declared)
	}
	checksumIndex := len(inner) - 2
	if calculated := sum8(inner[1:checksumIndex]); inner[checksumIndex] != calculated {
		return nil, fmt.Errorf("酷冷帧校验和无效: got=0x%02X want=0x%02X", inner[checksumIndex], calculated)
	}
	return append([]byte(nil), inner[3:checksumIndex]...), nil
}

func escapeFrame(frame []byte) []byte {
	if len(frame) < 2 {
		return append([]byte(nil), frame...)
	}
	output := make([]byte, 0, len(frame))
	output = append(output, frame[0])
	for _, value := range frame[1 : len(frame)-1] {
		switch value {
		case frameMarker:
			output = append(output, escapeMarker, escapeFrameMarker)
		case escapeMarker:
			output = append(output, escapeMarker, escapeEscape)
		default:
			output = append(output, value)
		}
	}
	return append(output, frame[len(frame)-1])
}

func unescapeFrame(frame []byte) ([]byte, error) {
	if len(frame) < 2 {
		return nil, errors.New("酷冷转义帧过短")
	}
	output := make([]byte, 0, len(frame))
	output = append(output, frame[0])
	for index := 1; index < len(frame)-1; index++ {
		value := frame[index]
		if value != escapeMarker {
			output = append(output, value)
			continue
		}
		if index+1 >= len(frame)-1 {
			return nil, errors.New("酷冷帧包含不完整转义")
		}
		index++
		switch frame[index] {
		case escapeFrameMarker:
			output = append(output, frameMarker)
		case escapeEscape:
			output = append(output, escapeMarker)
		default:
			return nil, fmt.Errorf("酷冷帧包含未知转义: 0x%02X", frame[index])
		}
	}
	return append(output, frame[len(frame)-1]), nil
}

func findFrameEnd(frame []byte) int {
	for index := 1; index < len(frame); index++ {
		if frame[index] == escapeMarker {
			index++
			continue
		}
		if frame[index] == frameMarker {
			return index
		}
	}
	return -1
}

func sum8(data []byte) byte {
	var result byte
	for _, value := range data {
		result += value
	}
	return result
}

func integerField(value map[string]any, key string) (int64, bool) {
	switch number := value[key].(type) {
	case float64:
		if number != float64(int64(number)) {
			return 0, false
		}
		return int64(number), true
	case int:
		return int64(number), true
	case int64:
		return number, true
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}
