@echo off
setlocal
chcp 65001 >nul
cd /d "%~dp0"
powershell -NoProfile -Command "$vkInput = Read-Host 'Personal VK application token (messages permission)' -AsSecureString; $vkPointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($vkInput); try { $env:VK_ACCESS_TOKEN = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($vkPointer); & '.\maps-prospector.exe' --check-vk; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }; if (Test-Path -LiteralPath '.\cities.csv') { & '.\maps-prospector.exe' --vk-backend api --cities cities.csv --scan-interval 1h --send --daily-limit 10 --send-interval 5m } else { & '.\maps-prospector.exe' --vk-backend api --send --daily-limit 10 --send-interval 5m }; exit $LASTEXITCODE } finally { Remove-Item Env:VK_ACCESS_TOKEN -ErrorAction SilentlyContinue; [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($vkPointer) }"
set "MAPS_EXIT_CODE=%ERRORLEVEL%"
pause
exit /b %MAPS_EXIT_CODE%
