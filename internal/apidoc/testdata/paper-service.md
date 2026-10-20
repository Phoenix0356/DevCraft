<!-- golden fixture（源：.doc/需求/接口文档解析/论文服务接口文档.md）：不可就地修改；修改需同步更新 internal/apidoc 解析测试断言 -->
# 论文服务接口文档

## 概述

本文档描述论文服务模块提供的 REST API 接口，包括论文检索、论文解析、论文摘要生成、论文推荐、AI简报存储、arXiv 分类查询和RPC论文写入功能。

**基础路径**：`/api/v1.0/paper`

---

## 一、论文检索接口

### 1.1 用户论文检索

在用户拥有的论文范围内进行混合检索。

- **URL**：`POST /api/v1.0/paper/retrieval`
- **Content-Type**：`application/json`

#### 请求参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| text | String | 是 | 用户查询文本（自然语言） |
| paperIdList | List\<Long\> | 否 | 用户拥有的论文ID列表，限定检索范围（可选） |

#### 请求示例

```json
{
  "text": "深度学习在图像分类中的应用",
  "paperIdList": [1001, 1002, 1003, 1005, 1008]
}
```

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | List\<PaperRetrievalVO\> | 检索结果列表（按相关性排序） |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

**PaperRetrievalVO 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| paperId | String | 论文ID（JSON序列化为字符串） |
| title | String | 论文标题 |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": [
    { "paperId": "1003", "title": "Attention Is All You Need" },
    { "paperId": "1001", "title": "Deep Residual Learning" },
    { "paperId": "1008", "title": "BERT: Pre-training of Deep Bidirectional Transformers" }
  ],
  "success": true,
  "code": 200
}
```

---

### 1.2 全量论文检索

在系统全部论文中进行混合检索。

- **URL**：`POST /api/v1.0/paper/retrieval/whole`
- **Content-Type**：`application/json`

#### 请求参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| text | String | 是 | 用户查询文本（自然语言） |
| paperIdList | List\<Long\> | 否 | 可忽略，全量检索不使用此参数 |

#### 请求示例

```json
{
  "text": "Transformer模型在自然语言处理中的应用"
}
```

#### 返回示例

```json
{
  "message": "操作成功",
  "data": [
    { "paperId": "2056", "title": "Generative Adversarial Networks" },
    { "paperId": "1892", "title": "Transformer-XL" },
    { "paperId": "3104", "title": "GPT-3: Language Models are Few-Shot Learners" },
    { "paperId": "1567", "title": "ELMo: Deep Contextualized Word Representations" }
  ],
  "success": true,
  "code": 200
}
```

---

## 二、论文解析接口

### 2.1 解析论文PDF

上传PDF文件，解析论文元数据（标题、作者、关键词、摘要等），不触发AI摘要生成。

- **URL**：`POST /api/v1.0/paper/parse`
- **Content-Type**：`multipart/form-data`

#### 请求参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| pdfFile | MultipartFile | 是 | 论文PDF文件 |

#### 请求示例

```
POST /api/v1.0/paper/parse
Content-Type: multipart/form-data; boundary=----WebKitFormBoundary

------WebKitFormBoundary
Content-Disposition: form-data; name="pdfFile"; filename="attention_is_all_you_need.pdf"
Content-Type: application/pdf

[PDF文件二进制内容]
------WebKitFormBoundary--
```

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | PaperVO | 解析出的论文信息 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "paperId": "4001",
    "title": "Attention Is All You Need",
    "authorList": ["Ashish Vaswani", "Noam Shazeer"],
    "keywordList": ["transformer", "attention mechanism"],
    "abstractText": "The dominant sequence transduction models...",
    "doi": "10.48550/arXiv.1706.03762",
    "publishedYear": "2017"
  },
  "success": true,
  "code": 200
}
```

---

## 三、论文摘要接口

### 3.1 生成论文摘要

上传PDF文件，自动解析并生成论文结构化摘要。若传入 paperId，则对已有论文重新生成摘要。

- **URL**：`POST /api/v1.0/paper/summary`
- **Content-Type**：`multipart/form-data`

#### 请求参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| pdfFile | MultipartFile | 是 | 论文PDF文件 |
| paperId | Long | 否 | 已有论文ID，传入时对该论文重新生成摘要 |

#### 请求示例

