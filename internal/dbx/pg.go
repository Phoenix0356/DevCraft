// Package dbx 封装外部数据库的"连接 + 只读查询 + 文本化输出"。
// 本期只有 PostgreSQL（pgx 纯 Go 驱动，与项目的 CGO-free 路线一致）。
//
// 设计要点：
//  1. 只读双保险之一在这里落地：连接建立后立即执行
//     `SET default_transaction_read_only = on`——即使上层 SQL 门禁被绕过
//     （如 WITH 里藏数据修改 CTE），服务端也会直接拒绝写操作；
//  2. 每次调用新建连接、用完即关（MVP 决策：简单可靠，设置变更即时生效，
//     不需要按配置指纹失效的连接缓存）；
//  3. 查询结果渲染成对齐的文本表格——消费方是 LLM（技能把文本回填给模型），
//     易读文本比 JSON 更省 token 也更利于模型理解。
//
// Java 类比：本包 ≈ 一个极简的 JdbcTemplate + ResultSetFormatter，
// 但没有连接池（每次 DriverManager.getConnection，用完 close）。
package dbx

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5" // 纯 Go 的 PostgreSQL 驱动（不依赖 libpq/C）
)

// 防护参数：包级变量（而非常量）便于单元测试调小。
var (
	// QueryTimeout 单次"连接 + 查询"的总超时。慢查询/网络不通都不会永久挂起
	// ——与项目"绝不永久挂起"的纪律一致（对照回合超时、部署执行超时）。
	QueryTimeout = 30 * time.Second
)

// 渲染防护：单元格内容先按字符截断，防止一行超长文本撑爆输出体量。
const (
	maxCellRunes   = 80 // 单元格显示上限（超出加省略号）
	maxColumnRunes = 40 // 列宽上限（对齐用；单元格内容仍保留到 maxCellRunes）
)

// PGTarget PostgreSQL 连接目标（一套凭据；database 在 RunQuery/Probe 单独传，
// 因为 PG 的连接是"库级"的——切库必须重连，这正是白名单按库校验的原因）。
type PGTarget struct {
	Host     string
	Port     int
	User     string
	Password string
}

// connString 拼出 pgx 可用的连接 URL。
// 用 net/url 构造而非手工拼接：用户名/密码里的特殊字符（@ : / 空格等）
// 会被正确转义，避免"密码里带个 @ 就连错主机"的经典事故。
func (t PGTarget) connString(database string) string {
	port := t.Port
	if port <= 0 {
		port = 5432 // PostgreSQL 默认端口
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(t.User, t.Password),
		Host:   net.JoinHostPort(t.Host, strconv.Itoa(port)),
		Path:   database,
	}
	return u.String()
}

// DialPG 建立一个只读 PG 连接。调用方负责 Close。
// ctx 携带超时/取消信号：连接阶段卡住也会被中断。
func DialPG(ctx context.Context, t PGTarget, database string) (*pgx.Conn, error) {
	if strings.TrimSpace(t.Host) == "" {
		return nil, fmt.Errorf("PostgreSQL 主机为空，请先在设置的「数据库配置」中填写")
	}
	if database == "" {
		database = "postgres" // PG 惯例的维护库（未指定库时的兜底）
	}
	conn, err := pgx.Connect(ctx, t.connString(database))
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL（%s:%d/%s）失败: %w", t.Host, port(t), database, err)
	}
	// 只读双保险的"连接层"：会话级强制只读事务，写语句会被服务端拒绝。
	// 放在连接建立后立即执行，任何后续查询都在只读语义下运行。
	if _, err := conn.Exec(ctx, "SET default_transaction_read_only = on"); err != nil {
		_ = conn.Close(ctx) // 设置失败则连接不可信，直接释放
		return nil, fmt.Errorf("设置只读模式失败: %w", err)
	}
	return conn, nil
}

// port 是渲染错误信息用的小助手（Port<=0 时显示默认端口）。
func port(t PGTarget) int {
	if t.Port <= 0 {
		return 5432
	}
	return t.Port
}

// Probe 试连一次并立即关闭（设置页"测试连接"按钮的落点）。
// 只验证"能连上、能执行只读语句"，不关心返回内容。
func Probe(ctx context.Context, t PGTarget, database string) error {
	ctx, cancel := context.WithTimeout(ctx, QueryTimeout)
	defer cancel()
	conn, err := DialPG(ctx, t, database)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx)) // 关闭不再受超时影响（连接已建立）
	if _, err := conn.Exec(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("连接已建立但执行测试语句失败: %w", err)
	}
	return nil
}

