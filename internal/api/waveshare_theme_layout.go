package api

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"unicode/utf16"
)

const waveshareThemeHeader = "Author:SYKVersion: V2.0.0Description: NULL"

type waveshareThemeWidget struct {
	start  int
	end    int
	fields map[string]string
	order  []string
}

type waveshareThemeWidgetTable struct {
	countOffset int
	tailOffset  int
	canvasStart int
	canvasEnd   int
	canvas      map[string]string
	canvasOrder []string
	widgets     []waveshareThemeWidget
}

// filterWaveshareThemeWidgets 只改写主题控件表，尾部图片、音频和视频资源保持原样。
func filterWaveshareThemeWidgets(data []byte, keep func(map[string]string) bool) ([]byte, error) {
	if len(data) < len(waveshareThemeHeader)+4 || string(data[:len(waveshareThemeHeader)]) != waveshareThemeHeader {
		return nil, errors.New("微雪主题头无效")
	}
	cursor := len(waveshareThemeHeader)
	if err := skipWaveshareVariantMap(data, &cursor); err != nil {
		return nil, fmt.Errorf("解析主题设置: %w", err)
	}
	if _, _, err := readWaveshareStringMap(data, &cursor); err != nil {
		return nil, fmt.Errorf("解析主题画布: %w", err)
	}
	countOffset := cursor
	widgetCount, err := readWaveshareUint32(data, &cursor)
	if err != nil {
		return nil, fmt.Errorf("读取主题控件数量: %w", err)
	}
	widgets := make([]waveshareThemeWidget, 0, widgetCount)
	for index := uint32(0); index < widgetCount; index++ {
		start := cursor
		fields, order, end, parseErr := readWaveshareStringMapOrdered(data, &cursor)
		if parseErr != nil {
			return nil, fmt.Errorf("解析主题控件 %d: %w", index, parseErr)
		}
		widgets = append(widgets, waveshareThemeWidget{start: start, end: end, fields: fields, order: order})
	}
	kept := make([]waveshareThemeWidget, 0, len(widgets))
	for _, widget := range widgets {
		if keep == nil || keep(widget.fields) {
			kept = append(kept, widget)
		}
	}
	result := make([]byte, 0, len(data))
	result = append(result, data[:countOffset]...)
	var encodedCount [4]byte
	binary.BigEndian.PutUint32(encodedCount[:], uint32(len(kept)))
	result = append(result, encodedCount[:]...)
	for _, widget := range kept {
		result = append(result, data[widget.start:widget.end]...)
	}
	result = append(result, data[cursor:]...)
	return result, nil
}

func parseWaveshareThemeWidgetTable(data []byte) (waveshareThemeWidgetTable, error) {
	if len(data) < len(waveshareThemeHeader)+4 || string(data[:len(waveshareThemeHeader)]) != waveshareThemeHeader {
		return waveshareThemeWidgetTable{}, errors.New("微雪主题头无效")
	}
	cursor := len(waveshareThemeHeader)
	if err := skipWaveshareVariantMap(data, &cursor); err != nil {
		return waveshareThemeWidgetTable{}, fmt.Errorf("解析主题设置: %w", err)
	}
	canvasStart := cursor
	canvas, canvasOrder, canvasEnd, err := readWaveshareStringMapOrdered(data, &cursor)
	if err != nil {
		return waveshareThemeWidgetTable{}, fmt.Errorf("解析主题画布: %w", err)
	}
	table := waveshareThemeWidgetTable{countOffset: cursor, canvasStart: canvasStart, canvasEnd: canvasEnd, canvas: canvas, canvasOrder: canvasOrder}
	count, err := readWaveshareUint32(data, &cursor)
	if err != nil {
		return waveshareThemeWidgetTable{}, fmt.Errorf("读取主题控件数量: %w", err)
	}
	table.widgets = make([]waveshareThemeWidget, 0, count)
	for index := uint32(0); index < count; index++ {
		start := cursor
		fields, order, end, parseErr := readWaveshareStringMapOrdered(data, &cursor)
		if parseErr != nil {
			return waveshareThemeWidgetTable{}, fmt.Errorf("解析主题控件 %d: %w", index, parseErr)
		}
		table.widgets = append(table.widgets, waveshareThemeWidget{start: start, end: end, fields: fields, order: order})
	}
	table.tailOffset = cursor
	return table, nil
}

// rewriteWaveshareThemeCanvas 让同一份 V2 控件模板可用于 7 英寸横屏与
// 8.8 英寸 480×1920 竖屏。屏端会读取这里的实际画布尺寸，而不是只看背景视频。
func rewriteWaveshareThemeCanvas(data []byte, width, height int) ([]byte, error) {
	if width <= 0 || height <= 0 {
		return nil, errors.New("微雪主题画布尺寸无效")
	}
	table, err := parseWaveshareThemeWidgetTable(data)
	if err != nil {
		return nil, err
	}
	canvas := make(map[string]string, len(table.canvas)+4)
	for key, value := range table.canvas {
		canvas[key] = value
	}
	canvas["canvas_w"] = fmt.Sprintf("%d", width)
	canvas["canvas_h"] = fmt.Sprintf("%d", height)
	if _, ok := canvas["canvas_rotate"]; !ok {
		canvas["canvas_rotate"] = "0"
	}
	if _, ok := canvas["canvas_flip"]; !ok {
		canvas["canvas_flip"] = "0"
	}
	var output bytes.Buffer
	output.Grow(len(data) + 32)
	output.Write(data[:table.canvasStart])
	writeWaveshareStringMap(&output, canvas, table.canvasOrder)
	output.Write(data[table.canvasEnd:])
	return output.Bytes(), nil
}

