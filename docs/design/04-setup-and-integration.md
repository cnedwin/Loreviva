> 设计文档 v3.0.2。本文定义产品目标与要求；本分支源码仍为 Task1–7 / 契约1.4.0，未包含开发中的1.5.0功能。可用能力和安装步骤见[本仓库支持范围](00-status.md)。

# Loreviva 搭建和接入指南

设计版本v3.0.2，R0执行基线v3.0.1。产品开发进行中，完整产品尚未验收；当前进度、支持范围及验证边界见[项目状态](00-status.md)。持续角色（LC）属于后续设计。

Loreviva — The AI Platform for Living Worlds  
创造世界，让它持续生长。

本文按设计v3.0.2与R0执行基线v3.0.1说明搭建和接入，与[产品设计概述](01-product-overview.md)、[产品需求说明书](02-requirements.md)、[技术架构白皮书](03-architecture.md)及[首阶段实施计划](05-roadmap.md)共享产品行为和技术约束，机器字段沿唯一契约源。

## 01 先确认现在能做什么

工程代码与生成类型不等于可安装产品。部署须核验当次应用、数据库、依赖、契约哈希及目标环境；下文说明必需流程，支持范围以已验收实物和实际验证范围为限。

## 02 当前可用的设计与契约核对路径

产品行为查[产品需求说明书](02-requirements.md)，技术不变量查[技术架构白皮书](03-architecture.md)，任务依赖查[首阶段实施计划](05-roadmap.md)。设计v3.0.2与R0执行基线v3.0.1各有职责；历史材料用于溯源，不构成另一套现行规范。

接入前核对对象与作用域、受理/生成/裁决/提交边界、核准packet、幂等、暂停/撤权/UNKNOWN恢复、包权限及客户保留的业务权威。机器字段唯一源为`contracts/source/openapi.yaml`与`contracts/source/wepkg.schema.json`；文档和Schema静态核对不等于服务器连通或产品验收。

工程根为`<repository>`。共同`contracts/generated`目录、Go类型、TypeScript SDK及契约文档均从`contracts/source`唯一源生成，并检查根路径与源哈希；不维护人工复制契约、第二套DTO或另一公开协议权威。SDK、Studio、参考客户端与薄后端分别位于`packages/sdk-ts`、`apps/studio`、`apps/reference-client`及`examples/thin-backend`。目录或生成类型存在不等于完整安装和发行已验收。

## 03 目标搭建流程与当前验证边界

本节说明可安装发行物的目标流程。M1负责独立工程、共同契约、配置、数据库迁移及启动骨架；其健康和版本检查不能替代M2–M4的创作者/玩家流程或非维护者安装验收。

默认组件为一个Loreviva应用和一个MySQL Community LTS数据库。Vue静态后台随应用提供，本机私有文件经应用鉴权代理。可选择Linux社区Engine或原生数据库，不强制Docker Desktop、付费云或平台账户。工程基线使用Go1.27.2/MySQL9.7.2、Node24 LTS与Vue3.5.43，精确兼容范围按实际实物和锁文件验证。每次发行固定平台实物及容器digest，不使用`latest`；既有哈希和锁文件不证明新机器安装成功。

取得经核验的源码或发行物及SHA256、版本、SBOM和权利文件，确认操作系统、端口、磁盘、数据库兼容范围与权限，再按实物说明安装。源码仓库不等于已验收产品安装包。数据库与账户专用于本项目，迁移与应用角色使用各自必需的最小权限，不复用无关数据库，不以真实私聊作seed。

以下配置示例只含非秘密默认值和秘密引用，是设计示意，不是已验证可加载的安装配置；实际支持键以发行物的`.env.example`为准：

```dotenv
LOREVIVA_HTTP_ADDR=127.0.0.1:8080
LOREVIVA_DB_ADDR=127.0.0.1:3306
LOREVIVA_DB_NAME=loreviva_demo
LOREVIVA_DB_USER=loreviva_demo
LOREVIVA_DB_PASSWORD_REF=env:LOREVIVA_DB_PASSWORD
LOREVIVA_FILES_ROOT=./var/private-files
LOREVIVA_MODEL_MODE=mock
LOREVIVA_BILLING_MODE=mock
LOREVIVA_PUBLIC_BASE_URL=http://127.0.0.1:8080
```

