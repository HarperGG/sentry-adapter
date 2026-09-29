# Sentry 用户反馈 → Teambition 适配器

这是一个 Go 服务，用于接收 Sentry Internal Integration 的 `issue.created` Webhook，并把指定 Sentry 项目的用户反馈转成 Teambition 任务。目标项目由 `.env` 中的 `SENTRY_PROJECT` 指定，示例值为 `anno-seg-test`。架构与当前限制见 [docs/architecture.md](docs/architecture.md)。

## 当前实现范围

已实现：

- `POST /webhooks/sentry/user-feedback`：对原始请求体验证 `Sentry-Hook-Signature`，过滤 `issue.created` 的 `FEEDBACK` 类别，也兼容 `event_alert.triggered`。当前 Sentry 配置使用的是前一种。
- 先写 PostgreSQL 去重作业，再返回 `202`；后台 worker 只读取 Sentry Issue 和 Event，不查询或下载截图。
- `content` 按 `【taskType: dataset_id】 反馈类型` 生成，反馈类型使用与 Sentry 页面一致的中文标签，例如 `【OD_correct: odc_0908-JGzm】 使用建议`。`note` 写入反馈原文、一个空行和这条反馈的 Sentry 链接；不处理图片。缺少分类、`taskType`、`dataset_id`、反馈正文或链接时，作业进入 `needs_review`，不调用 Teambition 建卡。
- Teambition 客户端默认向私有 `https://teambition.gwm.cn/gateway/appToken` POST `appId`、`appSecret`，缓存返回的 `appToken`，作为 `Authorization: Bearer <appToken>` 调用 `/gateway/v3/task/create`；距过期不足 100 秒时重新获取。可配置 `TEAMBITION_AUTH_MODE=local_jwt`，沿用本地签发 HS256 JWT 的方式。建卡 body 包含 `involveMembers`、`objectType=task`、共用场景 ID 和配置的下拉自定义字段；只有拿到明确 task ID 才记为成功。
- `mock` 模式不会写入 Teambition；真实模式第一版不调用截图上传接口，也不把截图放进任务的文件自定义字段。上传辅助代码保留供后续联调，当前业务链路不会使用。
- 建卡请求发出后发生超时、5xx 或响应不完整时进入 `uncertain`，不会自动再次建卡。

**尚未完成私有环境端到端联调**：反馈 Event 的分类字段和链接需要用一条 Sentry 26.6 真样例核对；私有 appToken 接口在本机仍返回 403。截图上传暂不属于第一版链路。

## 目录

```text
cmd/adapter         HTTP 服务与 worker 入口
internal/webhook    Sentry Webhook 验签与解析
internal/sentry     Issue、Event 读取；截图辅助代码暂不使用
internal/teambition 在线 appToken / 本地 JWT、建卡与 Mock；上传辅助代码暂不使用
internal/store      PostgreSQL 作业与状态转换
internal/worker     反馈过滤、转换、失败分类
docs/architecture.md 架构与上线清单
config/credentials.env.example Teambition 应用凭据配置模板
```

## 配置文件

部署时使用三处配置，实际凭据只保存在部署主机：

| 文件                     | 内容                                                                                                                          | 是否提交                                     |
| ------------------------ | ----------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------- |
| `.env`                   | 非敏感设置，包括 `SENTRY_PROJECT`、Teambition 目标项目与任务字段 ID                                                           | 否；由 `.env.example` 复制                   |
| `config/credentials.env` | `TEAMBITION_APP_ID` 和 `TEAMBITION_APP_SECRET`，真实模式下由 Compose 注入为容器环境变量，用于在线获取 appToken 或本地签发 JWT | 否；由 `config/credentials.env.example` 复制 |
| `secrets/*`              | 数据库密码与连接串、Sentry Webhook Secret、Sentry API Token                                                                   | 否；由 Compose secrets 挂载                  |

在 `.env` 中设置 `SENTRY_PROJECT` 为当前目标 Sentry 项目的 **slug**，并确保 Internal Integration 已在该项目安装且 Sentry API Token 有相应读取权限。改动后重新部署适配器；现有数据库中其他项目的作业不会因改配置自动迁移或重新处理。`TEAMBITION_PROJECT_ID` 是建卡目标项目，与 `SENTRY_PROJECT` 不同。

