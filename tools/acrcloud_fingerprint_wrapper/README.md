# ACRCloud 屏端声纹适配器

这个目录只提供 CaseScreen 与 ACRCloud 官方 Linux C SDK 之间的适配源码，不包含、
复制或授权任何 ACRCloud 二进制。官方库必须由部署者从 ACRCloud 官方仓库或控制台
合法取得，并自行确认其许可和账户条款。

适配器完成四件事：

1. 严格验证 `casescreen-edge` 传入的格式参数；
2. 从 stdin 读取固定长度的 16 kHz、单声道、S16_LE PCM；
3. 用 15 阶半带 FIR 低通后降采样到官方 `create_fingerprint` 所需的 8 kHz；
4. 从同目录 `libacrcloud_extr_tool.so` 生成识别声纹，stdout 只输出声纹二进制。

原始 PCM、8 kHz 中间数据和声纹都不写入 TF 卡，退出前会清零进程内缓冲区。输出
硬限制为 64 KiB，与 `internal/edgerecognition` 的传输门禁一致。

## 构建

在安装了 ARM hard-float 交叉编译器的 Linux 主机执行：

```sh
arm-linux-gnueabihf-gcc \
  -O2 -std=c11 -Wall -Wextra -Werror \
  -fstack-protector-strong -D_FORTIFY_SOURCE=2 \
  -Wl,-z,relro,-z,now \
  -o casescreen-acrcloud-fingerprint main.c -ldl
```

本仓库 beta.12 发行候选使用官方 Zig 0.16.0 在 Windows 交叉编译，显式锁定目标
glibc 2.23 和 Cortex-A7：

```powershell
zig cc -target arm-linux-gnueabihf.2.23 -mcpu=cortex_a7 `
  -O2 -std=c11 -Wall -Wextra -Werror -fstack-protector-strong `
  -D_FORTIFY_SOURCE=2 -Wl,-z,relro,-z,now `
  -o casescreen-acrcloud-fingerprint-linux-armv7 main.c -ldl
```

得到的包装程序是 ELF32 ARM EABI5 hard-float、解释器
`/lib/ld-linux-armhf.so.3`，只要求 `libc.so.6`、`libdl.so.2` 和
`GLIBC_2.4`；这低于两款原镜像的 glibc 2.23。发行包仍不会包含官方
`libacrcloud_extr_tool.so`。

构建结果必须用 `readelf -h` 确认是 ELF32、ARM、EABI5，并与目标系统使用相同的
hard-float ABI。官方仓库当前的 `linux/arm/arm32/libacrcloud_extr_tool.so` 也是
ARM32 EABI5 hard-float，依赖 `libc.so.6`、`libm.so.6`、`libpthread.so.0` 和
`librt.so.1`；仍须在真机执行 `ldd` 和一次已知音频测试后才能认定兼容。

## 部署契约

两个文件必须放在同一目录：

```text
/data/casescreen/fingerprint-extractor
/data/casescreen/libacrcloud_extr_tool.so
```

镜像补丁时把第一个文件作为 `--fingerprint-extractor`，算法/版本固定采用中继端
明确允许的标识，例如 `acrcloud-fingerprint` / `1`。不要把识歌服务的 access
secret 写入副屏；副屏只持有 CaseScreen relay 的可撤销 Bearer token。

手工冒烟测试（输入必须正好是 10 秒、16 kHz、单声道、S16_LE）：

```sh
./fingerprint-extractor \
  --format pcm_s16le --sample-rate 16000 --channels 1 --duration-ms 10000 \
  < known-10s.pcm > known-10s.fp
test -s known-10s.fp
test "$(wc -c < known-10s.fp)" -le 65536
```

该测试只证明本地提取成功；还需要把声纹提交给配置好的 relay，并用已知歌曲验证
ACRCloud 返回的 `play_offset_ms` 和稳定识别结果。
