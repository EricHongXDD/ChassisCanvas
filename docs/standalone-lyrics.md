# 微雪歌词的两种运行模式

> 本文仅作为历史方案比较，其中的屏端 `standalone-recognition`
> 不是正式运行模式，本文也不进入 Windows 发行包。正式歌词方案只以
> [Windows 主机歌词同步](host-lyrics.md) 为准。

## 先选模式

| 模式 | 歌曲来源 | 歌词与时间轴 | 日常传输 | 定位 |
|---|---|---|---|---|
| A：Windows 主机同步 | GSMTC + 平台原生 ID 适配器 | Windows 按同平台 ID 取词并跟随播放器时间轴 | USB `CommandSystemData` 小包 | **默认推荐**；资源占用、隐私面和故障面最小 |
| B：`standalone-recognition` | 微雪屏端麦克风 + HTTPS 识歌中继 | 屏端按已验证的平台 ID 取词、缓存到 TF，并在本机推进时间轴 | 紧凑 fingerprint 优先；无提取器时显式使用高带宽 PCM 兼容回退 | 条件式实验路径；只有全部硬件与信任门禁通过后才能启用 |

模式 B 不是模式 A 的“无电脑即插即用”保证。原厂镜像的静态内容不能证明实体麦克风、ALSA 录音链、Wi-Fi 芯片及其驱动/固件可用，也没有可证明可用的 CA 根证书环境。尤其不能仅凭 8.8 英寸镜像就宣称它能够独立联网。详细边界见 [屏端麦克风识歌模式](standalone-recognition.md)。

## 模式 A：推荐的 Windows 主机同步

微雪屏端不联网、不登录 QQ 音乐或网易云音乐，也不运行识歌服务。Windows 主机负责确定歌曲、取得同平台歌词、选择当前句与下一句，再通过现有 USB `CommandSystemData` 发送小型 `QStringMap`。副屏只显示 TF 中驻留 Theme 的动态控件。

```text
Windows GSMTC
  -> 标题/歌手/专辑/时长/播放状态/位置
  -> 平台适配器取得来源原生歌曲 ID
  -> 使用同平台登录态获取并缓存 LRC/QRC/纯文本歌词
  -> 计算当前句、下一句及两句时间戳；按需量化当前句进度
  -> USB QStringMap 事件小包；有频谱时细进度合并进约 9～10 Hz 频谱包，无频谱时最高 20 Hz
  -> 微雪 TF 驻留 Theme 的双行文字控件
```

这条路径的边界是：

- 静态 Theme 只在首次应用或内容变化时上传；
- 日常不传背景、整份歌词、完整视频帧、Cookie、平台密钥或原始音频；
- 不读取麦克风，也不把任何音频上传到服务；
- 平台登录态只留在 Windows，且只使用用户账号已经拥有的登录或会员权益。

### 准确歌曲 ID

准确身份始终是 `source + track_id`：

- QQ 音乐：`source=qq`，`track_id` 必须是 QQ `songmid`；
- 网易云音乐：`source=netease`，`track_id` 必须是规范十进制歌曲 ID。

GSMTC 提供媒体说明和播放时间轴，但不原生公开 QQ `songmid` 或网易云数字歌曲 ID。标题搜索、播放队列、歌词搜索或标题/歌手相似度得到的候选都不能冒充来源原生 ID；拿不到可靠 ID 时准确取词必须失败关闭。

当前 Windows 路径包括：

- 网易云适配器由 CaseScreen 启动官方客户端并持有私有匿名管道，只用固定表达式读取 `window.__autotest__` 的当前歌曲数字 `resourceId`、媒体字段、播放状态和时间轴；不读取 `webdb`、窗口标题、队列或历史。真实客户端端到端结果仍应单独验收。
- QQ 适配器只接受来源确认为 QQ 的媒体会话。CaseScreen 主进程对 `QQMusic.exe` 做有界只读检查，仅当 `QQMusic.dll` 整体不超过 20 MiB 时才完整扫描；模块超限、任一分块读取失败或多进程证据不一致都整轮失败关闭。songId、标题、全部歌手、专辑、时长、位置和播放状态还必须与同一轮 GSMTC 一致，随后只按 songId 换取 songmid 并再次严格核对。该路径不写内存、不注入 DLL、不需要 companion 或独立桥；真实客户端切歌、暂停和恢复仍应单独验收。
- 生产入口只保留网易云、QQ 和用户明确选择的本地 LRC。酷狗/搜狗标题搜索不进入准确模式。

