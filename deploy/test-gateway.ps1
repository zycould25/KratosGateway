#requires -Version 7.0
param([switch]$SkipBuild)

# This test creates a private Docker network and temporary containers with
# tmpfs storage and dummy credentials. It never reads .env or touches DATA_ROOT.
$ErrorActionPreference = 'Stop'
$projectPath = Split-Path -Parent $PSScriptRoot
$suffix = ([guid]::NewGuid().ToString('N')).Substring(0, 8)
$network = "kratos-gateway-test-$suffix"
$created = [Collections.Generic.List[string]]::new()
$token = 'test-only-0123456789abcdefghijklmnopqrstuv'

function Invoke-Docker([string[]]$DockerArgs) {
    $output = & docker @DockerArgs 2>&1
    if ($LASTEXITCODE -ne 0) { throw "docker failed: $($output -join [Environment]::NewLine)" }
    return $output
}

function Start-TestContainer([string]$Alias, [string[]]$Options, [string]$Image, [string[]]$Command = @()) {
    $name = "kratos-$Alias-test-$suffix"
    $created.Add($name)
    $null = Invoke-Docker -DockerArgs (@('run', '-d', '--name', $name, '--network', $network, '--network-alias', $Alias) + $Options + @($Image) + $Command)
    return $name
}

function Wait-Until([scriptblock]$Check, [string]$Description, [int]$TimeoutSeconds = 45) {
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        try { if (& $Check) { return } } catch {}
        Start-Sleep -Seconds 1
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Timed out: $Description"
}

