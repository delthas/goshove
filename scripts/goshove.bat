@echo off
cd /d %~dp0
title goshove
set GTK_CSD=0
set GTK_THEME=win32
goshove.exe
if %errorlevel% equ 0 (exit) else (
pause > nul
)