### USB 小包

典型歌词包只包含：

```text
CaseScreen_LyricsCurrent       当前句
CaseScreen_LyricsNext          下一句
CaseScreen_LyricsCurrentAtMs   当前句起始时间（毫秒）
CaseScreen_LyricsNextAtMs      下一句起始时间（毫秒）
CaseScreen_LyricsPlaying       true / false
LV                             0 隐藏整层；1 显示（暂停且有当前句时仍为 1）
LFP                            可选，新版 Theme 当前句 0..10000 细进度
CaseScreen_LyricsProgress      兼容旧 Theme，当前句 0..100 整数进度
```

为了维持低资源占用：

- 当前句和下一句各自最多 256 个 Unicode 字符；
- 包含 `LV` 的基础六字段只在歌词行、时间戳、播放状态或可见性变化时发送；
- 仅当微雪主题启用进度轨时才发送细进度；频谱可见时 `LFP` 并入约 9～10 Hz 的完整频谱 `QStringMap`，无频谱时才发独立单键且最高 20 Hz，可见像素未变化会去重；旧键不进入频谱包，只在跨整百分比或状态事件时单独发送；暂停后停止周期发送；
- 设备 ACK 后才记录最近载荷，完全相同的已确认包不会重复发送；
- 断线重连或重新加载 Theme 后清空去重状态并重发一次当前状态；
- 切歌取词中、无歌词、当前句尚未出现或禁用歌词时发送显式清空包：`LV=0`、
  空双行、空时间戳、`playing=false` 和新旧进度归零；失败则在重连后重试；
- 等待与错误原因留在日志/状态页，不进入歌词层，也不生成伪造的 `0ms` 时间戳。

### 播放、暂停与拖动

有时间戳的 LRC/QRC 由 Windows 主机解析。每次采样以同一锚点计算当前位置、当前句与下一句，但连续播放位置不进入 USB 包；启用进度轨时只下发主机量化后的单个百分比整数。

- 播放：主机按播放器锚点和本机经过时间投影位置，越过下一句时间戳时换行；
- 暂停：锁定当前位置，保留当前/下一句与 `LV=1` 并发送 `playing=false`；启用进度轨时同一事件携带最终进度，随后停止周期发送；
- 恢复：从锁定位置继续并发送 `playing=true`；
- 拖动：接受播放器新锚点并立即重新选择歌词行；
- 纯文本：不伪造时间轴，以 `LV=1` 固定显示“该首歌的歌词暂不支持滚动”。

## 模式 B：屏端 `standalone-recognition`

该模式让副屏麦克风经 ALSA 连续取得 1 秒、16 kHz、单声道、S16_LE 短块，在内存中维护 10 秒环形窗口。短块先通过本地 VAD；静音不触发联网。窗口满足条件后，屏端优先运行 ARM32/ARMv7 紧凑 fingerprint 提取器，并向配置好的 HTTPS relay 发送声纹、时间和可撤销设备令牌。未配置提取器时允许显式 PCM 兼容回退，但 10 秒 PCM 的 JSON base64 约 427 KiB/次，不能描述成低带宽模式。原始 PCM 不写入 TF，使用完立即清零。

中继返回稳定识歌 ID、识别时的歌曲偏移和置信度。首次歌曲或切歌默认需要连续两次相同的高置信结果；接受后，屏端以该偏移为锚点，使用本机单调经过时间推进歌词。连续两个静音短块会在约 2–2.5 秒内触发“暂停”的保守推断并清空旧识别环，不等于知道播放器真实状态；随后连续两个有声短块可先本地恢复推进，后台低频识别再校准 offset。

只有中继提供经过可靠平台映射验证的 `source + track_id`，屏端才会访问对应平台的精确歌词接口并把结果缓存到 TF。命中缓存后，逐句选择和滚动都在屏端完成。断网或 relay 暂时失败时，请求按有界指数策略退避；已有缓存和当前锚点可继续工作，但新歌无法凭空识别或取词。

模式 B 的完整数据、隐私、门禁和回退规则见 [屏端麦克风识歌模式](standalone-recognition.md)。

## 识歌不能证明播放平台

