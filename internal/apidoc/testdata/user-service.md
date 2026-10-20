<!-- golden fixture（源：.doc/需求/接口文档解析/用户服务接口文档.md）：不可就地修改；修改需同步更新 internal/apidoc 解析测试断言 -->
# 用户服务接口文档

## 概述

本文档描述用户服务模块提供的 REST API 接口，包括Token状态检查、用户注册、用户登录、获取当前用户、按用户名查询用户、密码修改、用户信息修改、订阅源管理、收藏夹管理、暂缓池管理、下载记录管理和操作留痕功能。

**基础路径**：`/api/v1.0/user`

---

## 一、Token状态接口

### 1.1 检查Token状态

检查当前Token是否有效。若请求未被认证拦截器拦截，说明Token有效。

- **URL**：`GET /api/v1.0/user/token/status`

#### 请求参数

无请求体参数，需在请求头中携带认证Token。

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | Boolean | Token是否有效 |
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
curl -X GET http://localhost:8080/api/v1.0/user/token/status \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
```

---

## 二、用户注册接口

### 2.1 用户注册

注册新用户账号。

- **URL**：`POST /api/v1.0/user/register`
- **Content-Type**：`application/json`

#### 请求参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| username | String | 是 | 用户名 |
| account | String | 否 | 账号（可选，默认与username相同） |
| password | String | 是 | 密码 |

#### 请求示例

```json
{
  "username": "zhangsan",
  "account": "zhangsan",
  "password": "123456"
}
```

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | UserVO | 注册成功的用户信息 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

**UserVO 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| id | String | 用户ID（JSON序列化为字符串） |
| username | String | 用户名 |
| tokenUsage | String | 已使用Token数量（JSON序列化为字符串） |
| token | String | 认证Token |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "id": "823456789012345678",
    "username": "zhangsan",
    "tokenUsage": "0",
    "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
  },
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X POST http://localhost:8080/api/v1.0/user/register \
  -H "Content-Type: application/json" \
  -d "{\"username\": \"zhangsan\", \"account\": \"zhangsan\", \"password\": \"123456\"}"
```

---

## 三、用户登录接口

### 3.1 用户登录

使用账号和密码进行登录，获取认证Token。

- **URL**：`POST /api/v1.0/user/login`
- **Content-Type**：`application/json`

#### 请求参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| account | String | 是 | 账号 |
| password | String | 是 | 密码 |

#### 请求示例

```json
{
  "account": "zhangsan",
  "password": "123456"
}
```

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | UserVO | 登录成功的用户信息（含Token） |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "id": "823456789012345678",
    "username": "zhangsan",
    "tokenUsage": "1024",
    "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
  },
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X POST http://localhost:8080/api/v1.0/user/login \
  -H "Content-Type: application/json" \
  -d "{\"account\": \"zhangsan\", \"password\": \"123456\"}"
```

---

## 四、获取当前用户接口

### 4.1 获取当前登录用户信息

获取当前已认证用户的详细信息。

- **URL**：`GET /api/v1.0/user/current`

#### 请求参数

无请求体参数，需在请求头中携带认证Token。

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | UserVO | 当前用户信息 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "id": "823456789012345678",
    "username": "zhangsan",
    "tokenUsage": "1024",
    "token": null
  },
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X GET http://localhost:8080/api/v1.0/user/current \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
```

---

## 五、按用户名查询用户接口

### 5.1 根据用户名查询用户信息

通过用户名精确查询指定用户的信息。

- **URL**：`GET /api/v1.0/user`

#### 请求参数

| 参数名 | 类型 | 必填 | 位置 | 说明 |
|--------|------|------|------|------|
| username | String | 是 | Query | 要查询的用户名 |

#### 请求示例

