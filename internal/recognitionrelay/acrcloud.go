package recognitionrelay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" // ACRCloud Identification Protocol 1 明确要求 HMAC-SHA1。
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"casescreen/internal/edgerecognition"
)

const (
	acrIdentifyPath       = "/v1/identify"
	acrSignatureVersion   = "1"
	maximumACRResponse    = 1 << 20
	minimumACRMusicScore  = 70
	maximumACRMusicScore  = 100
	maximumACRSampleBytes = (30 * edgerecognition.SampleRateHz * edgerecognition.BytesPerSample) + 44
)

// ACRCloudConfig 只在中继端使用。AccessSecret 绝不能写入屏端镜像或映射文件。
type ACRCloudConfig struct {
	Host                 string
	AccessKey            string
	AccessSecret         string
	FingerprintAlgorithm string
	FingerprintVersion   string
	Client               *http.Client
	Timeout              time.Duration
	AllowInsecureHTTP    bool
	Now                  func() time.Time
}

// ACRCloudProvider 实现官方 Identification Protocol 1 multipart/form-data 契约。
type ACRCloudProvider struct {
	endpoint             string
	accessKey            string
	accessSecret         []byte
	fingerprintAlgorithm string
	fingerprintVersion   string
	client               *http.Client
	timeout              time.Duration
	now                  func() time.Time
}

// NewACRCloudProvider 验证上游地址和仅服务端持有的项目凭据。
func NewACRCloudProvider(config ACRCloudConfig) (*ACRCloudProvider, error) {
	endpoint, err := normalizeACRCloudEndpoint(config.Host, config.AllowInsecureHTTP)
	if err != nil {
		return nil, err
	}
	accessKey := strings.TrimSpace(config.AccessKey)
	accessSecret := strings.TrimSpace(config.AccessSecret)
	if !safeCredential(accessKey, 8, 256) || !safeCredential(accessSecret, 8, 512) {
		return nil, errors.New("ACRCloud access_key/access_secret 格式无效")
	}
	timeout := config.Timeout
	if timeout == 0 {
		timeout = 12 * time.Second
	}
	if timeout < time.Second || timeout > 30*time.Second {
		return nil, errors.New("ACRCloud 请求超时必须在 1..30 秒")
	}
	algorithm := strings.TrimSpace(config.FingerprintAlgorithm)
	version := strings.TrimSpace(config.FingerprintVersion)
	if (algorithm == "") != (version == "") {
		return nil, errors.New("ACRCloud 声纹 algorithm/version 必须同时配置")
	}
	if algorithm != "" && (!safeIdentifier(algorithm, 1, 64) || !safeIdentifier(version, 1, 32)) {
		return nil, errors.New("ACRCloud 声纹 algorithm/version 格式无效")
	}
	client := config.Client
	if client == nil {
		client = &http.Client{}
	}
	copyClient := *client
	copyClient.Timeout = timeout
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &ACRCloudProvider{
		endpoint: endpoint, accessKey: accessKey, accessSecret: []byte(accessSecret),
		fingerprintAlgorithm: algorithm, fingerprintVersion: version,
		client: &copyClient, timeout: timeout, now: now,
	}, nil
}

