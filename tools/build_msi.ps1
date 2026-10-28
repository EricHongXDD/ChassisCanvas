param(
    [string]$Version = "1.0.2",
    [string]$SourceDirectory = "",
    [string]$WixDirectory = "",
    [switch]$PrivateLocal
)

$ErrorActionPreference = "Stop"
chcp 65001 > $null
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
$root = Split-Path -Parent $PSScriptRoot
if ($Version -notmatch '^(\d+)\.(\d+)\.(\d+)$' -or
    [int]$Matches[1] -gt 255 -or [int]$Matches[2] -gt 255 -or [int]$Matches[3] -gt 65535) {
    throw "MSI 版本必须为合法的三段数字：$Version"
}
if ([string]::IsNullOrWhiteSpace($WixDirectory)) {
    $WixDirectory = Join-Path $root ".tools\wix314"
    if ($env:WIX -and -not (Test-Path -LiteralPath (Join-Path $WixDirectory "candle.exe"))) {
        $WixDirectory = Join-Path $env:WIX "bin"
    }
}
foreach ($tool in @("candle.exe", "light.exe")) {
    if (-not (Test-Path -LiteralPath (Join-Path $WixDirectory $tool) -PathType Leaf)) {
        throw "需要 WiX 3.14 工具：$tool；请设置 -WixDirectory"
    }
}
if ([string]::IsNullOrWhiteSpace($SourceDirectory)) {
    $SourceDirectory = Join-Path $root "artifacts\ChassisCanvas-windows-x64-$Version-core"
}
$source = (Resolve-Path -LiteralPath $SourceDirectory).Path
$files = @(Get-ChildItem -LiteralPath $source -File -Recurse | Sort-Object FullName)
if ($files.Count -eq 0) { throw "安装源目录为空：$source" }
if (@(Get-ChildItem -LiteralPath $source -Recurse -Force | Where-Object {
    $_.Attributes -band [IO.FileAttributes]::ReparsePoint
}).Count -gt 0) { throw "安装源不得包含重解析点" }
foreach ($exe in @("CaseScreen.exe", "CaseScreenDesktop.exe")) {
    $path = Join-Path $source $exe
    if (-not (Test-Path -LiteralPath $path -PathType Leaf) -or
        [Diagnostics.FileVersionInfo]::GetVersionInfo($path).FileVersion -ne $Version) {
        throw "安装源缺少 $Version 版程序：$exe"
    }
}
foreach ($file in $files) {
    $relative = $file.FullName.Substring($source.Length + 1)
    if ($relative -match '(?i)(^|[\\/])(config\.json|device-config\.json|CaseScreen-data|platform-auth|lyrics-cache)([\\/]|$)' -or
        $file.Extension -in @('.log', '.token', '.cookie', '.key')) {
        throw "安装源包含本机设置或会话文件：$relative"
    }
    if (-not $PrivateLocal -and ($file.Extension -in @('.dll', '.Theme') -or $file.Name -ieq 'ffmpeg.exe')) {
        throw "公开核心安装包不得包含运行时或厂商 Theme：$relative"
    }
}

