# install.ps1 -- build tasks-cli and install it to the one place Windows looks.
#
# The README used to say `go build -o tasks-cli.exe .\cmd\tasks-cli`, which
# leaves a second binary beside the source. That copy is on nobody's PATH but
# is easy to run by accident, and it ages silently while the installed one
# moves on. Build through this script instead: there is exactly one installed
# binary per host, and `tasks-cli version` reports the commit it came from.

[CmdletBinding()]
param(
    [string]$Destination = 'C:\Tools\tasks-cli.exe',
    [switch]$SkipTests
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
Push-Location $repo
try {
    if (-not $SkipTests) {
        Write-Host 'running tests...'
        & go test ./...
        if ($LASTEXITCODE -ne 0) { throw 'tests failed; not installing' }
    }

    $commit = (& git rev-parse --short HEAD).Trim()
    if ((& git status --porcelain)) { $commit += '-dirty' }
    $built = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    $ldflags = "-X main.buildCommit=$commit -X main.buildTime=$built"

    # Build to a temp file first: overwriting a running binary fails on Windows,
    # and a failed build must never leave a half-written exe on PATH.
    $staging = Join-Path ([System.IO.Path]::GetTempPath()) 'tasks-cli.build.exe'
    & go build -ldflags $ldflags -o $staging ./cmd/tasks-cli
    if ($LASTEXITCODE -ne 0) { throw 'build failed' }

    $destDir = Split-Path -Parent $Destination
    if (-not (Test-Path -LiteralPath $destDir)) { New-Item -ItemType Directory -Path $destDir -Force | Out-Null }

    # Move-Item -Force does not reliably clobber an existing destination on
    # Windows PowerShell -- it can still fail with "file already exists". Remove
    # first, and retry: the old binary may be momentarily held by a scheduled
    # sync tick or an agent shelling out to it.
    for ($attempt = 1; ; $attempt++) {
        try {
            if (Test-Path -LiteralPath $Destination) { Remove-Item -LiteralPath $Destination -Force }
            Move-Item -LiteralPath $staging -Destination $Destination
            break
        } catch {
            if ($attempt -ge 5) { throw "could not install to ${Destination}: $($_.Exception.Message)" }
            Start-Sleep -Seconds 1
        }
    }

    Write-Host "installed $commit -> $Destination"
    & $Destination version
} finally {
    Pop-Location
}
