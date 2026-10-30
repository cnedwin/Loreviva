> 当前安全文档预览与剩余范围见[文档提取](progress/2026-10-30-documents.zh.md)。完整导入和Task9/R0仍未验收。

> 当前内容包审核、导出与第二安装进展见 [内容包安装迁移](progress/2026-10-29-transfer.zh.md)；Task9/R0仍未整体验收。

# 搭建和接入指南

当前范围（2026-10-29）：Task1–8保留声明范围内采用；Task9已接通持久隔离导入、按修订审核、静态导出下载及第二安装发布。安全文档、完整Skill共享及Task9/R0整体仍未完成。工作契约1.9.0：75操作/197 Schema，迁移0001–0010。见[当前内容包验证](progress/2026-10-29-transfer.zh.md)。

Loreviva — The AI Platform for Living Worlds。

本指南适用于本地已批准 Tasks 1–7 的合成 Mock 基线，介绍 CLI、原生 Studio 与行政 API。它不是实际公开副本的安装验收，也不是完整客户玩家接入、真实 AI 或生产托管教程。公开文档是现行规范的整理分发视图。

## 准备环境

- Go 1.27.2、MySQL Community LTS / InnoDB。
- Node.js 24、pnpm 11.25.0，用于契约生成与 TypeScript 开发检查。
- 一个受限的本地开发数据库账户，以及独立的私有文件目录和治理记录目录。

Kratos 3 已由 [go.mod](../go.mod)固定。Go 应用在 `/studio/` 提供原生 Vue Studio，`/` 保留启动检查用途。修改前端后须先重建真实嵌入产物，再构建 Go 应用；容器部署与完整恢复仍未纳入已完成范围。

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

如需从源码重建 Studio，先在仓库根目录执行：

```powershell
pnpm install --frozen-lockfile
pnpm build:studio
pnpm build:server ./cmd/loreviva
```

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

## Studio 与本地设置

打开 `/studio/`，使用私下提供且已登记的 Operator 凭据。Studio 只在页面内存保留当前 bearer，刷新后需重新登录；没有默认管理员密码。保存与取消、关联影响确认、可选 Mock 助手、检查和发布复用实际行政服务；采纳候选后仍须显式保存。

