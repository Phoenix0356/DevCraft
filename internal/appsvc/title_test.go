package appsvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"DevCraft/internal/llm"
	"DevCraft/internal/store"
)

// ==================== 测试替身 ====================

// scriptedLLM 固定的"一次性补全"替身：记录调用次数与收到的消息序列，
// 按脚本返回文本 / 报错 / panic。标题总结走 ChatStream 聚合，所以一个替身
// 同时能扮演回合 LLM 与总结 LLM（按 system prompt 区分）。
type scriptedLLM struct {
	mu       sync.Mutex
	calls    int
	reply    string // 总结请求的回复
	answer   string // 回合请求的回复
	err      error
	panics   bool
	blocking bool // true = 挂到 ctx 结束（模拟不返的模型）
}

func (f *scriptedLLM) ChatStream(ctx context.Context, _ string, msgs []llm.Message, _ []llm.ToolSpec, onDelta func(string) error) (*llm.Response, error) {
	f.mu.Lock()
	f.calls++
	sys := ""
	if len(msgs) > 0 {
		sys = msgs[0].Content
	}
	reply, answer, err, panics, block := f.reply, f.answer, f.err, f.panics, f.blocking
	f.mu.Unlock()

	if panics {
		panic("总结替身崩了")
	}
	if block { // 挂到 ctx 结束（与真实适配器被取消后的行为一致）
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	// system 提示词等于标题 prompt 的那次调用 = 标题总结，其余 = 回合
	out := answer
	if sys == titlePrompt {
		out = reply
	}
	if onDelta != nil {
		_ = onDelta(out)
	}
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: out}}, nil
}

