# Waveshare 7 英寸屏镜像分析

分析对象：`D:\BaiduNetdiskDownload\7inch USB Monitor-240719-001.img`

所有检查均为只读操作。镜像大小为 1,977,614,336 字节（约 1.84 GiB），使用 GPT 分区表。

## 分区布局

| 分区 | LBA | 分区大小 | 内容 |
|---|---:|---:|---|
| boot-resource | 41464–45559 | 2.00 MiB | FAT16，卷标 `Volumn` |
| env | 45560–46063 | 252 KiB | U-Boot 环境 |
| env-redund | 46064–46567 | 252 KiB | 冗余 U-Boot 环境 |
| boot | 46568–59167 | 6.15 MiB | Android boot image，签名 `ANDROID!` |
| rootfs | 59168–2156319 | 1.00 GiB | ext4；文件系统容量约 800 MiB |
| UDISK | 2156320–3860479 | 832.11 MiB | ext4；OverlayFS 可写数据分区 |

镜像分区表外只有开头约 20.25 MiB 和末尾约 1 MiB。开头区域可能包含启动加载器和平台保留数据，不能当作普通空闲空间使用。

`rootfs` 估算已使用约 710.45 MiB，可用约 89.55 MiB。`UDISK` 估算已使用约 29.94 MiB，可用约 802.17 MiB。

如果把这个固定大小的 IMG 写入更大的 TF 卡，镜像末端之后的物理卡空间不会自动加入分区，因此会显示为未分配。Windows 同时不能原生读取 ext4，可能把 Linux 分区显示为未知或不可用。

## 操作系统和启动链

- 系统：Tina/OpenWrt，版本标识 `tina.chen.20240709.070921`。
- 目标平台：Allwinner T113-Pro。
- `/etc/init.d/qt_app` 在启动时执行 `/data/serial_uart.sh`，然后运行 `/data/PCMonitorLinux`。
- `PCMonitorLinux` 是 32 位 ARM ELF 程序，使用 Qt 5 和 QtSerialPort。
- `serial_uart.sh` 通过 Linux configfs 创建 CDC ACM USB Gadget：
  - VID：`1D6B`
  - PID：`0104`
  - 产品名：`Virtual Serial Port`
  - 厂商名：`SYK`
- 这正是 Windows 上出现 COM3 的来源。

## 可写主题数据

`UDISK` 挂载为 `/overlay`，其 `/upper/data` 覆盖系统中的 `/data`。当前数据包括：

- `/data/PCMonitorLinux`：屏幕端渲染与串口程序；
- `/data/theme/1024x600/070-astronaut.Theme`：当前主题，约 3.47 MiB；
- `/data/video/1024x600/白色太空人lite.mp4`：主题视频，约 334 KiB；
- `/data/fonts/`：主题字体；
- `/data/image/`：图片、相册和图片字体资源；
- `/data/info/ThemeMessage`：当前主题记录；
- `/data/config.txt`：Qt QDataStream 配置。

`config.txt` 中的关键配置为：

- `isCanvasFlip = false`
- `deviceBl = 128`
- `defaultTheme = /data/theme/1024x600/070-astronaut.Theme`

## 协议相关证据

屏幕端程序包含以下串口功能和字符串：

- `device_state_receive_file`
- `host_send_file_state`
- `device_set_theme_reload`
- `device_set_system_data`
- `AA551234 Abort file transfer 123455AA`
- `AA551234 FIXEDCMDHEAD `

这与 Windows 端抓包相符，说明原厂协议支持：

1. 将主题、视频、字体等文件写入 TF 卡的 `UDISK` 数据分区；
2. 通知屏幕端重新加载主题；
3. 运行期间只发送小型系统数据键值包。

## 对统一软件的意义

Waveshare 适配器应优先复用原厂文件传输和主题重载协议：主题仅上传一次，日常刷新只发送系统数据，不需要反复传输 1024×600 画面。

现有 `UDISK` 已有约 802 MiB 可用空间，普通主题无需扩容。若未来需要存放大量视频，可以在备份原卡后，把 GPT 备份表移动到物理卡末端，再扩展 `UDISK` 分区和 ext4 文件系统；不能直接用 Windows 磁盘管理扩展。
