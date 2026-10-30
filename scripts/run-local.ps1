# SUMP v2 本地开发：一键启动服务（settings-center + memory + embedding + images + files + qq + shell + approval + batch + timer + mcp + reasoner + frontend）
# 用法：.\scripts\run-local.ps1 [-CenterAddr 127.0.0.1:9000] [-MemoryAddr 127.0.0.1:9201] [-EmbeddingAddr 127.0.0.1:9601] [-ImageAddr 127.0.0.1:9401] [-FilesAddr 127.0.0.1:9802] [-QQAddr 127.0.0.1:9301] [-ShellAddr 127.0.0.1:9701] [-TimerAddr 127.0.0.1:9801] [-ApprovalAddr 127.0.0.1:9901] [-BatchAddr 127.0.0.1:9951] [-MCPAddr 127.0.0.1:9902] [-MCPHTTPAddr 127.0.0.1:9903] [-ReasonerAddr 127.0.0.1:9101] [-FrontendAddr 127.0.0.1:9501]
#       [-NapCatURL ws://127.0.0.1:3000] [-NapCatToken <令牌>] [-Owner 主人QQ] [-LlmKey sk-...] [-LlmModel deepseek-flash]
# 停止：Ctrl+C（脚本会清理子进程）
# 注：qq 连不上 NapCat 会自动重试，不影响其他服务；不填 -Owner 则拒绝所有 QQ 私聊
# 注：shell 的工作目录根 = 仓库根；接入 approval 第一层。解释器不指定，交给自动探测（Windows 上 bash 优先），
#     bash 走只读白名单（不修改文件的命令直过），其余一律转人工；PowerShell 走 policy.json 规则。
# 注：mcp 初次启动无 token 时拒绝外部请求；在面板设置 http.token，并显式配置 data/mcp.json，见 mcp/README.md。
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
    [string]$BatchAddr = '127.0.0.1:9951',
    [string]$TimerAddr = '127.0.0.1:9801',
    [string]$MCPAddr = '127.0.0.1:9902',
    [string]$MCPHTTPAddr = '127.0.0.1:9903',
    [string]$ReasonerAddr = '127.0.0.1:9101',
    [string]$FrontendAddr = '127.0.0.1:9501',
    [string]$NapCatURL = 'ws://127.0.0.1:3000',
    [string]$NapCatToken = $env:SUMP_NAPCAT_TOKEN,
    [string]$Owner = '',
    [string]$LlmBase = 'https://api.deepseek.com',
    [string]$LlmKey = $env:DEEPSEEK_API_KEY,
    [string]$LlmModel = 'deepseek-flash'
)

$ErrorActionPreference = 'Stop'

$approvalRuntime = Get-Command pwsh -ErrorAction SilentlyContinue
if (-not $approvalRuntime) { $approvalRuntime = Get-Command powershell -ErrorAction SilentlyContinue }
if (-not $approvalRuntime) { throw '命令审批需要 PowerShell' }

$root   = Split-Path -Parent $PSScriptRoot
$binDir = Join-Path $env:TEMP 'sump-v2-bin'
New-Item -ItemType Directory -Force -Path $binDir | Out-Null

function Assert-PortFree([string]$Addr) {
    $h, $p = $Addr.Split(':')
    $busy = $false
    $client = New-Object System.Net.Sockets.TcpClient
    try { $client.Connect($h, [int]$p); $client.Close(); $busy = $true } catch { }
    if ($busy) {
        throw "端口 $Addr 已被占用：上一次的 SUMP 可能没关干净（常见残留：shell.exe / sump-approval.exe）。请先在任务管理器里结束它们，或改用别的端口，再启动。"
    }
}

