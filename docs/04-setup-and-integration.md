# 搭建和接入指南

Loreviva — The AI Platform for Living Worlds。

本指南适用于已批准 Tasks 1–6 的本地合成Mock基线，介绍现有 CLI 与行政 API。它不是完整 Studio、真实 AI、客户玩家接入或生产托管安装教程。公开文档是现行规范的整理分发视图。

## 准备环境

- Go 1.27.2、MySQL Community LTS / InnoDB。
- Node.js 24、pnpm 11.25.0，用于契约生成与 TypeScript 开发检查。
- 一个受限的本地开发数据库账户，以及独立的私有文件目录和治理记录目录。

Kratos 3 已由 [go.mod](../go.mod)固定。Vue Studio 尚待实现；当前应用根页只是最小启动页。容器部署与完整恢复流程未纳入本批已完成范围。

## 配置

参考 [.env.example](../.env.example)，把变量设置到启动进程的环境中。应用**不会自动加载 `.env`**。秘密通过显式 `env:` 引用解析，勿把密码、Operator token 或测试连接串提交到仓库。

| 变量 | 配置含义 |
| --- | --- |
| `LOREVIVA_MODE` | `all` / `api` / `worker`，默认 `all` |
| `LOREVIVA_HTTP_ADDR` | HTTP 监听地址，默认本机 8080 |
| `LOREVIVA_DB_ADDR` / `LOREVIVA_DB_NAME` / `LOREVIVA_DB_USER` | 数据库地址、名称与账户；均必填 |
| `LOREVIVA_DB_PASSWORD_REF` | 如 `env:LOREVIVA_DB_PASSWORD`；该环境变量须私下提供值 |
| `LOREVIVA_FILES_ROOT` | 私有文件根；不放入公开静态目录 |
| `LOREVIVA_GOVERNANCE_ROOT` | 独立治理记录根，默认从文件根派生 |
| `LOREVIVA_MODEL_MODE` / `LOREVIVA_BILLING_MODE` | 当前只允许 `mock` |
| `LOREVIVA_PUBLIC_BASE_URL` | 不含嵌入凭据的 HTTP(S) 基础地址 |
| `LOREVIVA_OPERATOR_TENANT` / `LOREVIVA_OPERATOR_PRODUCT` / `LOREVIVA_OPERATOR_SUBJECT` / `LOREVIVA_OPERATOR_CREDENTIAL_ID` | Operator 范围与登记身份；示例使用 demo-local/coffee/creator/owner |
| `LOREVIVA_OPERATOR_SECRET_REF` | 显式秘密引用，解析为 32–512 字节 token；重启保留同一私下凭据 |

配置规则以 [config.go](../internal/config/config.go)为依据。数据库名称需遵守现有本地范围校验；初次开发可使用模板的 `loreviva_dev`。没有默认管理员密码或秘密读取接口。

## 新合成安装

在仓库根目录执行：

```powershell
go run ./cmd/loreviva migrate plan
go run ./cmd/loreviva migrate up --plan-id <plan返回的plan_id>
go run ./cmd/loreviva migrate status
go run ./cmd/loreviva bootstrap --mock --tenant demo-local
go run ./cmd/loreviva seed coffee --mock --tenant demo-local
go run ./cmd/loreviva --mode all
```

迁移只能应用当前固定计划。已有安装拒绝普通 `migrate up`，不要把这组新安装命令当作升级/恢复工具。

`bootstrap` 显式登记新合成安装的凭据哈希、作用域授权和初始治理记录，不覆盖既有凭据，不替恢复数据补造缺失水位。凭据丢失或治理记录缺失需要后续受审计恢复流程，重新 bootstrap 不能绕过检查。

咖啡馆 seed 幂等建立项目、草稿与 release；定义 Alex、Mia 和木牌保管规则，不创建运行实例、会话或真实供应商调用。重新执行应返回原内容身份。

可访问 `/health`、`/version`、`/ready` 与 `/` 查看启动状态。就绪不证明完整创作流程、真实模型质量或生产容量。`--mode api` 与 `--mode worker` 可分进程运行，共用同一数据库与领域代码。

## 现有行政 API

请求使用 `Authorization: Bearer <私下解析的Operator token>`，按契约提供 `product_id`，由服务端检查当前登记身份和动作资格。修改请求使用 `Idempotency-Key`，相关保存/控制/迁移还需要原契约中的预期版本或预览绑定。

