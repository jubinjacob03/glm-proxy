#Requires -Version 5.1
<#
.SYNOPSIS
    One entry point for every local task: run from source, build the Windows
    installer, or regenerate the icon.

.EXAMPLE
    .\make.ps1 run          # build the binaries and start the proxy
    .\make.ps1 installer    # produce installer\out\GLM-Proxy-Setup.exe
    .\make.ps1 icon         # regenerate cmd\glm-tray\icon.ico from the Z.AI logo

.NOTES
    Docker needs none of this — `docker compose up -d` builds and runs the
    container on its own.
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [ValidateSet('run', 'installer', 'icon')]
    [string]$Task = 'run',

    [switch]$Rebuild
)

$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot

# --- Go toolchain ------------------------------------------------------------
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    $goBin = 'C:\Program Files\Go\bin'
    if (Test-Path $goBin) { $env:Path = "$goBin;$env:Path" }
    else { throw 'Go toolchain not found. Install it (winget install GoLang.Go) and retry.' }
}

$ldflags = '-s -w'

# The installer script is the single source of truth for the version; the tray
# needs it compiled in so auto-update can compare against GitHub releases.
function Get-AppVersion {
    $nsi = Join-Path $root 'installer\installer.nsi'
    if (-not (Test-Path $nsi)) { return '0.0.0' }
    $m = [regex]::Match((Get-Content $nsi -Raw), '!define\s+APP_VERSION\s+"([^"]+)"')
    if ($m.Success) { return $m.Groups[1].Value }
    return '0.0.0'
}

function Invoke-GoBuild {
    param([string]$Output, [string]$Package, [string]$ExtraLd = '')
    Write-Host "  building $([System.IO.Path]::GetFileName($Output))" -ForegroundColor DarkGray
    $ld = ("$ldflags $ExtraLd").Trim()
    $previousCgoEnabled = [Environment]::GetEnvironmentVariable('CGO_ENABLED', 'Process')
    try {
        [Environment]::SetEnvironmentVariable('CGO_ENABLED', '0', 'Process')
        go build -trimpath "-ldflags=$ld" -o $Output $Package
        if ($LASTEXITCODE) { throw "build failed: $Package" }
    }
    finally {
        [Environment]::SetEnvironmentVariable('CGO_ENABLED', $previousCgoEnabled, 'Process')
    }
}

