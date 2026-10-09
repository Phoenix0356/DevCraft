// pg.go 是 PostgreSQL 配置的编排实现：独立的 PG 设置读写（GetPgSettings /
// SavePgSettings / TestPg 三个绑定的业务落点）+ 技能侧的实时配置回调与
// 查询执行器 + 一次性自动补装迁移。
//
// 为什么 PG 设置走独立绑定、不塞进现有 SaveSettings：
// 设置页是「通用配置 / 数据库配置」两个 tab，各自独立保存。若共用一个
// 全量表单 struct，保存 A tab 时 B tab 的字段会以"表单当前值"整体覆盖——
// 前端稍有疏漏（漏带字段）就会互相清空对方配置。独立绑定 = 独立职责边界，
// 两个 tab 永不相扰。
//
// 密码语义与 apiKey 完全同款：
//   - 存储：AES-GCM 加密后落 settings 键值表（secrets.Box，复用现有加密）
//   - 读取：GetPgSettings 只回传 passwordSet（"是否已设置"），绝不明文回传
//   - 写入：SavePgSettings 密码留空 = 保持原值不变
package appsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"DevCraft/internal/agent"    // BuiltinOpsAgentID（补装迁移的目标 Agent）
	"DevCraft/internal/dbx"      // PG 连接与只读查询封装
	"DevCraft/internal/skill/pg" // pg.Config（技能侧配置回调的返回类型）
)

// PG 设置的键名常量（settings 键值表，命名对照 docker.* / llm.* 惯例）。
const (
	settingPgHost      = "pg.host"      // 主机 IP/域名
	settingPgPort      = "pg.port"      // 端口（存字符串，读时转 int；默认 5432）
	settingPgUser      = "pg.user"      // 用户名
	settingPgPassword  = "pg.password"  // 密码（AES-GCM 密文，永不明文落盘）
	settingPgDatabases = "pg.databases" // 关注数据库白名单（JSON 数组文本）
	settingPgSchemas   = "pg.schemas"   // 关注 schema 白名单（JSON 数组文本）

	// settingSeedPgSkills 一次性补装迁移的幂等标记：
	// 存在即表示"pg 技能已补装进运维 Agent"，后续启动直接跳过。
	settingSeedPgSkills = "seed.pg_skills_v1"

	pgDefaultPort = 5432 // PostgreSQL 默认端口
)

// pgProbe 是"试连"的可注入点（对照 newLLM 工厂 / deployRunnerFor 的可测范式）：
// 生产指向 dbx.Probe（真连一次 PG），单测替换成假实现验证参数组装。
var pgProbe = dbx.Probe

// ==================== 设置表单与视图 ====================

// PgSettings 前端「数据库配置」tab 提交的表单。
// Password 是只写字段（UI → 后端）：留空表示"保持原值不变"，与 apiKey 同款语义。
type PgSettings struct {
	Host      string   `json:"host"`
	Port      int      `json:"port"` // <=0 时归一化为 5432
	User      string   `json:"user"`
	Password  string   `json:"password"`  // 只写字段：留空=不修改
	Databases []string `json:"databases"` // 关注数据库白名单
	Schemas   []string `json:"schemas"`   // 关注 schema 白名单
}

// PgSettingsView 返回给前端的 PG 设置视图。
// 密码绝不回传明文，只告知"是否已设置"（与 SettingsView.apiKeySet 同一纪律）。
type PgSettingsView struct {
	Host        string   `json:"host"`
	Port        int      `json:"port"`
	User        string   `json:"user"`
	PasswordSet bool     `json:"passwordSet"`
	Databases   []string `json:"databases"`
	Schemas     []string `json:"schemas"`
}

// GetPgSettings 读 PG 设置（「数据库配置」tab 打开时调用）。
func (s *Service) GetPgSettings() (PgSettingsView, error) {
	// 初始化为默认值/空切片：JS 侧拿到 [] 而不是 null，前端少一层判空
	view := PgSettingsView{Port: pgDefaultPort, Databases: []string{}, Schemas: []string{}}
	if v, ok, err := s.setting(settingPgHost); err != nil {
		return view, err
	} else if ok {
		view.Host = v
	}
	if v, ok, err := s.setting(settingPgPort); err != nil {
		return view, err
	} else if ok && v != "" {
		// 存储层是字符串（settings 表只有 TEXT 列），读时转 int；坏数据回退默认端口
		if p, perr := strconv.Atoi(v); perr == nil && p > 0 {
			view.Port = p
		}
	}
	if v, ok, err := s.setting(settingPgUser); err != nil {
		return view, err
	} else if ok {
		view.User = v
	}
	if enc, ok, err := s.setting(settingPgPassword); err != nil {
		return view, err
	} else if ok && enc != "" {
		view.PasswordSet = true // 只暴露"已设置"这个事实
	}
	if v, err := s.pgStringList(settingPgDatabases); err != nil {
		return view, err
	} else {
		view.Databases = v
	}
	if v, err := s.pgStringList(settingPgSchemas); err != nil {
		return view, err
	} else {
		view.Schemas = v
	}
	return view, nil
}