推荐按以下顺序理解现有调用：

1. 创建或查询草稿，保存内容修订，查看关联影响与精确校验问题。
2. 发布预检后提交 release；发布重新检查修订、依赖和权利。
3. 根据固定 release 创建实例，再提交 START 等控制命令。
4. 查询原已登记任务和实例，按获准范围读取演员及记忆元数据。
5. 必要时执行兼容迁移、记忆纠正/删除或用途政策治理，查询各自完成状态。

方法、请求体和响应以 [OpenAPI](../contracts/source/openapi.yaml)及[实际路由拥有者](03-architecture.md#http-实际接入范围)为准。草稿`generation-resolution`读取当前解析结果；`rehearsals`和`candidates`在对应Mock拥有者/依赖就绪且资格、来源、路由和输入校验通过时返回202并开始持久任务，缺依赖仍拒绝。202不等于生成或排练完成；按原任务查询结果、费用核验和清理进度。候选不自动保存草稿，排练不改写正式世界。安全任务列表与结果正文有独立资格；其他预留客户端契约不代表已有处理器。

私有上传使用 multipart，顺序为完整 `metadata` 再 `file`；大小、哈希、类型和内容会检查。下载走鉴权内容接口，不返回存储路径。已有清理命令：

```powershell
go run ./cmd/loreviva maintenance cleanup-files --key <稳定幂等键>
```

此任务只处理合格的已登记 staging 文件，不是通用文件删除器。Worker还处理世界控制、实例迁移、记忆治理，以及对应拥有者就绪的Mock TURN、候选和隔离排练。

## 重试、分页与治理

提交结果不明时用原键、原输入查询/重放；同键不同输入冲突，当前授权仍重新检查。202 表示受理，不表示世界动作、迁移或全部清理已经完成；取消任务不承诺回滚已成立事实或撤回已交付内容。

草稿、修订和 release 列表使用 `limit=1..100` 与不透明 cursor。继续查询保持原筛选和页大小；cursor 失效应清掉它从第一页查询。相关实例列表也支持分页，具体参数按契约与处理器核对，勿自行解释内部序号。

记忆元数据、私人正文、治理和用途政策有独立资格。能查询后台任务不代表能读私人正文，也不授予模型使用权。已交付副本不能承诺召回；完整备份恢复另待实现。

## 客户接入的下一步

现有 [TypeScript 包](../packages/sdk-ts/src/index.ts)导出生成类型，并校验/冻结 `baseUrl` 与 `sessionToken` 配置；它尚无请求方法、SSE、重试或身份适配器，不是完整可用客户端 SDK。

计划中的 Integration Kit 将提供受限会话、互动、快照与事件恢复、媒体更新、参考客户端和薄后端适配器。客户后端仍负责玩家身份、业务资格、行业规则、钱包/订单/支付/退款及原业务服务回执；客户端不持 Operator 或模型秘密。

真实模型、SaaS 计量管理和收费开关尚未接入。当前 Mock 发布不能标成真实 AI 或可收费生产服务。

## 开发检查

```powershell
pnpm install --frozen-lockfile
pnpm generate:contracts
pnpm test
go test ./...
```

契约源维护在`contracts/source/`，本批为1.3.0 / 51 operations / 131 schemas；生成文件不可手工改成第二源。公开测试的两份历史基准位于`tests/fixtures/`，保持original18操作/43 DTO及1.2的49操作/123 Schema比较语义。Node检查实际包含行政读契约检查，共13项；严格SDK typecheck另执行。嵌入迁移为0001–0006，完整产品安装/升级发行仍未验收。

数据库集成测试强制使用 TCP `127.0.0.1:33267` 和非 root 账户，通过私下提供的 `LOREVIVA_TEST_DSN` 创建并清理 `loreviva_test_<24位小写十六进制>` 测试库。未设置该变量时测试失败，不会自动跳过；账户应只拥有此受限测试范围的权限。实际 CLI 测试另使用 `LOREVIVA_TEST_GO_EXE` 指定执行器。这是测试夹具的限制，服务本身的 `LOREVIVA_DB_ADDR` 按配置指定。检查结果以具体发布副本为准。

返回[首页](../README.md) · 查看[需求与状态](02-requirements.md)
