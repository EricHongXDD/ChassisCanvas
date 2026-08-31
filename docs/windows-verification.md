# Windows 正式版 1.0 验证

本清单只验收 Windows 主机控制器。正式版只需同一发布目录中的两个
Windows 程序、原有音乐客户端和 USB 副屏；不需要购买其他硬件、安装
辅助程序或配置副屏网络。

## 自动检查

- `internal/config` 默认配置只含本机 API、遥测、WASAPI 频谱、
  主机歌词、渲染、Windows GPU 首选项和两类 USB 设备字段；示例必须显式为
  `"gpu_preference": "system"`。
- `cmd/casescreen` 和 `cmd/casescreen-desktop` 的依赖图只允许主机 API、
  WASAPI、QQ/网易云只读原生状态、同平台取词、渲染和两类 USB 驱动能力。
- 正式入口不得依赖旧的播放器启动/调试通道、屏端联网程序或外部常驻程序。
- `tools/build_windows.ps1` 只构建两个 Windows GUI 入口，并以白名单复制
  主机文档、主题和必要 DLL/FFmpeg 运行时。
- staging 不得携带平台会话、导入文件、额外可执行程序、部署脚本或服务配置。
- 两个 EXE 在压缩前均检查为 PE Windows GUI Subsystem 2。
- 配置 API 只允许回环监听地址。
- GPU API 接受“系统自动”选择或当前枚举到的稳定适配器 ID；旧版
  `system`、`power_saving`、`high_performance` 请求继续兼容。API 只管理同目录的
  两个固定 EXE；第二个注册值写入失败时必须回滚第一个，并保留其他 DirectX 字段。
- 桌面入口必须在启动服务和创建 Gio 窗口前，把配置中保存的 GPU 选择登记到
  当前发行目录的两个绝对 EXE 路径；换目录导致登记变化时只允许安全自重启一次。
- 工作台必须在所有目标屏下提供独立“程序”页；“检测到的显卡”下拉框首项为
  “系统自动”，后续选项只显示实际适配器名称，并提供保存状态与重启提示。
- 酷冷方向自动测试必须覆盖 1～12 钟面标识到 30° 补偿的完整映射、设备配置
  持久化、校准 API 的目标与鉴权、JPEG 编码前变换以及不同补偿角的缓存隔离。
  这些测试不得打开 HID 或写 Offline Mode 槽位，也不能替代真机验收。

## 版本边界

- 本清单面向当前工作树与正式版 `1.0`。旧版发行包不能作为
  当前 QQ/网易云只读适配器已经进入发行物的证据。
- `go test` 验证的是当前源码工作树。只有重新构建正式版、核对二进制哈希，
  并对同一发布目录完成本清单，才能宣称功能已进入发行物。
- 不得混用不同版本的后台与桌面端；两者的应用版本和本机 API 协议必须一致。
- 既往酷冷实时帧或 Offline Mode 记录不等于圆形预览、磁吸方向、离线 JPG/MP4
  补偿已经验收；方向功能只能在完成下述专项步骤后记录为通过。

## Windows 真机验收

1. 启动后检查桌面工作台按微雪 7 英寸、微雪 8.8 英寸、酷冷至尊的顺序显示目标屏。右侧“程序”页的下拉框应显示
   “系统自动”和本机实际物理显卡名称；保存时同时登记
   `CaseScreen.exe` 与 `CaseScreenDesktop.exe`，明确提示退出并重新打开后生效。
   把完整发行目录复制到新路径后首次启动，应自动把已保存选择迁移到新路径并至多
   自重启一次；新路径的两个注册值必须与配置一致。这只能验收 Windows 调度首选项，
   不得宣称强制固定到某块显卡。
   点击工作台 `X` 后窗口应隐藏，后台与设备连接继续运行；从系统托盘可重新
   打开。托盘右键“完全退出”后，工作台及其托管的 `CaseScreen.exe` 都必须结束。
   快速连续启动两次 `CaseScreenDesktop.exe`，以及窗口隐藏后再启动，都只允许一个工作台和一个桌面托盘。先单独启动同目录同版 `CaseScreen.exe` 的 tray 与 headless 模式后再打开工作台，原服务应被身份核验后接管，不得留下双托盘或无入口后台。
2. 播放 Windows 默认 Console 输出的声音，确认条状/环状频谱跟随
   实际混音，暂停后在静音门限内归零。分别调整样式、方向、频带数、间距、
   粗细、内径、渐变、灵敏度、增益、幅度范围、平滑度、静音门限、峰谷对比、
   邻频分离、低频冲击、公共底座抑制、柔光和峰值保持，分别确认细胶囊霓虹柱、居中双向波形和闭合锯齿波环在预览及目标设备按协议能力生效。静音时柱高和波形必须归零，不得保留正弦假动画或固定波峰。重新应用主题后，完整 48 频带必须使用三字符短键；典型两位值预算应为 10 FPS，最坏三位值约为 9.4 FPS。相同变换的条状和环状图层必须共享同一组键，不能因自定义增益、静音门限等参数退化成长哈希键。
