<#
.SYNOPSIS
    Builds the TaskView Windows application and its Inno Setup installer.

.DESCRIPTION
    The script produces two artifacts in dist\:

        TaskView.exe                              the GUI application
        TaskView-Setup-<version>-windows-amd64.exe the per-user installer

    The application is a single static binary (CGO disabled). The Windows
    resources (icon and application manifest) are embedded with rsrc, which is
    run through "go run" so that no tool has to be installed on the machine.

    The manifest is copied to a temporary file first: its assembly version is
    replaced with the version of this build, so the manifest, the program
    (main.version) and the installer always carry the same version.

    The Inno Setup compiler is looked up in this order:

        1. the environment variable ISCC_PATH
        2. the usual installation directories

.PARAMETER Version
    The version of the build, for example 2.0.0. It ends up in the installer
    name, in the program (main.version) and in the manifest.

.EXAMPLE
    pwsh -File ./scripts/build-windows.ps1
    pwsh -File ./scripts/build-windows.ps1 -Version 2.1.0
#>
[CmdletBinding()]
param(
    [string]$Version = '2.1.0'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if ($Version -notmatch '^\d+\.\d+\.\d+$') {
    throw "Version '$Version' is not a stable release version (expected X.Y.Z)."
}

$RepoRoot = Split-Path -Parent $PSScriptRoot
$DistDir = Join-Path $RepoRoot 'dist'
$ResourceDir = Join-Path $RepoRoot 'resources'
$IconFile = Join-Path $ResourceDir 'taskview.ico'
$ManifestFile = Join-Path $ResourceDir 'taskview.manifest'
$InstallerDir = Join-Path $RepoRoot 'installer'
$InstallerScript = Join-Path $InstallerDir 'taskview.iss'
$AppExe = Join-Path $DistDir 'TaskView.exe'
$SetupExe = Join-Path $DistDir "TaskView-Setup-$Version-windows-amd64.exe"
$ChecksumFile = Join-Path $DistDir 'SHA256SUMS'

# The resource file has to live in the package directory, otherwise "go build"
# does not pick it up. It is removed again when the build ends.
$SysoFile = Join-Path $RepoRoot 'taskview_windows_amd64.syso'

function Write-Step([string]$Message) {
    Write-Host "==> $Message" -ForegroundColor Cyan
}

function Resolve-Iscc {
    $candidates = @()
    if ($env:ISCC_PATH) {
        $candidates += $env:ISCC_PATH
    }
    $candidates += @(
        (Join-Path ${env:ProgramFiles(x86)} 'Inno Setup 6\ISCC.exe'),
        (Join-Path $env:ProgramFiles 'Inno Setup 6\ISCC.exe'),
        (Join-Path $env:LOCALAPPDATA 'Programs\Inno Setup 6\ISCC.exe')
    )
    foreach ($candidate in $candidates) {
        if ($candidate -and (Test-Path -LiteralPath $candidate)) {
            return (Resolve-Path -LiteralPath $candidate).Path
        }
    }
    throw @"
The Inno Setup 6 compiler was not found.
Install Inno Setup 6 or point ISCC_PATH at ISCC.exe, for example:
    `$env:ISCC_PATH = 'C:\Program Files (x86)\Inno Setup 6\ISCC.exe'
"@
}

function Invoke-Checked {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [Parameter(Mandatory = $true)][string]$WorkingDirectory
    )
    Write-Host "    $FilePath $($Arguments -join ' ')" -ForegroundColor DarkGray
    & $FilePath @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$FilePath failed with exit code $LASTEXITCODE."
    }
}

# The digest is computed with the .NET crypto provider so that the script does
# not depend on Get-FileHash, which is missing on some PowerShell installations.
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

Write-Step "Building TaskView $Version for windows/amd64"

if (-not (Test-Path -LiteralPath $IconFile)) {
    throw "Missing $IconFile. The Windows resources have to be generated first."
}
if (-not (Test-Path -LiteralPath $ManifestFile)) {
    throw "Missing $ManifestFile. The Windows resources have to be generated first."
}
if (-not (Test-Path -LiteralPath $InstallerScript)) {
    throw "Missing $InstallerScript."
}

$iscc = Resolve-Iscc
Write-Step "Inno Setup compiler: $iscc"

New-Item -ItemType Directory -Force -Path $DistDir | Out-Null

$tempManifest = Join-Path ([System.IO.Path]::GetTempPath()) ("taskview-manifest-" + [guid]::NewGuid().ToString('N') + '.xml')
$tempDir = $null
try {
    # The assembly version of the manifest follows the version of the build.
    $manifestText = Get-Content -LiteralPath $ManifestFile -Raw
    $updated = [regex]::Replace(
        $manifestText,
        '(<assemblyIdentity\b[^>]*?\bversion=")[^"]*(")',
        ('${1}' + $Version + '.0${2}'))
    if ($updated -eq $manifestText -and $manifestText -notmatch [regex]::Escape($Version)) {
        Write-Host "    (the manifest carries no assembly version, it was left unchanged)" -ForegroundColor DarkGray
    }
    Set-Content -LiteralPath $tempManifest -Value $updated -Encoding UTF8 -NoNewline

    # The resource file has to be in the package directory for "go build".
    if (Test-Path -LiteralPath $SysoFile) {
        Remove-Item -LiteralPath $SysoFile -Force
    }

    Write-Step "Embedding the Windows resources"
    Push-Location $RepoRoot
    try {
        Invoke-Checked -FilePath 'go' -WorkingDirectory $RepoRoot -Arguments @(
            'run', 'github.com/akavel/rsrc@v0.10.2',
            '-ico', $IconFile,
            '-manifest', $tempManifest,
            '-o', $SysoFile
        )
    }
    finally {
        Pop-Location
    }

    Write-Step "Compiling the console CLI"
    $CliExe = Join-Path $DistDir 'taskview-cli.exe'
    Push-Location $RepoRoot
    try {
        Invoke-Checked -FilePath 'go' -WorkingDirectory $RepoRoot -Arguments @(
            'build',
            '-trimpath',
            '-ldflags', "-s -w -X main.version=$Version",
            '-o', $CliExe,
            './cmd/taskview-cli'
        )
    }
    finally {
        Pop-Location
    }

    Write-Step "Compiling the application"
    $env:CGO_ENABLED = '0'
    Push-Location $RepoRoot
    try {
        Invoke-Checked -FilePath 'go' -WorkingDirectory $RepoRoot -Arguments @(
            'build',
            '-trimpath',
            '-ldflags', "-s -w -H=windowsgui -X main.version=$Version",
            '-o', $AppExe,
            '.'
        )
    }
    finally {
        Pop-Location
    }

    Write-Step "Compiling the installer"
    Push-Location $InstallerDir
    try {
        Invoke-Checked -FilePath $iscc -WorkingDirectory $InstallerDir -Arguments @(
            "/DMyAppVersion=$Version",
            'taskview.iss'
        )
    }
    finally {
        Pop-Location
    }
}
finally {
    # The temporary resource file never survives the build.
    if (Test-Path -LiteralPath $SysoFile) {
        Remove-Item -LiteralPath $SysoFile -Force -ErrorAction SilentlyContinue
    }
    if (Test-Path -LiteralPath $tempManifest) {
        Remove-Item -LiteralPath $tempManifest -Force -ErrorAction SilentlyContinue
    }
    if ($tempDir -and (Test-Path -LiteralPath $tempDir)) {
        Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

if (-not (Test-Path -LiteralPath $SetupExe)) {
    throw "The installer was not produced at $SetupExe."
}

# The Inno Setup compiler leaves a backup copy of every file it compresses
# next to the original. They are build leftovers and are removed here.
Get-ChildItem -LiteralPath $DistDir -Filter '*~' -ErrorAction SilentlyContinue |
    Remove-Item -Force -ErrorAction SilentlyContinue

Write-Step "Writing the checksum list"
$hash = Get-Sha256Hex -Path $SetupExe
$installerLeaf = Split-Path -Leaf $SetupExe
$existing = @()
if (Test-Path -LiteralPath $ChecksumFile) {
    $existing = (Get-Content -LiteralPath $ChecksumFile) | Where-Object { $_ -and -not $_.StartsWith('#') }
}
$allLines = @()
foreach ($line in $existing) {
    $fileName = ($line -split '\s+')[1]
    if ($fileName -ne $installerLeaf) {
        $allLines += $line
    }
}
$allLines += "$hash  $installerLeaf"
$allLines | Set-Content -LiteralPath $ChecksumFile -Encoding ASCII

Write-Step "Done"
Get-ChildItem -LiteralPath $DistDir |
    Where-Object { $_.Name -like "TaskView-Setup-$Version*" -or $_.Name -eq 'SHA256SUMS' -or $_.Name -eq 'TaskView.exe' } |
    ForEach-Object {
        Write-Host ("    {0}  ({1:N0} bytes)" -f $_.Name, $_.Length)
    }
Write-Host "    SHA-256: $hash" -ForegroundColor DarkGray