```
GET /api/v1.0/user?username=zhangsan
```

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "id": "823456789012345678",
    "username": "zhangsan",
    "tokenUsage": "1024",
    "token": null
  },
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X GET "http://localhost:8080/api/v1.0/user?username=zhangsan" \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
```

---

## 六、密码修改接口

### 6.1 修改密码

已登录用户通过旧密码验证后修改新密码。

- **URL**：`PUT /api/v1.0/user/password`
- **Content-Type**：`application/json`
- **认证**：需要登录

#### 请求参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| oldPassword | String | 是 | 当前密码 |
| newPassword | String | 是 | 新密码 |

#### 请求示例

```json
{
  "oldPassword": "123456",
  "newPassword": "newPass789"
}
```

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | null | 无返回数据 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": null,
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X PUT http://localhost:8080/api/v1.0/user/password \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..." \
  -d "{\"oldPassword\": \"123456\", \"newPassword\": \"newPass789\"}"
```

---

## 七、用户信息修改接口

### 7.1 修改用户信息

已登录用户修改个人信息（目前仅支持修改用户名）。

- **URL**：`PUT /api/v1.0/user/profile`
- **Content-Type**：`application/json`
- **认证**：需要登录

#### 请求参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| username | String | 是 | 新用户名 |

#### 请求示例

```json
{
  "username": "zhangsan_new"
}
```

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | UserVO | 修改后的用户信息 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "id": "823456789012345678",
    "username": "zhangsan_new",
    "tokenUsage": "1024",
    "token": null
  },
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X PUT http://localhost:8080/api/v1.0/user/profile \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..." \
  -d "{\"username\": \"zhangsan_new\"}"
```

---

## 八、订阅源管理接口

### 8.1 获取当前用户订阅列表

获取当前用户配置的所有 Arxiv 订阅源。

- **URL**：`GET /api/v1.0/user/subscription`
- **认证**：需要登录

#### 请求参数

无。

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | List\<SubscriptionVO\> | 订阅列表 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

**SubscriptionVO 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| id | String | 订阅ID |
| userId | String | 用户ID |
| topCategory | String | Arxiv 顶级分类（如 cs、stat） |
| subCategories | String | 子分类，多个用逗号分隔（如 cs.AI,cs.LG） |
| keywords | String | 关键词，多个用逗号分隔（如 transformer,LLM） |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": [
    {
      "id": "1001",
      "userId": "823456789012345678",
      "topCategory": "cs",
      "subCategories": "cs.AI,cs.LG",
      "keywords": "transformer,attention"
    }
  ],
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X GET http://localhost:8080/api/v1.0/user/subscription \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
```

---

### 8.2 新增订阅

为当前用户新增一条 Arxiv 订阅。

- **URL**：`POST /api/v1.0/user/subscription`
- **Content-Type**：`application/json`
- **认证**：需要登录

#### 请求参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| topCategory | String | 否 | Arxiv 顶级分类 |
| subCategories | String | 否 | 子分类，多个用逗号分隔 |
| keywords | String | 否 | 关键词，多个用逗号分隔 |

#### 请求示例

```json
{
  "topCategory": "cs",
  "subCategories": "cs.AI,cs.CL",
  "keywords": "large language model,reasoning"
}
```

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "id": "1002",
    "userId": "823456789012345678",
    "topCategory": "cs",
    "subCategories": "cs.AI,cs.CL",
    "keywords": "large language model,reasoning"
  },
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X POST http://localhost:8080/api/v1.0/user/subscription \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..." \
  -d "{\"topCategory\": \"cs\", \"subCategories\": \"cs.AI,cs.CL\", \"keywords\": \"large language model,reasoning\"}"