// SavePgSettings 保存 PG 设置；密码非空时先 AES-GCM 加密再落盘，
// 留空则完全跳过（保持原值——"不修改"语义的实现关键：不是写空串覆盖）。
func (s *Service) SavePgSettings(in PgSettings) error {
	in.Host = strings.TrimSpace(in.Host)
	in.User = strings.TrimSpace(in.User)
	if in.Port <= 0 {
		in.Port = pgDefaultPort // 未填端口按 PG 默认端口归一化
	}
	if in.Port > 65535 {
		return fmt.Errorf("端口号不合法: %d（有效范围 1-65535）", in.Port)
	}
	if err := s.store.SetSetting(settingPgHost, in.Host); err != nil {
		return err
	}
	if err := s.store.SetSetting(settingPgPort, strconv.Itoa(in.Port)); err != nil {
		return err
	}
	if err := s.store.SetSetting(settingPgUser, in.User); err != nil {
		return err
	}
	// 白名单归一化：逐项去空白、丢空项（动态标签输入的常见残留）
	if err := s.setPgStringList(settingPgDatabases, normalizeList(in.Databases)); err != nil {
		return err
	}
	if err := s.setPgStringList(settingPgSchemas, normalizeList(in.Schemas)); err != nil {
		return err
	}
	if in.Password != "" {
		enc, err := s.box.Encrypt(in.Password)
		if err != nil {
			return fmt.Errorf("加密 PostgreSQL 密码失败: %w", err)
		}
		if err := s.store.SetSetting(settingPgPassword, enc); err != nil {
			return err
		}
	}
	slog.Info("PG 设置已保存", "host", in.Host, "port", in.Port, "user", in.User,
		"databases", len(normalizeList(in.Databases)), "schemas", len(normalizeList(in.Schemas)))
	return nil
}

// pgStringList 读一个存 JSON 数组文本的设置键；未设置或坏数据都按空列表处理
// （对照 store.ListDeployFlows 对 JSON 列的容错：坏一条不崩全局）。
func (s *Service) pgStringList(key string) ([]string, error) {
	v, ok, err := s.setting(key)
	if err != nil {
		return nil, err
	}
	out := []string{}
	if ok && v != "" {
		_ = json.Unmarshal([]byte(v), &out)
	}
	return out, nil
}

// setPgStringList 把字符串列表序列化成 JSON 文本落库。
func (s *Service) setPgStringList(key string, list []string) error {
	data, err := json.Marshal(list)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	return s.store.SetSetting(key, string(data))
}