func (provider *ACRCloudProvider) Recognize(ctx context.Context, submission edgerecognition.Submission) (edgerecognition.Response, error) {
	if provider == nil || provider.client == nil {
		return edgerecognition.Response{}, errors.New("ACRCloud provider 尚未初始化")
	}
	if ctx == nil || submission.SampledAt.IsZero() || submission.Duration <= 0 {
		return edgerecognition.Response{}, errors.New("ACRCloud 识歌采样无效")
	}
	dataType := "audio"
	sample := submission.PCM
	if submission.Fingerprint != nil {
		fingerprint := submission.Fingerprint
		if provider.fingerprintAlgorithm == "" || fingerprint.Algorithm != provider.fingerprintAlgorithm ||
			fingerprint.Version != provider.fingerprintVersion {
			return edgerecognition.Response{}, errors.New("屏端声纹格式与 ACRCloud provider 配置不兼容")
		}
		dataType = "fingerprint"
		sample = fingerprint.Data
	} else {
		if len(sample) == 0 || len(sample)%edgerecognition.BytesPerSample != 0 {
			return edgerecognition.Response{}, errors.New("ACRCloud PCM 格式无效")
		}
		wav, err := pcmS16LEToWAV(sample, edgerecognition.SampleRateHz, edgerecognition.ChannelCount)
		if err != nil {
			return edgerecognition.Response{}, err
		}
		defer clear(wav)
		sample = wav
	}
	if len(sample) == 0 || len(sample) > maximumACRSampleBytes {
		return edgerecognition.Response{}, errors.New("ACRCloud 采样大小无效")
	}

	timestamp := strconv.FormatInt(provider.now().UTC().Unix(), 10)
	signature := provider.signature(dataType, timestamp)
	defer clear(signature)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fields := [][2]string{
		{"access_key", provider.accessKey},
		{"sample_bytes", strconv.Itoa(len(sample))},
		{"timestamp", timestamp},
		{"signature", string(signature)},
		{"data_type", dataType},
		{"signature_version", acrSignatureVersion},
	}
	for _, field := range fields {
		if err := writer.WriteField(field[0], field[1]); err != nil {
			return edgerecognition.Response{}, errors.New("编码 ACRCloud multipart 字段失败")
		}
	}
	part, err := writer.CreateFormFile("sample", "sample.bin")
	if err != nil {
		return edgerecognition.Response{}, errors.New("编码 ACRCloud 采样字段失败")
	}
	if _, err := part.Write(sample); err != nil {
		return edgerecognition.Response{}, errors.New("编码 ACRCloud 采样失败")
	}
	if err := writer.Close(); err != nil {
		return edgerecognition.Response{}, errors.New("结束 ACRCloud multipart 编码失败")
	}
	defer clear(body.Bytes())

	requestContext, cancel := context.WithTimeout(ctx, provider.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, provider.endpoint, bytes.NewReader(body.Bytes()))
	if err != nil {
		return edgerecognition.Response{}, errors.New("创建 ACRCloud 请求失败")
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "CaseScreen-Recognition-Relay/1")
	response, err := provider.client.Do(request)
	if err != nil {
		return edgerecognition.Response{}, temporaryError{message: "ACRCloud 暂时不可用", after: 10 * time.Second}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		after := parseRetryAfter(response.Header.Get("Retry-After"))
		if after == 0 {
			after = 30 * time.Second
		}
		return edgerecognition.Response{}, temporaryError{message: "ACRCloud 暂时拒绝请求", after: after}
	}
	decoded, err := decodeACRResponse(response.Body)
	if err != nil {
		return edgerecognition.Response{}, err
	}
	if decoded.Status.Code == 1001 {
		return edgerecognition.Response{Matched: false, RetryAfter: 10 * time.Second}, nil
	}
	if decoded.Status.Code != 0 {
		return edgerecognition.Response{}, temporaryError{message: "ACRCloud 返回服务错误", after: 30 * time.Second}
	}
	result, found := bestACRMusic(decoded.Metadata.Music, submission.SampledAt)
	if !found {
		return edgerecognition.Response{Matched: false, RetryAfter: 10 * time.Second}, nil
	}
	return edgerecognition.Response{Matched: true, Result: result}, nil
}

func (provider *ACRCloudProvider) signature(dataType, timestamp string) []byte {
	stringToSign := strings.Join([]string{
		http.MethodPost, acrIdentifyPath, provider.accessKey, dataType, acrSignatureVersion, timestamp,
	}, "\n")
	mac := hmac.New(sha1.New, provider.accessSecret)
	_, _ = mac.Write([]byte(stringToSign))
	encoded := make([]byte, base64.StdEncoding.EncodedLen(mac.Size()))
	base64.StdEncoding.Encode(encoded, mac.Sum(nil))
	return encoded
}

func normalizeACRCloudEndpoint(host string, allowHTTP bool) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", errors.New("ACRCloud host 不能为空")
	}
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	parsed, err := url.Parse(host)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("ACRCloud host 无效")
	}
	if parsed.Scheme != "https" && !(allowHTTP && parsed.Scheme == "http") {
		return "", errors.New("ACRCloud 上游必须使用 HTTPS")
	}
	if parsed.Path != "" && parsed.Path != "/" && parsed.Path != acrIdentifyPath {
		return "", errors.New("ACRCloud host 不能包含自定义路径")
	}
	parsed.Path = acrIdentifyPath
	parsed.RawPath = ""
	return parsed.String(), nil
}

func safeCredential(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum || value != strings.TrimSpace(value) {
		return false
	}
	return !strings.ContainsAny(value, "\x00\r\n\t ")
}