```

---

### 8.3 修改订阅

修改当前用户的指定订阅。

- **URL**：`PUT /api/v1.0/user/subscription/{id}`
- **Content-Type**：`application/json`
- **认证**：需要登录

#### 路径参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| id | Long | 是 | 订阅ID |

#### 请求参数

同新增订阅。

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "id": "1002",
    "userId": "823456789012345678",
    "topCategory": "cs",
    "subCategories": "cs.AI,cs.CL,cs.LG",
    "keywords": "large language model,reasoning,RLHF"
  },
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X PUT http://localhost:8080/api/v1.0/user/subscription/1002 \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..." \
  -d "{\"topCategory\": \"cs\", \"subCategories\": \"cs.AI,cs.CL,cs.LG\", \"keywords\": \"large language model,reasoning,RLHF\"}"
```

---

### 8.4 删除订阅

删除当前用户的指定订阅。

- **URL**：`DELETE /api/v1.0/user/subscription/{id}`
- **认证**：需要登录

#### 路径参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| id | Long | 是 | 订阅ID |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": null,
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X DELETE http://localhost:8080/api/v1.0/user/subscription/1002 \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
```

---

## 九、收藏夹接口

### 9.1 获取收藏列表（分页）

获取当前用户收藏的论文列表，按收藏时间倒序排列。

- **URL**：`GET /api/v1.0/user/favorite`
- **认证**：需要登录

#### 请求参数

| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |
|--------|------|------|------|--------|------|
| page | int | 否 | Query | 1 | 页码 |
| size | int | 否 | Query | 20 | 每页条数 |

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
| id | String | 收藏记录ID |
| paperId | String | 论文ID |
| briefSnapshot | String | 简报快照（JSON字符串） |
| createTime | String | 收藏时间（ISO 8601） |

**IPage 分页结构**：

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
    "records": [
      {
        "id": "2001",
        "paperId": "3001",
        "briefSnapshot": "{\"title_zh\":\"注意力机制综述\",\"one_sentence_summary\":\"...\"}",
        "createTime": "2026-05-31T10:30:00"
      }
    ],
    "total": 15,
    "size": 20,
    "current": 1,
    "pages": 1
  },
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X GET "http://localhost:8080/api/v1.0/user/favorite?page=1&size=20" \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
```

---

### 9.2 添加收藏

收藏一篇论文，可附带简报快照。

- **URL**：`POST /api/v1.0/user/favorite`
- **Content-Type**：`application/json`
- **认证**：需要登录

#### 请求参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| paperId | Long | 是 | 论文ID |
| briefSnapshot | String | 否 | 简报快照（JSON字符串，提供时须为合法 JSON，非法时返回 400） |

#### 请求示例

```json
{
  "paperId": 3001,
  "briefSnapshot": "{\"title_zh\":\"注意力机制综述\",\"one_sentence_summary\":\"本文全面回顾了注意力机制的发展\"}"
}
```

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "id": "2002",
    "paperId": "3001",
    "briefSnapshot": "{\"title_zh\":\"注意力机制综述\",\"one_sentence_summary\":\"本文全面回顾了注意力机制的发展\"}",
    "createTime": "2026-05-31T11:00:00"
  },
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X POST http://localhost:8080/api/v1.0/user/favorite \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..." \
  -d "{\"paperId\": 3001, \"briefSnapshot\": \"{\\\"title_zh\\\":\\\"注意力机制综述\\\"}\"}"
```

---

### 9.3 取消收藏

取消对指定论文的收藏。

- **URL**：`DELETE /api/v1.0/user/favorite/{paperId}`
- **认证**：需要登录

#### 路径参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| paperId | Long | 是 | 论文ID |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": null,
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X DELETE http://localhost:8080/api/v1.0/user/favorite/3001 \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
```

---

## 十、暂缓池接口

### 10.1 获取暂缓池列表（分页）

获取当前用户暂缓池中的论文列表，按加入时间倒序排列。

- **URL**：`GET /api/v1.0/user/deferred`
- **认证**：需要登录

#### 请求参数

| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |
|--------|------|------|------|--------|------|
| page | int | 否 | Query | 1 | 页码 |
| size | int | 否 | Query | 20 | 每页条数 |

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | IPage\<DeferredVO\> | 分页结果 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