`config/credentials.env` 的值是 Teambition 企业内部应用的 Client ID 和 Client Secret。请只在本机私有文件中填写实际值，不要放到 `.env.example`、命令行参数或提交记录中。Compose 的环境变量可被拥有 Docker 管理权限的人查看，应限制部署主机和此文件的访问权限。程序同时支持 `TEAMBITION_APP_SECRET_FILE`，但本部署方式使用直接的 `TEAMBITION_APP_SECRET` 环境变量；两者不能同时设置。

之前提供的任务 cURL 使用浏览器 Cookie 调用 `/api/v2/tasks`。现在以用户已调用成功的私有 `/gateway/appToken` 和 `/gateway/v3/task/create` 请求为准：已确认一个 `scenariofieldconfigId` 可供所有反馈共用，`involveMembers` 使用配置的操作人 ID，下拉 `customfields` 使用单独配置的字段与选项 ID。旧 cURL 中的任务列表、阶段、流程状态和执行人 ID 仍可配置，但本地 `.env` 已清空这些未经当前成功请求验证的可选值。第一版无需配置截图文件字段。

| Teambition 建卡字段                  | 来源                                                           |
| ------------------------------------ | -------------------------------------------------------------- |
| `content`                            | `【{taskType}: {dataset_id}】 {反馈类型}`；反馈类型使用与 Sentry UI 一致的中文标签 |
| `note`                               | 反馈原文、空行、Sentry 反馈链接；不包含图片                    |
| `projectId`、`scenariofieldconfigId` | `TEAMBITION_PROJECT_ID`、共用的 `TEAMBITION_SCENARIO_ID`       |
| `involveMembers`                     | `[TEAMBITION_OPERATOR_ID]`                                     |
| `objectType`                         | 固定为 `task`                                                  |
| `customfields`                       | 配置的下拉字段与选项；第一版不加入截图文件字段                 |

反馈深链接使用 Sentry Issue API 返回的项目 slug、项目数值 ID 和 Issue ID，构成 `.../organizations/{org}/issues/feedback/?feedbackSlug={projectSlug}:{issueId}&project={projectId}`。缺少这些字段时回退到 API permalink 或普通 Issue 地址。

缺少分类、`taskType`、`dataset_id`、反馈正文或最终生成的反馈链接时，作业进入 `needs_review`，不会向 Teambition 发送建卡请求。

## 使用 Make 构建与验证

本地构建需要 Go 1.27 和 GNU Make；部署还需要 Docker Compose。Windows 主机须先安装 GNU Make 并让 `go`、`make` 可从终端调用。

```bash
make help
make check
make build
make probe-build
make release VERSION=1.0.0
make docker-build IMAGE=sentry-adapter:1.0.0
# 需要发布到镜像仓库时：
make docker-build IMAGE=registry.example.com/team/sentry-adapter:1.0.0
make docker-push IMAGE=registry.example.com/team/sentry-adapter:1.0.0
```

`make check` 运行 Go 测试和静态检查；`make build` 生成适配器本机程序，`make probe-build` 生成本机联通性探针（Windows 均为 `.exe`）；`make release` 先运行 `check`，再生成两个程序的 Linux amd64/arm64 可执行文件及 `dist/<VERSION>/SHA256SUMS`。`make help` 列出所有目标。发布产物和镜像的版本由命令中的 `VERSION`、`IMAGE` 参数指定。`make deploy IMAGE=...` 在部署机编译并以该名称标记镜像；`make docker-build IMAGE=...` 可单独打镜像，随后用 `make deploy-image IMAGE=...` 部署已有镜像。`docker-push` 需指定已授权的镜像仓库地址。

## 用 Swagger 检查 Teambition 私有网关

在你已能用 Postman 调通私有接口的**同一台机器或同一网络环境**运行探针。它直接从主机访问 `TEAMBITION_TOKEN_URL`，不依赖 Docker 容器的网络。先在 `.env` 配好私有 URL、租户与操作人 ID，并在 `config/credentials.env` 放入 AppID、AppSecret，然后运行：

```bash
make teambition-check
make swagger
```

要检查 **Docker 容器内**的出站网络，可运行 `make teambition-check-docker`。它在独立的 Compose 诊断容器里执行一次 token 检查，不开放端口，也不需要先启动数据库或适配器。两次检查分别验证主机与容器的网络路径；容器命令仍只输出脱敏状态。若部署时使用了自定义 Compose 项目名，运行诊断命令时设置相同的 `COMPOSE_PROJECT_NAME`，以加入同名项目网络。

