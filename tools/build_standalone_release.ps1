param(
    [string]$Version = "beta.13"
)

$ErrorActionPreference = "Stop"
$repositoryRoot = Split-Path -Parent $PSScriptRoot
$artifactsRoot = [System.IO.Path]::GetFullPath((Join-Path $repositoryRoot "artifacts"))
$releaseName = "CaseScreen-Waveshare-standalone-recognition-$Version"
$documentVersion = $Version.Replace(".", "")
$outputPath = [System.IO.Path]::GetFullPath((Join-Path $artifactsRoot $releaseName))
$archivePath = [System.IO.Path]::GetFullPath((Join-Path $artifactsRoot "$releaseName.zip"))
$stagingSuffix = "{0}-{1}" -f $PID, ([Guid]::NewGuid().ToString("N"))
$stagingPath = "$outputPath.staging-$stagingSuffix"
$previousPath = "$outputPath.previous-$stagingSuffix"
$temporaryArchive = [System.IO.Path]::GetFullPath((Join-Path $artifactsRoot "$releaseName.staging-$stagingSuffix.zip"))
$previousArchive = [System.IO.Path]::GetFullPath((Join-Path $artifactsRoot "$releaseName.previous-$stagingSuffix.zip"))
$newOutputCommitted = $false
$newArchiveCommitted = $false

function Assert-PathUnderArtifacts {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Label
    )
    $rootWithSeparator = $artifactsRoot.TrimEnd(
        [System.IO.Path]::DirectorySeparatorChar,
        [System.IO.Path]::AltDirectorySeparatorChar
    ) + [System.IO.Path]::DirectorySeparatorChar
    if (-not $Path.StartsWith($rootWithSeparator, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "$Label 必须位于发行目录内：$artifactsRoot；实际路径：$Path"
    }
}

foreach ($candidate in @($outputPath, $archivePath, $stagingPath, $previousPath, $temporaryArchive, $previousArchive)) {
    Assert-PathUnderArtifacts -Path $candidate -Label "发行路径"
}

trap {
    # 失败时只处理本次随机命名的路径，并尽量恢复同版本旧产物。
    if ($newOutputCommitted -and (Test-Path -LiteralPath $outputPath)) {
        if (Test-Path -LiteralPath $stagingPath) {
            throw "发行回滚目标已存在，拒绝覆盖：$stagingPath"
        }
        Move-Item -LiteralPath $outputPath -Destination $stagingPath
        $newOutputCommitted = $false
    }
    if ($newArchiveCommitted -and (Test-Path -LiteralPath $archivePath)) {
        if (Test-Path -LiteralPath $temporaryArchive) {
            throw "发行 ZIP 回滚目标已存在，拒绝覆盖：$temporaryArchive"
        }
        Move-Item -LiteralPath $archivePath -Destination $temporaryArchive
        $newArchiveCommitted = $false
    }
    if ((Test-Path -LiteralPath $previousPath) -and -not (Test-Path -LiteralPath $outputPath)) {
        Move-Item -LiteralPath $previousPath -Destination $outputPath
    }
    if ((Test-Path -LiteralPath $previousArchive) -and -not (Test-Path -LiteralPath $archivePath)) {
        Move-Item -LiteralPath $previousArchive -Destination $archivePath
    }
    foreach ($temporary in @($stagingPath, $temporaryArchive)) {
        if (Test-Path -LiteralPath $temporary) {
            Remove-Item -LiteralPath $temporary -Recurse -Force
        }
    }
    throw $_
}

Set-Location $repositoryRoot
chcp 65001 > $null
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$edgeSource = "artifacts\casescreen-edge-linux-armv7-$Version"
$relaySource = "artifacts\casescreen-recognition-relay-linux-amd64-$Version"
$fingerprintSource = "artifacts\casescreen-acrcloud-fingerprint-linux-armv7-$Version"
$releaseDocument = "docs\standalone-release-$documentVersion.md"
$requiredSources = @(
    $edgeSource,
    $relaySource,
    $fingerprintSource,
    $releaseDocument,
    "output\standalone-themes\casescreen-lyrics-1024x600.Theme",
    "output\standalone-themes\casescreen-lyrics-480x1920.Theme"
)
foreach ($requiredSource in $requiredSources) {
    if (-not (Test-Path -LiteralPath $requiredSource -PathType Leaf)) {
        throw "发行输入不存在：$requiredSource"
    }
}

New-Item -ItemType Directory -Path $stagingPath | Out-Null
foreach ($relativeDirectory in @("screen", "server", "docs", "firmware", "themes", "tools", "deploy")) {
    New-Item -ItemType Directory -Path (Join-Path $stagingPath $relativeDirectory) | Out-Null
}
foreach ($relativeDirectory in @(
    "deploy\recognition-relay",
    "deploy\waveshare-usb-bridge",
    "deploy\waveshare-usb-bridge\esp32-s3",
    "deploy\waveshare-usb-bridge\esp32-s3\main"
)) {
    New-Item -ItemType Directory -Path (Join-Path $stagingPath $relativeDirectory) | Out-Null
}

