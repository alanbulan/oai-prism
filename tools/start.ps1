# ============================================================
# OAIprism 一键启动（PowerShell）
#
# 只有一个进程：8787 网关（oaiprism serve）。出站直连 prism.openai.com，
# 内置 Chrome TLS 指纹，Sentinel token 纯 Go 签发 —— 不需要 Node.js / Chrome。
#
# 用法：tools\start.ps1            # 已在运行则跳过
#       tools\start.ps1 -Rebuild   # 先从源码重新构建（会先停掉正在运行的网关）
# ============================================================
param([switch]$Rebuild, [int]$Port = 8787)
$ErrorActionPreference = "Stop"

$Repo = Split-Path -Parent $PSScriptRoot
$Exe = Join-Path $Repo "oaiprism.exe"
$Config = Join-Path $Repo "configs\config.yaml"
$Log = Join-Path $env:TEMP "oaiprism_$Port.log"
$ErrLog = Join-Path $env:TEMP "oaiprism_$Port.err.log"

function Get-Listener([int]$P) {
    Get-NetTCPConnection -LocalPort $P -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
}

$running = Get-Listener $Port
if ($running -and -not $Rebuild) {
    Write-Host "网关已在运行（端口 $Port，PID $($running.OwningProcess)），跳过"
    exit 0
}

if ($Rebuild -or -not (Test-Path $Exe)) {
    Write-Host "构建 oaiprism.exe ..."
    Push-Location $Repo
    try { go build -o "$Exe.new" ./cmd/oaiprism } finally { Pop-Location }
    if ($LASTEXITCODE -ne 0) { throw "构建失败" }
    if ($running) {
        Write-Host "停止旧网关（PID $($running.OwningProcess)）..."
        Stop-Process -Id $running.OwningProcess -Force
        Start-Sleep -Seconds 1
    }
    Move-Item -Force "$Exe.new" $Exe
}

if (-not (Test-Path $Config)) {
    Copy-Item (Join-Path $Repo "configs\config.example.yaml") $Config
    Write-Host "已从 config.example.yaml 生成 configs\config.yaml"
}

Write-Host "启动网关（日志：$Log）..."
# 用 Win32_Process.Create 而不是 Start-Process -Redirect*：后者让网关继承本脚本的标准输出句柄，
# 调用方若把脚本输出接到管道里，会一直等到网关退出才返回。
$cmdline = "cmd.exe /c `"`"$Exe`" serve -config `"$Config`" -port $Port > `"$Log`" 2> `"$ErrLog`"`""
$hidden = New-CimInstance -ClassName Win32_ProcessStartup -ClientOnly -Property @{ ShowWindow = [uint16]0 }
$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{
    CommandLine = $cmdline; CurrentDirectory = $Repo; ProcessStartupInformation = $hidden
}
if ($r.ReturnValue -ne 0) { throw "启动失败（Win32_Process.Create 返回 $($r.ReturnValue)）" }

for ($i = 0; $i -lt 20; $i++) {
    Start-Sleep -Milliseconds 500
    try {
        $r = Invoke-WebRequest -Uri "http://127.0.0.1:$Port/healthz" -TimeoutSec 2 -UseBasicParsing
        if ($r.StatusCode -eq 200) {
            Write-Host "就绪：http://127.0.0.1:$Port/dashboard/"
            exit 0
        }
    } catch {}
}
Write-Host "网关未在 10 秒内就绪，查看日志：$Log / $ErrLog"
exit 1
