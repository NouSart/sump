param([string]$GoExe='go')
$ErrorActionPreference='Stop'
$root = Split-Path -Parent $PSScriptRoot
foreach($module in @('src\protocol','src\approval','src\shell','src\reasoner','testing\approval-integration')){
 Push-Location (Join-Path $root $module)
 try{
  & $GoExe test ./... -count=1
  if($LASTEXITCODE -ne 0){throw "$module tests failed"}
 } finally{Pop-Location}
}
