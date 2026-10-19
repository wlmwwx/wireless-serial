<#
.SYNOPSIS
    在 Windows 上安装 com0com 并创建一对虚拟串口（默认 COM11 <-> COM12），
    供 wserial 与用户串口程序（Arduino IDE / PuTTY / 串口助手等）配对使用。

.DESCRIPTION
    工作流程：
      1. 检查管理员权限（安装驱动必须提升权限）。
      2. 若 com0com 未安装：从 SourceForge 下载官方 *signed* 发行包并静默安装。
      3. 用 setupc.exe 创建端口对（默认 COM11 <-> COM12），已存在则跳过。
      4. 打印 setupc list 结果与后续 wserial 用法。

    用法（以管理员身份打开 PowerShell 后执行）：
      powershell -ExecutionPolicy Bypass -File .\scripts\install-com0com.ps1
      powershell -ExecutionPolicy Bypass -File .\scripts\install-com0com.ps1 -PortA COM20 -PortB COM21
      powershell -ExecutionPolicy Bypass -File .\scripts\install-com0com.ps1 -SkipInstall   # 仅建端口对
      powershell -ExecutionPolicy Bypass -File .\scripts\install-com0com.ps1 -ListOnly      # 只查看现有端口对

.PARAMETER PortA
    用户程序打开的一侧端口，默认 COM11。

.PARAMETER PortB
    wserial 打开的一侧端口，默认 COM12。

.PARAMETER DownloadDir
    com0com 安装包缓存目录，默认 $env:TEMP\com0com。

.PARAMETER SkipInstall
    跳过下载/安装，只要求已安装的 com0com，并创建端口对。

.PARAMETER ListOnly
    只列出当前端口对，不做任何修改。

.NOTES
    com0com 旧版（2.x）驱动在 Windows 10/11 上可能因驱动签名策略无法加载，
    请使用 3.0.0.0 的 fre-signed 包；仍失败时脚本会打印“禁用驱动签名强制”的步骤。
#>

[CmdletBinding()]
param(
    [string]$PortA = "COM11",
    [string]$PortB = "COM12",
    [string]$DownloadDir = (Join-Path $env:TEMP "com0com"),
    [switch]$SkipInstall,
    [switch]$ListOnly
)

$ErrorActionPreference = "Stop"

function Write-Step([string]$Message) {
    Write-Host "==> $Message" -ForegroundColor Cyan
}

function Write-Ok([string]$Message) {
    Write-Host "[OK] $Message" -ForegroundColor Green
}

function Write-Warn([string]$Message) {
    Write-Host "[警告] $Message" -ForegroundColor Yellow
}

function Write-Err([string]$Message) {
    Write-Host "[错误] $Message" -ForegroundColor Red
}

function Test-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Find-Setupc {
    $candidates = @(
        (Join-Path ${env:ProgramFiles(x86)} "com0com\setupc.exe"),
        (Join-Path $env:ProgramFiles "com0com\setupc.exe"),
        (Join-Path $DownloadDir "com0com\setupc.exe"),
        (Join-Path $DownloadDir "setupc.exe")
    )
    foreach ($path in $candidates) {
        if ($path -and (Test-Path $path)) {
            return (Resolve-Path $path).Path
        }
    }
    return $null
}

function Get-Com0comPackageUrl {
    $arch = $env:PROCESSOR_ARCHITECTURE
    $base = "https://sourceforge.net/projects/com0com/files/com0com/3.0.0.0"
    switch ($arch) {
        "ARM64" { return "$base/com0com-3.0.0.0-arm64-fre-signed.zip/download" }
        "AMD64" { return "$base/com0com-3.0.0.0-x64-fre-signed.zip/download" }
        default {
            if ([Environment]::Is64BitOperatingSystem) {
                return "$base/com0com-3.0.0.0-x64-fre-signed.zip/download"
            }
            return "$base/com0com-3.0.0.0-i386-fre-signed.zip/download"
        }
    }
}

function Install-Com0com {
    if ($SkipInstall) {
        Write-Err "指定了 -SkipInstall，但系统未找到 setupc.exe。请先手动安装 com0com 3.0.0.0。"
        return $null
    }

    $url = Get-Com0comPackageUrl
    if (-not (Test-Path $DownloadDir)) {
        New-Item -ItemType Directory -Path $DownloadDir -Force | Out-Null
    }
    $zip = Join-Path $DownloadDir "com0com-signed.zip"

    Write-Step "下载 com0com 3.0.0.0 (signed)"
    Write-Host "    $url"
    try {
        # SourceForge 的 /download 会 302 到镜像；Invoke-WebRequest 默认跟随跳转。
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        Invoke-WebRequest -Uri $url -OutFile $zip -UseBasicParsing
    } catch {
        Write-Err "下载失败：$($_.Exception.Message)"
        Write-Host "请手动下载后放到 $zip，或在浏览器中打开："
        Write-Host "    https://sourceforge.net/projects/com0com/files/com0com/3.0.0.0/"
        return $null
    }

    Write-Step "解压到 $DownloadDir"
    try {
        Expand-Archive -Path $zip -DestinationPath $DownloadDir -Force
    } catch {
        Write-Err "解压失败：$($_.Exception.Message)"
        return $null
    }

    $setupExe = Get-ChildItem -Path $DownloadDir -Filter "setup.exe" -Recurse -ErrorAction SilentlyContinue |
                Select-Object -First 1
    if (-not $setupExe) {
        Write-Err "解压后未找到 setup.exe，请检查下载包是否完整。"
        return $null
    }

    Write-Step "静默安装 com0com（可能需要几十秒）"
    $proc = Start-Process -FilePath $setupExe.FullName -ArgumentList "/S" -Wait -PassThru
    if ($proc.ExitCode -ne 0) {
        Write-Warn "安装程序退出码 $($proc.ExitCode)；继续尝试查找 setupc.exe。"
    }

    $setupc = Find-Setupc
    if (-not $setupc) {
        Write-Err "安装后仍未找到 setupc.exe。请重启后重试，或手动安装 com0com。"
    }
    return $setupc
}

