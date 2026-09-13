# sub2api Prompt Audit Sidecar

独立部署在客户端与 sub2api 之间的反向代理，用于把用户提交的 prompt 写入独立
PostgreSQL 表，供管理员人工审计。它不修改 sub2api 源码，也不调用第三方审计模型。

```text
client -> sidecar_proxy:8090 -> sub2api:8080
                    |
                    +-> prompt_audit PostgreSQL
                    |
                    +-> sub2api PostgreSQL (read-only identity lookup)
```

## 核心边界

- 只保存请求中的用户输入、请求时间、用户/API Key 归属、请求 ID、入口路径和请求模型。
- 不保存上游响应的 body 或 headers，WebSocket 服务端帧也不参与审计。
- 不保存原始请求 body、`Authorization`、`x-api-key`、`x-goog-api-key` 或原始 API Key。
- 审计副本会排除 `system`、`developer`、`assistant`、`tool`、OpenAI
  `instructions`、Anthropic `system`、Gemini `systemInstruction` 等系统内容。
- 排除只发生在审计副本中；转发给 sub2api 的原始请求不会被修改。
- 未知格式不会退化为保存完整 JSON，而是跳过或标记为不可解析。
- 审计写库使用异步有界队列。数据库不可用或队列已满时丢弃审计副本，正常请求仍继续转发。

> Sidecar 位于请求链路中，因此 Sidecar 进程本身不可用时会影响流量。生产环境应由
> Docker/systemd 自动拉起，并先在旁路端口验证后再切换入口。审计数据库故障不会阻断代理。

## 支持的请求

| 协议 | 提取的用户内容 | 明确排除 |
| --- | --- | --- |
| OpenAI Chat Completions | `messages[role=user]` 的文本 | system/developer/assistant/tool |
| OpenAI Responses | `input` 中的用户文本 | `instructions` 和非用户角色 |
| Anthropic Messages | `messages[role=user]` 的文本 | 顶层 `system` 和非用户角色 |
| Gemini | `contents` 中的用户文本 | `systemInstruction`、system/model 内容 |
| Realtime WebSocket | 客户端文本帧中的用户输入 | 所有服务端帧及系统内容 |
| 常见表单/通用 JSON | 白名单字段 `prompt/query/input/text/description` | 未知字段和原始 body |

图片、音频、文件等二进制内容不会落入 prompt 表。超过采集大小、压缩编码或无法识别的
请求仍会原样转发，但不会被审计。

## 快速启动

### 1. 准备配置

```bash
git clone https://github.com/Ssshake1996/sidecar_proxy.git
cd sidecar_proxy
cp .env.example .env
```

生成两个不同的随机密钥，并写入 `.env`：

```bash
openssl rand -hex 32
openssl rand -hex 32
```

至少修改以下配置：

- `PROMPT_AUDIT_UPSTREAM_URL`：sub2api 的地址。
- `PROMPT_AUDIT_IDENTITY_DATABASE_URL`：sub2api 数据库的只读连接串。
- `PROMPT_AUDIT_IDENTITY_HMAC_SECRET`：API Key 内存指纹密钥。
- `PROMPT_AUDIT_ADMIN_TOKEN`：人工审计页面的访问令牌。
- `PROMPT_AUDIT_DB_PASSWORD`：独立审计数据库密码。

连接串中的特殊字符必须进行 URL 编码。`.env` 已被 `.gitignore` 排除，不要提交真实密钥。

### 2. 创建 sub2api 只读账号

使用 sub2api 数据库管理员执行以下 SQL，按实际数据库名和密码修改：

```sql
CREATE ROLE sub2api_audit_reader LOGIN PASSWORD 'change_me';
GRANT CONNECT ON DATABASE sub2api TO sub2api_audit_reader;
GRANT USAGE ON SCHEMA public TO sub2api_audit_reader;
GRANT SELECT ON TABLE api_keys, users, usage_logs TO sub2api_audit_reader;
```

此账号不需要写权限。Sidecar 定期读取 API Key 与用户的映射，在内存中只保留
HMAC 指纹；原始 Key 不写入审计库或日志。`usage_logs.request_id` 用作身份补全来源。

