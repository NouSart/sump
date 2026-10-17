# SUMP v2 本地开发：一键启动服务（settings-center + memory + embedding + images + files + qq + shell + approval + timer + reasoner + frontend）
# 用法：.\scripts\run-local.ps1 [-CenterAddr 127.0.0.1:9000] [-MemoryAddr 127.0.0.1:9201] [-EmbeddingAddr 127.0.0.1:9601] [-ImageAddr 127.0.0.1:9401] [-FilesAddr 127.0.0.1:9802] [-QQAddr 127.0.0.1:9301] [-ShellAddr 127.0.0.1:9701] [-TimerAddr 127.0.0.1:9801] [-ApprovalAddr 127.0.0.1:9901] [-ReasonerAddr 127.0.0.1:9101] [-FrontendAddr 127.0.0.1:9501]
#       [-NapCatURL ws://127.0.0.1:3001] [-NapCatToken xxx] [-Owner 主人QQ] [-LlmKey sk-...] [-LlmModel deepseek-chat]
# 停止：Ctrl+C（脚本会清理子进程）
# 注：qq 连不上 NapCat 会自动重试，不影响其他服务；不填 -Owner 则拒绝所有 QQ 私聊
# 注：shell 的工作目录根 = 仓库根；接入 approval 第一层，使用 PowerShell 默认有限规则。
# 注：frontend 需要 Node ≥ 22；首次运行会自动 npm install + npm run build（面板 http://127.0.0.1:9501/）
# 注：embedding 需要 Python ≥ 3.12；首次运行自动建 venv + 装依赖；模型缺席时以 fake 后端启动（语义召回不可用，控制台会显著警告）
param(
    [string]$CenterAddr = '127.0.0.1:9000',
    [string]$MemoryAddr = '127.0.0.1:9201',
    [string]$EmbeddingAddr = '127.0.0.1:9601',
    [string]$ImageAddr = '127.0.0.1:9401',
    [string]$FilesAddr = '127.0.0.1:9802',
    [string]$QQAddr = '127.0.0.1:9301',
    [string]$ShellAddr = '127.0.0.1:9701',
    [string]$ApprovalAddr = '127.0.0.1:9901',
    [string]$TimerAddr = '127.0.0.1:9801',
    [string]$ReasonerAddr = '127.0.0.1:9101',
    [string]$FrontendAddr = '127.0.0.1:9501',
    [string]$NapCatURL = 'ws://127.0.0.1:3001',
    [string]$NapCatToken = $env:SUMP_NAPCAT_TOKEN,
    [string]$Owner = '',
    [string]$LlmBase = 'https://api.deepseek.com',
    [string]$LlmKey = $env:DEEPSEEK_API_KEY,
    [string]$LlmModel = 'deepseek-chat'
)

$ErrorActionPreference = 'Stop'
$approvalRuntime = Get-Command pwsh -ErrorAction SilentlyContinue
if (-not $approvalRuntime) { $approvalRuntime = Get-Command powershell -ErrorAction SilentlyContinue }
if (-not $approvalRuntime) { throw '命令审批需要 PowerShell' }

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

