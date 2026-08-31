# CaseScreen Windows 图标与版本资源检查、嵌入函数。
# 本文件只定义函数，由 build_windows.ps1 在 staging 内的 EXE 上调用。

if (-not ("CaseScreen.NativeResources" -as [type])) {
    Add-Type -TypeDefinition @"
using System;
using System.IO;
using System.Runtime.InteropServices;
using System.Text;

namespace CaseScreen {
    public static class NativeResources {
        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        public static extern IntPtr BeginUpdateResource(string fileName, bool deleteExistingResources);

        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        public static extern bool UpdateResource(IntPtr update, IntPtr type, IntPtr name, ushort language, byte[] data, uint dataSize);

        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        public static extern bool EndUpdateResource(IntPtr update, bool discard);

        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        public static extern IntPtr LoadLibraryEx(string fileName, IntPtr file, uint flags);

        [DllImport("kernel32.dll", SetLastError = true)]
        public static extern bool FreeLibrary(IntPtr module);

        [DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        public static extern IntPtr LoadImage(IntPtr instance, IntPtr name, uint type, int width, int height, uint flags);

        [DllImport("user32.dll", SetLastError = true)]
        public static extern bool DestroyIcon(IntPtr icon);
    }

    public static class VersionResourceBuilder {
        private static void Align(BinaryWriter writer) {
            while ((writer.BaseStream.Position & 3) != 0) {
                writer.Write((byte)0);
            }
        }

        private static byte[] Block(string key, byte[] value, ushort valueLength, ushort type, params byte[][] children) {
            using (MemoryStream stream = new MemoryStream())
            using (BinaryWriter writer = new BinaryWriter(stream)) {
                writer.Write((ushort)0);
                writer.Write(valueLength);
                writer.Write(type);
                writer.Write(Encoding.Unicode.GetBytes(key + "\0"));
                Align(writer);
                if (value != null && value.Length != 0) {
                    writer.Write(value);
                }
                if (children != null) {
                    foreach (byte[] child in children) {
                        Align(writer);
                        writer.Write(child);
                    }
                }
                writer.Flush();
                byte[] result = stream.ToArray();
                if (result.Length > ushort.MaxValue) {
                    throw new InvalidDataException("VERSIONINFO resource exceeds 65535 bytes");
                }
                byte[] length = BitConverter.GetBytes((ushort)result.Length);
                result[0] = length[0];
                result[1] = length[1];
                return result;
            }
        }

        private static byte[] StringValue(string key, string value) {
            string terminated = value + "\0";
            return Block(key, Encoding.Unicode.GetBytes(terminated), (ushort)terminated.Length, 1);
        }

        private static byte[] FixedFileInfo(ushort major, ushort minor, ushort build, ushort revision) {
            using (MemoryStream stream = new MemoryStream())
            using (BinaryWriter writer = new BinaryWriter(stream)) {
                uint versionMS = ((uint)major << 16) | minor;
                uint versionLS = ((uint)build << 16) | revision;
                writer.Write(0xFEEF04BDu);
                writer.Write(0x00010000u);
                writer.Write(versionMS);
                writer.Write(versionLS);
                writer.Write(versionMS);
                writer.Write(versionLS);
                writer.Write(0x0000003Fu);
                writer.Write(0u);
                writer.Write(0x00040004u);
                writer.Write(1u);
                writer.Write(0u);
                writer.Write(0u);
                writer.Write(0u);
                writer.Flush();
                return stream.ToArray();
            }
        }

        public static byte[] Build(
            ushort major,
            ushort minor,
            ushort build,
            ushort revision,
            string version,
            string productName,
            string fileDescription,
            string originalFilename
        ) {
            byte[][] strings = new byte[][] {
                StringValue("FileDescription", fileDescription),
                StringValue("FileVersion", version),
                StringValue("InternalName", Path.GetFileNameWithoutExtension(originalFilename)),
                StringValue("OriginalFilename", originalFilename),
                StringValue("ProductName", productName),
                StringValue("ProductVersion", version)
            };
            byte[] stringTable = Block("040904B0", null, 0, 1, strings);
            byte[] stringFileInfo = Block("StringFileInfo", null, 0, 1, stringTable);
            byte[] translation = new byte[] { 0x09, 0x04, 0xB0, 0x04 };
            byte[] translationBlock = Block("Translation", translation, 4, 0);
            byte[] varFileInfo = Block("VarFileInfo", null, 0, 1, translationBlock);
            byte[] fixedInfo = FixedFileInfo(major, minor, build, revision);
            return Block("VS_VERSION_INFO", fixedInfo, (ushort)fixedInfo.Length, 0, stringFileInfo, varFileInfo);
        }
    }
}
"@
}

function Throw-CaseScreenWin32Error {
    param([Parameter(Mandatory = $true)][string]$Operation)

    $errorCode = [Runtime.InteropServices.Marshal]::GetLastWin32Error()
    $detail = [ComponentModel.Win32Exception]::new($errorCode).Message
    throw "${Operation}失败（Win32 $errorCode）：$detail"
}

function Get-CaseScreenIconEntries {
    param([Parameter(Mandatory = $true)][string]$Path)

    $bytes = [System.IO.File]::ReadAllBytes($Path)
    if ($bytes.Length -lt 6) {
        throw "ICO 文件过短：$Path"
    }
    $reserved = [BitConverter]::ToUInt16($bytes, 0)
    $kind = [BitConverter]::ToUInt16($bytes, 2)
    $count = [BitConverter]::ToUInt16($bytes, 4)
    if ($reserved -ne 0 -or $kind -ne 1 -or $count -eq 0) {
        throw "ICO 头无效：$Path"
    }
    $directoryLength = 6 + 16 * [int]$count
    if ($bytes.Length -lt $directoryLength) {
        throw "ICO 目录被截断：$Path"
    }

    $entries = [Collections.Generic.List[object]]::new()
    for ($index = 0; $index -lt $count; $index++) {
        $offset = 6 + 16 * $index
        $widthByte = $bytes[$offset]
        $heightByte = $bytes[$offset + 1]
        $width = if ($widthByte -eq 0) { 256 } else { [int]$widthByte }
        $height = if ($heightByte -eq 0) { 256 } else { [int]$heightByte }
        $planes = [BitConverter]::ToUInt16($bytes, $offset + 4)
        $bitCount = [BitConverter]::ToUInt16($bytes, $offset + 6)
        $dataLength = [BitConverter]::ToUInt32($bytes, $offset + 8)
        $dataOffset = [BitConverter]::ToUInt32($bytes, $offset + 12)
        $dataEnd = [uint64]$dataOffset + [uint64]$dataLength
        if ($width -ne $height -or $dataLength -eq 0 -or $dataOffset -lt $directoryLength -or $dataEnd -gt $bytes.Length) {
            throw "ICO 第 $($index + 1) 个图像项无效：$Path"
        }
        $normalizedPlanes = if ($planes -eq 0) { 1 } else { $planes }
        $normalizedBitCount = if ($bitCount -eq 0) { 32 } else { $bitCount }
        $imageData = [byte[]]::new([int]$dataLength)
        [Array]::Copy($bytes, [int]$dataOffset, $imageData, 0, [int]$dataLength)
        $entries.Add([pscustomobject]@{
            WidthByte = [byte]$widthByte
            HeightByte = [byte]$heightByte
            Width = $width
            Height = $height
            ColorCount = [byte]$bytes[$offset + 2]
            Reserved = [byte]$bytes[$offset + 3]
            Planes = [uint16]$normalizedPlanes
            BitCount = [uint16]$normalizedBitCount
            DataLength = [uint32]$dataLength
            Data = $imageData
        })
    }
    return $entries.ToArray()
}

function Assert-CaseScreenIconAssets {
    param(
        [Parameter(Mandatory = $true)][string]$PNGPath,
        [Parameter(Mandatory = $true)][string]$ICOPath
    )

    foreach ($asset in @($PNGPath, $ICOPath)) {
        if (-not (Test-Path -LiteralPath $asset -PathType Leaf)) {
            throw "缺少应用图标素材：$asset"
        }
        $item = Get-Item -LiteralPath $asset -Force
        if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "应用图标素材不得是重解析点：$asset"
        }
    }

    $png = [System.IO.File]::ReadAllBytes($PNGPath)
    $signature = [byte[]](137, 80, 78, 71, 13, 10, 26, 10)
    if ($png.Length -lt 33) {
        throw "PNG 图标过短：$PNGPath"
    }
    for ($index = 0; $index -lt $signature.Length; $index++) {
        if ($png[$index] -ne $signature[$index]) {
            throw "PNG 图标签名无效：$PNGPath"
        }
    }
    $width = ([int]$png[16] -shl 24) -bor ([int]$png[17] -shl 16) -bor ([int]$png[18] -shl 8) -bor [int]$png[19]
    $height = ([int]$png[20] -shl 24) -bor ([int]$png[21] -shl 16) -bor ([int]$png[22] -shl 8) -bor [int]$png[23]
    if ($width -ne 256 -or $height -ne 256 -or $png[24] -ne 8 -or $png[25] -ne 6) {
        throw "托盘 PNG 必须是 256×256、8 位 RGBA：$PNGPath"
    }

    $entries = @(Get-CaseScreenIconEntries -Path $ICOPath)
    $sizes = @($entries | ForEach-Object { $_.Width })
    foreach ($requiredSize in @(16, 20, 24, 32, 40, 48, 64, 128, 256)) {
        if ($sizes -notcontains $requiredSize) {
            throw "ICO 缺少 $requiredSize×$requiredSize 图层：$ICOPath"
        }
    }
}

