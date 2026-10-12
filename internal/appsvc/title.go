// 会话标题的手动改名。
//
// 标题的最终来源就是 SendMessage 里"首条消息文本截断"（title=="新会话"||"" 时
// 写入 truncate(text,30)）；手动改名后 title 不再是"新会话"，占位逻辑天然不会
// 覆盖用户改的名字，无需任何状态机/CAS。
package appsvc

import (
	"fmt"
	"log/slog"
	"strings"
)

// RenameSession 手动改名（前端 ✏️ 行内编辑的落点）：
// 校验非空、截断 30 字（复用 truncate，与占位标题同款语义），GetSession 改 Title 后写回。
func (s *Service) RenameSession(id, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("会话标题不能为空")
	}
	title = truncate(title, 30)
	sess, err := s.store.GetSession(id)
	if err != nil {
		return err
	}
	sess.Title = title
	if err := s.store.UpdateSession(sess); err != nil {
		return err
	}
	slog.Info("会话已手动改名", "session", id, "title", title)
	return nil
}
