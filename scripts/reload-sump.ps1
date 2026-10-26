<#
.SYNOPSIS
    重载 SUMP 进程栈：先关掉所有 SUMP 进程（含它们派生出来的命令进程），再重新拉起。

.DESCRIPTION
    改完代码（Go / 前端 / 配置）后，一条命令把整栈换掉，让新代码生效。
    本脚本只负责"停干净 + 再拉一次"；构建与启动细节仍由 run-local.ps1（开发）
    或 serve.ps1（长期守护 + 落日志）负责，不重复实现。

    找进程的判定（宁可少杀，也不误杀别人家的 qq.exe / node.exe）：
      1) 服务可执行文件：路径在 %TEMP%\sump-v2-bin 下，或命令行里带本仓库根目录；
      2) 宿主：命令行里带 run-local.ps1 / serve.ps1 / start-sump.cmd 的 cmd / powershell；
      3) 顺父子关系收编：上面这些进程派生出来的 node.exe（前端）/ python.exe（embedding）等。
    然后按依赖逆序 taskkill /T /F（/T 连带整棵进程树，比如 shell 服务正在跑的那条命令）。

.PARAMETER DryRun
    只列出"会杀掉哪些进程"，不动手。改完脚本先跑这个最稳。

.PARAMETER Check
    只体检前端，不重载：跑 scripts\check-frontend.ps1，回答「上次构建之后 src 动没动过、要不要重建」。
    不动任何文件、不杀进程、不重启，看完结论就退出。
    退出码同 check-frontend：0 = 无需重建，10 = 需要重建。

.PARAMETER Detach
    把自己派成后台进程再干活，调用方立刻返回。
    从 SUMP 内部（对话里的 shell 服务）调用时必须这样：否则重载在杀 shell.exe 的那一刻
    会把发起重载的那条命令一起带走，重载自己就断在半路。
    脚本会自动识别这种场景（祖先里有 shell.exe / reasoner.exe）并强制走后台派发。

.PARAMETER Serve
    重启时改用 scripts\serve.ps1（长期守护：日志落 logs\，退出退避重拉）。

.PARAMETER DelaySec
    后台模式先等几秒再动手（默认 3），让发起方（门户、那条 shell 命令）先收到回执。

.EXAMPLE
    .\scripts\reload-sump.ps1 -DryRun     # 先看会杀掉哪些进程
    .\scripts\reload-sump.ps1             # 停干净再拉起（开发用 run-local）
    .\scripts\reload-sump.ps1 -Serve      # 重载成长期守护形态
    .\scripts\reload-sump.ps1 -Detach     # 在 SUMP 对话里让它这么跑
    .\scriptseload-sump.ps1 -Check      # 只看前端动没动过、要不要重建（不重载）
#>
[CmdletBinding()]
param(
    [switch]$DryRun,
    [switch]$Detach,
    [switch]$Check,
    [switch]$CheckDetail,
    [switch]$Watcher,
    [switch]$Serve,
    [int]$DelaySec = 3,
    [int]$StopTimeoutSec = 20,
    [int]$ReadyTimeoutSec = 180,
    [string]$LogFile = ''
)

$ErrorActionPreference = 'Stop'

$root      = Split-Path -Parent $PSScriptRoot
$binDir    = Join-Path $env:TEMP 'sump-v2-bin'
$logDir    = Join-Path $root 'logs'
$script:LogFile = $null

$serviceExes     = @('settings-center.exe','memory.exe','sump-images.exe','sump-files.exe','qq.exe','shell.exe','sump-approval.exe','batch.exe','sump-mcp.exe','timer.exe','reasoner.exe')
$childExes       = $serviceExes + @('node.exe','python.exe','pythonw.exe')
$launcherPattern = 'run-local\.ps1|serve\.ps1|start-sump\.cmd'

function Write-Line([string]$text, [string]$color = 'Gray') {
    Write-Host $text -ForegroundColor $color
    if ($script:LogFile) {
        try { Add-Content -LiteralPath $script:LogFile -Value $text -Encoding UTF8 } catch { }
    }
}