```
POST /api/v1.0/paper/summary
Content-Type: multipart/form-data; boundary=----WebKitFormBoundary

------WebKitFormBoundary
Content-Disposition: form-data; name="pdfFile"; filename="attention_is_all_you_need.pdf"
Content-Type: application/pdf

[PDF文件二进制内容]
------WebKitFormBoundary--
```

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | PaperVO | 生成的论文信息 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "paperId": "4001",
    "title": "Attention Is All You Need",
    "authorList": ["Ashish Vaswani", "Noam Shazeer"],
    "keywordList": ["transformer", "attention mechanism"],
    "abstractText": "The dominant sequence transduction models...",
    "doi": "10.48550/arXiv.1706.03762",
    "publishedYear": "2017"
  },
  "success": true,
  "code": 200
}
```

---

## 四、论文推荐接口

### 4.1 获取推荐论文简报

获取系统推荐的论文简报列表。

- **URL**：`GET /api/v1.0/paper/recommend/briefs`
- **认证**：需要登录

#### 请求参数

| 参数名 | 类型 | 必填 | 位置 | 说明 |
|--------|------|------|------|------|
| n | int | 是 | Query | 请求的推荐简报数量 |

#### 请求示例

```
GET /api/v1.0/paper/recommend/briefs?n=2
```

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | List\<PaperBriefVO\> | 推荐的论文简报列表 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

**PaperBriefVO 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| paperId | String | 平台内部论文 ID |
| brief | Brief | 论文简报详情 |
| tags | Tags | 标签信息 |
| postCard | PostCard | 卡片文案 |
| pdfUrl | String | PDF 下载链接（来自 t_paper.pdf_url，可能为 null） |

**Brief 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| title_zh | String | 中文标题 |
| one_sentence_summary | String | 一句话总结 |
| problem | String | 论文要解决的问题 |
| method | String | 方法概述 |
| contributions | List\<String\> | 核心贡献 |
| code_status | String | 代码状态：`open_source` / `project_available` / `not_provided` / `unknown` |
| code_url | String | 代码链接（可为 null） |
| project_url | String | 项目链接（可为 null） |
| limitations_or_unknowns | String | 局限或无法确认的信息 |

**Tags 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| suggested_tags | List\<String\> | 通用展示标签 |
| tag_source | String | 标签来源，固定为 `ai_generated` |

**PostCard 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| template_id | String | 卡片模板 ID，V1 固定为 `paper_card_v1` |
| cover_title | String | 卡片封面标题 |
| subtitle | String | 卡片副标题 |
| highlight_points | List\<String\> | 卡片高亮信息 |
| badges | List\<String\> | 卡片徽标文案 |
| display_tags | List\<String\> | 卡片展示标签 |
| footer_text | String | 底部文案 |
| image_source | ImageSource | 图片来源 |

**ImageSource 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| type | String | 图片类型：`none` / `thumbnail_url` / `project_image_url` |
| url | String | 图片 URL（type=none 时为 null） |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": [
    {
      "paperId": "3001",
      "brief": {
        "title_zh": "注意力机制综述",
        "one_sentence_summary": "本文全面回顾了注意力机制从加性注意到自注意力的发展历程及其在NLP和CV领域的应用。",
        "problem": "注意力机制种类繁多，缺乏统一的分类框架来理解不同注意力变体之间的关系及其适用场景。",
        "method": "提出了一种基于计算复杂度和信息流的分类体系，将注意力机制分为加性注意、乘性注意、自注意和跨注意四大类，并分析了各自的计算复杂度与表达能力。",
        "contributions": [
          "提出了注意力机制的统一分类框架",
          "系统比较了不同注意力变体的计算复杂度",
          "给出了各领域注意力选择的实践指南"
        ],
        "code_status": "open_source",
        "code_url": "https://github.com/example/attention-survey",
        "project_url": null,
        "limitations_or_unknowns": "未涵盖2024年后的最新注意力变体（如线性注意力、稀疏注意力的最新进展），部分实验仅在英文数据集上验证。"
      },
      "tags": {
        "suggested_tags": ["深度学习", "注意力机制", "综述", "NLP"],
        "tag_source": "ai_generated"
      },
      "postCard": {
        "template_id": "paper_card_v1",
        "cover_title": "注意力机制全景综述",
        "subtitle": "从加性注意到自注意的统一分类框架",
        "highlight_points": [
          "提出四大类注意力分类体系",
          "覆盖NLP与CV两大领域",
          "附实践选择指南"
        ],
        "badges": ["综述论文", "开源代码"],
        "display_tags": ["深度学习", "注意力机制"],
        "footer_text": "被引用 1,200+ 次 · 发表于 2023",
        "image_source": {
          "type": "thumbnail_url",
          "url": "https://arxiv.org/pdf/2301.00001/page1.png"
        }
      },
      "pdfUrl": "https://arxiv.org/pdf/2301.00001"
    },
    {
      "paperId": "3002",
      "brief": {
        "title_zh": "大规模语言模型的高效微调方法",
        "one_sentence_summary": "提出了一种参数高效的微调策略，仅更新不到1%的模型参数即可在大语言模型上达到全量微调的性能。",
        "problem": "大语言模型的全量微调成本极高，现有参数高效微调方法（如LoRA、Adapter）在超大规模模型上仍存在性能差距。",
        "method": "结合低秩分解与动态适配器选择，提出DoRA（Dynamic Orthogonal Rank Adaptation）方法，在保持参数高效的同时显著提升微调性能。",
        "contributions": [
          "提出DoRA微调方法，参数量仅为全量微调的0.8%",
          "在多个基准任务上超越LoRA和Adapter v2",
          "提供了与全量微调的理论等价性分析"
        ],
        "code_status": "open_source",
        "code_url": "https://github.com/example/dora",
        "project_url": "https://dora-llm.example.com",
        "limitations_or_unknowns": "仅在LLaMA和Mistral模型架构上验证，对其他架构（如Mamba、RWKV）的泛化性未知。"
      },
      "tags": {
        "suggested_tags": ["大语言模型", "高效微调", "LoRA", "参数效率"],
        "tag_source": "ai_generated"
      },
      "postCard": {
        "template_id": "paper_card_v1",
        "cover_title": "DoRA: 0.8% 参数达到全量微调性能",
        "subtitle": "动态正交秩适配——大模型微调的新范式",
        "highlight_points": [
          "仅需 0.8% 参数",
          "超越 LoRA 和 Adapter v2",
          "理论等价性证明"
        ],
        "badges": ["原创方法", "开源代码", "在线Demo"],
        "display_tags": ["LLM微调", "参数效率"],
        "footer_text": "发表于 NeurIPS 2024 · Spotlight",
        "image_source": {
          "type": "none",
          "url": null
        }
      },
      "pdfUrl": null
    }
  ],
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X GET "http://localhost:8080/api/v1.0/paper/recommend/briefs?n=5" \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
```