实际数据库密码由安装者通过受控环境或秘密管理机制注入，不写进源码、分享包、截图、日志或示例。真实模型凭据也只在服务端登记秘密引用，业务对象引用连接ID。仅本机监听是新安装默认值；向网络开放前单独配置TLS、可信代理和认证。应用启动须检查配置完整性、私有目录不可公开列出、数据库Schema兼容性和删除记录水位；失败时保持未就绪状态，错误给出关联ID而不显示密码。

以下命令接口是设计示例，不是已验证安装命令。只有所选实物支持相应选项且发行说明已核验时才使用：

```powershell
Set-Location '<repository>'
pnpm --dir apps/studio install --frozen-lockfile
pnpm --dir apps/studio build
go run ./cmd/loreviva migrate status
go run ./cmd/loreviva migrate plan
go run ./cmd/loreviva migrate up --plan-id <已核验计划ID>
go run ./cmd/loreviva seed coffee --mock --tenant demo-local
go run ./cmd/loreviva --mode all
```

独立工程应能报告本地Git顶层就是 `<repository>`，依赖锁、迁移计划、Schema版本、应用版本和契约哈希可读取。seed必须幂等：同一合成seed版本重复执行返回原记录；需要新的独立世界时显式复制。禁止使用不明默认管理员密码。首次管理员初始化采用本地一次性流程，凭据不会写入seed或内容包，后续操作需要登录与明确作用域。

all模式同时提供API与持久任务执行，适合单机。目标按需拆分命令为 `go run ./cmd/loreviva --mode api` 和 `--mode worker`，两个进程共享同一数据库与领域合同，静态资源由api提供。分进程不改变租约、fence、幂等或共同治理屏障，也不要求立即加入Redis、队列或微服务。多节点时才评估共享存储；本机目录不能因为复制进程变成共享资源保证。

<a id="04-r0完成后的第一个世界任务未执行"></a>

## 04 R0完成后的第一个世界任务

新手从工作台选择合成“雨夜咖啡馆”模板，创建自己的世界项目。对象树包含咖啡馆、入口、座位、木牌与Alex、Mia两名演员。创作者确认背景文字，打开规则表单检查木牌的所有权、Alex的保管权及Mia仅能提议的约束；这些字段不能混成一段不执行的提示词。

在舞台上改变座位位置、入口和可见区，在人物卡改变目标与表达方式，并检查关联影响。当前R0使用Mock候选，界面明确标注“合成结果”，可预览字段差异再采用；普通属性表单始终可用。保存形成草稿修订，取消放弃未保存修改，试演使用隔离实例。创作者能新建、复制、编辑、归档或删除对象；复制不得带走真实玩家关系、账务、任务、凭据或批准状态。

发布检查必须列出必要职责、规则正反例、引用、Skill版本、权利、受众、Mock路由、调用上限和降级。发布得到不可变release；启动实例是下一步，并非发布自动开跑。旧实例继续旧release，编辑新草稿不会改写过去事实。默认一个主要回应演员，一次主文本候选联合输出行为、对白、表现意图和记忆关系候选；结构修复与状态冲突重算共同占最多一次额外槽位。R0通过Mock计数验证这条规则，尚无真实模型采购。

玩家进入后，先展示有权快照和角色，再允许提交互动。咖啡馆脚本覆盖私人告知“我喜欢安静”、公开修木牌、未获保管人许可借牌被拒、有依据的承诺和再次进入。对白说“我已把牌给你”不会自行转移物品；操作成功表现必须引用成立事件。Alex的私人认知不会成为Mia或公共视图的知识。玩家可查看与纠正自己的偏好、忘记相关经历，并看到治理回执。

暂停世界时，界面说明新叙事生成和提交停止，已发出的费用或外部回执仍可登记。关闭浏览器后重新进入，先查询原turn，再取得快照或沿有效cursor继续，不另建同一未知互动。取消任务、停止客户端播放、暂停世界和退款各有独立结果。演示任务最后记录创作者完成率、所需帮助、耗时、失败和断线恢复，不用一次内部演示替代非维护者安装和新手验收。