function Get-PowerShellPath {
    $exe = Join-Path $PSHOME 'powershell.exe'
    if (Test-Path $exe) { return $exe }
    return 'powershell.exe'
}

function Has-Root([string]$s) {
    if ([string]::IsNullOrEmpty($s)) { return $false }
    return ($s.IndexOf($root, [StringComparison]::OrdinalIgnoreCase) -ge 0)
}

# 停止顺序：先停"会派生别的东西"的，最后停基础设施（settings-center 一停，其余服务会自然掉线）。
function Get-StopRank([string]$name) {
    switch ($name.ToLower()) {
        'cmd.exe'             { return 0 }
        'powershell.exe'      { return 1 }
        'pwsh.exe'            { return 1 }
        'shell.exe'           { return 2 }
        'reasoner.exe'        { return 3 }
        'node.exe'            { return 4 }
        'python.exe'          { return 5 }
        'pythonw.exe'         { return 5 }
        'sump-mcp.exe'        { return 5 }
        'batch.exe'           { return 5 }
        'timer.exe'           { return 6 }
        'sump-approval.exe'   { return 7 }
        'qq.exe'              { return 8 }
        'sump-files.exe'      { return 9 }
        'sump-images.exe'     { return 10 }
        'memory.exe'          { return 11 }
        'settings-center.exe' { return 12 }
        default               { return 20 }
    }
}

# 找出"属于本仓库的 SUMP 进程"。判定见文件头；核心是不靠进程名一刀切（qq.exe/node.exe 可能是别人家的）。
function Get-SumpProcesses {
    $all  = @(Get-CimInstance Win32_Process)
    $byId = @{}
    foreach ($p in $all) { $byId[[int]$p.ProcessId] = $p }

    $picked = New-Object 'System.Collections.Generic.HashSet[int]'
    foreach ($p in $all) {
        $id = [int]$p.ProcessId
        if ($id -eq $PID) { continue }
        $name = $p.Name.ToLower()
        $cmd  = if ($p.CommandLine) { $p.CommandLine } else { '' }
        $exe  = if ($p.ExecutablePath) { $p.ExecutablePath } else { '' }

        if ($serviceExes -contains $name) {
            if (($exe -and $exe.IndexOf($binDir, [StringComparison]::OrdinalIgnoreCase) -ge 0) -or (Has-Root $cmd)) {
                [void]$picked.Add($id)
            }
            continue
        }
        if ($name -eq 'cmd.exe' -or $name -eq 'powershell.exe' -or $name -eq 'pwsh.exe') {
            if ($cmd -match $launcherPattern -and $cmd -notmatch 'reload-sump') {
                [void]$picked.Add($id)
            }
            continue
        }
        if (($name -eq 'python.exe' -or $name -eq 'pythonw.exe') -and $cmd -match 'sump_embedding') {
            [void]$picked.Add($id)
        }
    }

    # 顺父子关系收编：前端的 node、embedding 的 python 命令行里未必带仓库路径，但父进程是我们找到的那些。
    $changed = $true
    while ($changed) {
        $changed = $false
        foreach ($p in $all) {
            $id = [int]$p.ProcessId
            if ($id -eq $PID -or $picked.Contains($id)) { continue }
            if ($childExes -notcontains $p.Name.ToLower()) { continue }
            if ($picked.Contains([int]$p.ParentProcessId)) {
                [void]$picked.Add($id)
                $changed = $true
            }
        }
    }

    $list = foreach ($id in $picked) {
        $p   = $byId[$id]
        $cmd = if ($p.CommandLine) { $p.CommandLine } else { '' }
        $addr = $null
        if ($cmd -match '-addr\s+"?([0-9A-Za-z\.\-]+:\d+)"?') { $addr = $Matches[1] }
        [pscustomobject]@{
            Pid         = $id
            Name        = $p.Name
            Addr        = $addr
            Rank        = Get-StopRank $p.Name
            CommandLine = $cmd
        }
    }
    return @($list | Sort-Object Rank, Pid)
}

