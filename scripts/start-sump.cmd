@echo off
rem SUMP launcher: double-click entry for scripts\run-local.ps1.
rem Kept ASCII-only on purpose: cmd.exe parses .cmd files with the OEM code page,
rem so Chinese text here would be mojibake. All Chinese output comes from the
rem PowerShell script, after `chcp 65001` switches the console to UTF-8.
chcp 65001 >nul
title SUMP
cd /d "D:\agent\SUMP\2"
powershell -NoProfile -ExecutionPolicy Bypass -File "D:\agent\SUMP\2\scripts\run-local.ps1" %*
echo.
echo [SUMP stopped] Press any key to close this window...
pause >nul
