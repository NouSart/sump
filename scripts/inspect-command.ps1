param([Parameter(Mandatory=$true)][string]$CommandText,[string]$WorkDir=$PSScriptRoot)
$ErrorActionPreference='Stop'
& (Join-Path (Split-Path -Parent $PSScriptRoot) 'bin/sump-approval.exe') inspect -command $CommandText -work-dir $WorkDir
exit $LASTEXITCODE
