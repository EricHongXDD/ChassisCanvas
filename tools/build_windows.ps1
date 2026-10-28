param(
    [string]$Version = "1.0.1",
    [string]$OutputDirectory = "",
    [switch]$PublicCore
)

$ErrorActionPreference = "Stop"
if ($Version -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$') {
    throw "版本号只能包含字母、数字、点、下划线和连字符，且最长 64 个字符：$Version"
}
$repositoryRoot = Split-Path -Parent $PSScriptRoot
$OutputDirectory = if ([string]::IsNullOrWhiteSpace($OutputDirectory)) {
    if ($PublicCore) {
        "artifacts\ChassisCanvas-windows-x64-{0}-core" -f $Version
    } else {
        "artifacts\CaseScreen-windows-x64-{0}" -f $Version
    }
} else {
    $OutputDirectory
}
$artifactsRoot = [System.IO.Path]::GetFullPath((Join-Path $repositoryRoot "artifacts"))
$outputPath = [System.IO.Path]::GetFullPath((Join-Path $repositoryRoot $OutputDirectory))
$archivePath = if ($PublicCore) {
    [System.IO.Path]::GetFullPath((Join-Path $repositoryRoot ("artifacts\ChassisCanvas-windows-x64-{0}-core.zip" -f $Version)))
} else {
    [System.IO.Path]::GetFullPath((Join-Path $repositoryRoot ("artifacts\CaseScreen-windows-x64-{0}.zip" -f $Version)))
}

function Ensure-PlainDirectory {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Label
    )

    if (-not (Test-Path -LiteralPath $Path)) {
        [System.IO.Directory]::CreateDirectory($Path) | Out-Null
    }
    Assert-PlainDirectory -Path $Path -Label $Label
}

function Assert-PlainDirectory {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Label
    )

    if (-not (Test-Path -LiteralPath $Path)) {
        throw "$Label 不存在：$Path"
    }
    $item = Get-Item -LiteralPath $Path -Force
    if (-not $item.PSIsContainer) {
        throw "$Label 不是目录：$Path"
    }
    if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "$Label 不得是重解析点：$Path"
    }
}

function Assert-PlainFile {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Label
    )

    if (-not (Test-Path -LiteralPath $Path)) {
        throw "$Label 不存在：$Path"
    }
    $item = Get-Item -LiteralPath $Path -Force
    if ($item.PSIsContainer) {
        throw "$Label 不是普通文件：$Path"
    }
    if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "$Label 不得是重解析点：$Path"
    }
}

function Assert-WindowsAMD64PEFile {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Label
    )

    Assert-PlainFile -Path $Path -Label $Label
    $peBytes = [System.IO.File]::ReadAllBytes($Path)
    if ($peBytes.Length -lt 64 -or $peBytes[0] -ne 0x4d -or $peBytes[1] -ne 0x5a) {
        throw "$Label 不是有效的 DOS/PE 文件：$Path"
    }
    $peHeaderOffset = [BitConverter]::ToInt32($peBytes, 0x3c)
    if ($peHeaderOffset -lt 64 -or $peHeaderOffset + 6 -gt $peBytes.Length -or
        $peBytes[$peHeaderOffset] -ne 0x50 -or $peBytes[$peHeaderOffset + 1] -ne 0x45 -or
        $peBytes[$peHeaderOffset + 2] -ne 0 -or $peBytes[$peHeaderOffset + 3] -ne 0) {
        throw "$Label 的 PE 头无效：$Path"
    }
    $machine = [BitConverter]::ToUInt16($peBytes, $peHeaderOffset + 4)
    if ($machine -ne [uint16]0x8664) {
        throw ("{0} 不是 AMD64 PE：{1} (Machine=0x{2:X4})" -f $Label, $Path, $machine)
    }
}

function Test-PathsOverlap {
    param(
        [Parameter(Mandatory = $true)][string]$Left,
        [Parameter(Mandatory = $true)][string]$Right
    )

    $separator = [System.IO.Path]::DirectorySeparatorChar
    $leftPath = [System.IO.Path]::GetFullPath($Left).TrimEnd($separator, [System.IO.Path]::AltDirectorySeparatorChar)
    $rightPath = [System.IO.Path]::GetFullPath($Right).TrimEnd($separator, [System.IO.Path]::AltDirectorySeparatorChar)
    if ($leftPath.Equals($rightPath, [System.StringComparison]::OrdinalIgnoreCase)) {
        return $true
    }
    $leftPrefix = $leftPath + $separator
    $rightPrefix = $rightPath + $separator
    return $leftPath.StartsWith($rightPrefix, [System.StringComparison]::OrdinalIgnoreCase) -or
        $rightPath.StartsWith($leftPrefix, [System.StringComparison]::OrdinalIgnoreCase)
}

