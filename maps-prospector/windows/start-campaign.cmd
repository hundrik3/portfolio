@echo off
setlocal
chcp 65001 >nul
cd /d "%~dp0"
maps-prospector.exe --cities cities.csv --scan-interval 1h --send --daily-limit 10 --send-interval 5m
set "MAPS_EXIT_CODE=%ERRORLEVEL%"
pause
exit /b %MAPS_EXIT_CODE%