function New-CaseScreenGroupIconData {
    param([Parameter(Mandatory = $true)][object[]]$Entries)

    $stream = [System.IO.MemoryStream]::new()
    $writer = [System.IO.BinaryWriter]::new($stream)
    try {
        $writer.Write([uint16]0)
        $writer.Write([uint16]1)
        $writer.Write([uint16]$Entries.Count)
        for ($index = 0; $index -lt $Entries.Count; $index++) {
            $entry = $Entries[$index]
            $writer.Write([byte]$entry.WidthByte)
            $writer.Write([byte]$entry.HeightByte)
            $writer.Write([byte]$entry.ColorCount)
            $writer.Write([byte]$entry.Reserved)
            $writer.Write([uint16]$entry.Planes)
            $writer.Write([uint16]$entry.BitCount)
            $writer.Write([uint32]$entry.DataLength)
            $writer.Write([uint16]($index + 1))
        }
        $writer.Flush()
        return $stream.ToArray()
    }
    finally {
        $writer.Dispose()
        $stream.Dispose()
    }
}

function Set-CaseScreenExecutableIcon {
    param(
        [Parameter(Mandatory = $true)][string]$ExecutablePath,
        [Parameter(Mandatory = $true)][string]$ICOPath
    )

    $entries = @(Get-CaseScreenIconEntries -Path $ICOPath)
    $groupData = New-CaseScreenGroupIconData -Entries $entries
    $update = [CaseScreen.NativeResources]::BeginUpdateResource($ExecutablePath, $false)
    if ($update -eq [IntPtr]::Zero) {
        Throw-CaseScreenWin32Error -Operation "打开 EXE 资源"
    }
    $ended = $false
    try {
        for ($index = 0; $index -lt $entries.Count; $index++) {
            $entry = $entries[$index]
            # Windows PowerShell 5.1 不支持这里使用的多行静态方法参数写法。
            $ok = [CaseScreen.NativeResources]::UpdateResource($update, [IntPtr]3, [IntPtr]($index + 1), 0, $entry.Data, $entry.DataLength)
            if (-not $ok) {
                Throw-CaseScreenWin32Error -Operation "写入 EXE RT_ICON"
            }
        }
        # Gio v0.10.2 优先读取 IDI_APPLICATION（32512），Windows 也将它用于文件、任务栏和标题栏。
        $ok = [CaseScreen.NativeResources]::UpdateResource($update, [IntPtr]14, [IntPtr]32512, 0, $groupData, [uint32]$groupData.Length)
        if (-not $ok) {
            Throw-CaseScreenWin32Error -Operation "写入 EXE RT_GROUP_ICON"
        }
        $ok = [CaseScreen.NativeResources]::EndUpdateResource($update, $false)
        $ended = $true
        if (-not $ok) {
            Throw-CaseScreenWin32Error -Operation "提交 EXE 图标资源"
        }
    }
    finally {
        if (-not $ended) {
            [void][CaseScreen.NativeResources]::EndUpdateResource($update, $true)
        }
    }
}