## 05 从Mock切到真实模型

R0默认不外发。启用实际模型前，先冻结待测用途、数据受众、供应方及实际模型、输入输出上界、采购责任、预算、费用未知恢复能力、保留政策和人评任务；用户或有权客户确认预算与资料外发范围后再启用。

连接能力检查包含结构输出、上下文、工具、多模态、取消、原attempt查询、用量和费用，不以“兼容通用API”代替全部能力证明。普通演员互动的路由按任务覆盖、主要演员、世界、工作空间解析；导演等共同方法合入上下文，未被实际调用的模型不可显示为本次互动已调用。能力缺失阻断相应功能，允许的fallback只能在首次外发前确认主连接不可用时采用。

真实请求先在完整dispatch治理屏障短事务中预留调用槽位和预算、取得外发授权；网络前经相同屏障持久化send_started_at，明确收到提交回执且退出锁后才发出请求，关闭自动SDK重试。SENT表示进入可能发送区间，不证明供应方收到；崩溃后即使可能没有发出字节，仍查原attempt而不自动再发。仅AUTHORIZED且无发送标记不能访问网络；接管须以原attempt重验屏障，槽位和预留不重置。领取worker租约只是短事务更新task与fence，释放后必须再进入dispatch屏障，不能把租约当成外发权限。暂停或撤权先提交时阻断后续授权；授权先提交的在途请求可能仍收费，本地结果提交重验，不能保证跨系统召回。超时、5xx或回执丢失进入原attempt查询及UNKNOWN流程，不能自动换模型再买一次。真实人物质量需公布模型、内容、语言和设备范围，评估稳定立场、不同经历、拒绝、来源记忆、纠正删除、承诺和返回；流程跑通仍只能证明相应工程流程。

## 06 外部客户身份与薄服务端接入

客户保留登录、产品资格、业务权限、玩家钱包和收费权威。Loreviva以租户、产品、世界实例、演员实例、玩家及关系作用域运行，不把一个外部用户ID默认当作跨产品人格键。身份映射使用租户与产品内明确规范的外部ID，空值拒绝，大小写和字节比较规则冻结，完整唯一约束与查询过滤保持一致。

目标接入流程为：客户客户端登录自己的业务；客户薄后端核验会话、资格、允许实例和操作；薄后端以服务端身份请求 `POST /world/v1/client-sessions`；Loreviva签发绑定主体、产品、实例、操作和期限的短期会话；客户端使用生成SDK消费获准接口。客户端不持客户服务器密钥或模型凭据，不直接写钱包、背包、正式成绩或世界事实。令牌交换样例只演示合成身份，真实登录接入需客户完成自己系统的验证。

公开契约采用OpenAPI 3.1.2与JSON Schema 2020-12，Go/TypeScript类型和Schema同源生成并带源哈希。SDK与薄后端支持范围以实际验收发行物为准；不假定已发布npm包，未经核验不能执行`npm install loreviva`。下列HTTP样例均使用合成对象：有权薄后端先创建和START实例、取得Mock报价，再交换绑定现有实例的玩家会话；服务凭据不交浏览器，实际ID和版本代替样例值。

<a id="r0客户创建的真实前置及当前支持边界"></a>

### R0客户创建前置与支持边界

客户创建顺序为InitializeLocal/显式bootstrap→真实Operator/init registrar→customer/service登记及有权WORLD和依赖发布→REGISTER_PLAYER_ACCESS→真实SERVICE创建PRIVATE单player实例→Session。发布先于player access登记；不得SQL造release/participant/permission/receipt。创建本身不START、不授本人positiveuse，SERVICE响应不返回初始private正文。

为integration初导入准备合法SYNTHETIC/OWNED、PRODUCT_READERS、COPY/PUBLISH/REDISTRIBUTE、无expires_at、空refs、无提取定位的World/Character及KNOWLEDGE/NOTE；保留真实来源，不把旧素材抹来源改fixture通过。复杂复制/社区许可/文档提取等未支持来源会整create拒绝并定位字段，须由其真实独立来源桥完成后才宣称支持。它不影响原Admin创建语义。

