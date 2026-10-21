# Loreviva 接入与参考客户端

参考客户端连接同源薄后端，由薄后端将本地登录映射到已经登记的客户、玩家、世界与平台凭据。平台仍负责每次请求的当前权限、来源资格、任务、账务和最终提交。参考客户端用于验证这一接入链；它不是部署完成或真实模型、真人体验、商业结果的证明。

## 运行环境与入口

工作区使用 Node 24、项目声明的 pnpm 版本及现有锁文件。SDK 的运行时导入使用显式 `.ts` 后缀，生成验证器使用 `.cjs`；Node 运行 TypeScript 不替代严格类型检查。SDK 保留原配置接口，能力查询与机器源描述的 65 个操作分别处理：类型上存在的操作不代表当前服务器已挂载、账户可用或获得权限。

在已准备依赖的工作区根目录：

```sh
pnpm check:reference
pnpm test:reference
pnpm build:reference
```

`check:reference` 检查 SDK、薄后端、接入类型、浏览器目标及 Vue。`test:reference` 使用 reference-client 的真实 Vitest 项目收集 Node 协议测试和 jsdom 页面状态测试；它不执行真实平台浏览器目标。构建产物位于 `apps/reference-client/dist`，由薄后端复制到受限静态资源映射中提供，不提供任意文件代理。构建目前有较大的主 JavaScript 分块，实际体积及加载影响需要随发行评审记录。

平台数据库和客户数据库必须由运行负责人提供各自明确的测试身份和生命周期。真实端到端目标是 `TestReferenceClientBrowserFixture`，它启动原平台、客户程序、薄后端及 `reference-player-real-loopback` 页面目标，并检查两库真实提交状态。直接运行浏览器测试不会创建这些前置资源。测试捕获器还负责运行预算、生成材料扫描、进程归还与私有证据；不要把仅构建或 jsdom 通过写成真实浏览器通过。

## 初始化与启动薄后端

薄后端入口是 `examples/thin-backend/reference-bff.ts`。`initialize` 从标准输入读取一行封闭的 `JournalOptions` JSON，创建新的私有日志安装；`serve` 读取一行 `ReferenceBffOptions` JSON 后保持标准输入开启。首行之外只接受 `STOP` 加换行；标准输入关闭也会触发收尾。命令分别为：

```sh
pnpm reference:initialize
pnpm reference:serve
```

这些命令需要运行负责人通过私有管道提供真实配置，不会交互询问或自动生成平台授权。配置结构以该文件和 `command-journal.ts` 的导出类型为准：

| 配置 | 责任 |
| --- | --- |
| `platformOrigin`、`port` | 平台 HTTP(S) origin；主薄后端固定监听 `127.0.0.1:27185`，端口占用时失败 |
| `journal.root`、`installation` | 独立私有安装目录及安装身份；初始化与重开是不同操作 |
| `journal.windows` | 真实 Windows helper 路径、字节指纹、原生文件身份与当前用户 SID；不能用模式位或布尔值替代 DACL 验证 |
| `users` | 已验证的登录凭据摘要，以及精确 tenant/product/customer/serviceSubject/player/externalSubject/entitlement/world/profile/actions 映射 |
| `serviceKey` | 仅由后端持有；不进入浏览器、页面存储或公开配置 |
| 可选 `customer` | 固定客户程序的实际指纹与各客户/世界对应的私有数据库连接；只返回真实确认数量和状态 |

初始化建立的密钥与日志应作为同一安装材料保存。重开不会补造缺失密钥、原记录或关联；恢复不了的原操作保持 UNKNOWN。日志实施了单写者、序列、认证加密、大小与记录数限制，以及首发前实际 write/sync/Close。它不证明断电耐久、日志全删或合法前缀回退可检测，也不承诺网络 exactly-once。

## 登录、本人权限与写操作

浏览器只持有同源 HttpOnly 登录 Cookie 和当前 CSRF 上下文；平台 Service key 与 CLIENT bearer 留在后端。请求校验 Host、Origin、CSRF 和当前本人映射。退出、切号、撤回和视图失效会取消旧请求、清空私人缓存并回收媒体 URL；迟到结果不能填入新身份的界面。

先取得当前本人会话和能力，再进行本人 INITIAL、RENEW、REVOKE。声明的 `PRIVATE_RESPONSE` 标签不是正文使用授权。记忆列表默认提供当前本人可见元数据；显式 provenance 正文读取还必须满足来源获知和当前正文许可。`public_v1` 单条默认返回 `Memory`，`integration_r0_v1` 返回 `PlayerMemoryMetadata`；显式 `include=provenance_v1` 返回 `MemoryProvenanceV1`。

