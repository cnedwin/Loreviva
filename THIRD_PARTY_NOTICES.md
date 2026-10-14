# 第三方许可与分发范围 / Third-party licenses and distribution scope

Loreviva 原创源码使用 [Apache-2.0](LICENSE)。第三方代码保持各自许可、版权和免责声明；相关完整原文见 [LICENSES](LICENSES/)。本源码包包含 Studio 的构建后 JavaScript、CSS 和 HTML，以及生成的 SDK/契约文件。以下区分声明依赖、实际嵌入运行时代码和构建辅助代码，不把声明依赖列表等同于完整发行物 SBOM。

Original Loreviva source uses [Apache-2.0](LICENSE). Third-party code retains its own licenses, copyrights and disclaimers; full texts are in [LICENSES](LICENSES/). This source distribution includes built Studio JavaScript, CSS and HTML, plus generated SDK/contract files. The sections distinguish declared dependencies, embedded runtime code and generated build helpers; a declared-dependency list is not a complete distribution SBOM.

## Go 声明依赖 / Declared Go dependencies

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

表中“直接/间接”对应 direct/indirect module declarations；版本与完整许可链接逐项列明。

The table lists each module, version, direct/indirect declaration, license and complete upstream text.

`yaml.v3` 的 MIT 和 Apache-2.0 分别适用于不同文件，不能理解为对整个包任选一种许可；文件范围与版权说明保留在其 LICENSE/NOTICE 中。

For `yaml.v3`, MIT and Apache-2.0 apply to different files, not as an either/or choice for the entire package. Its LICENSE/NOTICE retain the file-specific scope and attribution.