同player两world按真实E1→W1→原mapping登记E2→W2；不续mapping、身份、权限或原期限。多个未绑定同release资格须解决实际歧义，API无额外selector；不得靠新字段/LIMIT1选世界。原key响应未知只查原result，不另造key。

<a id="s67规范实际采用与未实施边界"></a>

### 登记时间哈希兼容

REGISTER_CUSTOMER/REGISTER_PLAYER_ACCESS使用固定域loreviva.registry.registration.utc.v2的完整UTC命令hash，其它kind不变。旧V1原键只匹配原typed raw输入，不用V1UTC兜底；hash、期限和revision不改，不续期、不授新权。完整历史证明支持已知旧UTC V1与V2 UTC；缺原命令的旧非UTC历史为UNVERIFIABLE，自动R1/CREATE重建为NOT_SUPPORTED，安全查询及原输入回放独立保留。

服务端创建实例，201回执中的 `scope.world_instance_id` 是后续路径ID，初始状态CREATED：

```http
POST /world/v1/instances
Authorization: Bearer <server_service_credential>
Idempotency-Key: demo-create-1
Content-Type: application/json

{
  "product_id": "product_coffee",
  "release_id": "release_mock_1",
  "mode": "PRIVATE",
  "initial_participants": [
    "player_demo"
  ],
  "client_capabilities": [
    "TEXT"
  ]
}
```

服务端发出START，202回执是TaskReceipt。用其中task_id及同一ServiceBearer请求 `GET /world/v1/tasks/{task_id}`；看到task_state为COMMITTED后，再查 `GET /world/v1/instances/{instance_id}` 确认RUNNING并读取当前版本，然后继续报价和会话交换。operator查询仅返回其授权范围内状态/版本及许可元数据，不能读取玩家私密packet；不能把202受理当成已经运行：

```http
POST /world/v1/instances/instance_demo/controls
Authorization: Bearer <server_service_credential>
Idempotency-Key: demo-start-1
Content-Type: application/json

{
  "command": "START",
  "expected_control_version": "0"
}
```

服务端请求合成报价，使用返回quote_id和冻结上界；示例不是实际费率：

```http
POST /world/v1/quotes
Authorization: Bearer <server_service_credential>
Idempotency-Key: demo-quote-1
Content-Type: application/json

{
  "scope": {
    "tenant_id": "tenant_demo",
    "product_id": "product_coffee",
    "world_instance_id": "instance_demo"
  },
  "service_kind": "TURN_TEXT",
  "primary_actor_id": "actor_alex",
  "input_upper_bound": "1000",
  "output_upper_bound": "200",
  "purchase_responsibility": "MOCK"
}
```

薄后端核验自己的玩家会话和权益后交换短期令牌；tenant从服务凭据推导，正文不能冒充另一租户。玩家会话没有START/END权限，PAUSE/RESUME只在获准私人实例范围可用：

```http
POST /world/v1/client-sessions
Authorization: Bearer <server_service_credential>
Idempotency-Key: demo-session-1
Content-Type: application/json

{
  "product_id": "product_coffee",
  "world_instance_id": "instance_demo",
  "external_subject_id": "synthetic-player-1",
  "entitlement_reference": "entitlement_demo",
  "requested_actions": [
    "READ",
    "TURN",
    "CONTROL_PAUSE",
    "CONTROL_RESUME",
    "MEMORY_VIEW",
    "MEMORY_GOVERN"
  ]
}
```

客户端用短期令牌取得 `GET /world/v1/instances/instance_demo/snapshot` 的有权快照，再提交互动。world_revision/control_version使用快照中的精确字符串，expected_versions不含privacy授权：