用户定位粒度是 **API Key 所属用户**。如果多个人共用一个 API Key，Sidecar 无法仅凭
现有请求区分真实操作人；需要为每个人分配独立 API Key。

### 3. 配置容器网络

`.env.example` 默认通过 `host.docker.internal` 访问宿主机上已发布端口的 sub2api 和
PostgreSQL，Linux 的 Compose 配置已经加入对应 host-gateway 映射：

```dotenv
PROMPT_AUDIT_UPSTREAM_URL=http://host.docker.internal:8080
PROMPT_AUDIT_IDENTITY_DATABASE_URL=postgres://sub2api_audit_reader:change_me@host.docker.internal:5432/sub2api?sslmode=disable
```

如果 sub2api/PostgreSQL 只在 Docker 内部网络开放，使用仓库中的网络覆盖文件：

```bash
docker network ls
cp docker-compose.network.yml.example docker-compose.network.yml
```

在 `.env` 中填写真实网络名及容器服务名：

```dotenv
SUB2API_DOCKER_NETWORK=sub2api_default
PROMPT_AUDIT_UPSTREAM_URL=http://sub2api:8080
PROMPT_AUDIT_IDENTITY_DATABASE_URL=postgres://sub2api_audit_reader:change_me@postgres:5432/sub2api?sslmode=disable
```

### 4. 启动

通过宿主机端口连接 sub2api：

```bash
docker compose up -d --build
```

通过现有 Docker 网络连接 sub2api：

```bash
docker compose -f docker-compose.yml -f docker-compose.network.yml up -d --build
```

检查状态：

```bash
curl -i http://127.0.0.1:8090/healthz
docker compose logs -f sidecar-proxy
```

`/healthz` 返回 `200` 表示代理和审计表可用；返回 `503 degraded` 表示审计库当前
不可用，但代理仍会继续转发请求。

### 5. 切换流量

把原先客户端的 sub2api Base URL 从 `http://<host>:8080` 改为
`http://<host>:8090`，API 路径和 API Key 均保持不变。建议先发送一条测试请求，确认：

1. 客户端正常收到 sub2api 响应。
2. `/admin/ui` 出现对应用户输入。
3. 系统提示词、响应内容和原始 API Key 未出现在审计表。

若入口由 Nginx 管理，只需把原 upstream 从 sub2api 的 `8080` 改为 Sidecar 的
`8090`。回滚时改回原端口，不需要修改或重启 sub2api。

## 人工审计

浏览器打开：

```text
http://127.0.0.1:8090/admin/ui
```

输入 `PROMPT_AUDIT_ADMIN_TOKEN` 后，可以按用户 ID 或关键字筛选，查看请求时间、用户、
模型、路径和用户 prompt，并设置审计状态：

- `pending`：待审计
- `approved`：通过
- `flagged`：需要处理
- `ignored`：忽略

页面不把 token 写入 localStorage。管理 API 和代理共用监听端口，生产环境应在防火墙或
反向代理中限制 `/admin` 只能从管理网络访问。

### 管理 API

列表接口支持 `user_id`、`request_id`、`q`、`from`、`to`、`limit`、`offset`。
`from`/`to` 使用 RFC 3339 时间：

```bash
curl -H "Authorization: Bearer $PROMPT_AUDIT_ADMIN_TOKEN" \
  'http://127.0.0.1:8090/admin/records?user_id=12&from=2026-09-01T00:00:00Z&limit=50'

curl -H "Authorization: Bearer $PROMPT_AUDIT_ADMIN_TOKEN" \
  'http://127.0.0.1:8090/admin/records/<record-id>'

curl -X PATCH \
  -H "Authorization: Bearer $PROMPT_AUDIT_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"review_status":"flagged","reviewer":"admin","review_note":"需要复核"}' \
  'http://127.0.0.1:8090/admin/records/<record-id>'
```

也可以直接查询独立审计库：

