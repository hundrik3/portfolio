@echo off
setlocal
chcp 65001 >nul
cd /d "%~dp0"
powershell -NoProfile -Command "$vkTestGroup = Read-Host 'HTTPS URL of your OWN test VK community'; & '.\maps-prospector.exe' --test-vk-group $vkTestGroup; exit $LASTEXITCODE"
set "MAPS_EXIT_CODE=%ERRORLEVEL%"
pause
exit /b %MAPS_EXIT_CODE%