func (f *scriptedLLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// seedFirstTurn 手工造一轮问答（不跑真回合也能测总结链路）。
func seedFirstTurn(t *testing.T, svc *Service, sessionID, question, answer string) {
	t.Helper()
	if err := svc.store.AppendMessage(store.Message{SessionID: sessionID, Role: "user", Content: question}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := svc.store.AppendMessage(store.Message{SessionID: sessionID, Role: "assistant", Content: answer}); err != nil {
		t.Fatalf("seed assistant: %v", err)
	}
}

// withTitleSummaryTimeout 临时调小总结超时（生产值 30 秒；照 withTurnTimeout 的先例）。
func withTitleSummaryTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := titleSummaryTimeout
	titleSummaryTimeout = d
	t.Cleanup(func() { titleSummaryTimeout = old })
}

// waitSessionTitle 轮询等异步总结落库（goroutine 路径无法同步 join）。
func waitSessionTitle(t *testing.T, svc *Service, id string, want func(store.Session) bool) store.Session {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		sess, err := svc.store.GetSession(id)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if want(sess) {
			return sess
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待会话标题超时： %+v", sess)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitTitleFrames 轮询等 n 条 session:title 帧。
func waitTitleFrames(t *testing.T, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		frames := titleFramesHook()
		if len(frames) >= n {
			return frames
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %d 条 session:title 帧超时，当前 = %v", n, frames)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ==================== 手动改名 ====================

// TestRenameSession 手动改名：非空校验（中文错误）、超长截断（truncate 同款语义：
// 30 字 + 省略号）、定向写（只动 title/title_source，agent_id 保持不变）。
func TestRenameSession(t *testing.T) {
	svc := newTestService(t)
	sess := newBareSession(t, svc)

	// 空/纯空白标题：中文错误
	for _, bad := range []string{"", "   ", "\t\n"} {
		if err := svc.RenameSession(sess.ID, bad); err == nil || !strings.Contains(err.Error(), "标题不能为空") {
			t.Fatalf("RenameSession(%q) err = %v, want 标题不能为空", bad, err)
		}
	}

	// 不存在的会话：报错（定向写返回 0 行）
	if err := svc.RenameSession("no-such", "x"); err == nil {
		t.Fatal("RenameSession(missing session) should fail")
	}

	// 正常改名：TrimSpace 后写回，agent_id 不受影响
	if err := svc.RenameSession(sess.ID, "  我的运维会话  "); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	got, err := svc.store.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Title != "我的运维会话" {
		t.Fatalf("after rename: title=%q, want 我的运维会话", got.Title)
	}
	if got.AgentID != "bare" {
		t.Fatalf("rename 不该动 agent_id: %q", got.AgentID)
	}
	if got.TitleSource != store.TitleSourceManual {
		t.Fatalf("rename 后 title_source=%q, want manual", got.TitleSource)
	}

	// 超长标题：复用 truncate——按 rune 截断到 30 字并加省略号（与占位标题同款语义）
	long := strings.Repeat("长", 40)
	if err := svc.RenameSession(sess.ID, long); err != nil {
		t.Fatalf("RenameSession(long): %v", err)
	}
	got, _ = svc.store.GetSession(sess.ID)
	if want := strings.Repeat("长", 30) + "…"; got.Title != want {
		t.Fatalf("truncated title = %q, want %q", got.Title, want)
	}
}

// ==================== cleanTitle ====================

// TestCleanTitle 模型输出噪声清洗：引号/书名号/「标题：」前缀/换行折叠/超长/空。
func TestCleanTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"干净直通", "容器重启排查", "容器重启排查"},
		{"首尾空白", "\n  磁盘告警处理 \t", "磁盘告警处理"},
		{"中文引号包裹", "“Nginx 日志分析”", "Nginx 日志分析"},
		{"ASCII 引号包裹", `"MySQL 慢查询` + `"`, "MySQL 慢查询"},
		{"书名号包裹", "《Redis 内存排查》", "Redis 内存排查"},
		{"标题前缀", "标题：K8s Pod 一直重启", "K8s Pod 一直重启"},
		{"标题前缀+引号（循环剥）", "标题：「Nginx 502 排查」", "Nginx 502 排查"},
		{"包裹符里再套前缀", "《标题：容器排查》", "容器排查"},
		{"内部换行与制表符折叠成单空格", "容器\n\t重启\t原因", "容器 重启 原因"},
		{"尾随句号", "磁盘清理。", "磁盘清理"},
		{"超长兜底截断 30 字", strings.Repeat("长", 40), strings.Repeat("长", 30) + "…"},
		{"仅全噪声（剥完只剩空格）", "标题：“”", ""}, // want 空串 = 期望报错，见下方断言
	}
	for _, c := range cases {
		got, err := cleanTitle(c.in)
		if c.want == "" {
			if err == nil {
				t.Fatalf("%s: cleanTitle(%q) = %q，want 报错（空标题不许写入）", c.name, c.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: cleanTitle(%q) err = %v", c.name, c.in, err)
		}
		if got != c.want {
			t.Fatalf("%s: cleanTitle(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// ==================== 自动总结：状态机与降级 ====================

// TestAutoSummarizeTitleWritesAITitle 占位态（来源为空串）→ 总结成功：写标题、来源置 'ai'、
// 发 session:title 帧（含 sessionId + title），且只读消息表不改它、不改 agent_id。
func TestAutoSummarizeTitleWritesAITitle(t *testing.T) {
	svc := newTestService(t)
	sess := newBareSession(t, svc)
	seedFirstTurn(t, svc, sess.ID, "容器为什么一直在重启", "内存超限被 OOMKilled，建议加限制")
	before := len(mustMessages(t, svc, sess.ID))

	fake := &scriptedLLM{reply: "标题：「容器重启排查」"}
	svc.newLLM = func(string, string) llm.Client { return fake }

	title, err := svc.autoSummarizeTitle(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("autoSummarizeTitle: %v", err)
	}
	if title != "容器重启排查" {
		t.Fatalf("title = %q, want 容器重启排查", title)
	}
	if fake.callCount() != 1 {
		t.Fatalf("LLM 调用次数 = %d, want 1", fake.callCount())
	}
	got, _ := svc.store.GetSession(sess.ID)
	if got.Title != "容器重启排查" || got.TitleSource != store.TitleSourceAI {
		t.Fatalf("落库结果 = %+v", got)
	}
	// 副作用审查：不改 agent_id、不碰消息表
	if got.AgentID != "bare" {
		t.Fatalf("总结不该动 agent_id: %q", got.AgentID)
	}
	if after := mustMessages(t, svc, sess.ID); len(after) != before {
		t.Fatalf("总结不该增删消息：before=%d after=%d", before, len(after))
	}
	// 帧内容
	frames := titleFramesHook()
	if len(frames) != 1 {
		t.Fatalf("session:title 帧数 = %d, want 1（frames=%v）", len(frames), frames)
	}
	if frames[0]["sessionId"] != sess.ID || frames[0]["title"] != "容器重启排查" {
		t.Fatalf("帧载荷 = %v, want sessionId + title", frames[0])
	}

	// 已是 'ai'：不再总结（不做每轮重总结），且不调用 LLM
	if _, err := svc.autoSummarizeTitle(context.Background(), sess.ID); err == nil {
		t.Fatal("ai 态再次总结应报错（不写入）")
	}
	if fake.callCount() != 1 {
		t.Fatalf("ai 态不该再调 LLM，calls = %d", fake.callCount())
	}
	if len(titleFramesHook()) != 1 {
		t.Fatalf("ai 态不该再发帧：%v", titleFramesHook())
	}
}

// TestAutoSummarizeSkipsManualTitle 手动改名后 AI 永不再覆盖：
// RenameSession 置 manual → 总结被闸门挡下（连 LLM 都不调），标题不变、不发帧。
func TestAutoSummarizeSkipsManualTitle(t *testing.T) {
	svc := newTestService(t)
	sess := newBareSession(t, svc)
	seedFirstTurn(t, svc, sess.ID, "查一下磁盘占用", "根分区 92%")
	if err := svc.RenameSession(sess.ID, "我自己起的名字"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	fake := &scriptedLLM{reply: "磁盘占用排查"}
	svc.newLLM = func(string, string) llm.Client { return fake }

	if _, err := svc.autoSummarizeTitle(context.Background(), sess.ID); err == nil {
		t.Fatal("manual 态总结应失败")
	}
	if fake.callCount() != 0 {
		t.Fatalf("manual 态不该调 LLM，calls = %d", fake.callCount())
	}
	got, _ := svc.store.GetSession(sess.ID)
	if got.Title != "我自己起的名字" || got.TitleSource != store.TitleSourceManual {
		t.Fatalf("manual 保护失效： %+v", got)
	}
	if frames := titleFramesHook(); len(frames) != 0 {
		t.Fatalf("manual 保护不该发帧：%v", frames)
	}

	// 存储层兜底同样成立（万一调用方绕过闸门直接写）
	if wrote, err := svc.store.UpdateSessionTitle(sess.ID, "硬写的AI标题"); err != nil || wrote {
		t.Fatalf("manual 态 UpdateSessionTitle = (%v, %v), want (false, nil)", wrote, err)
	}
}

// TestMaybeSummarizeTitleTriggersOnlyForPlaceholder 触发闸门：来源为空串才起 goroutine，
// 'ai'/'manual' 一律不起（不起 = 一次 LLM 调用都没有）。
func TestMaybeSummarizeTitleTriggersOnlyForPlaceholder(t *testing.T) {
	svc := newTestService(t)
	svc.disableAutoTitle = false
	fake := &scriptedLLM{reply: "标题"}
	svc.newLLM = func(string, string) llm.Client { return fake }

	sess := newBareSession(t, svc)
	seedFirstTurn(t, svc, sess.ID, "问题一", "回答一")

	// 'ai' 态
	if _, err := svc.autoSummarizeTitle(context.Background(), sess.ID); err != nil {
		t.Fatalf("首次总结: %v", err)
	}
	seedFirstTurn(t, svc, sess.ID, "问题二", "回答二") // 第二轮结束
	svc.maybeSummarizeTitle(sess.ID)
	time.Sleep(150 * time.Millisecond) // 该跳过是同步判断，留点时间给"不该发生"的异步调用
	if got := fake.callCount(); got != 1 {
		t.Fatalf("ai 态再触发产生了额外 LLM 调用：calls=%d, want 1", got)
	}

	// 'manual' 态
	other := newBareSession(t, svc)
	seedFirstTurn(t, svc, other.ID, "问题", "回答")
	if err := svc.RenameSession(other.ID, "锁定的名字"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	svc.maybeSummarizeTitle(other.ID)
	time.Sleep(150 * time.Millisecond)
	if got := fake.callCount(); got != 1 {
		t.Fatalf("manual 态再触发产生了额外 LLM 调用：calls=%d, want 1", got)
	}
	if frames := titleFramesHook(); len(frames) != 1 {
		t.Fatalf("帧数 = %d, want 1（只允许第一次总结发帧）：%v", len(frames), frames)
	}
}

// TestAutoSummarizeFailureIsSilent 静默降级：LLM 报错 / 未配置 / 输出全是噪声 / 超时
// ——一律只记日志，不改标题、不发帧、不 panic，占位的截断标题保留。
// （这些用例都从"占位态 = 标题还是新会话、来源空串"起步，不跑真回合。）
func TestAutoSummarizeFailureIsSilent(t *testing.T) {
	t.Run("LLM 报错", func(t *testing.T) {
		svc := newTestService(t)
		sess := newBareSession(t, svc)
		seedFirstTurn(t, svc, sess.ID, "问题", "回答")
		svc.newLLM = func(string, string) llm.Client { return &scriptedLLM{err: errors.New("连接被重置")} }
		if _, err := svc.autoSummarizeTitle(context.Background(), sess.ID); err == nil {
			t.Fatal("LLM 报错时应有错误")
		}
		assertDegraded(t, svc, sess.ID)
	})

	t.Run("未配置 LLM", func(t *testing.T) {
		svc := newTestService(t)
		sess := newBareSession(t, svc)
		seedFirstTurn(t, svc, sess.ID, "问题", "回答")
		if err := svc.store.SetSetting(settingLLMAPIKey, ""); err != nil {
			t.Fatalf("清掉 api key: %v", err)
		}
		if _, err := svc.autoSummarizeTitle(context.Background(), sess.ID); err == nil {
			t.Fatal("未配置 Key 时应有错误")
		}
		assertDegraded(t, svc, sess.ID)
	})

	t.Run("输出全是噪声", func(t *testing.T) {
		svc := newTestService(t)
		sess := newBareSession(t, svc)
		seedFirstTurn(t, svc, sess.ID, "问题", "回答")
		svc.newLLM = func(string, string) llm.Client { return &scriptedLLM{reply: "  “标题：”  "} }
		if _, err := svc.autoSummarizeTitle(context.Background(), sess.ID); err == nil {
			t.Fatal("噪声输出应有错误")
		}
		assertDegraded(t, svc, sess.ID)
	})

	t.Run("总结超时", func(t *testing.T) {
		withTitleSummaryTimeout(t, 50*time.Millisecond)
		svc := newTestService(t)
		sess := newBareSession(t, svc)
		seedFirstTurn(t, svc, sess.ID, "问题", "回答")
		svc.newLLM = func(string, string) llm.Client { return &scriptedLLM{blocking: true} }
		ctx, cancel := context.WithTimeout(context.Background(), titleSummaryTimeout)
		defer cancel()
		if _, err := svc.autoSummarizeTitle(ctx, sess.ID); err == nil {
			t.Fatal("超时应有错误")
		}
		assertDegraded(t, svc, sess.ID)
	})

	// goroutine 路径的 panic 兜底：崩在异步任务里也不能带崩进程、不能发帧
	t.Run("异步 panic 兜底", func(t *testing.T) {
		svc := newTestService(t)
		svc.disableAutoTitle = false
		sess := newBareSession(t, svc)
		seedFirstTurn(t, svc, sess.ID, "问题", "回答")
		svc.newLLM = func(string, string) llm.Client { return &scriptedLLM{panics: true} }
		svc.maybeSummarizeTitle(sess.ID)
		time.Sleep(200 * time.Millisecond) // 给 goroutine 崩完的时间（没兜住就会让测试进程挂掉）
		assertDegraded(t, svc, sess.ID)
	})
}

// ==================== 端到端接线 ====================

// TestSendMessageTriggersAutoTitle 真实接线：回合成功 → 占位截断标题先落库 →
// 异步总结覆盖成 AI 标题并推 session:title 帧（侧栏实时刷新的依据）。
func TestSendMessageTriggersAutoTitle(t *testing.T) {
	withTurnTimeout(t, 5*time.Second)
	svc := newTestService(t)
	svc.disableAutoTitle = false
	fake := &scriptedLLM{answer: "回答正文", reply: "长问题概括成的短标题"}
	svc.newLLM = func(string, string) llm.Client { return fake }
	sess := newBareSession(t, svc)

	if err := svc.SendMessage(context.Background(), sess.ID, strings.Repeat("问", 40)); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	// 总结前：占位标题 = 首条消息截断 30 字 + 省略号
	// 总结后（异步）：AI 标题
	got := waitSessionTitle(t, svc, sess.ID, func(s store.Session) bool {
		return s.TitleSource == store.TitleSourceAI
	})
	if got.Title != "长问题概括成的短标题" {
		t.Fatalf("AI 标题未覆盖占位标题： %+v", got)
	}
	frames := waitTitleFrames(t, 1)
	if frames[0]["sessionId"] != sess.ID || frames[0]["title"] != "长问题概括成的短标题" {
		t.Fatalf("帧载荷 = %v", frames[0])
	}
	if errs := doneErrsHook(); len(errs) != 0 {
		t.Fatalf("不该有 done 错误：%v", errs)
	}
	// 回合 + 总结各一次 LLM 调用
	if n := fake.callCount(); n != 2 {
		t.Fatalf("LLM 调用次数 = %d, want 2（回合 + 总结）", n)
	}
}

// TestPlaceholderTitleSurvivesManualRename 占位标题写入的 SQL 守卫：
// 手动改名（哪怕把名字改回"新会话"）后，回合再进来也写不进去。
func TestPlaceholderTitleSurvivesManualRename(t *testing.T) {
	withTurnTimeout(t, 5*time.Second)
	svc := newTestService(t)
	svc.newLLM = func(string, string) llm.Client { return &scriptedLLM{answer: "ok"} }
	sess := newBareSession(t, svc)
	if err := svc.RenameSession(sess.ID, "新会话"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := svc.SendMessage(context.Background(), sess.ID, "真实问题"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	got, _ := svc.store.GetSession(sess.ID)
	if got.Title != "新会话" || got.TitleSource != store.TitleSourceManual {
		t.Fatalf("manual 态被占位标题覆盖： %+v", got)
	}
}

// ==================== 小助手 ====================

// mustMessages 取会话消息（断言副作用用）。
func mustMessages(t *testing.T, svc *Service, sessionID string) []store.Message {
	t.Helper()
	msgs, err := svc.store.Messages(sessionID, 0)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	return msgs
}

// assertDegraded 静默降级的统一断言：标题仍是占位值（"新会话"，即从没被写过）、
// 来源仍是空串、一帧未发。
func assertDegraded(t *testing.T, svc *Service, id string) {
	t.Helper()
	got, err := svc.store.GetSession(id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Title != "新会话" || got.TitleSource != "" {
		t.Fatalf("降级路径不该改标题： %+v", got)
	}
	if frames := titleFramesHook(); len(frames) != 0 {
		t.Fatalf("降级路径不该发帧：%v", frames)
	}
}
