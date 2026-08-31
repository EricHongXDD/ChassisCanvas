package lyrics

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var timestampPattern = regexp.MustCompile(`\[(\d{1,3}):(\d{2})(?:[.:](\d{1,3}))?\]`)

// Line 是一条已同步的歌词。
type Line struct {
	At   time.Duration `json:"at"`
	Text string        `json:"text"`
}

// Document 保留 LRC 元数据和排序后的时间轴。
type Document struct {
	Title     string            `json:"title,omitempty"`
	Artist    string            `json:"artist,omitempty"`
	Album     string            `json:"album,omitempty"`
	PlainText string            `json:"plain_text,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Lines     []Line            `json:"lines"`
}

const UnsynchronizedLyricsMessage = "该首歌的歌词暂不支持滚动"

// HasContent 同时接受带时间戳的 LRC 与平台只提供的纯文本歌词。
func (document Document) HasContent() bool {
	return len(document.Lines) > 0 || strings.TrimSpace(document.PlainText) != ""
}

// Scrollable 表示歌词具有可用于逐行滚动的时间戳。
func (document Document) Scrollable() bool { return len(document.Lines) > 0 }

// Mode 是小数据协议使用的稳定能力值。
func (document Document) Mode() string {
	if document.Scrollable() {
		return "timed"
	}
	if strings.TrimSpace(document.PlainText) != "" {
		return "plain"
	}
	return "none"
}

// ParseLyricsText 优先解析同步 LRC；若平台只返回无时间戳文本，则保留正文，
// 由界面明确提示该歌曲暂不支持滚动，而不是把它误报成“没有歌词”。
func ParseLyricsText(text string) (Document, error) {
	document, err := ParseLRC(bytes.NewBufferString(text))
	if err != nil {
		return Document{}, err
	}
	if document.Scrollable() {
		return document, nil
	}
	plain := plainLyricsText(text)
	if plain == "" {
		return Document{}, errors.New("歌词内容为空")
	}
	document.PlainText = plain
	return document, nil
}

func plainLyricsText(text string) string {
	var lines []string
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(strings.TrimPrefix(raw, "\ufeff"))
		if line == "" {
			continue
		}
		// 纯元数据标签不属于歌词正文。
		if _, _, ok := parseMetadata(line); ok {
			continue
		}
		line = timestampPattern.ReplaceAllString(line, "")
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// ParseLRC 支持标准 LRC、每行多时间戳和 offset 偏移。
func ParseLRC(reader io.Reader) (Document, error) {
	document := Document{Metadata: make(map[string]string)}
	scanner := bufio.NewScanner(reader)
	offset := time.Duration(0)
	for scanner.Scan() {
		raw := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if raw == "" {
			continue
		}
		matches := timestampPattern.FindAllStringSubmatchIndex(raw, -1)
		if len(matches) == 0 {
			if key, value, ok := parseMetadata(raw); ok {
				document.Metadata[key] = value
				switch key {
				case "ti":
					document.Title = value
				case "ar":
					document.Artist = value
				case "al":
					document.Album = value
				case "offset":
					milliseconds, err := strconv.ParseInt(value, 10, 64)
					if err != nil {
						return Document{}, errors.New("LRC offset 无效")
					}
					offset = time.Duration(milliseconds) * time.Millisecond
				}
			}
			continue
		}
		textStart := matches[len(matches)-1][1]
		text := strings.TrimSpace(raw[textStart:])
		for _, match := range matches {
			at, err := parseTimestamp(raw, match)
			if err != nil {
				return Document{}, err
			}
			document.Lines = append(document.Lines, Line{At: at, Text: text})
		}
	}
	if err := scanner.Err(); err != nil {
		return Document{}, err
	}
	for index := range document.Lines {
		document.Lines[index].At += offset
		if document.Lines[index].At < 0 {
			document.Lines[index].At = 0
		}
	}
	sort.SliceStable(document.Lines, func(left, right int) bool {
		return document.Lines[left].At < document.Lines[right].At
	})
	return document, nil
}

// Current 返回当前行、下一行和当前行进度。
func (document Document) Current(position time.Duration) (current Line, next Line, progress float64, ok bool) {
	if len(document.Lines) == 0 || position < document.Lines[0].At {
		return Line{}, Line{}, 0, false
	}
	index := sort.Search(len(document.Lines), func(index int) bool {
		return document.Lines[index].At > position
	}) - 1
	current = document.Lines[index]
	if index+1 < len(document.Lines) {
		next = document.Lines[index+1]
		duration := next.At - current.At
		if duration > 0 {
			progress = float64(position-current.At) / float64(duration)
		}
	} else {
		progress = 1
	}
	if progress < 0 {
		progress = 0
	} else if progress > 1 {
		progress = 1
	}
	return current, next, progress, true
}

func parseMetadata(raw string) (string, string, bool) {
	if len(raw) < 4 || raw[0] != '[' || raw[len(raw)-1] != ']' {
		return "", "", false
	}
	content := raw[1 : len(raw)-1]
	separator := strings.IndexByte(content, ':')
	if separator <= 0 {
		return "", "", false
	}
	return strings.ToLower(content[:separator]), content[separator+1:], true
}

func parseTimestamp(raw string, match []int) (time.Duration, error) {
	minutes, err := strconv.Atoi(raw[match[2]:match[3]])
	if err != nil {
		return 0, err
	}
	seconds, err := strconv.Atoi(raw[match[4]:match[5]])
	if err != nil || seconds >= 60 {
		return 0, errors.New("LRC 时间戳秒数无效")
	}
	fraction := 0
	if match[6] >= 0 {
		value := raw[match[6]:match[7]]
		fraction, err = strconv.Atoi(value)
		if err != nil {
			return 0, err
		}
		switch len(value) {
		case 1:
			fraction *= 100
		case 2:
			fraction *= 10
		case 3:
		default:
			return 0, errors.New("LRC 时间戳小数位无效")
		}
	}
	return time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second + time.Duration(fraction)*time.Millisecond, nil
}