function Set-CaseScreenExecutableVersionInfo {
    param(
        [Parameter(Mandatory = $true)][string]$ExecutablePath,
        [Parameter(Mandatory = $true)][uint16]$Major,
        [Parameter(Mandatory = $true)][uint16]$Minor,
        [Parameter(Mandatory = $true)][uint16]$VersionBuild,
        [Parameter(Mandatory = $true)][uint16]$Revision,
        [Parameter(Mandatory = $true)][string]$Version,
        [Parameter(Mandatory = $true)][string]$ProductName,
        [Parameter(Mandatory = $true)][string]$FileDescription,
        [Parameter(Mandatory = $true)][string]$OriginalFilename
    )

    $versionData = [CaseScreen.VersionResourceBuilder]::Build($Major, $Minor, $VersionBuild, $Revision, $Version, $ProductName, $FileDescription, $OriginalFilename)
    $update = [CaseScreen.NativeResources]::BeginUpdateResource($ExecutablePath, $false)
    if ($update -eq [IntPtr]::Zero) {
        Throw-CaseScreenWin32Error -Operation "打开 EXE 版本资源"
    }
    $ended = $false
    try {
        # RT_VERSION=16、VS_VERSION_INFO=1，字符串表使用 en-US/Unicode 标准翻译项。
        $ok = [CaseScreen.NativeResources]::UpdateResource($update, [IntPtr]16, [IntPtr]1, [uint16]0x0409, $versionData, [uint32]$versionData.Length)
        if (-not $ok) {
            Throw-CaseScreenWin32Error -Operation "写入 EXE VS_VERSION_INFO"
        }
        $ok = [CaseScreen.NativeResources]::EndUpdateResource($update, $false)
        $ended = $true
        if (-not $ok) {
            Throw-CaseScreenWin32Error -Operation "提交 EXE 版本资源"
        }
    }
    finally {
        if (-not $ended) {
            [void][CaseScreen.NativeResources]::EndUpdateResource($update, $true)
        }
    }
}