---

## 五、AI简报存储接口

### 5.1 上传/覆盖论文简报

上传或覆盖指定论文的 AI 简报。简报由前端调用大模型生成后上传，后端仅校验 `briefJson` 为合法 JSON（非法时返回 400），不校验简报内部结构。同一篇论文的简报允许覆盖（幂等）。

- **URL**：`POST /api/v1.0/paper/brief`
- **Content-Type**：`application/json`
- **认证**：需要登录

#### 请求参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| paperId | Long | 是 | 论文ID |
| briefJson | String | 是 | 简报JSON内容（字符串形式，须为合法 JSON，非法时返回 400） |

#### 请求示例

```json
{
  "paperId": 3001,
  "briefJson": "{\"title_zh\":\"注意力机制综述\",\"one_sentence_summary\":\"本文全面回顾了注意力机制的发展\",\"problem\":\"...\",\"method\":\"...\",\"contributions\":[\"...\"],\"code_status\":\"open_source\"}"
}
```

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | PaperBriefVO | 存储后的简报信息 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

**PaperBriefVO 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| id | String | 简报记录ID |
| paperId | String | 论文ID |
| briefJson | String | 简报JSON内容 |
| sourceType | String | 来源类型：`frontend_generated` / `backend_generated` |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "id": "5001",
    "paperId": "3001",
    "briefJson": "{\"title_zh\":\"注意力机制综述\",\"one_sentence_summary\":\"...\"}",
    "sourceType": "frontend_generated"
  },
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X POST http://localhost:8080/api/v1.0/paper/brief \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..." \
  -d "{\"paperId\": 3001, \"briefJson\": \"{\\\"title_zh\\\":\\\"注意力机制综述\\\"}\"}"
```

---

### 5.2 查询论文简报

查询指定论文已存储的 AI 简报。

- **URL**：`GET /api/v1.0/paper/brief/{paperId}`
- **认证**：需要登录

#### 路径参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| paperId | Long | 是 | 论文ID |

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | PaperBriefVO | 简报信息（不存在时为null） |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "id": "5001",
    "paperId": "3001",
    "briefJson": "{\"title_zh\":\"注意力机制综述\",\"one_sentence_summary\":\"...\"}",
    "sourceType": "frontend_generated"
  },
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X GET http://localhost:8080/api/v1.0/paper/brief/3001 \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
```