// normalizeList 逐项 TrimSpace 并丢弃空项（防御动态标签输入的残留空串）。
func normalizeList(list []string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// pgPassword 解密 PG 密码；未设置或解密失败返回空串（对照 sshPassword 的容错：
// 密钥文件丢失不应阻塞读取其他设置，连接时自然失败并给出明确错误）。
func (s *Service) pgPassword() string {
	enc, ok, err := s.setting(settingPgPassword)
	if err != nil || !ok || enc == "" {
		return ""
	}
	pass, err := s.box.Decrypt(enc)
	if err != nil {
		return ""
	}
	return pass
}

// ==================== 连接测试 ====================

// TestPg 用"当前表单值"测试 PG 连接（设置页「测试连接」按钮，对照 TestSSH：
// 参数直接来自输入框而非已保存配置，未保存也能测）。
// password 留空 = 用已保存的密码（与"留空不修改"语义呼应：测试的也是保存后将用的凭据）。
func (s *Service) TestPg(ctx context.Context, host string, port int, user, password string) error {
	host = strings.TrimSpace(host)
	user = strings.TrimSpace(user)
	if host == "" {
		return fmt.Errorf("请填写 PostgreSQL 主机地址")
	}
	if user == "" {
		return fmt.Errorf("请填写 PostgreSQL 用户名")
	}
	if port <= 0 {
		port = pgDefaultPort
	}
	if port > 65535 {
		return fmt.Errorf("端口号不合法: %d（有效范围 1-65535）", port)
	}
	if password == "" {
		password = s.pgPassword() // 表单没填密码：回退到已保存的密文解密值
	}
	// 连到哪个库：优先关注列表的第一个（最贴近真实使用场景），否则 PG 维护库 postgres
	database := "postgres"
	if dbs, err := s.pgStringList(settingPgDatabases); err == nil && len(dbs) > 0 {
		database = dbs[0]
	}
	target := dbx.PGTarget{Host: host, Port: port, User: user, Password: password}
	if err := pgProbe(ctx, target, database); err != nil {
		slog.Error("测试 PostgreSQL 连接失败", "host", host, "port", port, "user", user, "db", database, "err", err)
		return err // dbx 已包装成中文错误（含主机/端口/库名），直接透传给前端展示
	}
	slog.Info("测试 PostgreSQL 连接成功", "host", host, "port", port, "user", user, "db", database)
	return nil
}

// ==================== 技能侧：实时配置回调 + 查询执行器 ====================

// PgSkillConfig 返回技能可见的白名单配置（注册 pg 技能时作为 ConfigFn 传入）。
// 每次技能执行都实时读库——用户在设置页改了关注范围，下一次调用立即生效
// （对照 DockerEndpoint 的回调范式：用回调而非快照）。
func (s *Service) PgSkillConfig() pg.Config {
	dbs, err := s.pgStringList(settingPgDatabases)
	if err != nil {
		slog.Error("读取关注数据库列表失败", "err", err)
		dbs = nil
	}
	schemas, err := s.pgStringList(settingPgSchemas)
	if err != nil {
		slog.Error("读取关注 schema 列表失败", "err", err)
		schemas = nil
	}
	return pg.Config{Databases: dbs, Schemas: schemas}
}

// PgQuery 执行一次只读查询（注册 pg 技能时作为 QueryFn 传入）。
// 技能层已完成白名单与 SQL 门禁校验；本层负责组装连接凭据并委托 dbx
// （连接层 SET default_transaction_read_only + 30s 超时 + 文本表格渲染）。
func (s *Service) PgQuery(ctx context.Context, database, query string, args []any, limit int) (string, error) {
	target, err := s.pgTarget()
	if err != nil {
		return "", err
	}
	return dbx.RunQuery(ctx, target, database, query, args, limit)
}

// pgTarget 从当前设置组装连接目标（密码现场解密；改设置立即生效）。
func (s *Service) pgTarget() (dbx.PGTarget, error) {
	host, _, _ := s.setting(settingPgHost)
	host = strings.TrimSpace(host)
	if host == "" {
		return dbx.PGTarget{}, fmt.Errorf("尚未配置 PostgreSQL 连接信息：请先在设置 →「数据库配置」中填写主机与账号")
	}
	port := pgDefaultPort
	if v, _, _ := s.setting(settingPgPort); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			port = p
		}
	}
	user, _, _ := s.setting(settingPgUser)
	user = strings.TrimSpace(user)
	if user == "" {
		return dbx.PGTarget{}, fmt.Errorf("尚未配置 PostgreSQL 用户名：请先在设置 →「数据库配置」中填写")
	}
	return dbx.PGTarget{Host: host, Port: port, User: user, Password: s.pgPassword()}, nil
}

// ==================== 一次性自动补装迁移 ====================

// MigratePgSkills 把 3 个 pg 技能补装进内置运维 Agent（升级兼容）。
//
// 背景：SeedDefaults 是 skip-if-exists——存量安装的运维 Agent 已存在，
// 永远不会自动获得新增技能。本方法在启动时（ServiceStartup，注册完技能后）
// 执行一次性补装，用 settings 标记 seed.pg_skills_v1 保证幂等：
//   - 标记已存在 → 直接返回（用户此后自由增删装配，绝不再干预）
//   - 标记不存在 → 并集合并（union：保留用户已装的全部技能，只补缺的）→ 写标记
//
// 新装用户同样受益：SeedDefaults 播种的 Agent 不含 pg 技能，首次启动由此补上。
func (s *Service) MigratePgSkills() error {
	if v, ok, err := s.store.GetSetting(settingSeedPgSkills); err != nil {
		return err
	} else if ok && v != "" {
		return nil // 已补装过：幂等跳过
	}
	a, err := s.store.GetAgent(agent.BuiltinOpsAgentID)
	if err != nil {
		return fmt.Errorf("补装 PG 技能失败（运维 Agent 不存在）: %w", err)
	}
	// 并集合并：现有装配原样保留（用户数据），只在尾部追加缺失的 pg 技能
	seen := make(map[string]bool, len(a.Skills))
	merged := make([]string, 0, len(a.Skills)+len(pg.SkillNames))
	for _, n := range a.Skills {
		seen[n] = true
		merged = append(merged, n)
	}
	added := 0
	for _, n := range pg.SkillNames {
		if !seen[n] {
			merged = append(merged, n)
			added++
		}
	}
	if added > 0 {
		if err := s.store.ReplaceAgentSkills(agent.BuiltinOpsAgentID, merged); err != nil {
			return fmt.Errorf("补装 PG 技能失败: %w", err)
		}
	}
	// 标记最后写：中途失败则下次启动重试（宁可重复检查，不可漏装）
	if err := s.store.SetSetting(settingSeedPgSkills, "1"); err != nil {
		return err
	}
	slog.Info("PG 技能补装迁移完成", "agent", agent.BuiltinOpsAgentID, "added", added)
	return nil
}
