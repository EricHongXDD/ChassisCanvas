# 第三方运行时说明

CaseScreen 的本地完整 Windows 发行目录可能包含以下开源运行时，仅用于访问用户自己的显示设备和解码主题视频：

- FFmpeg n4.3.1 共享库构建：启用了 `--enable-version3`，按 LGPLv3 条款使用；项目主页为 <https://ffmpeg.org/>。
- hidapi：按其 BSD 许可选项使用；项目主页为 <https://github.com/libusb/hidapi>。

这些 DLL/EXE 保持为独立动态库，CaseScreen 不修改其二进制内容。FFmpeg 文件名中的下划线副本在发行目录中按其导入表要求改为连字符名称。它们可以由用户替换为兼容 ABI 的动态库：FFmpeg 需要保持同一主版本的 `av*-58/56/7/5/3.dll` 文件名与导出接口，hidapi 需要保持 `hidapi.dll` 的同名 C ABI。

工作区中既有的本地 `1.0` ZIP 是为本机现有设备生成的部署产物，不主张获得其中第三方二进制文件的再分发许可，也不应直接作为公开下载包转发。若要对外分发完整包，发布者必须先从具有明确再分发许可的来源重新取得或自行构建 FFmpeg/hidapi，随包提供对应构建的完整许可证、版权声明、可替换说明以及许可证要求的源代码/获取方式。本说明不以链接代替届时必须随公开发行包提供的材料。

GitHub Actions 生成的 `ChassisCanvas-windows-x64-*-core.zip` 是公开核心包，明确不包含 FFmpeg、hidapi、厂商安装程序、厂商 Theme 或 TF 卡镜像。用户如需相应功能，必须从自己有权使用的原厂软件、设备或其他许可清晰的来源取得兼容运行时和主题文件。

QQMusic-ID-Injector 不是 CaseScreen 发行包或正式歌曲身份路径的一部分。CaseScreen 不下载、安装、复制或注入其 DLL，也不读取其 SMTC `Genres` 标记。用户若自行采用该 MIT 项目，应独立核对其版本兼容性、许可证和进程注入风险。

CaseScreen 内置 QQ 只读内存适配器的 `CurrentSongInfo` 布局和特征码依据 Kxnrl/NetEase-Cloud-Music-DiscordRPC 原始 MIT 仓库 3.1 发布提交中的 `Vanessa/Players/Tencent.cs` 实现重新编写。CaseScreen 不复制或分发其二进制文件，只在自身 Windows 主进程中调用系统进程查询与 `ReadProcessMemory`；不向 QQMusic 写入数据、不注入 DLL，也不启动该第三方程序。固定版本的原始实现与许可证：<https://github.com/Kxnrl/NetEase-Cloud-Music-DiscordRPC/blob/3a97a4b5c8b673ce2f69b0205adbc6a3ac861ba5/Vanessa/Players/Tencent.cs>、<https://github.com/Kxnrl/NetEase-Cloud-Music-DiscordRPC/blob/3a97a4b5c8b673ce2f69b0205adbc6a3ac861ba5/LICENSE>。Windows 权限依据：<https://learn.microsoft.com/windows/win32/api/memoryapi/nf-memoryapi-virtualqueryex>、<https://learn.microsoft.com/windows/win32/api/memoryapi/nf-memoryapi-readprocessmemory>。

CaseScreen 内置网易云 3.x 只读内存适配器的 AudioPlayer/Schedule 特征与 MSVC 字符串布局依据同一 MIT 仓库的 `Vanessa/Players/NetEase.cs` 固定提交重新编写：<https://github.com/Kxnrl/NetEase-Cloud-Music-DiscordRPC/blob/d3b77c679379aff1294cc83a285ad4f695376ad6/Vanessa/Players/NetEase.cs>。CaseScreen 仍只在自身主进程中使用查询与 `ReadProcessMemory` 权限，不复制上游程序、不写播放器内存、不注入代码，也不启动调试通道；许可证见该提交的 <https://github.com/Kxnrl/NetEase-Cloud-Music-DiscordRPC/blob/d3b77c679379aff1294cc83a285ad4f695376ad6/LICENSE>。

上游 MIT 许可证原文保留如下：

```text
MIT License

Copyright (c) 2018 Kyle

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
