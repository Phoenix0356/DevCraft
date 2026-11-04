// Package store 是持久层（≈ Java 的 DAO 层），用 SQLite 存储会话、消息、
// Agent 定义与设置。没有 ORM，全部是手写 SQL（≈ MyBatis 手写 SQL 的风格）。
//
// 使用纯 Go 驱动 modernc.org/sqlite（由 C 版 SQLite 机器翻译而来），
// 好处是不需要 CGO（C 编译器），交叉编译到任何平台都无痛。
//
// 文件划分（按领域分文件，一个文件只出现一个领域的 struct / 方法 / 常量）：
//
//	store.go       —— 核心句柄：Store、Open、Close（本文件）
//	migrate.go     —— 迁移：执行建表 DDL + 幂等加列（不含 DDL 字符串）
//	schema_core.go —— 核心业务表 DDL（sessions/messages/agents/…/deploy_history）
//	schema_api.go  —— API 文档表 DDL（api_* 系列）
//	session.go     —— Sessions 领域：CRUD + 定向写入
//	message.go     —— Messages 领域
//	agent.go       —— Agents 领域
//	setting.go     —— Settings 领域
//	deploy.go      —— DeployFlows / DeployHistory 领域
//	api.go         —— API 文档领域（api_* 表的实体与 CRUD）
package store

import (
	"database/sql" // Go 标准库的数据库抽象（≈ JDBC：DriverManager + Connection 的统一接口）
	"fmt"

	_ "modernc.org/sqlite" // 下划线导入 = 只为执行包的 init() 副作用：
	// 该 init() 会向 database/sql 注册名为 "sqlite" 的驱动（≈ Class.forName 注册 JDBC 驱动）
)

// Store 持有数据库连接池句柄（≈ DataSource）。
type Store struct {
	db *sql.DB // sql.DB 内部自带连接池且并发安全，全局共享一个即可
}

// Open 打开（或创建）SQLite 数据库文件并执行建表迁移。
func Open(path string) (*Store, error) {
	// sql.Open 只是建立"驱动+连接串"的句柄，并不真正连接（懒加载）
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err) // %w 包装原错误，上层可用 errors.Is/As 解包
	}
	// SQLite 同一时刻只允许一个写者。把连接池上限设为 1，
	// 让并发写在 Go 侧排队，避免 "database is locked" 错误。
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close() // 出错时记得释放资源（Go 没有 try-with-resources，手动处理）
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close 关闭数据库连接（应用退出时调用）。
func (s *Store) Close() error { return s.db.Close() }
