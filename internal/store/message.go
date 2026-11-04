package store

import "time"

// Message 消息实体。
type Message struct {
	ID        int64  `json:"id"`
	SessionID string `json:"sessionId"`
	Role      string `json:"role"` // user | assistant | tool
	Content   string `json:"content"`
	CreatedAt int64  `json:"createdAt"`
}

// AppendMessage 追加一条消息，并同步刷新所属会话的 updated_at。
func (s *Store) AppendMessage(m Message) error {
	m.CreatedAt = time.Now().UnixMilli()
	res, err := s.db.Exec(`INSERT INTO messages (session_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
		m.SessionID, m.Role, m.Content, m.CreatedAt)
	if err != nil {
		return err
	}
	// LastInsertId 拿到自增 id（失败也不致命，忽略错误只记 id）
	if id, err := res.LastInsertId(); err == nil {
		m.ID = id
	}
	_, err = s.db.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, m.CreatedAt, m.SessionID)
	return err
}

// Messages 返回会话消息；limit>0 时只取最近 limit 条。
// 实现技巧：先按 id DESC 取最近 N 条，再在内存里反转回时间正序。
func (s *Store) Messages(sessionID string, limit int) ([]Message, error) {
	query := `SELECT id, session_id, role, content, created_at FROM messages WHERE session_id = ? ORDER BY id DESC`
	args := []any{sessionID} // []any = []interface{}，动态拼参数列表（any ≈ Object）
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...) // args... 把切片展开成多个参数（≈ varargs）
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 双指针原地反转切片（倒序 → 正序）
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i] // Go 支持元组式交换写法
	}
	return out, nil
}
