package main

import (
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

const (
	fdtMagic     = 0xd00dfeed
	fdtBeginNode = 1
	fdtEndNode   = 2
	fdtProp      = 3
	fdtNop       = 4
	fdtEnd       = 9
)

type fdtHeader struct {
	Magic             uint32
	TotalSize         uint32
	OffDTStruct       uint32
	OffDTStrings      uint32
	OffMemReserveMap  uint32
	Version           uint32
	LastCompVersion   uint32
	BootCPUIDPhysical uint32
	SizeDTStrings     uint32
	SizeDTStruct      uint32
}

func align4(value int) int {
	return (value + 3) &^ 3
}

func readCString(data []byte, offset int) (string, int, error) {
	if offset < 0 || offset >= len(data) {
		return "", offset, fmt.Errorf("字符串偏移越界: %d", offset)
	}
	end := offset
	for end < len(data) && data[end] != 0 {
		end++
	}
	if end >= len(data) {
		return "", offset, fmt.Errorf("字符串缺少 NUL 结尾: %d", offset)
	}
	return string(data[offset:end]), end + 1, nil
}

func propertyText(value []byte) string {
	if len(value) == 0 {
		return "<空>"
	}
	printable := true
	for _, current := range value {
		if current == 0 {
			continue
		}
		if current > unicode.MaxASCII || (!unicode.IsPrint(rune(current)) && current != '\t') {
			printable = false
			break
		}
	}
	if printable && value[len(value)-1] == 0 {
		parts := strings.Split(strings.TrimRight(string(value), "\x00"), "\x00")
		quoted := make([]string, 0, len(parts))
		for _, part := range parts {
			quoted = append(quoted, fmt.Sprintf("%q", part))
		}
		return strings.Join(quoted, ", ")
	}
	if len(value)%4 == 0 && len(value) <= 32 {
		cells := make([]string, 0, len(value)/4)
		for offset := 0; offset < len(value); offset += 4 {
			cells = append(cells, fmt.Sprintf("0x%08x", binary.BigEndian.Uint32(value[offset:offset+4])))
		}
		return "<" + strings.Join(cells, " ") + ">"
	}
	preview := value
	if len(preview) > 48 {
		preview = preview[:48]
	}
	suffix := ""
	if len(preview) != len(value) {
		suffix = fmt.Sprintf("… (%d bytes)", len(value))
	}
	return hex.EncodeToString(preview) + suffix
}

func main() {
	matchRaw := flag.String("match", "", "只输出路径或属性名匹配该正则的项目")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "用法: go run tools/inspect_fdt.go [-match 正则] <文件.dtb>\n")
		os.Exit(2)
	}

	path := flag.Arg(0)
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取 %s: %v\n", path, err)
		os.Exit(1)
	}
	if len(data) < 40 {
		fmt.Fprintln(os.Stderr, "DTB 小于 40 字节")
		os.Exit(1)
	}
	header := fdtHeader{
		Magic:             binary.BigEndian.Uint32(data[0:4]),
		TotalSize:         binary.BigEndian.Uint32(data[4:8]),
		OffDTStruct:       binary.BigEndian.Uint32(data[8:12]),
		OffDTStrings:      binary.BigEndian.Uint32(data[12:16]),
		OffMemReserveMap:  binary.BigEndian.Uint32(data[16:20]),
		Version:           binary.BigEndian.Uint32(data[20:24]),
		LastCompVersion:   binary.BigEndian.Uint32(data[24:28]),
		BootCPUIDPhysical: binary.BigEndian.Uint32(data[28:32]),
		SizeDTStrings:     binary.BigEndian.Uint32(data[32:36]),
		SizeDTStruct:      binary.BigEndian.Uint32(data[36:40]),
	}
	if header.Magic != fdtMagic || int(header.TotalSize) > len(data) {
		fmt.Fprintf(os.Stderr, "无效 DTB：magic=0x%08x total=%d file=%d\n", header.Magic, header.TotalSize, len(data))
		os.Exit(1)
	}
	var filter *regexp.Regexp
	if strings.TrimSpace(*matchRaw) != "" {
		filter, err = regexp.Compile("(?i)" + *matchRaw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "无效正则: %v\n", err)
			os.Exit(2)
		}
	}

	structStart := int(header.OffDTStruct)
	structEnd := structStart + int(header.SizeDTStruct)
	stringsStart := int(header.OffDTStrings)
	stringsEnd := stringsStart + int(header.SizeDTStrings)
	if structStart < 0 || structEnd > len(data) || stringsStart < 0 || stringsEnd > len(data) {
		fmt.Fprintln(os.Stderr, "DTB 区段越界")
		os.Exit(1)
	}
	structBlock := data[structStart:structEnd]
	stringsBlock := data[stringsStart:stringsEnd]
	fmt.Printf("文件: %s\n", filepath.Clean(path))
	fmt.Printf("总大小: %d，版本: %d，结构区: %d，字符串区: %d\n", header.TotalSize, header.Version, header.SizeDTStruct, header.SizeDTStrings)

	stack := make([]string, 0, 16)
	currentPath := "/"
	for cursor := 0; cursor+4 <= len(structBlock); {
		token := binary.BigEndian.Uint32(structBlock[cursor : cursor+4])
		cursor += 4
		switch token {
		case fdtBeginNode:
			name, next, readErr := readCString(structBlock, cursor)
			if readErr != nil {
				fmt.Fprintln(os.Stderr, readErr)
				os.Exit(1)
			}
			cursor = align4(next)
			stack = append(stack, name)
			parts := make([]string, 0, len(stack))
			for _, part := range stack {
				if part != "" {
					parts = append(parts, part)
				}
			}
			currentPath = "/" + strings.Join(parts, "/")
			if filter == nil || filter.MatchString(currentPath) {
				fmt.Printf("\n[%s]\n", currentPath)
			}
		case fdtEndNode:
			if len(stack) == 0 {
				fmt.Fprintln(os.Stderr, "节点栈下溢")
				os.Exit(1)
			}
			stack = stack[:len(stack)-1]
			parts := make([]string, 0, len(stack))
			for _, part := range stack {
				if part != "" {
					parts = append(parts, part)
				}
			}
			currentPath = "/" + strings.Join(parts, "/")
		case fdtProp:
			if cursor+8 > len(structBlock) {
				fmt.Fprintln(os.Stderr, "属性头越界")
				os.Exit(1)
			}
			length := int(binary.BigEndian.Uint32(structBlock[cursor : cursor+4]))
			nameOffset := int(binary.BigEndian.Uint32(structBlock[cursor+4 : cursor+8]))
			cursor += 8
			if length < 0 || cursor+length > len(structBlock) {
				fmt.Fprintln(os.Stderr, "属性值越界")
				os.Exit(1)
			}
			name, _, readErr := readCString(stringsBlock, nameOffset)
			if readErr != nil {
				fmt.Fprintln(os.Stderr, readErr)
				os.Exit(1)
			}
			value := structBlock[cursor : cursor+length]
			cursor = align4(cursor + length)
			if filter == nil || filter.MatchString(currentPath) || filter.MatchString(name) || filter.MatchString(propertyText(value)) {
				fmt.Printf("%s: %s = %s\n", currentPath, name, propertyText(value))
			}
		case fdtNop:
		case fdtEnd:
			return
		default:
			fmt.Fprintf(os.Stderr, "未知 FDT token 0x%08x，结构偏移 0x%x\n", token, cursor-4)
			os.Exit(1)
		}
	}
}
