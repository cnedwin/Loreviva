# Loreviva

**The AI Platform for Living Worlds**  
创造世界，让它持续生长。

Loreviva 的目标是让创作者组合世界规则、角色、岗位、Skill、知识与模型配置，制作持续发展的互动内容，并让客户通过自己的客户端接入。

本批源码候选来自已批准的 **Tasks 1–6** 基线：内容制作、世界实例控制、兼容迁移、记忆治理，以及持久 Mock Harness、候选和隔离排练内核。当前只允许合成 Mock 模型与 Mock 计费。完整 Studio、客户 SDK、安装发行、真实 AI 长期体验和生产 SaaS 尚未交付或验收；具体分发范围以 `PUBLICATION.json` 为准。

## 当前范围

| 状态 | 能力 |
| --- | --- |
| 已实现 | 单应用 API/Worker、持久任务基础、幂等命令、提交回执与当前授权检查 |
| 已实现 | 内容草稿、修订、关联影响检查、发布预检、不可变 release 与固定依赖/模型路由 |
| 已实现 | 私有文件上传、鉴权下载、可恢复清理任务 |
| 已实现 | 世界实例与演员实例、启动/暂停/恢复/结束、声明式动作校验与提交、兼容内容迁移 |
| 已实现 | 作用域记忆、来源与获知、私人关系边界、用途政策及纠正/删除治理 |
| 已实现 | 有界 Mock Harness、原 attempt 查询恢复、草稿候选与隔离排练、必要费用记录和结果分离 |
| 计划中 | Vue Studio、完整客户端 SDK 与接入样例 |
| 计划中 | 内容包导入导出、计量管理、完整备份恢复与真实模型/托管收费发行验证 |

`/` 目前是最小启动页。机器契约为 **1.3.0，51 operations / 131 schemas**，嵌入迁移为 0001–0006。OpenAPI 中的预留操作不等于已注册可用接口；内部 TURN 已实现不代表客户玩家 HTTP 互动已经接通，实际边界见[架构与接口拥有者](docs/03-architecture.md)。

开发设计3.0.2中的持续角色成长与深入记忆要求仍是后续设计；12项LC要求和24条场景未实现、未执行。R0完整产品流程、真人体验、R1真实模型、收费与部署仍未验收。

## 本地启动

需要 **Go 1.27.2、Kratos 3、MySQL Community LTS、Node.js 24、pnpm 11.25.0**。Vue 是后续 GUI 技术路线，尚未包含已完成的 Studio。

先按[搭建和接入指南](docs/04-setup-and-integration.md)配置数据库、私有文件目录及环境变量中的秘密引用。应用不会自动读取 `.env`，没有默认管理员密码。

从仓库根目录执行：

```powershell
go run ./cmd/loreviva migrate plan
go run ./cmd/loreviva migrate up --plan-id <plan返回的plan_id>
go run ./cmd/loreviva migrate status
go run ./cmd/loreviva bootstrap --mock --tenant demo-local
go run ./cmd/loreviva seed coffee --mock --tenant demo-local
go run ./cmd/loreviva --mode all
```

`bootstrap` 只用于新的合成安装，不能替代恢复或凭据找回。咖啡馆种子定义 Alex、Mia 与木牌保管规则，创建内容项目/草稿/release，**不会创建运行实例、玩家会话或真实模型请求**。

## 文档与开发入口

- [产品概述](docs/01-product-overview.md)：愿景、角色关系与轻量范围。
- [需求与实现状态](docs/02-requirements.md)：已完成内核与待完成产品流程。
- [技术架构](docs/03-architecture.md)：模块、持久化与已接入 HTTP 能力。
- [搭建和接入](docs/04-setup-and-integration.md)：配置、初始化、行政 API 与 SDK 边界。
- [OpenAPI 唯一维护源](contracts/source/openapi.yaml)与[内容包 Schema](contracts/source/wepkg.schema.json)：字段依据；[TypeScript 包](packages/sdk-ts/src/index.ts)目前只有生成类型和配置封装。

这些公开文档是维护方现行产品规范的整理分发视图，不另立一套权威知识库。产品目标与本批实现状态分开说明；生成物不可手改成为另一字段源。

```powershell
pnpm install --frozen-lockfile
pnpm generate:contracts
pnpm test
go test ./...
```

公开契约测试使用仓库内的两份固定历史基准夹具，保留 original18操作/43 DTO 以及1.2的49操作/123 Schema比较。数据库集成测试另需私下配置受限测试账户及可销毁数据库；详见[测试边界](docs/04-setup-and-integration.md#开发检查)。具体发布副本以对应的检查结果为准。

原创代码使用 [Apache-2.0](LICENSE)；参见[贡献说明](CONTRIBUTING.md)、[安全报告](SECURITY.md)和[第三方许可](THIRD_PARTY_NOTICES.md)。
