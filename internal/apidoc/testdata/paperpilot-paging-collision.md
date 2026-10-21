<!-- 最小复现 fixture（源：用户真实文档 PaperPilot-接口文档-v1.md 第 1414–1554 行「九、收藏夹接口」9.1/9.2 与第 2037–2043 + 2111–2172 行「十三、论文反馈接口」13.2）：不可就地修改；修改需同步更新 internal/apidoc 解析测试断言。脱敏说明：文档里原本就是 `Bearer <token>` 占位与内网 localhost:9901，按现状保留，全文无任何真实密钥；业务文案与标题原样保留。本片段的价值就在于它是真实方言——**FavoriteVO 结构** 与 **FavoriteVO 分页结构** 同族并存，10-21 之前会被归一成同一个名字。 -->
# PaperPilot 接口变更文档 · 分页结构撞名最小复现片段（scope: incremental）

```apidoc-meta
spec: 1
project: paper-pilot
service: PaperPilot 后端（manager）
scope: incremental
change_type: fix
change_title: 修复第4至6项测试缺陷 D01–D12（后端接口面变更）
change_note: 客户端反馈发现页速览接不到详情、BYOK 上传失败、更新弹窗两按钮不可用、列表只能显示内部编号、工作台数量为空；后端补齐更新处理与计数端点、列表展示字段与权威 paperId，并把简报出参 briefJson 结构化。
version_tag: FIX-D01-D12-20261020
```

## 九、收藏夹接口

收藏/暂缓/忽略三态对同一论文**互斥**：处于任一状态时写入另外两个状态会被拒绝（需先撤销）。

### 9.1 收藏列表

获取当前用户收藏的论文列表，按收藏时间倒序。title/arxivId/doi 读取时实时 join `t_paper` 补齐（D06：客户端列表展示不依赖 paperId）。

- **URL**：`GET /api/v1.0/user/favorite`
- **认证**：需要登录
- **变更**：修改
- **变更点**：D06：分页元素 FavoriteVO 新增响应字段 title、arxivId、doi（实时 join t_paper）
- **breaking**：false

#### 请求参数

| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |
|--------|------|------|------|--------|------|
| page | int | 否 | Query | 1 | 页码（小于1归一化为1） |
| size | int | 否 | Query | 20 | 每页条数（夹到 1~100） |

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | IPage\<FavoriteVO\> | 分页结果 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

**FavoriteVO 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| id | String | 收藏记录ID（JSON序列化为字符串） |
| paperId | String | 论文ID（仅操作键，勿作展示列） |
| title | String | 论文标题（实时取 t_paper，缺失时 null） |
| arxivId | String | 公开来源标识 arXiv ID（可 null） |
| doi | String | 公开来源标识 DOI（可 null） |
| briefSnapshot | String | 收藏时的简报快照（JSON字符串，可 null） |
| createTime | String | 收藏时间（ISO 8601） |

**FavoriteVO 分页结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| records | List\<FavoriteVO\> | 当前页数据 |
| total | long | 总记录数 |
| size | long | 每页条数 |
| current | long | 当前页码 |
| pages | long | 总页数 |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "records": [ { "id": "9001", "paperId": "3001", "title": "Attention Is All You Need", "arxivId": "1706.03762", "doi": "10.48550/arXiv.1706.03762", "briefSnapshot": null, "createTime": "2026-06-01T10:00:00" } ],
    "total": 1, "size": 20, "current": 1, "pages": 1
  },
  "success": true, "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X GET "http://localhost:9901/api/v1.0/user/favorite?page=1&size=20" -H "Authorization: Bearer <token>"
