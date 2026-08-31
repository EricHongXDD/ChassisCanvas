# 微雪屏端一次性配网与平台授权入口

## 结论

当前两款原厂镜像可以承载一个不需要电脑常驻服务的**一次性本地浏览器门户**，也可以在
`/dev/fb0` 上显示入口和一次性设置码。但它们不能被当作已经具备触控、Wi-Fi 或可直接进行
QQ/网易官方设备授权的成品：触摸在实际设备树中禁用，Wi-Fi 实物与驱动/固件未确认，系统
也没有 CA 根证书。所有这些能力都必须先探测，缺失时失败关闭。

当前最可验证的入口是：

```text
屏幕本地 QML 引导页（只显示地址与一次性码，不依赖触摸）
  -> USB RNDIS：浏览器一次性访问 http://192.168.77.2:19324
  -> 256 位 capability（二维码 URL fragment）或 8 位限次短码
  -> 只配置本地 Wi-Fi / 系统时间
  -> QQ/网易仅展示官方设备授权能力状态；无适配器时固定失败关闭
```

这里的“无需电脑常驻服务”表示门户和配置写入都运行在屏端。当前 USB-RNDIS 路径仍需用户
临时用电脑浏览器打开页面；页面完成后不要求浏览器或 CaseScreen 电脑服务继续运行。
手机直接通过 USB-RNDIS 访问取决于手机的 USB Host/以太网支持，不能作为通用承诺。

## 原厂图形和输入能力证据

对 7 英寸镜像的内核配置、实际 DTB 和 rootfs 做只读检查，得到：

| 能力 | 镜像证据 | 可交付边界 |
|---|---|---|
| framebuffer | `CONFIG_FB=y`；DTB 中 `lcd_fb0`、`disp`、`lcd0` 均为 `okay`；原厂程序已持续使用 `/dev/fb0` | 可显示独占的本地设置引导页 |
| Qt 本地图形 | `/arm-qt/plugins/platforms/libqlinuxfb.so`、`/arm-qt/bin/qmlscene` 存在；`qt_env.sh` 指定 `QT_QPA_PLATFORM=linuxfb:tty=/dev/fb0` | 可复用原厂 Qt 5.12.9，不必在 Go 程序里重写 framebuffer 字体栈 |
| 触摸 | DTB 中 Goodix `ctp@14` 为 `status="disabled"`；`CONFIG_TOUCHSCREEN_GOODIX`、GT9xx、FTS 等均未启用 | **不能承诺触控**；QML 页必须无需触摸即可完成引导 |
| 物理键 | `CONFIG_KEYBOARD_SUNXI=y`；DTB 有 5 键 ADC keyboard，键值包含音量、菜单、Enter、Home | 可在真机确认事件节点后做本地菜单；当前门户不依赖它 |
| ACM | 原厂 configfs Gadget 和 `/dev/ttyGS0` 已确认 | 继续保留原串口协议；ACM 本身不适合让手机浏览器访问 HTTP |
| RNDIS | 内核有 `USB_F_RNDIS`/`USB_CONFIGFS_RNDIS`；仓库脚本已创建 `192.168.77.2/24` 链路 | 当前最可靠的一次性浏览器入口；应绑定具体地址，不绑定所有接口 |
| Wi-Fi 用户态 | `wpa_supplicant`、`wpa_cli`、`hostapd`、`dnsmasq`、softap/wifimanager 示例均存在 | 只能说明软件栈存在，不能证明无线芯片、驱动、固件或 AP 模式可用 |
| RTC/时间 | `RTC_DRV_SUNXI=y`，系统有 `sysfixtime`、`hwclock` 和 `ntpd` 启动脚本 | RTC 实物、后备电池和走时可靠性未验证；必须兼容无 RTC |
| TLS CA | rootfs 没有 `/etc/ssl` 或 CA bundle | 平台 HTTPS 默认不可用；禁止用 `InsecureSkipVerify` 绕过 |

