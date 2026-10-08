# Kratos LAN 存储节点

这套服务把 Mac mini 留给业务计算，把文件、PostgreSQL 和 MinIO 放在局域网存储机上。它由 Kratos v3 HTTP 服务组成：`hub` 负责节点注册、发现、文件入口，以及向 Traefik 提供动态路由；`node` 负责本地文件读写、副本传送与服务探测。MinIO 与 PostgreSQL 保持自己的协议和数据格式，分别使用 MinIO 站点复制与 PostgreSQL WAL 流复制。

## 当前拓扑

```text
Mac mini 业务应用 ── LAN ── Windows 存储机 1
                         ├─ hub :8080（注册、发现、文件入口）
                         ├─ node :8081（文件数据）
                         ├─ PostgreSQL :5432
                         └─ MinIO S3 :9000 / 控制台 :9001

以后加入存储机 2：node 双副本 + PostgreSQL 只读备库 + MinIO 第二站点
```

`GET /v1/nodes` 返回最近 35 秒内注册的节点，以及 MinIO、PostgreSQL 地址与探测状态。节点每 10 秒重新注册。MinIO 探测 `/minio/health/live`；PostgreSQL 使用专用账号执行 `SELECT pg_is_in_recovery()`，报告 `primary`、`standby` 或 `unknown`。没有配置 SQL 探测账号或探测失败时，不会声明自己是主库。节点可以设置 `HUB_URL` 直接连接中心，或通过 UDP 多播 `239.255.41.43:39471` 发现中心；Docker Desktop 和跨网段环境建议使用 `HUB_URL`。

## 在第一台存储机启动

1. 在 Windows 上安装 Docker Desktop，保证它有足够的磁盘容量。检查 `8080`、`8081`、`5432`、`9000`、`9001` 端口未被占用。
2. 将 `.env.example` 复制为 `.env`。把 `DATA_ROOT` 指向要存数据的磁盘目录，把 `LAN_IP` 改为这台机器的固定局域网 IP，并替换所有 `CHANGE_ME`。`LAN_TOKEN` 至少 24 字符，推荐随机生成 32 字节；`.env` 不要提交到 Git。
3. 先检查配置：`docker compose config`。
4. 启动：`docker compose up -d --build`。
5. 检查 `http://<LAN_IP>:8080/healthz`、`http://<LAN_IP>:8081/healthz`、`http://<LAN_IP>:9000/minio/health/live`。PostgreSQL 可用 `docker compose exec postgres pg_isready -U <POSTGRES_USER> -d <POSTGRES_DB>` 检查。

所有数据目录位于 `DATA_ROOT/files`、`DATA_ROOT/postgres`、`DATA_ROOT/minio`。不要把 `DATA_ROOT/minio` 当普通文件目录复制或修改；MinIO 管理对象的实际布局。请定期做独立备份，复制不是备份。

## 文件 API

除 `/healthz` 外，所有 hub/node HTTP 请求必须带 `Authorization: Bearer <LAN_TOKEN>`。文件 key 可以有 `/` 分层，不允许 `.`、`..`、反斜杠或空路径段。单文件默认上限 10 GiB，可通过 `MAX_FILE_BYTES` 修改。文件上传先写临时文件，再原子替换本地文件；启用双副本时写入成功需至少两台节点完成。

```powershell
$headers = @{ Authorization = "Bearer $env:LAN_TOKEN" }
Invoke-RestMethod -Headers $headers http://192.168.1.10:8080/v1/nodes
Invoke-WebRequest -Method Put -Headers $headers -InFile .\example.pdf http://192.168.1.10:8080/v1/files/docs/example.pdf
Invoke-WebRequest -Headers $headers -OutFile .\download.pdf http://192.168.1.10:8080/v1/files/docs/example.pdf
```

支持 `GET`、`HEAD`、`PUT`、`DELETE`，以及 HTTP Range 下载。`POST /v1/sync` 是存储节点的旧文件补同步入口，须直接请求节点端口 `8081`。`hub` 的文件入口是私有局域网入口，需要公开分发时应另设鉴权与缓存层。

## Traefik 自动路由

`GET /v1/traefik` 返回 Traefik HTTP Provider 支持的动态 JSON，仍需要 `Authorization: Bearer <LAN_TOKEN>`。Traefik 每 5 秒拉取一次。节点注册或心跳过期后，下一轮拉取会更新路由；离线节点通常在最后一次心跳后约 40 秒被摘除。`GET /v1/gateway/status` 返回选中的 MinIO 节点、PostgreSQL 主库以及路由不可用的原因。

| 入口 | 后端选择 |
| --- | --- |
| `registry.<GATEWAY_DOMAIN>` | Hub，用于节点注册和查询；没有存储节点时仍保留 |
| `files.<GATEWAY_DOMAIN>` | Hub `/v1/files/...`，保留 Hub 的副本读取回退；没有可用节点时移除路由 |
| `s3.<GATEWAY_DOMAIN>` | 健康的 `GATEWAY_MINIO_NODE_ID` 对应站点，不在异步副本站点间轮询 |
| `console.<GATEWAY_DOMAIN>` | 上述站点的 MinIO Console |
| 网关 TCP `:15432` | 唯一、健康且 SQL 确认为 `primary` 的 PostgreSQL；备库和未知角色不会进入写路由 |