3. 确认 CaseScreen 只打开默认播放端点的 WASAPI loopback，没有打开
   Capture、录音或输入端点。
4. 分别用 QQ 音乐和网易云音乐验证换歌、暂停、恢复和拖动。无可靠原生 ID
   时必须失败关闭，不得以标题搜索候选替代。

   QQ 音乐只需官方 Windows 客户端。在未改动客户端的环境中运行：

   ```powershell
   $env:CASESCREEN_QQ_MEMORY_LIVE = "1"
   go test -v -buildvcs=false -count=1 ./internal/lyrics -run '^TestQQProcessMemoryNativeIDLive$'
   ```

   网易云音乐也只需已经正常运行的官方 Windows 客户端；无需退出、重启或
   通过 CaseScreen 启动。运行：

   ```powershell
   $env:CASESCREEN_NETEASE_MEMORY_LIVE = "1"
   go test -v -buildvcs=false -count=1 ./internal/lyrics -run '^TestNeteaseProcessMemoryNativeIDLive$'
   ```

   两项内存门禁都只允许 `PROCESS_QUERY_INFORMATION | PROCESS_VM_READ`。
   模块超限、分块短读、候选不唯一、特征失配、结构越界或多进程证据冲突
   必须整轮失败关闭。QQ 必须与同一轮 GSMTC 严格一致；网易云必须用内存中的
   当前 ID、状态和时间轴，只按同 ID 有界读取 `playingList` 元数据，并在
   GSMTC 可用时增加交叉核对。客户端更新后两项都必须重新验收。

   再对当前平台运行端到端精确身份门禁：

   ```powershell
   $env:CASESCREEN_PLAYBACK_LIVE = "1"
   $env:CASESCREEN_PLAYBACK_EXPECT_SOURCE = "netease" # 或 qq
   $env:CASESCREEN_PLAYBACK_EXPECT_STATE = "playing"  # 或 paused
   # 可选：已知期望 ID 时再锁定精确值
   $env:CASESCREEN_PLAYBACK_EXPECT_TRACK_ID = "757567"
   go test -v -buildvcs=false -count=1 ./internal/lyrics -run '^TestWindowsNowPlayingExactIDLive$'
   ```

   实际媒体会话必须命中目标平台、提供 source-origin ID 和可用位置。播放时
   位置应推进；暂停时位置应冻结。歌曲 ID 只用于主机向同一平台精确取词，
   不进入副屏数据包。

   最后运行不会自动控制播放器的人工转换门禁：

   ```powershell
   $env:CASESCREEN_PLAYBACK_TRANSITIONS_LIVE = "1"
   $env:CASESCREEN_PLAYBACK_EXPECT_SOURCE = "netease" # 或 qq
   $env:CASESCREEN_PLAYBACK_STEP_TIMEOUT = "60s"
   go test -v -timeout 6m -buildvcs=false -count=1 ./internal/lyrics -run '^TestWindowsNowPlayingTransitionLive$'
   ```

   测试会依次提示人工切歌、暂停、恢复和拖动；不得自行改动播放器状态。
5. 对有时间轴歌词检查滚动；对纯文本歌词检查固定提示
   “该首歌的歌词暂不支持滚动”。切换四种歌词样式并检查当前/下一句层次、
   字号、颜色、描边、阴影、柔光、面板、淡入与进度轨；酷冷检查句内
   渐进高亮，微雪检查驻留 Theme 的字体和多层文字效果。
6. 解包真实歌词事件并确认基础字段为 `CaseScreen_LyricsCurrent`、
   `CaseScreen_LyricsNext`、`CaseScreen_LyricsCurrentAtMs`、
   `CaseScreen_LyricsNextAtMs`、`CaseScreen_LyricsPlaying` 和可见键 `LV`
   六个键；微雪主题启用进度轨时允许出现
   `LFP` 细进度整数键及按需发送的旧兼容进度键。频谱可见时 `LFP` 应并入约
   9～10 Hz 的完整频谱 `QStringMap`，不得再并行发送
   第二个细进度包；无频谱时才允许最高 20 Hz 的单键包。兼容旧键不得并入频谱，只在跨整百分比或状态事件时单独发送。暂停事件只发一次
   `Playing=false`、`LV=1`、未改变的当前句和最终进度，随后不再周期发送。
   切歌取词中、无歌词或当前句尚未出现时必须收到显式清空包：`LV=0`、空双行、
   空时间戳、`Playing=false` 以及新旧进度归零；不得出现任何等待占位内容。
   纯文本歌词应保持 `LV=1` 并显示固定说明。任何包都不得包含连续位置、整份歌词、
   歌曲 ID、Cookie 或 PCM。
7. 微雪断线重连后确认按 2/4/8/16/30 秒有界指数退避，30 秒封顶且成功后
   复位；恢复待生效或当前主题，只补最新遥测、频谱和歌词状态，驻留资源不
   重复上传，旧频谱帧不排队。以新进程启动时间为界检查
   `Microsoft-Windows-Kernel-PnP/Device Management` 的 1010/1011 事件；
   同一 COM3 主题连续播放 30～60 分钟并执行一次变更资源上传，期间不得新增
   对应微雪父复合设备的意外删除/失败事件。
