param([string]$GoExe='go')
$ErrorActionPreference='Stop'
$root=Split-Path -Parent $PSScriptRoot
$bin=Join-Path $root 'bin'
New-Item -ItemType Directory -Force -Path $bin | Out-Null
$targets=@{'approval'='sump-approval.exe';'settings-center'='settings-center.exe';'shell'='shell.exe'}
foreach($module in $targets.Keys){
 Push-Location (Join-Path $root 'src' $module)
 try{
  & $GoExe build -trimpath -o (Join-Path $bin $targets[$module]) .
  if($LASTEXITCODE -ne 0){throw "$module build failed"}
 } finally{Pop-Location}
}
Write-Host 'Build complete: bin/'
