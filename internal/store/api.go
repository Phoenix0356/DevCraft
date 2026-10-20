package store

// 接口文档模块（api_* 九张表）的实体与 CRUD。
// 约定与仓库既有 store 代码一致：
//   - 方法不带 ctx、不打日志（日志只在 appsvc 层）；接收者恒为 s *Store；
//   - 需要回填自增/生成主键的入参用指针（照 SaveDeployFlow / InsertDeployHistory）；
//   - 实体零指针可空字段：可空语义用 -1 / '' 哨兵；json tag 一律 camelCase；
//   - bool 落库手工 0/1；时间戳一律 INTEGER 毫秒；
//   - sql.ErrNoRows 翻译成中文错误，不向上暴露哨兵；
//   - 多语句原子写用 Begin → defer Rollback → Commit（照 ReplaceAgentSkills）；
//     不用 FK/CASCADE（全仓无 PRAGMA foreign_keys），级联手写多条 DELETE；
//   - 「一次导入的多表写」（PRD R4 单事务、AC3 失败零写入）走 Store.WithTx + Tx 方法：
//     写路径核心逻辑面向 apiExecutor（*sql.DB 与 *sql.Tx 的公共子集）编写，单方法自动提交
//     与导入大事务共用同一份 SQL；
//   - 列表 SQL 明确列名、禁止 SELECT *（api_change_events.doc_text 每行几十 KB）。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"DevCraft/internal/apidoc"
)

// ===================== 枚举取值（字符串常量对，不用 Go enum）=====================

// api_fields.owner_kind / api_examples.owner_kind 取值。
const (
	ApiOwnerEndpoint = "endpoint"
	ApiOwnerSchema   = "schema"
)

// api_fields.kind 取值（owner_kind=schema 时恒 body）。
const (
	ApiFieldKindRequest  = "request"
	ApiFieldKindResponse = "response"
	ApiFieldKindPath     = "path"
	ApiFieldKindHeader   = "header"
	ApiFieldKindBody     = "body"
)

// api_entries.status 取值（软删，PRD D17/R7）。
const (
	ApiEntryStatusActive  = "active"
	ApiEntryStatusRemoved = "removed"
)

// api_entries.visibility 取值（内部 RPC 端点入库并默认过滤，R11）。
const (
	ApiVisibilityPublic   = "public"
	ApiVisibilityInternal = "internal"
)

// api_entries.parse_state 取值（「不许猜」的落地位）。
const (
	ApiParseStateOK       = "ok"
	ApiParseStatePartial  = "partial"
	ApiParseStateUnparsed = "unparsed"
)

// api_change_items.change_type 取值。
const (
	ApiChangeTypeAdded    = "added"
	ApiChangeTypeUpdated  = "updated"
	ApiChangeTypeRemoved  = "removed"
	ApiChangeTypeMoved    = "moved"
	ApiChangeTypeRestored = "restored"
)

// api_examples.kind 取值。
const (
	ApiExampleReq       = "req_example"
	ApiExampleRes       = "res_example"
	ApiExampleCURL      = "curl"
	ApiExampleMultipart = "multipart_raw"
	ApiExampleError     = "error_example"
)

// api_change_events.source / kind / scope_mode 取值。
const (
	ApiEventSourceImport = "import"
	ApiEventSourceManual = "manual"

	ApiEventKindFeature  = "feature"
	ApiEventKindFix      = "fix"
	ApiEventKindRefactor = "refactor"
	ApiEventKindDocs     = "docs"

	ApiScopeIncremental = "incremental"
	ApiScopeFull        = "full"
	ApiScopeInit        = "init"
)

// api_change_events.breaking / api_change_items.breaking 三态。
const (
	ApiBreakingNo      = 0 // 无/未判定
	ApiBreakingYes     = 1 // 有破坏性
	ApiBreakingConfirm = 2 // 需人工确认（等同破坏处理，不确认不写）
)

// ===================== 实体 =====================