---

## 六、arXiv 分类查询接口

### 6.1 获取所有顶层领域

获取 arXiv 全部顶层学科领域列表（共 19 个）。

- **URL**：`GET /api/v1.0/paper/arxiv/categories`
- **认证**：无需登录

#### 请求参数

无。

#### 请求示例

```
GET /api/v1.0/paper/arxiv/categories
```

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | List\<ArxivCategoryVO\> | 顶层领域列表 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

**ArxivCategoryVO 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| primaryId | String | 顶层领域标识（如 `cs`、`math`、`physics`） |
| displayName | String | 领域显示名称（如 `Computer Science`） |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": [
    { "primaryId": "cs", "displayName": "Computer Science" },
    { "primaryId": "math", "displayName": "Mathematics" },
    { "primaryId": "physics", "displayName": "Physics" },
    { "primaryId": "astro-ph", "displayName": "Astrophysics" },
    { "primaryId": "cond-mat", "displayName": "Condensed Matter" }
  ],
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X GET http://localhost:8080/api/v1.0/paper/arxiv/categories
```

---

### 6.2 获取二级子领域

根据顶层领域标识（primaryId）获取其下所有二级子领域。

- **URL**：`GET /api/v1.0/paper/arxiv/categories/{primaryId}/subcategories`
- **认证**：无需登录

#### 路径参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| primaryId | String | 是 | 顶层领域标识（如 `cs`、`math`、`physics`） |

#### 请求参数

无。

#### 请求示例

```
GET /api/v1.0/paper/arxiv/categories/cs/subcategories
```

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | List\<ArxivSubCategoryVO\> | 二级子领域列表 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

**ArxivSubCategoryVO 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| categoryId | String | 子分类标识（如 `cs.AI`、`cs.LG`） |
| description | String | 子分类描述（如 `Artificial Intelligence`） |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": [
    { "categoryId": "cs.AI", "description": "Artificial Intelligence" },
    { "categoryId": "cs.CL", "description": "Computation and Language" },
    { "categoryId": "cs.CV", "description": "Computer Vision and Pattern Recognition" },
    { "categoryId": "cs.LG", "description": "Machine Learning" },
    { "categoryId": "cs.SE", "description": "Software Engineering" }
  ],
  "success": true,
  "code": 200
}
```

#### 错误响应示例

当 primaryId 无效时：

```json
{
  "message": "无效的 arXiv 领域: invalid-id",
  "data": null,
  "success": false,
  "code": 500
}
```

#### 测试 curl 命令

```bash
curl -X GET http://localhost:8080/api/v1.0/paper/arxiv/categories/cs/subcategories
```

---

## 七、RPC 论文写入接口（内部）

供内部服务（paper-pilot-harvester）通过 Feign 调用的论文写入端点，前端不使用。

**认证方式**：不走用户 JWT。请求必须携带 Header `X-Internal-Token`，其值须与 manager 配置项 `rpc.internal-token` 一致（harvester 侧配置同名键，Feign 客户端 `PaperManagerClient` 出站请求自动附带该 Header）。Token 缺失或不匹配时返回 HTTP 401：

```json
{
  "message": "请重新登录",
  "data": null,
  "success": false,
  "code": 401
}
```

**调用方**：paper-pilot-harvester（manager-rpc 模块 `PaperManagerClient`，每日 arXiv 定时拉取任务按 200 条分块调用批量接口）。

### 7.1 保存 arXiv 论文（单条）

- **URL**：`POST /api/v1.0/paper/rpc/arxiv/save`
- **Content-Type**：`application/json`
- **Header**：`X-Internal-Token`（必需，值与 `rpc.internal-token` 配置一致）

#### 请求参数（ArxivPaperDTO）

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| arxivId | String | 是 | arXiv ID（已归一化，无 URL 前缀与版本后缀，如 `2301.12345`） |
| title | String | 是 | 论文标题 |
| summary | String | 否 | 论文摘要 |
| authors | List\<String\> | 否 | 作者列表 |
| published | String | 否 | 首次提交日期（ISO 8601 格式） |
| updated | String | 否 | 最后更新日期（ISO 8601 格式） |
| doi | String | 否 | DOI 标识符 |
| journalRef | String | 否 | 期刊引用 |
| primaryCategory | String | 否 | 主分类，如 `cs.AI` |
| categories | List\<String\> | 否 | 所有分类列表 |
| pdfUrl | String | 否 | PDF 下载链接 |

