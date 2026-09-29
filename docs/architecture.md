# Sentry User Feedback → Teambition Adapter：架构与实现状态

> 本文按当前仓库的第一版代码描述实现。Sentry 26.6 与 Teambition 私有实例的真实端到端联调尚未完成；“待验证”和“后续设计”均不表示已上线。

## 1. 目标与边界

服务接收自建 Sentry 的 Issue webhook，从指定组织和项目读取 User Feedback 的 Issue、Event，在固定的 Teambition 项目创建一张需求或缺陷任务。第一版不查询、下载或上传截图，任务备注只放 Sentry 反馈链接。目标 Sentry 项目由 `SENTRY_PROJECT` 配置，当前本地示例为 `anno-seg-test`。业务键是 Sentry Issue ID：同一 Issue 的重复投递在本地只产生一个作业。若真实反馈证明一个 Issue 可包含多条应分别建卡的反馈，需要先重新定义业务键并迁移已有数据。

已知 Sentry Internal Integration 配置了 https://sentry-adapter.gwm-adas.com/webhooks/sentry/user-feedback，订阅 Issue webhook；实际 FEEDBACK 类 issue.created 是否投递仍要在目标 Sentry 26.6 实例验证。解析器也支持 event_alert.triggered，但当前未配置 Alert Rule Action。

## 2. 技术选型与部署拓扑

