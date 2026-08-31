// Package skill 定义了 DevCraft 的"能力单元"抽象——整个插件化架构的核心。
//
// 设计要点：Skill 的元数据（名称/描述/参数 Schema）与 OpenAI function-calling
// 的 tool 定义一一对应，因此任何 Skill 都能直接挂载给任何 Agent，无需额外映射。
//
// Java 类比：Skill 接口 ≈ 一个 SPI 接口；Registry ≈ ServiceLoader + 注册中心；
// 每个具体技能 ≈ 策略模式的一个实现类。Go 的接口是"隐式实现"的——
// 不需要写 implements，只要方法签名齐全就自动满足接口（鸭子类型）。
package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"sort" // 排序：All() 按名字排序保证输出稳定（map 遍历顺序是随机的）
	"strings"
	"sync" // 标准库并发原语：这里用 RWMutex 读写锁保护注册表
)

// Skill 是自描述能力单元的接口（≈ Java interface）。
// 任何 struct 只要实现了这 4 个方法，就自动"是"一个 Skill，可注册进 Registry。
type Skill interface {
	// Name 返回带 namespace 的技能名，如 "ops_list_containers"。
	// namespace 防止不同域的技能重名（未来 test.run_suite、code.review 等）。
	Name() string
	// Description 写给 LLM 看的说明：模型靠它判断"何时该调用我"。
	// 这句话的质量直接决定路由准确性，务必写清使用场景。
	Description() string
	// Parameters 返回参数的 JSON Schema（原始 JSON）。
	// 模型按此生成调用参数；Agent 循环据此校验/解析参数。
	Parameters() json.RawMessage
	// Execute 执行技能，返回的文本会作为 tool 消息回填给 LLM。
	// args 是模型生成的参数 JSON。出错时返回 error，Agent 循环会把
	// 错误信息转述给用户而不是中断整个对话。
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry 技能注册表：全局能力池，按名字索引（≈ 注册中心 + ConcurrentHashMap）。
type Registry struct {
	mu     sync.RWMutex     // 读写锁：读多写少场景比互斥锁并发度高
	skills map[string]Skill // map ≈ HashMap<String, Skill>
}

// NewRegistry 创建空注册表。Go 的 map 必须初始化后才能写入（nil map 写入会 panic）。
func NewRegistry() *Registry {
	return &Registry{skills: map[string]Skill{}}
}

// Register 注册一个技能。规则：
//  1. 名字必须带 namespace（包含 "_"），强制领域划分。
//     注意：分隔符只能是下划线——LLM function-calling 协议要求工具名
//     匹配 ^[a-zA-Z0-9_-]+$，点号会被供应商以 400 拒绝。
//  2. 同名重复注册视为错误（防止静默覆盖）
func (r *Registry) Register(s Skill) error {
	name := s.Name()
	if !strings.Contains(name, "_") {
		return fmt.Errorf("skill name %q must be namespaced (domain_name)", name)
	}
	r.mu.Lock() // 写锁（≈ ReentrantLock.lock）；defer 保证返回时必定解锁
	defer r.mu.Unlock()
	if _, exists := r.skills[name]; exists { // map 取值第二返回值表示 key 是否存在
		return fmt.Errorf("skill %q already registered", name)
	}
	r.skills[name] = s
	return nil
}

// Get 按名字取技能；第二返回值表示是否存在（Go 惯用法，代替返回 null）。
func (r *Registry) Get(name string) (Skill, bool) {
	r.mu.RLock() // 读锁：多个读操作可并发
	defer r.mu.RUnlock()
	s, ok := r.skills[name]
	return s, ok
}

// Resolve 按给定名字列表批量解析技能（保持顺序）；遇到未注册的名字立即报错。
// Agent 装配时用它在启动阶段快速失败（fail-fast）。
func (r *Registry) Resolve(names []string) ([]Skill, error) {
	out := make([]Skill, 0, len(names)) // make 预分配容量，避免 append 反复扩容
	for _, n := range names {
		s, ok := r.Get(n)
		if !ok {
			return nil, fmt.Errorf("skill %q not registered", n)
		}
		out = append(out, s)
	}
	return out, nil
}

// Names 返回所有已注册技能名（调试/展示用）。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.skills))
	for n := range r.skills { // range map 只取 key 时写一个变量
		out = append(out, n)
	}
	return out
}

// All 返回所有已注册技能，按名字排序保证输出稳定（map 遍历顺序随机，
// 展示类调用方——如设置页技能管理——需要确定性顺序）。
// 返回的是接口值快照，调用方只读使用；注册表后续增删不影响已取出的切片。
func (r *Registry) All() []Skill {
	names := r.Names()
	sort.Strings(names)
	out := make([]Skill, 0, len(names))
	for _, n := range names {
		s, _ := r.Get(n) // 刚从 Names() 取到的名字必然存在
		out = append(out, s)
	}
	return out
}

// IsBuiltin 返回某技能是否内置（技能分类标记的唯一来源）。
// 本期技能全部是编译期注册，故已注册即内置；该方法作为分类的统一入口而存在——
// 下期自定义技能落地时在此按注册来源判定，消费方（前端分组展示）始终只认
// 这个标记，不按名字硬编码判断。
func (r *Registry) IsBuiltin(name string) bool {
	_, ok := r.Get(name)
	return ok
}
