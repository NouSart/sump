param([string]$GoExe='go')
$ErrorActionPreference='Stop'
foreach($module in @('protocol','approval','shell','reasoner','approval-integration')){
 Push-Location (Join-Path $PSScriptRoot $module)
 try{
  & $GoExe test ./... -count=1
  if($LASTEXITCODE -ne 0){throw "$module tests failed"}
 } finally{Pop-Location}
}
