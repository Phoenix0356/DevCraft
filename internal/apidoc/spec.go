package apidoc

// 规范文本里大量出现反引号（围栏块与 inline code），而 Go 原始字符串（反引号定界）
// 内部写不了反引号——因此用编译期常量拼接：文本仍是「一个 Go 常量」（const 表达式），
// 满足 PRD D15/R12 的单一真相源要求：
//  1. 注入 internal/skill/apidoc 的 prompt（chat 里按规范生成文档 / 体检文档）；
//  2. 界面「导出 api-doc-spec.md」= 写同一常量到文件，字节一致（AC9）。
const (
	q      = "`"
	fence3 = "```"
)

// SpecText 接口变更文档规范 v1 全文 + 最小示例。
// 内容以 .trellis/tasks/10-18-api-docs/spec-draft.md（实现契约）为准。
const SpecText = `# DevCraft 接口变更文档规范 v1

> 本文是「接口变更文档」的单一真相源：chat skill 提示词与界面导出的 api-doc-spec.md 都来自本文（字节一致）。
> 用法：业务侧写完代码后，让 agent 按本规范产出一份变更文档（接口长什么样 + 本次改了什么 + 为什么改），
> 交给 DevCraft 导入。导入 = 确定性程序解析：更新接口条目定义 + 生成一条变更事件
> （版本 + 备注 + 涉及接口 + 字段级 diff）。程序校验是唯一权威闸门——AI 说没问题但程序不过，照样拒收。

---

## 1. 文档三段式

一份文档 = 一个项目（一个后端服务/代码仓库）的一次变更批次。骨架：

` + "# 用户服务接口变更文档\n\n" + fence3 + `apidoc-meta
spec: 1
project: paper-manager
service: 用户服务
base_path: /api/v1.0/user
scope: incremental
change_type: feature
change_title: 新增论文下载记录同步接口
change_note: 前端需要跨端展示下载历史，旧接口不变
version_tag: REQ-1024
` + fence3 + `

## 一、订阅源管理

### 8.2 新增订阅
...接口块...

结构硬规则：

* meta 用带自定义 info string 的围栏块 ` + fence3 + `apidoc-meta ，不用 --- front-matter——正文里 --- 是分隔线，会撞车。
* meta 内只允许一层 key: value（程序手写 split，不是 YAML）。未知键 → E-META-UNKNOWN-KEY。
* # 一级标题无结构意义；## 标题 = 领域；### N.M 标题 = 接口条目；#### = 接口小节。
* 接口块必须位于某个 ## 领域之下，否则 E-ENDPOINT-NO-DOMAIN。
* ### N.M 的序号不进接口身份、不参与任何逻辑（插条目导致序号顺延是安全的）。
* #### 小节名按前缀匹配归一到 7 种 kind（允许「请求参数（ArxivPaperDTO）」这类括号变体）：

| kind | 前缀匹配 |
|------|----------|
| 请求参数 | 请求参数 |
| 路径参数 | 路径参数 |
| 返回参数 | 返回参数 |
| 请求示例 | 请求示例 |
| 返回示例 | 返回示例 |
| 错误响应示例 | 错误响应示例 / 错误响应 |
| 测试 curl 命令 | 测试 curl 命令 |

* 代码围栏内的内容永不参与元数据/表格解析——multipart 原始报文（内含 Content-Type: 与
  Content-Disposition: 行）可以放心原样贴进「请求示例」。

---

## 2. meta 字段字典

| 字段 | 必填 | 取值 | 说明 |
|------|------|------|------|
| spec | 是 | 1 | 不等于 1 → E-SPEC-VERSION |
| project | 是 | 字符串 | = 项目标识（一个后端服务/仓库） |
| service | 否 | 字符串 | 显示名，缺省同 project |
| base_path | 否 | 路径 | 只做断言、绝不剥离（§4） |
| scope | 是 | incremental / init / full | MVP 支持前两档；full 解析可识别但导入报 E-SCOPE-FULL-NOT-SUPPORTED |
| change_type | scope≠init 必填 | feature / fix / refactor / docs | 事件意图分类 |
| change_title | 同上 | 一行 | 事件标题 |
| change_note | 同上 | 一行 | 为什么改——本功能的核心价值，必填 |
| version_tag | 否 | 字符串 | 需求单号/发布标签；同时是「按需求聚合」视图的分组键，事实上准必填 |

---

## 3. 接口块

` + "### 8.2 新增订阅\n\n为当前用户新增一条 Arxiv 订阅。\n\n" +
	"- **URL**：" + q + "POST /api/v1.0/user/subscription" + q + "\n" +
	`- **Content-Type**：` + q + `application/json` + q + `
- **认证**：需要登录
- **变更**：修改
- **变更点**：请求体新增可选字段 source
- **breaking**：false

#### 请求参数

| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |
|--------|------|------|------|--------|------|
| topCategory | String | 否 | Body | | Arxiv 顶级分类 |
| source | String | 否 | Body | | 来源渠道 |

#### 请求示例
#### 返回参数
#### 返回示例
#### 测试 curl 命令

### 3.1 元信息行（bullet，全角「：」，标签用 ** 包裹）

| 行 | 必填 | 规则 |
|----|------|------|
| - **URL**：` + q + `METHOD /path` + q + ` | 是（唯一硬规则） | 方法 ∈ GET/POST/PUT/DELETE/PATCH 大写；路径以 / 开头；不符 → E-URL-FORMAT / E-METHOD-INVALID |
| - **变更**：新增\|修改\|删除\|未变更 | 是（init 除外） | 缺失 E-CHANGE-MISSING；越界 E-CHANGE-ENUM |
| - **变更点**：<一行> | 变更=修改/删除 时必填 | 原文存进明细 declared_note，不改写；缺失 E-CHANGEPOINT-MISSING |
| - **breaking**：true\|false | 否 | 人工覆盖。声明 false 而启发式判破坏 → 拒收（E-BREAKING-DENIED）；声明 true 而启发式非破坏 → 接受 |
| - **Content-Type**：... | 否 | 有 Body/multipart 建议写；缺失只出警告 |
| - **认证**：需要登录\|无需登录\|内部Token | 否 | 枚举变化参与 breaking（§7） |
| - **Header**：... | 否 | 如内部 RPC 的 X-Internal-Token |

字面约定极窄、没有弹性（这是刻意设计，别写方言）：

* bullet 必须形如 - **标签**：值 ——全角冒号、冒号在 ** 之外。
  写成 **URL:** 或 **URL**: 一律不识别（E-URL-FORMAT），该接口块不入库。
* URL 的方法与路径写在一对反引号里；路径必须以 / 开头。

### 3.2 参数表（表头驱动，绝不按列序）

* 允许列名字典：参数名、字段、类型、必填、位置、默认值、说明。出现字典外的列 → E-PARAM-COL-UNKNOWN（防漂移）。
* 规范模式必填列：「请求参数」「路径参数」表必须含 参数名|类型|必填|位置；缺「位置」列 → E-POS-COL-MISSING。
  「返回参数」与命名结构表只要求 名称|类型（参数名/字段 + 类型）。
* 必填只接受 是 / 否；没有该列 = 未知（required=-1），不是「否」——返回参数表通常没有必填列，这是真实的第三种状态。
* 位置 ∈ Path / Query / Body / Header。
* 参数名同表重复 → E-PARAM-DUP；数据行列数与表头不符 → E-PARAM-ROW-COLS。
* 交叉引用写法在规范模式一律拒收：「同新增订阅。」「元素结构见 7.1」→ E-CROSS-REF，必须原样展开参数表。
  理由：不展开则字段表为空，重复导入时「空 vs 空」永远判未变化，真实改动被静默吞掉（比误报更危险）。
* 命名结构子表（**SubscriptionVO 结构**： 这种粗体标题 + 表）：作为命名结构入库，
  主键 (name, owner_scope)，owner_scope = 宿主接口或文档级。同名不同构 → 标记 ambiguous
  并拒绝按名自动展开，只报 W-STRUCT-DIVERGED。

### 3.3 示例与 curl

* 原文保存、永不结构化、不参与变更判定——只改示例/curl/中文说明不算接口变更（防「文档润色」刷屏假事件）。
* 示例块会尝试 JSON 校验：失败 → W-EXAMPLE-JSON（可能是 multipart 原始报文或请求行示例，属正常，预览确认即可）。
* curl 里的 method/path 与 **URL** 行不一致 → W-CURL-MISMATCH。
* curl 是优秀的校验源，但绝不能当身份源：占位符在 curl 里被替换成了实例值（…/subscription/1002）。

---

## 4. 接口身份与归一化

` + "identity_key = upper(METHOD) + \"|\" + normPath(path)" + `

normPath 只做两件事：

1. 占位符折叠：{任意} → {}（所以 {id} 与 {paperId} 同 key，占位符改名是 cosmetic、非破坏）；
2. 去尾部多余 /、折叠重复 /。

明确不做（都是实测踩过坑的方向）：

* 不剥离 base_path——GET /api/v1.0/user 的 URL 可能与 base_path 逐字相等，剥离后退化成空串、
  这条真实接口永久无法入库。base_path 只做断言：不以 base_path 开头 → W-BASEPATH-ASSERT-FAIL（不阻断）。
* 不做大小写归一——/arxiv/save 与 /arxiv/saveBatch 是两个真实接口，lowercase 会把它们静默合并成一条。

原样保留 path_raw（含 {id} 变量名）用于渲染与复制 curl。

类型双字段：

* type_raw：显示保真（List\<Long\> / boolean / Boolean 原样）；
* type_key：比较归一——List\<T\> / List<T> / T[] → List<T>；HTML 实体反转义；Boolean/boolean 归一时统一。
  反转义会跳过 inline code 区间，代码跨度里的字面内容不会被改坏。

---

## 5. 领域（章节）判定与命名

* 判定用结构规则、不用标题字符串黑名单：该 ## 之下存在至少一个带 **URL** bullet 的 ### → 是领域；
  否则是非接口章节（概述 / 错误响应 / 接口汇总 / 结构说明会被自动排除）。
* 领域名归一顺序（不可颠倒）：
  1. 剥中文/阿拉伯序号前缀（一、 十二、 1.）；
  2. 剥尾部括号限定语并转 flag：（内部）→ visibility=internal——必须先走这步，否则下一步剥不掉尾部「接口」；
  3. 仅剥尾部的「接口」二字（剥后为空则保留原文）；
  4. 空白折叠。
  例：「七、RPC 论文写入接口（内部）」→ 领域名「RPC 论文写入」+ internal；「一、Token状态接口」→「Token状态」。
* 领域名取自标题、不取自 URL 段（URL 段大多推不对：/operation-log vs「操作留痕」）。
* 章级 preamble（如「认证方式：X-Internal-Token」「调用方：paper-pilot-harvester」）下渗给该章所有子接口。
* 内部端点入库并打 visibility=internal、列表默认过滤——不是跳过章节（跳过会让接口计数对不上）。

---

## 6. 什么算「变更」（双 hash）

| hash | 内容 | 用途 |
|------|------|------|
| semantic_hash | method、path（占位符个数与位置，不含变量名）、参数集合 (位置,名称,type_key,必填,默认值)、返回结构 (字段名,type_key) 与命名结构集合、Content-Type、认证枚举 | 进 diff 判定、进事件明细 |
| doc_hash | 中文说明、接口名、示例内容、curl 文本、表头列集合 | 只变 doc_hash → 不发事件（只更新条目原文） |

理由：| message | String | 响应消息 | 这类 envelope 行在 20 张表里逐字重复——说明/示例一旦被算进
变更判定，一次全局润色就能造 20+ 条假事件，直接打爆「重复导入 diff 为空」的幂等验收。

字段级 diff 的身份在数据库唯一索引 (owner_kind, owner_id, kind, name) 上。
声明「未变更」的全量重述接口不写入事件明细，只出现在导入预览。

---

## 7. breaking 三态判定

| 档 | 规则 |
|----|------|
| 1 自动破坏 | 新增必填请求参数；既有参数必填 否→是（含默认值被清空）；响应字段删除；类型变更（按 type_key 比）；路径静态段或 method 变更；Content-Type 变更（json ↔ multipart）；认证从「外部可调用 → 内部Token」 |
| 0 非破坏 | 新增接口；新增可选请求参数；响应新增字段 |
| 2 需人工确认（等同破坏，不确认不写） | 请求参数删除（结构上算破坏，实际客户端照发、后端多忽略）；字段改名（removed+added 同型）；鉴权要求其他方向变化；状态码/错误码语义；幂等/分页默认排序等纯散文语义 |
| cosmetic（不记） | 说明/curl/示例/中文名/字段顺序；占位符改名 {id}→{paperId}（客户端实际 URL 一字未变） |

接口块可以显式写 - **breaking**：true|false 做人工覆盖：声明 false 而启发式判破坏 → 拒收报错；
声明 true 而启发式非破坏 → 接受（人比规则懂业务）。

diff 载荷形态（字段级，存进事件明细）：

` + fence3 + `json
[{"section":"request.body","key":"subCategories","op":"updated",
  "before":{"name":"subCategories","type":"String","required":0,"default":"","desc":"子分类，多个用逗号分隔"},
  "after":{"name":"subCategories","type":"String","required":1,"default":"","desc":"子分类，多个用逗号分隔"},
  "breaking":true,"rule":"request-property-became-required"}]
` + fence3 + `

* before/after 存「能独立渲染的最小完整片段」（自带完整字段描述），不是只带 JSON Pointer 的补丁——
  前端直接渲染，不回查上下文。required 用 -1/0/1 哨兵（-1 = 未知，与库表一致）。
* op = added|removed|updated|renamed|moved；section 固定词表：
  meta / path.params / request.query / request.body / request.header / response.body
  （这让「参数从 Query 挪到 Body」这类真实破坏能被显式表达）。
* rule 取 oasdiff 的 check id 词表（如 request-property-became-required），便于 UI 统一 tooltip。
* removed 行例外：载荷存被删接口的「请求必填参数 + 响应字段清单」最后一次完整最小片段
  （这是「不存快照」的唯一局部例外，否则删除事件在 UI 上是空壳）。
* 路径/方法改名落一条 moved（{before:"POST /a", after:"POST /b"}），不拆成 removed+added——
  否则前端会读成「接口下线 + 新接口上线」。

---

## 8. 删除语义（最容易出事的地方）

1. MVP 只有 incremental + 显式声明删除：库里有、文档没写的接口一律不动、也不报删除。
   误删的代价是对外说谎——前端同学看到「接口下线」就会去改代码。
2. 删除必须写成 ### x.x 名字 + - **URL**： + - **变更**：删除 + - **变更点**：；参数/响应表可省略。
   库里找不到 → E-DELETE-NOT-FOUND。
3. 软删（status=removed + deleted_at）：事件要能长期引用「当时那个接口」；重新出现则 change_type=restored。
4. 「疑似重命名」不做相似度合并，老实报 1 增 + 1 删，导入报告里给一行参考提示。

---

## 9. 校验输出与错误码

E-* 阻断整批导入（事务回滚、零写入）；W-* 只在预览提示。输出是结构化列表：

Issue{ level: "E"|"W", code, line, snippet, want, hint } ——行号必给（chat 里 AI 靠它把错误码翻译成人话）。

| 码 | 含义 |
|----|------|
| E-SPEC-BLOCK-MISSING / E-SPEC-VERSION / E-SCOPE-FULL-NOT-SUPPORTED | meta 块缺失 / 版本不支持 / full 模式未实现 |
| E-META-FIELD-MISSING / E-META-ENUM / E-META-UNKNOWN-KEY | meta 字段 |
| E-ENDPOINT-NO-DOMAIN / E-ENDPOINT-DUP | 接口块不在 ## 下 / 同文档内 key 重复 |
| E-URL-FORMAT / E-METHOD-INVALID | URL 行 |
| E-CHANGE-MISSING / E-CHANGE-ENUM / E-CHANGEPOINT-MISSING | 变更声明 |
| E-PARAM-COL-UNKNOWN / E-PARAM-ROW-COLS / E-PARAM-DUP / E-POS-COL-MISSING | 参数表 |
| E-CROSS-REF | 引用式写法（「同新增订阅。」） |
| E-ADDED-EXISTS / E-UPDATED-NOT-FOUND / E-UPDATED-NO-DIFF / E-DELETE-NOT-FOUND | 声明与库状态矛盾 |
| E-DIFF-MISMATCH-ADDED / -UPDATED / -UNCHANGED | 声明集合 vs 程序 diff 集合不一致（逐项列两边结果） |
| E-BREAKING-DENIED | 文档声明 breaking: false 但启发式判破坏 |
| W-BASEPATH-ASSERT-FAIL / W-EXAMPLE-JSON / W-CURL-MISMATCH / W-STRUCT-DIVERGED / W-POS-INFERRED / W-EMPTY-DOMAIN / W-UNKNOWN-SECTION / W-UNPARSED-BLOCK / W-SUMMARY-DIVERGED | 警告 |

降级三件套（「不许猜」的落地位）：① 结构化 Issue 列表；② 条目级 parse_state = ok|partial|unparsed
+ Unresolved 槽位（记「哪个槽位没解析出来」，不是整条丢掉）；③ raw_markdown 原文切片。
文档自身的 bug（定义与示例不一致、类型混用）在导入报告里与变更事件分栏。

完整性自检（报告用，不作删除依据）：C1 汇总闭合——正文 URL 集 vs 末尾「接口汇总」表
（表头含 方法+URL 列的表）双向差集 = 0；C3 base_path 前缀断言。
汇总表只能校验到 (method, path)：「接口」列名称常与 H3 标题不一致，绝不可用名称做连接键。

---

## 10. scope: init（灌现存散文文档）

同一套管线，放宽：

* 允许缺 变更 / 变更点 行：库里没有 → 新增；已有 → 覆盖定义并记「未变更」；
* 不产生事件（可选手动补一条「初始化 baseline」）；
* 缺「位置」列按 method 推断（GET/DELETE→Query，POST/PUT/PATCH→Body；标题为「路径参数」的表→Path），
  每个推断点打 W-POS-INFERRED 并列出受影响接口，预览要求人工核对；
* E-CROSS-REF 降为 W-CROSS-REF：条目保留、参数表留空、parse_state=partial、预览标红「需人工补全」；
* 目标：散文文档全部灌入，且所有推断/缺口可见（不静默）。

---

## 11. 最小示例（完整可粘贴）

` + "# 演示服务接口变更文档\n\n" + fence3 + `apidoc-meta
spec: 1
project: demo-service
scope: incremental
change_type: feature
change_title: 新增用户查询接口
change_note: 首页需要展示用户昵称
version_tag: DEMO-1
` + fence3 + `

## 一、用户接口

### 1.1 查询用户

按 ID 查询用户信息。

- **URL**：` + q + `GET /api/v1/demo/users/{id}` + q + `
- **认证**：需要登录
- **变更**：新增
- **变更点**：新接口，首页展示昵称用
- **breaking**：false

#### 路径参数

| 参数名 | 类型 | 必填 | 位置 | 说明 |
|--------|------|------|------|------|
| id | Long | 是 | Path | 用户ID |

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | UserVO | 用户信息 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

**UserVO 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| id | String | 用户ID |
| nickname | String | 昵称 |

#### 返回示例

` + fence3 + `json
{
  "message": "操作成功",
  "data": { "id": "1", "nickname": "张三" },
  "success": true,
  "code": 200
}
` + fence3 + `

#### 测试 curl 命令

` + fence3 + `bash
curl -X GET http://localhost:8080/api/v1/demo/users/1 \
  -H "Authorization: Bearer <token>"
` + fence3 + `

---

## 12. 给生成 agent 的提示要点（随本文一起交付）

* URL 行是唯一硬规则，必须 - **URL**：` + q + `METHOD /path` + q + `（全角冒号 + 反引号包裹）；
* 每个接口必须写 **变更**；修改/删除必须写 **变更点**；
* 禁止「同新增订阅。」这类引用式写法，参数表必须原样展开；
* 请求参数/路径参数表必须带「位置」列（Path/Query/Body/Header）；
* 不要为了「让校验通过」而删字段或改类型——校验器只读不改，写什么就入库什么。
`
