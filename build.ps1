# Builds cove.exe (CLI), CoveTray.exe (tray + background service) and CoveApp.exe (desktop window).
# Binaries are not symbol-stripped: anonymous stripped Go binaries tend to trip
# antivirus ML heuristics. If go-winres is installed, a version resource,
# manifest and icon are embedded too.
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot
New-Item -ItemType Directory -Force dist | Out-Null
$env:CGO_ENABLED = '0'
$version = '1.0.0'

Remove-Item rsrc_windows_*.syso -ErrorAction SilentlyContinue
if (Get-Command go-winres -ErrorAction SilentlyContinue) {
    go build -o dist\icon-gen.exe .
    & .\dist\icon-gen.exe __icon dist\icon.png
    Remove-Item dist\icon-gen.exe
    go-winres simply --arch amd64 --icon dist\icon.png --manifest gui `
        --product-name Cove --file-description "Cove local PHP development environment" `
        --product-version $version --file-version $version --copyright "Cove contributors" --original-filename cove.exe
}

go build -trimpath -o dist\cove.exe .
go build -trimpath -ldflags "-H windowsgui -X main.guiBuild=1" -o dist\CoveTray.exe .
go build -trimpath -ldflags "-H windowsgui -X main.guiBuild=app" -o dist\CoveApp.exe .
Get-ChildItem dist\*.exe | Select-Object Name, @{n='MB';e={[math]::Round($_.Length/1MB,1)}}
