package store

import "database/sql"

// GetSetting 读设置项。返回 (值, 是否存在, 错误)——Go 惯用多返回值代替 Optional。
func (s *Store) GetSetting(key string) (string, bool, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil // 不存在不是错误，用第二个返回值表达
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetSetting 写设置项（存在则覆盖）。
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}
