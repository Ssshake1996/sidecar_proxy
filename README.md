# sub2api Prompt Audit Sidecar

独立部署在客户端与 sub2api 之间的反向代理，用于把用户提交的 prompt 写入独立 PostgreSQL 表，供管理员人工审计。它不修改 sub2api 源码，也不调用第三方审计模型。

~~~
client -> sidecar_proxy HTTP :8090  -> sub2api
       -> sidecar_proxy HTTPS:8443  -> sub2api
                         |
                         +-> 独立 prompt_audit PostgreSQL
                         +-> sub2api PostgreSQL（只读身份查询）
~~~

## 数据边界

- 保存用户输入、请求时间、API Key 所属用户、请求 ID、入口路径和模型。
- 不保存上游响应 body、响应 headers 或 WebSocket 服务端帧。
- 不保存原始请求 body、Authorization、x-api-key、x-goog-api-key 或原始 API Key。
- 审计副本排除 system、developer、assistant、tool、OpenAI instructions、Anthropic system、Gemini systemInstruction 等系统内容。
- 排除只发生在审计副本中；转发给 sub2api 的原始请求不被修改。
- 未知格式不会退化为保存完整 JSON，而是跳过审计或标记为不可解析。
- 审计写库使用异步有界队列。审计库不可用或队列已满时丢弃审计副本，正常请求继续转发。

> HTTP 是明文协议。公网环境建议使用 HTTPS；HTTP 仅用于兼容、内网或由外部网关负责 TLS 的场景。Sidecar 同时支持直接 TLS 监听和外部 Nginx/Caddy TLS 终止。

## 支持的请求

| 协议 | 提取的用户内容 | 明确排除 |
| --- | --- | --- |
| OpenAI Chat Completions | messages 中 role=user 的文本 | system/developer/assistant/tool |
| OpenAI Responses | input 中的用户文本 | instructions 和非用户角色 |
| Anthropic Messages | messages 中 role=user 的文本 | 顶层 system 和非用户角色 |
| Gemini | contents 中的用户文本 | systemInstruction、system/model 内容 |
| Realtime WebSocket | 客户端文本帧中的用户输入 | 所有服务端帧及系统内容 |
| 常见表单/通用 JSON | 白名单字段 prompt/query/input/text/description | 未知字段和原始 body |

## 部署

### 1. 获取代码和配置

~~~
git clone https://github.com/Ssshake1996/sidecar_proxy.git
cd sidecar_proxy
cp .env.example .env
~~~

至少修改以下值：

- PROMPT_AUDIT_UPSTREAM_URL：sub2api 的地址。
- PROMPT_AUDIT_IDENTITY_DATABASE_URL：sub2api 数据库的只读连接串。
- PROMPT_AUDIT_IDENTITY_HMAC_SECRET：身份指纹密钥，可用 openssl rand -hex 32 生成。
- PROMPT_AUDIT_DB_PASSWORD：Sidecar 独立审计数据库密码。

PROMPT_AUDIT_ADMIN_PASSWORD 留空时，首次部署会自动生成随机密码。连接串中的特殊字符必须 URL 编码；.env 已被 Git 忽略，不要提交真实密钥。

### 2. 创建 sub2api 只读账号

使用 sub2api 数据库管理员执行以下 SQL，按实际数据库名和密码替换：

~~~
CREATE ROLE sub2api_audit_reader LOGIN PASSWORD 'change_me';
GRANT CONNECT ON DATABASE sub2api TO sub2api_audit_reader;
GRANT USAGE ON SCHEMA public TO sub2api_audit_reader;
GRANT SELECT ON TABLE api_keys, users, usage_logs TO sub2api_audit_reader;
~~~

此账号不需要写权限。Sidecar 定期读取 API Key 与用户映射，在内存中只保留 HMAC 指纹；原始 Key 不写入审计库或日志。usage_logs.request_id 用作身份补全来源。

用户定位粒度是 API Key 所属用户。多人共用一个 API Key 时，Sidecar 无法仅凭现有请求区分真实操作人，需要为每个人分配独立 API Key。

### 3. 配置 HTTP 和 HTTPS

HTTP 默认监听 :8090：

~~~
PROMPT_AUDIT_HTTP_ADDR=:8090
PROMPT_AUDIT_HTTPS_ADDR=
~~~

要让 Sidecar 直接提供 HTTPS：

~~~
mkdir -p certs
# 将域名对应的 fullchain.pem 和 privkey.pem 放入 certs/
~~~

然后修改 .env：

~~~
PROMPT_AUDIT_HTTPS_ADDR=:8443
PROMPT_AUDIT_TLS_CERT_FILE=/run/prompt-audit/certs/fullchain.pem
PROMPT_AUDIT_TLS_KEY_FILE=/run/prompt-audit/certs/privkey.pem
PROMPT_AUDIT_TLS_CERT_DIR=./certs
~~~

只有明确填写 PROMPT_AUDIT_HTTPS_ADDR 才会启动 HTTPS；证书和私钥必须同时配置。HTTP 和 HTTPS 可以同时启用。生产环境应使用受信任 CA 证书，不建议用自签名证书作为公网登录入口。

