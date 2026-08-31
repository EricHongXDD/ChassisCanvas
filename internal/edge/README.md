# CaseScreen 屏端编排服务

`casescreen-edge` 运行在微雪 TF 卡内的 Linux 系统中，链路如下：

```text
主机 Edge Sync
  -> /v1/now-playing：source + track_id + position + playing + lyric_mode 小包
  -> /v1/lyrics：仅切歌或 TF 缓存未命中时，按同平台精确 ID 返回完整歌词
  -> 屏端 edgelyrics.Core
     -> 按 source + track_id 缓存到 TF
     -> 日常只查询屏端本地时间轴
  -> edgedisplay.Proxy
     -> Waveshare SystemData QStringMap
     -> 原厂 PCMonitorLinux
```

元数据同步断开时，不会用空包清除 `Core`，已经加载或命中 TF 缓存的歌词会继续
按本地播放锚点显示。重复网络错误会去重并指数退避。缺少 `track_id` 时只显示
“播放器未提供平台歌曲 ID，无法精确获取歌词”，不会使用标题搜索。纯文本歌词只
显示“该首歌的歌词暂不支持滚动”，整份歌词不会进入高频显示包。

## 启动条件

原厂 `/etc/init.d/qt_app` 不能再直接启动 `./PCMonitorLinux`，否则它会提前持有真实
`/dev/ttyGS0`。应保留 `serial_uart.sh`，先加载原厂 Qt 环境，再 `exec` 本服务：

```sh
source /data/qt_env.sh
exec /data/casescreen-edge \
  -sync-url http://192.168.1.2:19322 \
  -token-file /data/casescreen/sync.token \
  -lyrics-cache /data/casescreen/lyrics-cache \
  -lyrics-mode relay \
  -poll-interval 1s \
  -render-interval 500ms
```

`sync.token` 必须是权限 `0600` 的普通文件，内容为 64 位十六进制配对令牌，可有
一个结尾换行。`relay` 是默认模式，QQ 音乐/网易云音乐登录态与平台请求都留在电脑端；
主机返回 `needs_login` 时副屏会明确提示在电脑端导入登录态或确认会员权限。只有显式指定
`-lyrics-mode direct` 时才使用屏端 `-auth-dir` 和平台网络。进程会继承已由
`qt_env.sh` 设置的 Qt/动态库环境，并由显示代理启动 `/data/PCMonitorLinux`。

本服务没有麦克风、录音、声纹识别或音频上传路径。

## 实验性 BlueZ 模式

`-metadata-mode bluez -lyrics-mode direct` 不再创建 Windows EdgeSync 客户端，但它依赖
真机 BlueZ、联网、CA 和平台登录态。AVRCP 没有 QQ `songmid` 或网易云数字歌曲 ID；
该模式只能用标题、歌手、专辑、时长四项在同平台严格匹配，仍属于推断，不能当作
“来源原生歌曲 ID”模式。正式准确路径应由已配对的播放来源 companion 发送原生 ID，
且不得退化为标题搜索。参见 `docs/standalone-lyrics.md`。

准确来源接收模式可使用电脑端同平台取词（当前 USB/RNDIS 推荐）：

```sh
exec /data/casescreen-edge \
  -metadata-mode source-push \
  -source-listen 0.0.0.0:19323 \
  -source-key-file /data/casescreen/source-push.key \
  -source-stale-after 45s \
  -lyrics-mode relay \
  -sync-url http://192.168.77.1:19322 \
  -token-file /data/casescreen/sync.token \
  -lyrics-cache /data/casescreen/lyrics-cache
```

也可显式选择屏端直连平台：

```sh
exec /data/casescreen-edge \
  -metadata-mode source-push \
  -source-listen 0.0.0.0:19323 \
  -source-key-file /data/casescreen/source-push.key \
  -source-stale-after 45s \
  -lyrics-mode direct \
  -lyrics-cache /data/casescreen/lyrics-cache \
  -auth-dir /data/casescreen/platform-auth
```

`source-push.key` 必须是权限 `0600` 的每屏独立 256 位密钥。接收器只接受通过
HMAC、一次性 challenge、随机 session 和递增 sequence 验证的 QQ/网易云原生 ID
小包；典型正文小于 256 bytes。relay 只在 TF 缓存未命中时从电脑取一次完整歌词，
direct 则要求副屏外网、CA 和屏端登录态。协议见 `docs/source-push-protocol.md`。
