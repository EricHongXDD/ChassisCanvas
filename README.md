# ChassisCanvas 机箱副屏统一控制与主题工作台

> **正式发布版本：** `1.0.2`
>
> **兼容名称：** 当前 Windows 可执行文件、配置目录和协议键仍沿用 `CaseScreen`，避免破坏既有设备与主题；GitHub 项目正式名称为 `ChassisCanvas`。

Windows 安装版见 [v1.0.2 Release](https://github.com/EricHongXDD/ChassisCanvas/releases/tag/v1.0.2) 中的
`ChassisCanvas-1.0.2-Core-Setup.msi`：双击后按向导安装到当前用户目录，提供开始菜单入口、卸载入口和可选的开机启动。
公开核心安装版不含 FFmpeg、hidapi DLL 或厂商 Theme；与 ZIP 核心版的功能边界相同。
已有本机完整安装版的用户应使用本机生成的 `CaseScreen-1.0.2-Setup.msi` 升级，以保留现有运行时。

安装包可用 WiX 3.14 和 `tools/build_msi.ps1 -Version 1.0.2 -SourceDirectory <发行目录>` 构建；
只有为自己的设备打包现有完整发行目录时才使用 `-PrivateLocal`，该产物不应上传公开 Release。

ChassisCanvas 是面向 Windows 的机箱副屏统一控制与主题创作软件，支持：

- 微雪 7 英寸与 8.8 英寸 USB Monitor（CDC ACM / 屏内 Linux 渲染）；
- 酷冷至尊冷龙影二代 LCD（`2516:0222` / 双 HID，480×480 圆形有效显示区）。

## 当前主路线

微雪采用“静态主题驻留 TF、动态数据由电脑发小包”的方式。背景图片、视频和原生控件只在首次使用或内容变化时写入 `.Theme`；连接、重启和重复应用相同主题时，会按大小与 CRC 复用 TF 中的文件。日常遥测、系统音频频带和歌词通过原厂 `CommandSystemData` 命令发送 `QStringMap`，不重复传输背景媒体或完整合成帧。

歌词也按这条路线工作：

```text
Windows QQ 音乐 / 网易云音乐
  -> QQ：GSMTC 时间轴 + 主机只读原生 ID 适配器
  -> 网易云：主机只读原生 ID / 状态 / 时间轴 + playingList 同 ID 元数据
  -> Windows 主机在同一平台获取并缓存歌词
  -> 主机定时选句 -> 双行文字、行时间戳、播放状态、可见键 LV 和可选细进度整数小包
  -> USB CommandSystemData
  -> 微雪 TF 中的驻留歌词 Theme
```

- QQ 音乐最终只用 QQ `songmid` 取词。CaseScreen 主进程以 `PROCESS_VM_READ` 对 `QQMusic.exe` 做有界只读检查；只有 `QQMusic.dll` 整体不超过 20 MiB 时才完整扫描，模块超限、任一分块读取失败或多进程证据不一致都立即失败关闭，不接受前缀结果。读取的数值 songId、曲名、全部歌手、专辑、时长、位置和播放状态必须与同一轮 GSMTC 严格一致，随后只按该 songId 换取、再次核对 songmid。
- 网易云音乐由 CaseScreen 主进程以查询和 `PROCESS_VM_READ` 权限读取官方 `cloudmusic.exe`/`cloudmusic.dll` 的当前播放结构，取得十进制歌曲 ID、位置、时长和播放状态；再只按这个精确 ID 从官方客户端本机 `playingList` 中取同 ID 的曲名、全部歌手、专辑和时长。文件读取有大小、条目和字段上限，同 ID 元数据冲突时失败关闭；GSMTC 若存在则作为额外交叉证据，不作为该客户端的硬依赖。CaseScreen 不写进程内存、不注入 DLL、不启动调试端口，也不读取 Cookie、账号或历史来推断歌曲。两类平台 ID 都只在 Windows 主机内部使用，不会发给副屏，也不会用标题搜索候选冒充准确 ID。
- 有时间轴的 LRC/QRC 由主机按播放器位置选择当前句和下一句。歌词状态事件包含 `CaseScreen_LyricsCurrent`、`CaseScreen_LyricsNext`、`CaseScreen_LyricsCurrentAtMs`、`CaseScreen_LyricsNextAtMs`、`CaseScreen_LyricsPlaying`，以及只占一个短标量的可见键 `LV`；新版微雪进度轨使用短键 `LFP` 的 0～10000 整数，兼容键 `CaseScreen_LyricsProgress` 仍保留为 0～100。不会发送整份歌词、连续播放位置、歌曲 ID、Cookie 或 PCM 音频。
- 从不含 `LV` 的旧版本升级后，应对每块微雪屏在编辑器中重新应用一次当前画布。模板库和轮播会在激活旧歌词模板时自动重编译；缺少源素材时会明确要求重新保存，不会静默复用已经烘焙歌词底板的旧 Theme。
- 主机在本地定时换行。暂停、恢复、拖动、切歌或双行/时间戳变化会立即生成一次歌词状态事件。频谱可见时，`LFP` 以约 9～10 Hz 合并进完整 48 频段包；频谱不可见时，才会以最高 20 Hz 发送独立细进度小包。兼容旧键不进入频谱包，只在跨整百分比或状态事件时单独发送。暂停且已有当前句时保持 `LV=1` 和当前歌词，发送 `Playing=false` 与最终进度后停止周期发送。切歌取词中、无歌词或当前句尚未出现时发送显式清空包：`LV=0`、双行与时间戳为空、`Playing=false`、新旧进度归零，因此整个歌词图层不会残留；纯文本不可滚动时仍以 `LV=1` 显示固定说明。设备 ACK 过的完全相同载荷不会反复发送，串口发送频率还会按载荷大小自适应，保留至少 15% 的 115200 bps 线速余量。旧 Theme 可继续显示 0～100 进度；重新应用由当前 1.0 编译的 Theme 后启用细进度和整层隐藏。
- 平台只有纯文本歌词时不伪造时间轴，固定显示“该首歌的歌词暂不支持滚动”。每句发送前会压缩空白并限制长度，避免异常歌词放大小包。
- 公开歌词默认不要求登录；只有平台实际拒绝后才标记“需要登录或会员权限”。用户在官方页登录后仍需主动导出并导入一次 `cookie.txt`；它只是本地导入载体，不是一次性 Cookie。导入成功后主机会跨启动加密复用，只有过期、被平台撤销或再次拒绝时才需更新；源文件不会被持续读取，可由用户自行妥善删除。打开网页不会自动连接 CaseScreen。会话 AES-GCM 数据和受当前 Windows 用户 DPAPI 保护的密钥只留在主机，账号密码、Cookie 和平台密钥都不会下发到副屏或写入 TF。登录只继承账号已有权限，不会自动获得 VIP。
- 正式 Windows 版的歌曲状态、平台取词、登录态加密和歌词计时全部在当前电脑完成；副屏只通过原有 USB 连接接收显示小包。它不需要网络、账号或任何音频输入，也无需购买、连接或安装任何额外设备或辅助程序。
- 7 英寸与 8.8 英寸原厂镜像所需字脸已经逐路径只读核验，详见 [微雪原厂镜像字体清单核验](docs/waveshare-font-inventory.md)；字体样式修复不要求再次修改或重刷镜像。
- Windows 生产进程只接受 QQ 音乐或网易云音乐可验证的原生歌曲 ID；旧配置会迁移为严格 `auto`，未知播放器不会发起在线歌词查询。本地 LRC 仍可由用户明确选择。

当前平台适配状态必须区分“歌词接口支持”和“正在播放歌曲的准确 ID 可获得”：

> **发行边界：** 下文对应正式版 `1.0.2` 的当前源码与发布边界。从源码运行 `go test` 只验证工作树；只有 `CaseScreen-windows-x64-1.0.2.zip`、二进制哈希及 [Windows 正式版验证](docs/windows-verification.md) 均对应同一次构建，才能宣称相应适配器已经进入发行物。

- 网易云：使用无需重启客户端、无需调试端口和额外常驻程序的主机内置只读内存适配器。它只接受唯一且完整的当前播放 ID、状态和时间轴，并按同一 ID 读取有界 `playingList` 元数据；模块版本、特征、结构、进程、同 ID 元数据或可选 GSMTC 证据存在歧义时立即失败关闭，不会回退到历史数据库、窗口标题或标题搜索冒充当前歌曲。
- QQ：只使用无需额外常驻程序或 DLL 注入的 `QQMusic.exe` 只读内存适配器。它只申请查询和读权限、最多检查 8 个同名进程；仅当 `QQMusic.dll` 整体不超过 20 MiB 时才完整扫描并缓存已验证结构地址，切歌日常只读 116 字节结构和有界 UTF-8 字段。模块超限、分块短读、候选负缓存或多进程证据不一致均整轮失败关闭。通过后按 songId 查询单曲详情，严格核对标题、全部歌手、专辑和时长，再取得 songmid 与同平台歌词。已在线验证 `songId=97773` 精确映射到 `songmid=0039MnYb0qxYhV`；映射使用固定 512 首的有界内存缓存。内存结构不是腾讯公开稳定 IPC，客户端更新后必须重新真机验收，不能退化为按歌名猜测。

酷冷至尊固件没有发现可让驻留控件消费上述 `QStringMap` 的接口。它显示歌词时仍由 Windows 合成变化后的完整 JPEG 帧，因此不能获得微雪小键值包同等的带宽收益；纯图片/视频背景仍可独立使用 Offline Mode 槽位。

酷冷的传输缓冲区是 480×480，实际可见 LCD 是圆形；尖角朝下的六边形是磁吸外壳和安装正向参考，不是六边形像素屏。工作台按圆形有效区预览，并用尖角朝下的外框提示安装方向。磁吸模块转位后只需做一次设备级方向校准：测试图按钟面显示 1～12，原始方向的下方是 6；用户选择当前外壳下方尖角旁的数字。该补偿保存在设备配置中，不进入主题 JSON，并统一用于实时 JPEG、离线 JPG 和离线 MP4。测试图只走实时帧，不写或删除 Offline Mode 槽位；显示前必须有可恢复的当前主题。测试图不会倒计时消失，只在确认保存、取消恢复、用户切换目标或完全退出软件时恢复主题；USB 重连会重新显示测试图。如遇 USB/素材处理失败，服务保留校准会话，工作台可继续确认或取消。详细行为与待执行验收见 [酷冷至尊显示几何与方向校准](docs/coolermaster-orientation.md)。

正式数据流、进程与发布边界见 [Windows 正式版架构](docs/windows-production.md)，
精确歌曲 ID、滚动和 USB 小包见 [Windows 主机歌词同步](docs/host-lyrics.md)，
发布门禁见 [Windows 正式版验证](docs/windows-verification.md)。

## 主要能力

- 微雪每屏独立模板库、顺序/随机轮播和每模板独立时长；重复激活已驻留模板不重传资源。
- 可视化编辑器支持遥测、自定义文本、时钟/日期、进度条、仪表、折线图、形状、条状/环状频谱和双行歌词，坐标按目标屏物理像素编辑。歌词提供流光播放器、卡拉 OK、经典双行和极简四种样式，可调当前/下一句字号和颜色、字体与字重、描边、阴影、柔光、面板、淡入和细进度轨；酷冷的主机合成帧支持按句内进度渐进高亮，微雪把字体、面板、多层文字效果和可选进度轨编译进驻留 Theme。频谱可见时细进度合并进约 9～10 Hz 的完整频谱包；无频谱时独立细进度小包最高 20 Hz，并按可见步进去重。
- 桌面工作台新增“预设”页，内置遥测 4 种、条状频谱 7 种、环状频谱 6 种、歌词 4 种不同布局与配色。选中同类图层后点击预设会直接套用；未选中同类图层时，会在画布中心创建对应图层。预设仅修改外观，不改变遥测绑定和音频动态参数。
- 微雪字体控件使用两张原厂镜像共同存在的真实屏内字体路径；歌词若选择不含完整中文的数码或等宽字体会安全回退到 Source Han，粗体优先选择屏内真实粗体文件，并保留动态描边，避免 Windows 字体族名导致屏端缺字、过细或只显示背景。
- 微雪设置页可读取主题清单及 TF 总量、已用和剩余容量。容量在连接时读取，也可由用户手动刷新；歌词播放期间默认不做 10 秒后台轮询，避免占用串口。7 英寸 COM3 已实测容量查询与驻留主题 CRC 去重；8.8 英寸 COM4 已实测识别 8GB 扩容分区、容量查询与相同主题快速复用。
- 酷冷至尊支持 Atmos V2 离线图片/MP4 五槽位、持久化指纹去重、Offline Mode 与实时 JPEG；酷冷槽位可配置为槽位 4。480×480 画布按圆形 LCD 有效区裁切，尖角朝下六边形只作为外壳方向参考。一次设备级方向校准会同时补偿实时帧、离线 JPG 和离线 MP4，切换主题不需要重复校准。
- 纯 Go WASAPI 共享模式回环读取 Windows 默认主输出（Console 角色）的最终混音，再以 Hann 窗 FFT 驱动条状/环状频谱；它只访问播放端点，不打开录音或输入端点、不创建虚拟输入，也不会占用或改写电脑原有声音输出。
- `/api/status.audio` 会显示实际 Console 默认输出端点、采集时间、电平、峰值与最多 64 个频带；后台也会直接显示端点和峰值/静音。如某个播放器被 Windows“音量合成器”单独路由到另一个输出，它不属于当前默认端点的混音；将该应用输出改为系统默认或与状态中端点一致即可。
- 频谱分析最多 30 FPS，并以固定静音门限过滤端点底噪；微雪服务每 100 ms 只生成最新量化频带，串口忙时直接丢弃旧实时帧而不排队。新编译的驻留主题会为主题内不同音频变换分配确定性的三字符短键，相同变换的条状/环状图层共享 48 个值，不再为每个频带重复发送长哈希。115200 波特模型使用 85% 的瞬时频谱预算，遥测、歌词和 ACK 通过串口互斥自然抢占；完整 48 频带典型两位值约 10 FPS、最坏三位值约 9.4 FPS。旧 TF 主题在再次应用前继续使用原键并保持兼容；重新应用后自动编译为短键。相同 ACK 载荷和静音后的重复零值不会持续占用 USB。
- 透明/黑/白画布与图片、遥测、频谱、歌词图层主机合成；图片可保持比例、缩放和旋转。
- 音频与歌词可即时启停；关闭音频会释放 WASAPI，关闭歌词会停止播放器状态轮询并只发送一次文字/时间戳/播放状态及进度归零复位包，避免驻留控件保留旧句；歌词失败会保留诊断并按退避策略重试。
- 两类设备断线后按 2/4/8/16/30 秒有界指数退避自动重连，30 秒封顶，成功后复位；恢复时重新应用待生效或当前主题与最新状态，微雪已驻留资源不会因为重连再次上传，过期频谱帧不会排队补发。
- 工作台可对当前副屏或全部副屏发送已确认的亮度命令，以亮度 `0` 实现黑屏；恢复时使用各驱动声明的默认亮度。关屏意图会写入配置，在断线重连、完全退出和系统重启后继续重放；关屏中的主机合成屏会暂停 JPEG 渲染和传输。这里的“关屏”是固件亮度为零，不代表切断 USB 供电或面板物理断电。
- 本机 HTTP 控制台、托盘开关、主题 JSON 导入、桌面可视化主题工作台和上次主题自动恢复。
- 点击桌面工作台右上角 `X` 只隐藏窗口并继续后台服务；可从 Windows 系统托盘重新打开，托盘右键菜单中的“完全退出”才会同时结束工作台及其托管服务。
- 工作台右侧“程序”页提供“检测到的显卡”下拉框，首项为“系统自动”，其余选项直接显示 Windows 枚举到的物理适配器名称。保存会把所选适配器映射为 Windows 支持的逐程序调度类别，同时持久化选择并统一登记 `CaseScreen.exe` 与 `CaseScreenDesktop.exe`；该值仍是 Windows 调度提示，不保证强制固定到某块显卡，保存后需退出并重新打开两个程序。

酷冷的 480×480 实时帧与 Offline Mode 恢复已有既往真机记录；自动测试不会写入离线槽位。该记录不包含本次圆形几何、磁吸方向校准或三条媒体路径的一致性验收，不能据此宣称方向功能已经通过真机验收。酷冷离线资源和实时渲染是互斥模式：静态/视频背景可以驻留设备，包含遥测、频谱或歌词动态图层时需要切换为实时 JPEG；方向验收必须按 [Windows 正式版验证](docs/windows-verification.md) 重新执行并记录。

## 微雪 8GB TF 卡镜像

两份干净扩容镜像是按型号独立发布的 TF 卡资产，不进入 Windows 程序 ZIP；一张卡只写对应的一份：

- 7 英寸：`7inch USB Monitor-240719-001-8GB-expanded.img`，7,800,000,000 字节，SHA-256 `13cc5e9661fd22fffe35694bce61cb37144d23733c43a643885682e1ad5f0d24`；
- 8.8 英寸：`8.8inch USB Monitor-240719-001-8GB-expanded.img`，7,800,000,000 字节，SHA-256 `d43c6da5c9e227cf175d9dbb9a46333e3d485cd617669112db1a5878523653bf`。

它们保留原厂屏内系统并扩大资源分区。歌词、遥测和频谱由 Windows 正式程序通过原厂 USB 小包驱动，背景、字体与 Theme 首次写入 TF，因此不需要另刷“歌词版”镜像。文件名含 `CaseScreen-beta.6` 的旧实验镜像不属于当前主路线。刷写会覆盖所选 TF 卡，必须先核对型号、目标磁盘和 SHA-256。

## 构建与测试

```powershell
chcp 65001 > $null
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$env:GOTELEMETRY = 'off'
$cacheDirectories = @('.\.gocache', '.\.gomodcache', '.\.gopath')
$cacheDirectories | ForEach-Object { New-Item -ItemType Directory -Path $_ -Force | Out-Null }
$env:GOCACHE = (Resolve-Path '.\.gocache').Path
$env:GOMODCACHE = (Resolve-Path '.\.gomodcache').Path
$env:GOPATH = (Resolve-Path '.\.gopath').Path

go test -buildvcs=false ./...
.\tools\build_windows.ps1
```

## GitHub 公开发布

推送与源码版本一致的标签（例如 `v1.0.2`）后，Release Action 会在 GitHub 托管的 Windows 环境中重新测试并构建 `ChassisCanvas-windows-x64-1.0.2-core.zip`，同时生成 SHA-256 校验文件并创建草稿 Release，待真机验收后由维护者发布。手动运行工作流只生成 Actions 构建产物，不创建 Release。

公开核心包不会包含来源未核验的 FFmpeg、hidapi、厂商安装程序、微雪原厂 Theme 或 TF 卡镜像。包内程序仍使用兼容文件名 `CaseScreen.exe` 和 `CaseScreenDesktop.exe`；视频处理、酷冷至尊设备支持与微雪主题编译所需文件，应由用户从自己有权使用的设备、原厂软件或其他许可清晰的来源取得。详细边界见 [第三方运行时说明](docs/third-party-notices.md)。

在本机仅生成同样的公开核心包：

```powershell
.\tools\build_windows.ps1 -Version 1.0.2 -PublicCore
```

发行脚本使用 Windows GUI 子系统构建，双击启动只显示托盘图标，不弹出控制台窗口；诊断日志写在配置文件旁的 `casescreen.log`。

酷冷至尊驱动需要把 `hidapi.dll` 放在 `CaseScreen.exe` 同目录；视频背景需要可运行的 `ffmpeg.exe` 及其共享 DLL。发行包会整理这些运行时文件。

## 运行

托盘模式：

```powershell
.\artifacts\CaseScreen.exe
```

无托盘调试模式：

```powershell
.\artifacts\CaseScreen.exe -headless -config .\device-config.json
```

桌面主题工作台：

```powershell
.\artifacts\CaseScreenDesktop.exe
```

桌面端是纯 Go Gio 原生窗口，不需要 Python 或 PySide6。它会自动启动同目录的桌面托管服务；如果同目录已有经版本与进程身份核验的同版手动 tray/headless 服务，会先结束它再转为桌面托管，避免双托盘或“完全退出”后留下无入口后台。顶部选择目标屏，左侧添加组件，中间画布拖动、缩放与旋转，右侧在选中图层后设置文件、数据、样式、字体、颜色、可见性和层级。选中微雪副屏时，右侧还有独立的“模板库”和“设置”页签；所有目标下都能打开“程序”页选择 Windows GPU 首选项。只有用户主动打开主机端音乐平台登录时才会调用系统浏览器。

服务默认只监听 `127.0.0.1:19321`。启动后可从托盘打开控制台，或在浏览器访问该地址。

## 设备配置示例

正式 Windows 配置只包含主机遥测、系统输出频谱、主机歌词、主题和两类 USB 设备字段。设备控制使用 CDC ACM 或 HID 协议；只需连接原有 USB 副屏，不需要为歌词或频谱再配置网络、音频输入或其他硬件。

```json
{
  "listen_address": "127.0.0.1:19321",
  "telemetry_enabled": true,
  "telemetry_interval_ms": 1000,
  "audio_enabled": true,
  "audio_bands": 32,
  "lyrics_enabled": true,
  "host_render_fps": 20,
  "jpeg_quality": 82,
  "gpu_preference": "system",
  "ffmpeg_path": "C:\\path\\to\\ffmpeg.exe",
  "start_minimized": true,
  "devices": [
    {
      "id": "waveshare-7",
      "name": "微雪 7 英寸",
      "kind": "waveshare",
      "enabled": true,
      "port": "COM3",
      "width": 1024,
      "height": 600
    },
    {
      "id": "waveshare-8.8",
      "name": "微雪 8.8 英寸（竖屏）",
      "kind": "waveshare",
      "enabled": false,
      "port": "COM4",
      "width": 480,
      "height": 1920
    },
    {
      "id": "coolermaster-atmos-v2",
      "name": "酷冷至尊冷龙影二代",
      "kind": "coolermaster",
      "enabled": true,
      "library_path": "C:\\path\\to\\hidapi.dll",
      "width": 480,
      "height": 480,
      "slot": 4,
      "display_rotation": 0,
      "orientation_calibrated": false,
      "orientation_profile": 0
    }
  ]
}
```

`display_rotation`、`orientation_calibrated` 和 `orientation_profile` 属于酷冷设备配置；主题 JSON 仍只描述逻辑画布和图层。导入、导出或切换主题都不得携带或覆盖设备安装方向。

`tools/build_windows.ps1` 只发布 `CaseScreen.exe`、`CaseScreenDesktop.exe` 和两者所需的主题、文档及运行库；正式安装与使用不需要另外部署服务、脚本或硬件。
