param(
    [string]$Version = $env:KNOT_VERSION,
    [string]$InstallDir = $env:KNOT_INSTALL_DIR,
    [string]$BaseUrl = $env:KNOT_BASE_URL
)

$ErrorActionPreference = 'Stop'

function Get-ReleaseFile($Asset, $Destination) {
    Invoke-WebRequest -Uri "$ReleaseUrl/$Asset" -OutFile $Destination -UseBasicParsing
}

function Get-VerifiedBinary($Asset, $Destination) {
    $NamePattern = [regex]::Escape($Asset)
    $Checksums = @(foreach ($Line in $ChecksumLines) {
        if ($Line -cmatch "^([0-9a-fA-F]{64})\s+\*?$NamePattern\s*$") {
            $Matches[1]
        }
    })
    if ($Checksums.Count -ne 1) {
        throw "Missing or ambiguous checksum for $Asset"
    }
    Write-Host "Downloading $Asset"
    Get-ReleaseFile $Asset $Destination
    if ((Get-FileHash -Algorithm SHA256 -LiteralPath $Destination).Hash -ne $Checksums[0]) {
        throw "Checksum mismatch for $Asset"
    }
}

function Install-Binaries {
    $Names = @('knot-core.exe', 'knot.exe')
    $Backup = $null
    $Moved = @()
    $Placed = @()
    $Complete = $false
    try {
        foreach ($Name in $Names) {
            $Target = Join-Path $InstallDir $Name
            if (Test-Path -LiteralPath $Target -PathType Container) {
                throw "Expected a binary at $Target"
            }
            if (Test-Path -LiteralPath $Target) {
                if (!$Backup) {
                    $Backup = Join-Path $InstallDir ('.knot-backup.' + [guid]::NewGuid().ToString('N'))
                    [IO.Directory]::CreateDirectory($Backup) | Out-Null
                }
                [IO.File]::Move($Target, (Join-Path $Backup $Name))
                $Moved += $Name
            }
        }
        foreach ($Name in $Names) {
            [IO.File]::Move((Join-Path $InstallStage $Name), (Join-Path $InstallDir $Name))
            $Placed += $Name
        }
        $Complete = $true
    } finally {
        if (!$Complete) {
            foreach ($Name in $Placed) {
                Remove-Item -LiteralPath (Join-Path $InstallDir $Name) -Force -ErrorAction Continue
            }
            foreach ($Name in $Moved) {
                try {
                    [IO.File]::Move((Join-Path $Backup $Name), (Join-Path $InstallDir $Name))
                } catch {
                    Write-Warning "Restore $Name from $Backup"
                }
            }
            if ($Backup -and @(Get-ChildItem -LiteralPath $Backup -Force).Count -eq 0) {
                [IO.Directory]::Delete($Backup)
            }
        }
    }
    Write-Host "Installed $CliExe and $CoreExe"
    if ($Backup) {
        Write-Host "Backed up previous binaries to $Backup"
    }
}

function Set-PrivateAcl($Path, [switch]$Directory) {
    $User = [Security.Principal.WindowsIdentity]::GetCurrent().User
    if ($Directory) {
        $Acl = [Security.AccessControl.DirectorySecurity]::new()
        $Rule = [Security.AccessControl.FileSystemAccessRule]::new(
            $User, 'FullControl', 'ContainerInherit, ObjectInherit', 'None', 'Allow')
    } else {
        $Acl = [Security.AccessControl.FileSecurity]::new()
        $Rule = [Security.AccessControl.FileSystemAccessRule]::new($User, 'FullControl', 'Allow')
    }
    $Acl.SetAccessRuleProtection($true, $false)
    $Acl.AddAccessRule($Rule)
    Set-Acl -LiteralPath $Path -AclObject $Acl
}