`qt_env.sh` 虽然配置了 tslib `/dev/input/event1`，这不等于产品具有触摸。实际 DTB 与内核驱动
是更强的否定证据；只有刷机后的 `getevent`/`ts_print_mt` 真机结果才能改变此结论。

## 已实现组件

### `internal/edgesetup`

- 默认入口为 `127.0.0.1:19324`；`Listen` 只接受 `localhost` 或数字 IP，默认拒绝
  `0.0.0.0`/`[::]`；
- 每次启动生成 256 位随机 capability。二维码应编码
  `http://设备地址:端口/#setup_token=...`，token 位于 URL fragment，不进入 HTTP 请求、代理
  或 Referer；
- 无二维码时显示 8 位一次性短码；连续 5 次错误后锁定；成功建立一个会话后 token 和短码都
  立即销毁；
- 设置会话使用 `HttpOnly + SameSite=Strict` Cookie，所有写操作还要求独立 CSRF token；
- 校验 Host，页面不加载任何外部资源，设置 `no-referrer`、CSP、禁止 camera/microphone；
- Wi-Fi API 只接受 `ssid`、本地 `passphrase` 和 `hidden`。未知字段（包括账号字段）直接拒绝；
- 平台授权只有脱敏能力状态。门户没有解析或保存 QQ/网易账号、密码、Cookie 的类型或端点；
  任意平台授权 POST 固定返回 `501 Not Implemented`；
- 已认证浏览器可以显式同步 Unix 时间，兼容没有 RTC 的设备。该步骤不替代 CA 或 TLS 校验。
- 用户点击“完成设置”后门户关闭，外层 launcher 可以立即启动正常屏幕程序，不必等待超时。

### Wi-Fi 配置存储

`WPAConfigStore` 只接受 WPA2-Personal 的 8–63 字节 passphrase。写盘前按 WPA 规范执行
PBKDF2-HMAC-SHA1（4096 次，SSID 为 salt），配置文件只包含 64 位十六进制派生 PSK：

- SSID 以十六进制写入，避免配置注入；
- 临时文件与最终文件均尽力设置为 `0600`，目录为 `0700`；
- `fsync` 后使用同目录原子替换；
- passphrase 不进入 JSON 响应、状态、格式化字符串或 `wpa_cli` 命令行；
- 输入无效时不会替换已有配置。

派生 PSK 本身仍是 Wi-Fi 凭据，只是没有保存用户输入的明文口令。物理取得整张 TF 卡的攻击者
仍可离线使用它，因此不能把文件权限描述成硬件级密钥保护。

### 独立入口程序与本地页面

`casescreen-edge-setup` 负责监听、一次性生命周期、可选 `wpa_cli reconfigure`、无 RTC 时间同步，
并启动 `firmware/waveshare/casescreen-setup.qml`。QML 页按横屏/竖屏自适应，只显示 HTTP 地址、
一次性短码和安全边界，不依赖触摸。

它必须在原厂 `PCMonitorLinux` 之前运行，或在原厂程序停止后独占运行。两个 Qt linuxfb 进程不能
同时拥有 `/dev/fb0`。设置窗口结束后，外层 launcher 再 `exec` `casescreen-edge` 或原厂程序。

USB-RNDIS 示例（Wi-Fi 真机能力尚未确认时应省略 `-wifi-config`）：

```sh
source /data/qt_env.sh
exec /data/casescreen-edge-setup \
  -listen 192.168.77.2:19324 \
  -public-url http://192.168.77.2:19324 \
  -qmlscene /arm-qt/bin/qmlscene \
  -qml /data/casescreen-setup.qml \
  -session-ttl 10m
```

只有真机确认 `wlan0`、驱动、固件和 STA 连接后，才可加入：

```sh
  -wifi-config /etc/wifi/wpa_supplicant.conf \
  -wifi-interface wlan0 \
  -wpa-cli /usr/sbin/wpa_cli \
  -wpa-control /etc/wifi/sockets
```

原厂 wpa_supplicant 服务读取 `/etc/wifi/wpa_supplicant.conf`；如果改用 `/data` 中的独立配置，
必须同时修改服务启动参数，否则 `wpa_cli reconfigure` 不会加载那个文件。