MySQL Go 驱动的 MPL-2.0 与项目原创源码的 Apache-2.0 分开保留。对应源码见 [go-sql-driver/mysql v1.10.1](https://github.com/go-sql-driver/mysql/tree/v1.10.1)。若分发含驱动的二进制或修改其文件，应提供适用许可、版权及对应源码获取说明。

The MySQL Go driver's MPL-2.0 remains separate from Apache-2.0 for original project source. Corresponding source is available at [go-sql-driver/mysql v1.10.1](https://github.com/go-sql-driver/mysql/tree/v1.10.1). Distribution of binaries containing the driver or modifications to its files must provide applicable license/copyright notices and corresponding-source access information.

## npm 构建与契约校验声明 / npm build and contract-validation declarations

| 包 | 版本 | 用途 | 许可 | 上游正文副本 |
| --- | --- | --- | --- | --- |
| ajv | 8.17.1 | 直接开发依赖，Schema 校验 | MIT | [LICENSE](LICENSES/ajv-8.17.1-LICENSE.txt) |
| ajv-formats | 3.0.1 | 直接开发依赖，格式校验 | MIT | [LICENSE](LICENSES/ajv-formats-3.0.1-LICENSE.txt) |
| typescript | 5.9.3 | 直接开发依赖，SDK 类型检查 | Apache-2.0 | [LICENSE](LICENSES/typescript-5.9.3-LICENSE.txt) |
| fast-deep-equal | 3.1.3 | Ajv 的传递依赖 | MIT | [LICENSE](LICENSES/fast-deep-equal-3.1.3-LICENSE.txt) |
| fast-uri | 3.1.8 | Ajv 的传递依赖 | BSD-3-Clause | [LICENSE](LICENSES/fast-uri-3.1.8-LICENSE.txt) |
| json-schema-traverse | 1.0.0 | Ajv 的传递依赖 | MIT | [LICENSE](LICENSES/json-schema-traverse-1.0.0-LICENSE.txt) |
| require-from-string | 2.0.2 | Ajv 的传递依赖 | MIT | [license](LICENSES/require-from-string-2.0.2-LICENSE.txt) |

表中用途是声明接点，不是全部运行时用途。Ajv 的生成校验代码及所引用的 Ajv runtime、ajv-formats、fast-deep-equal 也进入交付的 SDK/Studio JavaScript，适用各自 MIT 保留要求。

Table purposes describe declaration points, not every runtime use. Generated Ajv validators and their Ajv runtime, ajv-formats and fast-deep-equal references also enter delivered SDK/Studio JavaScript and retain their respective MIT notices.

## Studio 嵌入运行时 / Embedded Studio runtime

| 包 / Package | 版本 / Version | 许可 / License | 完整原文 / Full text |
| --- | --- | --- | --- |
| @vue/shared | 3.5.43 | MIT | [LICENSE](LICENSES/vue-3.5.43/LICENSE.txt) |
| @vue/reactivity | 3.5.43 | MIT | [LICENSE](LICENSES/vue-3.5.43/LICENSE.txt) |
| @vue/runtime-core | 3.5.43 | MIT | [LICENSE](LICENSES/vue-3.5.43/LICENSE.txt) |
| @vue/runtime-dom | 3.5.43 | MIT | [LICENSE](LICENSES/vue-3.5.43/LICENSE.txt) |
| Pinia | 4.0.3 | MIT | [LICENSE](LICENSES/pinia-4.0.3/LICENSE.txt) |
| Vue Router | 5.4.0 | MIT | [LICENSE](LICENSES/vue-router-5.4.0/LICENSE.txt) |
| ajv | 8.17.1 | MIT | [LICENSE](LICENSES/ajv-8.17.1-LICENSE.txt) |
| ajv-formats | 3.0.1 | MIT | [LICENSE](LICENSES/ajv-formats-3.0.1-LICENSE.txt) |
| fast-deep-equal | 3.1.3 | MIT | [LICENSE](LICENSES/fast-deep-equal-3.1.3-LICENSE.txt) |

这些包的代码参与嵌入 JavaScript。Vue 3.5.43 提供组件与响应式运行时，Pinia 提供状态管理，Vue Router 提供路由。Vue 完整许可保留 2018-present Yuxi (Evan) You 的版权；Pinia 和 Vue Router 保留 2019-present Eduardo San Martin Morote 的版权。

Code from these packages contributes to embedded JavaScript. Vue 3.5.43 supplies components/reactivity, Pinia supplies state management and Vue Router supplies routing. The complete Vue license retains the 2018-present copyright of Yuxi (Evan) You; Pinia and Vue Router retain the 2019-present copyright of Eduardo San Martin Morote.

## 生成辅助代码与构建材料 / Generated helpers and build materials

| 辅助代码 / Helper | 生成工具版本 / Producer version | 许可保留 / License retention |
| --- | --- | --- |
| Rolldown runtime | Rolldown 1.2.13 | [MIT](LICENSES/rolldown-1.2.13/LICENSE.txt)、[third-party notices](LICENSES/rolldown-1.2.13/THIRD-PARTY-LICENSE.txt) |
| modulepreload polyfill | Vite 8.3.4 / Rolldown 1.2.13 | [Vite core MIT](LICENSES/vite-8.3.4/CORE-LICENSE.txt)、[Rolldown MIT](LICENSES/rolldown-1.2.13/LICENSE.txt)、[third-party notices](LICENSES/rolldown-1.2.13/THIRD-PARTY-LICENSE.txt) |
| Vue component export helper | @vitejs/plugin-vue 6.0.9 | [MIT](LICENSES/vite-plugin-vue-6.0.9/LICENSE.txt) |

三个辅助实现进入嵌入 JavaScript。Vite 核心 MIT 保留 2019-present VoidZero Inc. and Vite contributors 的版权；plugin-vue MIT 保留 2019-present Yuxi (Evan) You and Vite contributors 的版权。Vite 核心许可文本不替代其其他 bundled-dependencies 的许可；若另分发相应依赖代码，应一并保留对应材料。

These three helpers enter embedded JavaScript. Vite core MIT retains the 2019-present copyright of VoidZero Inc. and Vite contributors; plugin-vue MIT retains the 2019-present copyright of Yuxi (Evan) You and Vite contributors. The Vite core text does not replace licenses for other bundled dependencies; redistribution of their code requires the corresponding materials as well.

`@rolldown/binding-win32-x64-msvc` 1.2.13 是 Windows x64 构建依赖，原生二进制不在本源码分发中。其主 MIT 与两段第三方 MIT 正文完整保留；上游同版原文见 [LICENSE](https://raw.githubusercontent.com/rolldown/rolldown/v1.2.13/LICENSE) 和 [THIRD-PARTY-LICENSE](https://raw.githubusercontent.com/rolldown/rolldown/v1.2.13/THIRD-PARTY-LICENSE)。保留这些文本不表示覆盖另行分发的整个 native 工具依赖集。

`@rolldown/binding-win32-x64-msvc` 1.2.13 is a Windows x64 build dependency; its native binary is not included in this source distribution. Its main MIT and two third-party MIT texts are retained completely; the linked upstream [LICENSE](https://raw.githubusercontent.com/rolldown/rolldown/v1.2.13/LICENSE) and [THIRD-PARTY-LICENSE](https://raw.githubusercontent.com/rolldown/rolldown/v1.2.13/THIRD-PARTY-LICENSE) are for that version. Retention does not claim coverage of every dependency in a separately redistributed native tool.

测试依赖链 jsdom 30.1.2 使用 saxes 6.0.0。[完整 saxes LICENSE](LICENSES/saxes-6.0.0/LICENSE.txt) 保留当前 ISC、原 sax ISC 与说明历史用途的 MIT 段，不是全包 ISC/MIT 任选，也不将历史已不再使用的代码称为当前使用。对应上游原文见 [saxes 6.0.0 commit LICENSE](https://raw.githubusercontent.com/lddubeau/saxes/211fa0ebec9b628affc09219199639887174bfc3/LICENSE)。测试依赖材料的保留不等于其代码进入 Studio。

The jsdom 30.1.2 test dependency chain uses saxes 6.0.0. The [complete saxes LICENSE](LICENSES/saxes-6.0.0/LICENSE.txt) retains current ISC, original sax ISC and the MIT section explaining historical use. These are not an either/or ISC/MIT choice for the entire package, and historical unused code is not represented as currently used. The corresponding upstream text is the linked [saxes 6.0.0 commit LICENSE](https://raw.githubusercontent.com/lddubeau/saxes/211fa0ebec9b628affc09219199639887174bfc3/LICENSE). Retaining test-dependency material does not establish inclusion in Studio.

## 分发与内容边界 / Distribution and content boundaries

Go、Node.js、pnpm、MySQL Community、依赖缓存、node_modules、原生构建工具及可选模型服务不随本源码包捆绑。模型权重、外部 API、创作者素材和社区 Skill 的权利应分别取得，项目 LICENSE 不自动授权这些内容。

Go, Node.js, pnpm, MySQL Community, dependency caches, node_modules, native build tools and optional model services are not bundled with this source package. Rights to model weights, external APIs, creator assets and community Skills must be obtained separately; the project LICENSE does not automatically grant them.

依赖升级、引入新第三方源码或分发二进制、容器、安装包时，应按实际分发范围更新许可与 NOTICE。本文是本源码及已列嵌入材料的保留说明，不是全 SBOM，也不表示完整产品、模型内容或商业使用已验收。

Dependency upgrades, new third-party source or distribution of binaries, containers or installers require license/NOTICE updates for the actual scope. This document records retention for this source package and listed embedded materials; it is not a full SBOM or acceptance of the complete product, model content or commercial use.