$flavor = if ($PrivateLocal) { "private" } else { "core" }
$work = Join-Path $root (".tmp\msi-$flavor-$Version-" + [Guid]::NewGuid().ToString("N"))
[IO.Directory]::CreateDirectory($work) | Out-Null
$output = Join-Path $root $(if ($PrivateLocal) { "artifacts\CaseScreen-$Version-Setup.msi" } else { "artifacts\ChassisCanvas-$Version-Core-Setup.msi" })
$product = if ($PrivateLocal) { "CaseScreen $Version" } else { "ChassisCanvas Core $Version" }
# 本机完整包保持旧 MSI 的产品升级关系；公开核心版独立安装，避免移除旧版运行时。
$upgrade = if ($PrivateLocal) { "952AC691-11E7-478F-9531-8500485AC5AA" } else { "DC41DA2C-E3E3-43D2-A84D-EB04DA6D675E" }
$folder = if ($PrivateLocal) { "CaseScreen" } else { "ChassisCanvas-Core" }
$registry = if ($PrivateLocal) { "Software\CaseScreen\Installer" } else { "Software\ChassisCanvas\CoreInstaller" }
$shortcut = if ($PrivateLocal) { "CaseScreen 机箱副屏" } else { "ChassisCanvas 机箱副屏（核心版）" }
$runName = if ($PrivateLocal) { "CaseScreen" } else { "ChassisCanvasCore" }
function Escape-XML([string]$Value) { [Security.SecurityElement]::Escape($Value) }
function Stable-ID([string]$Value) {
    if (-not $PrivateLocal) { $Value = "core|$Value" }
    $hash = [Security.Cryptography.SHA256]::Create()
    try { "I" + ([BitConverter]::ToString($hash.ComputeHash([Text.Encoding]::UTF8.GetBytes($Value))).Replace("-", "").Substring(0, 24)) }
    finally { $hash.Dispose() }
}
function Directory-ID([string]$Relative) {
    if ([string]::IsNullOrWhiteSpace($Relative)) { return "INSTALLFOLDER" }
    "D" + (Stable-ID $Relative)
}
$directories = [Collections.Generic.List[string]]::new()
$components = [Collections.Generic.List[string]]::new()
$references = [Collections.Generic.List[string]]::new()
foreach ($directory in Get-ChildItem -LiteralPath $source -Directory -Recurse | Sort-Object FullName) {
    $relative = $directory.FullName.Substring($source.Length + 1)
    $parent = Directory-ID (Split-Path $relative -Parent)
    $id = Directory-ID $relative
    $directories.Add('<DirectoryRef Id="' + $parent + '"><Directory Id="' + $id + '" Name="' + (Escape-XML $directory.Name) + '"/></DirectoryRef>')
}
foreach ($file in $files) {
    $relative = $file.FullName.Substring($source.Length + 1)
    $id = Stable-ID $relative
    $guid = [Guid]::ParseExact(("43534352" + $id.Substring(1)), "N").ToString()
    $parent = Directory-ID (Split-Path $relative -Parent)
    $components.Add('<DirectoryRef Id="' + $parent + '"><Component Id="' + $id + '" Guid="' + $guid + '" Win64="yes"><File Id="F' + $id + '" Source="' + (Escape-XML $file.FullName) + '"/><RegistryValue Root="HKCU" Key="' + $registry + '\Files" Name="' + $id + '" Type="integer" Value="1" KeyPath="yes"/></Component></DirectoryRef>')
    $references.Add('<ComponentRef Id="' + $id + '"/>')
}
$icon = Escape-XML (Join-Path $root "assets\casescreen-icon.ico")
$notice = Escape-XML (Join-Path $work "notice.rtf")
$xml = @"
<?xml version="1.0" encoding="utf-8"?>
<Wix xmlns="http://schemas.microsoft.com/wix/2006/wi">
<Product Id="*" Name="$product" Language="2052" Version="$Version" Manufacturer="CaseScreen" UpgradeCode="$upgrade">
<Package InstallerVersion="500" Compressed="yes" InstallScope="perUser" Platform="x64"/>
<MajorUpgrade AllowSameVersionUpgrades="yes" DowngradeErrorMessage="已安装更新版本。"/>
<MediaTemplate EmbedCab="yes"/>
<Icon Id="AppIcon" SourceFile="$icon"/>
<Property Id="ARPPRODUCTICON" Value="AppIcon"/><Property Id="ARPNOMODIFY" Value="1"/>
<Directory Id="TARGETDIR" Name="SourceDir"><Directory Id="LocalAppDataFolder"><Directory Id="ProgramsFolder" Name="Programs"><Directory Id="INSTALLFOLDER" Name="$folder"/></Directory></Directory><Directory Id="ProgramMenuFolder"/></Directory>
$($directories -join "`n")
$($components -join "`n")
<DirectoryRef Id="INSTALLFOLDER">
<Component Id="StartMenu" Guid="*" Win64="yes"><Shortcut Id="LaunchShortcut" Directory="ProgramMenuFolder" Name="$shortcut" Target="[INSTALLFOLDER]CaseScreenDesktop.exe" WorkingDirectory="INSTALLFOLDER" Icon="AppIcon"/><RegistryValue Root="HKCU" Key="$registry" Name="StartMenu" Type="integer" Value="1" KeyPath="yes"/></Component>
<Component Id="AutoStart" Guid="*" Win64="yes"><RegistryValue Root="HKCU" Key="Software\Microsoft\Windows\CurrentVersion\Run" Name="$runName" Type="string" Value="&quot;[INSTALLFOLDER]CaseScreenDesktop.exe&quot;" KeyPath="yes"/></Component>
</DirectoryRef>
<Feature Id="Core" Title="主程序" Level="1" Absent="disallow">$($references -join '')<ComponentRef Id="StartMenu"/></Feature>
<Feature Id="Startup" Title="登录 Windows 时自动启动" Description="可选，默认关闭。" Level="2"><ComponentRef Id="AutoStart"/></Feature>
<Property Id="INSTALLLEVEL" Value="1"/>
<UIRef Id="WixUI_FeatureTree"/><WixVariable Id="WixUILicenseRtf" Value="$notice"/>
</Product></Wix>
"@
[IO.File]::WriteAllText((Join-Path $work "installer.wxs"), $xml, [Text.UTF8Encoding]::new($false))
$noticeText = '{\rtf1\ansi ChassisCanvas ' + $Version + '\par Current-user Windows installation. Existing user settings and themes are retained.\par Windows startup is optional and disabled by default.\par See the included third-party notices for runtime and source license information.}'
[IO.File]::WriteAllText((Join-Path $work "notice.rtf"), $noticeText, [Text.UTF8Encoding]::new($false))
$temporaryOutput = Join-Path $work "setup.msi"
& (Join-Path $WixDirectory "candle.exe") -nologo -out "$work\installer.wixobj" "$work\installer.wxs"
if ($LASTEXITCODE) { throw "MSI 编译失败" }
& (Join-Path $WixDirectory "light.exe") -nologo -ext WixUIExtension -cultures:zh-cn -sice:ICE64 -out $temporaryOutput "$work\installer.wixobj"
if ($LASTEXITCODE) { throw "MSI 链接失败" }
Copy-Item -LiteralPath $temporaryOutput -Destination $output -Force
[PSCustomObject]@{ Version = $Version; PrivateLocal = [bool]$PrivateLocal; Installer = $output; SHA256 = (Get-FileHash -LiteralPath $output -Algorithm SHA256).Hash }