try {
    if (-not $SkipBuild) { $null = Invoke-Docker -DockerArgs @('build', '-q', '-t', 'kratos-lan:dev', $projectPath) }
    $null = Invoke-Docker -DockerArgs @('network', 'create', $network)
    $primary = Start-TestContainer -Alias primary -Image 'postgres:17-alpine' -Options @(
        '--mount', 'type=tmpfs,destination=/var/lib/postgresql/data',
        '--mount', "type=bind,source=$projectPath/deploy/postgres-primary-init.sh,target=/docker-entrypoint-initdb.d/01-replication.sh,readonly",
        '-e', 'POSTGRES_DB=app', '-e', 'POSTGRES_USER=app', '-e', 'POSTGRES_PASSWORD=test-password',
        '-e', 'REPLICATION_PASSWORD=test-replication', '-e', 'POSTGRES_PROBE_PASSWORD=test-probe', '-e', 'REPLICATION_CIDR=0.0.0.0/0'
    ) -Command @('postgres', '-c', 'listen_addresses=*', '-c', 'wal_level=replica', '-c', 'max_wal_senders=10', '-c', 'wal_keep_size=1GB')
    Wait-Until -Description 'PostgreSQL primary startup' -Check {
        & docker exec $primary pg_isready -h 127.0.0.1 -U app -d app 2>$null | Out-Null
        return $LASTEXITCODE -eq 0
    }
    $standby = Start-TestContainer -Alias standby -Image 'postgres:17-alpine' -Options @(
        '--mount', 'type=tmpfs,destination=/var/lib/postgresql/data',
        '--mount', "type=bind,source=$projectPath/deploy/postgres-standby.sh,target=/opt/init-standby.sh,readonly",
        '-e', 'PGDATA=/var/lib/postgresql/data', '-e', 'PG_PRIMARY_HOST=primary', '-e', 'PG_PRIMARY_PORT=5432', '-e', 'REPLICATION_PASSWORD=test-replication'
    ) -Command @('/bin/sh', '/opt/init-standby.sh')
    Wait-Until -Description 'PostgreSQL standby recovery' -Check {
        $result = & docker exec $standby psql -U app -d app -tAc 'SELECT pg_is_in_recovery()' 2>$null
        return $LASTEXITCODE -eq 0 -and "$result".Trim() -eq 't'
    }
    $goOutput = Invoke-Docker -DockerArgs @('run', '--rm', '--network', $network,
        '-v', "${projectPath}:/src", '-v', 'kratos-go-modcache:/go/pkg/mod', '-v', 'kratos-go-buildcache:/root/.cache/go-build', '-w', '/src',
        '-e', 'KRATOS_TEST_PRIMARY=primary:5432', '-e', 'KRATOS_TEST_STANDBY=standby:5432',
        '-e', 'KRATOS_TEST_PG_USER=kratos_probe', '-e', 'KRATOS_TEST_PG_PASSWORD=test-probe',
        'golang:1.25-alpine', 'go', 'test', './internal/lan', '-run', '^TestPostgresRolesIntegration$', '-v')
    $goOutput | Write-Output
    $minio = Start-TestContainer -Alias minio -Image 'alpine/minio:RELEASE.2025-10-15T17-29-55Z' -Options @(
        '--mount', 'type=tmpfs,destination=/data', '-e', 'MINIO_ROOT_USER=testadmin', '-e', 'MINIO_ROOT_PASSWORD=testpassword123'
    ) -Command @('server', '/data', '--console-address', ':9001')
    $hub = Start-TestContainer -Alias hub -Image 'kratos-lan:dev' -Options @(
        '-e', 'ROLE=hub', '-e', "LAN_TOKEN=$token", '-e', 'ADVERTISE_URL=http://hub:8080',
        '-e', 'GATEWAY_HUB_URL=http://hub:8080', '-e', 'GATEWAY_DOMAIN=kratos.test', '-e', 'GATEWAY_MINIO_NODE_ID=storage-1'
    )
    $gateway = Start-TestContainer -Alias gateway -Image 'traefik:v3.7.14' -Options @('-p', '127.0.0.1::80') -Command @(
        '--providers.http.endpoint=http://hub:8080/v1/traefik', '--providers.http.pollInterval=1s', '--providers.providersThrottleDuration=0s',
        "--providers.http.headers.Authorization=Bearer $token", '--entryPoints.web.address=:80', '--entryPoints.postgres.address=:5432', '--log.level=INFO'
    )
    $portLine = Invoke-Docker -DockerArgs @('port', $gateway, '80/tcp')
    $port = [int](($portLine -split ':')[-1])
    $gatewayURL = "http://127.0.0.1:$port"
    $registryHeaders = @{ Host = 'registry.kratos.test'; Authorization = "Bearer $token" }
    $fileHeaders = @{ Host = 'files.kratos.test'; Authorization = "Bearer $token" }
    Wait-Until -Description 'Traefik authenticated HTTP provider' -Check {
        $status = Invoke-RestMethod -Headers $registryHeaders "$gatewayURL/v1/gateway/status" -TimeoutSec 3
        return $status.active_nodes -eq 0
    }
    $missing = Invoke-WebRequest -Headers $fileHeaders "$gatewayURL/healthz" -SkipHttpErrorCheck
    if ($missing.StatusCode -ne 404) { throw 'File route must be absent before nodes register' }

    $node1 = Start-TestContainer -Alias node1 -Image 'kratos-lan:dev' -Options @(
        '--mount', 'type=tmpfs,destination=/data', '-e', 'ROLE=node', '-e', "LAN_TOKEN=$token", '-e', 'NODE_ID=storage-1',
        '-e', 'FILE_URL=http://node1:8080', '-e', 'HUB_URL=http://hub:8080', '-e', 'DATA_DIR=/data', '-e', 'REPLICA_COUNT=2',
        '-e', 'MINIO_URL=http://minio:9000', '-e', 'MINIO_CONSOLE_URL=http://minio:9001', '-e', 'POSTGRES_ADDRESS=primary:5432',
        '-e', 'POSTGRES_PROBE_USER=kratos_probe', '-e', 'POSTGRES_PROBE_PASSWORD=test-probe', '-e', 'POSTGRES_PROBE_DATABASE=app'
    )
    $node2 = Start-TestContainer -Alias node2 -Image 'kratos-lan:dev' -Options @(
        '--mount', 'type=tmpfs,destination=/data', '-e', 'ROLE=node', '-e', "LAN_TOKEN=$token", '-e', 'NODE_ID=storage-2',
        '-e', 'FILE_URL=http://node2:8080', '-e', 'HUB_URL=http://hub:8080', '-e', 'DATA_DIR=/data', '-e', 'REPLICA_COUNT=2',
        '-e', 'POSTGRES_ADDRESS=standby:5432', '-e', 'POSTGRES_PROBE_USER=kratos_probe', '-e', 'POSTGRES_PROBE_PASSWORD=test-probe', '-e', 'POSTGRES_PROBE_DATABASE=app'
    )
    Wait-Until -Description 'both node registrations and confirmed primary' -Check {
        $status = Invoke-RestMethod -Headers $registryHeaders "$gatewayURL/v1/gateway/status" -TimeoutSec 3
        return $status.active_nodes -eq 2 -and $status.postgres_state -eq 'ready' -and $status.minio_state -eq 'ready'
    }
    Wait-Until -Description 'file route appears without restarting Traefik' -Check {
        $response = Invoke-WebRequest -Headers $fileHeaders "$gatewayURL/healthz" -SkipHttpErrorCheck -TimeoutSec 3
        return $response.StatusCode -eq 200
    }
    $response = Invoke-WebRequest -Method Put -Headers $fileHeaders -Body 'gateway-replica-data' "$gatewayURL/v1/files/gateway-check.txt"
    if ($response.StatusCode -ne 201) { throw 'Gateway upload failed' }
    foreach ($node in @($node1, $node2)) {
        $copy = Invoke-Docker -DockerArgs @('exec', $node, 'cat', '/data/gateway-check.txt')
        if ("$copy".Trim() -ne 'gateway-replica-data') { throw "File replica missing on $node" }
    }
    Wait-Until -Description 'S3 route appears' -Check {
        $response = Invoke-WebRequest -Headers @{ Host = 's3.kratos.test' } "$gatewayURL/minio/health/live" -SkipHttpErrorCheck -TimeoutSec 3
        return $response.StatusCode -eq 200
    }
    $recovery = Invoke-Docker -DockerArgs @('exec', '-e', 'PGPASSWORD=test-password', '-e', 'PGCONNECT_TIMEOUT=3', $primary,
        'psql', '-h', 'gateway', '-U', 'app', '-d', 'app', '-tAc', 'SELECT pg_is_in_recovery()')
    if ("$recovery".Trim() -ne 'f') { throw 'Gateway PostgreSQL write route selected a standby' }
    Write-Output 'PASS: registration -> Traefik HTTP/TCP routes; file upload replicated to two nodes; PostgreSQL points to primary'

    $null = Invoke-Docker -DockerArgs @('rm', '-f', $node1)
    $null = $created.Remove($node1)
    Wait-Until -Description 'expired primary agent removes S3 and PostgreSQL routes' -TimeoutSeconds 50 -Check {
        $status = Invoke-RestMethod -Headers $registryHeaders "$gatewayURL/v1/gateway/status" -TimeoutSec 3
        return $status.active_nodes -eq 1 -and $status.postgres_state -eq 'no_confirmed_primary' -and $status.minio_state -eq 'preferred_site_unavailable'
    }
    Wait-Until -Description 'Traefik removes expired S3 route' -Check {
        $response = Invoke-WebRequest -Headers @{ Host = 's3.kratos.test' } "$gatewayURL/minio/health/live" -SkipHttpErrorCheck -TimeoutSec 3
        return $response.StatusCode -eq 404
    }
    $download = Invoke-WebRequest -Headers $fileHeaders "$gatewayURL/v1/files/gateway-check.txt"
    if ($download.StatusCode -ne 200 -or $download.Content -ne 'gateway-replica-data') { throw 'File read did not fall back to surviving node' }
    $blockedOutput = & docker exec -e PGPASSWORD=test-password -e PGCONNECT_TIMEOUT=3 $primary psql -h gateway -U app -d app -tAc 'SELECT pg_is_in_recovery()' 2>&1
    if ($LASTEXITCODE -eq 0) { throw 'PostgreSQL write route remained after primary registration expired' }
    Write-Output 'PASS: node expiry updates Traefik; surviving file replica stays readable; standby is excluded from write route'
} catch {
    foreach ($name in $created) { & docker logs $name --tail 15 2>$null | Write-Output }
    throw
} finally {
    for ($i = $created.Count - 1; $i -ge 0; $i--) { & docker rm -f $created[$i] 2>$null | Out-Null }
    & docker network rm $network 2>$null | Out-Null
}
