@echo off
setlocal
chcp 65001 >nul
cd /d "%~dp0"
maps-prospector.exe --refresh-cities cities.csv
set "MAPS_EXIT_CODE=%ERRORLEVEL%"
pause
exit /b %MAPS_EXIT_CODE%
