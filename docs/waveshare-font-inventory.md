# 微雪原厂镜像字体清单核验

核验日期：2026-08-30。

本记录用于证明 CaseScreen 1.0 当前使用的屏端字体已经存在于原厂 7 英寸与
8.8 英寸镜像中，因此字体族、字体权重和歌词显示不需要再次修改或重刷镜像。

## 核验对象

| 型号 | 原厂镜像 SHA-256 |
|---|---|
| 7 英寸 `7inch USB Monitor-240719-001.img` | `C7CE12A75CC6BFA91505451F9B59C7682DF2EBBC293893668909525C5B7CEFCF` |
| 8.8 英寸 `8.8inch USB Monitor-240719-001.img` | `4ED5BB1004BC2B16422C631C111AC36EE1BE93DA02873468FC538F63BA37A808` |

两张镜像均以只读方式解析 rootfs 分区（起始 LBA `59168`），并逐路径读取
`/usr/share/fonts` 的 inode 元数据；没有挂载、提取或写回镜像。两张镜像的
字体目录均为 418 个条目，CaseScreen 当前引用的以下文件全部存在：

- 等线：`Dengl.ttf`、`Deng.ttf`、`Dengb.ttf`；
- 微软雅黑：`msyhl.ttc`、`msyh.ttc`、`msyhbd.ttc`；
- 黑体：`simhei.ttf`；
- 游黑：`YuGothL.ttc`、`YuGothR.ttc`、`YuGothM.ttc`、`YuGothB.ttc`；
- 液晶数字：`LiquidCrystal-Light.otf`、`LiquidCrystal-Normal.otf`、
  `LiquidCrystal-Bold.otf`、`LiquidCrystal-ExBold.otf`；
- 等宽：`CascadiaMono.ttf`、`consola.ttf`、`consolab.ttf`；
- 宋体：`simsun.ttc`、`simsunb.ttf`；
- 思源黑体粗体：`Source San Hans BLOD.ttf`（原厂镜像中的文件名即为此拼写）。

微雪驻留 Theme 的 `source-han` 四档字重全部引用这张真实的
`Source San Hans BLOD.ttf` 字脸：Light 使用有界透明细化，Regular 保留
单层原形，Medium/Bold 使用共享同一动态数据键的 1 px 形态膨胀分档。
因此四档与 `microsoft-yahei` 始终使用不同的底层字脸，不上传字库，
也不需要修改镜像。Windows 工作台预览优先使用 Noto Sans SC，未安装时
回退到黑体/等线，不再回退到微软雅黑，以保证字体族切换在预览中可见。

仓库中的两份 8GB 扩容镜像只扩展 UDISK 并调整 GPT，rootfs 内容保持不变，
所以同一结论也适用于 `artifacts/8gb-expanded` 下的 7 英寸与 8.8 英寸镜像。

运行期只在 `.Theme` 中引用上述驻留路径；字体文件不会随遥测、频谱或歌词
小包重复上传。