8. 酷冷的动态主题应以变化后 JPEG 帧正常刷新；静态/视频背景可单独验证
   Offline Mode 槽位。连续跨过多个 5 秒健康检查周期，设备不得因 `param`
   无响应而误判掉线、重复重连或闪烁；真实 HID 读写失败仍应进入断线重连。
9. 对酷冷执行方向专项验收。先记录发行包哈希、设备 ID、当前磁吸安装位置、
   Offline Mode 槽位号及槽位内原媒体指纹。工作台目标必须显示 480×480 圆形
   有效区，外侧尖角朝下六边形只能作为外壳参考，不能把六边形误当成 LCD 裁切。
   打开方向校准后，屏幕应以原始像素方向显示普通钟面 1～12 和向下箭头；测试
   图的原始逻辑下方必须是 `6`。只有原始像素下方恰好对准外壳物理尖角时，尖角
   旁才会看到 `6`；其它安装位置应选择实际看到的数字，不应要求用户输入角度或
   修改主题 JSON。

   仅显示测试图、等待或取消期间，原槽位媒体和指纹均不得改变，日志中
   不得出现由校准触发的槽位删除或上传。取消应立即恢复当前主题；重新打开测试
   图后不做操作并等待至少 2 分钟，测试图仍应持续显示，直到显式取消或保存。随后使用同一份带明显“上/下”标记的
   主题分别验收动态实时 JPEG、离线静态 JPG 和离线 MP4，三种路径都应相对物理
   下方保持同一正向；MP4 验收环境须包含发行包的 FFmpeg。保存方向会重新应用
   当前主题，当前为离线主题时允许按新方向正常更新槽位，必须与“测试图本身不写
   槽位”分开记录。保存后切换主题、重插
   USB、完全退出并重启程序，方向仍应保持。导出的主题 JSON 不得出现
   `display_rotation`、`orientation_calibrated` 或 `orientation_profile`；这些字段
   只能出现在对应设备配置中。记录每条路径的照片、所选数字和结果，未完成任一
   项时不得写“方向真机验收通过”。完整判据见
   [酷冷至尊显示几何与方向校准](coolermaster-orientation.md)。
10. 需要受限歌词时，登录与登录态导入只在 Windows 上发生；从 TF、
   设备配置、设备传输包和发行 ZIP 中均不应找到 Cookie 或平台密钥。
   成功导入后删除源 `cookie.txt` 并重启服务，登录态仍应由当前 Windows
   用户的加密存储复用；平台拒绝后状态应变为 `needs_login`。
11. 分别选择三台副屏及“全部副屏”执行关屏和恢复。关屏必须发送亮度 `0`，
     恢复使用各设备默认亮度；状态 API 在关屏时仍须明确返回
     `current_brightness: 0` 与 `powered_off: true`。酷冷主机合成主题关屏后不得
    继续编码或发送 JPEG，恢复后应立即显示当前帧。关屏期间重插设备或应用主题，
    重连/主题提交后必须再次恢复为亮度 `0`；完全退出并重启程序后也必须保持同一
    关屏意图。验收结论只能写“亮度黑屏”，不能
    写成物理断电。

## 小包节流验收

- 主机按播放器时间轴本地选句，副屏接收包含 `LV` 的基础六字段；启用微雪进度轨时
  另收 `LFP`（0～10000），并按需发送 `CaseScreen_LyricsProgress`（0～100）兼容旧 Theme。
- 播放期间不发送连续位置；基础状态只有换行、暂停、恢复、拖动、切歌、可见性变化、重连
  或 Theme 重载需要补状态时才发送。有频谱时 `LFP` 合并进约 9～10 Hz 的完整频谱包；
  无频谱时才独立发送且最高 20 Hz，并按原生轨道可见步进去重。
- 已 ACK 的相同载荷不得重复上传；等待/无歌词和关闭歌词都发送显式清空复位包，
  其中 `LV=0`，而暂停且当前句非空时不得清空。
- 背景媒体和完整歌词均不得混入日常歌词事件。
- 微雪频谱按实际 `CommandSystemData` 命令和最小 ACK 总长度计算发送间隔，
  使用 85% 瞬时线速预算且最短间隔为 100 ms；串口忙时只保留调用方下一次生成的新帧，不建立旧帧队列。
- 每秒一次的典型遥测为 48 字段、命令与最小 ACK 合计约 1732 字节；与频谱
  争用时必须优先于可丢弃的频谱帧；歌词、控制和 ACK 不得被旧频谱帧排队阻塞。
- 使用 README 中两份 clean expanded 8GB 镜像验证；本功能不得依赖或要求
  `CaseScreen-beta.6` edge-display 代理镜像。

## 发布判定

只有主机测试、Windows GUI 构建、依赖图审计、staging 门禁和目标
USB 设备验证通过后，才能声明 Windows 正式包可用。酷冷方向能力还必须附带
第 9 项的照片、标识数字、槽位未改写证据和三条媒体路径结果，不能用自动测试
或既往酷冷记录替代。
