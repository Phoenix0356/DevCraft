// 接口字段级 diff 的 rule 中文映射表（apidoc.DiffItem.rule → 界面展示文案）。
//
// 词表来源：rule 字段取 oasdiff 的 check id 词表（10-18-api-docs spec-draft §7 的决定：
// 「UI 统一 tooltip、将来接 OpenAPI diff 零翻译成本」）。实际产出点在 Go 侧，本表逐一对应：
//   · internal/apidoc/diff.go —— DiffEndpoints（meta 组）、diffFieldGroup（added/removed/
//   renamed 配对）、diffUpdated（type/必填/默认值）、diffSchemas（结构增删）与
//   RuleStructAmbiguous 常量（:654）的全部产出分支；
//   · internal/appsvc/apidoc.go —— 删除接口的兜底明细复用同一词表的三条 removed。
//
// 为什么界面上仍要保留原始 id（title tooltip 可见）：
//   1) 这套 id 是对接 oasdiff / OpenAPI diff 的锚点，报障与工具比对输出都按 id 对齐；
//   2) diff 载荷以 JSON 落库，排查解析问题时按 id 反查 diff.go 产出分支最快；
//   3) 中文只是展示层的一张皮，词表本体（Go 侧）不动 —— 改 id 会破坏与 oasdiff 的对应。
//
// 后端新增 rule 时改这里：在 APIDOC_RULES 补一行 {label, desc} 即可。忘了补也不会出
// 空白/undefined —— ruleLabel/ruleDesc 对未收录 id 原样返回传入值（兜底契约，见 AC2）。
//
// 措辞约定（务必保持）：
//   · struct-ambiguous 是「降级项」：只说明「命名结构有歧义、无法判定破坏性」，
//     绝不能写成有破坏 —— 它存在的意义恰恰是程序认不出时不假装裁判；
//   · 必填方向必须一眼可辨：became-required =「收紧」、no-longer-required =「放宽」，
//     括号里把方向词钉死，避免两个长词表 id 读串。