```http
POST /world/v1/instances/instance_demo/turns
Authorization: Bearer <restricted_client_session>
Idempotency-Key: demo-turn-1
Content-Type: application/json

{
  "expected_versions": {
    "world_revision": "1",
    "control_version": "1"
  },
  "primary_actor_id": "actor_alex",
  "input": {
    "kind": "TEXT",
    "text": "我想坐在安静的位置"
  },
  "quote_id": "quote_demo",
  "requested_audience": "PRIVATE_RELATIONSHIP"
}
```

下列TypeScript示意包装层调用；实际支持签名以所选实物和生成契约为准，不能作为独立可运行接入示例。供应方费用、服务额度与客户玩家权益分别保持。

```typescript
// instanceId、quoteId由获准薄后端提供；调用签名以发行物为准。
const sdk = createLorevivaClient({ baseUrl, sessionToken });
const snapshot = await sdk.getSnapshot({ path: { instance_id: instanceId } });
const accepted = await sdk.submitTurn({
  path: { instance_id: instanceId },
  headers: { "Idempotency-Key": turnKey },
  body: {
    expected_versions: {
      world_revision: snapshot.versions.world_revision,
      control_version: snapshot.versions.control_version
    },
    primary_actor_id: "actor_alex",
    input: { kind: "TEXT", text: "我想坐在安静的位置" },
    quote_id: quoteId,
    requested_audience: "PRIVATE_RELATIONSHIP"
  }
});
const turn = await sdk.getTurn({ path: { turn_id: accepted.turn_id } });
// 202仅受理；消费approved_packets、deliveries和billing的独立状态。
```

公开契约包括实例创建、controls、snapshot、turns、原任务查询、events、cancel及受限memory治理。创建实例返回201且不自动启动，实例ID从 `scope.world_instance_id` 读取；控制正文使用大写 `START/PAUSE/RESUME/END` 与精确字符串 `expected_control_version`。互动返回202且不表示成功，回执包含turn_id、task和quote_id。幂等键在请求Header `Idempotency-Key`，范围含租户、主体、操作、实例和键，同键同规范载荷返回原任务，同键异参返回409 `IDEMPOTENCY_CONFLICT`。同一玩家的网络重试应复用原键；想重新演出须形成新的明确业务动作。

## 07 断线、并发、SSE与私人视图

请求超时先查原turn。任务可处于ACCEPTED、RUNNING、WAITING_EXTERNAL、UNKNOWN、COMMITTED、FAILED、CANCELLED或REVOKED；HTTP查询成功只说明查到了当前状态。COMMITTED的动作不能因为客户端没播放或媒体失败重新提交。`EXTERNAL_UNKNOWN`要求查原attempt，`ALREADY_COMMITTED`要求展示已成立结果，`BUDGET_EXCEEDED`要求停止新增成本；不要用一个“重试”按钮换键重复采购。

快照与起始cursor来自同一一致性边界，cursor不透明并绑定主体、租户、产品、实例、视图epoch与有效期。续页即使没有可见事件也能安全推进，不能暴露裸序号或私人缺口。收到`VIEW_RESET`或cursor过期时清理旧视图缓存，重新获取有权快照；撤权后保留旧缓存继续展示是错误。`SNAPSHOT_STALE`需核对最新状态，客户端不自行决定动作重算。

SSE目标只传安全通知或获准交付更新，丢包后仍以原任务查询和cursor恢复为准。不要把SSE当作唯一事实来源，不为断线重复生成。事件按event身份去重，动作按packet和动作身份去重，交付按delivery_id与packet_revision更新；较旧版本不能覆盖较新版本，后到音频更新不能因文本已展示而丢弃。手动重播不构成新的世界动作。

两名玩家竞争唯一木牌，服务端按当前许可、状态和版本只成立一个结果。私下告诉Alex的偏好、派生邀请或关系条件继承原受众，即使玩家能做公共动作，也不自动获得公开该来源的权利。资源经鉴权代理读取，私有目录不能映射为无认证静态URL。已被客户端保存的字节无法保证远程召回，删除承诺必须说明这条边界。

## 08 .wepkg、Skill与扩展任务