Copy-Item -LiteralPath $edgeSource -Destination (Join-Path $stagingPath "screen\casescreen-edge")
Copy-Item -LiteralPath $fingerprintSource -Destination (Join-Path $stagingPath "screen\casescreen-acrcloud-fingerprint")
Copy-Item -LiteralPath $relaySource -Destination (Join-Path $stagingPath "server\casescreen-recognition-relay")
Copy-Item -LiteralPath "README.md" -Destination (Join-Path $stagingPath "README.md")

foreach ($document in @(
    "architecture.md",
    "recognition-relay.md",
    "standalone-lyrics.md",
    "standalone-recognition.md",
    "standalone-release-$documentVersion.md",
    "verification.md"
)) {
    Copy-Item -LiteralPath (Join-Path "docs" $document) -Destination (Join-Path $stagingPath "docs\$document")
}

Copy-Item -LiteralPath "firmware\waveshare\README.md" -Destination (Join-Path $stagingPath "firmware\README.md")
Copy-Item -LiteralPath "firmware\waveshare\serial_uart_casescreen.sh" -Destination (Join-Path $stagingPath "firmware\serial_uart_casescreen.sh")
Copy-Item -LiteralPath "output\standalone-themes\casescreen-lyrics-1024x600.Theme" -Destination (Join-Path $stagingPath "themes")
Copy-Item -LiteralPath "output\standalone-themes\casescreen-lyrics-480x1920.Theme" -Destination (Join-Path $stagingPath "themes")

foreach ($tool in @(
    "create_release_zip.py",
    "expand_gpt_ext4_image.py",
    "patch_waveshare_image.py",
    "build_standalone_release.ps1"
)) {
    Copy-Item -LiteralPath (Join-Path "tools" $tool) -Destination (Join-Path $stagingPath "tools\$tool")
}
Copy-Item -LiteralPath "tools\acrcloud_fingerprint_wrapper" -Destination (Join-Path $stagingPath "tools") -Recurse

# 部署目录只复制经过审查的示例和源码白名单，避免把部署者后来写入的正式 .env、token、
# Cookie、sdkconfig 或构建目录带进发行包。
foreach ($relayExample in @(
    "acrcloud.env.example",
    "Caddyfile.example",
    "casescreen-recognition-relay.service.example",
    "native-id-mappings.example.json",
    "recognition-tokens.example.json"
)) {
    Copy-Item -LiteralPath (Join-Path "deploy\recognition-relay" $relayExample) `
        -Destination (Join-Path $stagingPath "deploy\recognition-relay\$relayExample")
}
foreach ($bridgeFile in @("README.md", "configure_raspberry_pi.sh")) {
    Copy-Item -LiteralPath (Join-Path "deploy\waveshare-usb-bridge" $bridgeFile) `
        -Destination (Join-Path $stagingPath "deploy\waveshare-usb-bridge\$bridgeFile")
}
foreach ($espFile in @(
    ".gitignore",
    "CMakeLists.txt",
    "README.md",
    "sdkconfig.defaults",
    "sdkconfig.defaults.esp32s3"
)) {
    Copy-Item -LiteralPath (Join-Path "deploy\waveshare-usb-bridge\esp32-s3" $espFile) `
        -Destination (Join-Path $stagingPath "deploy\waveshare-usb-bridge\esp32-s3\$espFile")
}
foreach ($espMainFile in @(
    "casescreen_usb_bridge.c",
    "CMakeLists.txt",
    "idf_component.yml",
    "Kconfig.projbuild"
)) {
    Copy-Item -LiteralPath (Join-Path "deploy\waveshare-usb-bridge\esp32-s3\main" $espMainFile) `
        -Destination (Join-Path $stagingPath "deploy\waveshare-usb-bridge\esp32-s3\main\$espMainFile")
}

# 正式凭据、Cookie、CA、厂商共享库不得进入公开发行包。
$forbiddenFiles = Get-ChildItem -LiteralPath $stagingPath -Recurse -File | Where-Object {
    $_.Extension -in @(".so", ".pem", ".key", ".cookie", ".cookies", ".token") -or
    ($_.Extension -eq ".env" -and $_.Name -notmatch "(?i)\.env\.example$") -or
    $_.Name -eq "sdkconfig" -or
    ($_.Name -match "(?i)(secret|credential|cookie|token)" -and $_.Name -notmatch "(?i)example")
}
if ($forbiddenFiles) {
    throw "发行包包含禁止文件：$($forbiddenFiles.FullName -join ', ')"
}

