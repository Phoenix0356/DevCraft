package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Session 会话实体。反引号里的是 struct tag：
// `json:"id"` 告诉 JSON 序列化器字段名叫 id（≈ Jackson 的 @JsonProperty("id")）。
// Wails 把 Go 对象传给前端时就是按这些 tag 序列化的。
type Session struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	AgentID     string `json:"agentId"`
	TitleSource string `json:"titleSource"` // 空串=首条消息截断占位 | 'ai' | 'manual'（见标题来源常量）
	Pinned      int    `json:"pinned"`      // 0 普通 | 1 置顶（SQLite 无 boolean，沿用本仓库 0/1 惯例）
	PinnedAt    int64  `json:"pinnedAt"`    // 置顶时间毫秒；0 = 未置顶（与 Pinned 同步变更，见 SetSessionPinned）
	CreatedAt   int64  `json:"createdAt"`
	UpdatedAt   int64  `json:"updatedAt"`
}

// 标题来源取值（sessions.title_source 列的状态机，见 appsvc/title.go）。
const (
	TitleSourceAI     = "ai"     // AI 自动总结写入
	TitleSourceManual = "manual" // 手动改名，永久锁定（AI 不再覆盖）
)

// ===================== 会话 CRUD =====================

// CreateSession 插入新会话。
// title_source 不在 INSERT 列里：新建会话必然是占位态（空串），由列 DEFAULT 落位，
// 不给调用方"带着别的来源创建"的机会（状态只能靠下面的定向写入推进）。
// pinned / pinned_at 同理：新建会话必然不置顶，由列 DEFAULT 落位，置顶只能走 SetSessionPinned。
func (s *Store) CreateSession(sess Session) error {
	now := time.Now().UnixMilli() // 毫秒时间戳
	if sess.CreatedAt == 0 {
		sess.CreatedAt = now
	}
	sess.UpdatedAt = now
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, title, agent_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		sess.ID, sess.Title, sess.AgentID, sess.CreatedAt, sess.UpdatedAt, // ? 占位符防注入（≈ PreparedStatement）
	)
	return err
}