.wepkg格式版本1.0保存逻辑定义、规则、Schema、知识和素材、固定依赖、来源权利及合成测试，不携带本机模型地址、秘密、绝对路径、真实身份、私聊、运行状态或账单。导入按上传、静态预检、内容及权利预览、ID映射、隔离草稿、Mock试演、本地发布进行。预检不调用模型、不安装工具、不启动世界，包内指令不能获得系统权限。

相同包ID、版本和哈希重复导入返回原记录；同版本异哈希显示冲突，新版本生成差异草稿。缺少模型、工具或资源定位到对象，用户选择本地兼容映射后重验，不静默换供应方。导入器拒绝越界路径、类型伪装、超总量、哈希错误、循环依赖和未知可执行原语；实际上限由实施时的契约和发行配置公开。运行备份使用另一受控格式，不能作为社区模板传播。

声明式Skill含ID、版本、哈希、输入输出Schema、用途、能力、许可、工具白名单、依赖、参数、触发、失败策略、成本和用例。社区作者可在已登记原语内编写方法和合成例子，不能靠文本安装程序或扩大权限。Skill与子Skill共享父任务调用、时间和费用，递归不能重置额度。增加可执行工具走独立源码评审、登记和回归。

模型扩展实现能力描述、Generate、QueryAttempt及用量回执接口；表现扩展读取核准packet并声明文字、动作、音频等能力。模型适配器不能直接提交世界，表现适配器不能以播放ACK写事实。3D及其它客户端适配器按声明能力分别验收，不成为R0默认依赖。社区自托管不要求平台充值、在线许可证或付费云。

## 09 五行业合成样例与独立验证

R0定义以下合成规格样例，不声称五行业生产支持。每个样例固定初始状态、输入、预期结果、权限、恢复和客户端责任；所有断言独立报告，不能用咖啡馆一次成功覆盖其他行业。

| 行业合成样例 | 必测分歧 | 客户保持的权威 |
| --- | --- | --- |
| 游戏：借牌任务NPC | 同物品竞争、建议与成立动作区分、背包外部结果未知 | 登录、玩法胜负、背包及经济 |
| 影游：雨夜分支 | 选择窗口、分支存档、镜头意图、资源降级及返回 | 剪辑、设备、素材与交互窗口 |
| 陪同：安静座位 | 私人偏好、更正删除、合理拒绝、关闭邀请及关系延续 | 关系边界、主动渠道、订阅及收费 |
| 教育：咖啡馆语言练习 | 课程来源、教师学生隔离、建议反馈不写正式成绩 | 评分器、成绩、班级与授权 |
| 社区：共享活动 | 公私视图、资格、触发去重、成员离开不暂停全世界 | 成员、公共资源、内容与容量 |

真实行业支持在对应发行门执行差异规格。首个收费客户若需要共享、教育评分或外部副作用，相关验证提前到R2；不能等R3才补齐正在出售的能力。

## 10 升级、备份与删除后的恢复

应用补丁、数据库Schema迁移和内容实例升级分别计划和回执。维护前记录版本、契约哈希、DDL版本、文件适配器及影响，停止新增叙事与成本工作；仍需登记已外发attempt回执和治理动作。先生成迁移预检、兼容与容量报告，再制作数据库及文件配对备份，经核验后执行。内容旧实例固定旧release，不因应用升级自动换人格。

目标维护命令由M4实现，当前未执行：`backup create --output ./var/backups`生成数据库快照、私有文件清单、哈希、Schema及治理水位的配对记录；`restore preflight --backup <ID> --deletion-ledger <受控路径>`检查配对、兼容、最新记录水位和可用空间；`restore apply --plan-id <ID>`只执行已核验计划。具体命令的认证与计划ID格式须经工程契约确认后发布。

DeletionLedger的最新有效撤权删除记录独立于旧备份保全。恢复先保持访问封闭，取得不低于当前权威水位的记录，重放备份之后的删除和撤权，清理原文、摘要、索引、关系与媒体派生，再验证DB和文件配对、权限、检索与代理均不回显已删除资料。拿不到新水位、文件清单损坏或Schema不兼容时阻断恢复开放，不用旧备份中的Ledger自行证明足够新。