Write-Host '[1/9] 构建 settings-center / memory / images / files / qq / shell / approval / timer / reasoner ...' -ForegroundColor Cyan
Push-Location $root
try {
    go build -o (Join-Path $binDir 'settings-center.exe') 'github.com/QAQ-awa-QAQ/sump/settings-center'
    if ($LASTEXITCODE -ne 0) { throw 'settings-center 构建失败' }
    go build -o (Join-Path $binDir 'memory.exe') 'github.com/QAQ-awa-QAQ/sump/memory'
    if ($LASTEXITCODE -ne 0) { throw 'memory 构建失败' }
    # 注：images 输出为 sump-images.exe——本机 McAfee 会把 "images.exe" 这个名字当作可疑样本拦截
    go build -o (Join-Path $binDir 'sump-images.exe') 'github.com/QAQ-awa-QAQ/sump/images'
    if ($LASTEXITCODE -ne 0) { throw 'images 构建失败' }
    # 同理输出为 sump-files.exe——避免杀软对通用文件名的误报
    go build -o (Join-Path $binDir 'sump-files.exe') 'github.com/QAQ-awa-QAQ/sump/files'
    if ($LASTEXITCODE -ne 0) { throw 'files 构建失败' }
    go build -o (Join-Path $binDir 'qq.exe') 'github.com/QAQ-awa-QAQ/sump/qq'
    if ($LASTEXITCODE -ne 0) { throw 'qq 构建失败' }
    go build -o (Join-Path $binDir 'shell.exe') 'github.com/QAQ-awa-QAQ/sump/shell'
    if ($LASTEXITCODE -ne 0) { throw 'shell 构建失败' }
    go build -o (Join-Path $binDir 'sump-approval.exe') 'github.com/QAQ-awa-QAQ/sump/approval'
    if ($LASTEXITCODE -ne 0) { throw 'approval 构建失败' }
    go build -o (Join-Path $binDir 'timer.exe') 'github.com/QAQ-awa-QAQ/sump/timer'
    if ($LASTEXITCODE -ne 0) { throw 'timer 构建失败' }
    go build -o (Join-Path $binDir 'reasoner.exe') 'github.com/QAQ-awa-QAQ/sump/reasoner'
    if ($LASTEXITCODE -ne 0) { throw 'reasoner 构建失败' }
} finally {
    Pop-Location
}

Write-Host '[2/9] 构建 frontend（首次会自动 npm install）...' -ForegroundColor Cyan
$frontendDir = Join-Path $root 'frontend'
if (-not (Test-Path (Join-Path $frontendDir 'node_modules'))) {
    Push-Location $frontendDir
    try {
        npm install --no-audit --no-fund
        if ($LASTEXITCODE -ne 0) { throw 'npm install 失败' }
    } finally {
        Pop-Location
    }
}
if (-not (Test-Path (Join-Path $frontendDir 'dist\server\index.js')) -or -not (Test-Path (Join-Path $frontendDir 'dist\web\index.html'))) {
    Push-Location $frontendDir
    try {
        npm run build
        if ($LASTEXITCODE -ne 0) { throw '前端构建失败' }
    } finally {
        Pop-Location
    }
} else {
    Write-Host '  已构建，跳过（重建：cd frontend; npm run build）' -ForegroundColor DarkGray
}

$dataDir = Join-Path $root 'data'

Write-Host '[3/9] 准备 embedding（venv + 依赖 + 模型检测）...' -ForegroundColor Cyan
$embeddingDir = Join-Path $root 'embedding'
$embVenv = Join-Path $embeddingDir '.venv'
$embPython = Join-Path $embVenv 'Scripts\python.exe'
if (-not (Test-Path $embPython)) {
    if (-not (Get-Command python -ErrorAction SilentlyContinue)) { throw 'embedding 需要 Python ≥ 3.12（未找到 python 命令）' }
    Write-Host '  创建 embedding 虚拟环境（首次）...' -ForegroundColor DarkGray
    python -m venv $embVenv
    if ($LASTEXITCODE -ne 0) { throw 'embedding venv 创建失败' }
}
& $embPython -c "import msgspec, websockets" 2>$null
if ($LASTEXITCODE -ne 0) {
    Write-Host '  安装 embedding 依赖（首次）...' -ForegroundColor DarkGray
    & $embPython -m pip install --disable-pip-version-check -q -e $embeddingDir
    if ($LASTEXITCODE -ne 0) { throw 'embedding 依赖安装失败' }
}
$modelDir = Join-Path $dataDir 'models\bge-small-zh-v1.5'
$modelFile = Get-ChildItem $modelDir -Filter '*.onnx' -ErrorAction SilentlyContinue | Select-Object -First 1
$embeddingArgs = @('-addr', $EmbeddingAddr, '-center', "ws://$CenterAddr/ws")
if ($null -ne $modelFile) {
    & $embPython -c "import onnxruntime, tokenizers, numpy" 2>$null
    if ($LASTEXITCODE -ne 0) {
        Write-Host '  安装 embedding onnx 依赖（首次）...' -ForegroundColor DarkGray
        & $embPython -m pip install --disable-pip-version-check -q -e "$embeddingDir[onnx]"
        if ($LASTEXITCODE -ne 0) { throw 'embedding onnx 依赖安装失败' }
    }
    $embeddingArgs += @('-backend', 'onnx', '-model-dir', $modelDir)
} else {
    Write-Host '  警告: 未找到嵌入模型（data/models/bge-small-zh-v1.5/*.onnx）——embedding 以 fake 后端启动：' -ForegroundColor Yellow
    Write-Host '        语义召回不可用（仅供联调）；放置模型后重启即启用真模型，见 embedding/README.md' -ForegroundColor Yellow
    $embeddingArgs += @('-backend', 'fake')
}

