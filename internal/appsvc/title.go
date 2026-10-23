// 会话标题的两条写入路径：AI 异步总结 + 手动改名。
//
// 标题生命周期（状态机落在 sessions.title_source 列上，SQL 条件即守卫）：
//
//	占位态（title_source = 空串，标题是首条消息截断）
//	  --AI 总结成功-->  'ai'（已是 'ai' 不再触发第二次总结：无每轮重总结）
//	  --手动改名────>  'manual'（锁定，AI 永不再覆盖）
//
// 自动总结的降级纪律：LLM 未配置 / 调用失败 / 输出是噪声 → 只 slog，不发帧、
// 不弹窗、不改标题，用户看到的仍是文本截断占位标题（静默降级）。
// 手动改名是用户显式动作，失败必须给中文错误（走 RPC 返回值）。
package appsvc

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"DevCraft/internal/llm"
)

// EventSessionTitle AI 总结出新标题后推送的事件帧名（复用 chat 流通道，
// 前端按 payload.sessionId 只更新侧栏对应会话项的标题，不整表重拉）。
const EventSessionTitle = "session:title"

// 标题写入的参数（都是包级变量或常量；超时用 var 是照 turnTimeout 的先例，便于单测调小）。
var titleSummaryTimeout = 30 * time.Second

const (
	// titleInputChars 送给 LLM 的每条消息截断长度（提问 + 回答各取前这么多字，控制 token 成本）。
	titleInputChars = 500
	// titleMaxRunes 最终标题的兜底长度（与占位标题、手动改名同款语义）。
	titleMaxRunes = 30
	// titlePrompt 要求"只输出标题本身"：模型很爱加引号/前缀，噪声由 cleanTitle 剥，
	// 但先把格式要求写清楚能减少噪声概率。
	titlePrompt = "你是会话标题生成器。阅读下面一轮对话（用户提问与助手回答），" +
		"用不超过 20 个字概括这次会话的主题。" +
		"只输出标题本身：不要引号、不要书名号、不要「标题：」这类前缀、不要句号、不要换行、不要解释。"
)