同一录音无论来自 QQ 音乐、网易云音乐、本地文件、浏览器还是扬声器，音频 fingerprint 都可能相同。声纹可以指向“哪一版录音”，不能证明“当前由哪个应用或平台播放”。因此：

- relay 的稳定识歌 ID 或 ISRC 不能直接当作 QQ `songmid` 或网易云数字 ID；
- `preferred_platform` 只是请求偏好，不是平台来源证据；
- `source`、`track_id` 只有在服务端授权目录、官方接口或其他可靠的平台映射已验证后，才能标记为 `native_id_verified=true`；
- 仅按标题、歌手、专辑或时长搜索并猜测 ID 不可进入准确歌词链路；
- 没有可靠映射时可以显示识歌结果，但 QQ/网易云精确歌词必须失败关闭。

无论模式 A 还是 B，登录态只继承用户已有权益，不会自动获得 VIP、绕过地区限制或提升歌词权限。平台拒绝请求时应在日志/状态页报告需要登录或会员权限，歌词层保持隐藏，也不能切换到标题猜测。

## 频谱输入与麦克风的边界

频谱始终使用 Windows 默认播放端点的 WASAPI shared-mode loopback，它复制系统即将播放的混音。频谱不使用微雪屏端麦克风，也不复用 `standalone-recognition` 的 ALSA 录音。没有 Windows 主机的模式 B 不应声称能够提供现有 WASAPI 频谱。

## 驻留歌词 Theme

`casescreen-theme-build` 会生成包含 `CaseScreen_LyricsCurrent`、`CaseScreen_LyricsNext` 动态文字控件及 `LV` 动态外壳的微雪 Theme。`LV=0` 时文字、面板、边框、样式装饰和进度底轨全部隐藏。模式 A 由 Windows 首次应用后驻留 TF；模式 B 的镜像构建也可预置与分辨率严格匹配的 Theme。

```powershell
go run ./cmd/casescreen-theme-build --ffmpeg ./.tmp/ffmpeg-runtime/ffmpeg.exe --output-dir ./output/standalone-themes
```

输出为 `casescreen-lyrics-1024x600.Theme` 和 `casescreen-lyrics-480x1920.Theme`。目录名 `standalone-themes` 是历史命名，不能据此判断运行模式。

## 硬件门禁与发布状态

`standalone-recognition` 在启动前至少必须验证：可工作的 ALSA 麦克风采集、真机物理网络，或显式启用且严格匹配 `usb0 -> 192.168.77.1` 的无电脑专用 USB Host/Wi-Fi 小桥、DNS、可信系统时间、有效 CA bundle、HTTPS relay 地址和逐屏可撤销 token。普通 Windows RNDIS 共享、任意 USB 默认路由和错误网关仍不被接受。任何一项失败都必须记录原因、关闭识歌链路并回到保留的原厂 `/data/PCMonitorLinux.original`。

ARM32/ARMv7 fingerprint 提取器不是 readiness 硬门禁，但正式低带宽部署强烈推荐配置有许可、与 relay 契约匹配的提取器。缺失时，探针、启动日志和文档都必须明确标记“PCM 高带宽回退”；配置了提取器后，提取失败不得暗中降级为 PCM。

这些门禁是“允许尝试启动”的条件，不是真机兼容性证明。7 英寸与 8.8 英寸机型仍需分别验证实体麦克风/DMIC、ALSA 设备、物理网络或专用小桥的 RNDIS 枚举与供电、独立默认路由、DNS/NTP/TLS、识歌准确率、功耗、温度、长期 TF 缓存以及回退行为。在这些结果完成前，不得把镜像构建成功、单元测试通过或离线节点存在写成真机验收通过。

## 历史实验的定位

EdgeSync、`source-push`、BlueZ/AVRCP 和旧的屏端 direct 标题匹配仍可作为协议研究保留，但不替代上面的两条产品路径。RNDIS 只在显式 `usb-gadget-bridge` 模式中作为无电脑专用小桥的数据链路，不参与识歌或歌曲 ID 推断：

- 模式 A 直接复用原厂 USB 小包，不需要绕行 RNDIS；
- `standalone-recognition` 以音频识别为来源，仍要求可靠的服务端平台映射；
- BlueZ/AVRCP 不携带 QQ/网易云原生歌曲 ID；
- 任何标题匹配链都不能冒充准确歌词。