Docker 构建阶段默认从 `https://goproxy.cn` 下载 Go 模块，以避开部分节点访问 `proxy.golang.org` 的超时。`go.sum` 和 Go 校验数据库仍用于校验依赖。若需使用企业模块代理，可在执行 Make 命令时设置 `GOPROXY`，例如 `GOPROXY=https://内部模块代理 make teambition-check-docker`；`make docker-build` 与 `make deploy-real` 也支持这个变量。此设置只影响镜像构建，不改变运行时对 Teambition 的请求。

第一条命令只检查 `POST /gateway/appToken`，输出 HTTP 状态、分类及 token 有效期，不输出 token 或凭据。第二条命令启动只监听本机的 Swagger UI，打开 [http://127.0.0.1:8790/swagger/](http://127.0.0.1:8790/swagger/)，在页面中执行 **检查 appToken 接口**。Swagger 调用本地诊断服务，由服务端携带凭据请求 Teambition，浏览器不直接向私有网关发送密钥。如果有一条已存在的 Teambition 任务 ID，还可以执行 **检查已有任务读取接口**；它先取 token，再调用只读的 `GET /gateway/v3/task/query?taskId=...`，不会创建任务。[Teambition 任务查询接口](https://open.teambition.com/docs/apis/6321c6d2912d20d3b5a4a7b8)是该检查的协议依据。

| 探针结果                                                          | 含义                                                                            |
| ----------------------------------------------------------------- | ------------------------------------------------------------------------------- |
| `gatewayStatus=200` 且 `tokenReceived=true`                       | 当前运行环境能从私有网关取到 appToken                                           |
| `gatewayStatus=403`                                               | 已收到 HTTP 403；可能由私有网关或代理返回，结合该环境的 VPN、代理和访问策略排查 |
| `gatewayStatus=0`，`category=dns_error` / `tls_error` / `timeout` | DNS、TLS 或连接阶段未完成                                                       |
| `taskCategory=not_attempted`                                      | 取 token 未成功，因此尚未向任务查询接口发请求                                   |
| `taskStatus=200` 且 `taskCategory=ok`                             | 已有任务的只读查询成功；任务内容不会返回到浏览器                                |

Windows 上还可先执行 `Resolve-DnsName teambition.gwm.cn` 和 `Test-NetConnection teambition.gwm.cn -Port 443` 检查 DNS 与 TCP 443；两者成功只说明传输层可达，应用层以探针返回的 HTTP 状态为准。没有 GNU Make 时，可以直接运行 `go run ./cmd/teambition-probe -once` 或 `go run ./cmd/teambition-probe`。Swagger UI 的 JS/CSS 已打包在探针中，不依赖公网 CDN。

## 在 Sentry 宿主机上部署 Docker 镜像

当前 Compose 会启动适配器和它**自己的 PostgreSQL**，不会使用 Sentry 数据库；宿主机端口为 8787，默认只绑定 `127.0.0.1`。已有公网域名、由 CLB 直连节点 8787 的部署方式见 [CLB 部署手册](docs/deploy-clb.md)：在 `.env` 中把 `ADAPTER_BIND_IP` 设为 CLB 后端绑定的节点内网 IP。把本项目文件放到 Sentry 所在的 Linux 宿主机，例如 `/opt/sentry-adapter`。宿主机需能运行 `docker compose` 和 GNU Make；Docker 构建镜像时会在构建容器内安装 Go，宿主机无需另装 Go。

```bash
cd /opt/sentry-adapter
docker compose version
make --version
cp .env.example .env
cp config/credentials.env.example config/credentials.env
install -d -m 0700 secrets
```

编辑 `.env`：填入目标 `SENTRY_PROJECT` slug、Sentry 地址/组织和 Teambition 的租户、操作人、项目、共用场景、自定义字段 ID；CLB 直连时另设置 `ADAPTER_BIND_IP` 为节点内网 IP，不能填 CLB 地址。保持 `TEAMBITION_AUTH_MODE=app_token`、`TEAMBITION_BASE_URL=https://teambition.gwm.cn`、`TEAMBITION_TOKEN_URL=https://teambition.gwm.cn/gateway/appToken`、`TEAMBITION_TASK_PATH=/gateway/v3/task/create`。运行模式由部署命令选择：`make deploy-real` 和 `make deploy-image` 使用真实 Teambition 接口，`make deploy-mock` 才使用模拟结果；不要在 `.env` 中设置 `TEAMBITION_MODE`，已有该行可删除。编辑 `config/credentials.env`，只在部署机填写企业内部应用的 AppID 和 AppSecret。第一版不需要截图相关配置。

在 `secrets/` 下准备四个文件：`db_password`、`database_url`、`sentry_webhook_secret`、`sentry_auth_token`。数据库密码可用下面的命令生成；Sentry 的两个值请从已配置的 Integration 和 API Token 填入文件，勿粘贴到命令参数或版本库。`db` 是本 Compose 的数据库服务名，不是 Sentry 的数据库地址。

```bash
umask 077
openssl rand -hex 24 > secrets/db_password
printf 'postgres://adapter:%s@db:5432/adapter?sslmode=disable' "$(cat secrets/db_password)" > secrets/database_url
# 编辑 secrets/sentry_webhook_secret 和 secrets/sentry_auth_token，写入实际值
chmod 0600 .env config/credentials.env
chmod 0700 secrets
chmod 0644 secrets/db_password secrets/database_url secrets/sentry_webhook_secret secrets/sentry_auth_token
```

`secrets/` 目录的 `0700` 限制宿主机其他普通用户进入；文件的 `0644` 使 Compose 挂载后的非 root 容器进程可以读取。Compose 对文件来源的 secret 使用 bind mount，不能依靠 Compose 的 `mode`、`uid`、`gid` 选项调整权限。[Docker Compose secrets 文档](https://docs.docker.com/reference/compose-file/services/#secrets)说明了此限制。四个文件、`.env` 和 `config/credentials.env` 均被 Git 忽略。

先从容器网络检查私有 Teambition 网关，然后在**这台宿主机**打镜像、部署已有镜像：

```bash
make teambition-check-docker
make docker-build IMAGE=sentry-adapter:1.0.0
make deploy-image IMAGE=sentry-adapter:1.0.0
make status
```

`make deploy-image` 不重新编译镜像。也可用 `make deploy-real IMAGE=sentry-adapter:1.0.0` 一条命令完成构建和启动。后续改代码时使用新版本标签重新构建、部署；`make logs` 跟踪适配器日志，`make stop` 停止服务并保留数据库卷。真实模式会在启动日志写入 `mode=real` 和 `auth_mode=app_token`；只在处理 Sentry 真实反馈时才调用 Teambition 建卡接口。若在另一台机器打镜像，打包时就使用完整仓库标签，例如 `make docker-build IMAGE=registry.example.com/team/sentry-adapter:1.0.0`，随后用同一 `IMAGE` 执行 `make docker-push`；在部署机执行 `docker pull registry.example.com/team/sentry-adapter:1.0.0` 和 `make deploy-image IMAGE=registry.example.com/team/sentry-adapter:1.0.0`。通过 CLB 部署时，健康检查和从 Sentry 容器到域名的测试命令见 [CLB 部署手册](docs/deploy-clb.md)。

如果另行选择由宿主机上的 HTTPS 反向代理转发，把 `https://sentry-adapter.gwm-adas.com/webhooks/sentry/user-feedback` 代理到 `http://127.0.0.1:8787/webhooks/sentry/user-feedback`，并保留 `ADAPTER_BIND_IP=127.0.0.1`。如果代理运行在 Docker 容器里，它自己的 `127.0.0.1` 不能访问适配器。先找出代理容器所在的现有网络，然后用可选的 `compose.proxy.yaml` 把适配器接入该网络：

```bash
docker ps --format '{{.Names}}'
docker inspect <代理容器名> --format '{{range $name, $_ := .NetworkSettings.Networks}}{{println $name}}{{end}}'
make deploy-image IMAGE=sentry-adapter:1.0.0 SENTRY_PROXY_NETWORK=<代理容器所在网络>
```

此时代理上游使用 `http://sentry-adapter:8787`，后续执行 `make deploy-real` 或 `make deploy-image` 时继续传入同一个 `SENTRY_PROXY_NETWORK`。外部网络必须事先存在；`compose.proxy.yaml` 只让适配器加入，自己的 PostgreSQL 仍留在本项目默认网络。[Docker Compose 跨项目网络说明](https://docs.docker.com/compose/how-tos/networking/#connecting-multiple-compose-projects)提供了这一配置方式。`/health/ready` 只验证适配器与其数据库；部署后仍需用目标 Sentry 项目的一条真实反馈验证 Webhook、Sentry API 和 Teambition 建卡链路。

### 不配置独立 DNS：让 Sentry 从 Docker 内网直连

Sentry 和适配器部署在同一台 Docker 宿主机时，可以让真正发送 Webhook 的 Sentry 容器与适配器加入同一个现有网络，再把 Internal Integration 的 Webhook URL 配成 `http://sentry-adapter:8787/webhooks/sentry/user-feedback`。这里的 `sentry-adapter` 是 Docker 网络别名，不需要企业 DNS 记录，也不需要对外发布 8787 端口。`SENTRY_PROXY_NETWORK` 虽沿用“proxy”变量名，但此处填写的是 **Sentry 发送容器所在的 Docker 网络名**。

先在部署机查看实际容器和网络；从网络的子网中选一个未被占用的 IPv4 地址作为适配器的固定地址。固定地址用于 Sentry 的精确放行规则，不能把下面的占位符原样执行：

```bash
docker ps --format '{{.Names}}'
SENTRY_SENDER='实际发送 Webhook 的 Sentry 容器名'
docker inspect "$SENTRY_SENDER" --format '{{range $name, $_ := .NetworkSettings.Networks}}{{println $name}}{{end}}'
SENTRY_PROXY_NETWORK='从上条输出选出的共享网络名'
docker network inspect "$SENTRY_PROXY_NETWORK" --format '{{range .IPAM.Config}}{{println .Subnet}}{{end}}'
docker network inspect "$SENTRY_PROXY_NETWORK" --format '{{range .Containers}}{{println .Name .IPv4Address}}{{end}}'
SENTRY_ADAPTER_IPV4='该子网内尚未占用的 IPv4 地址'

make deploy-image IMAGE=sentry-adapter:1.0.0 \
  SENTRY_PROXY_NETWORK="$SENTRY_PROXY_NETWORK" \
  SENTRY_ADAPTER_IPV4="$SENTRY_ADAPTER_IPV4"
```

若尚未打镜像，把 `deploy-image` 换成 `deploy-real`。`compose.proxy.yaml` 负责加入现有网络并设置别名，`compose.static-ip.yaml` 设置固定 IP；部署命令会先执行 `docker compose config --quiet` 检查合并后的配置。后续 `make status`、`make logs`、`make stop` 及重新部署时，继续传入相同的两个网络参数。共享网络须有包含所选地址的 IPv4 子网。[Docker 跨项目网络](https://docs.docker.com/compose/how-tos/networking/#connecting-multiple-compose-projects)和[固定 IP 配置](https://docs.docker.com/reference/compose-file/services/#ipv4_address-ipv6_address)说明了这些前提。

Sentry 26.6 默认禁止向 Docker 私有地址发送 Webhook。需在 **Sentry 部署的** `sentry/sentry.conf.py` 中增加精确到适配器地址的规则（替换为上面选定的实际 IP），然后重新创建或重启负责发送 Webhook 的 Sentry 进程，使配置生效：

```python
SENTRY_ALLOWED_IPS = ("<SENTRY_ADAPTER_IPV4>/32",)
```

保留 Sentry 的 `SENTRY_DISALLOWED_IPS` 默认值；只为适配器放行一个地址。Sentry 26.6 的[默认私网限制](https://github.com/getsentry/sentry/blob/26.6.0/src/sentry/conf/server.py#L107-L156)与[允许列表优先级](https://github.com/getsentry/sentry/blob/26.6.0/src/sentry/net/socket.py#L19-L42)是此配置的依据。若 `SENTRY_ENSURE_FQDN` 被手动设为 `True`，单段网络别名可能不能按预期解析，应先检查 Sentry 的实际 DNS 设置。

最后在目标项目的 Sentry Internal Integration 中，把 Webhook URL 改为：

```text
http://sentry-adapter:8787/webhooks/sentry/user-feedback
```

从 Sentry 发送容器的网络命名空间做一次不带签名的测试；收到适配器的 `401 invalid signature` 就说明网络和地址已通，且不会创建作业：

```bash
docker run --rm --network "container:${SENTRY_SENDER}" curlimages/curl:latest -i \
  -H 'Content-Type: application/json' --data '{}' \
  http://sentry-adapter:8787/webhooks/sentry/user-feedback
```

这条 curl 不会读取 Sentry 进程的 `SENTRY_ALLOWED_IPS` 配置；还须从目标 Sentry 项目发送一条真实用户反馈，核对 Webhook 是否投递及 Teambition 是否建卡。`SENTRY_BASE_URL` 仍保留用户可访问的 `https://sentry.gwm-adas.com`，因为适配器用它调用 Sentry API，并生成 Teambition 备注中的反馈链接。若 Sentry 直接运行在宿主机而非容器中，可使用现有的 `http://127.0.0.1:8787/webhooks/sentry/user-feedback` 宿主机映射，并在 Sentry 中只放行 `127.0.0.1/32`。

### 分段检查网络路径

Sentry 的 Webhook 发送容器必须能够访问实际配置的 Webhook URL；适配器容器还必须能够访问 `SENTRY_BASE_URL` 和私有 Teambition 网关。如果继续使用 `https://sentry-adapter.gwm-adas.com`，同机部署也可能遇到域名解析到宿主机公网地址后无法回流的问题。下面的临时 curl 容器共享目标容器的网络命名空间，不要求精简的适配器运行镜像内安装 curl：

```bash
# 在部署机执行；选实际负责发送 Webhook 的 Sentry 容器名
SENTRY_SENDER='填入实际发送Webhook的Sentry容器名'
docker run --rm --network "container:${SENTRY_SENDER}" curlimages/curl:latest -i \
  -H 'Content-Type: application/json' --data '{}' \
  https://sentry-adapter.gwm-adas.com/webhooks/sentry/user-feedback

ADAPTER_ID="$(docker compose -f compose.yaml ps -q adapter)"
docker run --rm --network "container:${ADAPTER_ID}" curlimages/curl:latest -sS \
  -o /dev/null -w 'Sentry HTTP=%{http_code}\n' https://sentry.gwm-adas.com/api/0/
make teambition-check-docker
```

第一条未签名 Webhook 请求预期返回适配器的 `401 invalid signature`，只用于验证 Sentry 容器经 DNS、TLS 和反向代理到达适配器，不会创建作业。第二条的地址对应示例 `.env` 的 `SENTRY_BASE_URL`；如果改了配置，请同步替换。该请求不带 Sentry Token，收到 HTTP 响应只能证明网络路径存在；真实反馈才能验证 Token、Issue/Event API 和字段。Teambition 探针只验证 `/gateway/appToken`，不调用建卡接口。`SENTRY_BASE_URL` 同时用于 Sentry API 请求和 Teambition 备注中的反馈链接，不要为了容器内连通性直接改成用户无法访问的内部 HTTP 地址；需要让原 HTTPS 域名从容器内可达，或另行拆分 API 地址和展示地址。

用户已在可访问私有网关的环境中成功调用 `/gateway/appToken` 和 `/gateway/v3/task/create`。本机当前请求私有 `/gateway/appToken` 仍返回 403，因此这里的 Go 测试验证的是请求格式和响应处理，尚未在本机创建真实任务。此前公共 `POST https://open.teambition.com/api/appToken` 返回 HTTP 200，但公共 token 不能替代私有环境的联调结论。官方 Go SDK 的旧响应模型把 `appToken` 和 `expire` 放在 `result` 下，客户端兼容顶层与嵌套两种结构。

`SENTRY_ATTACHMENT_REDIRECT_HOSTS` 目前不参与第一版业务链路；只有后续启用截图下载时才需要配置。

## 作业状态与排障

`pending/retry → processing → creating → done` 是成功路径。取 token 发生在 `processing`，任务创建发生在 `creating`。非目标反馈变为 `ignored`；必需字段不足变为 `needs_review`；建卡结果不确定变为 `uncertain`。`uncertain` 必须先在 Teambition 核对是否已有对应 Sentry 反馈链接的任务，再人工处理。重启时 `creating` 的过期租约也会转为 `uncertain`。

可从 PostgreSQL 查看脱敏的处理状态：

```sql
SELECT id, issue_id, status, task_id, attempts, last_error
FROM feedback_jobs ORDER BY id DESC LIMIT 20;
```

上线前还必须用当前 `SENTRY_PROJECT` 对应的真实反馈验证 Webhook 是否实际投递，以及 Sentry 26.6 的 Event JSON 和反馈链接。若目标版本没有发送 FEEDBACK 类 `issue.created`，需要设计其他触发方式；当前版本尚未实现轮询。

## 官方接口依据

- [Sentry Webhook 签名与投递](https://docs.sentry.io/integrations/integration-platform/webhooks/)
- [Issue Webhook 与 FEEDBACK 类别](https://docs.sentry.io/integrations/integration-platform/webhooks/issues/)
- [Sentry Issue Event](https://docs.sentry.io/api/events/retrieve-an-issue-event/)
- [Teambition 官方 Go SDK 的 appToken 接口](https://github.com/teambition/openapi-sdk-golang/blob/master/api_app.go)、[自签应用 JWT](https://open.teambition.com/docs/documents/5db8f7e77baeb50014957fc1)、[创建任务](https://open.teambition.com/docs/apis/6321c6d1912d20d3b5a4a514)。