// rewriteWaveshareThemeWidgets 重写控件表，但保留主题设置、画布以及尾部媒体资源。
func rewriteWaveshareThemeWidgets(data []byte, widgets []waveshareThemeWidget) ([]byte, error) {
	table, err := parseWaveshareThemeWidgetTable(data)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	output.Grow(len(data) + len(widgets)*256)
	output.Write(data[:table.countOffset])
	writeWaveshareUint32(&output, uint32(len(widgets)))
	for _, widget := range widgets {
		writeWaveshareStringMap(&output, widget.fields, widget.order)
	}
	output.Write(data[table.tailOffset:])
	return output.Bytes(), nil
}

func cloneWaveshareThemeWidget(source waveshareThemeWidget) waveshareThemeWidget {
	fields := make(map[string]string, len(source.fields))
	for key, value := range source.fields {
		fields[key] = value
	}
	return waveshareThemeWidget{fields: fields, order: append([]string(nil), source.order...)}
}

func readWaveshareStringMap(data []byte, cursor *int) (map[string]string, int, error) {
	fields, _, end, err := readWaveshareStringMapOrdered(data, cursor)
	return fields, end, err
}

func readWaveshareStringMapOrdered(data []byte, cursor *int) (map[string]string, []string, int, error) {
	count, err := readWaveshareUint32(data, cursor)
	if err != nil {
		return nil, nil, *cursor, err
	}
	fields := make(map[string]string, count)
	order := make([]string, 0, count)
	for index := uint32(0); index < count; index++ {
		key, readErr := readWaveshareQString(data, cursor)
		if readErr != nil {
			return nil, nil, *cursor, readErr
		}
		value, readErr := readWaveshareQString(data, cursor)
		if readErr != nil {
			return nil, nil, *cursor, readErr
		}
		fields[key] = value
		order = append(order, key)
	}
	return fields, order, *cursor, nil
}

func writeWaveshareStringMap(output *bytes.Buffer, fields map[string]string, order []string) {
	keys := make([]string, 0, len(fields))
	written := make(map[string]bool, len(fields))
	for _, key := range order {
		if _, ok := fields[key]; ok && !written[key] {
			keys = append(keys, key)
			written[key] = true
		}
	}
	extra := make([]string, 0, len(fields)-len(keys))
	for key := range fields {
		if !written[key] {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	keys = append(keys, extra...)
	writeWaveshareUint32(output, uint32(len(keys)))
	for _, key := range keys {
		writeWaveshareQString(output, key)
		writeWaveshareQString(output, fields[key])
	}
}

func writeWaveshareQString(output *bytes.Buffer, value string) {
	units := utf16.Encode([]rune(value))
	writeWaveshareUint32(output, uint32(len(units)*2))
	for _, unit := range units {
		var encoded [2]byte
		binary.BigEndian.PutUint16(encoded[:], unit)
		output.Write(encoded[:])
	}
}

func writeWaveshareUint32(output *bytes.Buffer, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	output.Write(encoded[:])
}

func skipWaveshareVariantMap(data []byte, cursor *int) error {
	count, err := readWaveshareUint32(data, cursor)
	if err != nil {
		return err
	}
	for index := uint32(0); index < count; index++ {
		if _, err := readWaveshareQString(data, cursor); err != nil {
			return err
		}
		variantType, err := readWaveshareUint32(data, cursor)
		if err != nil {
			return err
		}
		if *cursor >= len(data) {
			return ioBoundsError(*cursor, 1, len(data))
		}
		*cursor++ // QVariant 的空值标记。
		switch variantType {
		case 1: // bool
			if *cursor >= len(data) {
				return ioBoundsError(*cursor, 1, len(data))
			}
			*cursor++
		case 2, 3: // int / uint
			if *cursor+4 > len(data) {
				return ioBoundsError(*cursor, 4, len(data))
			}
			*cursor += 4
		case 4, 5, 6: // 64 位整数 / double
			if *cursor+8 > len(data) {
				return ioBoundsError(*cursor, 8, len(data))
			}
			*cursor += 8
		case 10: // QString
			if _, err := readWaveshareQString(data, cursor); err != nil {
				return err
			}
		default:
			return fmt.Errorf("不支持的 QVariant 类型 %d", variantType)
		}
	}
	return nil
}

func readWaveshareQString(data []byte, cursor *int) (string, error) {
	length, err := readWaveshareUint32(data, cursor)
	if err != nil {
		return "", err
	}
	if length == ^uint32(0) {
		return "", nil
	}
	if length%2 != 0 || uint64(*cursor)+uint64(length) > uint64(len(data)) {
		return "", ioBoundsError(*cursor, int(length), len(data))
	}
	units := make([]uint16, int(length)/2)
	for index := range units {
		units[index] = binary.BigEndian.Uint16(data[*cursor+index*2:])
	}
	*cursor += int(length)
	return string(utf16.Decode(units)), nil
}

func readWaveshareUint32(data []byte, cursor *int) (uint32, error) {
	if *cursor+4 > len(data) {
		return 0, ioBoundsError(*cursor, 4, len(data))
	}
	value := binary.BigEndian.Uint32(data[*cursor : *cursor+4])
	*cursor += 4
	return value, nil
}

func ioBoundsError(offset, length, available int) error {
	return fmt.Errorf("主题数据越界: offset=%d length=%d size=%d", offset, length, available)
}