function Assert-CaseScreenExecutableVersionInfo {
    param(
        [Parameter(Mandatory = $true)][string]$ExecutablePath,
        [Parameter(Mandatory = $true)][uint16]$Major,
        [Parameter(Mandatory = $true)][uint16]$Minor,
        [Parameter(Mandatory = $true)][uint16]$VersionBuild,
        [Parameter(Mandatory = $true)][uint16]$Revision,
        [Parameter(Mandatory = $true)][string]$Version,
        [Parameter(Mandatory = $true)][string]$ProductName,
        [Parameter(Mandatory = $true)][string]$FileDescription,
        [Parameter(Mandatory = $true)][string]$OriginalFilename
    )

    # FileVersionInfo 在 Windows 上通过 version.dll 读取刚写入的 VS_VERSION_INFO。
    $info = [Diagnostics.FileVersionInfo]::GetVersionInfo($ExecutablePath)
    $numericFileVersion = "{0}.{1}.{2}.{3}" -f $info.FileMajorPart, $info.FileMinorPart, $info.FileBuildPart, $info.FilePrivatePart
    $numericProductVersion = "{0}.{1}.{2}.{3}" -f $info.ProductMajorPart, $info.ProductMinorPart, $info.ProductBuildPart, $info.ProductPrivatePart
    $expectedNumericVersion = "{0}.{1}.{2}.{3}" -f $Major, $Minor, $VersionBuild, $Revision
    if ($numericFileVersion -cne $expectedNumericVersion -or $numericProductVersion -cne $expectedNumericVersion) {
        throw "EXE 数值版本资源错误：$ExecutablePath（File=$numericFileVersion，Product=$numericProductVersion）"
    }
    foreach ($field in @(
        @{ Name = "FileVersion"; Actual = $info.FileVersion; Expected = $Version }
        @{ Name = "ProductVersion"; Actual = $info.ProductVersion; Expected = $Version }
        @{ Name = "ProductName"; Actual = $info.ProductName; Expected = $ProductName }
        @{ Name = "FileDescription"; Actual = $info.FileDescription; Expected = $FileDescription }
        @{ Name = "OriginalFilename"; Actual = $info.OriginalFilename; Expected = $OriginalFilename }
    )) {
        if ($field.Actual -cne $field.Expected) {
            throw "EXE 版本字段 $($field.Name) 错误：$ExecutablePath（期望 '$($field.Expected)'，实际 '$($field.Actual)'）"
        }
    }
}

function Assert-CaseScreenExecutableIcon {
    param([Parameter(Mandatory = $true)][string]$ExecutablePath)

    $loadLibraryAsDataFile = [uint32]2
    $imageIcon = [uint32]1
    $module = [CaseScreen.NativeResources]::LoadLibraryEx($ExecutablePath, [IntPtr]::Zero, $loadLibraryAsDataFile)
    if ($module -eq [IntPtr]::Zero) {
        Throw-CaseScreenWin32Error -Operation "加载 EXE 资源"
    }
    try {
        $icon = [CaseScreen.NativeResources]::LoadImage($module, [IntPtr]32512, $imageIcon, 32, 32, 0)
        if ($icon -eq [IntPtr]::Zero) {
            Throw-CaseScreenWin32Error -Operation "读取 EXE IDI_APPLICATION 图标"
        }
        [void][CaseScreen.NativeResources]::DestroyIcon($icon)
    }
    finally {
        [void][CaseScreen.NativeResources]::FreeLibrary($module)
    }
}
