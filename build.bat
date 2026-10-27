@echo off
rem Build restreminder.exe (Windows amd64).
rem Requires Go 1.20+. See go.mod for why the language version is pinned.
setlocal
set GOOS=windows
set GOARCH=amd64
go build -trimpath -ldflags="-s -w" -o restreminder.exe .
if errorlevel 1 (
    echo.
    echo [FAILED] build error, see output above.
    pause
    exit /b 1
)
echo.
echo [OK] restreminder.exe generated.