如果希望直接使用标准公网端口，可在 Compose 的 .env 中设置 PROMPT_AUDIT_PORT=80 和 PROMPT_AUDIT_HTTPS_PORT=443；也可以保持 8090/8443，由 Nginx/Caddy 代理到域名的 80/443。

如果由 Nginx/Caddy 终止 TLS，可让外部网关代理到 Sidecar 的 8090，并转发 X-Forwarded-Proto: https。Sidecar 会据此设置安全 Cookie 属性。

### 4. 启动

~~~
docker compose up -d --build
~~~

Compose 会创建独立的 audit-db，Sidecar 自动创建以下表和索引：

- manual_prompt_audit_records：prompt 审计记录。
- manual_prompt_audit_admins：管理员账号及 bcrypt 密码哈希。
- manual_prompt_audit_sessions：过期时间受限的会话令牌哈希。

审计库启动或网络暂时不可用时，Sidecar 会继续启动并在后台重试；恢复后自动建表并创建管理员账号。

检查代理：

~~~
curl -i http://127.0.0.1:8090/healthz
docker compose logs -f sidecar-proxy
~~~

首次创建管理员时，日志会打印一次类似下面的行：

~~~
PROMPT_AUDIT_ADMIN_INITIAL_CREDENTIALS username=admin password=<随机密码>
~~~

查看初始凭据：

~~~
docker compose logs sidecar-proxy | grep PROMPT_AUDIT_ADMIN_INITIAL_CREDENTIALS
~~~

已有管理员账号的数据库不会在重启时重新生成或覆盖密码。请把首次凭据保存到密码管理器；如果丢失，需要在维护窗口中处理管理员表后重新部署。

丢失密码时，可先停止 Sidecar，在 `.env` 设置一个新的 `PROMPT_AUDIT_ADMIN_PASSWORD`，再对独立审计库执行以下 SQL，最后重新启动：

~~~
DELETE FROM manual_prompt_audit_sessions;
DELETE FROM manual_prompt_audit_admins WHERE username = 'admin';
~~~

这会删除旧管理员及其会话；Sidecar 启动后会创建新账号并再次打印初始凭据。

### 5. 接入 sub2api

将客户端原先的 sub2api Base URL 改成 Sidecar 地址，API 路径和 API Key 保持不变：

~~~
HTTP : http://公网域名:8090
HTTPS: https://公网域名:8443
~~~

建议先发送测试请求，确认客户端仍能收到 sub2api 响应，同时审计页面能看到对应用户输入。回滚时把 Base URL 改回原 sub2api 地址即可，不需要修改 sub2api。

## 管理员登录和人工审计

登录地址：

~~~
http://公网域名:8090/admin/login
https://公网域名:8443/admin/login
~~~

登录后访问 /admin/ui。前端提供：

- 用户 ID、关键字和审计状态筛选。
- 服务端分页，单页最多 200 条。
- 列表只返回最多 512 字节 prompt 预览。
- 点击记录后才请求完整 prompt 详情。
- 审计状态：pending、approved、flagged、ignored。
- 退出登录和会话过期处理。

分页使用 limit 和 offset 参数，后端限制 offset 最大为 1,000,000，并使用时间、用户、审计状态索引，避免前端一次性加载全部数据。

## Linux 命令行访问

保存会话 Cookie 并登录：

~~~
BASE_URL=https://公网域名:8443
COOKIE_FILE=/tmp/prompt-audit.cookies

curl -i -c "$COOKIE_FILE" \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"首次启动日志中的密码"}' \
  "$BASE_URL/admin/login"
~~~

查询第 1 页：

~~~
curl -sS -b "$COOKIE_FILE" \
  "$BASE_URL/admin/records?limit=25&offset=0" | jq .
~~~

按用户和时间筛选：

~~~
curl -sS -b "$COOKIE_FILE" \
  "$BASE_URL/admin/records?user_id=12&from=2026-09-01T00:00:00Z&limit=25&offset=25" | jq .
~~~

查看某条记录的完整用户 prompt：

~~~
curl -sS -b "$COOKIE_FILE" \
  "$BASE_URL/admin/records/<record-id>" | jq .
~~~

标记审核结果并退出：

~~~
curl -sS -b "$COOKIE_FILE" -X PATCH \
  -H 'Content-Type: application/json' \
  -d '{"review_status":"flagged","reviewer":"admin","review_note":"需要复核"}' \
  "$BASE_URL/admin/records/<record-id>"

curl -sS -b "$COOKIE_FILE" -X POST "$BASE_URL/admin/logout"
~~~

也可以配置可选的 PROMPT_AUDIT_ADMIN_TOKEN 作为机器调用的 Bearer token；它不替代浏览器的账号登录，且应当像密码一样保密：

~~~
curl -H "Authorization: Bearer $PROMPT_AUDIT_ADMIN_TOKEN" \
  "$BASE_URL/admin/records?limit=25&offset=0"
~~~