// RunQuery 一次性执行只读查询：连接 → 查询 → 渲染文本表格 → 关连接。
// 总超时 QueryTimeout（30s）；maxRows 是行数上限（超出截断并在尾部注明）。
// 这是 pg 技能包注入的 QueryFn 的真实实现（见 internal/skill/pg）。
func RunQuery(ctx context.Context, t PGTarget, database, query string, args []any, maxRows int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, QueryTimeout)
	defer cancel()
	conn, err := DialPG(ctx, t, database)
	if err != nil {
		return "", err
	}
	// defer + context.WithoutCancel：即使 ctx 已超时也要把连接关干净，
	// 否则服务端会话残留直到 TCP 超时（资源泄漏）。
	defer conn.Close(context.WithoutCancel(ctx))
	return QueryTable(ctx, conn, query, args, maxRows)
}

// QueryTable 执行查询并把结果集渲染成对齐的文本表格。
// 行数防护：最多读 maxRows 行——多探一行判断"是否被截断"，
// 截断时在表格尾部注明，让 LLM 知道结果不完整（可建议用户加 LIMIT）。
func QueryTable(ctx context.Context, conn *pgx.Conn, query string, args []any, maxRows int) (string, error) {
	if maxRows <= 0 {
		maxRows = 1 // 防御：调用方传了非法上限也不至于读出全表
	}
	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("查询执行失败: %w", err)
	}
	defer rows.Close()

	// 列名来自结果集元数据（≈ ResultSetMetaData.getColumnLabel）
	fields := rows.FieldDescriptions()
	headers := make([]string, len(fields))
	for i, f := range fields {
		headers[i] = string(f.Name)
	}

	var body [][]string
	truncated := false
	for rows.Next() {
		if len(body) >= maxRows {
			truncated = true // 还有数据但已达上限：停止读取（不消耗剩余行）
			break
		}
		// Values() 把当前行所有列取成 []any（pgx 已按 PG 类型解码成 Go 值）
		vals, err := rows.Values()
		if err != nil {
			return "", fmt.Errorf("读取结果行失败: %w", err)
		}
		line := make([]string, len(vals))
		for i, v := range vals {
			line[i] = cellText(v)
		}
		body = append(body, line)
	}
	if err := rows.Err(); err != nil { // 迭代中途出错必须检查（对照 store.go 的纪律）
		return "", fmt.Errorf("读取查询结果失败: %w", err)
	}

	var b strings.Builder
	b.WriteString(renderTable(headers, body))
	if len(body) == 0 {
		b.WriteString("（0 行）")
	} else {
		fmt.Fprintf(&b, "（共 %d 行）", len(body))
	}
	if truncated {
		fmt.Fprintf(&b, "\n注意: 结果超过 %d 行，已截断。可缩小查询范围或降低返回列数。", maxRows)
	}
	return b.String(), nil
}

// cellText 把任意列值转成适合表格展示的单行文本。
// nil → NULL；换行/制表符压成空格（保持表格一行一条记录）；超长按字符截断。
func cellText(v any) string {
	var s string
	switch tv := v.(type) {
	case nil:
		return "NULL"
	case string:
		s = tv
	case []byte:
		s = string(tv) // bytea 等二进制列按原文展示（MVP 够用）
	case time.Time:
		s = tv.Format("2006-01-02 15:04:05") // 时间统一格式（Go 用参考时间当模板）
	default:
		s = fmt.Sprint(tv)
	}
	s = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
	r := []rune(s)
	if len(r) > maxCellRunes {
		s = string(r[:maxCellRunes]) + "…"
	}
	return s
}

// renderTable 渲染对齐的文本表格：表头 + 分隔线 + 数据行。
// 列宽 = 该列最宽内容（封顶 maxColumnRunes），中文按 rune 计数不会错位。
func renderTable(headers []string, body [][]string) string {
	if len(headers) == 0 {
		return ""
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = min(maxColumnRunes, len([]rune(h)))
	}
	for _, row := range body {
		for i, c := range row {
			if i < len(widths) {
				widths[i] = min(maxColumnRunes, max(widths[i], len([]rune(c))))
			}
		}
	}
	var b strings.Builder
	writeRow(&b, headers, widths)
	// 分隔线：按列宽画短横线（+ 连接）
	parts := make([]string, len(widths))
	for i, w := range widths {
		parts[i] = strings.Repeat("-", w+2)
	}
	b.WriteString(strings.Join(parts, "+"))
	b.WriteString("\n")
	for _, row := range body {
		writeRow(&b, row, widths)
	}
	return b.String()
}

// writeRow 写一行：每列 " 内容（按列宽左对齐补齐） "，竖线分隔。
// fmt 的 %-Ns 宽度按 rune 计数，中文列也能对齐。
func writeRow(b *strings.Builder, cells []string, widths []int) {
	parts := make([]string, len(widths))
	for i := range widths {
		c := ""
		if i < len(cells) {
			c = cells[i]
		}
		parts[i] = fmt.Sprintf(" %-*s ", widths[i], c)
	}
	b.WriteString(strings.Join(parts, "|"))
	b.WriteString("\n")
}
