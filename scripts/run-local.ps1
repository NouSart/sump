# SUMP v2 本地开发：一键启动服务（settings-center + memory + images + qq + reasoner）
# 用法：.\scripts\run-local.ps1 [-CenterAddr 127.0.0.1:9000] [-MemoryAddr 127.0.0.1:9201] [-ImageAddr 127.0.0.1:9401] [-QQAddr 127.0.0.1:9301] [-ReasonerAddr 127.0.0.1:9101]
#       [-NapCatURL ws://127.0.0.1:3001] [-NapCatToken xxx] [-Owner 主人QQ] [-LlmKey sk-...] [-LlmModel deepseek-chat]
# 停止：Ctrl+C（脚本会清理子进程）
# 注：qq 连不上 NapCat 会自动重试，不影响其他服务；不填 -Owner 则拒绝所有 QQ 私聊
param(
    [string]$CenterAddr = '127.0.0.1:9000',
    [string]$MemoryAddr = '127.0.0.1:9201',
    [string]$ImageAddr = '127.0.0.1:9401',
    [string]$QQAddr = '127.0.0.1:9301',
    [string]$ReasonerAddr = '127.0.0.1:9101',
    [string]$NapCatURL = 'ws://127.0.0.1:3001',
    [string]$NapCatToken = $env:SUMP_NAPCAT_TOKEN,
    [string]$Owner = '',
    [string]$LlmBase = 'https://api.deepseek.com',
    [string]$LlmKey = $env:DEEPSEEK_API_KEY,
    [string]$LlmModel = 'deepseek-chat'
)

$ErrorActionPreference = 'Stop'

$root   = Split-Path -Parent $PSScriptRoot
$binDir = Join-Path $env:TEMP 'sump-v2-bin'
New-Item -ItemType Directory -Force -Path $binDir | Out-Null

function Wait-Port([string]$Addr, [int]$TimeoutSec) {
    $h, $p = $Addr.Split(':')
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        $client = New-Object System.Net.Sockets.TcpClient
        try {
            $client.Connect($h, [int]$p)
            $client.Close()
            return $true
        } catch {
            Start-Sleep -Milliseconds 300
        }
    }
    return $false
}

Write-Host '[1/6] 构建 settings-center / memory / images / qq / reasoner ...' -ForegroundColor Cyan
Push-Location $root
try {
    go build -o (Join-Path $binDir 'settings-center.exe') 'github.com/QAQ-awa-QAQ/sump/settings-center'
    if ($LASTEXITCODE -ne 0) { throw 'settings-center 构建失败' }
    go build -o (Join-Path $binDir 'memory.exe') 'github.com/QAQ-awa-QAQ/sump/memory'
    if ($LASTEXITCODE -ne 0) { throw 'memory 构建失败' }
    # 注：images 输出为 sump-images.exe——本机 McAfee 会把 "images.exe" 这个名字当作可疑样本拦截
    go build -o (Join-Path $binDir 'sump-images.exe') 'github.com/QAQ-awa-QAQ/sump/images'
    if ($LASTEXITCODE -ne 0) { throw 'images 构建失败' }
    go build -o (Join-Path $binDir 'qq.exe') 'github.com/QAQ-awa-QAQ/sump/qq'
    if ($LASTEXITCODE -ne 0) { throw 'qq 构建失败' }
    go build -o (Join-Path $binDir 'reasoner.exe') 'github.com/QAQ-awa-QAQ/sump/reasoner'
    if ($LASTEXITCODE -ne 0) { throw 'reasoner 构建失败' }
} finally {
    Pop-Location
}

$center   = $null
$mem      = $null
$img      = $null
$qq       = $null
$reasoner = $null
try {
    Write-Host '[2/6] 启动 settings-center ...' -ForegroundColor Cyan
    $center = Start-Process -FilePath (Join-Path $binDir 'settings-center.exe') -ArgumentList '-addr', $CenterAddr -PassThru -NoNewWindow
    if (-not (Wait-Port $CenterAddr 60)) { throw 'settings-center 未在 60 秒内就绪' }

    $dataDir = Join-Path $root 'data'
    New-Item -ItemType Directory -Force -Path $dataDir | Out-Null

    Write-Host '[3/6] 启动 memory ...' -ForegroundColor Cyan
    $mem = Start-Process -FilePath (Join-Path $binDir 'memory.exe') -ArgumentList '-addr', $MemoryAddr, '-center', "ws://$CenterAddr/ws", '-db', (Join-Path $dataDir 'memory.db') -PassThru -NoNewWindow
    if (-not (Wait-Port $MemoryAddr 60)) { throw 'memory 未在 60 秒内就绪' }

    Write-Host '[4/6] 启动 images ...' -ForegroundColor Cyan
    $img = Start-Process -FilePath (Join-Path $binDir 'sump-images.exe') -ArgumentList '-addr', $ImageAddr, '-center', "ws://$CenterAddr/ws", '-db', (Join-Path $dataDir 'images.db'), '-dir', (Join-Path $dataDir 'images') -PassThru -NoNewWindow
    if (-not (Wait-Port $ImageAddr 60)) { throw 'images 未在 60 秒内就绪' }

    Write-Host '[5/6] 启动 qq ...' -ForegroundColor Cyan
    $qqArgs = @('-addr', $QQAddr, '-center', "ws://$CenterAddr/ws", '-napcat', $NapCatURL)
    if ($NapCatToken) { $qqArgs += @('-napcat-token', $NapCatToken) }
    if ($Owner) { $qqArgs += @('-owner', $Owner) }
    $qq = Start-Process -FilePath (Join-Path $binDir 'qq.exe') -ArgumentList $qqArgs -PassThru -NoNewWindow
    if (-not (Wait-Port $QQAddr 60)) { throw 'qq 未在 60 秒内就绪' }

    Write-Host '[6/6] 启动 reasoner ...' -ForegroundColor Cyan
    $reasonerArgs = @('-addr', $ReasonerAddr, '-center', "ws://$CenterAddr/ws", '-llm-base', $LlmBase, '-llm-model', $LlmModel)
    if ($LlmKey) { $reasonerArgs += @('-llm-key', $LlmKey) }
    $reasoner = Start-Process -FilePath (Join-Path $binDir 'reasoner.exe') -ArgumentList $reasonerArgs -PassThru -NoNewWindow

    Write-Host ''
    Write-Host "settings-center: ws://$CenterAddr/ws  (PID $($center.Id))"
    Write-Host "memory:          ws://$MemoryAddr/ws  (PID $($mem.Id))  db: data/memory.db"
    Write-Host "images:          ws://$ImageAddr/ws  (PID $($img.Id))  db: data/images.db  dir: data/images"
    Write-Host "qq:              ws://$QQAddr/ws  (PID $($qq.Id))  napcat: $NapCatURL"
    Write-Host "reasoner:       ws://$ReasonerAddr/ws  (PID $($reasoner.Id))"
    Write-Host '按 Ctrl+C 停止' -ForegroundColor Yellow

    Wait-Process -Id $center.Id, $mem.Id, $img.Id, $qq.Id, $reasoner.Id
} finally {
    foreach ($proc in @($center, $mem, $img, $qq, $reasoner)) {
        if ($null -ne $proc -and -not $proc.HasExited) {
            Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
        }
    }
}