如果同时发现两个主库，写路由会被移除，并在状态接口报告 `multiple_primaries_blocked`。这只是路由检查，不提供数据库仲裁或旧主库隔离，也不会自动提升备库。已有数据库连接不因一次路由更新自动迁移到另一台主机。Hub 无法访问时，Traefik 不能获得新的注册表，因此 Hub 仍是控制面的单点。

### 在存储机上运行一个新网关

```sh
docker compose -f compose.yaml -f compose.gateway.yaml up -d --build
```

可选网关默认监听 `8088`（HTTP）和 `15432`（PostgreSQL TCP）。`compose.gateway.yaml` 将 Hub 的后端地址设为同一 Docker 网络中的 `http://hub:8080`。该地址只能用于同机网关；Mac mini 网关应使用下面的跨机配置。

### 在 Mac mini 上运行网关

把 `deploy/compose.gateway.remote.yaml` 和 `deploy/gateway.remote.env.example` 复制到 Mac mini 的网关目录。将后者另存为 `.env`，填写存储机的 `HUB_LAN_URL` 与同一个 `LAN_TOKEN`，然后执行：

```sh
docker compose -f compose.gateway.remote.yaml up -d
```

存储机使用基础 `compose.yaml`，其 `GATEWAY_HUB_URL` 默认是 `http://<LAN_IP>:8080`。节点的 `FILE_URL`、`MINIO_URL`、`MINIO_CONSOLE_URL` 和 `POSTGRES_ADDRESS` 必须是网关与其他存储节点能直连的局域网地址；不要填只在另一台 Docker 主机内部解析的容器名。

如果 Mac mini 已有 Traefik，将 `deploy/traefik-existing.example.yaml` 中的 HTTP Provider 和 PostgreSQL entryPoint 合并到现有静态配置，并把该 entryPoint 的端口映射到宿主机。配置里直接填写令牌，不会自动展开 `${LAN_TOKEN}`。可以保留现有 Docker Provider，两者同时工作；若已有 HTTP Provider，则需要合并配置源。HTTPS 网关在 Hub 上设置 `GATEWAY_HTTP_ENTRYPOINT=websecure`、`GATEWAY_TLS=true`，有现成证书解析器时再设置 `GATEWAY_CERT_RESOLVER`。新建网关示例使用局域网 HTTP，不自动申请证书。

### 稳定域名与诊断

在局域网 DNS 或业务主机的 hosts 中，把 `registry/files/s3/console.kratos.internal` 一次性指向网关 IP。域名可通过 `GATEWAY_DOMAIN` 改成公司内部域名。节点上下线更新后端，不会自动修改局域网 DNS。

没有配置 DNS 时，可以用临时解析验证：

```sh
curl --resolve registry.kratos.internal:8088:<网关IP> \
  -H "Authorization: Bearer $LAN_TOKEN" \
  http://registry.kratos.internal:8088/v1/gateway/status
```