func pcmS16LEToWAV(pcm []byte, sampleRate, channels int) ([]byte, error) {
	if sampleRate <= 0 || channels <= 0 || len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) > math.MaxUint32-44 {
		return nil, errors.New("PCM 无法编码为 WAV")
	}
	bitsPerSample := 16
	blockAlign := channels * bitsPerSample / 8
	byteRate := sampleRate * blockAlign
	wav := make([]byte, 44+len(pcm))
	copy(wav[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], uint32(36+len(pcm)))
	copy(wav[8:12], "WAVE")
	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)
	binary.LittleEndian.PutUint16(wav[20:22], 1)
	binary.LittleEndian.PutUint16(wav[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(wav[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(wav[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(wav[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(wav[34:36], uint16(bitsPerSample))
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], uint32(len(pcm)))
	copy(wav[44:], pcm)
	return wav, nil
}

type flexibleNumber float64

func (number *flexibleNumber) UnmarshalJSON(data []byte) error {
	value := strings.TrimSpace(string(data))
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		unquoted, err := strconv.Unquote(value)
		if err != nil {
			return err
		}
		value = unquoted
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return errors.New("ACRCloud 数值字段无效")
	}
	*number = flexibleNumber(parsed)
	return nil
}

type acrResponse struct {
	Status struct {
		Code int `json:"code"`
	} `json:"status"`
	Metadata struct {
		Music []acrMusic `json:"music"`
	} `json:"metadata"`
}

type acrMusic struct {
	ACRID        string         `json:"acrid"`
	Title        string         `json:"title"`
	Score        flexibleNumber `json:"score"`
	DurationMS   flexibleNumber `json:"duration_ms"`
	PlayOffsetMS flexibleNumber `json:"play_offset_ms"`
	Album        struct {
		Name string `json:"name"`
	} `json:"album"`
	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`
	ExternalIDs struct {
		ISRC string `json:"isrc"`
	} `json:"external_ids"`
}

func decodeACRResponse(reader io.Reader) (acrResponse, error) {
	limited := io.LimitReader(reader, maximumACRResponse+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return acrResponse{}, errors.New("读取 ACRCloud 响应失败")
	}
	defer clear(data)
	if len(data) > maximumACRResponse {
		return acrResponse{}, errors.New("ACRCloud 响应超过大小上限")
	}
	var decoded acrResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return acrResponse{}, errors.New("ACRCloud 响应不是有效 JSON")
	}
	return decoded, nil
}

func bestACRMusic(music []acrMusic, sampledAt time.Time) (edgerecognition.Result, bool) {
	var best edgerecognition.Result
	for _, candidate := range music {
		score := float64(candidate.Score)
		if score < minimumACRMusicScore || score > maximumACRMusicScore {
			continue
		}
		acrid := strings.TrimSpace(candidate.ACRID)
		title := strings.TrimSpace(candidate.Title)
		artists := make([]string, 0, len(candidate.Artists))
		for _, artist := range candidate.Artists {
			if name := strings.TrimSpace(artist.Name); name != "" {
				artists = append(artists, name)
			}
		}
		if !safeIdentifier(acrid, 1, 128) || title == "" || len(artists) == 0 {
			continue
		}
		durationMS := int64(candidate.DurationMS)
		offsetMS := int64(candidate.PlayOffsetMS)
		if durationMS < 0 || durationMS > int64((24*time.Hour)/time.Millisecond) ||
			offsetMS < 0 || offsetMS > int64((24*time.Hour)/time.Millisecond) {
			continue
		}
		confidence := score / 100
		if confidence <= best.Confidence {
			continue
		}
		isrc := normalizeISRC(candidate.ExternalIDs.ISRC)
		if isrc != "" && !validISRC(isrc) {
			isrc = ""
		}
		best = edgerecognition.Result{
			StableID: "acrcloud:" + acrid, ISRC: isrc,
			Title: title, Artist: strings.Join(artists, ", "), Album: strings.TrimSpace(candidate.Album.Name),
			Duration:       time.Duration(durationMS) * time.Millisecond,
			PlaybackOffset: time.Duration(offsetMS) * time.Millisecond,
			Confidence:     confidence, SampledAt: sampledAt,
		}
	}
	return best, best.StableID != ""
}

func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 1 || seconds > 24*60*60 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
