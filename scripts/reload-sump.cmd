@echo off
rem SUMP reload: stop every SUMP process, then start the stack again.
rem Double-click entry for scripts\reload-sump.ps1 (pass -DryRun to only preview).
rem Kept ASCII-only on purpose: cmd.exe reads .cmd in the OEM code page,
rem so Chinese here would be mojibake. Chinese output comes from the
rem PowerShell script after chcp 65001 switches the console to UTF-8.
chcp 65001 >nul
title SUMP reload
cd /d "%~dp0.."
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0reload-sump.ps1" %*
echo.
echo [SUMP reload finished] Press any key to close this window...
pause >nul