function Get-PackageRelativePath {
    param(
        [Parameter(Mandatory = $true)][string]$BasePath,
        [Parameter(Mandatory = $true)][string]$Path
    )

    # Windows PowerShell 5.1 的 .NET Framework 没有 Path.GetRelativePath，
    # 用 URI 计算仅用于发行包检查的正斜杠相对路径。
    $separator = [System.IO.Path]::DirectorySeparatorChar
    $baseFullPath = [System.IO.Path]::GetFullPath($BasePath).TrimEnd($separator, [System.IO.Path]::AltDirectorySeparatorChar) + $separator
    $pathFullPath = [System.IO.Path]::GetFullPath($Path)
    $baseUri = [System.Uri]$baseFullPath
    $pathUri = [System.Uri]$pathFullPath
    $relativePath = [System.Uri]::UnescapeDataString($baseUri.MakeRelativeUri($pathUri).ToString()).Replace('\', '/')
    if ($relativePath -eq ".." -or $relativePath.StartsWith("../", [System.StringComparison]::Ordinal)) {
        throw "发行文件越过 staging：$Path"
    }
    return $relativePath
}

function Assert-PathUnderArtifacts {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Label
    )

    $rootWithSeparator = $artifactsRoot.TrimEnd([System.IO.Path]::DirectorySeparatorChar, [System.IO.Path]::AltDirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar
    if (-not $Path.StartsWith($rootWithSeparator, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "$Label 必须位于发行目录内：$artifactsRoot；实际路径：$Path"
    }
}

function Assert-PlainExistingPathChain {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Label
    )

    $candidate = [System.IO.Path]::GetDirectoryName($Path)
    while (-not [string]::IsNullOrWhiteSpace($candidate)) {
        if (-not $candidate.StartsWith($artifactsRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
            throw "$Label 的父目录越过发行根目录：$candidate"
        }
        if (Test-Path -LiteralPath $candidate) {
            $item = Get-Item -LiteralPath $candidate -Force
            if (-not $item.PSIsContainer) {
                throw "$Label 的父路径不是目录：$candidate"
            }
            if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
                throw "$Label 的父目录不得是重解析点：$candidate"
            }
        }
        if ($candidate.Equals($artifactsRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
            break
        }
        $next = [System.IO.Path]::GetDirectoryName($candidate)
        if ([string]::IsNullOrWhiteSpace($next) -or $next.Equals($candidate, [System.StringComparison]::OrdinalIgnoreCase)) {
            throw "$Label 无法追溯到发行根目录：$candidate"
        }
        $candidate = $next
    }
}

Ensure-PlainDirectory -Path $artifactsRoot -Label "发行根目录"
Assert-PathUnderArtifacts -Path $outputPath -Label "发行输出目录"
Assert-PathUnderArtifacts -Path $archivePath -Label "发行压缩包"

$stagingSuffix = "{0}-{1}" -f $PID, ([Guid]::NewGuid().ToString("N"))
$stagingPath = "$outputPath.staging-$stagingSuffix"
$previousPath = "$outputPath.previous-$stagingSuffix"
$archiveStagingPath = "$archivePath.staging-$stagingSuffix.zip"
$previousArchivePath = "$archivePath.previous-$stagingSuffix"
$executablePath = Join-Path $stagingPath "CaseScreen.exe"
$desktopLauncherPath = Join-Path $stagingPath "CaseScreenDesktop.exe"
Assert-PathUnderArtifacts -Path $stagingPath -Label "临时发行目录"
Assert-PathUnderArtifacts -Path $previousPath -Label "旧发行目录备份"
Assert-PathUnderArtifacts -Path $archiveStagingPath -Label "临时发行压缩包"
Assert-PathUnderArtifacts -Path $previousArchivePath -Label "旧发行压缩包备份"
$distinctTargets = @($outputPath, $archivePath, $stagingPath, $previousPath, $archiveStagingPath, $previousArchivePath) |
    ForEach-Object { $_.ToLowerInvariant() }
if (($distinctTargets | Select-Object -Unique).Count -ne $distinctTargets.Count) {
    throw "发行目录、压缩包与事务临时路径不得互相重合"
}
foreach ($checkedTarget in @(
    @{ Path = $outputPath; Label = "发行输出目录" },
    @{ Path = $archivePath; Label = "发行压缩包" },
    @{ Path = $stagingPath; Label = "临时发行目录" },
    @{ Path = $previousPath; Label = "旧发行目录备份" },
    @{ Path = $archiveStagingPath; Label = "临时发行压缩包" },
    @{ Path = $previousArchivePath; Label = "旧发行压缩包备份" }
)) {
    Assert-PlainExistingPathChain -Path $checkedTarget.Path -Label $checkedTarget.Label
}
if ((Test-Path -LiteralPath $outputPath) -and -not (Get-Item -LiteralPath $outputPath -Force).PSIsContainer) {
    throw "发行输出目标已存在但不是目录：$outputPath"
}
if ((Test-Path -LiteralPath $archivePath) -and (Get-Item -LiteralPath $archivePath -Force).PSIsContainer) {
    throw "发行压缩包目标已存在但不是普通文件：$archivePath"
}
if ((Test-Path -LiteralPath $stagingPath) -or (Test-Path -LiteralPath $previousPath) -or
    (Test-Path -LiteralPath $archiveStagingPath) -or (Test-Path -LiteralPath $previousArchivePath)) {
    throw "本次构建的随机临时路径已经存在，拒绝复用：$stagingSuffix"
}

$lockPath = Join-Path $artifactsRoot ".casescreen-windows-build.lock"
$reservedOutputPaths = @(
    (Join-Path $artifactsRoot "CaseScreen.exe"),
    (Join-Path $artifactsRoot "CaseScreenDesktop.exe"),
    $lockPath
)
$transactionTargets = @(
    @{ Path = $outputPath; Label = "发行输出目录" },
    @{ Path = $archivePath; Label = "发行压缩包" },
    @{ Path = $stagingPath; Label = "临时发行目录" },
    @{ Path = $previousPath; Label = "旧发行目录备份" },
    @{ Path = $archiveStagingPath; Label = "临时发行压缩包" },
    @{ Path = $previousArchivePath; Label = "旧发行压缩包备份" },
    @{ Path = $lockPath; Label = "构建锁" },
    @{ Path = $reservedOutputPaths[0]; Label = "保留后台程序路径" },
    @{ Path = $reservedOutputPaths[1]; Label = "保留工作台程序路径" }
)
for ($leftIndex = 0; $leftIndex -lt $transactionTargets.Count; $leftIndex++) {
    for ($rightIndex = $leftIndex + 1; $rightIndex -lt $transactionTargets.Count; $rightIndex++) {
        $leftTarget = $transactionTargets[$leftIndex]
        $rightTarget = $transactionTargets[$rightIndex]
        if (Test-PathsOverlap -Left $leftTarget.Path -Right $rightTarget.Path) {
            throw "$($leftTarget.Label)与$($rightTarget.Label)不得相同或互为父子路径：$($leftTarget.Path)；$($rightTarget.Path)"
        }
    }
}
foreach ($reservedOutputPath in $reservedOutputPaths) {
    if ($outputPath.Equals($reservedOutputPath, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "发行输出目录不得占用保留路径：$outputPath"
    }
}
if ([System.IO.Path]::GetExtension($outputPath).Equals(".zip", [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "发行输出目录不得使用 .zip 文件名：$outputPath"
}
if (Test-Path -LiteralPath $lockPath) {
    $existingLock = Get-Item -LiteralPath $lockPath -Force
    if ($existingLock.PSIsContainer -or (($existingLock.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)) {
        throw "构建锁必须是发行根目录中的普通文件：$lockPath"
    }
}
$buildLock = $null
try {
    $buildLock = [System.IO.File]::Open($lockPath, [System.IO.FileMode]::OpenOrCreate, [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::None)
}
catch [System.IO.IOException] {
    throw "另一个 CaseScreen Windows 构建正在使用发行目录；请等待其完成：$($_.Exception.Message)"
}
catch {
    throw "无法创建或打开发行构建锁：$lockPath；$($_.Exception.Message)"
}
$hadPreviousOutput = Test-Path -LiteralPath $outputPath
$hadPreviousArchive = Test-Path -LiteralPath $archivePath
$initialOutputCreationTicks = $null
$initialOutputWriteTicks = $null
$initialArchiveHash = $null
if ($hadPreviousOutput) {
    Assert-PlainDirectory -Path $outputPath -Label "原发行输出目录"
    $initialOutput = Get-Item -LiteralPath $outputPath -Force
    $initialOutputCreationTicks = $initialOutput.CreationTimeUtc.Ticks
    $initialOutputWriteTicks = $initialOutput.LastWriteTimeUtc.Ticks
}
if ($hadPreviousArchive) {
    Assert-PlainFile -Path $archivePath -Label "原发行压缩包"
    $initialArchiveHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath).Hash
}
$outputPublished = $false
$archivePublished = $false
$outputBackupCreated = $false
$archiveBackupCreated = $false
$transactionCommitted = $false

trap {
    $originalFailure = $_
    $recoveryFailures = [System.Collections.Generic.List[string]]::new()
    if (-not $transactionCommitted) {
        foreach ($recoveryStep in @(
            @{ Label = "撤回新发行压缩包"; Action = {
                if ($archivePublished -and (Test-Path -LiteralPath $archivePath) -and -not (Test-Path -LiteralPath $archiveStagingPath)) {
                    Move-Item -LiteralPath $archivePath -Destination $archiveStagingPath
                }
            } },
            @{ Label = "恢复旧发行压缩包"; Action = {
                if ($archiveBackupCreated -and (Test-Path -LiteralPath $previousArchivePath) -and -not (Test-Path -LiteralPath $archivePath)) {
                    Assert-PlainFile -Path $previousArchivePath -Label "旧发行压缩包备份"
                    Move-Item -LiteralPath $previousArchivePath -Destination $archivePath
                }
            } },
            @{ Label = "撤回新发行目录"; Action = {
                if ($outputPublished -and (Test-Path -LiteralPath $outputPath) -and -not (Test-Path -LiteralPath $stagingPath)) {
                    Move-Item -LiteralPath $outputPath -Destination $stagingPath
                }
            } },
            @{ Label = "恢复旧发行目录"; Action = {
                if ($outputBackupCreated -and (Test-Path -LiteralPath $previousPath) -and -not (Test-Path -LiteralPath $outputPath)) {
                    Assert-PlainDirectory -Path $previousPath -Label "旧发行目录备份"
                    Move-Item -LiteralPath $previousPath -Destination $outputPath
                }
            } }
        )) {
            try { & $recoveryStep.Action } catch { $recoveryFailures.Add("$($recoveryStep.Label)：$($_.Exception.Message)") }
        }
    }
    foreach ($cleanupStep in @(
        @{ Label = "清理临时发行目录"; Action = {
            if (Test-Path -LiteralPath $stagingPath) {
                Assert-PlainExistingPathChain -Path $stagingPath -Label "临时发行目录"
                Assert-PlainDirectory -Path $stagingPath -Label "临时发行目录"
                Remove-Item -LiteralPath $stagingPath -Recurse -Force
            }
        } },
        @{ Label = "清理临时发行压缩包"; Action = {
            if (Test-Path -LiteralPath $archiveStagingPath) {
                Assert-PlainFile -Path $archiveStagingPath -Label "临时发行压缩包"
                Remove-Item -LiteralPath $archiveStagingPath -Force
            }
        } }
    )) {
        try { & $cleanupStep.Action } catch { $recoveryFailures.Add("$($cleanupStep.Label)：$($_.Exception.Message)") }
    }
    if ($null -ne $buildLock) {
        try { $buildLock.Dispose() } catch { $recoveryFailures.Add("释放构建锁：$($_.Exception.Message)") }
    }
    if ($recoveryFailures.Count -gt 0) {
        throw "$($originalFailure.Exception.Message)；恢复/清理还发生：$($recoveryFailures -join '；')"
    }
    throw $originalFailure
}

Set-Location $repositoryRoot
chcp 65001 > $null
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$iconResourceScript = Join-Path $repositoryRoot "tools\windows_icon_resource.ps1"
$iconPNGPath = Join-Path $repositoryRoot "assets\casescreen-icon.png"
$iconICOPath = Join-Path $repositoryRoot "assets\casescreen-icon.ico"
Assert-PlainFile -Path $iconResourceScript -Label "Windows 图标资源工具"
# Windows PowerShell 5.1 会把无 BOM 的 UTF-8 脚本按系统 ANSI 代码页读取，
# 中文错误消息可能吞掉相邻引号并造成伪语法错误。显式按严格 UTF-8 读取后
# 再点调用脚本块，同时保持源文件原有的 UTF-8 编码。
$strictUTF8 = New-Object System.Text.UTF8Encoding($false, $true)
$iconResourceSource = [System.IO.File]::ReadAllText($iconResourceScript, $strictUTF8)
. ([ScriptBlock]::Create($iconResourceSource))
Assert-CaseScreenIconAssets -PNGPath $iconPNGPath -ICOPath $iconICOPath

$env:GOTELEMETRY = "off"
$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "amd64"
$env:GOAMD64 = "v1"
$goCachePath = Join-Path $repositoryRoot ".gocache"
$goModuleCachePath = Join-Path $repositoryRoot ".gomodcache"
$goPath = Join-Path $repositoryRoot ".gopath"
Ensure-PlainDirectory -Path $goCachePath -Label "Go 构建缓存"
Ensure-PlainDirectory -Path $goModuleCachePath -Label "Go 模块缓存"
Ensure-PlainDirectory -Path $goPath -Label "Go 工作目录"
$env:GOCACHE = $goCachePath
$env:GOMODCACHE = $goModuleCachePath
$env:GOPATH = $goPath

$mainSource = Get-Content -LiteralPath "cmd\casescreen\main.go" -Raw -Encoding UTF8
if ($mainSource -notmatch ('const version = "{0}"' -f [Regex]::Escape($Version))) {
    throw "脚本版本与 cmd\casescreen\main.go 不一致：$Version"
}
$desktopSource = Get-Content -LiteralPath "cmd\casescreen-desktop\main.go" -Raw -Encoding UTF8
if ($desktopSource -notmatch ('const desktopVersion = "{0}"' -f [Regex]::Escape($Version))) {
    throw "脚本版本与 cmd\casescreen-desktop\main.go 不一致：$Version"
}
if (-not $desktopSource.Contains("workbench.NewAPI(serviceURL, desktopVersion)")) {
    throw "Windows 工作台没有在后续重连时持续校验后台应用版本"
}
$versionMatch = [Regex]::Match($Version, '^(\d+)\.(\d+)(?:\.(\d+))?(?:\.(\d+))?$')
if (-not $versionMatch.Success) {
    throw "Windows VERSIONINFO 需要 2 到 4 段数字版本：$Version"
}
$versionBuild = if ($versionMatch.Groups[3].Success) { $versionMatch.Groups[3].Value } else { 0 }
$versionRevision = if ($versionMatch.Groups[4].Success) { $versionMatch.Groups[4].Value } else { 0 }
$numericVersionParts = @(
    [uint32]$versionMatch.Groups[1].Value,
    [uint32]$versionMatch.Groups[2].Value,
    [uint32]$versionBuild,
    [uint32]$versionRevision
)
if (@($numericVersionParts | Where-Object { $_ -gt [uint16]::MaxValue }).Count -ne 0) {
    throw "Windows VERSIONINFO 的每段数值不得超过 65535：$Version"
}
$requiredReleaseDocumentPatterns = @{
    "README.md" = ('(?m)^> \*\*正式发布版本：\*\* `{0}`\r?$' -f [Regex]::Escape($Version))
    "docs\windows-verification.md" = ('(?m)^# Windows 正式版 {0} 验证\r?$' -f [Regex]::Escape($Version))
}
foreach ($versionedDocument in $requiredReleaseDocumentPatterns.Keys) {
    $versionedText = Get-Content -LiteralPath $versionedDocument -Raw -Encoding UTF8
    if (-not [Regex]::IsMatch($versionedText, $requiredReleaseDocumentPatterns[$versionedDocument])) {
        throw "正式文档缺少精确的发布版本标题或字段：$versionedDocument -> $Version"
    }
}
foreach ($requiredHostMarker in @(
    "audio.NewEngine(audio.NewLoopback()",
    "lyrics.NewNowPlayingSource()",
    "lyrics.NeteaseProvider{Session: platformSessions}",
    "lyrics.QQProvider{Session: platformSessions}",
    "server.SetApplicationVersion(version)"
)) {
    if (-not $mainSource.Contains($requiredHostMarker)) {
        throw "Windows 正式入口缺少主机实现约束：$requiredHostMarker"
    }
}
foreach ($retiredSourceMarker in @(
    "playerhost",
    "lyrics.KugouProvider",
    "lyrics.SogouProvider",
    "recognitionrelay",
    "edgerecognition"
)) {
    if ($mainSource.Contains($retiredSourceMarker)) {
        throw "Windows 正式入口启用了已排除的识歌/猜词实现：$retiredSourceMarker"
    }
}
$loopbackSource = Get-Content -LiteralPath "internal\audio\loopback_windows.go" -Raw -Encoding UTF8
foreach ($requiredLoopbackMarker in @(
    "GetDefaultAudioEndpoint(wca.ERender",
    "wca.AUDCLNT_STREAMFLAGS_LOOPBACK"
)) {
    if (-not $loopbackSource.Contains($requiredLoopbackMarker)) {
        throw "Windows 正式音频源不再是系统输出回环：$requiredLoopbackMarker"
    }
}
if ($loopbackSource.Contains("GetDefaultAudioEndpoint(wca.ECapture")) {
    throw "Windows 正式音频源不得打开麦克风采集端点"
}

# 正式 UI 只说明主机内置能力，不提供或暗示另购硬件、屏端网络、
# 麦克风音频识别及第三方辅助程序入口。
$formalUISources = @(
    "internal\workbench\platform_login.go",
    "internal\workbench\file_dialog_windows.go",
    "internal\workbench\gio_ui.go",
    "internal\workbench\gio_gpu_settings.go",
    "internal\api\index.go"
)
$forbiddenUserFacingMarkers = @(
    "识歌", "麦克风", "microphone", "声纹", "acrcloud", "独立桥", "外部桥", "网桥", "bridge",
    "companion", "edgesync", "source-push", "bluez", "rndis",
    "recognition-relay", "cmd/casescreen-edge", "启动并连接网易云",
    "启动网易云并同步播放信息", "remote-debugging", "player-host/netease",
    "thermalright", "利民"
)
foreach ($formalUISource in $formalUISources) {
    $formalUIText = (Get-Content -LiteralPath $formalUISource -Raw -Encoding UTF8).ToLowerInvariant()
    foreach ($forbiddenMarker in $forbiddenUserFacingMarkers) {
        if ($formalUIText.Contains($forbiddenMarker)) {
            throw "正式 Windows UI 含有已移除功能或外部设备入口：$formalUISource -> $forbiddenMarker"
        }
    }
}
if (-not (Get-Content -LiteralPath "internal\workbench\platform_login.go" -Raw -Encoding UTF8).Contains("无需购买、连接或安装任何额外设备或辅助程序")) {
    throw "网易云主机同步 UI 没有明确说明无需额外设备或辅助程序"
}
$gpuUI = Get-Content -LiteralPath "internal\workbench\gio_gpu_settings.go" -Raw -Encoding UTF8
foreach ($requiredGPUMarker in @(
    'label: "系统自动"',
    "status.Adapters",
    "state.dropdownOpen",
    "SaveGPUAdapter(adapterID)"
)) {
    if (-not $gpuUI.Contains($requiredGPUMarker)) {
        throw "Windows GPU UI 不再是系统自动加实际显卡名称的下拉框：$requiredGPUMarker"
    }
}
foreach ($retiredGPUMarker in @(
    '"通常"',
    "PowerSavingLabel",
    "HighPerformanceLabel"
)) {
    if ($gpuUI.Contains($retiredGPUMarker)) {
        throw "Windows GPU UI 恢复了旧提示或旧三按钮实现：$retiredGPUMarker"
    }
}

# 以 Go 实际依赖图而不是文件名猜测发行边界。研究代码可在同一
# module 中继续编译测试，但不能成为两个 Windows 正式入口的间接依赖。
$productionDependencies = @(go list -buildvcs=false -deps -f "{{.ImportPath}}" ".\cmd\casescreen" ".\cmd\casescreen-desktop")
if ($LASTEXITCODE -ne 0) {
    throw "无法计算 Windows 正式入口依赖图"
}
$requiredProductionDependencies = @(
    "casescreen/internal/audio",
    "casescreen/internal/lyrics",
    "casescreen/internal/gpupreference",
    "casescreen/internal/platformauth",
    "casescreen/internal/platformid",
    "casescreen/internal/service"
)
foreach ($requiredDependency in $requiredProductionDependencies) {
    if ($productionDependencies -notcontains $requiredDependency) {
        throw "Windows 正式入口缺少主机能力依赖：$requiredDependency"
    }
}
$forbiddenProductionDependencies = @(
    "casescreen/internal/playerhost",
    "casescreen/internal/edge",
    "casescreen/internal/edgebridge",
    "casescreen/internal/edgerecognition",
    "casescreen/internal/edgeprobe",
    "casescreen/internal/edgelyrics",
    "casescreen/internal/edgesync",
    "casescreen/internal/edgepush",
    "casescreen/internal/edgedisplay",
    "casescreen/internal/edgemetadata",
    "casescreen/internal/edgesetup",
    "casescreen/internal/recognitionrelay",
    "casescreen/internal/device/thermalright",
    "casescreen/internal/usb/libusb"
)
foreach ($forbiddenDependency in $forbiddenProductionDependencies) {
    if ($productionDependencies -contains $forbiddenDependency) {
        throw "Windows 正式入口依赖了已移除或实验代码：$forbiddenDependency"
    }
}
foreach ($productionDependency in $productionDependencies) {
    if ($productionDependency.StartsWith("casescreen/internal/edge", [System.StringComparison]::Ordinal) -or
        $productionDependency -eq "casescreen/internal/recognitionrelay") {
        throw "Windows 正式入口依赖了屏端/识歌实验代码：$productionDependency"
    }
}

# 两个 EXE 直接构建到本次随机 staging，避免并行任务共享中间文件。
New-Item -ItemType Directory -Path $stagingPath | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stagingPath "docs") | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stagingPath "themes") | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stagingPath "desktop") | Out-Null

# GUI 子系统保证双击启动时只显示托盘，不弹出控制台窗口。
go build -buildvcs=false -ldflags "-s -w -H=windowsgui" -o $executablePath ".\cmd\casescreen"
if ($LASTEXITCODE -ne 0) {
    throw "CaseScreen.exe 构建失败"
}
go build -buildvcs=false -ldflags "-s -w -H=windowsgui" -o $desktopLauncherPath ".\cmd\casescreen-desktop"
if ($LASTEXITCODE -ne 0) {
    throw "CaseScreenDesktop.exe 构建失败"
}

# 同一份多尺寸 ICO 写入两个入口；VERSIONINFO 则保留各自的文件用途与原始文件名。
$windowsExecutableResources = @(
    @{ Path = $executablePath; Description = "CaseScreen 后台服务"; OriginalFilename = "CaseScreen.exe" },
    @{ Path = $desktopLauncherPath; Description = "CaseScreen 主题工作台"; OriginalFilename = "CaseScreenDesktop.exe" }
)
foreach ($resourceTarget in $windowsExecutableResources) {
    Set-CaseScreenExecutableIcon -ExecutablePath $resourceTarget.Path -ICOPath $iconICOPath
    Set-CaseScreenExecutableVersionInfo -ExecutablePath $resourceTarget.Path `
        -Major $numericVersionParts[0] -Minor $numericVersionParts[1] `
        -VersionBuild $numericVersionParts[2] -Revision $numericVersionParts[3] `
        -Version $Version -ProductName "CaseScreen" `
        -FileDescription $resourceTarget.Description -OriginalFilename $resourceTarget.OriginalFilename
    Assert-CaseScreenExecutableIcon -ExecutablePath $resourceTarget.Path
    Assert-CaseScreenExecutableVersionInfo -ExecutablePath $resourceTarget.Path `
        -Major $numericVersionParts[0] -Minor $numericVersionParts[1] `
        -VersionBuild $numericVersionParts[2] -Revision $numericVersionParts[3] `
        -Version $Version -ProductName "CaseScreen" `
        -FileDescription $resourceTarget.Description -OriginalFilename $resourceTarget.OriginalFilename
}

# 在打包前直接核对两个入口都是 x64 Windows GUI PE。
foreach ($guiExecutable in @($executablePath, $desktopLauncherPath)) {
    $peBytes = [System.IO.File]::ReadAllBytes($guiExecutable)
    if ($peBytes.Length -lt 64 -or $peBytes[0] -ne 0x4d -or $peBytes[1] -ne 0x5a) {
        throw "Executable is not a valid DOS/PE binary: $guiExecutable"
    }
    $peHeaderOffset = [BitConverter]::ToInt32($peBytes, 0x3c)
    if ($peHeaderOffset -lt 64 -or $peHeaderOffset + 24 -gt $peBytes.Length -or
        $peBytes[$peHeaderOffset] -ne 0x50 -or $peBytes[$peHeaderOffset + 1] -ne 0x45 -or
        $peBytes[$peHeaderOffset + 2] -ne 0 -or $peBytes[$peHeaderOffset + 3] -ne 0) {
        throw "Executable has an invalid PE header: $guiExecutable"
    }
    $machine = [BitConverter]::ToUInt16($peBytes, $peHeaderOffset + 4)
    if ($machine -ne [uint16]0x8664) {
        throw ("Executable is not an AMD64 PE binary: {0} (Machine=0x{1:X4})" -f $guiExecutable, $machine)
    }
    $optionalHeaderSize = [BitConverter]::ToUInt16($peBytes, $peHeaderOffset + 20)
    if ($optionalHeaderSize -lt 70 -or $peHeaderOffset + 24 + $optionalHeaderSize -gt $peBytes.Length) {
        throw "Executable has a truncated PE optional header: $guiExecutable"
    }
    $subsystemOffset = $peHeaderOffset + 24 + 68
    $subsystem = [BitConverter]::ToUInt16($peBytes, $subsystemOffset)
    if ($subsystem -ne 2) {
        throw "Executable is not a Windows GUI subsystem binary: $guiExecutable ($subsystem)"
    }
}

Copy-Item -LiteralPath "README.md" -Destination (Join-Path $stagingPath "README.md") -Force
Copy-Item -LiteralPath "examples\device-config.json" -Destination (Join-Path $stagingPath "device-config.example.json") -Force
Copy-Item -LiteralPath "docs\windows-production.md" -Destination (Join-Path $stagingPath "docs\windows-production.md") -Force
Copy-Item -LiteralPath "docs\third-party-notices.md" -Destination (Join-Path $stagingPath "docs\third-party-notices.md") -Force
Copy-Item -LiteralPath "docs\windows-verification.md" -Destination (Join-Path $stagingPath "docs\windows-verification.md") -Force
Copy-Item -LiteralPath "docs\host-lyrics.md" -Destination (Join-Path $stagingPath "docs\host-lyrics.md") -Force
Copy-Item -LiteralPath "docs\coolermaster-orientation.md" -Destination (Join-Path $stagingPath "docs\coolermaster-orientation.md") -Force
$productionThemeFiles = @(
    "themes\host-layer-demo.json",
    "themes\lyrics-overlay.json",
    "themes\music-bars-overlay.json",
    "themes\music-ring-overlay.json"
)
foreach ($productionThemeFile in $productionThemeFiles) {
    Copy-Item -LiteralPath $productionThemeFile -Destination (Join-Path $stagingPath "themes") -Force
}
Copy-Item -LiteralPath "desktop\README.md" -Destination (Join-Path $stagingPath "desktop\README.md") -Force

if ($PublicCore) {
    $publicRuntimeNotice = @"
ChassisCanvas GitHub 公开核心包

此压缩包不包含 FFmpeg、hidapi、厂商安装程序或 TF 卡镜像。
CaseScreen.exe 与 CaseScreenDesktop.exe 是品牌迁移期间保留的兼容文件名。

如需视频处理或酷冷至尊设备支持，请从具有明确许可的来源自行取得兼容的
FFmpeg 与 hidapi 运行时，并按照 docs/third-party-notices.md 核对许可证和 ABI。
微雪基础 Theme 也必须由用户从自己拥有的设备或原厂软件中取得。
"@
    [System.IO.File]::WriteAllText(
        (Join-Path $stagingPath "RUNTIME-NOTICE.txt"),
        $publicRuntimeNotice,
        [System.Text.UTF8Encoding]::new($false)
    )
}

$formalReleaseDocuments = @(
    "README.md",
    "desktop\README.md",
    "docs\windows-production.md",
    "docs\windows-verification.md",
    "docs\host-lyrics.md",
    "docs\coolermaster-orientation.md"
)
foreach ($formalDocument in $formalReleaseDocuments) {
    $formalDocumentText = (Get-Content -LiteralPath (Join-Path $stagingPath $formalDocument) -Raw -Encoding UTF8).ToLowerInvariant()
    foreach ($forbiddenMarker in $forbiddenUserFacingMarkers) {
        if ($formalDocumentText.Contains($forbiddenMarker)) {
            throw "正式发行操作说明含有研究功能或外部设备入口：$formalDocument -> $forbiddenMarker"
        }
    }
}

# 本机完整包仍要求经过人工核验的第三方运行时；公开核心包明确不打包
# FFmpeg、hidapi 或任何从厂商软件提取的二进制文件。
if (-not $PublicCore) {
    $runtimeFiles = @(
        "hidapi.dll",
        "ffmpeg.exe",
        "avcodec-58.dll",
        "avdevice-58.dll",
        "avfilter-7.dll",
        "avformat-58.dll",
        "avutil-56.dll",
        "swresample-3.dll",
        "swscale-5.dll"
    )
    foreach ($runtimeFile in $runtimeFiles) {
        $runtimeDestination = Join-Path $stagingPath $runtimeFile
        $runtimeCandidates = @(
            (Join-Path $outputPath $runtimeFile),
            (Join-Path $repositoryRoot ("artifacts\CaseScreen-windows-x64\{0}" -f $runtimeFile)),
            (Join-Path $repositoryRoot ("artifacts\{0}" -f $runtimeFile)),
            (Join-Path $repositoryRoot ("artifacts\MasterCTRL-app\File\{0}" -f $runtimeFile)),
            (Join-Path $repositoryRoot (".tmp\ffmpeg-runtime\{0}" -f $runtimeFile))
        )
        $runtimeSource = $runtimeCandidates | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
        if ($runtimeSource) {
            Assert-PlainFile -Path $runtimeSource -Label "运行时源文件 $runtimeFile"
            Copy-Item -LiteralPath $runtimeSource -Destination $runtimeDestination -Force
        }
        if (-not (Test-Path -LiteralPath $runtimeDestination)) {
            throw "发行目录缺少运行时文件：$runtimeFile"
        }
        Assert-WindowsAMD64PEFile -Path $runtimeDestination -Label "运行时文件 $runtimeFile"
    }
}

# 正式 Windows 包只是主机控制器：不带入屏端可执行文件、识歌中继、
# 网桥脚本、任何平台 Cookie/token 或实验识歌说明。实验源码可以留在仓库，
# 但即使旧发行目录曾经有这些文件，全新 staging 也必须失败关闭。
$forbiddenWindowsReleaseNames = @(
    "casescreen-edge",
    "casescreen-edge.exe",
    "casescreen-edge-setup",
    "casescreen-edge-setup.exe",
    "casescreen-edge-display",
    "casescreen-edge-display.exe",
    "casescreen-recognition-relay",
    "casescreen-recognition-relay.exe",
    "casescreen-acrcloud-fingerprint",
    "casescreen-acrcloud-fingerprint.exe",
    "casescreen-source-push",
    "casescreen-source-push.exe",
    "recognition.env",
    "recognition.token",
    "qq.cookies.txt",
    "netease.cookies.txt",
    "libusb-1.0.dll",
    "standalone-recognition.md"
)
$forbiddenWindowsReleaseExtensions = @(
    ".py", ".sh", ".so",
    ".env", ".token", ".cookie", ".cookies",
    ".pem", ".key", ".pfx", ".p12"
)
foreach ($releaseFile in Get-ChildItem -LiteralPath $stagingPath -Recurse -File) {
    $relativePath = Get-PackageRelativePath -BasePath $stagingPath -Path $releaseFile.FullName
    $lowerName = $releaseFile.Name.ToLowerInvariant()
    $lowerExtension = $releaseFile.Extension.ToLowerInvariant()
    if ($forbiddenWindowsReleaseNames -contains $lowerName) {
        throw "正式 Windows 发行包夹带了屏端识歌或凭据文件：$relativePath"
    }
    if ($forbiddenWindowsReleaseExtensions -contains $lowerExtension) {
        throw "正式 Windows 发行包夹带了非 Windows 研究程序或脚本：$relativePath"
    }
    if ($lowerName -match '(cookie|credential|secret|token)') {
        throw "正式 Windows 发行包夹带了疑似凭据文件：$relativePath"
    }
    if ($relativePath -match '^(screen|server|deploy|firmware)/') {
        throw "正式 Windows 发行包夹带了实验部署目录：$relativePath"
    }
}
$productionExample = Get-Content -LiteralPath (Join-Path $stagingPath "device-config.example.json") -Raw -Encoding UTF8
$productionExampleObject = $productionExample | ConvertFrom-Json
if ($productionExampleObject.gpu_preference -ne "system") {
    throw "正式 Windows 默认配置必须显式使用系统默认 GPU 首选项"
}
foreach ($forbiddenConfigKey in @(
    "recognition_relay_url",
    "recognition_token",
    "recognition_pcm",
    "edge_sync",
    "source_push",
    "screen_cookie",
    "screen_network"
)) {
    if ($productionExample -match ('"{0}"\s*:' -f [Regex]::Escape($forbiddenConfigKey))) {
        throw "正式 Windows 默认配置包含屏端实验字段：$forbiddenConfigKey"
    }
}

# 始终先压缩到本次随机临时文件，校验完成前不覆盖同版本旧 ZIP。
Assert-PlainDirectory -Path $stagingPath -Label "临时发行目录"
Compress-Archive -Path (Join-Path $stagingPath "*") -DestinationPath $archiveStagingPath -CompressionLevel Optimal
Assert-PlainFile -Path $archiveStagingPath -Label "临时发行压缩包"
Add-Type -AssemblyName System.IO.Compression.FileSystem
$archiveReader = [System.IO.Compression.ZipFile]::OpenRead($archiveStagingPath)
try {
    $archiveEntries = @($archiveReader.Entries | ForEach-Object { $_.FullName.Replace('\', '/') })
}
finally {
    $archiveReader.Dispose()
}
$requiredArchiveEntries = @(
    "CaseScreen.exe",
    "CaseScreenDesktop.exe",
    "README.md",
    "device-config.example.json",
    "docs/third-party-notices.md",
    "docs/coolermaster-orientation.md"
)
if ($PublicCore) {
    $requiredArchiveEntries += "RUNTIME-NOTICE.txt"
}
foreach ($requiredArchiveEntry in $requiredArchiveEntries) {
    if ($archiveEntries -notcontains $requiredArchiveEntry) {
        throw "临时发行压缩包缺少必要文件：$requiredArchiveEntry"
    }
}
$stagedArchiveHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $archiveStagingPath).Hash

# 仅替换经过校验的单个版本目录；其他版本目录和历史压缩包保持不变。
foreach ($checkedTarget in @(
    @{ Path = $outputPath; Label = "发行输出目录" },
    @{ Path = $archivePath; Label = "发行压缩包" },
    @{ Path = $stagingPath; Label = "临时发行目录" },
    @{ Path = $previousPath; Label = "旧发行目录备份" },
    @{ Path = $archiveStagingPath; Label = "临时发行压缩包" },
    @{ Path = $previousArchivePath; Label = "旧发行压缩包备份" }
)) {
    Assert-PlainExistingPathChain -Path $checkedTarget.Path -Label $checkedTarget.Label
}
$currentOutputExists = Test-Path -LiteralPath $outputPath
$currentArchiveExists = Test-Path -LiteralPath $archivePath
if ($currentOutputExists -ne $hadPreviousOutput) {
    throw "发行输出目录在构建期间被其他进程创建或删除，拒绝覆盖：$outputPath"
}
if ($currentArchiveExists -ne $hadPreviousArchive) {
    throw "发行压缩包在构建期间被其他进程创建或删除，拒绝覆盖：$archivePath"
}
if ((Test-Path -LiteralPath $previousPath) -or (Test-Path -LiteralPath $previousArchivePath)) {
    throw "事务备份路径在构建期间被占用，拒绝发布"
}
Assert-PlainDirectory -Path $stagingPath -Label "临时发行目录"
Assert-PlainFile -Path $archiveStagingPath -Label "临时发行压缩包"
if (Test-Path -LiteralPath $outputPath) {
    $existingOutput = Get-Item -LiteralPath $outputPath -Force
    if (-not $existingOutput.PSIsContainer) {
        throw "发行输出目标已存在但不是目录：$outputPath"
    }
    if (($existingOutput.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "拒绝替换重解析点发行目录：$outputPath"
    }
    if ($existingOutput.CreationTimeUtc.Ticks -ne $initialOutputCreationTicks -or
        $existingOutput.LastWriteTimeUtc.Ticks -ne $initialOutputWriteTicks) {
        throw "发行输出目录在构建期间发生变化，拒绝覆盖：$outputPath"
    }
    Move-Item -LiteralPath $outputPath -Destination $previousPath
    $outputBackupCreated = $true
}
Move-Item -LiteralPath $stagingPath -Destination $outputPath
$outputPublished = $true
if (Test-Path -LiteralPath $archivePath) {
    $existingArchive = Get-Item -LiteralPath $archivePath -Force
    if ($existingArchive.PSIsContainer) {
        throw "发行压缩包目标已存在但不是普通文件：$archivePath"
    }
    if (($existingArchive.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "拒绝替换重解析点发行压缩包：$archivePath"
    }
    $currentArchiveHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath).Hash
    if ($currentArchiveHash -ne $initialArchiveHash) {
        throw "发行压缩包在构建期间发生变化，拒绝覆盖：$archivePath"
    }
    Move-Item -LiteralPath $archivePath -Destination $previousArchivePath
    $archiveBackupCreated = $true
}
Move-Item -LiteralPath $archiveStagingPath -Destination $archivePath
$archivePublished = $true

$hash = Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath
if ($hash.Hash -ne $stagedArchiveHash) {
    throw "发行压缩包在发布移动后哈希发生变化：$archivePath"
}
$archive = Get-Item -LiteralPath $archivePath
$transactionCommitted = $true

# 旧版本备份只在新目录与新 ZIP 都可读后清理；清理失败不会撤销有效发布物。
if ($outputBackupCreated -and (Test-Path -LiteralPath $previousPath)) {
    try {
        Assert-PlainDirectory -Path $previousPath -Label "旧发行目录备份"
        Remove-Item -LiteralPath $previousPath -Recurse -Force
    } catch { Write-Warning "旧发行目录备份未能清理：$($_.Exception.Message)" }
}
if ($archiveBackupCreated -and (Test-Path -LiteralPath $previousArchivePath)) {
    try {
        Assert-PlainFile -Path $previousArchivePath -Label "旧发行压缩包备份"
        Remove-Item -LiteralPath $previousArchivePath -Force
    } catch { Write-Warning "旧发行压缩包备份未能清理：$($_.Exception.Message)" }
}
if ($null -ne $buildLock) {
    try { $buildLock.Dispose() } catch { Write-Warning "构建锁释放失败：$($_.Exception.Message)" }
    $buildLock = $null
}

[PSCustomObject]@{
    Version = $Version
    Archive = $archive.FullName
    Length = $archive.Length
    SHA256 = $hash.Hash
} | Format-List
