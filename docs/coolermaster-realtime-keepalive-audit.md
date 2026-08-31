# 酷冷至尊 ATMOS V2 实时画面保活审计

审计日期：2026-08-31

## 结论

ATMOS V2 固件为实时 JPEG 显示设置了 5 秒保活超时。显示方向测试图后，即使主机没有再发送普通主题帧，设备也会在约 5 秒无保活时自动退出实时显示并回到设备原有内容。因此，界面仍保持“显示测试图”选中而实物恢复旧主题，并不是前端状态丢失。

原厂 MasterCTRL 在正常连续播放链路中，每约 4 秒发送两个很小的 HID 命令：

1. `timestamp`，序号 42，正文为当前 Unix 纪元毫秒数；
2. `realtimeDisplay`，序号 42，正文为 `{"enable":true}`。

两个命令都收到 `AckNumber=43`。按当前协议实现“ACK 序号等于请求序号加一”的规则，这与序号 42 完全对应。

后续真机 A/B 验证补充了一个关键边界：把一张静态方向测试 JPEG 只发送一次后，周期发送上述两条命令虽然持续收到成功 ACK，物理屏仍会在约 5 秒恢复离线主题。也就是说，这两条命令能维持通讯和实时模式状态，但不能单独续期一张不再变化的静态帧。方向测试必须同时周期刷新同一张 JPEG；正常动态主题本来就在持续发送新帧，不受这个边界影响。

## 审计对象

- 文件：`C:\Program Files\Cooler Master\MasterControl\CMAgent.exe`
- 文件版本：`1.5.2.243`
- 产品版本：`1.5.2.243`
- SHA-256：`BB1C11CAB403F32A92958C8BBCB8DB7CDAC30CA45C2814A32ED816318F47C2C4`
- 日志：`%APPDATA%\MasterCTRL\logs\CMAgent.log`

## 运行日志证据

固件状态响应在日志第 100、123、145 行均包含：

```json
{"AckNumber":"1","keepAliveTimeout":5,"maxSuspendMediaCount":5,"suspendMediaActive":[0,0,0,0,1]}
```

其中 `keepAliveTimeout:5` 与实物严格约 5 秒恢复原内容相符。

日志第 154–157、239–242、255–258、270–273 行等位置反复出现：

```text
LCDATMOSV2SubDevice::SendKeepAlive - Begin
... {"AckNumber":"43","response":200}
... {"AckNumber":"43","response":200}
LCDATMOSV2SubDevice::SendKeepAlive - Return value: true
```

相邻 `SendKeepAlive` 的间隔约为 4.0–4.1 秒，小于固件的 5 秒超时。

## `SendKeepAlive` 反汇编证据

函数范围：VA `0x1402ab400`–`0x1402abad0`。

### 第一条命令：更新时间戳

```text
0x1402ab5c7  mov dword ptr [rsp+0x40], 0x2a  ; 请求序号 42
0x1402ab668  lea rdx, [...]                  ; JSON 字段名 "timestamp"
0x1402ab6ea  lea rdx, [...]                  ; action "timestamp"
0x1402ab726  mov r9d, 0x2a                  ; 请求序号 42
0x1402ab744  call 0x1402b0400               ; 提交命令
```

### 第二条命令：续期实时显示模式

```text
0x1402ab77b  mov byte ptr [rsp+0x30], 1     ; 布尔值 true
0x1402ab7ba  lea rdx, [...]                  ; JSON 字段名 "enable"
0x1402ab883  lea rdx, [...]                  ; action "realtimeDisplay"
0x1402ab8bf  mov r9d, 0x2a                  ; 请求序号 42
0x1402ab8dd  call 0x1402b0100               ; 提交命令
```

二进制字符串位置分别为：`enable` 文件偏移 `0x28ae4d4`、`realtimeDisplay` 文件偏移 `0x28ae4e0`、`timestamp` 字段名文件偏移 `0x28ae4f0`、`timestamp` action 文件偏移 `0x28ae500`。

## 时间戳单位和来源

`SendKeepAlive` 的时间源调用链最终进入导入函数：

```text
0x1400df5c0  ... call qword ptr [0x1409eade0]
```

PE 导入表确认 `0x1409eade0` 对应 `MSVCP140.dll!_Xtime_get_ticks`。随后转换函数执行：

```text
0x14022d12d  call 0x140044a70  ; 取得时钟 tick 计数
0x14022d132  cqo
0x14022d134  mov ecx, 0x2710  ; 10000
0x14022d139  idiv rcx         ; 100 纳秒 tick / 10000 = 毫秒
```

因此：

- 请求头 `Date=` 沿用协议中的 Unix 纪元秒；
- JSON 正文里的 `timestamp` 是当前 Unix 纪元毫秒，不能误传为秒。

