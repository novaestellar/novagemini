# run-novagemini.ps1 - supervisor for NovaGemini
# Restarts the server whenever it exits, so 9router never sees a dead upstream.
# Path diturunkan dari lokasi skrip ini, jadi folder bisa dipindah tanpa edit.

$ErrorActionPreference = "Stop"

$Dir      = $PSScriptRoot
$Exe      = Join-Path $Dir "novagemini.exe"
$Log      = Join-Path $Dir "novagemini.log"
$ErrLog   = Join-Path $Dir "novagemini.err.log"
$SuperLog = Join-Path $Dir "supervisor.log"

function Write-Sup {
    param([string]$Msg)
    $line = "[{0}] {1}" -f (Get-Date -Format "yyyy-MM-dd HH:mm:ss"), $Msg
    Add-Content -Path $SuperLog -Value $line -Encoding utf8
}

if (-not (Test-Path $Exe)) {
    Write-Sup "FATAL: binary not found at $Exe"
    exit 1
}

Set-Location $Dir
Write-Sup "supervisor start pid=$PID dir=$Dir"

while ($true) {
    $busy = Get-NetTCPConnection -LocalPort 8083 -State Listen -ErrorAction SilentlyContinue
    if ($busy) {
        Write-Sup "port 8083 busy pid=$($busy[0].OwningProcess) wait 30s"
        Start-Sleep -Seconds 30
        continue
    }

    Write-Sup "starting server"
    $started = Get-Date
    try {
        $p = Start-Process -FilePath $Exe -WorkingDirectory $Dir -RedirectStandardOutput $Log -RedirectStandardError $ErrLog -WindowStyle Hidden -PassThru
        Write-Sup "server pid=$($p.Id)"
        $p.WaitForExit()
        $up = [int]((Get-Date) - $started).TotalSeconds
        Write-Sup "server exited code=$($p.ExitCode) uptime=${up}s"
    }
    catch {
        $msg = $_.Exception.Message
        Write-Sup "start failed: $msg"
    }

    Start-Sleep -Seconds 5
}
