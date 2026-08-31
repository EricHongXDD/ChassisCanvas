# 微雪屏 standalone-recognition 镜像部署

当前推荐的屏端运行链是 `standalone-recognition`：屏幕由独立电源供电，通过本地 ALSA 采集设备
录取短片段，经独立网络访问 CaseScreen 识歌 relay，拿到经过验证的 QQ 音乐或网易云音乐原生
歌曲 ID，再在屏端获取逐行歌词并写入驻留 Theme 的动态键。

> 本目录只是不会进入 Windows 正式发行包的历史实验。仓库中的原始 IMG 没有被修改，
> 也不能把 DHCP 自动成功当作既定事实。实验桥固定使用 `192.168.77.1/24`，Windows
> 网卡的网关：留空；主机正式服务仍只监听 `127.0.0.1:19321`。真机恢复时可先执行
> `/data/serial_uart.sh restore-acm`，离线恢复路径为 `/upper/data/serial_uart.sh`。

旧 EdgeSync、`source-push` 和主机 relay 仅保留作历史研究。新的
`usb-gadget-bridge` 是独立识歌的可选联网方式：屏幕仍自行录音、提取声纹和滚动歌词，
ACM+RNDIS 只接无需电脑的专用 USB Host/Wi-Fi 小桥。它与 Windows USB 共享网络不是同一门禁。

## 运行门禁与原厂回退

新链启动前必须同时满足：

1. 能枚举 ALSA 声卡；
2. `arecord` 可执行，指定 PCM 能在安全超时内产生单声道 `S16_LE/16 kHz` 短采样；
3. 存在已启用的物理独立网络，或显式启用并验证 `usb0 -> 192.168.77.1` 专用小桥；
4. DNS、PEM CA 根证书和由时间同步服务确认的可信时间均可用；
5. `recognition.env` 含有效 HTTPS relay URL，`recognition.token` 是每屏独有的 256 位令牌。

采样临时文件在成功、失败和超时后都会删除。安静时 RMS 可以为零，这不等同于录音设备失败。
动态门禁写到 `/data/casescreen/diagnostics/latest.json`，报告不含 URL、IP、PCM 名称、SSID、token
或登录态。`recognition_transport` 会明确记录 `fingerprint` 或 `pcm_json`，
`recognition_transport_ready` 表示当前配置的传输方式通过校验。门禁失败或 edge 异常退出时，
launcher 恢复运行 `/data/PCMonitorLinux.original`。

原厂镜像是否实际焊接麦克风、是否包含可工作的声卡及独立 Wi-Fi 驱动，仍必须在 7 英寸和 8.8 英寸
真机上分别验证；离线 DTB 节点或用户空间命令存在不能替代真机测试。

## 制作命令

输入必须是干净的原厂扩容镜像。工具只修改第 6 个 `UDISK` ext4 分区，不就地修改输入、
不覆盖已存在输出；任一写入、e2fsck、GPT、权限、内容或分区外 SHA-256 复验失败都不会提交输出。

```powershell
python tools\patch_waveshare_image.py `
  "artifacts\8gb-expanded\7inch USB Monitor-240719-001-8GB.img" `
  "output\7inch USB Monitor-240719-001-8GB-standalone-recognition.img" `
  --runtime-mode standalone-recognition `
  --network-mode usb-gadget-bridge `
  --edge-binary "screen\casescreen-edge" `
  --recognition-relay-url "https://recognition.example.com/v1/recognize" `
  --device-token-file "private\waveshare-7-recognition.token" `
  --ca-bundle "private\ca-certificates.crt" `
  --recognition-pcm "plughw:audiocodec" `
  --fingerprint-extractor "screen\casescreen-acrcloud-fingerprint" `
  --fingerprint-library "private\libacrcloud_extr_tool.so" `
  --fingerprint-algorithm "acrcloud-fingerprint" `
  --fingerprint-version "1" `
  --standalone-theme "themes\casescreen-lyrics-1024x600.Theme" `
  --theme-resolution 1024x600 `
  --work-dir C:\casescreen-work
```

8.8 英寸应使用其实际分辨率对应的 Theme 和 `--theme-resolution 480x1920`。补丁器会拒绝旧模式、
HTTP relay、非 ARM32 edge、缺少 recognition 参数的旧二进制、无效 token、不可解析的 X.509 CA、
无效 Theme，以及已经
注入过 CaseScreen 屏端链的输入镜像。

`--network-mode physical` 保留给未来经真机证明的板载/独立物理网卡；当前两款原镜像没有可用
Wi-Fi/USB 网卡驱动。`usb-gadget-bridge` 会备份原 `/data/serial_uart.sh`，安装 ACM+RNDIS 复合
脚本，并用 30 秒有界 DHCP 获取地址、`192.168.77.1` 默认路由和 DNS。小桥端配置见
[`deploy/waveshare-usb-bridge`](../../deploy/waveshare-usb-bridge/README.md)。

