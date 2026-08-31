# CaseScreen standalone-recognition beta.13

beta.13 在 beta.12 的屏端识歌、精确平台 ID 门禁和 TF 歌词缓存基础上，补齐了原镜像
实际具备的独立联网路径：副屏继续作为 USB Device，显式启用 ACM+RNDIS 复合 Gadget，
再连接一台不依赖电脑的 USB Host/Wi-Fi 小桥。当前可部署实现使用 Raspberry Pi OS /
Debian 的 NetworkManager shared IPv4；桥端只转发网络，不采集或处理音频。

这个发行包仍是需要实体硬件验收的软件发布候选，不是已经验证“插电即可识歌”的最终 IMG。
它不包含 ACRCloud 官方共享库、账号密钥、逐屏 token、QQ/网易云 Cookie，也不会用占位
凭据制作一个表面可启动、实际不可用的镜像。

## beta.13 增量

- 镜像补丁器新增显式 `--network-mode usb-gadget-bridge`；默认 `physical` 行为保持不变；
- 在保留原 `/dev/ttyGS0` 的同时创建 ACM+RNDIS，并在失败时恢复 ACM-only；
- 屏端通过 BusyBox `udhcpc` 从独立小桥取得地址、DNS 和默认路由；
- 只接受 `usb0` 且网关严格为 `192.168.77.1`，普通 Windows RNDIS 共享不会被误放行；
- 原厂 `qt_app` 连续调用两次串口脚本时复用已验证的 DHCP 租约，不重复广播；
- 增加 Raspberry Pi NetworkManager 一键安装、状态和移除脚本；
- 附带默认拒绝启动的 ESP32-S3 实验骨架，并明确记录官方 RNDIS-first 与现有
  ACM-first 之间的兼容性硬门禁；
- 网络模式、接口和网关进入 readiness 报告，硬门禁失败仍回到原厂程序；
- 自动化覆盖错误网关、普通 RNDIS 拒绝、文件注入、回滚和权限校验。

## 目录

- `screen/casescreen-edge`：微雪 ARMv7 屏端程序；
- `screen/casescreen-acrcloud-fingerprint`：只含自研代码的 ARMv7 声纹包装程序；
- `server/casescreen-recognition-relay`：Linux amd64 自托管 relay；
- `deploy/recognition-relay/`：HTTPS、systemd、token 摘要和强映射示例；
- `deploy/waveshare-usb-bridge/`：独立网络桥说明与部署脚本；
- `firmware/serial_uart_casescreen.sh`：ACM+RNDIS/回滚实现；
- `tools/`：8 GB 镜像扩容、补丁工具与声纹包装源码；
- `themes/`：1024×600 和 480×1920 驻留歌词主题；
- `docs/`：架构、映射规则、硬件门禁与验收记录。

## 不能省略的真机门禁

制作正式镜像仍需要：真实 HTTPS relay URL、逐屏 64 位十六进制 token、有效 CA、
有许可且与 relay 匹配的 ARMv7 fingerprint 库、QQ/网易云原生 ID 强映射，以及用户已有
权益对应的平台登录态。7 英寸和 8.8 英寸还必须分别验证麦克风/ALSA、RNDIS 枚举、
小桥与屏幕供电、DNS/NTP/TLS、长期断线恢复、识歌准确率、功耗和温度。

识歌只能确定录音，不能证明播放来源平台。relay 只有在目录中已经验证 QQ `songmid` 或
网易云十进制歌曲 ID 时才返回 `native_id_verified=true`；标题、歌手或时长搜索结果不得
进入精确歌词链路。映射或权限不足时应明确失败，不得静默猜歌或绕过会员限制。

## 二进制校验

- `casescreen-edge`：SHA-256 `FDF7930BEBD5DD91B1933A079958BD0A232ECD7B06BE546C5D509673761570AA`
- `casescreen-recognition-relay`：SHA-256 `66AA1AA76AC07AC0CD0327A8006D5B65A749EFBAB1DE527B17DFC5862ECEF994`
- `casescreen-acrcloud-fingerprint`：SHA-256 `7E20DE4A1B9E8E9E16CF27A528CA1114835823D694252048369D65226B4E5FC9`

详细制作命令见 `firmware/README.md`，独立网络桥步骤见
`deploy/waveshare-usb-bridge/README.md`。不要把正式 token、Cookie 或 ACRCloud secret
提交到源码目录或发送到聊天中。
