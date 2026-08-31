# CaseScreen 自托管识歌 Relay

> **实验/研究文档。** Relay 和 ACRCloud 均不是 Windows 正式版的构建输入、
> 运行依赖或发行内容，本文也不进入 Windows 正式包。正式版只使用
> Windows 播放来源、主机取词与 USB 小包。

`casescreen-recognition-relay` 是 `recognition-v1` 的服务端实现。它应监听同机回环
HTTP，由 Caddy/Nginx 等反向代理终止公网 HTTPS。屏端只保存逐屏 256 位随机
Bearer token；ACRCloud 的 `access_key` 与 `access_secret` 只从 Relay 进程环境读取，
不能写入 TF 镜像、主题、设备 token 文件或平台 ID 映射。

该命令包含两个 provider：

- `acrcloud`：真实识歌路径，按 ACRCloud 官方 Identification Protocol 1 发送
  `POST /v1/identify` multipart/form-data，并使用 HMAC-SHA1 + Base64 签名；
- `deterministic`：不联网的协议端到端测试替身。它只按输入 SHA-256 生成测试
  `stable_id`，不是识歌服务，不能用于正式运行。

官方契约参考：[Identification API](https://docs.acrcloud.com/reference/identification-api/identification-api)、
[Music metadata](https://docs.acrcloud.com/metadata/music) 和
[ACRCloud 官方 Web API 示例](https://github.com/acrcloud/webapi_example)。

## 构建

```bash
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
  -o casescreen-recognition-relay ./cmd/casescreen-recognition-relay
```

Relay 不需要 ACRCloud 原生 SDK。PCM 回退会在服务端封装成 16 kHz、mono、16-bit
WAV 再交给 ACRCloud。若屏端部署了合法取得的 ACRCloud ARM32 声纹提取器，则在
Relay 与屏端配置完全相同的 algorithm/version 后，Relay 会把紧凑声纹作为
`data_type=fingerprint` 转发。默认最大采样为 15 秒，项目推荐使用 10 秒。

## 设备 token

每块屏必须使用不同的 64 位十六进制随机 token。屏端保存明文 token；Relay 的
JSON 只保存明文 token 的 SHA-256。鉴权会对所有固定长度摘要执行常量时间比较，
且不会把 token 或摘要写入日志。

token 按原始 ASCII 字节区分大小写；生成摘要和写入屏端镜像时必须使用同一份文件。
镜像补丁器会保留十六进制字母大小写，不会替调用者改写秘密值。

Linux 上可分别生成明文与摘要；不要把 shell 历史、终端输出或明文 token 提交到
仓库：

```bash
umask 077
openssl rand -hex 32 > screen-7.token
tr -d '\n' < screen-7.token | sha256sum
```

把 `sha256sum` 第一列填入 `/etc/casescreen/recognition-tokens.json`：

```json
{
  "version": 1,
  "tokens": [
    {
      "device_id": "waveshare-7",
      "token_sha256": "替换为64位十六进制SHA256"
    }
  ]
}
```

文件权限必须为 `0600` 或更严格。撤销某台屏时，删除或更换该条摘要，同时更新
该屏的 token；不需要轮换 ACRCloud 主密钥。

## QQ/网易云原生 ID 强映射

ACRCloud 返回 `acrid`、ISRC、标题、歌手、`play_offset_ms`、`duration_ms` 和
`score`。它不会证明当前声音来自 QQ 音乐还是网易云音乐。Relay 因此会无条件
清除 provider 自带或伪造的 `source/track_id/native_id_verified`，只有静态强映射
命中后才重新写入这些字段。

映射文件没有标题、歌手或搜索字段，结构上禁止标题猜测：

```json
{
  "version": 1,
  "records": [
    {
      "stable_id": "acrcloud:替换为真实acrid",
      "isrc": "USRC17607839",
      "qq_track_id": "0039MnYb0qxYhV",
      "netease_track_id": "347230"
    }
  ]
}
```

每条记录必须包含 `stable_id` 或规范 ISRC，并至少包含一个有效的 QQ `songmid` 或
网易云十进制歌曲 ID。相同强证据出现冲突时，Relay 拒绝启动。识别结果同时带有
`stable_id` 和 ISRC、但与记录冲突时，本次映射失败关闭。

屏端 `preferred_platform=qq|netease` 只选择该映射文件中已经存在的对应 ID。
`preferred_platform=auto` 默认不选平台；管理员只有显式设置
`-auto-platform qq` 或 `-auto-platform netease` 后，才会应用固定平台策略。
这项策略仍依赖映射表，不会把录音来源猜成某个平台。

## ACRCloud 配置与启动

凭据只放在 root 可读的 `/etc/casescreen/acrcloud.env`：

```bash
ACRCLOUD_HOST=identify-REGION.acrcloud.com
ACRCLOUD_ACCESS_KEY=替换为项目access_key
ACRCLOUD_ACCESS_SECRET=替换为项目access_secret
```

先执行不监听端口的配置检查：

```bash
set -a
. /etc/casescreen/acrcloud.env
set +a
/usr/local/bin/casescreen-recognition-relay \
  -check \
  -token-digests /etc/casescreen/recognition-tokens.json \
  -native-mappings /etc/casescreen/native-id-mappings.json \
  -provider acrcloud
```

再让进程只监听回环地址：

```bash
/usr/local/bin/casescreen-recognition-relay \
  -listen 127.0.0.1:19323 \
  -token-digests /etc/casescreen/recognition-tokens.json \
  -native-mappings /etc/casescreen/native-id-mappings.json \
  -auto-platform none \
  -provider acrcloud
```

若使用屏端紧凑声纹，还要同时传入例如：

```text
-acr-fingerprint-algorithm acrcloud-fingerprint -acr-fingerprint-version 1
```

这里的名称只是部署双方的格式锁；它不会生成声纹，也不会把仓库中不存在的第三方
SDK 冒充成可用组件。

## HTTPS 反向代理

可复制并替换 [`deploy/recognition-relay/Caddyfile.example`](../deploy/recognition-relay/Caddyfile.example)
和 [`deploy/recognition-relay/casescreen-recognition-relay.service.example`](../deploy/recognition-relay/casescreen-recognition-relay.service.example)。
屏端最终地址应类似：

```text
https://recognition.example.com/v1/recognize
```

地址不得携带用户名、密码、query 或 fragment。Relay 自身默认拒绝绑定非回环地址；
只有容器网络确实需要时才可显式使用 `-allow-public-http`，并仍需由受控网络内的
反向代理提供外部 HTTPS。不要把该选项当成允许公网明文 HTTP。

## 资源与安全边界

- 默认请求体上限 2 MiB，JSON 拒绝未知字段和尾随第二个值；
- 只接受 16 kHz、mono、S16_LE PCM 或不超过 64 KiB 的已配置声纹；
- 默认允许 5–15 秒采样，屏端时间戳只允许有限过去/未来偏差；
- 每设备默认 1 请求/秒、突发 3 次，并有未鉴权来源限流；
- 上游默认 12 秒超时、并发 4；即使插件忽略 context，HTTP 请求仍会超时，未退出
  插件继续占用并发槽以阻止 goroutine 无界增长；
- ACRCloud 响应最大 1 MiB，重定向被禁止，第三方错误正文不会返回屏端；
- ACRCloud music `score` 只接受 70..100，并除以 100 归一化成 0..1；
- Relay 显式持有的 PCM、声纹、WAV 和 multipart 工作缓冲在使用后清零；程序不主动
  将音频落盘，也不会把它写入诊断日志。

## 最小测试

`deterministic` 只能验证 TLS、Bearer、请求格式、大小限制和屏端状态机连通性：

```bash
/usr/local/bin/casescreen-recognition-relay \
  -listen 127.0.0.1:19323 \
  -provider deterministic \
  -token-digests /etc/casescreen/recognition-tokens.json
```

完成协议测试后必须切回 `acrcloud` 并提供真实项目凭据。仓库没有附带 ACRCloud
凭据，也没有替用户创建 QQ/网易云目录授权或真实原生 ID 映射；在这些外部条件未
配置且两款屏未完成真机麦克风/网络验收前，不能把镜像声明为已完成独立识歌验收。