// maybeSummarizeTitle 回合成功收尾后尝试触发一次异步标题总结（不阻塞对话）。
// 触发条件：该会话 title_source 仍是空串（占位标题）。其他状态一律跳过：
// 'ai' = 已经总结过（不做每轮重总结）；'manual' = 用户改过名，永久锁定。
// 同一会话只允许一个总结任务在飞（in-flight 去重，见 Service.titleGen）。
func (s *Service) maybeSummarizeTitle(sessionID string) {
	if s.disableAutoTitle {
		return
	}
	sess, err := s.store.GetSession(sessionID)
	if err != nil {
		slog.Warn("标题总结跳过：会话读取失败", "session", sessionID, "err", err)
		return
	}
	if sess.TitleSource != "" {
		return
	}
	s.titleGenMu.Lock()
	if _, busy := s.titleGen[sessionID]; busy {
		s.titleGenMu.Unlock()
		return
	}
	s.titleGen[sessionID] = struct{}{}
	s.titleGenMu.Unlock()

	go func() {
		defer func() {
			s.titleGenMu.Lock()
			delete(s.titleGen, sessionID)
			s.titleGenMu.Unlock()
		}()
		// panic 兜底：异步总结绝不能把进程带崩（形状照 executeApproval）
		defer func() {
			if r := recover(); r != nil {
				slog.Error("会话标题总结 panic", "session", sessionID, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), titleSummaryTimeout)
		defer cancel()
		if _, err := s.autoSummarizeTitle(ctx, sessionID); err != nil {
			// 静默降级：保留文本截断占位标题，只留日志（不发帧、不打扰用户）
			slog.Warn("会话标题自动总结失败", "session", sessionID, "err", err)
		}
	}()
}

// autoSummarizeTitle 同步执行一次"总结 + 落库 + 发帧"，返回写入的新标题。
// 拆成同步函数是为了单测能直接驱动整条链路（goroutine 只是它外面的一层壳）。
// 失败路径全部返回 error，由调用方决定记日志还是报错；本函数内部不发帧、不弹窗。
func (s *Service) autoSummarizeTitle(ctx context.Context, sessionID string) (string, error) {
	sess, err := s.store.GetSession(sessionID)
	if err != nil {
		return "", err
	}
	if sess.TitleSource != "" {
		return "", fmt.Errorf("会话标题已有来源（%s），不再自动总结", sess.TitleSource)
	}
	question, answer, err := s.firstExchange(sessionID)
	if err != nil {
		return "", err
	}
	apiKey, baseURL, model, err := s.llmConfig()
	if err != nil {
		return "", err
	}
	if model == "" {
		return "", fmt.Errorf("尚未配置模型名称")
	}
	raw, err := s.completeText(ctx, s.newLLM(apiKey, baseURL), model, []llm.Message{
		{Role: llm.RoleSystem, Content: titlePrompt},
		{Role: llm.RoleUser, Content: fmt.Sprintf("用户提问：%s\n助手回答：%s",
			truncate(question, titleInputChars), truncate(answer, titleInputChars))},
	})
	if err != nil {
		return "", err
	}
	title, err := cleanTitle(raw)
	if err != nil {
		return "", err
	}
	// 定向 UPDATE：title_source='manual' 的行被 SQL 条件挡下（手动改名保护不靠自律）
	wrote, err := s.store.UpdateSessionTitle(sessionID, title)
	if err != nil {
		return "", err
	}
	if !wrote {
		return "", fmt.Errorf("会话标题已被手动改名，自动总结结果不写入")
	}
	slog.Info("会话标题已由 AI 总结", "session", sessionID, "title", title)
	s.emit(EventSessionTitle, map[string]any{"sessionId": sessionID, "title": title})
	return title, nil
}

// firstExchange 取会话首轮问答（首条 user 消息 + 首条 assistant 回复）。
// 只读消息表，不写。缺任一条都算无法总结（返回中文 error 走静默降级）。
func (s *Service) firstExchange(sessionID string) (question, answer string, err error) {
	msgs, err := s.store.Messages(sessionID, 0)
	if err != nil {
		return "", "", err
	}
	for _, m := range msgs {
		switch m.Role {
		case "user":
			if question == "" {
				question = m.Content
			}
		case "assistant":
			if answer == "" {
				answer = m.Content
			}
		}
	}
	if question == "" || answer == "" {
		return "", "", fmt.Errorf("会话缺少首轮问答，无法总结标题")
	}
	return question, answer, nil
}

// completeText 一次"非流式"补全：复用现有 ChatStream，在 appsvc 侧把增量聚合成全文。
// 刻意不给 llm.Client 加 Complete 方法——那会牵动 openai.go 的实现面与三处测试 fake，
// 属于把内部小需求外溢成接口变更。聚合语义与 TestLLM 同款（ChatStream 的 onDelta 可为 nil，
// 但流式实现通常会回调；两者都取：优先聚合结果，兜底取汇总消息）。
func (s *Service) completeText(ctx context.Context, cli llm.Client, model string, msgs []llm.Message) (string, error) {
	var sb strings.Builder
	resp, err := cli.ChatStream(ctx, model, msgs, nil, func(delta string) error {
		sb.WriteString(delta)
		return nil
	})
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(sb.String())
	if out == "" && resp != nil {
		out = strings.TrimSpace(resp.Message.Content)
	}
	if out == "" {
		return "", fmt.Errorf("模型未返回任何内容")
	}
	return out, nil
}

// cleanTitle 清洗模型输出的标题：剥「标题：」这类前缀、剥首尾成对的引号/书名号、
// 换行与制表符折叠成单个空格、去掉尾随标点，最后兜底截断 30 字。
// 清洗后为空（模型只吐了个引号之类）返回 error——宁可不写，也不要写进垃圾标题。
func cleanTitle(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	// 前缀噪声与包裹符交替剥到不动点：模型会写成 "标题：《xxx》"，也可能写成
	// "《标题：xxx》"——只按单一顺序剥一轮会留下半截噪声。
	for {
		before := s
		s = strings.TrimSpace(stripTitlePrefix(s))
		s = strings.TrimSpace(stripWrappers(s))
		if s == before {
			break
		}
	}
	// 内部换行/制表符/全角空格 → 单空格
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r' || r == '\t' || r == ' ' || r == '\v' || r == '\f' || r == 0x3000
	}), " ")
	s = strings.TrimSpace(s)
	// 尾随句号/叹号/问号等标点噪声（标题惯例不带）
	s = strings.TrimRight(s, "。．.!！？?；;：:，,、")
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("标题总结结果为空")
	}
	return truncate(s, titleMaxRunes), nil
}

// stripTitlePrefix 剥「标题：」「题目:」「名称：」这类前缀（模型爱带的说明性前缀）。
func stripTitlePrefix(s string) string {
	for _, p := range []string{"标题：", "标题:", "题目：", "题目:", "名称：", "名称:", "Title:", "TITLE:"} {
		if strings.HasPrefix(s, p) {
			return strings.TrimSpace(s[len(p):])
		}
	}
	return s
}

// stripWrappers 剥首尾成对的包裹符（引号 / 书名号 / 直角引号 / 方括号）。
func stripWrappers(s string) string {
	pairs := [][2]string{
		{`"`, `"`}, {"'", "'"}, {`“`, `”`}, {`‘`, `’`},
		{`《`, `》`}, {`〈`, `〉`}, {`「`, `」`}, {`『`, `』`}, {`【`, `】`}, {`[`, `]`},
	}
	for _, p := range pairs {
		if strings.HasPrefix(s, p[0]) && strings.HasSuffix(s, p[1]) && len(s) >= 2*len(p[0]) {
			return strings.TrimSuffix(strings.TrimPrefix(s, p[0]), p[1])
		}
	}
	return s
}

// RenameSession 手动改名（前端 ✏️ 行内编辑的落点）：
// 校验非空、截断 30 字（复用 truncate，与占位标题同款语义），
// 定向写入 title + title_source='manual'——manual 之后 AI 总结永不再覆盖。
func (s *Service) RenameSession(id, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("会话标题不能为空")
	}
	title = truncate(title, titleMaxRunes)
	wrote, err := s.store.SetSessionTitleManual(id, title)
	if err != nil {
		return err
	}
	if !wrote {
		return fmt.Errorf("会话不存在或已被删除")
	}
	slog.Info("会话已手动改名", "session", id, "title", title)
	return nil
}
