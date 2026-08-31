package waveshare

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
	"unicode/utf16"
)

var fixedHeader = []byte("AA551234 FIXEDCMDHEAD ")

const packetHeaderSize = 38

// Command 是已从 PCMonitor V2.22 和屏端 PCMonitorLinux 确认的命令号。
type Command uint32

const (
	CommandFindDevice       Command = 0
	CommandSystemData       Command = 1
	CommandReloadTheme      Command = 2
	CommandGetThemes        Command = 3
	CommandSetBrightness    Command = 4
	CommandSetScanState     Command = 5
	CommandFileStart        Command = 6
	CommandFileEnd          Command = 7
	CommandGetVersion       Command = 8
	CommandSetCanvasFlip    Command = 9
	CommandGetScreenMessage Command = 10
	CommandDeleteTheme      Command = 11
)

func (command Command) String() string {
	switch command {
	case CommandFindDevice:
		return "find_device(0)"
	case CommandSystemData:
		return "system_data(1)"
	case CommandReloadTheme:
		return "reload_theme(2)"
	case CommandGetThemes:
		return "get_themes(3)"
	case CommandSetBrightness:
		return "set_brightness(4)"
	case CommandSetScanState:
		return "set_scan_state(5)"
	case CommandFileStart:
		return "file_start(6)"
	case CommandFileEnd:
		return "file_end(7)"
	case CommandGetVersion:
		return "get_version(8)"
	case CommandSetCanvasFlip:
		return "set_canvas_flip(9)"
	case CommandGetScreenMessage:
		return "get_screen_message(10)"
	case CommandDeleteTheme:
		return "delete_theme(11)"
	default:
		return fmt.Sprintf("unknown(%d)", uint32(command))
	}
}

// PacketType 是包头第 22 字节处的类型，而不是序号。
type PacketType uint32

const (
	PacketTypeCommand    PacketType = 0
	PacketTypeFile       PacketType = 1
	PacketTypeCommandAck PacketType = 2
	PacketTypeFileAck    PacketType = 3
)

// Packet 是微雪串口固定命令包。
type Packet struct {
	Type    PacketType
	Command Command
	Payload []byte
}

// EncodePacket 按原厂字节序编码命令包，校验使用 IEEE CRC32。
func EncodePacket(packet Packet) []byte {
	output := make([]byte, packetHeaderSize+len(packet.Payload))
	copy(output, fixedHeader)
	binary.LittleEndian.PutUint32(output[22:26], uint32(packet.Type))
	binary.LittleEndian.PutUint32(output[26:30], uint32(packet.Command))
	binary.LittleEndian.PutUint32(output[30:34], uint32(len(packet.Payload)))
	binary.LittleEndian.PutUint32(output[34:38], crc32.ChecksumIEEE(packet.Payload))
	copy(output[packetHeaderSize:], packet.Payload)
	return output
}

// DecodePacket 严格验证固定头、长度和 CRC32。
func DecodePacket(data []byte) (Packet, error) {
	if len(data) < packetHeaderSize {
		return Packet{}, errors.New("微雪命令包长度不足")
	}
	if !bytes.Equal(data[:len(fixedHeader)], fixedHeader) {
		return Packet{}, errors.New("微雪命令包固定头错误")
	}
	payloadLength := binary.LittleEndian.Uint32(data[30:34])
	if uint64(payloadLength)+packetHeaderSize != uint64(len(data)) {
		return Packet{}, errors.New("微雪命令包载荷长度不一致")
	}
	payload := append([]byte(nil), data[packetHeaderSize:]...)
	if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(data[34:38]) {
		return Packet{}, errors.New("微雪命令包 CRC32 错误")
	}
	return Packet{
		Type:    PacketType(binary.LittleEndian.Uint32(data[22:26])),
		Command: Command(binary.LittleEndian.Uint32(data[26:30])),
		Payload: payload,
	}, nil
}

// EncodeQStringMap 复制原厂的 QMap<QString, QString> 序列化。
// 实际抓包表明：数量、字节长度和 UTF-16 字符均为大端，键按降序写入。
func EncodeQStringMap(values map[string]string) []byte {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))

	var output bytes.Buffer
	_ = binary.Write(&output, binary.BigEndian, uint32(len(keys)))
	for _, key := range keys {
		writeQString(&output, key)
		writeQString(&output, values[key])
	}
	return output.Bytes()
}

// DecodeQStringMap 用于抓包回放和单元测试。
func DecodeQStringMap(data []byte) (map[string]string, error) {
	if len(data) < 4 {
		return nil, errors.New("QString 映射长度不足")
	}
	count := binary.BigEndian.Uint32(data[:4])
	cursor := 4
	values := make(map[string]string, count)
	for index := uint32(0); index < count; index++ {
		key, next, err := readQString(data, cursor)
		if err != nil {
			return nil, err
		}
		value, nextValue, err := readQString(data, next)
		if err != nil {
			return nil, err
		}
		values[key] = value
		cursor = nextValue
	}
	if cursor != len(data) {
		return nil, errors.New("QString 映射尾部存在多余数据")
	}
	return values, nil
}

func writeQString(output *bytes.Buffer, value string) {
	units := utf16.Encode([]rune(value))
	_ = binary.Write(output, binary.BigEndian, uint32(len(units)*2))
	for _, unit := range units {
		_ = binary.Write(output, binary.BigEndian, unit)
	}
}

func readQString(data []byte, cursor int) (string, int, error) {
	if cursor+4 > len(data) {
		return "", cursor, errors.New("QString 长度字段不完整")
	}
	byteLength := binary.BigEndian.Uint32(data[cursor : cursor+4])
	cursor += 4
	if byteLength%2 != 0 || uint64(cursor)+uint64(byteLength) > uint64(len(data)) {
		return "", cursor, errors.New("QString 字节长度无效")
	}
	units := make([]uint16, byteLength/2)
	for index := range units {
		units[index] = binary.BigEndian.Uint16(data[cursor+index*2 : cursor+index*2+2])
	}
	cursor += int(byteLength)
	return string(utf16.Decode(units)), cursor, nil
}

// SystemDataPacket 生成日常的小型监控数据包。
func SystemDataPacket(values map[string]string) []byte {
	return EncodePacket(Packet{Type: PacketTypeCommand, Command: CommandSystemData, Payload: EncodeQStringMap(values)})
}
