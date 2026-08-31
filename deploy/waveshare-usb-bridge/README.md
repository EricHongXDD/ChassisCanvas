# 微雪独立 USB/Wi-Fi 网络桥

该桥用于 `standalone-recognition` 的 `usb-gadget-bridge` 模式。微雪屏保持 USB Device，
以 ACM+RNDIS 复合设备连接一台独立低功耗 USB Host；小桥通过自己的 Wi-Fi 上网。电脑、
电脑声卡和 CaseScreen Windows 服务都不参与识歌或歌词滚动。

## 推荐首个可验证实现

首个可验证实现使用带 Wi-Fi 和 USB Host 的 Raspberry Pi Zero 2 W（或其他运行
NetworkManager 的 Debian 小主机）：

1. 用 Raspberry Pi OS 自己的界面把小桥连接到家庭 Wi-Fi；
2. 复制本目录后执行 `sudo ./configure_raspberry_pi.sh install`；
3. 给微雪屏和小桥使用符合各自额定值的独立电源；
4. 用 USB 数据线连接屏幕数据口与小桥 USB Host 口；
5. 烧录使用 `--network-mode usb-gadget-bridge` 生成的屏幕镜像。

NetworkManager 的 shared IPv4 配置会在 `192.168.77.1/24` 上提供 DHCP、DNS 转发和
Wi-Fi NAT。屏幕端只接受 `usb0` 上来自 `192.168.77.1` 的默认路由，并继续要求 NTP
成功标记、有效 CA、HTTPS relay 和逐屏令牌；桥未接、DHCP/DNS 失败或时间未同步时会
失败关闭并恢复原厂显示程序。

脚本用 `connection.stable-id=casescreen-usb-bridge-v1` 标记自己创建的连接。若系统里已经
存在同名但没有该标记的连接，安装和移除都会拒绝操作，避免覆盖或删除用户原有网络配置。

查看或移除配置：

```sh
sudo ./configure_raspberry_pi.sh status
sudo ./configure_raspberry_pi.sh remove
```

## 带宽与主机资源

- Windows 主机占用：0；不会打开、复制或改写电脑声音输出。
- 屏幕与小桥之间不传整帧画面，也不持续传 PCM。
- 配置 ACRCloud ARM32 fingerprint 提取器后，每轮只上传紧凑声纹；歌曲稳定后默认
  15 秒复核一次。
- 歌词在首次命中时下载并写入 TF 的有界缓存，滚动由屏幕本机单调时钟完成。

## 电源限制

微雪官方资料没有公布两个 Type-C 的 Host 角色和整机 USB 供电预算。不要让未经测量的
ESP32-S3 或 Raspberry Pi Host 口直接承担屏幕整机供电；首轮真机测试应让双方独立供电，
必要时使用合规的自供电 USB Hub，并确认不会在两路 5V 之间反向灌电。

## 更小的量产桥

ESP32-S3 理论上可使用乐鑫官方 USB Host RNDIS 组件加 Wi-Fi NAPT 做成更小、更低功耗的桥，
但官方 Host 组件要求复合设备中的 RNDIS 为第一个功能，而当前镜像为保护原串口兼容性采用
ACM-first。`esp32-s3/` 只提供默认拒绝启动的实验骨架；必须先完成 RNDIS-first 描述符、
Windows 串口回归、USB Host 供电、DHCP/DNS/NAT、看门狗和断线恢复的实体测试，才能启用。
当前 Raspberry Pi/NetworkManager 路径用于先把网络链路可靠地跑通，不把未经验证的 ESP32
固件伪装成成品。