COMMIT回执丢失使用稳定提交键在权威主库查询；不要把未知事务当成失败重新调用模型。数据库明确失败才整体回滚并按稳定键有界重试。配置回退不能抹去已成立事件、费用、付款或删除，迁移不可逆时使用预先演练的恢复路径。备份含私人资料，访问、保留、导出与销毁需相应权限；演练首先使用合成数据。

R0维护实现范围（当前救援与连续性规则，尚待Task11实测）：先在可信当前原库/私有根关闭并排空全部参与者，封印完整当前救援集C，再重建并补追最新治理G、核验原Mock请求J，双条件封口后开放。旧B只能隔离演练或修复C当前指定的同hash文件，不能凭删除日志恢复备份后的世界进度和账务。若当前库已丢且无可信完整末端材料，显示BLOCKED_POST_SNAPSHOT_EFFECTS_UNPROVEN，不清零调用或重买。独立保全域不随DB/files覆盖；C是私有受治理材料。具体原plan/key查询、C封印/DB/files分阶段及all/api/worker排空命令待Task11实际CLI冻结，本文不虚构已可执行选项。此路径不宣称完整灾难恢复或恢复从未保全的历史；缺非维护者实证仍未验收。

## 11 故障定位与可提交证据

| 看到的现象 | 首先检查 | 正确下一步 |
| --- | --- | --- |
| 启动未就绪 | 配置、DB连通、Schema、私有目录及Ledger水位 | 修复对应前提，保留错误关联ID；不输出秘密 |
| 创建成功但世界无响应 | 实例是否启动、控制版本、任务状态、worker租约 | 查原任务；暂停状态不新发生成 |
| 同键返回409 | 原任务载荷哈希、操作与实例 | 查原结果或明确新动作，不篡改原幂等键 |
| 任务UNKNOWN | attempt、供应商查询能力、提交键、费用状态 | 查原请求及原提交，不自动重买 |
| 收不到事件或音频 | cursor、VIEW_RESET、delivery版本、媒体状态 | 重建有权视图或补取更新，降级显示核准文字 |
| 私有资源404 | 当前主体、产品、受众及治理版本 | 作为不可见处理，不显示资源存在细节 |
| import失败 | 对象定位、哈希、依赖、路径、许可 | 修改隔离草稿或补映射，旧release不动 |
| 恢复后仍封闭 | 配对校验及最新DeletionLedger重放 | 补齐权威新水位后重验，不能强行开放 |

支持材料包括应用及契约版本、合成重现步骤、任务/事件/attempt/提交关联ID、控制及privacy epoch、fence、公开错误码和脱敏日志。禁止提交Token、Cookie、数据库密码、模型密钥或原始私人正文。日志只能说明相关运行事实，不能凭一段日志宣称角色长期质量或商业效果通过。

## 12 SaaS三层Token启用前验收

R0只提供费用替身和合成账本，真实充值、支付与收款关闭。供应商原始用量、平台Token、客户玩家权益分成三个账本或权威来源，不共享余额。平台Token是服务额度，客户玩家钱包由客户持有；BYOK不重复转售客户直付模型，自托管不需要平台充值。

R2启用前必须固定客户服务SKU、版本费率、输入输出与媒体类别、包含量、精确金额和舍入、报价上界、结算触发、reservation_settle_by、风险承担与限额、付款权威回执、退款拒付和续费规则。金额用DECIMAL或整数最小单位，API用精确字符串，服务事件幂等结算，真实供应商重复收费逐条保留并进入异常。未有可信上界、付款核验或有限预留截止的路由不得收费自动外发。

UNKNOWN到客户预留截止时，授权账务在同一事务追加平台风险posting、释放客户预留及创建对账工单，供应商安全预算仍占用，attempt和调用数不重置。迟到成本默认归平台，不悄悄追扣客户；迟到正文仍重验当前许可、暂停、取消及删除。退款、供应商退费和玩家退款不自动联动。重复乱序回调、COMMIT未知、撤权、恢复、容量、对账和实际支持行业全部验收后，才开启相应收费范围。

<a id="13-后台配置预检与有权任务目录s59task6已本地批准"></a>

## 13 后台配置预检与有权任务目录

