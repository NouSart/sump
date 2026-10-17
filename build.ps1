param([string]$GoExe='go')
$ErrorActionPreference='Stop'
$bin=Join-Path $PSScriptRoot 'bin'
New-Item -ItemType Directory -Force -Path $bin | Out-Null
$targets=@{'approval'='sump-approval.exe';'settings-center'='settings-center.exe';'shell'='shell.exe'}
foreach($module in $targets.Keys){
 Push-Location (Join-Path $PSScriptRoot $module)
 try{
  & $GoExe build -trimpath -o (Join-Path $bin $targets[$module]) .
  if($LASTEXITCODE -ne 0){throw "$module build failed"}
 } finally{Pop-Location}
}
Write-Host 'Build complete: bin/'
