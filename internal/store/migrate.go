package store

import (
	"database/sql"
	"fmt"
)

// migrate 建表。CREATE TABLE IF NOT EXISTS 天然幂等，
// 所以每次启动都跑一遍也没问题（简化版迁移，没有版本号管理）。
// DDL 字符串本身在 schema_core.go / schema_api.go，本文件只留骨架。
func migrate(db *sql.DB) error {
	// 反引号字符串 = 原始字符串（≈ Java 15 文本块 """），见 schema_*.go
	if _, err := db.Exec(schemaCore + schemaAPI); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	// CREATE TABLE IF NOT EXISTS 只管新库；存量库的表结构演进靠下面的幂等加列。
	return ensureColumns(db)
}

// ensureColumns 为存量库补齐后加的列（本仓库唯一的加列迁移入口）。
// 每一步都必须失败可感知：迁移失败会一路返回到 Open，让启动阶段直接暴露问题，
// 绝不静默跳过（列缺失会让后面的读写全部报错，静默只会把故障推迟到更难定位的地方）。
func ensureColumns(db *sql.DB) error {
	if err := ensureTableColumn(db, "sessions", "title_source",
		`ALTER TABLE sessions ADD COLUMN title_source TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureTableColumn(db, "sessions", "pinned",
		`ALTER TABLE sessions ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := ensureTableColumn(db, "sessions", "pinned_at",
		`ALTER TABLE sessions ADD COLUMN pinned_at INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	// 11-01-req-section-absent：未解析槽位随条目落库（详情页 partial 横幅要列出具体缺失小节）。
	// 存量 partial 旧行取 '[]'（前端兜底原文案），重新导入同文档即被刷成真实槽位集。
	return ensureTableColumn(db, "api_entries", "unresolved",
		`ALTER TABLE api_entries ADD COLUMN unresolved TEXT NOT NULL DEFAULT '[]'`)
}

// tableInfoSQL 各表的 PRAGMA table_info 语句。表名写死成常量、不接任何外部输入
// （PRAGMA 的表名不能走 ? 占位符，只能拼字符串——所以把可拼的部分收敛成常量白名单，
// 杜绝标识符注入面）。
var tableInfoSQL = map[string]string{
	"sessions":    `PRAGMA table_info(sessions)`,
	"api_entries": `PRAGMA table_info(api_entries)`,
}

// ensureTableColumn 幂等加列：先 PRAGMA table_info 探测列是否存在，缺了才 ALTER TABLE。
// 三种库都要干净通过：
//   - 全新库：CREATE TABLE 已带该列 → 探测命中 → 跳过；
//   - 存量库无该列：探测未命中 → ALTER 加列（DEFAULT 让旧行取空值）；
//   - 存量库已有该列（历史上跑过旧版迁移的机器）：探测命中 → 跳过，绝不重复 ALTER。
func ensureTableColumn(db *sql.DB, table, column, alterDDL string) error {
	infoSQL, ok := tableInfoSQL[table]
	if !ok {
		return fmt.Errorf("未知的加列目标表: %s", table)
	}
	rows, err := db.Query(infoSQL)
	if err != nil {
		return fmt.Errorf("读取 %s 表结构失败: %w", table, err)
	}
	defer rows.Close()
	exists := false
	for rows.Next() {
		name, err := scanColumnInfo(rows)
		if err != nil {
			return fmt.Errorf("解析 %s 表结构失败: %w", table, err)
		}
		if name == column {
			exists = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历 %s 表结构失败: %w", table, err)
	}
	if exists {
		return nil
	}
	if _, err := db.Exec(alterDDL); err != nil {
		return fmt.Errorf("为 %s 添加列 %s 失败: %w", table, column, err)
	}
	return nil
}

// scanColumnInfo 读 PRAGMA table_info 当前行（列顺序固定：cid, name, type,
// notnull, dflt_value, pk），只取列名。
func scanColumnInfo(rows *sql.Rows) (string, error) {
	var cid, notNull, pk int
	var name, colType string
	var dfltValue sql.NullString
	if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
		return "", err
	}
	return name, nil
}

// sessionColumns 查 sessions 表的列名清单（仅测试用于断言列没有被重复添加）。
func (s *Store) sessionColumns() ([]string, error) {
	rows, err := s.db.Query(tableInfoSQL["sessions"])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		name, err := scanColumnInfo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