function Get-PortPairs([string]$Setupc) {
    try {
        $output = & $Setupc list 2>&1 | Out-String
    } catch {
        return @{ Raw = ""; Error = $_.Exception.Message }
    }
    return @{ Raw = $output; Error = $null }
}

function Show-DriverSignatureHelp {
    Write-Host ""
    Write-Warn "如果 setupc 报错、设备管理器里出现黄色感叹号，通常是驱动签名被拦截："
    Write-Host "  1. 确认安装的是 3.0.0.0 的 *signed* 包（本脚本默认下载 signed 包）。"
    Write-Host "  2. 卸载旧版 com0com 后重启，再重新运行本脚本。"
    Write-Host "  3. 仍失败时，可临时禁用驱动签名强制（有安全风险，测试机专用）："
    Write-Host "       设置 -> 系统 -> 恢复 -> 高级启动 -> 立即重新启动"
    Write-Host "       -> 疑难解答 -> 高级选项 -> 启动设置 -> 重启 -> 按 7（禁用驱动程序强制签名）"
    Write-Host "    注意：该设置只在本次启动有效，重启后恢复。"
    Write-Host "  4. Windows 11 24H2+ / ARM64 请优先使用签名版；也可改用 wserial 的 Linux/macOS 方案。"
    Write-Host ""
}

# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------
Write-Host ""
Write-Host "wireless-serial / com0com 安装助手" -ForegroundColor White
Write-Host "-----------------------------------"

if (-not (Test-Administrator)) {
    Write-Err "请以管理员身份运行 PowerShell（右键 -> 以管理员身份运行），然后重试。"
    exit 1
}

$setupc = Find-Setupc
if (-not $setupc) {
    Write-Step "未检测到 com0com，开始安装"
    $setupc = Install-Com0com
}
if (-not $setupc) {
    Show-DriverSignatureHelp
    Write-Err "com0com 安装未完成。"
    exit 1
}
Write-Ok "setupc.exe: $setupc"

$pairs = Get-PortPairs -Setupc $setupc
if ($pairs.Error) {
    Write-Warn "执行 setupc list 失败：$($pairs.Error)"
} else {
    Write-Step "当前端口对（setupc list）"
    Write-Host $pairs.Raw
}

if ($ListOnly) {
    Write-Step "仅列出（-ListOnly），未做修改。"
    exit 0
}

# 已存在同名端口对则跳过创建
$pairExists = $false
if ($pairs.Raw -match [regex]::Escape($PortA) -and $pairs.Raw -match [regex]::Escape($PortB)) {
    $pairExists = $true
}

if ($pairExists) {
    Write-Ok "端口对 $PortA <-> $PortB 已存在，跳过创建。"
} else {
    Write-Step "创建端口对 $PortA <-> $PortB"
    # com0com 的 setupc 语法：setupc install PortName=COM11 PortName=COM12
    try {
        & $setupc install "PortName=$PortA" "PortName=$PortB"
        if ($LASTEXITCODE -ne 0) {
            throw "setupc install 退出码 $LASTEXITCODE"
        }
    } catch {
        Write-Err "创建端口对失败：$($_.Exception.Message)"
        Write-Host "可手动执行（管理员 PowerShell）："
        Write-Host "    & '$setupc' install PortName=$PortA PortName=$PortB"
        Show-DriverSignatureHelp
        exit 1
    }

    $pairs = Get-PortPairs -Setupc $setupc
    if ($pairs.Raw -match [regex]::Escape($PortA) -and $pairs.Raw -match [regex]::Escape($PortB)) {
        Write-Ok "端口对创建成功：$PortA <-> $PortB"
    } else {
        Write-Warn "未在 setupc list 中确认到新端口对，请手动核对："
        Write-Host $pairs.Raw
    }
}

Write-Host ""
Write-Host "后续步骤" -ForegroundColor White
Write-Host "--------"
Write-Host "  1. 确认设备管理器 -> 端口(COM 和 LPT) 中存在 $PortA 与 $PortB。"
Write-Host "  2. 让 wserial 打开其中一侧（通常是 $PortB）："
Write-Host "       wserial run --host <设备IP> --device $PortB -b 115200 -v"
Write-Host "     IP 可从路由器、设备串口日志或 mDNS 名 wserial-XXXXXX.local 获得；"
Write-Host "     也可先运行: wserial list"
Write-Host "  3. 用户程序（Arduino IDE / PuTTY / 串口助手）打开另一侧 $PortA，波特率一致即可。"
Write-Host "  4. 两侧都打开后数据自动互通；关闭 wserial 时 $PortB 会被释放，但端口对会保留。"
Write-Host ""
Write-Host "如需修改端口号，重新运行并指定 -PortA / -PortB；删除端口对可用："
Write-Host "    & '$setupc' remove <端口对编号>    # 编号见 setupc list 输出的 [CNCAx, CNCBx]"
Write-Host ""
exit 0