真机首次测试可在停止 CaseScreen/原厂麦克风控件后执行：

```sh
cat /proc/asound/cards
arecord -L
arecord -D plughw:audiocodec -q -f S16_LE -r 16000 -c 1 -d 2 -t wav /tmp/casescreen-probe.wav
ls -l /tmp/casescreen-probe.wav
rm -f /tmp/casescreen-probe.wav
```

原镜像 `/etc/asound.conf` 的 `CaptureDsnoop` 固定为 `hw:audiocodec`、16 kHz、3 声道；直接使用
`hw:audiocodec -c 1` 可能得到 `channels unavailable`，所以默认用 `plughw:audiocodec` 做声道转换。
同一时间不能让原厂 type 107/108 麦克风控件和识歌采集争用硬件；补丁器会解析并拒绝含 type
107/108 的 standalone Theme。

## 镜像内文件

| 路径 | 权限 | 用途 |
|---|---:|---|
| `/data/PCMonitorLinux` | `0755` | 白名单读取配置并负责原厂回退的 launcher |
| `/data/PCMonitorLinux.original` | `0755` | 输入镜像中的原厂程序 |
| `/data/casescreen-edge` | `0755` | ARM32 屏端识歌、歌词与显示代理 |
| `/data/casescreen/recognition.env` | `0600` | relay URL、token 路径及非秘密调度参数 |
| `/data/casescreen/recognition.token` | `0600` | 可在 relay 撤销的每屏设备 token |
| `/data/casescreen/fingerprint-extractor` | `0755` | 可选 ARM32 紧凑声纹提取器 |
| `/data/casescreen/ca-bundle.pem` | `0644` | HTTPS CA 根证书 |
| `/data/casescreen/lyrics-cache` | `0700` | 256 文件/32 MiB 有界歌词缓存 |
| `/data/casescreen/platform-auth` | `0700` | 加密的平台登录态 |
| `/data/casescreen/import` | `0700` | 一次性私有 cookie 投递目录 |
| `/data/casescreen/diagnostics/latest.json` | `0600` | 脱敏门禁报告 |
| `/data/serial_uart.sh.original` | `0755` | 专用桥模式保留的原厂 ACM-only 脚本 |
| `/data/casescreen/usb-gadget-bridge.sh` | `0755` | ACM+RNDIS、DHCP、路由与 DNS 启动脚本 |

歌词缓存会在启动时及每 10 分钟修剪。PCM 只驻留内存，不写入 TF 缓存。声纹提取器必须是 ARM32
小端 ELF，stdin 接收最近 10 秒的 `S16_LE/16 kHz/单声道` 原始 PCM，stdout 最多输出 64 KiB
紧凑声纹；algorithm/version 由 relay 校验。配置了提取器后，提取失败会失败关闭，不能暗中回退 PCM。

提取器三项可以全部省略以兼容开发/初次硬件验证，但 launcher 会明确警告并发送 10 秒 PCM JSON，
约 427 KiB/次；这不是低带宽正式状态，不能宣称已经做到紧凑声纹传输。

## QQ 音乐 / 网易云一次性登录导入

公开镜像绝不内置平台 cookie 或第三方识歌供应商主密钥。需要 VIP 登录态时，仅在用户自己的 TF
卡上把 Netscape `cookie.txt` 放入私有目录，并设置 `recognition.env` 中对应的可选字段：

```text
CASESCREEN_QQ_COOKIE_FILE=/data/casescreen/import/qq.cookies.txt
CASESCREEN_NETEASE_COOKIE_FILE=/data/casescreen/import/netease.cookies.txt
```

可以只设置其中一个。`casescreen-edge` 成功加密导入后会删除明文。relay 若不能返回经过验证的同平台
原生 ID，屏端不能用标题猜 ID，应显示该首歌的歌词暂不支持滚动。

## 撤销与恢复

设备丢失或 token 泄露时，先在 relay 服务端撤销旧 token，再给该屏写入新的随机 64 位十六进制
token；不要多屏共用，也不要把第三方主密钥写入镜像。

恢复原厂时，在离线挂载的 UDISK overlay 中把 `/upper/data/PCMonitorLinux.original` 复制回
`/upper/data/PCMonitorLinux`；若使用专用桥，再把 `/upper/data/serial_uart.sh.original` 复制回
`/upper/data/serial_uart.sh`，两者均保持 `0755`，然后删除补丁器拥有的 CaseScreen 文件。最稳妥的
恢复方式仍是重新写入保存好的原厂镜像。