S3 路由保留原 Host，不改写路径或签名；MinIO 使用自己的 S3 凭据。PostgreSQL 使用自己的账号密码。注册表和文件 API 的客户端仍需带 `LAN_TOKEN`。网关不需要访问任何 Docker socket，通过 Hub 的 JSON 接口发现跨机器服务。HTTP Provider 机制见 [Traefik 官方文档](https://doc.traefik.io/traefik/reference/install-configuration/providers/others/http/)。

## 加入第二台存储机

第二台机器需要 Docker、独立存储盘、固定局域网 IP，并能够与第一台双向访问。先复制仓库，在第二台将 `.env.second.example` 复制为 `.env.second`，填写自身 IP、第一台 IP、数据目录，以及与第一台一致的 `LAN_TOKEN` 和 `REPLICATION_PASSWORD`。先检查 `docker compose --env-file .env.second -f compose.second.yaml config`，然后启动：

```sh
docker compose --env-file .env.second -f compose.second.yaml up -d --build
```

确认第一台 hub 的 `/v1/nodes` 出现两个节点以后，把第一台 `.env` 中 `REPLICA_COUNT` 改为 `2`，执行 `docker compose up -d --build node`。两台都设为 2 后，新文件写入需要两台存储节点。对原有文件，在两台节点各执行一次：

```sh
curl -X POST -H "Authorization: Bearer $LAN_TOKEN" http://<节点IP>:8081/v1/sync
```

此补同步会遍历本地文件，只向缺少该 key 的节点写入；同名文件不会被覆盖。请在低负载时执行，并核对两台 `files` 目录的文件清单和关键文件哈希；同名但内容不同的文件需要人工处理。两节点下任一节点不可用时，新写入和删除会失败；已有文件仍可从存活节点读取。没有第三个仲裁者时，这个版本不自动提升 PostgreSQL 备库，以避免网络分区造成双主。恢复后先核对数据和角色，再重新启用写入。

### PostgreSQL

第一台创建新 PostgreSQL 数据目录时，初始化脚本会创建 `replicator` 复制账号和 `kratos_probe` 探测账号，并允许 `REPLICATION_CIDR` 内的主机复制。探测账号只需连接数据库和调用公开的角色查询函数，不授予业务表权限。第二台的 `postgres-standby` 在空数据目录中使用 `pg_basebackup -R` 从第一台初始化，并持续接收 WAL。备库在 `:5432` 提供只读查询。**如果第一台已有 PostgreSQL 数据目录，初始化脚本不会重新执行；需要由管理员在现有主库手工创建复制账号和 `pg_hba.conf` 规则。** 第二台的 PostgreSQL 目录必须为空，不能把原主库的数据目录直接复制进去。

第二台的 `POSTGRES_PROBE_PASSWORD` 和 `POSTGRES_DB` 必须与主库一致，账号由基准备份和 WAL 复制过去。已有本项目主库若缺少探测账号，在填写新密码并重新创建容器使环境变量生效后，显式执行 `docker compose exec postgres sh /opt/postgres-probe-init.sh` 创建或更新探测账号，再验证节点目录的 `postgres_role`。更新探测密码前应同时准备所有节点的配置。

主库确认：`SELECT client_addr, state, sync_state FROM pg_stat_replication;`。备库确认：`SELECT pg_is_in_recovery(), pg_last_wal_replay_lsn();`。默认是异步流复制，主库故障时可能有少量尚未传到备库的事务。业务应用写入始终连接第一台主库；故障切换需要先隔离旧主库、核对复制进度，再手动提升备库并修改连接地址。不要同时向两个主库写入。

### MinIO

两台各运行独立 MinIO 站点，不共享数据目录。MinIO 镜像使用 `alpine/minio:RELEASE.2025-10-15T17-29-55Z`，因为本次检查时原 `quay.io/minio/minio:latest` 无法拉取；这是社区打包镜像，正式使用前应按你的安全策略复核镜像来源。两站版本必须一致。

在两台站点都启动且网络互通后，用 MinIO Client (`mc`) 安全地配置两个站点别名，先指向已有数据的站点，再执行：

```sh
mc admin replicate add storage1 storage2
mc admin replicate info storage1
mc admin replicate status storage1
```

MinIO 站点复制会处理已有对象及后续对象；在状态显示初次同步完成前，不要把第二站点当成完整副本。业务 S3 endpoint 暂时仍指向第一台；切换到第二台应先确认复制状态。MinIO 单节点单盘本身没有纠删码容错；两站复制也不能替代离线备份。

## 迁移和运行边界

- 此仓库不会自动迁移 Mac mini 上现有的 PostgreSQL、MinIO 或文件。迁移时先做备份，按应用逐个切换连接串和 S3 endpoint，再校验数据量与抽样哈希。
- PostgreSQL 的 `5432`、MinIO 的 `9000/9001` 使用各自的账号密码。`LAN_TOKEN` 只保护文件 API 和节点目录，不能代替数据库或 S3 的权限配置。
- 建议在交换机或主机防火墙上只允许可信局域网访问这些端口。跨不可信网络应通过 VPN/TLS，不要直接暴露到公网。
- 文件副本协议是面向两台可信节点的基础版：没有版本历史、分布式锁或自动冲突修复。同一个 key 的并发覆盖需要由上层应用串行化；文件删除后离线节点若带旧文件重新加入，需要人工核对和清理。

## 开发验证

本机没有 Go 命令时，可用 Docker：

```powershell
docker run --rm -v "${PWD}:/src" -w /src golang:1.25-alpine go test ./...
docker build -t kratos-lan:dev .
```

Traefik 端到端验证脚本：`pwsh -File deploy/test-gateway.ps1`。它创建隔离的 Docker 网络、两个临时文件节点、PostgreSQL 主备、MinIO 与 Traefik，检查自动注册、动态路由、文件双副本，以及节点过期后移除 S3/数据库写路由并从存活副本读文件。所有测试数据位于容器 tmpfs，结束后删除测试容器和网络；不会读取 `.env` 或写入 `DATA_ROOT`。该验证覆盖同机容器网络，实际两台存储机上线后仍需验证局域网连通性和跨机复制。

Kratos 的 HTTP 与服务生命周期见 [官方文档](https://go-kratos.dev/docs/component/transport/http/)；MinIO [站点复制](https://min.io/docs/minio/kubernetes/eks/operations/install-deploy-manage/multi-site-replication.html) 与 PostgreSQL [流复制](https://www.postgresql.org/docs/current/warm-standby.html)、[故障切换](https://www.postgresql.org/docs/current/warm-standby-failover.html) 应作为实际部署前的操作参考。