如果使用自签名证书进行测试，可临时加 -k；公网生产环境不要关闭证书校验。

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| PROMPT_AUDIT_PORT | 8090 | Compose 映射的 HTTP 宿主机端口 |
| PROMPT_AUDIT_HTTPS_PORT | 8443 | Compose 映射的 HTTPS 宿主机端口 |
| PROMPT_AUDIT_TLS_CERT_DIR | ./certs | Compose 挂载证书目录 |
| PROMPT_AUDIT_HTTP_ADDR | :8090 | HTTP 监听地址；为空可关闭 HTTP |
| PROMPT_AUDIT_HTTPS_ADDR | 空 | HTTPS 监听地址；填写后必须配置证书和私钥 |
| PROMPT_AUDIT_TLS_CERT_FILE | 空 | TLS 证书文件，通常为 fullchain.pem |
| PROMPT_AUDIT_TLS_KEY_FILE | 空 | TLS 私钥文件，通常为 privkey.pem |
| PROMPT_AUDIT_LISTEN_ADDR | 兼容别名 | 旧版本 HTTP 地址配置 |
| PROMPT_AUDIT_UPSTREAM_URL | 无 | sub2api 绝对 HTTP(S) URL，必填 |
| PROMPT_AUDIT_DATABASE_URL | 无 | Sidecar 独立 PostgreSQL 连接串，必填 |
| PROMPT_AUDIT_IDENTITY_DATABASE_URL | 空 | sub2api PostgreSQL 只读连接串 |
| PROMPT_AUDIT_IDENTITY_HMAC_SECRET | 空 | 身份指纹密钥；配置身份库时必填 |
| PROMPT_AUDIT_IDENTITY_REFRESH | 30s | API Key/用户映射刷新周期 |
| PROMPT_AUDIT_ADMIN_USERNAME | admin | 首次自动创建的管理员用户名 |
| PROMPT_AUDIT_ADMIN_PASSWORD | 空 | 首次密码；为空则自动生成并打印 |
| PROMPT_AUDIT_ADMIN_SESSION_TTL | 12h | 登录会话有效期 |
| PROMPT_AUDIT_ADMIN_TOKEN | 空 | 可选机器调用 Bearer token |
| PROMPT_AUDIT_CAPTURE_PREFIXES | /v1,/v1beta | 需要解析的入口路径前缀 |
| PROMPT_AUDIT_MAX_CAPTURE_BYTES | 1048576 | 单个 HTTP 请求最大解析字节数 |
| PROMPT_AUDIT_MAX_PROMPT_BYTES | 1048576 | 单条合并 prompt 最大落库字节数 |
| PROMPT_AUDIT_QUEUE_SIZE | 2048 | 异步写入队列容量 |
| PROMPT_AUDIT_CAPTURE_UNKNOWN_IDENTITY | false | 是否保存无法关联用户的记录 |
| PROMPT_AUDIT_CAPTURE_EMPTY_PROMPTS | false | 是否保存没有用户文本的记录 |
| PROMPT_AUDIT_RETENTION_DAYS | 0 | 大于 0 时每天自动删除过期记录 |
| PROMPT_AUDIT_WEBSOCKET | true | 是否透明代理 WebSocket 客户端文本帧 |
| PROMPT_AUDIT_SHUTDOWN_TIMEOUT | 10s | 优雅退出等待时间 |

## Docker 内部网络

如果 sub2api/PostgreSQL 只在 Docker 网络内开放：

~~~
cp docker-compose.network.yml.example docker-compose.network.yml
~~~

在 .env 中填写真实网络名和服务名：

~~~
SUB2API_DOCKER_NETWORK=sub2api_default
PROMPT_AUDIT_UPSTREAM_URL=http://sub2api:8080
PROMPT_AUDIT_IDENTITY_DATABASE_URL=postgres://sub2api_audit_reader:change_me@postgres:5432/sub2api?sslmode=disable
~~~

启动：

~~~
docker compose -f docker-compose.yml -f docker-compose.network.yml up -d --build
~~~

## 本地构建和测试

需要 Go 1.27：

~~~
go test -race ./...
go vet ./...
go build -trimpath -o sidecar-proxy .
~~~

GitHub CI 还会构建 Docker 镜像。没有 PostgreSQL 时仍可运行解析、代理和认证边界测试；完整部署会在启动时自动执行迁移。

## 升级、兼容与安全边界

这个仓库不导入 sub2api 内部 Go package，也不写入 sub2api 数据库，因此 sub2api 常规升级通常不需要修改或重新开发 Sidecar。以下变化才需要单独适配：

- sub2api 改动 api_keys、users、usage_logs 的表名或关键字段。
- 上游新增协议或改变用户输入字段结构。
- 客户端启用当前不解析的压缩/二进制请求格式。

Sidecar 必须部署在能看到解密后 HTTP body 的位置；仅做四层 TCP 转发无法提取 prompt。审计库包含用户输入和用户标识，应配置最小权限、备份加密和合理保留周期。监控 /healthz 的 queue_dropped，持续增长表示数据库过慢或队列容量不足。

审计写入是 fail-open 设计，不提供“每个成功请求必有审计记录”的强一致保证；但 Sidecar 进程本身不可用时会影响流量入口，应由 Docker/systemd 自动拉起并先在旁路端口验证。