#### 请求示例

```json
{
  "arxivId": "2301.12345",
  "title": "Attention Is All You Need",
  "summary": "The dominant sequence transduction models...",
  "authors": ["Ashish Vaswani", "Noam Shazeer"],
  "published": "2023-01-30T18:00:00Z",
  "updated": "2023-02-01T09:30:00Z",
  "doi": "10.48550/arXiv.2301.12345",
  "journalRef": null,
  "primaryCategory": "cs.AI",
  "categories": ["cs.AI", "cs.LG"],
  "pdfUrl": "https://arxiv.org/pdf/2301.12345"
}
```

#### 返回参数（ApiResponse\<Boolean\>）

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | Boolean | 是否保存成功 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": true,
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X POST http://localhost:9901/api/v1.0/paper/rpc/arxiv/save \
  -H "Content-Type: application/json" \
  -H "X-Internal-Token: <rpc.internal-token 配置值>" \
  -d "{\"arxivId\": \"2301.12345\", \"title\": \"Attention Is All You Need\", \"primaryCategory\": \"cs.AI\"}"
```

---

### 7.2 批量保存 arXiv 论文

- **URL**：`POST /api/v1.0/paper/rpc/arxiv/saveBatch`
- **Content-Type**：`application/json`
- **Header**：`X-Internal-Token`（必需，值与 `rpc.internal-token` 配置一致）

#### 请求参数

请求体为 `List<ArxivPaperDTO>`（JSON 数组，元素结构见 7.1）。

#### 请求示例

```json
[
  { "arxivId": "2301.12345", "title": "Paper A", "primaryCategory": "cs.AI" },
  { "arxivId": "2301.12346", "title": "Paper B", "primaryCategory": "cs.LG" }
]
```

#### 返回参数

同 7.1，`ApiResponse<Boolean>`，`data` 表示本批是否全部保存成功。

#### 返回示例

```json
{
  "message": "操作成功",
  "data": true,
  "success": true,
  "code": 200
}
```

---

## 八、PaperVO 结构说明

论文解析和论文摘要接口返回的论文完整信息对象。

| 字段 | 类型 | 说明 |
|------|------|------|
| paperId | String | 论文ID（JSON序列化为字符串） |
| title | String | 论文标题 |
| authorList | List\<String\> | 作者列表 |
| keywordList | List\<String\> | 关键词列表 |
| abstractText | String | 论文摘要 |
| doi | String | DOI标识 |
| publishedYear | String | 发表年份 |
| summary | String | AI生成的论文结构化摘要 |

---

## 九、错误响应

当接口调用失败时，返回错误响应。

#### 错误响应示例

```json
{
  "message": "查询文本不能为空",
  "data": null,
  "success": false,
  "code": 400
}
```

---

## 十、接口汇总

| 接口 | 方法 | URL | 说明 |
|------|------|-----|------|
| 用户论文检索 | POST | /api/v1.0/paper/retrieval | 在指定论文范围内检索 |
| 全量论文检索 | POST | /api/v1.0/paper/retrieval/whole | 在全部论文中检索 |
| 解析论文PDF | POST | /api/v1.0/paper/parse | 上传PDF解析元数据 |
| 生成论文摘要 | POST | /api/v1.0/paper/summary | 上传PDF生成摘要 |
| 获取推荐简报 | GET | /api/v1.0/paper/recommend/briefs | 获取系统推荐论文简报 |
| 上传/覆盖简报 | POST | /api/v1.0/paper/brief | 上传论文AI简报（幂等覆盖） |
| 查询论文简报 | GET | /api/v1.0/paper/brief/{paperId} | 查询指定论文的简报 |
| 获取所有顶层领域 | GET | /api/v1.0/paper/arxiv/categories | 获取 arXiv 全部顶层学科领域 |
| 获取二级子领域 | GET | /api/v1.0/paper/arxiv/categories/{primaryId}/subcategories | 根据顶层领域获取子领域 |
| 保存 arXiv 论文（内部 RPC） | POST | /api/v1.0/paper/rpc/arxiv/save | harvester 单条写入，需 X-Internal-Token |
| 批量保存 arXiv 论文（内部 RPC） | POST | /api/v1.0/paper/rpc/arxiv/saveBatch | harvester 批量写入，需 X-Internal-Token |