# Wait-Port 等的是"我起的这个进程把端口打开"——只测端口能连是不够的：
# 上一轮残留的进程占着同一个端口时，新进程会绑定失败并立刻退出，而端口探测
# 仍然成功，脚本会误以为启动完成，最后卡在 Wait-Process 上把所有服务一起拖死。
function Wait-Port([string]$Addr, [int]$TimeoutSec, $Proc = $null) {
    $h, $p = $Addr.Split(':')
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        if ($null -ne $Proc -and $Proc.HasExited) { return $false }
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

foreach ($a in @($CenterAddr, $MemoryAddr, $EmbeddingAddr, $ImageAddr, $FilesAddr, $QQAddr, $ApprovalAddr, $BatchAddr, $ShellAddr, $TimerAddr, $MCPAddr, $MCPHTTPAddr, $ReasonerAddr, $FrontendAddr)) {
    Assert-PortFree $a
}

Write-Host '[1/9] 构建 settings-center / memory / images / files / qq / shell / approval / batch / timer / mcp / reasoner ...' -ForegroundColor Cyan
Push-Location $root
try {
    go build -o (Join-Path $binDir 'settings-center.exe') 'github.com/NouSart/sump/settings-center'
    if ($LASTEXITCODE -ne 0) { throw 'settings-center 构建失败' }
    go build -o (Join-Path $binDir 'memory.exe') 'github.com/NouSart/sump/memory'
    if ($LASTEXITCODE -ne 0) { throw 'memory 构建失败' }
    # 注：images 输出为 sump-images.exe——本机 McAfee 会把 "images.exe" 这个名字当作可疑样本拦截
    go build -o (Join-Path $binDir 'sump-images.exe') 'github.com/NouSart/sump/images'
    if ($LASTEXITCODE -ne 0) { throw 'images 构建失败' }
    # 同理输出为 sump-files.exe——避免杀软对通用文件名的误报
    go build -o (Join-Path $binDir 'sump-files.exe') 'github.com/NouSart/sump/files'
    if ($LASTEXITCODE -ne 0) { throw 'files 构建失败' }
    go build -o (Join-Path $binDir 'qq.exe') 'github.com/NouSart/sump/qq'
    if ($LASTEXITCODE -ne 0) { throw 'qq 构建失败' }
    go build -o (Join-Path $binDir 'shell.exe') 'github.com/NouSart/sump/shell'
    if ($LASTEXITCODE -ne 0) { throw 'shell 构建失败' }
    go build -o (Join-Path $binDir 'sump-approval.exe') 'github.com/NouSart/sump/approval'
    if ($LASTEXITCODE -ne 0) { throw 'approval 构建失败' }
    go build -o (Join-Path $binDir 'batch.exe') 'github.com/NouSart/sump/batch'
    if ($LASTEXITCODE -ne 0) { throw 'batch 构建失败' }
    go build -o (Join-Path $binDir 'timer.exe') 'github.com/NouSart/sump/timer'
    if ($LASTEXITCODE -ne 0) { throw 'timer 构建失败' }
    go build -o (Join-Path $binDir 'sump-mcp.exe') 'github.com/NouSart/sump/mcp'
    if ($LASTEXITCODE -ne 0) { throw 'mcp 构建失败' }
    go build -o (Join-Path $binDir 'reasoner.exe') 'github.com/NouSart/sump/reasoner'
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
# 前端要不要重建：交给 frontend/scripts/build-stamp.mjs 判定——它比对的是构建时记下的逐文件内容指纹
# （frontend/dist/.build-stamp），而不只是 mtime。原因：mtime 只能回答「dist 是不是比 src 旧」，
# 回答不了「上次构建之后 src 到底动没动过、动了哪几个」；而且时钟被改、git checkout 把 mtime 刷成
# 当前时间，mtime 都会说谎（旧 dist 反而显得更新，脚本就跳过构建，面板一直跑旧 bundle）。
# 退出码：0=无需重建  10=需要重建  11=没有指纹（退回 mtime 兜底）  2=判定失败
$frontendOut = Join-Path $frontendDir 'dist\web\index.html'
$frontendSrv = Join-Path $frontendDir 'dist\server\index.js'
$stampTool   = Join-Path $frontendDir 'scripts\build-stamp.mjs'
$rebuildWhy  = $null
$checkLines  = @()
$checkCode   = 2

# node 输出是 UTF-8：不先把控制台解码改过来，中文会变成乱码。
$prevEnc = $null
try { $prevEnc = [Console]::OutputEncoding; [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }
try {
    if (-not (Test-Path -LiteralPath $stampTool)) {
        $checkLines = @('找不到 frontend/scripts/build-stamp.mjs，无法体检')
    } elseif (-not (Get-Command node -ErrorAction SilentlyContinue)) {
        $checkLines = @('找不到 node，无法体检')
    } else {
        $checkLines = @(& node $stampTool check 2>&1 | ForEach-Object { [string]$_ })
        $checkCode  = $LASTEXITCODE
        if ($null -eq $checkCode) { $checkCode = 2 }
    }
} finally {
    if ($null -ne $prevEnc) { try { [Console]::OutputEncoding = $prevEnc } catch { } }
}

$concl = @($checkLines | Where-Object { $_ -match '^\s*结论' })
$detail = @()
if ($concl.Count -gt 0) {
    $ci = [array]::IndexOf($checkLines, $concl[0])
    if ($ci -lt ($checkLines.Count - 1)) {
        $detail = @($checkLines[($ci + 1)..([Math]::Min($ci + 4, $checkLines.Count - 1))] | Where-Object { $_ -and $_.Trim() })
    }
}

switch ($checkCode) {
    0 { }
    10 {
        $rebuildWhy = if ($concl.Count -gt 0) { $concl[0].Trim() } else { '有源码改动' }
    }
    11 {
        # 没有指纹（老 dist）：只能退回 mtime 兜底，并提示建一次基线
        if (-not (Test-Path -LiteralPath $frontendOut) -or -not (Test-Path -LiteralPath $frontendSrv)) {
            $rebuildWhy = 'dist 缺失'
        } else {
            $stamp = (Get-Item -LiteralPath $frontendOut).LastWriteTimeUtc
            $inputs = @(Get-ChildItem -LiteralPath (Join-Path $frontendDir 'src') -Recurse -File -ErrorAction SilentlyContinue)
            foreach ($f in @('package.json', 'index.html', 'vite.config.ts', 'vitest.config.ts', 'tsconfig.json', 'tsconfig.web.json', 'tsconfig.test.json')) {
                $fp = Join-Path $frontendDir $f
                if (Test-Path -LiteralPath $fp) { $inputs += Get-Item -LiteralPath $fp }
            }
            $newest = $inputs | Sort-Object LastWriteTimeUtc -Descending | Select-Object -First 1
            if ($null -ne $newest -and $newest.LastWriteTimeUtc -gt $stamp) {
                $rel = $newest.FullName.Substring($frontendDir.Length).TrimStart('\')
                $rebuildWhy = ('源码比 dist 新：{0}（{1}）> dist（{2}）' -f $rel,
                    $newest.LastWriteTime.ToString('MM-dd HH:mm'),
                    $stamp.ToLocalTime().ToString('MM-dd HH:mm'))
            }
            Write-Host '  提示：没有构建指纹，只能退回 mtime 判断（跑一次 npm run build 就会自动写指纹）' -ForegroundColor DarkGray
        }
    }
    default { $rebuildWhy = '体检判定失败（按需要重建处理）' }
}

if ($null -ne $rebuildWhy) {
    Write-Host ('  重建前端：{0}' -f $rebuildWhy) -ForegroundColor DarkGray
    foreach ($l in $detail) { Write-Host ('  ' + $l.Trim()) -ForegroundColor DarkGray }
    Push-Location $frontendDir
    try {
        npm run build
        if ($LASTEXITCODE -ne 0) { throw '前端构建失败' }
        # npm run build 的最后一步已经写过指纹；这里兜底再写一次，防止有人改了 build 脚本漏掉
        if (Test-Path -LiteralPath $stampTool) { & node $stampTool write | Out-Null }
    } finally {
        Pop-Location
    }
} else {
    $note = if ($concl.Count -gt 0) { $concl[0].Trim() } else { 'build-stamp 判定无需重建' }
    Write-Host ('  跳过：{0}' -f $note) -ForegroundColor DarkGray
    Write-Host '  强制重建：cd frontend; npm run build' -ForegroundColor DarkGray
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
$fil    = $null
$qq     = $null
$shellSvc = $null
$approvalSvc = $null
$batchSvc = $null
$timerSvc = $null
$mcpSvc = $null
$reasoner = $null
$frontend = $null
try {
    Write-Host '[4/9] 启动 settings-center ...' -ForegroundColor Cyan
    $center = Start-Process -FilePath (Join-Path $binDir 'settings-center.exe') -ArgumentList '-addr', $CenterAddr -PassThru -WindowStyle Hidden
    if (-not (Wait-Port $CenterAddr 60 $center)) { throw 'settings-center 未在 60 秒内就绪' }

    New-Item -ItemType Directory -Force -Path $dataDir | Out-Null

    Write-Host '[5/9] 启动 memory + embedding ...' -ForegroundColor Cyan
    $mem = Start-Process -FilePath (Join-Path $binDir 'memory.exe') -ArgumentList '-addr', $MemoryAddr, '-center', "ws://$CenterAddr/ws", '-db', (Join-Path $dataDir 'memory.db') -PassThru -WindowStyle Hidden
    if (-not (Wait-Port $MemoryAddr 60 $mem)) { throw 'memory 未在 60 秒内就绪' }
    $emb = Start-Process -FilePath $embPython -ArgumentList (@('-m', 'sump_embedding') + $embeddingArgs) -WorkingDirectory $root -PassThru -WindowStyle Hidden
    if (-not (Wait-Port $EmbeddingAddr 60 $emb)) { throw 'embedding 未在 60 秒内就绪（首次启动需加载模型，或依赖缺失——见上方日志）' }

    Write-Host '[6/9] 启动 images + files + qq ...' -ForegroundColor Cyan
    $img = Start-Process -FilePath (Join-Path $binDir 'sump-images.exe') -ArgumentList '-addr', $ImageAddr, '-center', "ws://$CenterAddr/ws", '-db', (Join-Path $dataDir 'images.db'), '-dir', (Join-Path $dataDir 'images') -PassThru -WindowStyle Hidden
    if (-not (Wait-Port $ImageAddr 60 $img)) { throw 'images 未在 60 秒内就绪' }
    $fil = Start-Process -FilePath (Join-Path $binDir 'sump-files.exe') -ArgumentList '-addr', $FilesAddr, '-center', "ws://$CenterAddr/ws", '-db', (Join-Path $dataDir 'files.db'), '-dir', (Join-Path $dataDir 'files') -PassThru -WindowStyle Hidden
    if (-not (Wait-Port $FilesAddr 60 $fil)) { throw 'files 未在 60 秒内就绪' }
    $qqArgs = @('-addr', $QQAddr, '-center', "ws://$CenterAddr/ws", '-napcat', $NapCatURL)
    if ($NapCatToken) { $qqArgs += @('-napcat-token', $NapCatToken) }
    if ($Owner) { $qqArgs += @('-owner', $Owner) }
    $qq = Start-Process -FilePath (Join-Path $binDir 'qq.exe') -ArgumentList $qqArgs -PassThru -WindowStyle Hidden
    if (-not (Wait-Port $QQAddr 60 $qq)) { throw 'qq 未在 60 秒内就绪' }

    Write-Host '[7/9] 启动 approval + batch + shell + timer + mcp ...' -ForegroundColor Cyan
    $approvalSvc = Start-Process -FilePath (Join-Path $binDir 'sump-approval.exe') -ArgumentList 'serve', '-addr', $ApprovalAddr, '-center', "ws://$CenterAddr/ws", '-portal', 'frontend' -PassThru -WindowStyle Hidden
    if (-not (Wait-Port $ApprovalAddr 60 $approvalSvc)) { throw 'approval 未在 60 秒内就绪' }
    $batchSvc = Start-Process -FilePath (Join-Path $binDir 'batch.exe') -ArgumentList '-addr', $BatchAddr, '-center', "ws://$CenterAddr/ws", '-db', ('"' + (Join-Path $dataDir 'batch.db') + '"') -PassThru -WindowStyle Hidden
    if (-not (Wait-Port $BatchAddr 60 $batchSvc)) { throw 'batch 未在 60 秒内就绪' }
    $shellSvc = Start-Process -FilePath (Join-Path $binDir 'shell.exe') -ArgumentList '-addr', $ShellAddr, '-center', "ws://$CenterAddr/ws", '-root', ('"' + $root + '"'), '-approval', 'approval' -PassThru -WindowStyle Hidden
    if (-not (Wait-Port $ShellAddr 60 $shellSvc)) { throw 'shell 未在 60 秒内就绪' }
    # -portal frontend：把在计时器推给门户展示倒计时（留空 = 不推）。
    $timerSvc = Start-Process -FilePath (Join-Path $binDir 'timer.exe') -ArgumentList '-addr', $TimerAddr, '-center', "ws://$CenterAddr/ws", '-portal', 'frontend' -PassThru -WindowStyle Hidden
    if (-not (Wait-Port $TimerAddr 60 $timerSvc)) { throw 'timer 未在 60 秒内就绪' }

    $mcpArgs = @('-addr', $MCPAddr, '-center', "ws://$CenterAddr/ws", '-http-addr', $MCPHTTPAddr,
        '-config', ('"' + (Join-Path $dataDir 'mcp.json') + '"'),
        '-secret-file', ('"' + (Join-Path $dataDir 'mcp-secret.json') + '"'))
    $mcpSvc = Start-Process -FilePath (Join-Path $binDir 'sump-mcp.exe') -ArgumentList $mcpArgs -WorkingDirectory $root -PassThru -WindowStyle Hidden
    if (-not (Wait-Port $MCPAddr 60 $mcpSvc)) { throw 'mcp 内部 WS 未在 60 秒内就绪' }
    if (-not (Wait-Port $MCPHTTPAddr 60 $mcpSvc)) { throw 'mcp HTTP 未在 60 秒内就绪' }

    Write-Host '[8/9] 启动 reasoner ...' -ForegroundColor Cyan
    $reasonerArgs = @('-addr', $ReasonerAddr, '-center', "ws://$CenterAddr/ws", '-llm-base', $LlmBase, '-llm-model', $LlmModel)
    if ($LlmKey) { $reasonerArgs += @('-llm-key', $LlmKey) }
    $reasoner = Start-Process -FilePath (Join-Path $binDir 'reasoner.exe') -ArgumentList $reasonerArgs -PassThru -WindowStyle Hidden

    Write-Host '[9/9] 启动 frontend ...' -ForegroundColor Cyan
    $frontend = Start-Process -FilePath 'node' -ArgumentList 'dist/server/index.js', '-addr', $FrontendAddr, '-center', "ws://$CenterAddr/ws", '-approval', 'approval' -WorkingDirectory $frontendDir -PassThru -WindowStyle Hidden
    if (-not (Wait-Port $FrontendAddr 60 $frontend)) { throw 'frontend 未在 60 秒内就绪' }

    Write-Host ''
    Write-Host "settings-center: ws://$CenterAddr/ws  (PID $($center.Id))"
    Write-Host "memory:          ws://$MemoryAddr/ws  (PID $($mem.Id))  db: data/memory.db"
    Write-Host "embedding:       ws://$EmbeddingAddr/ws  (PID $($emb.Id))  后端: $(if ($null -ne $modelFile) { 'onnx（真模型）' } else { 'fake（无模型——语义召回不可用）' })"
    Write-Host "files:           ws://$FilesAddr/ws  (PID $($fil.Id))  db: data/files.db  dir: data/files"
    Write-Host "images:          ws://$ImageAddr/ws  (PID $($img.Id))  db: data/images.db  dir: data/images"
    Write-Host "qq:              ws://$QQAddr/ws  (PID $($qq.Id))  napcat: $NapCatURL"
    Write-Host "shell:           ws://$ShellAddr/ws  (PID $($shellSvc.Id))  root: $root  审批: approval"
    Write-Host "approval:        ws://$ApprovalAddr/ws  (PID $($approvalSvc.Id))  PowerShell 第一层"
    Write-Host "batch:           ws://$BatchAddr/ws  (PID $($batchSvc.Id))  db: data/batch.db"
    Write-Host "timer:           ws://$TimerAddr/ws  (PID $($timerSvc.Id))  门户: frontend"
    Write-Host "mcp:             ws://$MCPAddr/ws  (PID $($mcpSvc.Id))  外部: http://$MCPHTTPAddr/mcp"
    Write-Host '                 首次使用：面板设置 mcp/http.token，data/mcp.json 配置导入/导出绑定。' -ForegroundColor DarkGray
    Write-Host "reasoner:       ws://$ReasonerAddr/ws  (PID $($reasoner.Id))"
    Write-Host "frontend:       http://$FrontendAddr/  (PID $($frontend.Id))  ← 设置面板" -ForegroundColor Green
    Write-Host '按 Ctrl+C 停止' -ForegroundColor Yellow

    Wait-Process -Id $center.Id, $mem.Id, $emb.Id, $img.Id, $fil.Id, $qq.Id, $shellSvc.Id, $approvalSvc.Id, $batchSvc.Id, $timerSvc.Id, $mcpSvc.Id, $reasoner.Id, $frontend.Id
} finally {
    foreach ($proc in @($center, $mem, $emb, $img, $fil, $qq, $shellSvc, $approvalSvc, $batchSvc, $timerSvc, $mcpSvc, $reasoner, $frontend)) {
        if ($null -ne $proc -and -not $proc.HasExited) {
            Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
        }
    }
}
