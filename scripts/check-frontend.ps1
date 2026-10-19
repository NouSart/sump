<#
.SYNOPSIS
    只读体检：前端动没动过、要不要重建。

.DESCRIPTION
    回答一个问题——「上次前端构建之后，src 到底改没改？现在跑的 bundle 是不是它？」

    比看 mtime 可靠：比对的是构建时记下的逐文件内容指纹（frontend/dist/.build-stamp），
    时钟被改、git checkout 把 mtime 刷成当前时间，都骗不了它。
    本脚本不构建、不重启、不修改任何文件，纯读。

    提示：npm run build 成功后会自动写指纹；老 dist 没指纹时用
         node frontend/scripts/build-stamp.mjs adopt
    按当前 dist 建一次基线即可。

.PARAMETER Detail
    把每个文件的比对结果都列出来（默认最多列 8 条）。

.EXAMPLE
    .\scripts\check-frontend.ps1
    .\scripts\check-frontend.ps1 -Detail
    .\scripts\reload-sump.ps1 -Check      # 同一条命令，从重载入口问

.NOTES
    退出码：0 = 无需重建；10 = 需要重建；11 = 没有指纹但 mtime 看不出差异；其他 = 判定失败。
#>
[CmdletBinding()]
param(
    [switch]$Detail
)

$ErrorActionPreference = 'Stop'

$root        = Split-Path -Parent $PSScriptRoot
$frontendDir = Join-Path $root 'frontend'
$tool        = Join-Path $frontendDir 'scripts\build-stamp.mjs'

# node 的输出是 UTF-8：不先把控制台解码改过来，中文会变成乱码。
$prevEnc = $null
try { $prevEnc = [Console]::OutputEncoding; [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

$code = 2
try {
    if (-not (Test-Path -LiteralPath $tool)) {
        Write-Host ("✗ 找不到体检工具：{0}" -f $tool) -ForegroundColor Red
        Write-Host '  它应随仓库提交：frontend/scripts/build-stamp.mjs' -ForegroundColor DarkGray
        exit 2
    }
    if (-not (Get-Command node -ErrorAction SilentlyContinue)) {
        Write-Host '✗ 找不到 node（前端构建需要 Node ≥ 22）' -ForegroundColor Red
        exit 2
    }

    $nodeArgs = @($tool, 'check')
    if ($Detail) { $nodeArgs += '-v' }

    & node @nodeArgs
    $code = $LASTEXITCODE
    if ($null -eq $code) { $code = 2 }
} finally {
    if ($null -ne $prevEnc) { try { [Console]::OutputEncoding = $prevEnc } catch { } }
}

exit $code