# ============================================================================
# icon — rasterize the official Z.AI logo into a multi-size .ico
# ============================================================================
function Build-Icon {
    param([string]$OutFile = (Join-Path $root 'cmd\glm-tray\icon.ico'))

    Add-Type -AssemblyName System.Drawing
    $svg = Join-Path $root 'installer\logo.svg'
    if (-not (Test-Path $svg)) {
        $ProgressPreference = 'SilentlyContinue'
        Invoke-WebRequest 'https://z-cdn.chatglm.cn/z-ai/static/logo.svg' -OutFile $svg -UseBasicParsing -TimeoutSec 30
    }

    $browser = $null
    foreach ($c in @(
            "$env:ProgramFiles\Google\Chrome\Application\chrome.exe",
            "${env:ProgramFiles(x86)}\Google\Chrome\Application\chrome.exe",
            "$env:ProgramFiles\Microsoft\Edge\Application\msedge.exe",
            "${env:ProgramFiles(x86)}\Microsoft\Edge\Application\msedge.exe")) {
        if (Test-Path $c) { $browser = $c; break }
    }
    if (-not $browser) { throw 'Chrome or Edge is required to rasterize the SVG.' }

    $sizes = @(16, 20, 24, 32, 40, 48, 64, 128, 256)
    $work = Join-Path ([System.IO.Path]::GetTempPath()) ("glm-icon-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Force -Path $work | Out-Null
    try {
        $svgUri = ([uri]$svg).AbsoluteUri
        $pngs = @{}
        foreach ($s in $sizes) {
            $html = "<!doctype html><meta charset=utf-8><style>html,body{margin:0;background:transparent}img{width:${s}px;height:${s}px;display:block}</style><img src='$svgUri'>"
            $htmlPath = Join-Path $work "w$s.html"
            [System.IO.File]::WriteAllText($htmlPath, $html, (New-Object System.Text.UTF8Encoding($false)))
            $shot = Join-Path $work "i$s.png"
            $args = @('--headless=new', '--disable-gpu', '--hide-scrollbars',
                '--default-background-color=00000000', '--force-device-scale-factor=1',
                '--no-first-run', '--no-default-browser-check',
                "--user-data-dir=$work\p$s", "--screenshot=$shot", "--window-size=$s,$s",
                ([uri]$htmlPath).AbsoluteUri)
            $prev = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
            try { & $browser @args 2>&1 | Out-Null } finally { $ErrorActionPreference = $prev }
            if (-not (Test-Path $shot)) { throw "Chrome produced no screenshot at ${s}px" }
            $img = [System.Drawing.Image]::FromFile($shot)
            try {
                $bmp = New-Object System.Drawing.Bitmap($s, $s, [System.Drawing.Imaging.PixelFormat]::Format32bppArgb)
                $g = [System.Drawing.Graphics]::FromImage($bmp)
                $g.InterpolationMode = 'HighQualityBicubic'; $g.Clear([System.Drawing.Color]::Transparent)
                $g.DrawImage($img, 0, 0, $s, $s); $g.Dispose()
                $ms = New-Object System.IO.MemoryStream
                $bmp.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
                $pngs[$s] = $ms.ToArray(); $ms.Dispose(); $bmp.Dispose()
            } finally { $img.Dispose() }
        }
        $out = New-Object System.IO.MemoryStream
        $bw = New-Object System.IO.BinaryWriter($out)
        $bw.Write([UInt16]0); $bw.Write([UInt16]1); $bw.Write([UInt16]$sizes.Count)
        $offset = 6 + (16 * $sizes.Count)
        foreach ($s in $sizes) {
            $d = $pngs[$s]; $dim = if ($s -ge 256) { 0 } else { $s }
            $bw.Write([Byte]$dim); $bw.Write([Byte]$dim); $bw.Write([Byte]0); $bw.Write([Byte]0)
            $bw.Write([UInt16]1); $bw.Write([UInt16]32); $bw.Write([UInt32]$d.Length); $bw.Write([UInt32]$offset)
            $offset += $d.Length
        }
        foreach ($s in $sizes) { $bw.Write($pngs[$s]) }
        $bw.Flush()
        New-Item -ItemType Directory -Force -Path (Split-Path $OutFile) | Out-Null
        [System.IO.File]::WriteAllBytes($OutFile, $out.ToArray())
        $bw.Dispose(); $out.Dispose()
        Write-Host "  wrote $OutFile" -ForegroundColor DarkGray
    } finally {
        Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
    }
}

# ============================================================================
# run — build the binaries and start the proxy from source
# ============================================================================
function Invoke-Run {
    Push-Location $root
    try {
        Write-Host 'Building...' -ForegroundColor Cyan
        # The collector must exist on disk so the proxy's token monitor can run
        # it; the proxy loads .env and seeds the token store itself.
        Invoke-GoBuild -Output (Join-Path $root 'token-collector.exe') -Package './cmd/token-collector'
        Invoke-GoBuild -Output (Join-Path $root 'zai-api.exe') -Package '.'
        if (-not (Test-Path (Join-Path $root '.env'))) {
            Write-Host 'No .env found; copy .env.example to .env and set ZAI_TOKEN.' -ForegroundColor Yellow
        }
        Write-Host 'Starting proxy (Ctrl+C to stop)...' -ForegroundColor Green
        & (Join-Path $root 'zai-api.exe')
    }
    finally { Pop-Location }
}

# ============================================================================
# installer — bundle everything into a single setup exe
# ============================================================================
function Invoke-Installer {
    $makensis = $null
    foreach ($c in @("${env:ProgramFiles(x86)}\NSIS\makensis.exe", "$env:ProgramFiles\NSIS\makensis.exe")) {
        if (Test-Path $c) { $makensis = $c; break }
    }
    if (-not $makensis) { throw 'makensis.exe not found. Install NSIS from https://nsis.sourceforge.io.' }

    $icon = Join-Path $root 'cmd\glm-tray\icon.ico'
    if ($Rebuild -or -not (Test-Path $icon)) {
        Write-Host 'Generating icon...' -ForegroundColor Cyan
        Build-Icon -OutFile $icon
    }

    $staging = Join-Path $root 'installer\staging'
    $out = Join-Path $root 'installer\out'
    Remove-Item -Recurse -Force $staging -ErrorAction SilentlyContinue
    New-Item -ItemType Directory -Force -Path $staging, $out | Out-Null

    $version = Get-AppVersion
    Push-Location $root
    try {
        Write-Host "Building binaries (version $version)..." -ForegroundColor Cyan
        Invoke-GoBuild -Output (Join-Path $staging 'zai-api.exe') -Package '.'
        Invoke-GoBuild -Output (Join-Path $staging 'token-collector.exe') -Package './cmd/token-collector'
        # -H=windowsgui: the tray runs with no console window of its own.
        # -X main.appVersion: lets auto-update compare against GitHub releases.
        Invoke-GoBuild -Output (Join-Path $staging 'glm-tray.exe') -Package './cmd/glm-tray' `
            -ExtraLd "-H=windowsgui -X main.appVersion=$version"
    }
    finally { Pop-Location }

    Write-Host 'Compiling installer with NSIS...' -ForegroundColor Cyan
    & $makensis (Join-Path $root 'installer\installer.nsi')
    if ($LASTEXITCODE) { throw 'makensis failed' }

    $setup = Join-Path $out 'GLM-Proxy-Setup.exe'
    if (-not (Test-Path $setup)) { throw "installer not produced at $setup" }
    $mb = [math]::Round((Get-Item $setup).Length / 1MB, 1)
    Write-Host ""
    Write-Host "Done: $setup ($mb MB)" -ForegroundColor Green
    Write-Host 'Ship that one file — it installs the proxy, collector, and tray, and autostarts on login.' -ForegroundColor Green
}

switch ($Task) {
    'run' { Invoke-Run }
    'installer' { Invoke-Installer }
    'icon' { Build-Icon; Write-Host 'Icon regenerated.' -ForegroundColor Green }
}
