# Minimal local stack: registry + approval + terminal. No model key required.
param(
 [int]$CenterPort = 19000,
 [int]$ApprovalPort = 19801,
 [int]$ShellPort = 19701,
 [string]$WorkRoot = (Join-Path $PSScriptRoot 'command-work'),
 [int]$RunSeconds = 0
)
$ErrorActionPreference = 'Stop'
if (@($CenterPort,$ApprovalPort,$ShellPort | Select-Object -Unique).Count -ne 3) { throw 'Ports must be distinct.' }
foreach ($port in @($CenterPort,$ApprovalPort,$ShellPort)) {
 if ($port -lt 1024 -or $port -gt 65535) { throw 'Use ports in 1024..65535.' }
 $probe = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback,$port)
 try { $probe.Start() } finally { $probe.Stop() }
}
$psRuntime = Get-Command pwsh -ErrorAction SilentlyContinue
if (-not $psRuntime) { $psRuntime = Get-Command powershell -ErrorAction SilentlyContinue }
if (-not $psRuntime) { throw 'PowerShell is required.' }
$bin = Join-Path $PSScriptRoot 'bin'
foreach ($exe in @('sump-approval.exe','shell.exe','settings-center.exe')) {
 if (-not (Test-Path -LiteralPath (Join-Path $bin $exe))) { throw "Missing $exe. Run build.ps1 first." }
}
$runtimeDir = Join-Path $PSScriptRoot 'command-runtime'
New-Item -ItemType Directory -Force -Path $runtimeDir,$WorkRoot | Out-Null
$WorkRoot = (Resolve-Path -LiteralPath $WorkRoot).Path
$centerURL = "ws://127.0.0.1:$CenterPort/ws"
$children = New-Object 'System.Collections.Generic.List[System.Diagnostics.Process]'
function Start-ServiceProcess([string]$Name,[string[]]$Arguments,[int]$Port) {
 # Start-Process joins arguments on Windows. Quote individual values, including paths with spaces.
 $quoted = foreach ($value in $Arguments) {
  if ($value.Contains('"') -or $value.EndsWith('\')) { throw 'Unsupported quote or trailing slash in launcher argument.' }
  '"' + $value + '"'
 }
 $child = Start-Process -FilePath (Join-Path $bin "$Name.exe") -ArgumentList $quoted -WorkingDirectory $PSScriptRoot -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $runtimeDir "$Name.out.log") -RedirectStandardError (Join-Path $runtimeDir "$Name.err.log")
 $children.Add($child)
 Write-Host "Starting $Name on 127.0.0.1:$Port (PID $($child.Id))"
 $deadline = (Get-Date).AddSeconds(15)
 while ((Get-Date) -lt $deadline) {
  if ($child.HasExited) { throw "$Name exited. See command-runtime logs." }
  # Wait for process startup output before attempting the first socket connection.
  $startupOutput = [string](Get-Content -LiteralPath (Join-Path $runtimeDir "$Name.out.log") -Raw -ErrorAction SilentlyContinue)
  $startupError = [string](Get-Content -LiteralPath (Join-Path $runtimeDir "$Name.err.log") -Raw -ErrorAction SilentlyContinue)
  if (($startupOutput.Length + $startupError.Length) -eq 0) { Start-Sleep -Milliseconds 100; continue }
  if ($Name -ne 'settings-center' -and ($startupOutput + $startupError) -notmatch 'rev [0-9]+') { Start-Sleep -Milliseconds 100; continue }
  $client = New-Object System.Net.Sockets.TcpClient
  try { $client.Connect('127.0.0.1',$Port); return }
  catch [System.Net.Sockets.SocketException] { Start-Sleep -Milliseconds 100 }
  finally { $client.Dispose() }
 }
 throw "$Name did not start within 15 seconds."
}
try {
 Start-ServiceProcess 'settings-center' @('-addr',"127.0.0.1:$CenterPort",'-settings',(Join-Path $runtimeDir 'settings.json')) $CenterPort
 Start-ServiceProcess 'sump-approval' @('serve','-addr',"127.0.0.1:$ApprovalPort",'-center',$centerURL) $ApprovalPort
 Start-ServiceProcess 'shell' @('-addr',"127.0.0.1:$ShellPort",'-center',$centerURL,'-approval','approval','-shell',$psRuntime.Source,'-root',$WorkRoot) $ShellPort
 Write-Host "READY: $centerURL" -ForegroundColor Green
 Write-Host "Work directory: $WorkRoot"
 Write-Host 'In another PowerShell window, run: .\submit-command.ps1 -CommandText "Write-Output hello"'
 Write-Host 'Press Ctrl+C to stop. Logs: command-runtime/'
 $end = if ($RunSeconds -gt 0) { (Get-Date).AddSeconds($RunSeconds) } else { [DateTime]::MaxValue }
 while ((Get-Date) -lt $end) {
  foreach ($child in $children) { if ($child.HasExited) { throw 'A service exited; see logs.' } }
  Start-Sleep -Milliseconds 300
 }
} finally {
 foreach ($child in $children) {
  if (-not $child.HasExited) { Stop-Process -Id $child.Id -Force -ErrorAction SilentlyContinue }
 }
}
