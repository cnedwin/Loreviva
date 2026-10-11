# 第三方依赖与许可

Loreviva 原创代码按 [Apache-2.0](LICENSE) 发布。第三方依赖保持各自许可，不因 Loreviva 的许可选择而改变。上游版权、许可正文与已有 NOTICE 的原文副本见 [LICENSES](LICENSES/)。

本次公开交付为源码与依赖锁，不附带依赖源码、工具链、数据库、模型权重或可执行程序。本表依据当前 `go.mod` 和 `pnpm-lock.yaml`，列出 11 个声明的 Go 模块及 7 个锁定的 npm 包；许可正文从这些固定版本的本地依赖包原字节提取。它不是整个工具链、所有测试依赖或后续二进制发行物的完整许可审核。

## Go 声明依赖

| 模块 | 版本 | 声明方式 | 许可 | 上游正文副本 |
| --- | --- | --- | --- | --- |
| github.com/go-kratos/kratos/v3 | v3.0.0 | 直接 | MIT | [LICENSE](LICENSES/go-kratos-v3.0.0-LICENSE.txt) |
| github.com/go-sql-driver/mysql | v1.10.1 | 直接 | MPL-2.0 | [LICENSE](LICENSES/go-sql-driver-mysql-v1.10.1-LICENSE.txt) |
| github.com/santhosh-tekuri/jsonschema/v6 | v6.0.2 | 直接 | Apache-2.0 | [LICENSE](LICENSES/jsonschema-v6.0.2-LICENSE.txt) |
| filippo.io/edwards25519 | v1.2.0 | 间接 | BSD-3-Clause | [LICENSE](LICENSES/edwards25519-v1.2.0-LICENSE.txt) |
| github.com/go-playground/form/v4 | v4.3.0 | 间接 | MIT | [LICENSE](LICENSES/form-v4.3.0-LICENSE.txt) |
| github.com/google/uuid | v1.6.0 | 间接 | BSD-3-Clause | [LICENSE](LICENSES/uuid-v1.6.0-LICENSE.txt) |
| github.com/kr/text | v0.2.0 | 间接 | MIT | [License](LICENSES/kr-text-v0.2.0-LICENSE.txt) |
| golang.org/x/sync | v0.20.0 | 间接 | BSD-3-Clause | [LICENSE](LICENSES/x-sync-v0.20.0-LICENSE.txt) |
| golang.org/x/text | v0.37.0 | 间接 | BSD-3-Clause | [LICENSE](LICENSES/x-text-v0.37.0-LICENSE.txt) |
| google.golang.org/protobuf | v1.36.11 | 间接 | BSD-3-Clause | [LICENSE](LICENSES/protobuf-v1.36.11-LICENSE.txt) |
| gopkg.in/yaml.v3 | v3.0.1 | 间接 | MIT 和 Apache-2.0，按文件划分 | [LICENSE](LICENSES/yaml-v3.0.1-LICENSE.txt)、[NOTICE](LICENSES/yaml-v3.0.1-NOTICE.txt) |

`yaml.v3` 的两种许可分别适用于不同文件，不能理解为对整个包任选一种许可。具体文件范围保留在其上游 LICENSE 中。

MySQL Go 驱动的 MPL-2.0 与 Loreviva 原创代码的 Apache-2.0 分开保留。驱动的固定版本源码由其模块仓库提供：[go-sql-driver/mysql v1.10.1](https://github.com/go-sql-driver/mysql/tree/v1.10.1)。后续分发含该驱动的二进制或修改其文件时，发行材料应同时提供适用许可、版权及对应源码获取说明。

## npm 构建与契约校验依赖

| 包 | 版本 | 用途 | 许可 | 上游正文副本 |
| --- | --- | --- | --- | --- |
| ajv | 8.17.1 | 直接开发依赖，Schema 校验 | MIT | [LICENSE](LICENSES/ajv-8.17.1-LICENSE.txt) |
| ajv-formats | 3.0.1 | 直接开发依赖，格式校验 | MIT | [LICENSE](LICENSES/ajv-formats-3.0.1-LICENSE.txt) |
| typescript | 5.9.3 | 直接开发依赖，SDK 类型检查 | Apache-2.0 | [LICENSE](LICENSES/typescript-5.9.3-LICENSE.txt) |
| fast-deep-equal | 3.1.3 | Ajv 的传递依赖 | MIT | [LICENSE](LICENSES/fast-deep-equal-3.1.3-LICENSE.txt) |
| fast-uri | 3.1.8 | Ajv 的传递依赖 | BSD-3-Clause | [LICENSE](LICENSES/fast-uri-3.1.8-LICENSE.txt) |
| json-schema-traverse | 1.0.0 | Ajv 的传递依赖 | MIT | [LICENSE](LICENSES/json-schema-traverse-1.0.0-LICENSE.txt) |
| require-from-string | 2.0.2 | Ajv 的传递依赖 | MIT | [license](LICENSES/require-from-string-2.0.2-LICENSE.txt) |

## 发行范围

Go、Node.js、pnpm、MySQL Community 和可选模型服务由使用者分别安装，本源码包没有捆绑这些发行物。模型权重、外部 API、创作者上传的素材与社区 Skill 也不由项目 LICENSE 统一授权。

更新依赖版本、引入第三方源码或发布二进制、容器镜像、安装包时，应同步更新此表及相应上游许可和 NOTICE；源码表不能代替具体发行物的依赖与权利清单。
