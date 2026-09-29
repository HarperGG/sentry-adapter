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
