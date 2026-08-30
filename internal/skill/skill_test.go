package skill

import (
	"context"
	"encoding/json"
	"testing"
)

type fakeSkill struct{ name string }

func (f *fakeSkill) Name() string                                             { return f.name }
func (f *fakeSkill) Description() string                                      { return "fake" }
func (f *fakeSkill) Parameters() json.RawMessage                              { return json.RawMessage(`{}`) }
func (f *fakeSkill) Execute(context.Context, json.RawMessage) (string, error) { return "ok", nil }

func TestRegistryRequiresNamespace(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeSkill{name: "noNamespace"}); err == nil {
		t.Fatal("expected error for non-namespaced skill")
	}
	if err := r.Register(&fakeSkill{name: "ops_demo"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Register(&fakeSkill{name: "ops_demo"}); err == nil {
		t.Fatal("expected duplicate registration error")
	}
}

func TestRegistryResolve(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(&fakeSkill{name: "ops_a"})
	_ = r.Register(&fakeSkill{name: "test_b"})
	if _, err := r.Resolve([]string{"ops_a", "test_b"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := r.Resolve([]string{"ops_missing"}); err == nil {
		t.Fatal("expected unknown skill error")
	}
}

func TestRegistryAllSorted(t *testing.T) {
	r := NewRegistry()
	// 故意乱序注册：All() 必须按名字排序返回（map 遍历顺序随机，不能依赖）
	_ = r.Register(&fakeSkill{name: "test_b"})
	_ = r.Register(&fakeSkill{name: "ops_a"})
	_ = r.Register(&fakeSkill{name: "deploy_c"})
	all := r.All()
	if len(all) != 3 {
		t.Fatalf("expected 3 skills, got %d", len(all))
	}
	want := []string{"deploy_c", "ops_a", "test_b"}
	for i, s := range all {
		if s.Name() != want[i] {
			t.Fatalf("All()[%d] = %q, want %q", i, s.Name(), want[i])
		}
	}
}
