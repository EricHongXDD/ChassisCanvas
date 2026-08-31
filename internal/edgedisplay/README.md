# 微雪屏端本地显示注入层

原厂启动链已经确认是：

```text
/etc/init.d/qt_app
  -> /data/serial_uart.sh
  -> /dev/ttyGS0
  -> /data/PCMonitorLinux
```

`PCMonitorLinux` 把 `/dev/ttyGS0` 写死在程序内。代理因此创建一对 PTY，临时把
`/dev/ttyGS0` 指向 PTY 从端，然后由自己启动原厂程序。代理持有 PTY 主端并复用
现有 Waveshare `SystemData + QMap<QString, QString>` 编码，所以原厂主题渲染器不需要
反编译或重写。

原字符设备移到 `/dev/ttyGS0.casescreen-real` 后不会闲置。代理会重新打开它，并用
单一协议协调器在电脑端命令和屏端歌词注入之间串行化：电脑端普通命令及原厂 ACK
双向透传；`FileStart` ACK 之后严格按声明长度转发裸字节，并独占到 `FileEnd` ACK，
因此主题上传期间歌词小包只排队、不插入文件流。电脑端命令默认最多等待 10 秒，
文件裸流连续 15 秒无读写即终止，两个阈值都可通过启动参数调整。
真实 gadget 每次打开都会切换到 8-bit raw 且保持非阻塞，避免 `ICANON`、`ECHO`、
`OPOST`、`ICRNL` 或软件流控缓存/改写二进制协议；退出恢复设备节点前会还原接管前
保存的 termios。

协议没有事务 ID。若命令已经写入 PTY 却发生 ACK 超时/EOF，或文件裸流中断，代理
无法无歧义判断迟到 ACK 属于哪次事务，因此会把它视为致命失步：关闭桥、终止原厂
进程并恢复真实设备节点，交给上层监督器整体重启。只有普通命令 ACK 已从原厂完整
读出、但回写电脑端失败时，才只重连真实 gadget 会话。

## 生命周期安全

- 启动前通过 `flock` 保证同一时间只有一个代理接管设备节点；
- 原字符设备移动到同目录备份后，使用临时链接加原子重命名安装 PTY；
- 任一步失败都会立即恢复原设备节点；
- 原厂程序退出、收到 SIGTERM 或输入结束时都会关闭 PTY并恢复节点；
- 恢复时只有链接仍指向本代理 PTY 才会删除，外部替换后的节点绝不会被覆盖；
- 上次进程被 SIGKILL 留下备份时，新实例会在取得独占锁后识别 `/dev/pts/*`
  遗留链接并恢复；未知链接保持不动并报错。

代理必须替代 `qt_app` 中直接启动 `./PCMonitorLinux` 的那一行；不能先启动一个已经
持有真实 `ttyGS0` 文件描述符的原厂进程。`qt_env.sh` 仍应在启动代理前加载。

## 小包输入

命令从标准输入读取一行一个 UTF-8 JSON，并在标准输出写回一行结果。示例：

```json
{"source":"qq","track_id":"0039MnYb0qxYhV","position":12345000000,"playing":true,"lyric_mode":"timed","lyrics_current":"当前句","lyrics_next":"下一句"}
```

发送到原厂程序的键只有：`source`、`track_id`、`position`、`playing`、
`lyric_mode`、`CaseScreen_LyricsCurrent` 和 `CaseScreen_LyricsNext`。完整歌词由屏端按
`source + track_id` 获取并缓存，不会进入高频包。`lyric_mode=plain` 时，显示层会
强制把当前句改成“该首歌的歌词暂不支持滚动”并清空下一句。

本层不打开音频设备，不读取麦克风，也不包含录音或声纹识别代码。

屏端服务可以在原厂渲染器握手后发送一次 `ReloadTheme`，切换到已经写入 TF 卡
`/data/theme/.../*.Theme` 的驻留歌词主题。这个命令只携带主题路径，不会重复上传
主题资源；之后的滚动仍只使用上面的轻量键值包。
