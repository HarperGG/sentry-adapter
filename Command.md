# 命令

## 查询sql数据

```bash
docker compose -f compose.yaml logs --tail=100 adapter

docker compose -f compose.yaml exec -T db \
  psql -U adapter -d adapter \
  -c 'SELECT id, issue_id, status, task_id, attempts, last_error FROM feedback_jobs ORDER BY id DESC LIMIT 10;'
```

## 构建image

```bash
make deploy-image IMAGE=sentry-adapter:1.0.0
```

## 报错： ✘ Container sentry-adapter-db-1 Error response from daemon: invalid mount config for type "bind": bind source path does not exist: /data/sentry-adapter/secrets/db_password

### 日志显示数据库卷刚创建。如果这是首次部署，且 db_password、database_url 两个文件都不存在，在节点执行

```bash
cd /data/sentry-adapter
install -d -m 0700 secrets
umask 077

openssl rand -hex 24 > secrets/db_password
printf 'postgres://adapter:%s@db:5432/adapter?sslmode=disable' \
  "$(cat secrets/db_password)" > secrets/database_url
```

### 再把实际的 Sentry Internal Integration 签名 Secret 和有目标项目读取权限的 Sentry API Token 分别写入以下文件。可用隐藏输入，值不会出现在命令历史中，HOOK_SECRET 和 API_TOKEN 去sentry获取

```bash
read -rsp 'Sentry Webhook Secret: ' HOOK_SECRET; printf '\n'
printf '%s' "$HOOK_SECRET" > secrets/sentry_webhook_secret
unset HOOK_SECRET

read -rsp 'Sentry API Token: ' API_TOKEN; printf '\n'
printf '%s' "$API_TOKEN" > secrets/sentry_auth_token
unset API_TOKEN

chmod 0644 secrets/db_password secrets/database_url \
  secrets/sentry_webhook_secret secrets/sentry_auth_token
```