/** rule id → {label 中文短语（列内展示）, desc 一句话说明（tooltip 用）} */
const APIDOC_RULES = {
  // —— meta：端点 / Content-Type / 认证 ——
  'path-changed': {
    label: '路径变更',
    desc: '接口请求路径发生变化（新旧端点落在一条 moved 上），调用地址需同步修改。'
  },
  'method-changed': {
    label: '请求方法变更',
    desc: '同一路径的 HTTP 方法发生变化（如 GET→POST），按旧方法调用会失败。'
  },
  'request-content-type-changed': {
    label: 'Content-Type 变更',
    desc: '请求体编码在 json/multipart 等之间切换；两边都非空时为破坏，空→有值只是文档补全。'
  },
  'request-authentication-changed': {
    label: '认证方式变更',
    desc: '鉴权要求变化（非「改为内部」方向）；一侧为空属文档补全，其余方向需人工确认。'
  },
  'request-authentication-internalized': {
    label: '改为内部鉴权',
    desc: '外部可调用改为内部 Token：前端从此调不了该接口，自动判破坏。'
  },

  // —— 参数跨位置迁移 ——
  'request-parameter-position-changed': {
    label: '参数位置迁移',
    desc: '同名参数在 Query/Body/Header 之间挪位（如 Query→Body），旧位置传参失效，自动判破坏。'
  },

  // —— renamed 配对（同组内一删一增且类型/必填/默认值全同）——
  'request-property-removed+added': {
    label: '请求字段改名',
    desc: '请求侧同型的一删一增配对为字段改名；改名按破坏处理，需人工确认。'
  },
  'response-property-removed+added': {
    label: '响应字段改名',
    desc: '响应侧同型的一删一增配对为字段改名；需人工确认老字段是否还有消费方。'
  },

  // —— added ——
  'response-property-added': {
    label: '响应新增字段',
    desc: '响应体新增字段，老客户端忽略未知字段即可，默认非破坏。'
  },
  'required-request-parameter-added': {
    label: '新增必填请求参数',
    desc: '请求侧新增且必填：老客户端不带该参数会被直接拒绝（400），自动判破坏。'
  },
  'request-parameter-added': {
    label: '新增可选请求参数',
    desc: '请求侧新增非必填参数，老客户端无需改动即可继续调用。'
  },
  'path-parameter-added': {
    label: '新增路径参数',
    desc: '路径新增占位参数，URL 形状改变，所有调用方必须适配，自动判破坏。'
  },

  // —— removed ——
  'path-parameter-removed': {
    label: '路径参数被删',
    desc: '路径占位参数被删除，新旧 URL 不再匹配，自动判破坏。'
  },
  'request-parameter-removed': {
    label: '请求参数被删',
    desc: '请求参数删除：老客户端照发通常被后端忽略，是否真破坏需人工确认（确认闸档）。'
  },
  'response-property-removed': {
    label: '响应字段被删',
    desc: '响应体字段删除，依赖它的前端逻辑会取到 undefined，自动判破坏。'
  },

  // —— 类型变更（按归一 type_key 比较）——
  'response-property-type-changed': {
    label: '响应字段类型变更',
    desc: '响应字段类型（归一后）变化，既有解析与展示路径可能失效，自动判破坏。'
  },
  'request-property-type-changed': {
    label: '请求字段类型变更',
    desc: '请求字段类型（归一后）变化，旧格式传参可能被拒，自动判破坏。'
  },
  'path-parameter-type-changed': {
    label: '路径参数类型变更',
    desc: '路径占位参数类型变化，旧取值格式可能不再合法，自动判破坏。'
  },

  // —— 必填态变化（方向词必须显式）——
  'request-property-became-required': {
    label: '改为必填（收紧）',
    desc: '请求字段由非必填/必填未知改为必填，不带该参数的旧请求会失败。'
  },
  'request-property-no-longer-required': {
    label: '改为非必填（放宽）',
    desc: '请求字段由必填改为非必填/未知，旧请求照常通过，一般非破坏。'
  },
  'request-property-required-changed': {
    label: '必填态变更（方向不明）',
    desc: '必填三态的其余方向变化（如未知↔非必填），无法自动判定松紧，需人工看一眼。'
  },

  // —— 默认值变化 ——
  'request-parameter-default-removed': {
    label: '默认值被清空',
    desc: '参数默认值从有到无，事实上的收紧：不传参时的行为改变，自动判破坏。'
  },
  'request-parameter-default-changed': {
    label: '默认值变更',
    desc: '参数默认值改写，未显式传参的调用方行为可能随之变化。'
  },

  // —— 命名结构（VO）增删 ——
  'response-schema-added': {
    label: '新增响应结构',
    desc: '响应侧新出现命名结构（VO），一般是新字段的挂载点。'
  },
  'response-schema-removed': {
    label: '响应结构被删',
    desc: '命名结构（VO）整体删除，引用它的全部响应字段一并消失，自动判破坏。'
  },

  // —— 降级项：不是「有破坏」，是「没法判」 ——
  'struct-ambiguous': {
    label: '结构歧义（无法判定）',
    desc: '同名结构存在多套定义，程序无法可靠比对，本条已降级为需人工确认；不代表接口有破坏。'
  }
}

/**
 * rule → 中文短语（列内展示用）。
 * 兜底契约：未收录的 id 原样返回传入值，绝不返回空串/undefined；
 * 传入 null/undefined（该条 diff 本就没有 rule）返回空串，由调用方决定不渲染。
 * @param {string} rule apidoc.DiffItem.rule
 * @returns {string} 收录项返回中文短语；未收录返回原 id
 */
export function ruleLabel(rule) {
  const id = rule == null ? '' : String(rule)
  const hit = APIDOC_RULES[id]
  return hit ? hit.label : id
}

/**
 * rule → 一句话详细说明（tooltip 用）。
 * 兜底契约与 ruleLabel 一致：未收录原样返回传入 id（不返回空串/undefined）。
 * @param {string} rule apidoc.DiffItem.rule
 * @returns {string} 收录项返回说明句；未收录返回原 id
 */
export function ruleDesc(rule) {
  const id = rule == null ? '' : String(rule)
  const hit = APIDOC_RULES[id]
  return hit ? hit.desc : id
}
