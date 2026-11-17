# wingetパッケージ用ビルドスクリプト
$ErrorActionPreference = "Stop"
$appname = "coe"

# The version lives in main.go only, so the binary and the archive names
# cannot disagree.
$mainSource = Get-Content -Raw (Join-Path $PSScriptRoot "main.go")
if ($mainSource -notmatch '(?m)^\s*(?:var\s+)?version\s*=\s*"([^"]+)"') {
    throw "version not found in main.go"
}
$version = $Matches[1]

# Every archive entry gets the time of the last commit, so rebuilding the
# same commit produces byte-identical archives and the same SHA-256.
$mtime = 315532800
if (Get-Command git -ErrorAction SilentlyContinue) {
    $commitTime = git -C $PSScriptRoot log -1 --format=%ct 2>$null
    if ($LASTEXITCODE -eq 0 -and $commitTime) {
        $mtime = [long]$commitTime
    }
}

# Sets (or, for $null, removes) an environment variable. Passing $null to
# SetEnvironmentVariable would set an empty variable on .NET 9 and later.
function Set-GoEnvVar([string]$Name, $Value) {
    if ($null -eq $Value) {
        Remove-Item "Env:$Name" -ErrorAction SilentlyContinue
    } else {
        Set-Item "Env:$Name" $Value
    }
}

# Runs $Script with the given environment variables ($null removes one) and
# then restores the caller's values, so the script never leaves GOOS/GOARCH
# changed in the user's session.
function Invoke-WithGoEnv {
    param(
        [Parameter(Mandatory = $true)][hashtable]$Vars,
        [Parameter(Mandatory = $true)][scriptblock]$Script
    )

    $saved = @{}
    foreach ($name in $Vars.Keys) {
        $saved[$name] = [Environment]::GetEnvironmentVariable($name)
    }
    try {
        foreach ($name in $Vars.Keys) {
            Set-GoEnvVar $name $Vars[$name]
        }
        & $Script
    }
    finally {
        foreach ($name in $saved.Keys) {
            Set-GoEnvVar $name $saved[$name]
        }
    }
}

function Invoke-CoeBuild {
    param(
        [Parameter(Mandatory = $true)][string]$GoOS,
        [Parameter(Mandatory = $true)][string]$GoArch,
        [Parameter(Mandatory = $true)][string]$OutputFile
    )

    Invoke-WithGoEnv @{ GOOS = $GoOS; GOARCH = $GoArch; CGO_ENABLED = "0" } {
        # Build the package, not main.go alone: the program spans several files.
        go build -trimpath -o $OutputFile -ldflags "-s -w" .
        if ($LASTEXITCODE -ne 0) {
            throw "go build failed: $OutputFile (exit $LASTEXITCODE)"
        }
    }
}

function New-ReleaseZip {
    param(
        [Parameter(Mandatory = $true)][string]$ZipPath,
        [Parameter(Mandatory = $true)][string]$Executable,
        [Parameter(Mandatory = $true)][string[]]$Files
    )

    if (Test-Path $ZipPath) {
        Remove-Item $ZipPath -Force
    }
    # Compress-Archive drops Unix permissions; mkzip stores the binary as 0755.
    & $script:mkzip -o $ZipPath -mtime $mtime -exec $Executable @Files
    if ($LASTEXITCODE -ne 0) {
        throw "mkzip failed: $ZipPath (exit $LASTEXITCODE)"
    }
}

Push-Location $PSScriptRoot
try {
    $releaseDir = "release"
    if (!(Test-Path $releaseDir)) {
        New-Item -ItemType Directory -Path $releaseDir -Force | Out-Null
    }

    # The archive tool runs here, so build it for this machine whatever
    # GOOS/GOARCH the caller has set.
    New-Item -ItemType Directory -Path "build" -Force | Out-Null
    $script:mkzip = Join-Path (Resolve-Path "build") "mkzip"
    if ($IsWindows -or $env:OS -eq "Windows_NT") {
        $script:mkzip += ".exe"
    }
    Invoke-WithGoEnv @{ GOOS = $null; GOARCH = $null } {
        go build -o $script:mkzip ./tools/mkzip
        if ($LASTEXITCODE -ne 0) {
            throw "building tools/mkzip failed (exit $LASTEXITCODE)"
        }
    }

    $targets = @(
        @{ OS = "windows"; Arch = "amd64" },
        @{ OS = "windows"; Arch = "arm64" },
        @{ OS = "linux"; Arch = "amd64" },
        @{ OS = "linux"; Arch = "arm64" },
        @{ OS = "darwin"; Arch = "amd64" },
        @{ OS = "darwin"; Arch = "arm64" }
    )

    foreach ($t in $targets) {
        $target = "$($t.OS)_$($t.Arch)"
        $buildDir = "build/$appname" + "_$target"
        $exeName = if ($t.OS -eq "windows") { "$appname.exe" } else { $appname }
        $outputFile = "$buildDir/$exeName"

        Write-Host "Building for $target..." -ForegroundColor Green
        if (Test-Path $buildDir) {
            Remove-Item $buildDir -Recurse -Force
        }
        New-Item -ItemType Directory -Path $buildDir -Force | Out-Null

        Invoke-CoeBuild -GoOS $t.OS -GoArch $t.Arch -OutputFile $outputFile

        $files = @($outputFile)
        if ($t.OS -eq "windows") {
            Copy-Item "installer.ps1" "$buildDir/"
            $files += "$buildDir/installer.ps1"
        }

        $zipPath = "$releaseDir/$appname" + "_$version" + "_$target.zip"
        Write-Host "Creating ZIP: $zipPath" -ForegroundColor Yellow
        New-ReleaseZip -ZipPath $zipPath -Executable $exeName -Files $files

        $hash = Get-FileHash -Path $zipPath -Algorithm SHA256
        Write-Host "SHA256 for $target`: $($hash.Hash)" -ForegroundColor Cyan
    }

    Write-Host "Build completed! Files are in the $releaseDir directory." -ForegroundColor Green
}
finally {
    Pop-Location
}
