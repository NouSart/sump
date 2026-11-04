<#
.SYNOPSIS
  打包 SUMP：编译 11 个服务，收集前端构建产物与 embedding 源码，产出可传输的 zip。

.DESCRIPTION
  只做「构建与收集」，不改变运行前提：目标机仍需要 Node / Python（见 automation/README.md）。
  任何一步失败即中止；最后核对产物清单，少一个就返回非零。

.EXAMPLE
  .\automation\package.ps1                  # 全量打包到 dist/
  .\automation\package.ps1 -SkipFrontend   # 复用现有前端 dist
  .\automation\package.ps1 -NoZip          # 只留目录，不压缩
#>
param(
    [string]$OutDir = '',
    [string]$Version = '',
    [switch]$SkipFrontend,
    [switch]$IncludeNodeModules,
    [switch]$NoZip
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot

if (-not $OutDir) { $OutDir = Join-Path $root 'dist' }

if (-not $Version) {
    $line = Select-String -Path (Join-Path $root 'README.md') -Pattern '\*\*(v\d+\.\d+\.\d+[0-9A-Za-z\.\-]*)\*\*' |
        Select-Object -First 1
    if (-not $line) { throw '从 README.md 取不到版本号，请用 -Version 指定' }
    $Version = $line.Matches[0].Groups[1].Value
}

$name = "sump-$Version-win-x64"
$stage = Join-Path $OutDir $name
$binDir = Join-Path $stage 'bin'

# 二进制名不能改：images / files / mcp / approval 用 sump- 前缀，是为躲杀软对通用名的误报。
$services = [ordered]@{
    'settings-center' = 'settings-center.exe'
    'memory'          = 'memory.exe'
    'images'          = 'sump-images.exe'
    'files'           = 'sump-files.exe'
    'qq'              = 'qq.exe'
    'shell'           = 'shell.exe'
    'approval'        = 'sump-approval.exe'
    'batch'           = 'batch.exe'
    'timer'           = 'timer.exe'
    'mcp'             = 'sump-mcp.exe'
    'reasoner'        = 'reasoner.exe'
}

Write-Host "[1/5] 构建 $($services.Count) 个服务..." -ForegroundColor Cyan
if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
New-Item -ItemType Directory -Force -Path $binDir | Out-Null

Push-Location $root
try {
    foreach ($mod in $services.Keys) {
        go build -trimpath -o (Join-Path $binDir $services[$mod]) "github.com/NouSart/sump/$mod"
        if ($LASTEXITCODE -ne 0) { throw "$mod 构建失败" }
        Write-Host "      $($services[$mod])"
    }
} finally {
    Pop-Location
}

Write-Host '[2/5] 前端...' -ForegroundColor Cyan
$frontendSrc = Join-Path $root 'src\frontend'
$frontendDist = Join-Path $frontendSrc 'dist'
if ($SkipFrontend) {
    if (-not (Test-Path -LiteralPath (Join-Path $frontendDist 'server\index.js'))) {
        throw '跳过前端构建，但 src/frontend/dist 不存在——先跑一次不带 -SkipFrontend 的打包'
    }
    Write-Host '      跳过构建，复用现有 dist'
} else {
    if (-not (Test-Path -LiteralPath (Join-Path $frontendSrc 'node_modules'))) {
        throw 'src/frontend/node_modules 不存在：先在该目录跑 npm ci'
    }
    Push-Location $frontendSrc
    try {
        npm run build
        if ($LASTEXITCODE -ne 0) { throw '前端构建失败' }
    } finally {
        Pop-Location
    }
}

$stageFrontend = Join-Path $stage 'frontend'
New-Item -ItemType Directory -Force -Path $stageFrontend | Out-Null
Copy-Item -LiteralPath $frontendDist -Destination $stageFrontend -Recurse -Force
foreach ($f in 'package.json', 'package-lock.json') {
    Copy-Item -LiteralPath (Join-Path $frontendSrc $f) -Destination $stageFrontend -Force
}
if ($IncludeNodeModules) {
    Write-Host '      打包运行时依赖（npm ci --omit=dev）...'
    Push-Location $stageFrontend
    try {
        npm ci --omit=dev --silent
        if ($LASTEXITCODE -ne 0) { throw '前端运行时依赖安装失败' }
    } finally {
        Pop-Location
    }
}

Write-Host '[3/5] embedding 源码...' -ForegroundColor Cyan
# 只带源码：.venv 与 90MB 的模型文件都不进包（模型缺席时召回退化为 bigram，功能不中断）
$embeddingSrc = Join-Path $root 'src\embedding'
$stageEmbedding = Join-Path $stage 'embedding'
New-Item -ItemType Directory -Force -Path $stageEmbedding | Out-Null
$skipDirs = @('.venv', '__pycache__', '.pytest_cache', 'sump_embedding.egg-info')
Get-ChildItem -LiteralPath $embeddingSrc -Force | Where-Object {
    $_.Name -ne 'tests' -and ($skipDirs -notcontains $_.Name)
} | ForEach-Object {
    Copy-Item -LiteralPath $_.FullName -Destination $stageEmbedding -Recurse -Force
}

Write-Host '[4/5] 清单与校验和...' -ForegroundColor Cyan
Set-Content -LiteralPath (Join-Path $stage 'VERSION') -Value $Version -Encoding ASCII

$manifest = @"
SUMP $Version · win-x64 · 构建产物集合

目录内容
  bin\            $($services.Count) 个服务可执行文件（settings-center 必须最先启动）
  frontend\       门户：dist\(server|web) + package.json（运行需 Node >= 22）
  embedding\      向量服务源码 + pyproject.toml（运行需 Python >= 3.12）
  VERSION         版本号
  SHA256SUMS.txt  逐文件校验和

运行前提（本包不携带运行时）
  1. Go 不需要——bin\ 里已是编译产物
  2. 门户：cd frontend && npm ci --omit=dev，然后 node dist\server\index.js
  3. 向量服务：cd embedding && python -m venv .venv，再装 -e .；
     模型 bge-small-zh-v1.5 不随包分发，缺席时退化为 bigram
  4. data\ 与 logs\ 在首次运行时生成；备份只需 data\
  5. 服务间 WS 不做身份认证，只可部署在可信本机 / 内网
"@
if (-not $IncludeNodeModules) {
    $manifest += "  6. 本包未含 frontend\node_modules：门户启动前必须先 npm ci --omit=dev`n"
}
Set-Content -LiteralPath (Join-Path $stage 'MANIFEST.txt') -Value $manifest -Encoding UTF8

$sums = Get-ChildItem -LiteralPath $stage -Recurse -File |
    Where-Object { $_.Name -ne 'SHA256SUMS.txt' } |
    Sort-Object FullName |
    ForEach-Object {
        $rel = $_.FullName.Substring($stage.Length + 1).Replace('\', '/')
        $hash = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLower()
        "$hash  $rel"
    }
Set-Content -LiteralPath (Join-Path $stage 'SHA256SUMS.txt') -Value $sums -Encoding ASCII

Write-Host '[5/5] 核对产物...' -ForegroundColor Cyan
$missing = @()
foreach ($exe in $services.Values) {
    $p = Join-Path $binDir $exe
    if (-not (Test-Path -LiteralPath $p)) { $missing += "bin\$exe"; continue }
    if ((Get-Item -LiteralPath $p).Length -lt 500KB) { $missing += "bin\$exe（体积异常）" }
}
foreach ($rel in 'frontend\dist\server\index.js', 'frontend\dist\web\index.html') {
    if (-not (Test-Path -LiteralPath (Join-Path $stage $rel))) { $missing += $rel }
}
if (-not (Test-Path -LiteralPath (Join-Path $stageEmbedding 'pyproject.toml'))) { $missing += 'embedding\pyproject.toml' }
if ($missing.Count -gt 0) {
    throw "产物不完整：`n  " + ($missing -join "`n  ")
}

if (-not $NoZip) {
    $zip = Join-Path $OutDir "$name.zip"
    if (Test-Path -LiteralPath $zip) { Remove-Item -LiteralPath $zip -Force }
    Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $zip -CompressionLevel Optimal
    $sizeMB = [math]::Round((Get-Item -LiteralPath $zip).Length / 1MB, 1)
    Write-Host "打包完成：$zip（$sizeMB MB）" -ForegroundColor Green
} else {
    Write-Host "打包完成：$stage（未压缩，-NoZip）" -ForegroundColor Green
}