## 精确请求形态

协议使用报告 ID `0x1E`、固定 1024 字节 HID 输出报告，不足部分补零。原厂每轮保活的两条请求形态为：

```text
POST timestamp 1\r\n
SeqNumber=42\r\n
Date=<Unix 纪元秒>\r\n
ContentType=json\r\n
ContentLength=<正文 UTF-8 字节数>\r\n
\r\n
{"timestamp":<Unix 纪元毫秒>}
```

```text
POST realtimeDisplay 1\r\n
SeqNumber=42\r\n
Date=<Unix 纪元秒>\r\n
ContentType=json\r\n
ContentLength=15\r\n
\r\n
{"enable":true}
```

每条请求期望 `AckNumber=43`。原厂小命令本身平均只有约 512 B/s；方向测试额外重发缓存 JPEG，但只发生在用户临时观察安装方向期间，不进入日常主题链路，也不写闪存。

## 与当前程序行为的交叉验证

方向测试处于 Active 时，连续查询显示酷冷设备的 `last_connection_check_at` 和 `last_verified_at` 都保持为 `2026-08-31T17:10:39.7312735+08:00`，健康探测没有运行；原厂服务也已停止。因此本次约 5 秒恢复不能由健康探测或原厂服务覆盖解释，只能由固件实时模式超时解释。

当前 `SetRealtimeEnabled(true)` 有模式缓存时可能直接返回，不保证实际写 HID，不能直接当作强制周期保活接口。

## 最终兼容实现

1. 驱动每轮都强制发送上述 `timestamp` 和 `realtimeDisplay` 两条请求，不走“模式已知则跳过”的缓存分支。
2. 只在方向测试 Active 期间每 2 秒调用一次，并重发首次编码后缓存的同一张测试 JPEG；不重复编码，也不修改离线槽位。
3. 发送前获取同一设备的 theme gate，并在 gate 内再次检查 Active，避免取消后尾随发送。
4. 复用现有设备 I/O 串行锁，确保刷新不会与主题应用、恢复或连接检查交错。
5. 确认、取消、设备关闭、断连或上下文取消时立即停止定时器。取消路径应先结束保活，再恢复正常主题。
6. 保活失败按现有设备 I/O 错误和重连路径处理，但不要把它记作 `LastConnectionCheck`。
7. 不用 `POST param` 假冒保活，也不发送 `delMedia`、`transport`、`transported`，因此不会覆盖用户槽位或增加闪存写入。

## 与 MasterCTRL 的独占关系

ATMOS V2 不支持 MasterCTRL 和 CaseScreen 同时控制。实机环境中，
`MasterCTRLService` 以 `AUTO_START` 运行，`CMTrayApp.exe` 还通过当前用户的
`Run\MasterCTRL` 登录项启动。这会产生一个开机竞态：CaseScreen 先打开
HID 并恢复主题，原厂服务稍后启动，再切换回原厂默认内容。

CaseScreen 的处理原则是：

1. 启动时同时检查原厂服务运行状态、服务启动类型、`CMTrayApp`
   登录项与 `CMAgent.exe`；即使原厂服务尚在延迟启动，也不先短暂抢占 HID。
2. 运行中每次设备扫描都复核原厂控制器。如果它后启动，CaseScreen
   会在单设备门闩内停止帧传输、释放 HID 并标记可恢复离线，避免两边继续互相覆盖；
   同时把对应驻留槽摘要持久标记为 `uncertain`，下次独占连接必须重传保存主题。
3. 原厂控制器停止且不再会自启后，现有重连路径会重新打开 HID，并在同一
   设备门闩内恢复最新已保存/待应用主题。
4. CaseScreen 不会未经用户确认就自动停止或禁用 Windows 服务。要让
   CaseScreen 在每次开机后稳定独占副屏，必须由用户明确选择停用
   `MasterCTRL Service`，并退出原厂 MasterCTRL。

新版状态文件另有 `trust_version`，它与媒体协议的 `replace_version` 分离。
旧版状态缺少该字段，可能是在 MasterCTRL 覆盖槽位前留下的摘要，因此升级后
首次应用会强制重传一次；只有收到明确的 `transported/200` 并写入当前
`trust_version` 后，后续普通断线重连才继续使用 digest 去重。这样即使用户在
安装新版前已经停用原厂服务，也不会误信遗留的槽位 4 缓存。

测试覆盖精确 action/序号/正文及毫秒时间戳、2 秒周期启动和停止、JPEG 媒体类型、禁止离线写入命令、取消竞态和发送失败；发布前还需用新构建在实机连续保持至少 15 秒，再取消确认可恢复原主题。
