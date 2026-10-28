# Builds cove.exe (CLI), CoveTray.exe (tray + background service) and CoveApp.exe (desktop window).
# Binaries are not symbol-stripped: anonymous stripped Go binaries tend to trip
# antivirus ML heuristics. If go-winres is installed, a version resource,
# manifest and icon are embedded too.
#
#   .\build.ps1            build into dist\
#   .\build.ps1 -Release   also publish dist\cove-windows-x64.zip as GitHub release v<VERSION>
#                          (needs gh; bump VERSION first — installs pick it up with `cove update`)
param([switch]$Release)
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot
New-Item -ItemType Directory -Force dist | Out-Null
$env:CGO_ENABLED = '0'
$version = (Get-Content VERSION -Raw).Trim()
$ld = "-X main.appVersion=$version"

Remove-Item rsrc_windows_*.syso -ErrorAction SilentlyContinue
if (Get-Command go-winres -ErrorAction SilentlyContinue) {
    go build -o dist\icon-gen.exe .
    & .\dist\icon-gen.exe __icon dist\icon.png
    Remove-Item dist\icon-gen.exe
    go-winres simply --arch amd64 --icon dist\icon.png --manifest gui `
        --product-name Cove --file-description "Cove local PHP development environment" `
        --product-version $version --file-version $version --copyright "Cove contributors" --original-filename cove.exe
}

go build -trimpath -ldflags "$ld" -o dist\cove.exe .
go build -trimpath -ldflags "-H windowsgui -X main.guiBuild=1 $ld" -o dist\CoveTray.exe .
go build -trimpath -ldflags "-H windowsgui -X main.guiBuild=app $ld" -o dist\CoveApp.exe .
Get-ChildItem dist\*.exe | Select-Object Name, @{n='MB';e={[math]::Round($_.Length/1MB,1)}}

if ($Release) {
    $zip = "dist\cove-windows-x64.zip"
    Remove-Item $zip -ErrorAction SilentlyContinue
    Compress-Archive -Path dist\cove.exe, dist\CoveTray.exe, dist\CoveApp.exe -DestinationPath $zip
    gh release create "v$version" $zip --title "Cove $version" --generate-notes
    if ($LASTEXITCODE -ne 0) { throw "gh release create failed" }
}