$center = $null
$mem    = $null
$emb    = $null
$img    = $null
$qq     = $null
$shellSvc = $null
$approvalSvc = $null
$timerSvc = $null
$reasoner = $null
$frontend = $null
try {
    Write-Host '[4/9] 启动 settings-center ...' -ForegroundColor Cyan
    $center = Start-Process -FilePath (Join-Path $binDir 'settings-center.exe') -ArgumentList '-addr', $CenterAddr -PassThru -NoNewWindow
    if (-not (Wait-Port $CenterAddr 60)) { throw 'settings-center 未在 60 秒内就绪' }

    New-Item -ItemType Directory -Force -Path $dataDir | Out-Null

    Write-Host '[5/9] 启动 memory + embedding ...' -ForegroundColor Cyan
    $mem = Start-Process -FilePath (Join-Path $binDir 'memory.exe') -ArgumentList '-addr', $MemoryAddr, '-center', "ws://$CenterAddr/ws", '-db', (Join-Path $dataDir 'memory.db') -PassThru -NoNewWindow
    if (-not (Wait-Port $MemoryAddr 60)) { throw 'memory 未在 60 秒内就绪' }
    $emb = Start-Process -FilePath $embPython -ArgumentList (@('-m', 'sump_embedding') + $embeddingArgs) -WorkingDirectory $root -PassThru -NoNewWindow
    if (-not (Wait-Port $EmbeddingAddr 60)) { throw 'embedding 未在 60 秒内就绪（首次启动需加载模型，或依赖缺失——见上方日志）' }

    Write-Host '[6/9] 启动 images + files + qq ...' -ForegroundColor Cyan
    $img = Start-Process -FilePath (Join-Path $binDir 'sump-images.exe') -ArgumentList '-addr', $ImageAddr, '-center', "ws://$CenterAddr/ws", '-db', (Join-Path $dataDir 'images.db'), '-dir', (Join-Path $dataDir 'images') -PassThru -NoNewWindow
    if (-not (Wait-Port $ImageAddr 60)) { throw 'images 未在 60 秒内就绪' }
    $fil = Start-Process -FilePath (Join-Path $binDir 'sump-files.exe') -ArgumentList '-addr', $FilesAddr, '-center', "ws://$CenterAddr/ws", '-db', (Join-Path $dataDir 'files.db'), '-dir', (Join-Path $dataDir 'files') -PassThru -NoNewWindow
    if (-not (Wait-Port $FilesAddr 60)) { throw 'files 未在 60 秒内就绪' }
    $qqArgs = @('-addr', $QQAddr, '-center', "ws://$CenterAddr/ws", '-napcat', $NapCatURL)
    if ($NapCatToken) { $qqArgs += @('-napcat-token', $NapCatToken) }
    if ($Owner) { $qqArgs += @('-owner', $Owner) }
    $qq = Start-Process -FilePath (Join-Path $binDir 'qq.exe') -ArgumentList $qqArgs -PassThru -NoNewWindow
    if (-not (Wait-Port $QQAddr 60)) { throw 'qq 未在 60 秒内就绪' }

    Write-Host '[7/9] 启动 approval + shell + timer ...' -ForegroundColor Cyan
    $approvalSvc = Start-Process -FilePath (Join-Path $binDir 'sump-approval.exe') -ArgumentList 'serve', '-addr', $ApprovalAddr, '-center', "ws://$CenterAddr/ws", '-portal', 'frontend' -PassThru -WindowStyle Hidden
    if (-not (Wait-Port $ApprovalAddr 60)) { throw 'approval 未在 60 秒内就绪' }
    $shellSvc = Start-Process -FilePath (Join-Path $binDir 'shell.exe') -ArgumentList '-addr', $ShellAddr, '-center', "ws://$CenterAddr/ws", '-root', ('"' + $root + '"'), '-approval', 'approval', '-shell', ('"' + $approvalRuntime.Source + '"') -PassThru -WindowStyle Hidden
    if (-not (Wait-Port $ShellAddr 60)) { throw 'shell 未在 60 秒内就绪' }
    # -portal frontend：把在计时器推给门户展示倒计时（留空 = 不推）。
    $timerSvc = Start-Process -FilePath (Join-Path $binDir 'timer.exe') -ArgumentList '-addr', $TimerAddr, '-center', "ws://$CenterAddr/ws", '-portal', 'frontend' -PassThru -NoNewWindow
    if (-not (Wait-Port $TimerAddr 60)) { throw 'timer 未在 60 秒内就绪' }

    Write-Host '[8/9] 启动 reasoner ...' -ForegroundColor Cyan
    $reasonerArgs = @('-addr', $ReasonerAddr, '-center', "ws://$CenterAddr/ws", '-llm-base', $LlmBase, '-llm-model', $LlmModel)
    if ($LlmKey) { $reasonerArgs += @('-llm-key', $LlmKey) }
    $reasoner = Start-Process -FilePath (Join-Path $binDir 'reasoner.exe') -ArgumentList $reasonerArgs -PassThru -NoNewWindow

    Write-Host '[9/9] 启动 frontend ...' -ForegroundColor Cyan
    $frontend = Start-Process -FilePath 'node' -ArgumentList 'dist/server/index.js', '-addr', $FrontendAddr, '-center', "ws://$CenterAddr/ws", '-approval', 'approval' -WorkingDirectory $frontendDir -PassThru -NoNewWindow
    if (-not (Wait-Port $FrontendAddr 60)) { throw 'frontend 未在 60 秒内就绪' }

    Write-Host ''
    Write-Host "settings-center: ws://$CenterAddr/ws  (PID $($center.Id))"
    Write-Host "memory:          ws://$MemoryAddr/ws  (PID $($mem.Id))  db: data/memory.db"
    Write-Host "embedding:       ws://$EmbeddingAddr/ws  (PID $($emb.Id))  后端: $(if ($null -ne $modelFile) { 'onnx（真模型）' } else { 'fake（无模型——语义召回不可用）' })"
    Write-Host "files:           ws://$FilesAddr/ws  (PID $($fil.Id))  db: data/files.db  dir: data/files"
    Write-Host "images:          ws://$ImageAddr/ws  (PID $($img.Id))  db: data/images.db  dir: data/images"
    Write-Host "qq:              ws://$QQAddr/ws  (PID $($qq.Id))  napcat: $NapCatURL"
    Write-Host "shell:           ws://$ShellAddr/ws  (PID $($shellSvc.Id))  root: $root  审批: approval"
    Write-Host "approval:        ws://$ApprovalAddr/ws  (PID $($approvalSvc.Id))  PowerShell 第一层"
    Write-Host "timer:           ws://$TimerAddr/ws  (PID $($timerSvc.Id))  门户: frontend"
    Write-Host "reasoner:       ws://$ReasonerAddr/ws  (PID $($reasoner.Id))"
    Write-Host "frontend:       http://$FrontendAddr/  (PID $($frontend.Id))  ← 设置面板" -ForegroundColor Green
    Write-Host '按 Ctrl+C 停止' -ForegroundColor Yellow

    Wait-Process -Id $center.Id, $mem.Id, $emb.Id, $img.Id, $fil.Id, $qq.Id, $shellSvc.Id, $approvalSvc.Id, $timerSvc.Id, $reasoner.Id, $frontend.Id
} finally {
    foreach ($proc in @($center, $mem, $emb, $img, $fil, $qq, $shellSvc, $approvalSvc, $timerSvc, $reasoner, $frontend)) {
        if ($null -ne $proc -and -not $proc.HasExited) {
            Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
        }
    }
}