// ApiProject 项目 = 一个后端服务/代码仓库（PRD D13）。
type ApiProject struct {
	ID        string `json:"id"`
	Name      string `json:"name"` // 服务标识 = meta 里的 project，全局唯一
	Display   string `json:"display"`
	BasePath  string `json:"basePath"` // 仅用于校验与导出，不参与身份键
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// ApiDomain 领域 = 文档一级 ## 章节（PRD D1）。
type ApiDomain struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Name      string `json:"name"` // 归一后的领域名
	Internal  bool   `json:"internal"`
	NotesRaw  string `json:"notesRaw"` // 章级 preamble，下渗给子接口
	Sort      int    `json:"sort"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// ApiEntry 接口条目：只存当前态，不存快照（PRD Goal-1）。
type ApiEntry struct {
	ID             string `json:"id"`
	ProjectID      string `json:"projectId"`
	DomainID       string `json:"domainId"`
	Name           string `json:"name"` // 接口中文名（不参与 diff）
	Method         string `json:"method"`
	PathRaw        string `json:"pathRaw"` // 原样（含 {id}）
	IdentityKey    string `json:"identityKey"`
	BasePathAssert string `json:"basePathAssert"` // 导入时的断言凭证
	ContentType    string `json:"contentType"`
	Auth           string `json:"auth"`
	Visibility     string `json:"visibility"` // public | internal
	SemanticHash   string `json:"semanticHash"`
	DocHash        string `json:"docHash"`
	Description    string `json:"description"`
	ParseState     string `json:"parseState"` // ok | partial | unparsed
	RawMarkdown    string `json:"rawMarkdown"`
	Status         string `json:"status"` // active | removed（软删）
	DeletedAt      int64  `json:"deletedAt"`
	Sort           int    `json:"sort"`
	CreatedAt      int64  `json:"createdAt"`
	UpdatedAt      int64  `json:"updatedAt"`
}

// ApiField 参数/返回/VO 字段：一行一字段。字段级 diff 的身份就在
// UNIQUE(owner_kind, owner_id, kind, name) 这条数据库级唯一索引上。
type ApiField struct {
	ID           int64  `json:"id"`
	OwnerKind    string `json:"ownerKind"` // endpoint | schema
	OwnerID      string `json:"ownerId"`
	Kind         string `json:"kind"` // request|response|path|header（schema 恒 body）
	Name         string `json:"name"`
	TypeRaw      string `json:"typeRaw"`  // 显示保真
	TypeKey      string `json:"typeKey"`  // 比较归一
	Required     int    `json:"required"` // -1 未知（无该列）| 0 否 | 1 是
	Position     string `json:"position"` // Path|Query|Body|Header
	DefaultValue string `json:"defaultValue"`
	Description  string `json:"description"` // 中文说明，不进 semantic_hash
	FactsJSON    string `json:"factsJson"`   // JSON 扩展槽（整读整写，MVP 解析器不填充）
	RefSchemaID  string `json:"refSchemaId"` // = $ref 语义（VO 引用），空 = 标量
	Depth        int    `json:"depth"`
	Sort         int    `json:"sort"`
}

// ApiSchema 命名结构（≈ OpenAPI components/schemas 极简版）。
// 主键 (project_id, name, owner_scope)；同名不同构 → Ambiguous，禁止按名自动展开。
type ApiSchema struct {
	ID          string `json:"id"`
	ProjectID   string `json:"projectId"`
	Name        string `json:"name"`
	OwnerScope  string `json:"ownerScope"` // '' = 文档级；否则 = 宿主接口 id
	RawMarkdown string `json:"rawMarkdown"`
	BodyHash    string `json:"bodyHash"`
	Ambiguous   bool   `json:"ambiguous"`
	CreatedAt   int64  `json:"createdAt"`
	UpdatedAt   int64  `json:"updatedAt"`
}

// ApiExample 示例与 curl：整块原文，永不结构化、不参与 diff（D9）。
type ApiExample struct {
	ID        int64  `json:"id"`
	OwnerKind string `json:"ownerKind"` // endpoint | schema
	OwnerID   string `json:"ownerId"`
	Kind      string `json:"kind"` // req_example|res_example|curl|multipart_raw|error_example
	Language  string `json:"language"`
	Content   string `json:"content"`
	Sort      int    `json:"sort"`
}

// ApiEventStats 事件头统计（列表页免 join 聚合），存 stats JSON 列。
type ApiEventStats struct {
	Added    int `json:"added"`
	Updated  int `json:"updated"`
	Removed  int `json:"removed"`
	Moved    int `json:"moved"`
	Restored int `json:"restored"`
}

// ApiChangeEvent 变更事件主表：一次导入 = 一行（导入即生效，无状态机，D10）。
type ApiChangeEvent struct {
	ID        int64          `json:"id"`
	ProjectID string         `json:"projectId"`
	Seq       int            `json:"seq"` // 项目内自增版本号（D7），0 = 插入时同事务 MAX(seq)+1
	Label     string         `json:"label"`
	Title     string         `json:"title"`
	Reason    string         `json:"reason"` // 为什么改（= meta change_note）
	Kind      string         `json:"kind"`   // feature|fix|refactor|docs
	Breaking  int            `json:"breaking"`
	Source    string         `json:"source"` // import | manual
	DocRef    string         `json:"docRef"`
	ScopeMode string         `json:"scopeMode"` // incremental|full|init
	Stats     ApiEventStats  `json:"stats"`
	Meta      map[string]any `json:"meta"`    // JSON 扩展槽（doc_sha256 等，避免加列迁移）
	DocText   string         `json:"docText"` // D12 输入凭证；仅 GetApiChangeEventDocText 返回
	CreatedAt int64          `json:"createdAt"`
	UpdatedAt int64          `json:"updatedAt"`
}

// ApiChangeItem 事件明细：本次涉及的每个接口。diff 列存字段级载荷
// （apidoc.DiffItem 数组，单一真相源在 internal/apidoc/diff.go）。
type ApiChangeItem struct {
	ID           int64             `json:"id"`
	EventID      int64             `json:"eventId"`
	ProjectID    string            `json:"projectId"`
	EntryID      string            `json:"entryId"`    // 软删后仍保留，供详情页跳转
	DomainName   string            `json:"domainName"` // 冗余快照（领域会被改名/合并）
	EntryName    string            `json:"entryName"`  // 冗余快照（照 deploy_history.flow_name）
	Method       string            `json:"method"`
	Path         string            `json:"path"`
	ChangeType   string            `json:"changeType"` // added|updated|removed|moved|restored
	Breaking     int               `json:"breaking"`
	Visibility   string            `json:"visibility"`
	Diff         []apidoc.DiffItem `json:"diff"`
	DeclaredNote string            `json:"declaredNote"` // = 文档「变更点」原文
	Sort         int               `json:"sort"`
}

// ApiImportBatch 导入批次：删除安全的载体（MVP 就建表、列留全；删除候选逻辑二期，D17）。
type ApiImportBatch struct {
	ID             int64               `json:"id"`
	ProjectID      string              `json:"projectId"`
	ScopeIdentity  string              `json:"scopeIdentity"` // project|base_path|规范化文档标题
	DeclaredMode   string              `json:"declaredMode"`  // incremental|full|init
	DocFingerprint string              `json:"docFingerprint"`
	Keys           []string            `json:"keys"`         // 本次声明的 identity_key 集
	SummaryCheck   apidoc.SummaryCheck `json:"summaryCheck"` // C1/C3 证书结果
	CreatedAt      int64               `json:"createdAt"`
}

// ===================== 事务骨架（一次导入的多表写，PRD R4）=====================

// apiExecutor 抽象 *sql.DB 与 *sql.Tx 的公共子集：api_* 写路径核心逻辑一律面向它编写，
// 于是「单方法自动提交」与「一次导入的多表写单事务」共用同一份 SQL，不会出现第二套。
type apiExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Tx 事务句柄：Store.WithTx 内只能用 Tx 上的方法完成全部读写。
// ⚠️ db.SetMaxOpenConns(1)：持有 Tx 期间绝不能再调 Store 上的其他方法——
// 它们会去抢唯一连接（已被本事务持有）而死锁。事务内可以读到自己未提交的写。
type Tx struct {
	x *sql.Tx
}

// WithTx 在单事务里执行 fn（照 ReplaceAgentSkills 模板：Begin → defer Rollback → Commit）：
// fn 返回错误则整体回滚（数据库零变更，AC3），全部成功才提交。
func (s *Store) WithTx(fn func(tx *Tx) error) error {
	sqlTx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer sqlTx.Rollback() // 已 Commit 后 Rollback 是 no-op（防御性）
	if err := fn(&Tx{x: sqlTx}); err != nil {
		return err
	}
	return sqlTx.Commit()
}

// ===================== 项目 CRUD =====================

// CreateApiProject 新建项目（name 全局唯一；ID 为空时生成 UUID 并回填）。
// 重名预检 + INSERT 在同一事务里（SetMaxOpenConns(1) 下预检即权威）。
func (s *Store) CreateApiProject(p *ApiProject) error {
	return s.WithTx(func(tx *Tx) error { return tx.CreateApiProject(p) })
}

// Tx.CreateApiProject 事务内版本（导入大事务里定位/新建项目）。
func (t *Tx) CreateApiProject(p *ApiProject) error { return createApiProject(t.x, p) }

func createApiProject(x apiExecutor, p *ApiProject) error {
	if dup, err := apiProjectNameTaken(x, p.Name, ""); err != nil {
		return err
	} else if dup {
		return fmt.Errorf("接口项目已存在: %s", p.Name)
	}
	now := time.Now().UnixMilli()
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	_, err := x.Exec(
		`INSERT INTO api_projects (id, name, display, base_path, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Display, p.BasePath, p.CreatedAt, p.UpdatedAt)
	return err
}

func apiProjectNameTaken(x apiExecutor, name, excludeID string) (bool, error) {
	var n int
	err := x.QueryRow(`SELECT COUNT(*) FROM api_projects WHERE name = ? AND id != ?`, name, excludeID).Scan(&n)
	return n > 0, err
}