// ListSessions 置顶会话整体在前，置顶组内按置顶时间倒序（刚钉住的排最前），
// 其余会话按最后活跃时间倒序（未置顶的 pinned_at 恒为 0，并列无效）。
// 排序只有这一个来源：前端从不本地排序，拿到什么顺序就渲染什么顺序。
func (s *Store) ListSessions() ([]Session, error) {
	rows, err := s.db.Query(`SELECT id, title, agent_id, title_source, pinned, pinned_at, created_at, updated_at FROM sessions ORDER BY pinned DESC, pinned_at DESC, updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() // defer = 函数返回前必定执行（≈ finally），防止连接泄漏
	var out []Session  // nil 切片可以直接 append，Go 的切片 ≈ 自动扩容的 ArrayList
	for rows.Next() {  // 逐行迭代（≈ ResultSet.next()）
		var sess Session
		// Scan 把当前行各列按顺序读入变量（列顺序必须与 SELECT 一致）
		if err := rows.Scan(&sess.ID, &sess.Title, &sess.AgentID, &sess.TitleSource, &sess.Pinned, &sess.PinnedAt, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, err // & 取地址：Scan 需要指针才能写入（≈ 传引用）
		}
		out = append(out, sess)
	}
	return out, rows.Err() // 迭代结束后必须检查是否有中途错误
}

// GetSession 按 id 查单个会话，不存在时返回带说明的 error。
func (s *Store) GetSession(id string) (Session, error) {
	var sess Session
	// QueryRow 用于"最多一行"的查询，Scan 时若没数据会得到哨兵错误 sql.ErrNoRows
	err := s.db.QueryRow(`SELECT id, title, agent_id, title_source, pinned, pinned_at, created_at, updated_at FROM sessions WHERE id = ?`, id).
		Scan(&sess.ID, &sess.Title, &sess.AgentID, &sess.TitleSource, &sess.Pinned, &sess.PinnedAt, &sess.CreatedAt, &sess.UpdatedAt)
	if err == sql.ErrNoRows {
		return sess, fmt.Errorf("session %s not found", id)
	}
	return sess, err
}

// ===================== 定向写入（列所有权）=====================
//
// 这里刻意没有"全字段 UPDATE"的口子：会话行由多个写入方并发演进（占位标题、
// AI 异步总结、手动改名、@前缀切 Agent），全字段回写会把调用方读到的陈旧内存值
// 盖回数据库，冲掉别的写入方刚写进去的列。所以每个写入方只写自己拥有的列，
// 并且把"能不能写"的判断也放进 SQL 条件里（保护逻辑落在库里，不靠调用方自律）。
// 返回值 bool = 是否真的写入了（RowsAffected > 0），false 表示被 WHERE 条件挡下。

// UpdateSessionTitle AI 总结写标题：只在标题不是手动改过的情况下生效。
// title_source='manual' 的行被 WHERE 永久排除——手动改名保护由 SQL 条件保证。
func (s *Store) UpdateSessionTitle(id, title string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE sessions SET title = ?, title_source = ?, updated_at = ?
		 WHERE id = ? AND title_source <> ?`,
		title, TitleSourceAI, time.Now().UnixMilli(), id, TitleSourceManual)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetSessionTitleManual 手动改名：写标题并把来源锁成 manual（此后 AI 永不再覆盖）。
func (s *Store) SetSessionTitleManual(id, title string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE sessions SET title = ?, title_source = ?, updated_at = ? WHERE id = ?`,
		title, TitleSourceManual, time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetSessionFallbackTitle 首条消息截断的占位标题定向写入。
// 守卫在 SQL 里：仅当标题仍是"新会话"/空 且 未被手动改名时才写——
// 这样并发的 AI 总结先落库后，占位写入自然落空（不会用截断文本盖掉 AI 标题）。
func (s *Store) SetSessionFallbackTitle(id, title string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE sessions SET title = ?, updated_at = ?
		 WHERE id = ? AND (title = '' OR title = '新会话') AND title_source <> ?`,
		title, time.Now().UnixMilli(), id, TitleSourceManual)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetSessionAgentID 只改会话绑定的 Agent（不触碰标题与来源）。
func (s *Store) SetSessionAgentID(id, agentID string) (bool, error) {
	res, err := s.db.Exec(`UPDATE sessions SET agent_id = ?, updated_at = ? WHERE id = ?`,
		agentID, time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetSessionPinned 只改置顶标记（不触碰标题、来源与 Agent 绑定）。
// 这是写 pinned 的唯一入口：0/1 由调用方决定，本方法不做 toggle。
// 刻意不刷 updated_at：置顶不是「活跃」，刷了会让取消置顶的老会话跳到最近组顶部，
// 回到的是它该有的历史位置而不是列表最前。
// pinned_at（置顶时间，置顶组内倒序的依据）由 pinned 在本函数内部派生，刻意不接参数：
// pinned=1 ⇔ pinned_at>0 这个不变量必须由写 pinned 的这一个点守住——一旦让调用方
// 分别赋值，两边就能写劈（标了置顶却排不出顺序，或取消置顶却留着旧时间戳）。
func (s *Store) SetSessionPinned(id string, pinned int) (bool, error) {
	pinnedAt := int64(0) // 取消置顶归零：未置顶的行不参与 pinned_at 排序
	if pinned == 1 {
		pinnedAt = time.Now().UnixMilli()
	}
	res, err := s.db.Exec(`UPDATE sessions SET pinned = ?, pinned_at = ? WHERE id = ?`, pinned, pinnedAt, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// DeleteSession 级联删除会话和它的所有消息。
func (s *Store) DeleteSession(id string) error {
	if _, err := s.db.Exec(`DELETE FROM messages WHERE session_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}
