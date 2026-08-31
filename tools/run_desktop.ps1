$ErrorActionPreference = "Stop"
$desktopRoot = if (Test-Path (Join-Path $PSScriptRoot "pyproject.toml")) { $PSScriptRoot } else { Split-Path -Parent $PSScriptRoot }
Set-Location $desktopRoot
if (-not (Get-Command py -ErrorAction SilentlyContinue)) {
    throw "未找到 Python 启动器，请安装 Python 3.11 或更高版本。"
}
$pythonVersion = $null
foreach ($candidate in @("3.11", "3.12", "3.13", "3.14", "3.15")) {
    & py ("-" + $candidate) -c "import sys; print(sys.version_info >= (3, 11))" 2>$null | Out-Null
    if ($LASTEXITCODE -eq 0) {
        $pythonVersion = $candidate
        break
    }
}
if (-not $pythonVersion) {
    throw "未找到 Python 3.11 或更高版本。"
}
& py ("-" + $pythonVersion) -m pip install -e .
& py ("-" + $pythonVersion) -m case_screen