当前客户端私人正文与 provenance 证明覆盖初始资料、由当前拥有写入口受理的 TURN 输入、已提交的衍生记忆及其可证明的 CORRECT 链。输入与衍生批次在原受理或提交事务内保存无正文的出生证明，与原任务、回执、完整成员及来源获知关系绑定；读取时还须满足当前本人许可、正文权限、用途、来源资格与最终校验。正常互动后可在这些条件成立时查看本人记忆，再更正、续授权或删除；RENEW 不授予缺失权限，也不恢复已清除正文或已撤销来源。

旧实例若缺少原生产者证明，不从现有记忆快照、ID 或后续请求补造历史。许可内元数据与必要治理仍沿原资格处理，不能完成证明的显式正文与 provenance 请求拒绝。删除和更正后的证明仅保留受治理的承诺与关联，不恢复旧文本；这些无正文记录仍是私人数据。该承诺链依赖原拥有写入和数据库的信任边界，不是抵御完整数据库协同篡改的外部签名。

CREATE、CONTROL、报价、TURN、本人许可、记忆治理、取消这七类写操作均经过同一个 broker。后端完成可信映射和源类型校验，冻结最终 method、target、key、profile、原始正文及哈希；日志完成持久化后才发送这些相同字节。调用方不能提供任意平台 URL、原始正文回调或重建发送许可。CLIENT 权限不会因薄后端关联存在而增加。

记忆更正和删除使用原 MEMORY_GOVERNANCE_ACCEPT → PROGRAM_OWNED 链，接受后显示真实 TaskView。撤回正向使用后，符合当前本人治理资格的必要清理仍可继续；治理任务不套用普通 TURN 取消按钮。任务无原关联时保持 UNKNOWN，不能从私人 SQL 猜出任务或自动重发。

## 丢失响应与视图恢复

浏览器在首发前保留原 operation/profile/key。首次响应整包丢失后，`POST /bff/find-original` 只在当前本人身份下查询已有日志精确关联，不创建记录、平台业务查询或首发。无记录的 404 仍是 UNKNOWN；得到关联后再使用原 original/queryOriginal。查找和原查询合用最多 20 次、至少间隔 1 秒及更长 Retry-After 的预算，不能通过换键、刷新页面或重启后端制造新业务重发。

VIEW_RESET 应丢弃原视图与 cursor，取得新的 snapshot 后继续。cursor 的有效期和任务、会话期限不会因轮询延长。SSE 按完整帧校验后才推进已接受位置；半帧 EOF、错误 UTF-8、超限帧或迟到事件不更新界面。JSON 错误、SSE 和媒体二进制使用各自的解码路径，416 是源定义的 JSON Error。

## 媒体与客户交付

每次播放都通过当前授权 fetch 获取媒体，不把 Service key 塞入 `<audio src>`，不生成额外签名 URL。播放器把授权字节绑定当前身份 epoch，使用 Blob URL 并在停止、切号、撤回、关闭时回收；私人响应使用 no-store。206 片段不能冒称完整正文哈希验证。

任务 COMMITTED、文本可见、媒体 READY 和客户已确认是独立状态。客户 Inbox 与 Message 先在客户库同一事务提交，再向平台发送原 receipt；只有实际确认数量和状态可以展示。客户库 UNKNOWN 或平台 receipt UNKNOWN 必须沿原身份查询，不能再次制造 Message、媒体或通知。

## 验证与已知边界

本工作包的真实浏览器记录限定 Windows 上实际核验的 Chrome 版本和专用临时 profile，不代表其它浏览器或环境均通过。模型输出与媒体为合成测试材料，没有真实模型采购或真实用户验证。

发行评审仍需保留过小报价已受理后原 worker 返回 `HARNESS_CONTEXT` 时的 RUNNING 终结与解释，以及主 JavaScript 分块体积。TURN 输入及原 Mock 空、非空衍生批次的验证，不等于任意外部模型、未知生产者或所有恢复分支已验收；缺失旧证明仍按上文拒绝正文。无记录与跨本人 original 查询统一返回不暴露原记录的本地 404；它不等于上游成功，也不能触发业务重发。

工程字节检查采用显式 source inventory：编辑、增加或删除文件后，由受控 `scope:refresh` 及明确 delta 生成新不可变 revision，再独立执行 `check:bytes`。检查不自动重算期望值；这不是 Git 提交、公开分发批准或产品验收。
