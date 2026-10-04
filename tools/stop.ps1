# OAIprism 一键停止：结束监听 8787（或 -Port 指定端口）的网关进程。
param([int]$Port = 8787)
$c = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
if (-not $c) {
    Write-Host "端口 $Port 上没有网关在运行"
    exit 0
}
Stop-Process -Id $c.OwningProcess -Force
Write-Host "已停止网关（PID $($c.OwningProcess)）"
