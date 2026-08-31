# 播放来源原生歌曲 ID 小包协议

> 历史实验文档：该屏端直连/companion 协议不属于 CaseScreen 1.0 正式路径，
> 发行包不会包含相关程序、密钥或入口。当前正式实现由 Windows 主机本地取得
> 来源歌曲 ID 和歌词，只向副屏发送当下显示字段及可选的量化进度整数。

该协议供 QQ 音乐或网易云音乐的来源 companion 把原生歌曲 ID 和播放锚点发送给微雪副屏。它不传歌词正文、Cookie、账号密码、音频、图片、歌名搜索结果或麦克风数据。

默认监听地址为 `0.0.0.0:19323`，正文必须是 UTF-8 `application/json`，最大 1 KiB。每台屏幕使用独立的 256 位配对密钥；密钥以 64 位十六进制文本保存，但 HMAC 使用解码后的 32 字节。

仓库提供一次性桥接命令 `casescreen-source-push`，供能直接取得平台原生 ID 的 QQ/网易云适配器调用：

```powershell
.\casescreen-source-push.exe `
  -screen-url http://192.168.77.2:19323 `
  -key-file .\screen-source-push.key `
  -source qq `
  -track-id 0039MnYb0qxYhV `
  -position-ms 42137 `
  -duration-ms 269000 `
  -playing
```

命令没有标题、歌手、搜索、Cookie、麦克风或登录参数；`-track-id` 必须由播放对象本身提供。停止或来源不可用时调用同一命令并加 `-clear`。长期 companion 应直接复用 Go `edgepush.Publisher`，避免每次心跳重新建立会话。

## 1. 获取一次性挑战

```http
GET /v1/source/challenge
```

响应示例：

```json
{"v":1,"challenge":"base64url-no-padding","expires_at_unix_ms":1787769000000}
```

挑战会绑定来源 IP，默认 1 分钟过期，只能用于建立一个会话。

## 2. 建立来源会话

companion 生成 16 个随机字节并编码为无填充 base64url：

```json
{"v":1,"challenge":"上一步的挑战","session":"16字节随机会话的base64url"}
```

请求：

```http
POST /v1/source/session
Content-Type: application/json
X-CaseScreen-MAC: base64url(HMAC-SHA256)
```

签名原文为：

```text
casescreen-edgepush/v1
POST
/v1/source/session
hex(sha256(原始 JSON 正文字节))
```

成功返回 `204 No Content`。完全相同的会话请求可幂等重试。

## 3. 发送当前歌曲小包

QQ 示例：

```json
{"v":1,"available":true,"source":"qq","track_id":"0039MnYb0qxYhV","position_ms":42137,"duration_ms":269000,"position_known":true,"playing":true}
```

网易云示例：

```json
{"v":1,"available":true,"source":"netease","track_id":"186016","position_ms":42137,"duration_ms":269000,"position_known":true,"playing":true}
```

请求：

```http
PUT /v1/source-state
Content-Type: application/json
X-CaseScreen-Session: 当前会话
X-CaseScreen-Sequence: 从 1 开始严格递增的十进制序号
X-CaseScreen-MAC: base64url(HMAC-SHA256)
```

签名原文为：

```text
casescreen-edgepush/v1
PUT
/v1/source-state
session
sequence
hex(sha256(原始 JSON 正文字节))
```

成功返回 `204 No Content`。同一序号和完全相同正文可幂等重试；旧序号或同序号不同正文返回冲突。新会话建立后，旧会话立即失去写权限，旧播放状态也会清空。

可用状态只接受：

- `source="qq"` 且 `track_id` 为合法 QQ `songmid`；
- `source="netease"` 且 `track_id` 为无前导零的正十进制歌曲 ID；
- 已知非负播放位置；位置不能超过非零歌曲时长。

停止或无法确定当前歌曲时发送：

```json
{"v":1,"available":false}
```

## 时序

切歌、暂停、继续、快进和后退时立即发送；播放中建议每 15 秒发送一次心跳。副屏按收到小包的本机单调时间自行推进位置，并每 500 ms 更新滚动歌词。默认 45 秒没有心跳就清空旧状态，所以不需要每秒发送数据。

来源包不发送 `lyric_mode`。副屏按同平台原生 ID 获取歌词后自行判断：有时间戳则滚动，只有纯文本则显示“该首歌的歌词暂不支持滚动”。

当前 USB/RNDIS 硬件推荐把 `source-push` 与 `lyrics-mode=relay` 组合：日常原生 ID/进度仍由上述小包发送；只有换歌且 TF 未命中时，副屏才向电脑 EdgeSync 请求一次完整歌词，电脑使用对应平台登录态获取。`lyrics-mode=direct` 会改为副屏直接访问平台，但必须另外具备外网、DNS、系统时间、CA 根证书和屏端登录态。

## 安全边界

- HMAC 密钥不会出现在请求中；错误信息和日志也不得输出密钥或 MAC。
- challenge、session 和序号防止旧抓包在副屏重启或会话切换后继续生效。
- HTTP 层仍会暴露歌曲 ID 和进度。如果无线网络中的隐私需求更高，应再启用证书指纹固定的 HTTPS；不能以关闭证书校验代替。
- 配对只证明消息来自持有密钥的 companion。真正的“来源原生 ID”仍必须由 QQ/网易云平台适配器保证，不能由 companion 用歌名搜索后伪装。

## 镜像运行模式

- `source-push-relay`：来源小包经 `192.168.77.2:19323` 进入副屏；TF 缓存未命中时，副屏再经 USB RNDIS 的 `192.168.77.1:19322` 让电脑端 EdgeSync 按同平台 ID 获取并返回一次完整歌词。副屏不需要外网、CA 或平台 Cookie，适合当前已验证的 USB 硬件边界。
- `source-push`：保留为副屏直连 QQ/网易云的实验模式，仍需真机 Wi-Fi/网络、DNS、正确时间、CA 和屏端登录态。

`source-push-relay` 过渡期把同一枚每屏独有的 256 位密钥同时用作 source-push HMAC 密钥和 EdgeSync token。这可以减少当前的配对与写盘复杂度，但没有做密钥用途隔离：任一通道的密钥泄露都会同时影响两个通道。密钥必须每屏唯一、不预置公共默认值，泄露时在副屏和电脑 companion 两端一起轮换。后续正式配对界面应为两个通道派生独立子密钥。