// GetApiProject 按 id 查项目。
func (s *Store) GetApiProject(id string) (ApiProject, error) {
	var p ApiProject
	err := s.db.QueryRow(`SELECT id, name, display, base_path, created_at, updated_at FROM api_projects WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Display, &p.BasePath, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return p, fmt.Errorf("接口项目不存在（id=%s）", id)
	}
	return p, err
}

// GetApiProjectByName 按服务标识查项目（导入时用 meta.project 定位）。
func (s *Store) GetApiProjectByName(name string) (ApiProject, error) {
	return getApiProjectByName(s.db, name)
}

// Tx.GetApiProjectByName 事务内版本（导入大事务里读自己未提交的写）。
func (t *Tx) GetApiProjectByName(name string) (ApiProject, error) {
	return getApiProjectByName(t.x, name)
}

func getApiProjectByName(x apiExecutor, name string) (ApiProject, error) {
	var p ApiProject
	err := x.QueryRow(`SELECT id, name, display, base_path, created_at, updated_at FROM api_projects WHERE name = ?`, name).
		Scan(&p.ID, &p.Name, &p.Display, &p.BasePath, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return p, fmt.Errorf("接口项目不存在: %s", name)
	}
	return p, err
}

// ListApiProjects 按名称排序返回全部项目。
func (s *Store) ListApiProjects() ([]ApiProject, error) {
	rows, err := s.db.Query(`SELECT id, name, display, base_path, created_at, updated_at FROM api_projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApiProject
	for rows.Next() {
		var p ApiProject
		if err := rows.Scan(&p.ID, &p.Name, &p.Display, &p.BasePath, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateApiProject 更新名称/显示名/base_path（改名撞唯一约束给中文错误）。
func (s *Store) UpdateApiProject(p ApiProject) error {
	if dup, err := apiProjectNameTaken(s.db, p.Name, p.ID); err != nil {
		return err
	} else if dup {
		return fmt.Errorf("接口项目已存在: %s", p.Name)
	}
	_, err := s.db.Exec(`UPDATE api_projects SET name = ?, display = ?, base_path = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.Display, p.BasePath, time.Now().UnixMilli(), p.ID)
	return err
}

// DeleteApiProject 删除项目：无 FK/CASCADE，级联在单事务里手写多条 DELETE。
func (s *Store) DeleteApiProject(id string) error {
	return s.WithTx(func(tx *Tx) error { return deleteApiProject(tx.x, id) })
}

func deleteApiProject(x apiExecutor, id string) error {
	stmts := []struct {
		query string
		arg   any
	}{
		{`DELETE FROM api_fields WHERE owner_kind = 'endpoint' AND owner_id IN (SELECT id FROM api_entries WHERE project_id = ?)`, id},
		{`DELETE FROM api_examples WHERE owner_kind = 'endpoint' AND owner_id IN (SELECT id FROM api_entries WHERE project_id = ?)`, id},
		{`DELETE FROM api_fields WHERE owner_kind = 'schema' AND owner_id IN (SELECT id FROM api_schemas WHERE project_id = ?)`, id},
		{`DELETE FROM api_examples WHERE owner_kind = 'schema' AND owner_id IN (SELECT id FROM api_schemas WHERE project_id = ?)`, id},
		{`DELETE FROM api_schemas WHERE project_id = ?`, id},
		{`DELETE FROM api_entries WHERE project_id = ?`, id},
		{`DELETE FROM api_domains WHERE project_id = ?`, id},
		{`DELETE FROM api_change_items WHERE project_id = ?`, id},
		{`DELETE FROM api_change_events WHERE project_id = ?`, id},
		{`DELETE FROM api_import_batches WHERE project_id = ?`, id},
		{`DELETE FROM api_projects WHERE id = ?`, id},
	}
	for _, st := range stmts {
		if _, err := x.Exec(st.query, st.arg); err != nil {
			return err
		}
	}
	return nil
}

// ===================== 领域 CRUD =====================

// SaveApiDomain 按 (project_id, name) upsert 领域并回填 id（导入流程用：已存在则更新元信息）。
func (s *Store) SaveApiDomain(d *ApiDomain) error { return saveApiDomain(s.db, d) }

// Tx.SaveApiDomain 事务内版本（一次导入的多表写）。
func (t *Tx) SaveApiDomain(d *ApiDomain) error { return saveApiDomain(t.x, d) }

func saveApiDomain(x apiExecutor, d *ApiDomain) error {
	now := time.Now().UnixMilli()
	if d.ID == "" {
		d.ID = uuid.NewString() // 仅在新插入时生效；冲突更新保留原 id（下面 SELECT 回填）
	}
	if d.CreatedAt == 0 {
		d.CreatedAt = now
	}
	d.UpdatedAt = now
	internal := 0
	if d.Internal {
		internal = 1
	}
	_, err := x.Exec(
		`INSERT INTO api_domains (id, project_id, name, internal, notes_raw, sort, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, name) DO UPDATE SET
		   internal = excluded.internal, notes_raw = excluded.notes_raw,
		   sort = excluded.sort, updated_at = excluded.updated_at`,
		d.ID, d.ProjectID, d.Name, internal, d.NotesRaw, d.Sort, d.CreatedAt, d.UpdatedAt)
	if err != nil {
		return err
	}
	return x.QueryRow(`SELECT id, created_at FROM api_domains WHERE project_id = ? AND name = ?`, d.ProjectID, d.Name).
		Scan(&d.ID, &d.CreatedAt)
}

// ListApiDomains 按 sort 返回项目下全部领域。
func (s *Store) ListApiDomains(projectID string) ([]ApiDomain, error) {
	return listApiDomains(s.db, projectID)
}

// Tx.ListApiDomains 事务内版本（导入大事务里读领域名 → id 映射，§13.14 死锁红线）。
func (t *Tx) ListApiDomains(projectID string) ([]ApiDomain, error) {
	return listApiDomains(t.x, projectID)
}

func listApiDomains(x apiExecutor, projectID string) ([]ApiDomain, error) {
	rows, err := x.Query(
		`SELECT id, project_id, name, internal, notes_raw, sort, created_at, updated_at
		 FROM api_domains WHERE project_id = ? ORDER BY sort, name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApiDomain
	for rows.Next() {
		var d ApiDomain
		var internal int
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.Name, &internal, &d.NotesRaw, &d.Sort, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		d.Internal = internal == 1
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetApiDomain 按 id 查领域。
func (s *Store) GetApiDomain(id string) (ApiDomain, error) {
	var d ApiDomain
	var internal int
	err := s.db.QueryRow(
		`SELECT id, project_id, name, internal, notes_raw, sort, created_at, updated_at FROM api_domains WHERE id = ?`, id).
		Scan(&d.ID, &d.ProjectID, &d.Name, &internal, &d.NotesRaw, &d.Sort, &d.CreatedAt, &d.UpdatedAt)
	if err == sql.ErrNoRows {
		return d, fmt.Errorf("接口领域不存在（id=%s）", id)
	}
	if err != nil {
		return d, err
	}
	d.Internal = internal == 1
	return d, nil
}

// UpdateApiDomain 手工改名/排序（D19：领域支持改名/排序/删除）。
// 全列写 name/internal/notes_raw/sort——调用方必须先 GetApiDomain 取既有行再改白名单列，
// 否则 internal/notes_raw 会被清零（与 saveApiEntry 同一类全列 upsert 陷阱，
// 服务端读-改-写的责任在 appsvc 层）。
func (s *Store) UpdateApiDomain(d ApiDomain) error {
	if dup, err := apiDomainNameTaken(s.db, d.ProjectID, d.Name, d.ID); err != nil {
		return err
	} else if dup {
		return fmt.Errorf("接口领域已存在: %s", d.Name)
	}
	internal := 0
	if d.Internal {
		internal = 1
	}
	_, err := s.db.Exec(`UPDATE api_domains SET name = ?, internal = ?, notes_raw = ?, sort = ?, updated_at = ? WHERE id = ?`,
		d.Name, internal, d.NotesRaw, d.Sort, time.Now().UnixMilli(), d.ID)
	return err
}

// apiDomainNameTaken 检查同项目内领域名是否被"其他"领域占用（excludeID 排除自己）。
// 照 deployFlowNameTaken / apiProjectNameTaken 先例：先查再给中文错误，不裸捕 UNIQUE。
func apiDomainNameTaken(x apiExecutor, projectID, name, excludeID string) (bool, error) {
	var n int
	err := x.QueryRow(`SELECT COUNT(*) FROM api_domains WHERE project_id = ? AND name = ? AND id != ?`,
		projectID, name, excludeID).Scan(&n)
	return n > 0, err
}

// DeleteApiDomain 删除领域及其全部接口（字段/示例/命名结构级联，单事务手写 DELETE）。
//
// ⚠️ 手工删除领域请用 DeleteApiDomainRow + 空领域校验，不要用这里：硬删条目会让
// 既有变更事件的 entry_id 引用悬空（违反 PRD R7「事件要能长期引用当时那个接口」）。
// 本方法保留给「按领域整片清理」这类显式全删语义（store 层完整 CRUD 面，暂无调用方）。
func (s *Store) DeleteApiDomain(id string) error {
	return s.WithTx(func(tx *Tx) error { return deleteApiDomain(tx.x, id) })
}

func deleteApiDomain(x apiExecutor, id string) error {
	stmts := []string{
		`DELETE FROM api_fields WHERE owner_kind = 'endpoint' AND owner_id IN (SELECT id FROM api_entries WHERE domain_id = ?)`,
		`DELETE FROM api_examples WHERE owner_kind = 'endpoint' AND owner_id IN (SELECT id FROM api_entries WHERE domain_id = ?)`,
		`DELETE FROM api_fields WHERE owner_kind = 'schema' AND owner_id IN (SELECT id FROM api_schemas WHERE owner_scope IN (SELECT id FROM api_entries WHERE domain_id = ?))`,
		`DELETE FROM api_examples WHERE owner_kind = 'schema' AND owner_id IN (SELECT id FROM api_schemas WHERE owner_scope IN (SELECT id FROM api_entries WHERE domain_id = ?))`,
		`DELETE FROM api_schemas WHERE owner_scope IN (SELECT id FROM api_entries WHERE domain_id = ?)`,
		`DELETE FROM api_entries WHERE domain_id = ?`,
		`DELETE FROM api_domains WHERE id = ?`,
	}
	for _, q := range stmts {
		if _, err := x.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

// CountApiEntriesInDomain 领域下的条目数：active = status='active'，total 含软删条目。
// 手工删除领域（D19「只允许删空领域」）的判据。domainID 为空直接返回 0，不发查询。
func (s *Store) CountApiEntriesInDomain(domainID string) (active int, total int, err error) {
	return countApiEntriesInDomain(s.db, domainID)
}

func countApiEntriesInDomain(x apiExecutor, domainID string) (int, int, error) {
	if domainID == "" {
		return 0, 0, nil
	}
	var active, total int
	err := x.QueryRow(`SELECT COALESCE(SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END), 0), COUNT(*)
	 FROM api_entries WHERE domain_id = ?`, domainID).Scan(&active, &total)
	if err != nil {
		return 0, 0, err
	}
	return active, total, nil
}

// DeleteApiDomainRow 物理删除领域行本身（手工管理路径，单事务两条语句）。
//
// 为什么是物理删：变更事件读的是 api_change_items.domain_name 冗余快照（PRD R7/§8），
// 领域行删掉不会让历史事件读不出领域名；留着空行反而让「已删领域」在列表里刷屏。
//
// 为什么不像 DeleteApiDomain 那样级联删条目：appsvc 只在「领域下没有 active 条目」时
// 放行，此时残余的行只可能是软删条目，它们必须继续存在供事件按 entry_id 跳转
// （硬删会让引用悬空，违反 R7）。所以这里把这些行改为「未归领域」（domain_id=”），
// 下次导入命中文档同名章节时领域行会被 upsert 重建、条目也会被重新挂回去。
//
// 只提供 Store 面（无 Tx 面）：手工管理是单条自动提交链路，导入大事务永不删领域行；
// 助手已是 apiExecutor 形态，将来需要事务面时直接加一行转发即可。
func (s *Store) DeleteApiDomainRow(id string) error {
	return s.WithTx(func(tx *Tx) error { return deleteApiDomainRow(tx.x, id) })
}

func deleteApiDomainRow(x apiExecutor, id string) error {
	now := time.Now().UnixMilli()
	if _, err := x.Exec(`UPDATE api_entries SET domain_id = '', updated_at = ? WHERE domain_id = ?`, now, id); err != nil {
		return err
	}
	res, err := x.Exec(`DELETE FROM api_domains WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("接口领域不存在（id=%s）", id)
	}
	return nil
}

// ===================== 接口条目 CRUD =====================

const apiEntryCols = `id, project_id, domain_id, name, method, path_raw, identity_key, base_path_assert,
	content_type, auth, visibility, semantic_hash, doc_hash, description, parse_state, raw_markdown,
	status, deleted_at, sort, created_at, updated_at`

func scanApiEntry(row interface{ Scan(...any) error }) (ApiEntry, error) {
	var e ApiEntry
	err := row.Scan(&e.ID, &e.ProjectID, &e.DomainID, &e.Name, &e.Method, &e.PathRaw, &e.IdentityKey,
		&e.BasePathAssert, &e.ContentType, &e.Auth, &e.Visibility, &e.SemanticHash, &e.DocHash,
		&e.Description, &e.ParseState, &e.RawMarkdown, &e.Status, &e.DeletedAt, &e.Sort, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}

// SaveApiEntry 按 UNIQUE(project_id, identity_key) upsert 接口定义（只存当前态），回填 id/created_at。
// 重新出现的历史软删条目由调用方传 Status=active / DeletedAt=0 完成「恢复」（change_type=restored）。
//
// ⚠️ 全列 upsert：21 列一次写满，入参没带的列会被直接清零（parse_state/status/visibility
// 还有空值兜底，会把「未解析」这类事实静默改掉）。除导入链路（它本来就产出全量定义）外，
// 手工编辑请走 appsvc.SaveApiEntry —— 那边做了读-改-写并列出了服务端保留列清单。
func (s *Store) SaveApiEntry(e *ApiEntry) error { return saveApiEntry(s.db, e) }

// Tx.SaveApiEntry 事务内版本（一次导入的多表写）。
func (t *Tx) SaveApiEntry(e *ApiEntry) error { return saveApiEntry(t.x, e) }

func saveApiEntry(x apiExecutor, e *ApiEntry) error {
	now := time.Now().UnixMilli()
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	if e.Status == "" {
		e.Status = ApiEntryStatusActive
	}
	if e.Visibility == "" {
		e.Visibility = ApiVisibilityPublic
	}
	if e.ParseState == "" {
		e.ParseState = ApiParseStateOK
	}
	_, err := x.Exec(
		`INSERT INTO api_entries (id, project_id, domain_id, name, method, path_raw, identity_key, base_path_assert,
		   content_type, auth, visibility, semantic_hash, doc_hash, description, parse_state, raw_markdown,
		   status, deleted_at, sort, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, identity_key) DO UPDATE SET
		   domain_id = excluded.domain_id, name = excluded.name, method = excluded.method,
		   path_raw = excluded.path_raw, base_path_assert = excluded.base_path_assert,
		   content_type = excluded.content_type, auth = excluded.auth, visibility = excluded.visibility,
		   semantic_hash = excluded.semantic_hash, doc_hash = excluded.doc_hash,
		   description = excluded.description, parse_state = excluded.parse_state,
		   raw_markdown = excluded.raw_markdown, status = excluded.status,
		   deleted_at = excluded.deleted_at, sort = excluded.sort, updated_at = excluded.updated_at`,
		e.ID, e.ProjectID, e.DomainID, e.Name, e.Method, e.PathRaw, e.IdentityKey, e.BasePathAssert,
		e.ContentType, e.Auth, e.Visibility, e.SemanticHash, e.DocHash, e.Description, e.ParseState,
		e.RawMarkdown, e.Status, e.DeletedAt, e.Sort, e.CreatedAt, e.UpdatedAt)
	if err != nil {
		return err
	}
	// 冲突更新时保留的是既有行的 id/created_at，回查回填（一次 SELECT，非逐行往返）
	return x.QueryRow(`SELECT id, created_at FROM api_entries WHERE project_id = ? AND identity_key = ?`,
		e.ProjectID, e.IdentityKey).Scan(&e.ID, &e.CreatedAt)
}

// GetApiEntry 按 id 查条目（含 raw_markdown 原文）。
func (s *Store) GetApiEntry(id string) (ApiEntry, error) {
	e, err := scanApiEntry(s.db.QueryRow(`SELECT `+apiEntryCols+` FROM api_entries WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return e, fmt.Errorf("接口条目不存在（id=%s）", id)
	}
	return e, err
}

// GetApiEntryByIdentity 按身份键查条目（导入 diff 用）。
func (s *Store) GetApiEntryByIdentity(projectID, identityKey string) (ApiEntry, error) {
	return getApiEntryByIdentity(s.db, projectID, identityKey)
}

// Tx.GetApiEntryByIdentity 事务内版本（导入大事务里对照当前态算 diff）。
func (t *Tx) GetApiEntryByIdentity(projectID, identityKey string) (ApiEntry, error) {
	return getApiEntryByIdentity(t.x, projectID, identityKey)
}

func getApiEntryByIdentity(x apiExecutor, projectID, identityKey string) (ApiEntry, error) {
	e, err := scanApiEntry(x.QueryRow(
		`SELECT `+apiEntryCols+` FROM api_entries WHERE project_id = ? AND identity_key = ?`, projectID, identityKey))
	if err == sql.ErrNoRows {
		return e, fmt.Errorf("接口条目不存在: %s", identityKey)
	}
	return e, err
}

// ListApiEntries 浏览列表（R9）：domainID 为空 = 全项目；默认过滤 removed 与 internal。
// 不返回 raw_markdown（大文本，详情页走 GetApiEntry）。
func (s *Store) ListApiEntries(projectID, domainID string, includeRemoved, includeInternal bool) ([]ApiEntry, error) {
	query := `SELECT id, project_id, domain_id, name, method, path_raw, identity_key, base_path_assert,
		content_type, auth, visibility, semantic_hash, doc_hash, description, parse_state, '',
		status, deleted_at, sort, created_at, updated_at FROM api_entries WHERE project_id = ?`
	args := []any{projectID}
	if domainID != "" {
		query += ` AND domain_id = ?`
		args = append(args, domainID)
	}
	if !includeRemoved {
		query += ` AND status = 'active'`
	}
	if !includeInternal {
		query += ` AND visibility = 'public'`
	}
	query += ` ORDER BY sort, name`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApiEntry
	for rows.Next() {
		var e ApiEntry
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.DomainID, &e.Name, &e.Method, &e.PathRaw, &e.IdentityKey,
			&e.BasePathAssert, &e.ContentType, &e.Auth, &e.Visibility, &e.SemanticHash, &e.DocHash,
			&e.Description, &e.ParseState, &e.RawMarkdown, &e.Status, &e.DeletedAt, &e.Sort, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkApiEntryRemoved 软删（status=removed + deleted_at）：事件要能长期引用「当时那个接口」，
// 硬删会让引用悬空。列表默认过滤 removed。
func (s *Store) MarkApiEntryRemoved(id string) error { return markApiEntryRemoved(s.db, id) }

// Tx.MarkApiEntryRemoved 事务内版本（导入的显式声明删除）。
func (t *Tx) MarkApiEntryRemoved(id string) error { return markApiEntryRemoved(t.x, id) }

func markApiEntryRemoved(x apiExecutor, id string) error {
	now := time.Now().UnixMilli()
	res, err := x.Exec(`UPDATE api_entries SET status = ?, deleted_at = ?, updated_at = ? WHERE id = ?`,
		ApiEntryStatusRemoved, now, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("接口条目不存在（id=%s）", id)
	}
	return nil
}

// DeleteApiEntry 硬删条目（手工管理用；导入删除一律走软删）。级联字段/示例/命名结构。
func (s *Store) DeleteApiEntry(id string) error {
	return s.WithTx(func(tx *Tx) error { return deleteApiEntry(tx.x, id) })
}

func deleteApiEntry(x apiExecutor, id string) error {
	stmts := []string{
		`DELETE FROM api_fields WHERE owner_kind = 'endpoint' AND owner_id = ?`,
		`DELETE FROM api_examples WHERE owner_kind = 'endpoint' AND owner_id = ?`,
		`DELETE FROM api_fields WHERE owner_kind = 'schema' AND owner_id IN (SELECT id FROM api_schemas WHERE owner_scope = ?)`,
		`DELETE FROM api_examples WHERE owner_kind = 'schema' AND owner_id IN (SELECT id FROM api_schemas WHERE owner_scope = ?)`,
		`DELETE FROM api_schemas WHERE owner_scope = ?`,
		`DELETE FROM api_entries WHERE id = ?`,
	}
	for _, q := range stmts {
		if _, err := x.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

// ===================== 字段 / 示例（整组替换，单事务批量写）=====================

// ReplaceApiFields 整组替换某宿主的字段（先删后插，单事务；SetMaxOpenConns(1) 下禁止逐行 SELECT 往返）。
func (s *Store) ReplaceApiFields(ownerKind, ownerID string, fields []ApiField) error {
	return s.WithTx(func(tx *Tx) error { return tx.ReplaceApiFields(ownerKind, ownerID, fields) })
}

// Tx.ReplaceApiFields 事务内版本（一次导入的多表写）。
func (t *Tx) ReplaceApiFields(ownerKind, ownerID string, fields []ApiField) error {
	return replaceApiFields(t.x, ownerKind, ownerID, fields)
}

func replaceApiFields(x apiExecutor, ownerKind, ownerID string, fields []ApiField) error {
	if _, err := x.Exec(`DELETE FROM api_fields WHERE owner_kind = ? AND owner_id = ?`, ownerKind, ownerID); err != nil {
		return err
	}
	for i := range fields {
		f := &fields[i]
		f.OwnerKind = ownerKind
		f.OwnerID = ownerID
		if f.FactsJSON == "" {
			f.FactsJSON = "{}"
		}
		if f.Sort == 0 {
			f.Sort = i + 1
		}
		res, err := x.Exec(
			`INSERT INTO api_fields (owner_kind, owner_id, kind, name, type_raw, type_key, required,
			   position, default_value, description, facts_json, ref_schema_id, depth, sort)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.OwnerKind, f.OwnerID, f.Kind, f.Name, f.TypeRaw, f.TypeKey, f.Required,
			f.Position, f.DefaultValue, f.Description, f.FactsJSON, f.RefSchemaID, f.Depth, f.Sort)
		if err != nil {
			return err
		}
		if id, err := res.LastInsertId(); err == nil {
			f.ID = id // 回填失败静默忽略（照现有先例）
		}
	}
	return nil
}

// ListApiFields 返回某宿主的全部字段（文档序）。
func (s *Store) ListApiFields(ownerKind, ownerID string) ([]ApiField, error) {
	return listApiFields(s.db, ownerKind, ownerID)
}

// Tx.ListApiFields 事务内版本（removed 明细载荷要读被删接口的当前字段，R7）。
func (t *Tx) ListApiFields(ownerKind, ownerID string) ([]ApiField, error) {
	return listApiFields(t.x, ownerKind, ownerID)
}

func listApiFields(x apiExecutor, ownerKind, ownerID string) ([]ApiField, error) {
	rows, err := x.Query(
		`SELECT id, owner_kind, owner_id, kind, name, type_raw, type_key, required, position,
		   default_value, description, facts_json, ref_schema_id, depth, sort
		 FROM api_fields WHERE owner_kind = ? AND owner_id = ? ORDER BY sort, id`, ownerKind, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApiField
	for rows.Next() {
		var f ApiField
		if err := rows.Scan(&f.ID, &f.OwnerKind, &f.OwnerID, &f.Kind, &f.Name, &f.TypeRaw, &f.TypeKey,
			&f.Required, &f.Position, &f.DefaultValue, &f.Description, &f.FactsJSON, &f.RefSchemaID,
			&f.Depth, &f.Sort); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ReplaceApiExamples 整组替换某宿主的示例块（单事务先删后插）。
func (s *Store) ReplaceApiExamples(ownerKind, ownerID string, list []ApiExample) error {
	return s.WithTx(func(tx *Tx) error { return tx.ReplaceApiExamples(ownerKind, ownerID, list) })
}

// Tx.ReplaceApiExamples 事务内版本（一次导入的多表写）。
func (t *Tx) ReplaceApiExamples(ownerKind, ownerID string, list []ApiExample) error {
	return replaceApiExamples(t.x, ownerKind, ownerID, list)
}

func replaceApiExamples(x apiExecutor, ownerKind, ownerID string, list []ApiExample) error {
	if _, err := x.Exec(`DELETE FROM api_examples WHERE owner_kind = ? AND owner_id = ?`, ownerKind, ownerID); err != nil {
		return err
	}
	for i := range list {
		ex := &list[i]
		ex.OwnerKind = ownerKind
		ex.OwnerID = ownerID
		if ex.Sort == 0 {
			ex.Sort = i + 1
		}
		res, err := x.Exec(
			`INSERT INTO api_examples (owner_kind, owner_id, kind, language, content, sort) VALUES (?, ?, ?, ?, ?, ?)`,
			ex.OwnerKind, ex.OwnerID, ex.Kind, ex.Language, ex.Content, ex.Sort)
		if err != nil {
			return err
		}
		if id, err := res.LastInsertId(); err == nil {
			ex.ID = id
		}
	}
	return nil
}

// ListApiExamples 返回某宿主的全部示例块（文档序）。
func (s *Store) ListApiExamples(ownerKind, ownerID string) ([]ApiExample, error) {
	rows, err := s.db.Query(
		`SELECT id, owner_kind, owner_id, kind, language, content, sort
		 FROM api_examples WHERE owner_kind = ? AND owner_id = ? ORDER BY sort, id`, ownerKind, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApiExample
	for rows.Next() {
		var ex ApiExample
		if err := rows.Scan(&ex.ID, &ex.OwnerKind, &ex.OwnerID, &ex.Kind, &ex.Language, &ex.Content, &ex.Sort); err != nil {
			return nil, err
		}
		out = append(out, ex)
	}
	return out, rows.Err()
}

// ===================== 命名结构 CRUD =====================

// SaveApiSchema 按 UNIQUE(project_id, name, owner_scope) upsert 命名结构并回填 id。
func (s *Store) SaveApiSchema(sc *ApiSchema) error { return saveApiSchema(s.db, sc) }

// Tx.SaveApiSchema 事务内版本（一次导入的多表写）。
func (t *Tx) SaveApiSchema(sc *ApiSchema) error { return saveApiSchema(t.x, sc) }

func saveApiSchema(x apiExecutor, sc *ApiSchema) error {
	now := time.Now().UnixMilli()
	if sc.ID == "" {
		sc.ID = uuid.NewString()
	}
	if sc.CreatedAt == 0 {
		sc.CreatedAt = now
	}
	sc.UpdatedAt = now
	ambiguous := 0
	if sc.Ambiguous {
		ambiguous = 1
	}
	_, err := x.Exec(
		`INSERT INTO api_schemas (id, project_id, name, owner_scope, raw_markdown, body_hash, ambiguous, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, name, owner_scope) DO UPDATE SET
		   raw_markdown = excluded.raw_markdown, body_hash = excluded.body_hash,
		   ambiguous = excluded.ambiguous, updated_at = excluded.updated_at`,
		sc.ID, sc.ProjectID, sc.Name, sc.OwnerScope, sc.RawMarkdown, sc.BodyHash, ambiguous, sc.CreatedAt, sc.UpdatedAt)
	if err != nil {
		return err
	}
	return x.QueryRow(`SELECT id, created_at FROM api_schemas WHERE project_id = ? AND name = ? AND owner_scope = ?`,
		sc.ProjectID, sc.Name, sc.OwnerScope).Scan(&sc.ID, &sc.CreatedAt)
}

// ListApiSchemas 返回项目下全部命名结构（按名称排序）。
func (s *Store) ListApiSchemas(projectID string) ([]ApiSchema, error) {
	rows, err := s.db.Query(
		`SELECT id, project_id, name, owner_scope, raw_markdown, body_hash, ambiguous, created_at, updated_at
		 FROM api_schemas WHERE project_id = ? ORDER BY name, owner_scope`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApiSchema
	for rows.Next() {
		var sc ApiSchema
		var ambiguous int
		if err := rows.Scan(&sc.ID, &sc.ProjectID, &sc.Name, &sc.OwnerScope, &sc.RawMarkdown,
			&sc.BodyHash, &ambiguous, &sc.CreatedAt, &sc.UpdatedAt); err != nil {
			return nil, err
		}
		sc.Ambiguous = ambiguous == 1
		out = append(out, sc)
	}
	return out, rows.Err()
}

// GetApiSchema 按 id 查命名结构。
func (s *Store) GetApiSchema(id string) (ApiSchema, error) {
	var sc ApiSchema
	var ambiguous int
	err := s.db.QueryRow(
		`SELECT id, project_id, name, owner_scope, raw_markdown, body_hash, ambiguous, created_at, updated_at
		 FROM api_schemas WHERE id = ?`, id).
		Scan(&sc.ID, &sc.ProjectID, &sc.Name, &sc.OwnerScope, &sc.RawMarkdown, &sc.BodyHash,
			&ambiguous, &sc.CreatedAt, &sc.UpdatedAt)
	if err == sql.ErrNoRows {
		return sc, fmt.Errorf("命名结构不存在（id=%s）", id)
	}
	if err != nil {
		return sc, err
	}
	sc.Ambiguous = ambiguous == 1
	return sc, nil
}

// DeleteApiSchema 删除命名结构及其字段/示例。
func (s *Store) DeleteApiSchema(id string) error {
	return s.WithTx(func(tx *Tx) error { return deleteApiSchema(tx.x, id) })
}

func deleteApiSchema(x apiExecutor, id string) error {
	for _, q := range []string{
		`DELETE FROM api_fields WHERE owner_kind = 'schema' AND owner_id = ?`,
		`DELETE FROM api_examples WHERE owner_kind = 'schema' AND owner_id = ?`,
		`DELETE FROM api_schemas WHERE id = ?`,
	} {
		if _, err := x.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

// ===================== 变更事件 CRUD =====================

// InsertApiChangeEvent 插入事件：Seq==0 时在同一事务里取 MAX(seq)+1（D7；
// SetMaxOpenConns(1) + UNIQUE(project_id, seq) 双保险不撞号）。回填自增 id。
func (s *Store) InsertApiChangeEvent(e *ApiChangeEvent) error {
	return s.WithTx(func(tx *Tx) error { return tx.InsertApiChangeEvent(e) })
}

// Tx.InsertApiChangeEvent 事务内版本：MAX(seq)+1 与 INSERT 落在导入大事务里（并发/重入不撞号）。
func (t *Tx) InsertApiChangeEvent(e *ApiChangeEvent) error { return insertApiChangeEvent(t.x, e) }

func insertApiChangeEvent(x apiExecutor, e *ApiChangeEvent) error {
	now := time.Now().UnixMilli()
	if e.CreatedAt == 0 {
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	if e.Seq == 0 {
		var maxSeq int
		if err := x.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM api_change_events WHERE project_id = ?`, e.ProjectID).
			Scan(&maxSeq); err != nil {
			return err
		}
		e.Seq = maxSeq + 1
	}
	if e.Source == "" {
		e.Source = ApiEventSourceImport
	}
	if e.Kind == "" {
		e.Kind = ApiEventKindFeature
	}
	if e.ScopeMode == "" {
		e.ScopeMode = ApiScopeIncremental
	}
	statsJSON, err := json.Marshal(e.Stats)
	if err != nil {
		statsJSON = []byte("{}")
	}
	metaJSON := []byte("{}")
	if e.Meta != nil {
		if b, err := json.Marshal(e.Meta); err == nil {
			metaJSON = b
		}
	}
	res, err := x.Exec(
		`INSERT INTO api_change_events (project_id, seq, label, title, reason, kind, breaking, source,
		   doc_ref, scope_mode, stats, meta, doc_text, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ProjectID, e.Seq, e.Label, e.Title, e.Reason, e.Kind, e.Breaking, e.Source,
		e.DocRef, e.ScopeMode, string(statsJSON), string(metaJSON), e.DocText, e.CreatedAt, e.UpdatedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		e.ID = id
	}
	return nil
}

// apiEventColsNoDocText 事件列表/详情共用的明确列名（禁止 SELECT *：doc_text 每行几十 KB）。
const apiEventColsNoDocText = `id, project_id, seq, label, title, reason, kind, breaking, source,
	doc_ref, scope_mode, stats, meta, created_at, updated_at`

func scanApiEventNoDocText(row interface{ Scan(...any) error }) (ApiChangeEvent, error) {
	var e ApiChangeEvent
	var statsJSON, metaJSON string
	err := row.Scan(&e.ID, &e.ProjectID, &e.Seq, &e.Label, &e.Title, &e.Reason, &e.Kind, &e.Breaking,
		&e.Source, &e.DocRef, &e.ScopeMode, &statsJSON, &metaJSON, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return e, err
	}
	// JSON 列反序列化容错：坏数据不让列表崩掉（照 ListDeployFlows 的 _ = json.Unmarshal 先例）
	_ = json.Unmarshal([]byte(statsJSON), &e.Stats)
	_ = json.Unmarshal([]byte(metaJSON), &e.Meta)
	return e, nil
}

// ListApiChangeEvents 事件列表：按 seq DESC（D7/AC7）；不含 doc_text。limit>0 时截断。
func (s *Store) ListApiChangeEvents(projectID string, limit int) ([]ApiChangeEvent, error) {
	query := `SELECT ` + apiEventColsNoDocText + ` FROM api_change_events WHERE project_id = ? ORDER BY seq DESC`
	args := []any{projectID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApiChangeEvent
	for rows.Next() {
		e, err := scanApiEventNoDocText(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetApiChangeEvent 事件详情（不含 doc_text，原文单独走 GetApiChangeEventDocText）。
func (s *Store) GetApiChangeEvent(id int64) (ApiChangeEvent, error) {
	e, err := scanApiEventNoDocText(s.db.QueryRow(
		`SELECT `+apiEventColsNoDocText+` FROM api_change_events WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return e, fmt.Errorf("变更事件不存在（id=%d）", id)
	}
	return e, err
}

// GetApiChangeEventDocText 单独取事件保存的导入文档全文（D12 输入凭证）。
func (s *Store) GetApiChangeEventDocText(id int64) (string, error) {
	var docText string
	err := s.db.QueryRow(`SELECT doc_text FROM api_change_events WHERE id = ?`, id).Scan(&docText)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("变更事件不存在（id=%d）", id)
	}
	return docText, err
}

// DeleteApiChangeEvent 删除事件及其明细。
func (s *Store) DeleteApiChangeEvent(id int64) error {
	return s.WithTx(func(tx *Tx) error { return deleteApiChangeEvent(tx.x, id) })
}

func deleteApiChangeEvent(x apiExecutor, id int64) error {
	if _, err := x.Exec(`DELETE FROM api_change_items WHERE event_id = ?`, id); err != nil {
		return err
	}
	if _, err := x.Exec(`DELETE FROM api_change_events WHERE id = ?`, id); err != nil {
		return err
	}
	return nil
}

// ===================== 事件明细 CRUD =====================

// InsertApiChangeItems 批量插入一次事件的明细（单事务；入参指针切片回填自增 id）。
func (s *Store) InsertApiChangeItems(items []*ApiChangeItem) error {
	return s.WithTx(func(tx *Tx) error { return tx.InsertApiChangeItems(items) })
}

// Tx.InsertApiChangeItems 事务内版本（一次导入的多表写）。
func (t *Tx) InsertApiChangeItems(items []*ApiChangeItem) error {
	return insertApiChangeItems(t.x, items)
}

func insertApiChangeItems(x apiExecutor, items []*ApiChangeItem) error {
	for i, it := range items {
		if it == nil {
			continue
		}
		if it.Sort == 0 {
			it.Sort = i + 1
		}
		if it.Visibility == "" {
			it.Visibility = ApiVisibilityPublic
		}
		diffJSON := []byte("[]")
		if it.Diff != nil {
			if b, err := json.Marshal(it.Diff); err == nil {
				diffJSON = b
			}
		}
		res, err := x.Exec(
			`INSERT INTO api_change_items (event_id, project_id, entry_id, domain_name, entry_name,
			   method, path, change_type, breaking, visibility, diff, declared_note, sort)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			it.EventID, it.ProjectID, it.EntryID, it.DomainName, it.EntryName,
			it.Method, it.Path, it.ChangeType, it.Breaking, it.Visibility, string(diffJSON), it.DeclaredNote, it.Sort)
		if err != nil {
			return err
		}
		if id, err := res.LastInsertId(); err == nil {
			it.ID = id
		}
	}
	return nil
}

// ListApiChangeItems 返回事件的全部明细（文档序）。diff JSON 列坏数据容错不崩。
func (s *Store) ListApiChangeItems(eventID int64) ([]ApiChangeItem, error) {
	rows, err := s.db.Query(
		`SELECT id, event_id, project_id, entry_id, domain_name, entry_name, method, path,
		   change_type, breaking, visibility, diff, declared_note, sort
		 FROM api_change_items WHERE event_id = ? ORDER BY sort, id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApiChangeItem
	for rows.Next() {
		var it ApiChangeItem
		var diffJSON string
		if err := rows.Scan(&it.ID, &it.EventID, &it.ProjectID, &it.EntryID, &it.DomainName, &it.EntryName,
			&it.Method, &it.Path, &it.ChangeType, &it.Breaking, &it.Visibility, &diffJSON, &it.DeclaredNote, &it.Sort); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(diffJSON), &it.Diff)
		out = append(out, it)
	}
	return out, rows.Err()
}

// ===================== 导入批次 CRUD（表 MVP 就建好，删除候选逻辑二期）=====================

// InsertApiImportBatch 记录一次导入批次（keys_json = 本次声明的 identity_key 集，
// 是将来 full 模式删除判定的「上次声明集」；doc_fingerprint 是幂等第二道保险）。回填自增 id。
func (s *Store) InsertApiImportBatch(b *ApiImportBatch) error { return insertApiImportBatch(s.db, b) }

// Tx.InsertApiImportBatch 事务内版本（批次记录与导入同生共死：回滚则批次不留痕）。
func (t *Tx) InsertApiImportBatch(b *ApiImportBatch) error { return insertApiImportBatch(t.x, b) }

func insertApiImportBatch(x apiExecutor, b *ApiImportBatch) error {
	b.CreatedAt = time.Now().UnixMilli()
	keysJSON := []byte("[]")
	if b.Keys != nil {
		if js, err := json.Marshal(b.Keys); err == nil {
			keysJSON = js
		}
	}
	checkJSON, err := json.Marshal(b.SummaryCheck)
	if err != nil {
		checkJSON = []byte("{}")
	}
	res, err := x.Exec(
		`INSERT INTO api_import_batches (project_id, scope_identity, declared_mode, doc_fingerprint,
		   keys_json, summary_check, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		b.ProjectID, b.ScopeIdentity, b.DeclaredMode, b.DocFingerprint,
		string(keysJSON), string(checkJSON), b.CreatedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		b.ID = id
	}
	return nil
}

// ListApiImportBatches 按 scope_identity 返回批次（id 倒序）；limit>0 时截断。
func (s *Store) ListApiImportBatches(scopeIdentity string, limit int) ([]ApiImportBatch, error) {
	query := `SELECT id, project_id, scope_identity, declared_mode, doc_fingerprint, keys_json, summary_check, created_at
		 FROM api_import_batches WHERE scope_identity = ? ORDER BY id DESC`
	args := []any{scopeIdentity}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApiImportBatch
	for rows.Next() {
		var b ApiImportBatch
		var keysJSON, checkJSON string
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.ScopeIdentity, &b.DeclaredMode, &b.DocFingerprint,
			&keysJSON, &checkJSON, &b.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(keysJSON), &b.Keys)
		_ = json.Unmarshal([]byte(checkJSON), &b.SummaryCheck)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ===================== 导入编排支撑（PR3 追加，全部为新增方法）=====================
//
// appsvc 的导入编排（Preview 只读 / Commit 大事务）需要一组「区分不存在与真实库错误」
// 的查询和事务内读写。既有方法签名一律不动，这里只追加。

// FindApiProjectByName 查询版 GetApiProjectByName：(零值, false, nil) = 不存在。
// 导入编排需要把「项目还没建」当正常分支处理，而不是靠中文错误串反推。
func (s *Store) FindApiProjectByName(name string) (ApiProject, bool, error) {
	return findApiProjectByName(s.db, name)
}

// Tx.FindApiProjectByName 事务内版本（导入大事务里定位/新建项目）。
func (t *Tx) FindApiProjectByName(name string) (ApiProject, bool, error) {
	return findApiProjectByName(t.x, name)
}

func findApiProjectByName(x apiExecutor, name string) (ApiProject, bool, error) {
	var p ApiProject
	err := x.QueryRow(`SELECT id, name, display, base_path, created_at, updated_at FROM api_projects WHERE name = ?`, name).
		Scan(&p.ID, &p.Name, &p.Display, &p.BasePath, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return ApiProject{}, false, nil
	}
	if err != nil {
		return p, false, err
	}
	return p, true, nil
}

// FindApiEntryByIdentity 查询版 GetApiEntryByIdentity：(零值, false, nil) = 不存在。
func (s *Store) FindApiEntryByIdentity(projectID, identityKey string) (ApiEntry, bool, error) {
	return findApiEntryByIdentity(s.db, projectID, identityKey)
}

// Tx.FindApiEntryByIdentity 事务内版本（导入 diff 对照当前态）。
func (t *Tx) FindApiEntryByIdentity(projectID, identityKey string) (ApiEntry, bool, error) {
	return findApiEntryByIdentity(t.x, projectID, identityKey)
}

func findApiEntryByIdentity(x apiExecutor, projectID, identityKey string) (ApiEntry, bool, error) {
	e, err := scanApiEntry(x.QueryRow(
		`SELECT `+apiEntryCols+` FROM api_entries WHERE project_id = ? AND identity_key = ?`, projectID, identityKey))
	if err == sql.ErrNoRows {
		return ApiEntry{}, false, nil
	}
	if err != nil {
		return e, false, err
	}
	return e, true, nil
}

// ListApiSchemasByOwner 按作用域查命名结构：ownerScope=” = 文档级；否则 = 宿主接口 id。
// 导入 diff 需要从库里回建「before 端点」的命名结构集合（含 body_hash）。
func (s *Store) ListApiSchemasByOwner(projectID, ownerScope string) ([]ApiSchema, error) {
	return listApiSchemasByOwner(s.db, projectID, ownerScope)
}

// Tx.ListApiSchemasByOwner 事务内版本。
func (t *Tx) ListApiSchemasByOwner(projectID, ownerScope string) ([]ApiSchema, error) {
	return listApiSchemasByOwner(t.x, projectID, ownerScope)
}

func listApiSchemasByOwner(x apiExecutor, projectID, ownerScope string) ([]ApiSchema, error) {
	rows, err := x.Query(
		`SELECT id, project_id, name, owner_scope, raw_markdown, body_hash, ambiguous, created_at, updated_at
		 FROM api_schemas WHERE project_id = ? AND owner_scope = ? ORDER BY name`, projectID, ownerScope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApiSchema
	for rows.Next() {
		var sc ApiSchema
		var ambiguous int
		if err := rows.Scan(&sc.ID, &sc.ProjectID, &sc.Name, &sc.OwnerScope, &sc.RawMarkdown,
			&sc.BodyHash, &ambiguous, &sc.CreatedAt, &sc.UpdatedAt); err != nil {
			return nil, err
		}
		sc.Ambiguous = ambiguous == 1
		out = append(out, sc)
	}
	return out, rows.Err()
}

// DeleteApiSchemasByScope 清空某作用域下的命名结构（含其字段/示例）：
// 「命名结构整组替换」的第一步，随后逐个 SaveApiSchema 重建。
func (s *Store) DeleteApiSchemasByScope(projectID, ownerScope string) error {
	return deleteApiSchemasByScope(s.db, projectID, ownerScope)
}

// Tx.DeleteApiSchemasByScope 事务内版本（导入大事务）。
func (t *Tx) DeleteApiSchemasByScope(projectID, ownerScope string) error {
	return deleteApiSchemasByScope(t.x, projectID, ownerScope)
}

func deleteApiSchemasByScope(x apiExecutor, projectID, ownerScope string) error {
	for _, q := range []string{
		`DELETE FROM api_fields WHERE owner_kind = 'schema' AND owner_id IN (SELECT id FROM api_schemas WHERE project_id = ? AND owner_scope = ?)`,
		`DELETE FROM api_examples WHERE owner_kind = 'schema' AND owner_id IN (SELECT id FROM api_schemas WHERE project_id = ? AND owner_scope = ?)`,
		`DELETE FROM api_schemas WHERE project_id = ? AND owner_scope = ?`,
	} {
		if _, err := x.Exec(q, projectID, ownerScope); err != nil {
			return err
		}
	}
	return nil
}

// CountActiveApiEntries 项目下 active 条目数（scope:init 闸门的判据，spec-draft §13.12）。
// projectID 为空（项目尚未创建）直接返回 0，不发查询。
func (s *Store) CountActiveApiEntries(projectID string) (int, error) {
	return countActiveApiEntries(s.db, projectID)
}

// Tx.CountActiveApiEntries 事务内版本（闸门在导入大事务里判定，回滚零写入）。
func (t *Tx) CountActiveApiEntries(projectID string) (int, error) {
	return countActiveApiEntries(t.x, projectID)
}

func countActiveApiEntries(x apiExecutor, projectID string) (int, error) {
	if projectID == "" {
		return 0, nil
	}
	var n int
	err := x.QueryRow(`SELECT COUNT(*) FROM api_entries WHERE project_id = ? AND status = 'active'`, projectID).Scan(&n)
	return n, err
}

// ApiEntryEventRef 条目详情页的「该接口历史事件」反向关联行（R9/AC7）。
// change_type/breaking 取自明细行（同一接口在不同事件里的角色不同）。
type ApiEntryEventRef struct {
	EventID    int64  `json:"eventId"`
	ItemID     int64  `json:"itemId"`
	Seq        int    `json:"seq"`
	Label      string `json:"label"`
	Title      string `json:"title"`
	Kind       string `json:"kind"`
	Breaking   int    `json:"breaking"`
	ChangeType string `json:"changeType"`
	CreatedAt  int64  `json:"createdAt"`
}

// ListApiEntryEvents 反向查某条目参与过的全部变更事件（seq DESC；明细 join 事件头，
// 不取 doc_text——原文永远只走 GetApiChangeEventDocText）。
func (s *Store) ListApiEntryEvents(entryID string) ([]ApiEntryEventRef, error) {
	rows, err := s.db.Query(
		`SELECT e.id, i.id, e.seq, e.label, e.title, e.kind, i.breaking, i.change_type, e.created_at
		 FROM api_change_items i JOIN api_change_events e ON e.id = i.event_id
		 WHERE i.entry_id = ? ORDER BY e.seq DESC, i.sort`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApiEntryEventRef
	for rows.Next() {
		var r ApiEntryEventRef
		if err := rows.Scan(&r.EventID, &r.ItemID, &r.Seq, &r.Label, &r.Title, &r.Kind,
			&r.Breaking, &r.ChangeType, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