# 杀掉一棵进程树。taskkill 对"已经不在了"的 PID（多半是刚被父进程树的 /T 连带带走）会往 stderr 写
# "错误: 没有找到进程"；而 PowerShell 5.1 在 $ErrorActionPreference='Stop' 下会把原生命令的 stderr
# 当成终止性错误 -> 整段重载就此中断（reload 打到一半突然退回提示符，就是这个坑）。
# 所以：先确认进程还在，再局部关掉 Stop 并把输出吞掉。返回 $false 表示"它早就没了"。
function Stop-Tree([int]$procId) {
    if (-not (Get-Process -Id $procId -ErrorAction SilentlyContinue)) { return $false }
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        & taskkill /T /F /PID $procId 2>&1 | Out-Null
    } catch {
    } finally {
        $ErrorActionPreference = $prev
    }
    return $true
}

function Test-PortOpen([string]$addr) {
    if ([string]::IsNullOrEmpty($addr)) { return $false }
    $h, $pt = $addr.Split(':')
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $client.Connect($h, [int]$pt)
        $client.Close()
        return $true
    } catch {
        return $false
    } finally {
        $client.Dispose()
    }
}

# 从 SUMP 内部调用时（祖先里有 shell.exe / reasoner.exe），直接在本进程里停服会连发起重载的那条命令
# 一起杀掉——所以必须识别出来，改走后台派发。
function Test-InsideSump {
    $cur = $PID
    for ($i = 0; $i -lt 8; $i++) {
        $p = Get-CimInstance Win32_Process -Filter "ProcessId=$cur" -ErrorAction SilentlyContinue
        if (-not $p) { break }
        $cur = [int]$p.ParentProcessId
        if ($cur -le 0) { break }
        $parent = Get-CimInstance Win32_Process -Filter "ProcessId=$cur" -ErrorAction SilentlyContinue
        if (-not $parent) { break }
        $n = $parent.Name.ToLower()
        if ($n -eq 'shell.exe' -or $n -eq 'reasoner.exe') { return $true }
    }
    return $false
}

