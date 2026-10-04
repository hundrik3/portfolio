@echo off
setlocal
chcp 65001 >nul
cd /d "%~dp0"
maps-prospector.exe --login vk
set "MAPS_EXIT_CODE=%ERRORLEVEL%"
pause
exit /b %MAPS_EXIT_CODE%