$acrExample = Get-Content -LiteralPath (Join-Path $stagingPath "deploy\recognition-relay\acrcloud.env.example") -Encoding UTF8
foreach ($placeholder in @(
    "ACRCLOUD_ACCESS_KEY=REPLACE_WITH_PROJECT_ACCESS_KEY",
    "ACRCLOUD_ACCESS_SECRET=REPLACE_WITH_PROJECT_ACCESS_SECRET"
)) {
    if ($placeholder -notin $acrExample) {
        throw "ACRCloud 示例已经写入非占位凭据，拒绝打包"
    }
}
$tokenExample = Get-Content -LiteralPath (Join-Path $stagingPath "deploy\recognition-relay\recognition-tokens.example.json") -Raw -Encoding UTF8
if ($tokenExample -notmatch '"token_sha256"\s*:\s*"REPLACE_WITH_64_HEX_SHA256_OF_THE_DEVICE_TOKEN"' -or
    $tokenExample -match '"token_sha256"\s*:\s*"[0-9A-Fa-f]{64}"') {
    throw "设备令牌示例已经写入真实摘要，拒绝打包"
}
$espKconfig = Get-Content -LiteralPath (Join-Path $stagingPath "deploy\waveshare-usb-bridge\esp32-s3\main\Kconfig.projbuild") -Raw -Encoding UTF8
if (($espKconfig | Select-String -Pattern '(?m)^\s*default\s+""\s*$' -AllMatches).Matches.Count -lt 2) {
    throw "ESP32 实验配置可能包含非空 Wi-Fi 默认凭据，拒绝打包"
}

$manifestLines = Get-ChildItem -LiteralPath $stagingPath -Recurse -File |
    Sort-Object FullName |
    ForEach-Object {
        $relative = [System.IO.Path]::GetRelativePath($stagingPath, $_.FullName).Replace("\", "/")
        $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName).Hash
        "$hash  $relative"
    }
[System.IO.File]::WriteAllLines(
    (Join-Path $stagingPath "SHA256SUMS.txt"),
    [string[]]$manifestLines,
    [System.Text.UTF8Encoding]::new($false)
)

$pythonLauncher = Get-Command py -ErrorAction SilentlyContinue
$pythonArguments = @("-3")
if (-not $pythonLauncher) {
    $pythonLauncher = Get-Command python -ErrorAction SilentlyContinue
    $pythonArguments = @()
}
if (-not $pythonLauncher) {
    throw "创建保留 Linux 权限的 ZIP 需要 Python 3"
}
& $pythonLauncher.Source @pythonArguments "tools\create_release_zip.py" `
    --source $stagingPath `
    --target $temporaryArchive
if ($LASTEXITCODE -ne 0) {
    throw "保留 Linux 权限的 ZIP 创建失败，退出码：$LASTEXITCODE"
}

Add-Type -AssemblyName System.IO.Compression.FileSystem
$zip = [System.IO.Compression.ZipFile]::OpenRead($temporaryArchive)
try {
    $entryNames = @($zip.Entries | ForEach-Object { $_.FullName.Replace("\", "/") })
    foreach ($expected in @(
        "screen/casescreen-edge",
        "screen/casescreen-acrcloud-fingerprint",
        "server/casescreen-recognition-relay",
        "deploy/waveshare-usb-bridge/configure_raspberry_pi.sh",
        "firmware/serial_uart_casescreen.sh",
        "SHA256SUMS.txt"
    )) {
        if ($expected -notin $entryNames) {
            throw "压缩包缺少必须文件：$expected"
        }
    }
    foreach ($executable in @(
        "screen/casescreen-edge",
        "screen/casescreen-acrcloud-fingerprint",
        "server/casescreen-recognition-relay",
        "deploy/waveshare-usb-bridge/configure_raspberry_pi.sh",
        "firmware/serial_uart_casescreen.sh"
    )) {
        $entry = $zip.GetEntry($executable)
        $unixMode = (($entry.ExternalAttributes -shr 16) -band 0x1FF)
        if ($unixMode -ne 0x1ED) {
            throw "ZIP 中 Linux 可执行权限不是 0755：$executable（$unixMode）"
        }
    }
}
finally {
    $zip.Dispose()
}

if (Test-Path -LiteralPath $outputPath) {
    if ((Get-Item -LiteralPath $outputPath).Attributes -band [System.IO.FileAttributes]::ReparsePoint) {
        throw "拒绝替换重解析点发行目录：$outputPath"
    }
    Move-Item -LiteralPath $outputPath -Destination $previousPath
}
if (Test-Path -LiteralPath $archivePath) {
    if ((Get-Item -LiteralPath $archivePath).Attributes -band [System.IO.FileAttributes]::ReparsePoint) {
        throw "拒绝替换重解析点发行 ZIP：$archivePath"
    }
    Move-Item -LiteralPath $archivePath -Destination $previousArchive
}

Move-Item -LiteralPath $stagingPath -Destination $outputPath
$newOutputCommitted = $true
Move-Item -LiteralPath $temporaryArchive -Destination $archivePath
$newArchiveCommitted = $true
$newOutputCommitted = $false
$newArchiveCommitted = $false
foreach ($previous in @($previousPath, $previousArchive)) {
    if (Test-Path -LiteralPath $previous) {
        Remove-Item -LiteralPath $previous -Recurse -Force -ErrorAction SilentlyContinue
    }
}

$archive = Get-Item -LiteralPath $archivePath
$archiveHash = Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath
[PSCustomObject]@{
    Version = $Version
    Archive = $archive.FullName
    Length = $archive.Length
    SHA256 = $archiveHash.Hash
} | Format-List