**DeferredVO 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| id | String | 记录ID |
| paperId | String | 论文ID |
| addedDate | String | 加入时间（ISO 8601） |
| locked | Boolean | 是否锁定（免费用户超过7天的论文为true，V1.0预留字段） |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "records": [
      {
        "id": "3001",
        "paperId": "4001",
        "addedDate": "2026-05-30T15:00:00",
        "locked": false
      },
      {
        "id": "3002",
        "paperId": "4002",
        "addedDate": "2026-05-20T10:00:00",
        "locked": true
      }
    ],
    "total": 8,
    "size": 20,
    "current": 1,
    "pages": 1
  },
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X GET "http://localhost:8080/api/v1.0/user/deferred?page=1&size=20" \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
```

---

### 10.2 加入暂缓池

将一篇论文加入暂缓池。

- **URL**：`POST /api/v1.0/user/deferred`
- **Content-Type**：`application/json`
- **认证**：需要登录

#### 请求参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| paperId | Long | 是 | 论文ID |

#### 请求示例

```json
{
  "paperId": 4001
}
```

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "id": "3003",
    "paperId": "4001",
    "addedDate": "2026-05-31T12:00:00",
    "locked": false
  },
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X POST http://localhost:8080/api/v1.0/user/deferred \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..." \
  -d "{\"paperId\": 4001}"
```

---

### 10.3 移出暂缓池

将指定论文从暂缓池移除。

- **URL**：`DELETE /api/v1.0/user/deferred/{paperId}`
- **认证**：需要登录

#### 路径参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| paperId | Long | 是 | 论文ID |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": null,
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X DELETE http://localhost:8080/api/v1.0/user/deferred/4001 \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
```

---

## 十一、下载记录接口

### 11.1 获取下载记录列表（分页）

获取当前用户的论文下载记录，按最近下载时间倒序排列。

- **URL**：`GET /api/v1.0/user/download`
- **认证**：需要登录

#### 请求参数

| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |
|--------|------|------|------|--------|------|
| page | int | 否 | Query | 1 | 页码（小于1时归一化为1） |
| size | int | 否 | Query | 20 | 每页条数（夹到 1~100） |

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | IPage\<UserDownloadVO\> | 分页结果 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

**UserDownloadVO 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| paperId | String | 论文ID（JSON序列化为字符串） |
| title | String | 论文标题（实时取自 t_paper） |
| pdfUrl | String | PDF 下载链接（实时取自 t_paper.pdf_url，可能为 null） |
| downloadTime | String | 最近一次下载时间（ISO 8601） |

**IPage 分页结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| records | List\<UserDownloadVO\> | 当前页数据 |
| total | long | 总记录数 |
| size | long | 每页条数 |
| current | long | 当前页码 |
| pages | long | 总页数 |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "records": [
      {
        "paperId": "4001",
        "title": "Attention Is All You Need",
        "pdfUrl": "https://arxiv.org/pdf/1706.03762",
        "downloadTime": "2026-08-31T21:40:00"
      },
      {
        "paperId": "4002",
        "title": "BERT: Pre-training of Deep Bidirectional Transformers",
        "pdfUrl": null,
        "downloadTime": "2026-08-30T09:15:00"
      }
    ],
    "total": 6,
    "size": 20,
    "current": 1,
    "pages": 1
  },
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X GET "http://localhost:8080/api/v1.0/user/download?page=1&size=20" \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
```

---

### 11.2 记录论文下载

记录当前用户下载了指定论文。该接口幂等：重复下载同一论文只刷新下载时间，不会产生重复记录，也不报错。用于跨端同步下载历史。

- **URL**：`POST /api/v1.0/user/download/{paperId}`
- **认证**：需要登录

#### 路径参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| paperId | Long | 是 | 论文ID |

#### 请求参数

无请求体。

#### 返回参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| message | String | 响应消息 |
| data | UserDownloadVO | 记录后的下载信息 |
| success | boolean | 是否成功 |
| code | int | 响应码（200=成功） |