```

### 9.2 添加收藏

收藏一篇论文，可附带简报快照。paperId 必须存在；`briefSnapshot` 提供时仅校验 JSON 合法性（非法返回 code 400）。三态互斥与重复收藏均报错不写入。

- **URL**：`POST /api/v1.0/user/favorite`
- **Content-Type**：`application/json`
- **认证**：需要登录
- **变更**：修改
- **变更点**：D06：响应 FavoriteVO 新增 title、arxivId、doi
- **breaking**：false

#### 请求参数

| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |
|--------|------|------|------|--------|------|
| paperId | Long | 是 | Body | | 论文ID |
| briefSnapshot | String | 否 | Body | | 简报快照（JSON字符串，须合法JSON） |

#### 请求示例

```json
{ "paperId": 3001, "briefSnapshot": "{\"title_zh\":\"注意力机制综述\"}" }
```

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | FavoriteVO | 新增的收藏记录 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

**FavoriteVO 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| id | String | 收藏记录ID（JSON序列化为字符串） |
| paperId | String | 论文ID |
| title | String | 论文标题（实时取 t_paper） |
| arxivId | String | arXiv ID（可 null） |
| doi | String | DOI（可 null） |
| briefSnapshot | String | 简报快照（可 null） |
| createTime | String | 收藏时间（ISO 8601） |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": { "id": "9001", "paperId": "3001", "title": "Attention Is All You Need", "arxivId": "1706.03762", "doi": "10.48550/arXiv.1706.03762", "briefSnapshot": "{\"title_zh\":\"注意力机制综述\"}", "createTime": "2026-06-01T10:00:00" },
  "success": true, "code": 200
}
```

#### 错误响应示例

```json
{ "message": "该论文已被忽略，请先撤销忽略", "data": null, "success": false, "code": 500 }
```

其他业务错误：`paperId 不能为空`、`briefSnapshot 不是合法的 JSON`（code 400）、`论文不存在`、`已收藏该论文`、`该论文已在暂缓池中，请先移出暂缓池`、`添加收藏失败`。

#### 测试 curl 命令

```bash
curl -X POST http://localhost:9901/api/v1.0/user/favorite \
  -H "Content-Type: application/json" -H "Authorization: Bearer <token>" \
  -d "{\"paperId\":3001}"
```

## 十三、论文反馈接口

用户提交论文反馈；处理状态推进**不开放用户侧接口**，由算法侧经内部 RPC（15.5）回写。同一论文可提交多条反馈（每次新增一条，无唯一约束）。

- 处理状态：`pending`（待处理，提交初始态）→ `processing`（处理中）→ `resolved`（已处理）
- `processVersion`：处理次数计数器（每次状态回写原子 +1），与论文版本无关

### 13.2 我的反馈列表

分页获取当前用户提交的反馈，按提交时间倒序。

- **URL**：`GET /api/v1.0/user/feedback`
- **认证**：需要登录
- **变更**：未变更

#### 请求参数

| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |
|--------|------|------|------|--------|------|
| page | int | 否 | Query | 1 | 页码 |
| size | int | 否 | Query | 20 | 每页条数（夹到 1~100） |

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | IPage\<FeedbackVO\> | 分页结果 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

**FeedbackVO 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| id | String | 反馈记录ID（JSON序列化为字符串） |
| paperId | String | 论文ID |
| feedbackType | String | 反馈类型（可 null） |
| userNote | String | 反馈说明 |
| status | String | pending / processing / resolved |
| statusLabel | String | 待处理 / 处理中 / 已处理 |
| processVersion | Integer | 处理次数（与论文版本无关） |
| createTime | String | 提交时间（ISO 8601） |

**FeedbackVO 分页结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| records | List\<FeedbackVO\> | 当前页数据 |
| total | long | 总记录数 |
| size | long | 每页条数 |
| current | long | 当前页码 |
| pages | long | 总页数 |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": { "records": [ { "id": "13001", "paperId": "4001", "feedbackType": null, "userNote": "第三页公式与原文不符", "status": "resolved", "statusLabel": "已处理", "processVersion": 2, "createTime": "2026-09-13T10:00:00" } ], "total": 1, "size": 20, "current": 1, "pages": 1 },
  "success": true, "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X GET "http://localhost:9901/api/v1.0/user/feedback?page=1&size=20" -H "Authorization: Bearer <token>"
```