## 手机浏览器门户的安全前提

本地门户使用 HTTP，是因为首次设置时设备没有受信 CA、域名和可靠时钟。它只适合以下承载：

1. 物理 USB-RNDIS 链路；或
2. 真机确认 AP 模式后，由屏端随机生成、逐台且逐次不同的 WPA2 密码，并在本地屏幕显示的
   临时 SoftAP。

禁止使用原厂 `/etc/wifi/hostapd.conf` 中固定的示例 SSID/密码，禁止开放无密码 AP，也禁止把
门户直接绑定到家庭 LAN 的 `0.0.0.0`。SoftAP 编排尚未实现，因为当前没有硬件/驱动/AP 模式
证据；在这些证据出现前强行启动只会制造虚假的“手机可配网”承诺。

## 平台授权：当前必须失败关闭

现有 QQ/网易歌词代码使用网页登录 Cookie；它不是 OAuth device authorization，且两家当前接入
代码没有经过平台确认的 device-code/二维码授权 API、公开 client ID、设备注册或 token 刷新契约。
因此本门户不复用现有 Cookie 导入接口，也不把账号密码交给屏幕：

```text
有平台官方 device-code / QR 契约 + 每设备公开客户端配置
  -> 才允许实现 Begin / Poll / Cancel
  -> 手机上打开平台官方 HTTPS 域名并授权
  -> 屏端只保存平台返回的设备 token

缺少任一项
  -> capability.ready=false
  -> POST 固定 501
  -> direct 歌词保持 needs_login，不退化为抓 Cookie、账号密码或标题猜测
```

登录只继承用户已有权限，不会自动获得 VIP。平台若要求 AppSecret、共享私钥或让设备收集用户
密码，也应继续失败关闭，不得把秘密预置到公共 IMG。

## 无 CA / 无 RTC 的引导顺序

1. 本地门户先在物理 USB 或随机 WPA2 AP 上完成会话验证；这个步骤不访问平台。
2. 若系统时间不可信，用户点击“同步本机时间”。该操作受一次性会话和 CSRF 保护；没有 RTC 时
   只设置系统时间，有 `/dev/rtc0` 时再尽力写入 RTC。
3. 安装并版本化受信 CA bundle，或为已确认的官方平台端点内置最小受信根。不能关闭主机名、
   签名或有效期验证。
4. 只有时钟处于合理范围、CA 可用、DNS/默认路由正常时，才把官方设备授权 capability 标记为
   ready。
5. NTP 可以用于日常校时，但原厂未认证 NTP 不能单独作为对抗主动攻击者的首次信任根。

当前仓库没有把 CA bundle 塞进镜像，也没有官方平台设备授权适配器，所以步骤 3–4 保持关闭。

## 仍需真机验收

1. 确认两种尺寸都能在停止 `PCMonitorLinux` 后运行 QML linuxfb 页，退出后 framebuffer 可被原厂
   程序重新取得。
2. 确认 5 个 ADC 键的实际 `/dev/input/event*` 与键值；不要依据 tslib 环境变量认定触摸存在。
3. Windows 在 ACM+RNDIS 复合 Gadget 下能访问 `192.168.77.2:19324`，且门户不出现在其他网卡。
4. 浏览器扫描 fragment token 和手工短码各走一次；验证成功即不可重放，错误 5 次锁定，到期关闭。
5. 若实物有 Wi-Fi，记录芯片 vendor/device、驱动、firmware、`iw list` 的 AP/STA 能力，再做随机
   WPA2 SoftAP 和网络切换。
6. 断电后检查 RTC；无后备走时时重复手机时间引导，并确认平台 TLS 在错误时间/缺 CA 时拒绝连接。
7. 平台授权只有在官方契约、凭据生命周期和真实账号端到端验证完成后才能从 `ready=false` 改为
   可用。

目前这些新文件尚未接入离线 IMG 补丁器，也未在两款实物上执行，因此应标为“实现与离线测试
完成，硬件集成待验收”，不能标为量产可用。
