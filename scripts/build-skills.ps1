#requires -Version 7.0
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3.0

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$skillDoc = Join-Path $repoRoot 'skills\taskview\SKILL.md'
$readmeDoc = Join-Path $repoRoot 'skills\taskview\README.md'
if (-not (Test-Path -LiteralPath $skillDoc) -or -not (Test-Path -LiteralPath $readmeDoc)) {
    throw "Required documentation missing: skills/taskview/SKILL.md or README.md"
}

$origCgo, $origGoos, $origGoarch = $env:CGO_ENABLED, $env:GOOS, $env:GOARCH
$tempBase = if ($env:LOCALAPPDATA -and (Test-Path -LiteralPath "$env:LOCALAPPDATA\Temp\opencode")) {
    "$env:LOCALAPPDATA\Temp\opencode"
} else {
    [System.IO.Path]::GetTempPath()
}
$tempRoot = Join-Path $tempBase "taskview-skill-$([System.Guid]::NewGuid())"
[System.IO.Directory]::CreateDirectory($tempRoot) | Out-Null

$targets = @(
    @{ OS = 'windows'; Arch = 'amd64'; Bin = 'taskview.exe';          IsPosix = $false },
    @{ OS = 'linux';   Arch = 'amd64'; Bin = 'taskview-linux-amd64';  IsPosix = $true },
    @{ OS = 'darwin';  Arch = 'arm64'; Bin = 'taskview-darwin-arm64'; IsPosix = $true }
)

Push-Location $repoRoot
try {
    foreach ($t in $targets) {
        Write-Host "Building TaskView for $($t.OS)/$($t.Arch)..."
        $targetDir = Join-Path $tempRoot "$($t.OS)-$($t.Arch)"
        [System.IO.Directory]::CreateDirectory($targetDir) | Out-Null
        $binPath = Join-Path $targetDir $t.Bin

        $env:CGO_ENABLED = '0'; $env:GOOS = $t.OS; $env:GOARCH = $t.Arch

        & go build -trimpath -ldflags='-s -w' -o $binPath .
        if ($LASTEXITCODE -ne 0) { throw "go build failed for $($t.OS)/$($t.Arch) (exit code: $LASTEXITCODE)" }
        if (-not (Test-Path -LiteralPath $binPath) -or (Get-Item -LiteralPath $binPath).Length -le 0) {
            throw "Expected binary was not created or is empty: $binPath"
        }
    }

    # All targets built and staged successfully; publish to skills/taskview/scripts/
    $skillScriptsDir = Join-Path $repoRoot 'skills\taskview\scripts'
    if (-not (Test-Path -LiteralPath $skillScriptsDir)) { [System.IO.Directory]::CreateDirectory($skillScriptsDir) | Out-Null }
    foreach ($t in $targets) {
        $srcBin = Join-Path (Join-Path $tempRoot "$($t.OS)-$($t.Arch)") $t.Bin
        $destBin = Join-Path $skillScriptsDir $t.Bin
        try {
            Copy-Item -LiteralPath $srcBin -Destination $destBin -Force
        } catch {
            throw "Failed to copy $destBin (if taskview server is running, stop it before rebuilding): $_"
        }
        if (-not $IsWindows -and $t.IsPosix) {
            & chmod +x $destBin
            if ($LASTEXITCODE -ne 0) { throw "chmod +x failed on $destBin (exit code: $LASTEXITCODE)" }
        }
        Write-Host "Published binary $destBin"
    }
} finally {
    $env:CGO_ENABLED, $env:GOOS, $env:GOARCH = $origCgo, $origGoos, $origGoarch
    Pop-Location
    if (Test-Path -LiteralPath $tempRoot) {
        Remove-Item -LiteralPath $tempRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}