function Invoke-Reload {
    $procs = Get-SumpProcesses

    Write-Line ''
    Write-Line ("仓库根：{0}" -f $root) 'DarkGray'
    Write-Line ("服务二进制目录：{0}" -f $binDir) 'DarkGray'
    Write-Line ''

    if ($procs.Count -eq 0) {
        Write-Line '没有发现正在跑的 SUMP 进程（视为已停干净）。' 'Yellow'
    } else {
        Write-Line ("发现 {0} 个 SUMP 进程：" -f $procs.Count) 'Cyan'
        $procs | Select-Object Pid, Name, Addr, Rank |
            Format-Table -AutoSize | Out-String -Width 200 |
            ForEach-Object { Write-Line $_.TrimEnd() 'Gray' }
    }

    $ports = @($procs | Where-Object { $_.Addr } | ForEach-Object { $_.Addr } | Sort-Object -Unique)

    if ($DryRun) {
        if ($ports.Count -gt 0) { Write-Line ("涉及端口：{0}" -f ($ports -join ', ')) 'DarkGray' }
        Write-Line 'DryRun：只报告，不动手。' 'Green'
        return 0
    }

    if ($procs.Count -gt 0) {
        Write-Line '按依赖逆序停止（taskkill /T /F，连带整棵进程树）……' 'Cyan'
        foreach ($p in $procs) {
            $suffix = if ($p.Addr) { '  ' + $p.Addr } else { '' }
            if (Stop-Tree $p.Pid) {
                Write-Line ("  - {0} (PID {1}){2}" -f $p.Name, $p.Pid, $suffix) 'Gray'
            } else {
                Write-Line ("  - {0} (PID {1}){2}  已随父进程树退出，跳过" -f $p.Name, $p.Pid, $suffix) 'DarkGray'
            }
        }

        # 等它们真的退出（taskkill /F 通常立刻生效，但给点余量）
        $deadline = (Get-Date).AddSeconds($StopTimeoutSec)
        while ((Get-Date) -lt $deadline) {
            $left = @($procs | Where-Object { Get-Process -Id $_.Pid -ErrorAction SilentlyContinue })
            if ($left.Count -eq 0) { break }
            Start-Sleep -Milliseconds 300
        }
        $left = @($procs | Where-Object { Get-Process -Id $_.Pid -ErrorAction SilentlyContinue })
        if ($left.Count -gt 0) {
            Write-Line ("  {0} 个进程还没退出，再补一刀……" -f $left.Count) 'Yellow'
            foreach ($p in $left) { [void](Stop-Tree $p.Pid) }
            Start-Sleep -Seconds 1
            $left = @($procs | Where-Object { Get-Process -Id $_.Pid -ErrorAction SilentlyContinue })
        }
        if ($left.Count -gt 0) {
            $names = ($left | ForEach-Object { "$($_.Name)($($_.Pid))" }) -join ', '
            Write-Line ("✗ 仍有 {0} 个进程停不掉：{1}" -f $left.Count, $names) 'Red'
            Write-Line '  已中止：进程没停干净就重启，run-local 的端口自检会直接抛错。' 'Red'
            return 1
        }
        Write-Line '  ✓ 全部已停止' 'Green'
    }

    # 端口兜底校验：万一有"不是我们起的"进程占着端口，run-local 起来也会立刻失败，这里先说清楚。
    Start-Sleep -Milliseconds 500
    $busy = @($ports | Where-Object { Test-PortOpen $_ })
    if ($busy.Count -gt 0) {
        Write-Line ("✗ 这些端口仍被占用：{0}" -f ($busy -join ', ')) 'Red'
        Write-Line '  占用的不是我们识别到的 SUMP 进程——请手工确认（netstat -ano ^| findstr :端口）。已中止重启。' 'Red'
        return 1
    }

    $launcher = if ($Serve) { Join-Path $PSScriptRoot 'serve.ps1' } else { Join-Path $PSScriptRoot 'run-local.ps1' }
    if (-not (Test-Path $launcher)) {
        Write-Line ("✗ 找不到启动脚本：{0}" -f $launcher) 'Red'
        return 1
    }

    Write-Line ("启动：{0}" -f $launcher) 'Cyan'
    $ps = Get-PowerShellPath
    $child = Start-Process -FilePath $ps `
        -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', ('"' + $launcher + '"')) `
        -WorkingDirectory $root -WindowStyle Minimized -PassThru
    Write-Line ("  已拉起 PID {0}（独立最小化窗口；关掉它或 Ctrl+C 即全停）" -f $child.Id) 'Gray'

    if ($ports.Count -eq 0) {
        Write-Line '完成（没有可等待的端口，不校验就绪）。' 'Green'
        return 0
    }

    Write-Line ("等端口就绪（最多 {0} 秒）：{1}" -f $ReadyTimeoutSec, ($ports -join ', ')) 'Cyan'
    $deadline = (Get-Date).AddSeconds($ReadyTimeoutSec)
    $pending  = New-Object 'System.Collections.Generic.List[string]'
    foreach ($a in $ports) { $pending.Add($a) }
    while ((Get-Date) -lt $deadline -and $pending.Count -gt 0) {
        if ($child.HasExited) {
            Write-Line ("✗ 启动进程已退出（码 {0}）——看它的窗口或 logs 里的报错。" -f $child.ExitCode) 'Red'
            return 1
        }
        foreach ($a in @($pending)) {
            if (Test-PortOpen $a) {
                Write-Line ("  ✓ {0} 已就绪" -f $a) 'Green'
                $pending.Remove($a) | Out-Null
            }
        }
        if ($pending.Count -gt 0) { Start-Sleep -Milliseconds 700 }
    }
    if ($pending.Count -gt 0) {
        Write-Line ("⚠ 这些端口还没起来：{0}" -f ($pending -join ', ')) 'Yellow'
        Write-Line ("  可能还在构建（Go 构建 + 前端 npm build 首次较慢）。它还在跑（PID {0}），稍后自己会起来。" -f $child.Id) 'Yellow'
        return 0
    }

    Write-Line ''
    Write-Line '✓ SUMP 已重载完成，门户：http://127.0.0.1:9501/' 'Green'
    return 0
}

