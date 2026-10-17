# serve.ps1 —— 上线运行（长期跑）。开发时用 run-local.ps1；这个脚本只负责"别停"：
#   · 把 11 个服务的全部输出落盘到 logs\（run-local 自己是前台脚本，日志只在控制台）
#   · run-local 退出（崩溃 / 误关 / 机器重启后重登）就按退避重拉
#   · 可选注册"登录时自动启动"的计划任务（当前用户，无需管理员）
#
# 用法：
#   .\scripts\serve.ps1                        # 前台守护（Ctrl+C 停止全部）
#   .\scripts\serve.ps1 -Once                  # 只跑一次，退出即止（排查用）
#   .\scripts\serve.ps1 -InstallTask           # 注册登录自启（之后每次登录自动跑）
#   .\scripts\serve.ps1 -UninstallTask         # 取消登录自启
param(
  [string]$LogDir = (Join-Path (Split-Path -Parent $PSScriptRoot) 'logs'),
  [int]$RestartDelaySec = 10,
  [switch]$Once,
  [switch]$InstallTask,
  [switch]$UninstallTask
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$taskName = 'SUMP-v2'
$self = $PSCommandPath

function Get-PowerShellPath {
  $exe = Join-Path $PSHOME 'powershell.exe'
  if (Test-Path $exe) { return $exe }
  return 'powershell.exe'
}

# 原生命令（schtasks）写 stderr 时，在 $ErrorActionPreference='Stop' 下会被当成异常终止脚本，
# 所以这两处调用前后临时放宽。
function Invoke-SchTasks([string]$argLine) {
  $prev = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  try {
    $global:LASTEXITCODE = 0
    cmd /c "schtasks $argLine 2>nul" | Out-Null
    return $LASTEXITCODE
  } finally {
    $ErrorActionPreference = $prev
  }
}

if ($UninstallTask) {
  $removed = $false
  if ((Invoke-SchTasks "/Delete /TN $taskName /F") -eq 0) {
    Write-Host "已取消登录自启（计划任务）：$taskName" -ForegroundColor Green
    $removed = $true
  }
  $cmdPath = Join-Path ([Environment]::GetFolderPath('Startup')) 'SUMP-v2.cmd'
  if (Test-Path $cmdPath) {
    Remove-Item $cmdPath -Force
    Write-Host "已取消登录自启（启动文件夹）：$cmdPath" -ForegroundColor Green
    $removed = $true
  }
  if (-not $removed) { Write-Host '没有找到任何自启项（计划任务与启动文件夹都没有）' -ForegroundColor Yellow }
  return
}

if ($InstallTask) {
  $ps = Get-PowerShellPath
  $args = "-NoProfile -ExecutionPolicy Bypass -File `"$self`""
  if ((Invoke-SchTasks "/Create /TN $taskName /TR `"`"$ps`" $args`" /SC ONLOGON /RL LIMITED /F") -eq 0) {
    Write-Host "已注册登录自启（计划任务）：$taskName（下次登录开始，日志在 $LogDir）" -ForegroundColor Green
    return
  }
  # 计划任务要管理员权限（ONLOGON 会被拒：Access is denied）。免管理员的回落：启动文件夹里放一个 .cmd。
  $startup = [Environment]::GetFolderPath('Startup')
  New-Item -ItemType Directory -Force -Path $startup | Out-Null
  $cmdPath = Join-Path $startup 'SUMP-v2.cmd'
  $body = "@echo off`r`nstart `"SUMP v2`" /min `"$ps`" -NoProfile -ExecutionPolicy Bypass -File `"$self`"`r`n"
  [IO.File]::WriteAllText($cmdPath, $body, (New-Object Text.UTF8Encoding($false)))
  Write-Host "已注册登录自启（启动文件夹）：$cmdPath" -ForegroundColor Green
  Write-Host '  （计划任务需要管理员权限，已自动改用启动文件夹——效果相同，无需提权）' -ForegroundColor DarkGray
  return
}

New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
$logFile = Join-Path $LogDir ("sump-{0}.log" -f (Get-Date -Format 'yyyyMMdd'))

function Write-Log([string]$text) {
  $line = "[{0}] {1}" -f (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'), $text
  Write-Host $line
  Add-Content -Path $logFile -Value $line -Encoding UTF8
}

Write-Log "上线守护启动；日志 $logFile"

$attempt = 0
while ($true) {
  $attempt++
  Write-Log "第 $attempt 次拉起 run-local.ps1（输出全部追加到日志）"
  $code = 0
  try {
    # run-local 自己会把子进程收干净；这里只等它退出。
    & (Join-Path $PSScriptRoot 'run-local.ps1') *>&1 | Tee-Object -FilePath $logFile -Append | Out-Host
    $code = $LASTEXITCODE
  } catch {
    Write-Log "run-local 抛出异常：$($_.Exception.Message)"
    $code = 1
  }
  if ($Once) {
    Write-Log "一次性模式：run-local 退出码 $code，守护结束"
    break
  }
  $wait = [Math]::Min(60, $RestartDelaySec * $attempt)
  Write-Log "run-local 退出（码 $code）；${wait}s 后重拉（Ctrl+C 可停止守护）"
  Start-Sleep -Seconds $wait
}