#### 返回示例

```json
{
  "message": "操作成功",
  "data": {
    "paperId": "4001",
    "title": "Attention Is All You Need",
    "pdfUrl": "https://arxiv.org/pdf/1706.03762",
    "downloadTime": "2026-08-31T21:40:00"
  },
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X POST http://localhost:8080/api/v1.0/user/download/4001 \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
```

---

## 十二、操作留痕接口

### 12.1 记录用户操作

记录用户的关键操作行为，用于后续分析。V1.0 仅提供写入接口，不提供查询。

- **URL**：`POST /api/v1.0/user/operation-log`
- **Content-Type**：`application/json`
- **认证**：需要登录

#### 请求参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| operationType | String | 是 | 操作类型，可选值：`view`、`favorite`、`defer`、`generate_brief`、`swipe` |
| paperId | Long | 否 | 关联论文ID |
| deviceType | String | 否 | 设备类型（如 `ios`、`android`、`pc`） |

#### 请求示例

```json
{
  "operationType": "favorite",
  "paperId": 3001,
  "deviceType": "ios"
}
```

#### 返回示例

```json
{
  "message": "操作成功",
  "data": null,
  "success": true,
  "code": 200
}
```

#### 测试 curl 命令

```bash
curl -X POST http://localhost:8080/api/v1.0/user/operation-log \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..." \
  -d "{\"operationType\": \"favorite\", \"paperId\": 3001, \"deviceType\": \"ios\"}"
```

---

## 十三、错误响应

当接口调用失败时，返回错误响应。

#### 错误响应示例

```json
{
  "message": "用户名或密码错误",
  "data": null,
  "success": false,
  "code": 400
}
```

---

## 十四、接口汇总

| 接口 | 方法 | URL | 说明 |
|------|------|-----|------|
| Token状态检查 | GET | /api/v1.0/user/token/status | 检查Token是否有效 |
| 用户注册 | POST | /api/v1.0/user/register | 注册新用户 |
| 用户登录 | POST | /api/v1.0/user/login | 账号密码登录，获取Token |
| 获取当前用户 | GET | /api/v1.0/user/current | 获取当前已登录用户信息 |
| 按用户名查询 | GET | /api/v1.0/user | 根据用户名精确查询用户 |
| 修改密码 | PUT | /api/v1.0/user/password | 旧密码验证后修改密码 |
| 修改用户信息 | PUT | /api/v1.0/user/profile | 修改用户名等个人信息 |
| 获取订阅列表 | GET | /api/v1.0/user/subscription | 获取当前用户所有订阅 |
| 新增订阅 | POST | /api/v1.0/user/subscription | 新增Arxiv订阅 |
| 修改订阅 | PUT | /api/v1.0/user/subscription/{id} | 修改指定订阅 |
| 删除订阅 | DELETE | /api/v1.0/user/subscription/{id} | 删除指定订阅 |
| 获取收藏列表 | GET | /api/v1.0/user/favorite | 分页查询收藏论文 |
| 添加收藏 | POST | /api/v1.0/user/favorite | 收藏论文（含简报快照） |
| 取消收藏 | DELETE | /api/v1.0/user/favorite/{paperId} | 取消收藏 |
| 获取暂缓池 | GET | /api/v1.0/user/deferred | 分页查询暂缓论文 |
| 加入暂缓池 | POST | /api/v1.0/user/deferred | 将论文加入暂缓池 |
| 移出暂缓池 | DELETE | /api/v1.0/user/deferred/{paperId} | 将论文移出暂缓池 |
| 获取下载记录 | GET | /api/v1.0/user/download | 分页查询论文下载记录 |
| 记录论文下载 | POST | /api/v1.0/user/download/{paperId} | 记录下载（幂等，跨端同步） |
| 记录操作 | POST | /api/v1.0/user/operation-log | 记录用户关键操作 |
