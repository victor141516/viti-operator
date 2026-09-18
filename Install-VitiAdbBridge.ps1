[CmdletBinding()]
param(
    [string]$BridgeExecutable = (Join-Path $PSScriptRoot 'viti-adb-bridge.exe'),
    [string]$AdbExecutable = 'adb.exe',
    [string]$ListenAddress,
    [ValidateRange(1025, 65535)]
    [int]$Port = 5038,
    [string]$TaskName = 'Viti Operator ADB Bridge'
)

$ErrorActionPreference = 'Stop'

$resolvedExecutable = (Resolve-Path -LiteralPath $BridgeExecutable).Path
$resolvedAdb = (Get-Command $AdbExecutable -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
if ([string]::IsNullOrWhiteSpace($ListenAddress)) {
    $ListenAddress = '127.0.0.1'
}
if ([string]::IsNullOrWhiteSpace($ListenAddress)) {
    throw 'A loopback or private listen address is required.'
}
if ($ListenAddress -ne '127.0.0.1') {
    $localAddress = Get-NetIPAddress -AddressFamily IPv4 -ErrorAction Stop |
        Where-Object IPAddress -EQ $ListenAddress |
        Select-Object -First 1
    if (-not $localAddress -or $ListenAddress -notmatch '^(10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.)') {
        throw "The bridge must listen on loopback or a private address assigned to this host: $ListenAddress"
    }
}

$userId = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
# The proxy does not start its upstream ADB daemon. Start both in order on
# every task run, preserving absolute paths even when the login PATH differs.
$quotedAdb = $resolvedAdb.Replace("'", "''")
$quotedBridge = $resolvedExecutable.Replace("'", "''")
$launcher = @"
`$ErrorActionPreference = 'Stop'
& '$quotedAdb' start-server
if (`$LASTEXITCODE -ne 0) { exit `$LASTEXITCODE }
& '$quotedBridge' -listen '${ListenAddress}:$Port' -upstream '127.0.0.1:5037'
exit `$LASTEXITCODE
"@
$encodedLauncher = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($launcher))
$action = New-ScheduledTaskAction -Execute "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe" `
    -Argument "-NoProfile -NonInteractive -WindowStyle Hidden -EncodedCommand $encodedLauncher"
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $userId
$principal = New-ScheduledTaskPrincipal -UserId $userId -LogonType Interactive -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -MultipleInstances IgnoreNew `
    -RestartCount 999 `
    -RestartInterval (New-TimeSpan -Minutes 1) `
    -StartWhenAvailable

$existingTask = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
if ($existingTask) {
    Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    $stopDeadline = (Get-Date).AddSeconds(10)
    while ((Get-ScheduledTask -TaskName $TaskName).State -eq 'Running' -and (Get-Date) -lt $stopDeadline) {
        Start-Sleep -Milliseconds 200
    }
}

Register-ScheduledTask `
    -TaskName $TaskName `
    -Action $action `
    -Trigger $trigger `
    -Principal $principal `
    -Settings $settings `
    -Description 'Starts host ADB before the private Viti Operator proxy for Docker.' `
    -Force | Out-Null

Start-ScheduledTask -TaskName $TaskName
$deadline = (Get-Date).AddSeconds(15)
do {
    Start-Sleep -Milliseconds 250
    $listener = Get-NetTCPConnection -State Listen -LocalAddress $ListenAddress -LocalPort $Port -ErrorAction SilentlyContinue
} while (-not $listener -and (Get-Date) -lt $deadline)
if (-not $listener) {
    $task = Get-ScheduledTaskInfo -TaskName $TaskName
    throw "The ADB bridge task did not open ${ListenAddress}:$Port (last result $($task.LastTaskResult))."
}

[pscustomobject]@{
    TaskName = $TaskName
    Executable = $resolvedExecutable
    AdbExecutable = $resolvedAdb
    ListenAddress = $ListenAddress
    Port = $Port
    Running = $true
} | ConvertTo-Json -Compress