- Go 1.27 的标准库 net/http、context、slog、http.Client 承担路由、超时和结构化日志。入口只有 webhook 与健康检查，暂不需要 Gin/Echo；[Go ServeMux](https://go.dev/blog/routing-enhancements/) 已支持按方法和路径注册。
- PostgreSQL 17 与 pgx/v5 保存并领取持久作业。第一版只有 feedback_jobs 表，不依赖 Redis 或消息代理。
- 同一个 Go 进程运行 HTTP server 与轮询 worker。Compose 部署 app 和 db，数据库使用持久卷；app 只把 8787 端口绑定到主机回环地址，预期由现有 HTTPS 反向代理转发。
- [Docker 多阶段构建](https://docs.docker.com/build/building/best-practices/)编译静态 Go 二进制，运行层采用 `scratch`、数字非 root 用户和从构建镜像复制的 CA 证书束；Compose 通过 [db 健康检查与 depends_on](https://docs.docker.com/compose/how-tos/startup-order/)控制启动顺序。
- Teambition 企业内部应用鉴权默认用 AppID、AppSecret 向私有 `POST /gateway/appToken` 换取应用 token，按接口返回的有效期缓存，并作为 Bearer Token 调用任务创建接口；同时保留用户提供的 Python 本地签发 HS256 JWT 方式，由 `TEAMBITION_AUTH_MODE` 选择。第一版只依赖取 token 和建卡两个 Teambition 接口；图片上传辅助代码保留供后续联调，不在当前业务链路执行。

~~~mermaid
flowchart LR
    S[Sentry Issue webhook] --> P[HTTPS 反向代理]
    P --> H[Go net/http]
    H --> D[(PostgreSQL: feedback_jobs)]
    D --> W[Go worker]
    W --> SA[Sentry Issue / Event API]
    W --> J[获取并缓存 appToken]
    J --> TA[Teambition 建卡 API]
    W --> D
~~~

## 3. 第一版实际数据流

1. **接收与验签。** POST /webhooks/sentry/user-feedback 限制 JSON 请求体为 1 MiB，以原始字节和 Sentry-Hook-Signature 做 HMAC-SHA256 验证。解析器只接收 FEEDBACK 类 issue.created 或符合条件的 event_alert.triggered；其他已验签投递返回 204。Sentry Webhook 的[签名和响应时限](https://docs.sentry.io/integrations/integration-platform/webhooks/)以及 [Issue FEEDBACK 事件](https://docs.sentry.io/integrations/integration-platform/webhooks/issues/)是此入口的协议依据。
2. **持久化后应答。** 对接收的事件生成 issue:<issue_id> 去重键，向 feedback_jobs 做唯一键插入；重复投递命中 ON CONFLICT DO NOTHING。写入成功或已存在后返回 202，数据库写入失败返回 503。没有独立 webhook_inbox，也没有把原始反馈正文写入数据库。Sentry 对失败响应是否重投仍需实测。
3. **领取并核实反馈。** worker 每秒尝试领取一个 pending/retry 作业，SQL 使用 FOR UPDATE SKIP LOCKED，并记录 attempts 与 10 分钟租约。worker 再调用 Sentry Issue 与 Event API 核实 issueCategory、项目 slug，并从反馈上下文或事件 tags 取分类；不能只相信 webhook payload。非目标项目或非反馈 Issue 标为 ignored。无法映射类别或没有反馈链接的作业标为 needs_review；第一版不要求反馈描述非空。
4. **转换卡片。** worker 将 platform_bug/feature_gap 等映射为缺陷，将 suggestion/other 等映射为需求。`content` 包含具体反馈类型与非泛化 Issue 标题，`note` 仅是一个 Sentry 反馈链接。链接优先按项目 slug、Issue ID 和项目数值 ID 生成当前 Sentry UI 使用的 `issues/feedback/?feedbackSlug=...&project=...` 地址；缺字段时回退到 API permalink 或 Issue 地址。第一版不请求 Sentry 附件接口，也不下载截图。
5. **鉴权后建卡。** worker 调用 Teambition.Prepare：真实模式按需获取并缓存 appToken（或按配置在本地签发 JWT），不会准备图片。Prepare 成功后，数据库由 processing 转为 creating，才发送任务创建请求。若取 token 失败，作业仍在尚未发建卡请求的 processing 阶段重试或进入 needs_review。
6. **确认结果。** Teambition 返回明确 task ID 且数据库成功写入后才置为 done。建卡请求超时、网络错误、5xx、响应不完整或 task ID 缺失时进入 uncertain；此状态不会被 worker 自动再次领取。明确的永久错误进入 needs_review。若创建成功但保存 task ID 失败，creating 租约过期后同样转 uncertain，防止盲目再建卡。

用户提供的 Python 示例在本地签发含 `_appId`、`iat`、`exp` 的 HS256 JWT，有效期 2 小时；该方式保留为 `local_jwt` 选项。默认在线模式使用已由用户手动验证的私有 `POST https://teambition.gwm.cn/gateway/appToken`，距返回过期时间不足 100 秒时重新获取。任务地址是 `https://teambition.gwm.cn/gateway/v3/task/create`，请求带 `Bearer` token、操作人及租户 Header；body 带 `objectType=task`、`involveMembers=[操作人 ID]`、共用的 `scenariofieldconfigId` 和已确认的下拉 `customfields`。本机访问私有取 token 接口仍为 403，不能从当前环境完成真实建卡联调；用户已在其可访问环境手动建卡成功。截图上传接口尚未联通，因此第一版明确跳过截图流程。

## 4. 当前模块和 HTTP 接口

| 代码位置 | 当前职责 |
| --- | --- |
| cmd/adapter | 加载配置、迁移、启动 HTTP 与 worker、优雅退出；注册三个 HTTP 路由 |
| internal/webhook | 基于原始请求体验签、过滤动作、提取稳定 ID |
| internal/sentry | 第一版读取 Issue/Event 并生成反馈链接；截图辅助代码暂不调用 |
| internal/teambition | 在线 appToken 获取与缓存、本地 JWT、建卡请求与 Mock；上传辅助代码暂不调用 |
| internal/worker | 业务过滤、字段映射、调用 Prepare/Create、按失败阶段更新状态 |
| internal/store | feedback_jobs 迁移、去重插入、租约领取及状态转换 |
| internal/config | 环境变量、必需值检查及敏感值的 _FILE 读取 |

HTTP 路由实际为 POST /webhooks/sentry/user-feedback、GET /health/live（进程存活）和 GET /health/ready（数据库 Ping）。当前没有 /healthz、/readyz、人工重放或管理 API。readiness 只检查数据库连接，不对 Sentry/Teambition 做主动探测。

## 5. 第一版数据模型与状态机

数据库迁移在启动时由 internal/store 执行。当前唯一业务表 feedback_jobs 包含 dedup_key、organization、project、issue_id、event_id、status、attempts、next_attempt_at、lease_until、task_id、last_error 和时间戳；dedup_key 唯一。它同时承担去重、作业队列和已确认任务 ID 的映射。当前部署锁定一个 Sentry 组织与目标项目；如果扩成多租户，issue:<issue_id> 必须重新评估是否足以隔离租户。

~~~mermaid
stateDiagram-v2
    [*] --> pending
    pending --> processing: Claim
    retry --> processing: Claim
    processing --> ignored: 非目标反馈
    processing --> needs_review: 永久数据/鉴权错误或达到重试上限
    processing --> retry: 可重试的读取/准备错误或租约过期
    processing --> creating: Prepare 成功，MarkCreating
    creating --> done: 确认 task ID 并写库
    creating --> needs_review: 明确的永久建卡错误
    creating --> uncertain: 创建结果不明或租约过期
~~~

processing 的可恢复错误最多尝试 5 次，当前退避为 2 的 attempts 次方秒，没有抖动。worker 启动时及运行中会恢复过期租约：processing → retry，creating → uncertain。uncertain 和 needs_review 需要人工核对与处理；目前没有自动查询 Teambition 是否已有任务、没有受控重放 CLI，也没有服务端幂等键已验证可用的证据。人工重发建卡前，应按任务备注中的 Sentry 反馈链接在 Teambition 查证；不能因本地 task_id 为空就推断未建卡。

## 6. 已有安全措施与运维边界

- Sentry webhook 验签使用常量时间比较；入口限制请求体、头大小与读写超时，只有成功入库才应答 202。Sentry 数据由受配置约束的 API 客户端读取。
- 第一版不下载或上传截图。预留的图片辅助代码只允许配置的重定向/上传来源，当前业务链路不会调用。
- Docker 运行层为非 root、只读文件系统并移除 Linux capabilities。Compose 把数据库密码和 Sentry 密钥从 secret 文件注入；真实模式的 Teambition AppID/AppSecret 从单独、被 Git 忽略的 `config/credentials.env` 作为容器环境变量注入。程序仍支持 AppSecret 的 `_FILE` 形式。日志不应记录 token、反馈正文、图片字节或上传 URL。
- `teambition-probe` 是独立的运维诊断程序，本地 Swagger 只监听 loopback。从 `.env`、`config/credentials.env` 或同名环境变量读取配置，请求私有 appToken，并可用已有任务 ID 执行只读查询。浏览器只接收 HTTP 状态、分类及耗时，不接收凭据、token 或任务正文。独立的 `compose.probe.yaml` 可从与适配器相同的 Compose 项目网络执行一次容器内检查，不暴露端口。它不调用建卡接口；先确认连通性，再做适配器端到端联调。
- 当前使用 slog JSON 日志、数据库作业状态和健康检查排障；没有专门的指标端点、自动告警、备份任务、双验签密钥轮换或人工核对工具。数据库备份、反向代理 TLS 与日志保留策略需由部署环境落实。

## 7. 待验证与后续设计

**真实环境待验证：**

- 用一条脱敏的 Sentry 26.6 反馈验证 FEEDBACK issue.created 的投递、签名、分类字段、反馈链接和失败响应重试。[Issue Event API](https://docs.sentry.io/api/events/retrieve-an-issue-event/)可作为接口依据。如果目标版本不发送这类 webhook，需另行设计触发方式；当前没有轮询。
- 在可访问私有网关的环境用适配器完整验证私有 appToken 响应、任务响应中的 task ID，以及 `note` 中单独的 Sentry 反馈链接。[私有部署网关](https://open.teambition.com/docs/apis/63a08189912d20d3b56b95f6)、[官方 Go SDK 的 appToken 接口](https://github.com/teambition/openapi-sdk-golang/blob/master/api_app.go)与[创建任务](https://open.teambition.com/docs/apis/6321c6d1912d20d3b5a4a514)是官方接口参考。本机私有取 token 接口仍返回 403，真实联调须在可访问私有网关的环境执行。
- 用重复 webhook、任务创建超时、Teambition 5xx、数据库断连和 worker 重启做故障演练；确认每种状态及人工核对流程。不要将本地单测或 Mock 成功等同于真实端到端验证。

**后续方案，当前未实现：**截图上传接口联通并确认文件字段协议后，可另行启用截图流程及附件补偿。若需保存投递审计、跨项目映射，可增加 webhook_inbox、task_mappings；同时设计清理期限与迁移。若确定要支持多反馈每 Issue、多租户、水平扩容或自动重放，先明确新的业务键、Teambition 查询/幂等能力及人工操作约束。监控指标、告警和恢复 CLI 也属于后续工作。
