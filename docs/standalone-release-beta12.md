# CaseScreen standalone-recognition beta.12

这个发行包交付可审计的屏端程序、自托管识歌 relay、ARMv7 ACRCloud 适配器、
8 GB 镜像补丁器、驻留歌词主题和部署说明。它不包含 ACRCloud 官方共享库、账号
密钥、设备 token、QQ/网易云 Cookie，也不包含用占位凭据伪造的“最终镜像”。

## 目录

- `screen/casescreen-edge`：微雪 ARMv7 屏端程序；
- `screen/casescreen-acrcloud-fingerprint`：只含自研代码的 ARMv7 声纹包装程序；
- `server/casescreen-recognition-relay`：Linux amd64 自托管 relay；
- `deploy/recognition-relay/`：Caddy、systemd、token 摘要和强映射示例；
- `tools/`：8 GB 镜像扩容/补丁工具与声纹包装源码；
- `themes/`：1024×600 和 480×1920 驻留歌词主题；
- `firmware/README.md`：按发行包目录编写的镜像制作与真机门禁说明；
- `docs/`：架构、协议、部署、硬件门禁与验收记录。

## 重要边界

当前两款原镜像没有可用的 Wi-Fi 驱动或固件，当前 7 英寸真机也只枚举 ACM
串口。因此本包是软件发布候选，不是“插电即可独立联网”的成品 IMG。正式制作
7 英寸或 8.8 英寸镜像前，必须先在实体设备上提供并验证独立网络、麦克风、NTP、
DNS 和 TLS；随后配置真实 HTTPS relay、逐屏 64 位十六进制 token、CA、可靠的
QQ/网易云原生 ID 映射以及相应平台登录态。

如果使用 ACRCloud 紧凑 fingerprint，部署者还必须从 ACRCloud 官方来源合法取得
ARM32 `libacrcloud_extr_tool.so`，并在本机使用补丁器的 `--fingerprint-library`
参数注入。包装程序与共享库必须一起提供；发行包不会代为授权或再分发该文件。

详细命令、映射格式和真机检查见 `docs/standalone-recognition.md` 与
`docs/recognition-relay.md`。不要把正式 token、Cookie 或 ACRCloud secret 提交到
源码目录或发送到聊天中。

## 二进制校验

- `casescreen-edge`：SHA-256 `35A03AF5044A54CDDE7AEACD36F3F886668135C98F45C721C71F70E1CCBACA81`
- `casescreen-recognition-relay`：SHA-256 `C35087904443913B605C38B257BFA64A04958ADAD951893430A2D32B78BE78BD`
- `casescreen-acrcloud-fingerprint`：SHA-256 `7E20DE4A1B9E8E9E16CF27A528CA1114835823D694252048369D65226B4E5FC9`
