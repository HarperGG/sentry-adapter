# 同节点部署：公网域名经 CLB 转发到 adapter

适用拓扑：`Sentry taskworker → HTTPS 域名 → CLB → 当前节点内网 IP:8787 → adapter 容器:8787`。Sentry 的 nginx 已占用节点 9000；adapter 使用独立的节点 8787，不与它冲突。本方案不使用 `compose.proxy.yaml` 或 `compose.static-ip.yaml`。

## 1. 核对 CLB 和节点地址

在 CLB 配置中确认：adapter 的域名由 HTTPS 监听器接收并终止 TLS，后端协议为 **HTTP**，后端服务是这台节点的 **内网 IP:8787**。建议该域名的 `/` 路径规则转发给 adapter，让 `/health/ready` 和 `/webhooks/sentry/user-feedback` 都可达，且不改写请求路径。健康检查设为 `GET /health/ready`，预期 HTTP 200；节点安全组和主机防火墙要允许 CLB 到节点 8787。

在部署机查看本机网卡地址和端口占用。`ADAPTER_BIND_IP` 应填写 CLB 后端绑定的节点内网 IP，而不是 CLB 的公网地址，也不是 `127.0.0.1`：

```bash
ip -4 -br addr
ss -lntp '( sport = :8787 )'
```

第二条若有输出，先确认现有监听进程是否属于本 adapter。不要让两个容器或进程同时占用节点 8787。

## 2. 准备项目和配置

将本项目源码放在节点上的独立目录，例如 `/opt/sentry-adapter`，进入该目录执行：

```bash
cd /opt/sentry-adapter
docker compose version
make --version
test -f .env || cp .env.example .env
test -f config/credentials.env || cp config/credentials.env.example config/credentials.env
install -d -m 0700 secrets
```

编辑 `.env`，至少核对以下设置。`ADAPTER_BIND_IP` 使用上一步查到、且与 CLB 后端一致的节点内网 IP；`SENTRY_PROJECT` 是目标 Sentry 项目的 slug。Teambition 的租户、操作人、目标项目、共用场景及自定义字段 ID 也在此文件配置。

```dotenv
ADAPTER_BIND_IP=节点内网IP
SENTRY_BASE_URL=https://sentry.gwm-adas.com
SENTRY_ORG=sentry
SENTRY_PROJECT=目标项目slug
TEAMBITION_BASE_URL=https://teambition.gwm.cn
TEAMBITION_TOKEN_URL=https://teambition.gwm.cn/gateway/appToken
TEAMBITION_TASK_PATH=/gateway/v3/task/create
```

`config/credentials.env` 只在部署机填写企业内部应用的 `TEAMBITION_APP_ID` 和 `TEAMBITION_APP_SECRET`。在 `secrets/` 中准备 `db_password`、`database_url`、`sentry_webhook_secret`、`sentry_auth_token`；后两个值分别来自当前 Internal Integration 的签名 Secret 和具有目标项目读取权限的 Sentry API Token。首次部署时可生成数据库密码与连接串；已有数据库时保留原文件，避免更换密码导致数据库无法连接：

```bash
umask 077
test -e secrets/db_password || openssl rand -hex 24 > secrets/db_password
test -e secrets/database_url || printf 'postgres://adapter:%s@db:5432/adapter?sslmode=disable' "$(cat secrets/db_password)" > secrets/database_url
test -e secrets/sentry_webhook_secret || install -m 0600 /dev/null secrets/sentry_webhook_secret
test -e secrets/sentry_auth_token || install -m 0600 /dev/null secrets/sentry_auth_token
# 编辑 secrets/sentry_webhook_secret 和 secrets/sentry_auth_token，填写实际值
chmod 0600 .env config/credentials.env
chmod 0700 secrets
chmod 0644 secrets/db_password secrets/database_url secrets/sentry_webhook_secret secrets/sentry_auth_token
```

上述真实配置和 secrets 均不属于源码归档。Compose 的 `compose.real.yaml` 会把运行模式设为 `real` 并加载 `config/credentials.env`。

## 3. 打镜像并启动

在 adapter 项目目录执行下面的命令。CLB 域名方案不设置 `SENTRY_PROXY_NETWORK` 或 `SENTRY_ADAPTER_IPV4`；若当前 shell 曾导出过它们，先清除。

```bash
cd /opt/sentry-adapter
unset SENTRY_PROXY_NETWORK SENTRY_ADAPTER_IPV4
make teambition-check-docker
make deploy-real IMAGE=sentry-adapter:1.0.0
make status
docker ps --format '{{.Names}} {{.Ports}}' | grep sentry-adapter
```

`make deploy-real` 会检查 Compose 配置、构建镜像并后台启动 adapter 和它自己的 PostgreSQL。若镜像已在这台节点上构建或拉取，可用 `make deploy-image IMAGE=sentry-adapter:1.0.0` 启动而不重新构建。`make teambition-check-docker` 只检查私有 `/gateway/appToken`，不能代替真实建卡验证。容器端口映射应显示 `节点内网IP:8787->8787/tcp`。

## 4. 逐段验证

把下面的 `NODE_IP` 改成 `.env` 中的 `ADAPTER_BIND_IP`，`ADAPTER_DOMAIN` 改成已绑定到 CLB 的实际域名。如果域名是 `sentry-adapter.gwm-adas.com`，可直接使用示例值。

```bash
NODE_IP='节点内网IP'
ADAPTER_DOMAIN='sentry-adapter.gwm-adas.com'
curl -fsS "http://${NODE_IP}:8787/health/ready"
curl -fsS "https://${ADAPTER_DOMAIN}/health/ready"

docker run --rm --network container:sentry-self-hosted-taskworker-1 \
  curlimages/curl:latest -i \
  -H 'Content-Type: application/json' --data '{}' \
  "https://${ADAPTER_DOMAIN}/webhooks/sentry/user-feedback"

ADAPTER_ID="$(docker compose -f compose.yaml ps -q adapter)"
docker run --rm --network "container:${ADAPTER_ID}" \
  curlimages/curl:latest -sS -o /dev/null \
  -w 'Sentry HTTP=%{http_code}\n' \
  https://sentry.gwm-adas.com/api/0/
```

第三条是未签名 POST；预期看到 adapter 返回的 `401 invalid signature`。这验证了 **Sentry 容器 → 域名 → CLB → 节点 8787 → adapter** 的实际网络路径，不会创建作业。第四条检查 adapter 容器能否访问 Sentry API 地址；不带 Token，收到 HTTP 响应仅证明网络路径存在。若返回 CLB 的 404/502、超时或 TLS 错误，应先排查 CLB 域名规则、后端健康、节点端口和证书。

在 Sentry Internal Integration 中将 Webhook URL 配为 `https://实际域名/webhooks/sentry/user-feedback`。Sentry 26.6 默认禁止向私网 IP 发 Webhook；只有域名在 Sentry 容器里解析并连接到可访问的公网 CLB 地址时，此方案才无需 `SENTRY_ALLOWED_IPS`。curl 只验证网络，最后必须从目标项目触发一条真实反馈，核对 Sentry 投递、adapter 日志与 Teambition 建卡。adapter 的 `SENTRY_BASE_URL` 仍指向用户可访问的 Sentry HTTPS 域名，用于读取 Issue/Event 和生成任务备注链接。

```bash
make logs
```

如果同节点经公网 CLB 回访不通，可改用 [Docker 共享网络直连](../README.md#不配置独立-dns让-sentry-从-docker-内网直连)；该路径需要在 Sentry 中放行 adapter 的固定私网 IP。
