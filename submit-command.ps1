param([Parameter(Mandatory=$true)][string]$CommandText,[string]$Center='ws://127.0.0.1:19000/ws',[string]$WorkDir='')
$ErrorActionPreference='Stop'
$arguments = @('send','-center',$Center,'-command',$CommandText)
if ($WorkDir) { $arguments += @('-work-dir',$WorkDir) }
& (Join-Path $PSScriptRoot 'bin/sump-approval.exe') @arguments
exit $LASTEXITCODE
