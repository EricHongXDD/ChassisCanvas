# CaseScreen 屏端识歌中继协议（recognition-v1）

屏端以 16 kHz、单声道、S16_LE 连续录制 1 秒短块。每个短块先执行本地能量
VAD，最近 10 秒只保存在 RAM 环中；连续 2 个静音块会在约 2 秒内暂停歌词并
清空旧歌音频，连续 2 个有声块先恢复本地时间轴，再由后台复核身份。识歌请求
异步执行，不阻塞歌词渲染。

原始 PCM 和声纹均只存在于进程内存，请求结束后立即清零，不写入 TF 卡。屏端
只保存一个可撤销 Bearer token，第三方识歌服务的密钥只能保存在中继端。首次
识别和切歌默认都要求连续 2 个相同 `stable_id` 且置信度不低于 0.78；稳定后
默认 15 秒才复核一次，联网失败从 10 秒指数退避到 5 分钟。

## 首选：设备端紧凑声纹

固件若部署了与中继匹配的 ARMv7 声纹提取器，请配置 extractor、algorithm、
version 三项。提取器从 stdin 读取 10 秒 raw PCM，参数固定为：

```text
--format pcm_s16le --sample-rate 16000 --channels 1 --duration-ms 10000
```

stdout 只输出不超过 64 KiB 的二进制声纹。此时 JSON 请求不含 PCM：

```json
{
  "protocol_version": 1,
  "sampled_at_unix_ms": 1700000000000,
  "preferred_platform": "qq",
  "sample_count": 160000,
  "device_fingerprint": {
    "algorithm": "vendor-acr",
    "version": "1",
    "data_base64": "..."
  }
}
```

仓库没有捆绑或伪装任何第三方 ACR SDK。紧凑声纹模式必须另行部署真实的 ARM32
提取器，并让中继校验 algorithm/version。配置了提取器但运行失败时会退避并
报错，不会静默改成高带宽 PCM。

如使用 ACRCloud 官方轻量库，可采用仓库中的
`tools/acrcloud_fingerprint_wrapper` 适配源码。该适配器负责把本模块的 16 kHz
输入低通降采样到官方库要求的 8 kHz，并从同目录动态加载部署者自行取得的
`libacrcloud_extr_tool.so`；仓库不分发该第三方库。

## 回退：PCM JSON

没有配置 extractor 时仍可使用 PCM 回退。10 秒 PCM 为 320,000 字节，Base64
约 426.7 KiB/次；歌曲稳定后默认每 15 秒复核一次，平均约 28 KiB/s（未计很小的
JSON/TLS 开销）。因此它只适合作为兼容路径，不是最低带宽方案：

请求：

```json
{
  "protocol_version": 1,
  "sampled_at_unix_ms": 1700000000000,
  "preferred_platform": "auto",
  "audio_format": "pcm_s16le",
  "sample_rate_hz": 16000,
  "channels": 1,
  "sample_count": 160000,
  "pcm_s16le_base64": "..."
}
```

## 响应

```json
{
  "protocol_version": 1,
  "matched": true,
  "retry_after_ms": 0,
  "result": {
    "stable_id": "provider:recording-id",
    "isrc": "USRC17607839",
    "title": "Track title",
    "artist": "Artist",
    "album": "Album",
    "duration_ms": 240000,
    "playback_offset_ms": 52000,
    "confidence": 0.94,
    "sampled_at_unix_ms": 1700000000000,
    "source": "qq",
    "track_id": "0039MnYb0qxYhV",
    "native_id_verified": true
  }
}
```

`preferred_platform` 是用户明确选择的 QQ/网易云目标；`auto` 只允许中继依据
强映射证据决定。声纹本身无法判断同一录音当前来自 QQ 还是网易云。

`source`、`track_id` 只有中继已经用平台原生映射或其他强证据验证时才能设置
`native_id_verified=true`，且必须符合 `preferred_platform`。屏端不会按标题或
歌手猜 QQ/网易云歌曲 ID；没有经过验证时仍可展示识别出的歌曲，但不会把稳定
识歌 ID 冒充平台 ID 去请求歌词。返回已验证原生 ID 后，现有 `edgelyrics`
会按该平台 ID 精确取词并写入 TF 歌词缓存；同一首歌之后可直接命中缓存。

## 启动门禁

`recognition` 模式会在原厂显示进程和驻留主题加载后执行一次真实 ALSA 短采样，
同时验证独立默认路由、DNS、CA、可信时间、HTTPS relay 和 0600 设备令牌。
任一条件失败都会关闭接管进程并返回错误，让启动器回退原厂模式。诊断报告不
包含 SSID、密码、token、Cookie 或原始音频。