`/studio/settings` 提供本地设置、成员与独立自同意入口，七个接口见[设置路由](03-architecture.md#契约与扩展)。这些动作仍检查当前资格；普通设置不依赖模型供应商就绪。成员操作不授予私人正文或模型使用权，通知偏好不会外发消息。已有安装的本地管理初始化必须使用已配置的拥有凭据显式进行，不能据此新造 owner；完整升级/恢复发行仍待验收。

## 现有行政 API

请求使用 `Authorization: Bearer <私下解析的Operator token>`，按契约提供 `product_id`，由服务端检查当前登记身份和动作资格。修改请求使用 `Idempotency-Key`，相关保存/控制/迁移还需要原契约中的预期版本或预览绑定。

推荐按以下顺序理解现有调用：

1. 创建或查询草稿，保存内容修订，查看关联影响与精确校验问题。
2. 发布预检后提交 release；发布重新检查修订、依赖和权利。
3. 根据固定 release 创建实例，再提交 START 等控制命令。
4. 查询原已登记任务和实例，按获准范围读取演员及记忆元数据。
5. 必要时执行兼容迁移、记忆纠正/删除或用途政策治理，查询各自完成状态。

方法、请求体和响应以 [OpenAPI](../contracts/source/openapi.yaml)及[实际路由拥有者](03-architecture.md#http-实际接入范围)为准。草稿`generation-resolution`读取当前解析结果；`rehearsals`和`candidates`在对应Mock拥有者/依赖就绪且资格、来源、路由和输入校验通过时返回 202 并开始持久任务，缺依赖仍拒绝。202 不等于生成或排练完成；按原任务查询结果、费用核验和清理进度。候选不自动保存草稿，排练不改写正式世界。安全任务列表与结果正文有独立资格；其他预留客户端契约不代表已有处理器。

私有上传使用 multipart，顺序为完整 `metadata` 再 `file`；大小、哈希、类型和内容会检查。下载走鉴权内容接口，不返回存储路径。已有清理命令：

```powershell
go run ./cmd/loreviva maintenance cleanup-files --key <稳定幂等键>
```

此任务只处理合格的已登记 staging 文件，不是通用文件删除器。Worker还处理世界控制、实例迁移、记忆治理，以及对应拥有者就绪的Mock TURN、候选和隔离排练。

## 重试,分页与治理

提交结果不明时用原键、原输入查询/重放；同键不同输入冲突，当前授权仍重新检查。202 表示受理，不表示世界动作、迁移或全部清理已经完成；取消任务不承诺回滚已成立事实或撤回已交付内容。

草稿、修订和 release 列表使用 `limit=1..100` 与不透明 cursor。继续查询保持原筛选和页大小；cursor 失效应清掉它从第一页查询。相关实例列表也支持分页，具体参数按契约与处理器核对，勿自行解释内部序号。

记忆元数据、私人正文、治理和用途政策有独立资格。能查询后台任务不代表能读私人正文，也不授予模型使用权。已交付副本不能承诺召回；完整备份恢复另待实现。

## 客户接入的下一步

[TypeScript包](../packages/sdk-ts/src/index.ts)导出行政/接入客户端、预备命令、BFF恢复、媒体与SSE类型。[薄后端](../examples/thin-backend/)与[参考客户端](../apps/reference-client/)提供参考接入实现，前置见[Quickstart](integration/Quickstart.zh.md)。不能把Operator凭据发送给玩家。完整Task8/R0未验收。

计划中的 Integration Kit 将提供受限会话、互动、快照与事件恢复、媒体更新、参考客户端和薄后端适配器。客户后端仍负责玩家身份、业务资格、行业规则、钱包/订单/支付/退款及原业务服务回执；客户端不持 Operator 或模型秘密。

真实模型、SaaS 计量管理和收费开关尚未接入。当前 Mock 发布不能标成真实 AI 或可收费生产服务。

## 开发检查

```powershell
pnpm install --frozen-lockfile
pnpm generate:contracts
pnpm test
go test ./...
```

契约源维护在 `contracts/source/`，本批工作契约为 1.9.0 / 75 operations / 197 schemas（已批准基线仍为 1.4.0 / 58 / 156）；生成文件不可手工改成第二源。`tests/fixtures/` 中原 18 操作/43 DTO及source1.2 的 49 操作/123 Schema保留各自历史比较语义，source1.3 为 51/131。根 `pnpm test` 覆盖契约/SDK脚本并包含 Studio contract 与 CSP 检查；Studio 单测、Vue 类型检查与生产构建使用下面的独立命令，不用旧检查总数代替实际结果。嵌入迁移为 0001–0010，完整产品安装/升级发行仍未验收。

```powershell
pnpm --dir apps/studio test
pnpm --dir apps/studio typecheck
pnpm build:studio
```

`pnpm build:studio` 实际运行 Vue 类型检查与生产构建，再记录完整源码输入和产物字节清单。`pnpm check:studio` 只读核对源码、SDK、契约、锁文件、构建配置及内嵌文件；任意新增、删除或字节变化都要求重新构建。`pnpm build:server ./cmd/loreviva` 先通过该检查再编译服务端。直接 `go build` 只编译现有内嵌字节，不能证明前端源码新鲜度；服务端另检查内嵌契约版本与文件完整性，错误产物返回503。

`go test ./...` 本身不会自动启用浏览器夹具。`TestStudioBrowser` 与 `TestStudioSettingsEntryBrowser` 需要显式 `LOREVIVA_STUDIO_BROWSER=1`，以及核验过的 `LOREVIVA_TEST_NODE`、`LOREVIVA_TEST_PNPM` 和私有 `LOREVIVA_STUDIO_EVIDENCE` 输出位置；未启用时为 SKIP，不能算浏览器通过。夹具启动真实本机服务并提供合成数据，按固定 Playwright 项目执行；直接 `pnpm test:e2e` 不能替代这些服务和凭据前置。受控浏览器通过也不是真人或真实模型验收。

上述命令定义验证入口，各次结果绑定当时的源码和产物。构建通过不等于数据库、浏览器或完整发行验收；实际数据库验证使用单一受限窗口，保留前置失败与未运行结果。具体已通过范围以对应版本的验证记录为准。

数据库集成测试强制使用 TCP `127.0.0.1:33267` 和非 root 账户，通过私下提供的 `LOREVIVA_TEST_DSN` 创建并清理 `loreviva_test_<24位小写十六进制>` 测试库。未设置该变量时测试失败，不会自动跳过；账户应只拥有此受限测试范围的权限。实际 CLI 测试另使用 `LOREVIVA_TEST_GO_EXE` 指定执行器。这是测试夹具的限制，服务本身的 `LOREVIVA_DB_ADDR` 按配置指定。检查结果以具体发布副本为准。

返回[首页](../README.md) · 查看[需求与状态](02-requirements.md)