固定OperatorBearer/content.read预检展示获准配置、支持字段与单条上界，RESOLVED不表示具有生成权限；真正AI协助沿原content.assist和当前执行资格。task.read目录仅安全ADMIN/当前有权普通WORLD TURN元数据，详情正文、source use与取消独立授权。必传product_id，可选精确task_id；精确查询不能同时带cursor。limit默认50、最大100；空items若仍有next_cursor须继续。每页均按当前资格重新鉴权；身份/治理/filter等绑定变化时拒绝旧cursor，重启后旧cursor失效，客户端从第一页重新查询。这是live keyset；要查看落在已越过键之前的新项，重新查询第一页。WORLD query_url回同一目录且带真实product_id/task_id，ADMIN回原详情操作。新字段/生成SDK以唯一contracts/source实际冻结为准，不新增本指南私有字段或第三接口。

<a id="14-后台任务目录到详情的权限一致s60task6已本地批准"></a>

## 14 后台任务目录到详情的权限一致

已有getAdminTask仅对当前有权且真实已登记的ADMIN提供安全元数据，task.read与当前owner/精确product_id/安装治理均必需；can_execute=0不妨碍查看任务进度。实际TURN及隔离child不能通过ADMIN详情查询，WORLD目录链接仍回原目录精确product_id/task_id查询。原intake登记证明必须成立；返回已提交结果引用时，另须真实业务提交及资产绑定证明。result_asset_id仅为原结果引用，打开正文、生成或取消继续按各自权限。缺失或无权详情安全404，全局身份错误401/403，基础设施失败不得伪装为空或成功。详情读取不增加独立接口或私有DTO。

## 15 制作持续角色与接入个性化体验

本节说明PRD第26节与白皮书第16节的后续设计接入流程。未冻结的LC字段和操作不是现有SDK调用；实施须沿具体版本合同，验证实际支持范围。

创作者先制作角色的稳定核心和差异例子，随后选择当前支持的变化政策：哪些经历可以改变哪项状态、作用于谁、怎样恢复、什么不能改变。再配置一条有明确后果的互动和一个可拒绝机会。每项使用普通属性表和合成正反例；AI可以提出候选，不能替创作者保存、发布或修改真实实例。发布前检查政策/原语支持表及实际试演证据。

用合成玩家分别试验第一次进入、共同经历、一次偏好更正、拒绝机会和再次返回。比较人物核心、当前理解、未完事项、合法世界后果及回应；同时检查另一玩家/演员看不到私人信息。后果预览区分提议、成立和显示，不把舞台动画当世界操作完成。保存草稿、发布固定版本和启动实例仍是独立动作。

客户接入由薄后端确认当前身份、产品与实例，再消费唯一源生成类型。客户端提供当轮表达选择、有权经历/偏好查看、更正遗忘、机会控制、暂停返回和精确反馈入口；不能自行维护另一套关系分数、认知或来源权威。停止播放只停止本端播放，暂停世界与资料治理走各自命令；切号清除旧视图和缓存，迟到结果仍按原身份及当前权限检查。

实例/主题/世界的能力词汇先按实际支持映射协商。文字是基础投影，声音或舞台缺项按已批准规则降级；未知必需能力不能只靠客户端自报开启。世界成立、客户消息持久化、资源可用和终端播放分别显示状态。同步历史不默认重播，晚到音频不重复世界事件；原结果回放与新生成有不同采购含义。

问题反馈默认发送类别和精确交付引用。玩家选择共享正文时另说明目的、期限与可见范围，作者优先制作原创合成复现；个性化更正与全局内容修订分流。每次修改固定被测角色、规则、Skill、模型、素材和政策，比较确定性失败及真实生成质量后发布。改变存量人格先看语义迁移冲突，不能以外观更新入口重置经历。

接入声明按实际已测能力填写：私人文字、具体声音路线与设备、共享成员政策、选择分支、生成媒体分别列状态。未执行标NOT_RUN，不把模板例子、配置保存或参考代码存在写成支持。完整工作流和非维护者操作仍按既有验收合同实测，原始私人资料不进入社区内容包或公开问题附件。
