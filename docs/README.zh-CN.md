# 文档索引

英文为主版本，中文为配套版本；完整对照表见 [README.md](README.md)。

- 设计：`docs/design/en/`（英文）与 `docs/design/`（中文）
- 运维与接入：`docs/en/` 与 `docs/`
- 进度记录：`docs/progress/*.en.md`、`docs/progress/*.zh.md`
- 契约唯一源：`contracts/source/openapi.yaml`、`contracts/source/wepkg.schema.json`
- 生成物：`contracts/generated/`、`internal/generated/`、`packages/sdk-ts/src/generated/`（不可手改）

规则：

1. 英文为主，中文不另立含义。
2. 写清支持范围与验收边界，而不是愿景。
3. 区分"契约中声明"与"已实现并验证"。
4. 用文件与操作引用代替散文描述。