```sql
SELECT received_at, user_id, username_snapshot, email_snapshot, prompt_text
FROM manual_prompt_audit_records
ORDER BY received_at DESC
LIMIT 100;
```

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PROMPT_AUDIT_LISTEN_ADDR` | `:8090` | HTTP/WebSocket 监听地址 |
| `PROMPT_AUDIT_UPSTREAM_URL` | 无 | sub2api 绝对 HTTP(S) URL，必填 |
| `PROMPT_AUDIT_DATABASE_URL` | 无 | Sidecar 独立 PostgreSQL 连接串，必填 |
| `PROMPT_AUDIT_IDENTITY_DATABASE_URL` | 空 | sub2api PostgreSQL 只读连接串 |
| `PROMPT_AUDIT_IDENTITY_HMAC_SECRET` | 空 | 身份指纹密钥；配置身份库时必填 |
| `PROMPT_AUDIT_IDENTITY_REFRESH` | `30s` | API Key/用户映射刷新周期 |
| `PROMPT_AUDIT_ADMIN_TOKEN` | 空 | 管理 API Bearer token；为空时关闭数据 API |
| `PROMPT_AUDIT_CAPTURE_PREFIXES` | `/v1,/v1beta` | 需要解析的入口路径前缀 |
| `PROMPT_AUDIT_MAX_CAPTURE_BYTES` | `1048576` | 单个 HTTP 请求最大解析字节数 |
| `PROMPT_AUDIT_MAX_PROMPT_BYTES` | `1048576` | 单条合并 prompt 最大落库字节数 |
| `PROMPT_AUDIT_QUEUE_SIZE` | `2048` | 异步写入队列容量 |
| `PROMPT_AUDIT_CAPTURE_UNKNOWN_IDENTITY` | `false` | 是否保存无法关联用户的记录 |
| `PROMPT_AUDIT_CAPTURE_EMPTY_PROMPTS` | `false` | 是否保存没有用户文本的记录 |
| `PROMPT_AUDIT_RETENTION_DAYS` | `90`（示例） | 大于 0 时每天自动删除过期记录；程序默认 0 |
| `PROMPT_AUDIT_WEBSOCKET` | `true` | 是否透明代理并审计 WebSocket 客户端文本帧 |
| `PROMPT_AUDIT_SHUTDOWN_TIMEOUT` | `10s` | 优雅退出等待时间 |

默认不保存身份未知或没有用户文本的记录，以避免产生无法归属的数据。若身份数据库暂时
不可用，可设置 `PROMPT_AUDIT_CAPTURE_UNKNOWN_IDENTITY=true`，但这类记录可能无法自动
补全为具体用户。

## 本地构建与测试

需要 Go 1.27：

```bash
go test -race ./...
go vet ./...
go build -trimpath -o sidecar-proxy .
```

不使用 Docker 时，先准备 PostgreSQL，然后直接设置 `PROMPT_AUDIT_DATABASE_URL` 等环境
变量运行 `./sidecar-proxy`。程序会自动创建 `manual_prompt_audit_records` 及索引。

## 升级与兼容性

这个仓库不导入 sub2api 内部 Go package，也不写入 sub2api 数据库，因此 sub2api 常规
升级通常不需要重新开发或重新编译 Sidecar。以下变化才需要单独适配：

- sub2api 改动 `api_keys`、`users`、`usage_logs` 的表名或关键字段。
- 上游新增协议，或已有协议改变用户输入字段结构。
- 客户端启用当前不解析的压缩/二进制请求格式。

未知格式不会保存完整请求作为兜底，因此协议变化的结果是“漏审计”，而不是把系统提示词
意外写入数据库。升级 sub2api 前可先在测试流量中验证身份关联和 prompt 提取。

## 运行注意事项

- Sidecar 必须部署在能看到解密后 HTTP body 的位置；仅做四层 TCP 转发无法提取 prompt。
- 建议 TLS 在 Nginx/负载均衡器终止，再转发到 Sidecar，或让 Sidecar 通过 HTTPS 访问上游。
- 审计库包含用户输入和用户标识，应配置最小权限、备份加密和合理的保留周期。
- 监控 `/healthz` 中的 `queue_dropped`。持续增长表示数据库过慢或队列容量不足。
- 审计写入是 fail-open 设计，不提供“每个成功请求必有审计记录”的强一致保证。