# 后台派发（-Detach / 从 SUMP 内部调用）：
# 用 WMI 建进程，新进程的父是 WmiPrvSE，不在调用者（shell 服务）的进程树里——
# 所以 shell 服务收尾时的 taskkill /T 杀不到它，重载不会把自己打断在半路。
function Start-DetachedReload {
    New-Item -ItemType Directory -Force -Path $logDir | Out-Null
    $log = Join-Path $logDir ('reload-{0}.log' -f (Get-Date -Format 'yyyyMMdd-HHmmss'))
    $ps  = Get-PowerShellPath
    $argLine = '-NoProfile -ExecutionPolicy Bypass -File "' + $PSCommandPath + '" -Watcher' +
               ' -DelaySec ' + $DelaySec + ' -StopTimeoutSec ' + $StopTimeoutSec +
               ' -ReadyTimeoutSec ' + $ReadyTimeoutSec + ' -LogFile "' + $log + '"'
    if ($Serve) { $argLine += ' -Serve' }

    $r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{
        CommandLine      = ('"' + $ps + '" ' + $argLine)
        CurrentDirectory = $root
    }
    if ($r.ReturnValue -ne 0) { throw ("派发后台重载进程失败（Win32_Process.Create 返回 {0}）" -f $r.ReturnValue) }

    Write-Line ("已派发后台重载进程 PID {0}；{1} 秒后开始停服 → 重启。" -f $r.ProcessId, $DelaySec) 'Green'
    Write-Line ("日志：{0}" -f $log) 'Gray'
    Write-Line '注意：重启会连同 reasoner / shell 一起换掉，这一轮对话会断开；门户重连后继续。' 'DarkGray'
    Write-Line '     结果看日志末尾，或过一两分钟刷新 http://127.0.0.1:9501/' 'DarkGray'
}

# ── 主流程 ───────────────────────────────────────────────────────────────────
# -Check：只体检前端，不重载。放在最前面，免得它跟 -Detach / 后台派发搅在一起，
# 误把人家的服务重启了。转交 scripts/check-frontend.ps1，退出码原样透出。
if ($Check) {
    $checker = Join-Path $PSScriptRoot 'check-frontend.ps1'
    if (-not (Test-Path -LiteralPath $checker)) {
        Write-Line ('找不到体检脚本：{0}' -f $checker) 'Red'
        exit 2
    }
    $psCheck = Get-PowerShellPath
    $checkArgs = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', ('"' + $checker + '"'))
    if ($CheckDetail) { $checkArgs += '-Detail' }
    & $psCheck @checkArgs
    exit $LASTEXITCODE
}

if ($Watcher) {
    if (-not $LogFile) {
        New-Item -ItemType Directory -Force -Path $logDir | Out-Null
        $LogFile = Join-Path $logDir ('reload-{0}.log' -f (Get-Date -Format 'yyyyMMdd-HHmmss'))
    }
    $script:LogFile = $LogFile
    Write-Line ("=== SUMP 重载开始（PID {0}）===" -f $PID) 'Cyan'
    if ($DelaySec -gt 0) {
        Write-Line ("先等 {0} 秒，让发起方先收到回执……" -f $DelaySec) 'Gray'
        Start-Sleep -Seconds $DelaySec
    }
    $code = Invoke-Reload
    Write-Line ("=== 结束（退出码 {0}）===" -f $code) 'Cyan'
    exit $code
}

if ($DryRun) {
    exit (Invoke-Reload)
}

if ($Detach -or (Test-InsideSump)) {
    if (-not $Detach) {
        Write-Line '检测到本进程运行在 SUMP 内部（祖先进程里有 shell.exe / reasoner.exe）。' 'Yellow'
        Write-Line '在这里直接停服会把发起重载的这条命令一起杀掉，所以自动改为后台派发。' 'Yellow'
    }
    Start-DetachedReload
    exit 0
}

exit (Invoke-Reload)