function ConvertTo-TomlPath([string]$Path) {
    return '"' + $Path.Replace('\', '\\').Replace('"', '\"') + '"'
}

# Preserve the TOML string expression when copying the secret to the client.
function Get-ConnectionBlock($SourceConfig) {
    $Port = 17898
    $Token = '""'
    $Service = $false
    $TableSeen = $false
    $ServiceSeen = $false
    $PortSeen = $false
    $TokenSeen = $false
    foreach ($Line in (Get-Content -LiteralPath $SourceConfig -Encoding UTF8)) {
        if ($Line -match '^\s*\[') {
            $TableSeen = $true
            $Service = $Line -cmatch '^\s*\[\s*(service|"service"|''service'')\s*\]\s*(#.*)?$'
            if (!$Service -and $Line -cmatch '^\s*\[+\s*(service|"service"|''service'')\s*[.\]]') {
                throw "Unsupported service settings in $CoreConfig"
            }
            if ($Service -and $ServiceSeen) {
                throw "Duplicate service settings in $CoreConfig"
            }
            $ServiceSeen = $ServiceSeen -or $Service
            continue
        }
        if (!$TableSeen -and $Line -cmatch '^\s*(service|"service"|''service'')\s*[.=]') {
            throw "Unsupported service settings in $CoreConfig"
        }
        if ($Service -and $Line -cmatch '^\s*(port|"port"|''port'')\s*=\s*(.*)$') {
            $Value = ($Matches[2] -replace '\s*(#.*)?$', '') -replace '^\+', ''
            if ($PortSeen -or $Value -cnotmatch '^[0-9]+(_[0-9]+)*$' -or
                ![int]::TryParse($Value.Replace('_', ''), [ref]$Port) -or $Port -lt 1 -or $Port -gt 65535) {
                throw "Invalid service port in $CoreConfig"
            }
            $PortSeen = $true
        } elseif ($Service -and $Line -cmatch '^\s*(secret|"secret"|''secret'')\s*=\s*(.*)$') {
            $Token = $Matches[2]
            if ($TokenSeen -or $Token -cnotmatch '^("([^"\\]|\\.)*"|''[^'']*'')\s*(#.*)?$') {
                throw "Unsupported service secret format in $CoreConfig"
            }
            $TokenSeen = $true
        }
    }
    '[core]'
    "url = ""http://127.0.0.1:$Port"""
    "secret = $Token"
    'auto_start = true'
    'executable = ' + (ConvertTo-TomlPath $CoreExe)
    'config_file = ' + (ConvertTo-TomlPath $CoreConfig)
}

function Merge-ClientConfig($Connection) {
    $Core = $false
    $Found = $false
    $TableSeen = $false
    foreach ($Line in (Get-Content -LiteralPath $ClientConfig -Encoding UTF8)) {
        if ($Line -cmatch '^\s*\[\s*(core|"core"|''core'')\s*\]\s*(#.*)?$') {
            if ($Found) {
                throw "Duplicate core settings in $ClientConfig"
            }
            $Found = $true
            $Core = $true
            $TableSeen = $true
            $Connection
            continue
        }
        if ($Line -match '^\s*\[') {
            $TableSeen = $true
            if ($Line -cmatch '^\s*\[+\s*(core|"core"|''core'')\s*[.\]]') {
                throw "Unsupported core settings in $ClientConfig"
            }
            $Core = $false
        }
        if (!$TableSeen -and $Line -cmatch '^\s*(core|"core"|''core'')\s*[.=]') {
            throw "Unsupported core settings in $ClientConfig"
        }
        if ($Core -and $Line -cmatch '^\s*(?:"([a-z_]+)"|''([a-z_]+)''|([a-z_]+))\s*=\s*(.*)$') {
            $Key = "$($Matches[1])$($Matches[2])$($Matches[3])"
            if ($Key -cin @('url', 'secret', 'auto_start', 'executable', 'config_file')) {
                if ($Matches[4].StartsWith('"""') -or $Matches[4].StartsWith("'''")) {
                    throw "Unsupported multiline core setting in $ClientConfig"
                }
                continue
            }
        }
        $Line
    }
    if (!$Found) {
        ''
        $Connection
    }
}

function Set-UpConfiguration {
    $ConfigDir = Join-Path $env:USERPROFILE '.config\knot'
    $CoreConfig = Join-Path $ConfigDir 'config.toml'
    $ClientConfig = Join-Path $ConfigDir 'client.toml'
    [IO.Directory]::CreateDirectory($ConfigDir) | Out-Null
    $SetupLockPath = Join-Path $ConfigDir '.knot-setup.lock'
    $SetupLock = [IO.File]::Open($SetupLockPath, 'CreateNew', 'Write', 'None')
    $SetupStage = Join-Path $ConfigDir ('.knot-config.' + [guid]::NewGuid().ToString('N'))
    try {
        foreach ($Config in @($CoreConfig, $ClientConfig)) {
            if (Test-Path -LiteralPath $Config -PathType Container) {
                throw "Expected a configuration file at $Config"
            }
        }
        $HasCore = [IO.File]::Exists($CoreConfig)
        $HasClient = [IO.File]::Exists($ClientConfig)
        if ($HasCore -and $HasClient) {
            Write-Host 'Core and client configurations already exist.'
            return
        }

        [IO.Directory]::CreateDirectory($SetupStage) | Out-Null
        Set-PrivateAcl $SetupStage -Directory
        $SourceConfig = $CoreConfig
        if (!$HasCore) {
            Push-Location -LiteralPath $SetupStage
            try {
                & $CoreExe init
                if ($LASTEXITCODE -ne 0) {
                    throw 'knot-core init failed'
                }
            } finally {
                Pop-Location
            }
            $SourceConfig = Join-Path $SetupStage 'config.toml'
            Set-PrivateAcl $SourceConfig
        }

        $Connection = @(Get-ConnectionBlock $SourceConfig)
        $ClientStage = Join-Path $SetupStage 'client.toml'
        if ($HasClient) {
            if ((Get-Item -LiteralPath $ClientConfig -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) {
                throw "Cannot update a symbolic link at $ClientConfig"
            }
            $ClientLines = @(Merge-ClientConfig $Connection)
        } else {
            $ClientLines = $Connection
        }
        [IO.File]::WriteAllLines($ClientStage, [string[]]$ClientLines, [Text.UTF8Encoding]::new($false))
        Set-PrivateAcl $ClientStage

        if ($HasClient) {
            # File.Replace preserves the destination ACL.
            Set-PrivateAcl $ClientConfig
            [IO.File]::Replace($ClientStage, $ClientConfig, [NullString]::Value)
        } else {
            [IO.File]::Move($ClientStage, $ClientConfig)
        }
        # Publish core last so a retry can repair a client-only installation.
        if (!$HasCore) {
            [IO.File]::Move($SourceConfig, $CoreConfig)
        }
        Write-Host "Configured the local core connection in $ClientConfig"
    } finally {
        Remove-Item -LiteralPath $SetupStage -Recurse -Force -ErrorAction SilentlyContinue
        $SetupLock.Dispose()
        Remove-Item -LiteralPath $SetupLockPath -Force
    }
}

$InstallLock = $null
try {
    $Arch = $env:PROCESSOR_ARCHITEW6432
    if (!$Arch) {
        $Arch = $env:PROCESSOR_ARCHITECTURE
    }
    switch ($Arch) {
        AMD64 { $Platform = 'windows-amd64' }
        ARM64 { $Platform = 'windows-arm64' }
        default { throw "Unsupported CPU architecture $Arch" }
    }
    if (!$BaseUrl) {
        $BaseUrl = 'https://github.com/2cydg/knot-next/releases'
    }
    $BaseUrl = $BaseUrl.TrimEnd('/')
    if (!$Version) {
        $ReleaseUrl = "$BaseUrl/latest/download"
    } elseif ($Version -cmatch '^[A-Za-z0-9._+-]+$' -and $Version -notin @('.', '..')) {
        $ReleaseUrl = "$BaseUrl/download/$Version"
    } else {
        throw 'Invalid release tag'
    }
    if (!$InstallDir) {
        $InstallDir = Join-Path $env:USERPROFILE '.local\bin'
    }
    $InstallDir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($InstallDir)
    [IO.Directory]::CreateDirectory($InstallDir) | Out-Null
    $CliExe = Join-Path $InstallDir 'knot.exe'
    $CoreExe = Join-Path $InstallDir 'knot-core.exe'
    $InstallLockPath = Join-Path $InstallDir '.knot-install.lock'
    $InstallLock = [IO.File]::Open($InstallLockPath, 'CreateNew', 'Write', 'None')
    $InstallStage = Join-Path $InstallDir ('.knot-stage.' + [guid]::NewGuid().ToString('N'))
    [IO.Directory]::CreateDirectory($InstallStage) | Out-Null

    $ChecksumPath = Join-Path $InstallStage 'checksums.txt'
    Get-ReleaseFile 'checksums.txt' $ChecksumPath
    $ChecksumLines = Get-Content -LiteralPath $ChecksumPath -Encoding ASCII
    Get-VerifiedBinary "knot-cli-$Platform.exe" (Join-Path $InstallStage 'knot.exe')
    Get-VerifiedBinary "knot-core-$Platform.exe" (Join-Path $InstallStage 'knot-core.exe')
    Install-Binaries
    Set-UpConfiguration

    if (($env:PATH -split ';') -notcontains $InstallDir) {
        Write-Host "Add $InstallDir to PATH if knot is not found by your shell."
    }
    Write-Host 'Run knot to open the client.'
} catch {
    [Console]::Error.WriteLine("knot install: $($_.Exception.Message)")
    exit 1
} finally {
    if ($InstallLock) {
        if ($InstallStage) {
            Remove-Item -LiteralPath $InstallStage -Recurse -Force -ErrorAction SilentlyContinue
        }
        $InstallLock.Dispose()
        Remove-Item -LiteralPath $InstallLockPath -Force
    }
}
