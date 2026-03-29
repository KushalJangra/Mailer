@echo off
setlocal
cd /d "%~dp0"

sendemails.exe
set EXITCODE=%ERRORLEVEL%

echo.
if %EXITCODE% neq 0 (
  echo Send failed — see the error above.
) else (
  echo Done.
)
pause
exit /b %EXITCODE%
