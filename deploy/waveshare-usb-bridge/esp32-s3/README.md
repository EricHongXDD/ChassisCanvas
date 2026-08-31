# ESP32-S3 USB RNDIS / Wi-Fi 小桥（受控实验骨架）

> **当前不可作为 beta.13 主实现或可用成品。** beta.13 的首个可验证实现仍是上级目录中的
> Linux/NetworkManager 小桥。此目录只用于收敛 ESP32-S3 量产方向，尚未在本机编译，也未在
> 微雪实体屏上验证。

## 当前硬阻塞：复合功能顺序不兼容

乐鑫官方 `iot_usbh_rndis` 文档要求：复合 USB 设备的 RNDIS 必须是第一个功能，RNDIS IAD 的
`bFirstInterface`/首个接口号应为 `0`。当前 CaseScreen 微雪镜像为了保留原串口身份，明确采用
**ACM-first、RNDIS-second**。因此当前镜像不能假定会被该官方 Host 驱动识别。

工程默认关闭 `CONFIG_CASESCREEN_RNDIS_FIRST_VERIFIED`；未由实体测试确认以下项目时，固件会在
启动入口失败关闭：

1. 镜像改成 RNDIS-first 后，ESP32-S3 看到的 RNDIS IAD 首接口确实为 `0`；
2. 7 英寸和 8.8 英寸实体屏均能枚举、DHCP、DNS、NAPT、断线重连；
3. Windows 原生串口和现有 CaseScreen 小包协议在 ACM 变成后续接口后仍完整回归；
4. Linux/NetworkManager 小桥路径没有被破坏。

在这些门禁完成前，不应修改当前镜像主脚本来迁就本骨架，也不应发布 ESP32 固件。

## 设计范围

```text
家庭 Wi-Fi/路由器
        │  Wi-Fi STA（DHCP client，默认路由）
        ▼
   ESP32-S3
        │  IPv4 forwarding + NAPT
        │  DHCP server：192.168.77.1/24
        │  DHCP 下发上游 DNS（不运行常驻 DNS 代理）
        ▼
USB Host ── RNDIS Gadget ── 微雪屏（udhcpc，预计首个租约 192.168.77.2）
```

骨架只调用 ESP-IDF 与乐鑫 ESP Component Registry 中的官方组件：

- `espressif/iot_usbh_rndis` `0.5.0`；
- `espressif/iot_usbh_cdc` `3.1.0`；
- `espressif/iot_eth` `1.1.0`；
- ESP-IDF `5.5.x` 的 Wi-Fi STA、ESP-NETIF DHCP server 与 NAPT API。

它把 USB 设备匹配限定为当前微雪 Gadget 的 `VID 1D6B / PID 0104`，将 `192.168.77.1/24`
设置为屏端私网网关，并直接把 Wi-Fi DHCP 获得的 DNS 地址通过下游 DHCP OFFER 交给屏幕。
这样不需要本机 DNS 代理，也不传画面或 PCM；链路上只有识歌声纹、歌词和 HTTPS 控制数据。

## 构建（尚未在本机执行）

本工作区当前没有 ESP-IDF，因而这里没有声称“已编译通过”。在独立、已安装 ESP-IDF
`release/v5.5` 的实验环境中执行：

```sh
cd deploy/waveshare-usb-bridge/esp32-s3
idf.py set-target esp32s3
idf.py menuconfig
idf.py build
```

在 `CaseScreen USB bridge (experimental)` 中：

- 设置实验 Wi-Fi SSID/密码；
- 只有完成上述 RNDIS-first 与串口回归后，才勾选实体兼容性确认项；
- 不要把生成的 `sdkconfig` 提交到仓库，它包含 Wi-Fi 凭据。

`idf_component.yml` 固定组件版本，避免后续 API 漂移。首次配置会从乐鑫官方 Registry 下载
组件，因此构建机需要联网。

## 仍未验证、不能省略的实体测试

- **USB 角色与供电：** 开发板必须把 ESP32-S3 原生 USB-OTG 接口接成 Host，并能按 USB
  规范提供/检测 VBUS。屏幕应独立供电；未测量前不得把开发板 5 V 与屏幕另一电源硬并联，
  也不得允许反向灌电。板级 VBUS 开关 GPIO 尚未实现，因为不同开发板电路不相同。
- **设备兼容性：** 乐鑫公开的 RNDIS 测试矩阵是若干 4G 模块，不包含该微雪 Linux Gadget。
  必须以真实描述符、控制传输、keepalive、拔插和冷启动结果验收。
- **DNS 更新：** Wi-Fi 重连后骨架会更新 DHCP server 的 DNS 配置并恢复 NAPT；已持有旧租约的
  屏幕何时重新获取 DNS 仍需真机确认。量产版可能需要缩短租期或实现经过测试的本机 DNS 转发。
- **地址冲突：** 固件会拒绝与固定私网 `192.168.77.0/24` 重叠的 Wi-Fi 上游；若家庭网络正好
  使用该网段，必须先设计可协商的屏端/桥端地址，不能强行打开 NAPT。
- **Wi-Fi 配网：** 当前 SSID/密码在 `sdkconfig` 和固件中明文，仅适合实验。安全配网、凭据加密、
  OTA、硬件看门狗、状态灯和恢复出厂尚未实现。
- **资源与稳定性：** 还需测量堆内存、USB Full-Speed 丢包、Wi-Fi 省电、NAPT 表容量、连续运行、
  上游重连和识歌高峰；不能用代码存在代替这些结果。

## 官方依据

- [乐鑫 USB RNDIS Host Driver](https://docs.espressif.com/projects/esp-iot-solution/en/latest/usb/usb_host/usb_rndis.html)
  （复合设备 RNDIS-first 约束、API 与 ESP32-S3 支持）
- [乐鑫 `iot_usbh_rndis` Component Registry](https://components.espressif.com/components/espressif/iot_usbh_rndis/versions/0.5.0/readme?language=en)
- [乐鑫 USB RNDIS 4G 官方示例](https://github.com/espressif/esp-iot-solution/tree/master/examples/usb/host/usb_rndis_4g_module)
- [ESP-IDF `softap_sta` 官方示例](https://github.com/espressif/esp-idf/blob/release/v5.5/examples/wifi/softap_sta/main/softap_sta.c)
  （DHCP DNS OFFER、默认上行和 `esp_netif_napt_enable()`）
- [ESP-NETIF 官方文档](https://docs.espressif.com/projects/esp-idf/en/release-v5.5/esp32s3/api-reference/network/esp_netif.html)
- [ESP32-S3 USB Host 官方文档](https://docs.espressif.com/projects/esp-idf/en/release-v5.5/esp32s3/api-reference/peripherals/usb_host.html)
