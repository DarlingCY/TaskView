<#
.SYNOPSIS
    Builds the multi-platform TaskView CLI binaries.

.DESCRIPTION
    Builds taskview-cli for:
      - windows/amd64, windows/arm64
      - linux/amd64, linux/arm64
      - darwin/amd64, darwin/arm64

    Archives are placed in dist/ and checksums are recorded in dist/SHA256SUMS.
    The same binaries are bundled in skills/task-view/scripts/<os>-<arch>/,
    with a self-contained taskview-skill-<version>.zip in dist/.
#>
[CmdletBinding()]
param(
    [string]$Version = '2.1.0'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$RepoRoot = Split-Path -Parent $PSScriptRoot
$DistDir = Join-Path $RepoRoot 'dist'
$ChecksumFile = Join-Path $DistDir 'SHA256SUMS'
$SkillDir = Join-Path $RepoRoot 'skills/task-view'

New-Item -ItemType Directory -Force -Path $DistDir | Out-Null

function Get-Sha256Hex {
    param([Parameter(Mandatory = $true)][string]$Path)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open,
            [System.IO.FileAccess]::Read, [System.IO.FileShare]::Read)
        try {
            $bytes = $sha.ComputeHash($stream)
        }
        finally {
            $stream.Dispose()
        }
    }
    finally {
        $sha.Dispose()
    }
    return ([System.BitConverter]::ToString($bytes)).Replace('-', '').ToLowerInvariant()
}

$targets = @(
    @{ os = 'windows'; arch = 'amd64'; ext = '.exe' },
    @{ os = 'windows'; arch = 'arm64'; ext = '.exe' },
    @{ os = 'linux';   arch = 'amd64'; ext = '' },
    @{ os = 'linux';   arch = 'arm64'; ext = '' },
    @{ os = 'darwin';  arch = 'amd64'; ext = '' },
    @{ os = 'darwin';  arch = 'arm64'; ext = '' }
)

$env:CGO_ENABLED = '0'

$stageDir = Join-Path $DistDir 'cli-staging'
if (Test-Path -LiteralPath $stageDir) {
    Remove-Item -Recurse -Force -LiteralPath $stageDir
}
New-Item -ItemType Directory -Force -Path $stageDir | Out-Null

$entries = @()

foreach ($t in $targets) {
    $goos = $t.os
    $goarch = $t.arch
    $binName = "taskview-cli$($t.ext)"
    $targetDir = Join-Path $stageDir "$goos-$goarch"
    New-Item -ItemType Directory -Force -Path $targetDir | Out-Null
    $binPath = Join-Path $targetDir $binName

    Write-Host "==> Compiling taskview-cli for $goos/$goarch" -ForegroundColor Cyan
    $env:GOOS = $goos
    $env:GOARCH = $goarch

    Push-Location $RepoRoot
    try {
        & 'go' build -trimpath -ldflags "-s -w -X main.version=$Version" -o $binPath ./cmd/taskview-cli
        if ($LASTEXITCODE -ne 0) {
            throw "Compilation failed for $goos/$goarch with exit code $LASTEXITCODE"
        }
    }
    finally {
        Pop-Location
    }

    # If windows amd64, also save to dist/taskview-cli.exe
    if ($goos -eq 'windows' -and $goarch -eq 'amd64') {
        Copy-Item -LiteralPath $binPath -Destination (Join-Path $DistDir 'taskview-cli.exe') -Force
    }

    if ($goos -eq 'windows') {
        $archiveName = "taskview-cli-$Version-$goos-$goarch.zip"
        $archivePath = Join-Path $DistDir $archiveName
        if (Test-Path -LiteralPath $archivePath) { Remove-Item -LiteralPath $archivePath -Force }
        Compress-Archive -Path $binPath -DestinationPath $archivePath -CompressionLevel Optimal
        $hash = Get-Sha256Hex -Path $archivePath
        $entries += "$hash  $archiveName"
    } else {
        $archiveName = "taskview-cli-$Version-$goos-$goarch.tar.gz"
        $archivePath = Join-Path $DistDir $archiveName
        if (Test-Path -LiteralPath $archivePath) { Remove-Item -LiteralPath $archivePath -Force }
        tar -czf $archivePath -C $targetDir $binName
        if ($LASTEXITCODE -ne 0) {
            throw "tar failed for $goos/$goarch with exit code $LASTEXITCODE"
        }
        $hash = Get-Sha256Hex -Path $archivePath
        $entries += "$hash  $archiveName"
    }

    $skillBinDir = Join-Path $SkillDir "scripts/$goos-$goarch"
    New-Item -ItemType Directory -Force -Path $skillBinDir | Out-Null
    Copy-Item -LiteralPath $binPath -Destination (Join-Path $skillBinDir $binName) -Force
}

$skillArchiveName = "taskview-skill-$Version.zip"
$skillArchivePath = Join-Path $DistDir $skillArchiveName
if (Test-Path -LiteralPath $skillArchivePath) { Remove-Item -LiteralPath $skillArchivePath -Force }
# PowerShell 5.1 Compress-Archive writes backslash paths that Unix unzip treats
# as literal names, so use explicit ZIP-standard forward-slash entries.
Add-Type -AssemblyName System.IO.Compression.FileSystem
$skillArchive = [System.IO.Compression.ZipFile]::Open($skillArchivePath, [System.IO.Compression.ZipArchiveMode]::Create)
try {
    foreach ($file in Get-ChildItem -LiteralPath $SkillDir -Recurse -File) {
        $entryName = 'task-view/' + $file.FullName.Substring($SkillDir.Length + 1).Replace('\', '/')
        [System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile(
            $skillArchive, $file.FullName, $entryName,
            [System.IO.Compression.CompressionLevel]::Optimal) | Out-Null
    }
}
finally {
    $skillArchive.Dispose()
}
$entries += "$(Get-Sha256Hex -Path $skillArchivePath)  $skillArchiveName"

Remove-Item -Recurse -Force -LiteralPath $stageDir -ErrorAction SilentlyContinue

# Append checksums while preserving any existing lines
$existing = @()
if (Test-Path -LiteralPath $ChecksumFile) {
    $existing = (Get-Content -LiteralPath $ChecksumFile) | Where-Object { $_ -and -not $_.StartsWith('#') }
}

$allLines = @()
# Keep existing lines that are not overwritten
foreach ($line in $existing) {
    $fileName = ($line -split '\s+')[1]
    $overwritten = $false
    foreach ($entry in $entries) {
        $newFile = ($entry -split '\s+')[1]
        if ($newFile -eq $fileName) {
            $overwritten = $true
            break
        }
    }
    if (-not $overwritten) {
        $allLines += $line
    }
}
$allLines += $entries
$allLines | Set-Content -LiteralPath $ChecksumFile -Encoding ASCII

Write-Host "Multi-platform CLI build finished successfully." -ForegroundColor Green
