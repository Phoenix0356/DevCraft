package appsvc

import (
	"encoding/json"
	"strings"
	"testing"

	"DevCraft/internal/skill/deploy" // 部署技能（测试里按生产方式注册）
	"DevCraft/internal/skill/ops"    // 运维三技能（同上）
	"DevCraft/internal/store"
)

// registerProdSkills 按 app.go buildService 的同样方式把生产技能注册进测试
// Service 的注册表（运维三技能 + 部署技能），保证 ListSkills 测试面对的是
// 真实技能集合而不是假数据。
// 依赖方向安全：ops/deploy 都不反向依赖 appsvc（deploy 只定义小接口，
// 这里把 svc 作为实现注入，与生产一致）。
func registerProdSkills(t *testing.T, svc *Service) {
	t.Helper()
	if err := ops.Register(svc.skills, svc.docker, svc.DockerEndpoint); err != nil {
		t.Fatalf("register ops skills: %v", err)
	}
	if err := deploy.Register(svc.skills, svc, svc); err != nil {
		t.Fatalf("register deploy skill: %v", err)
	}
}

// TestListSkills 设置页技能管理的数据源契约：
// 全量（4 个生产技能）、有序（按名字排序）、描述与参数 Schema 完整。
func TestListSkills(t *testing.T) {
	svc := newTestService(t)
	registerProdSkills(t, svc)

	got := svc.ListSkills()
	if len(got) != 4 {
		t.Fatalf("expected 4 skills, got %d: %+v", len(got), got)
	}
	// 名字按字典序稳定输出（前端列表顺序依赖它）
	wantNames := []string{"deploy_run_flow", "ops_analyze_logs", "ops_container_stats", "ops_list_containers"}
	for i, s := range got {
		if s.Name != wantNames[i] {
			t.Fatalf("skill[%d].Name = %q, want %q", i, s.Name, wantNames[i])
		}
		if strings.TrimSpace(s.Description) == "" {
			t.Errorf("skill %s: description empty", s.Name)
		}
		// 参数 Schema 必须是合法 JSON 对象且含 properties（前端按此渲染参数表）
		var schema map[string]any
		if err := json.Unmarshal(s.Parameters, &schema); err != nil {
			t.Errorf("skill %s: parameters not valid JSON: %v", s.Name, err)
			continue
		}
		if _, ok := schema["properties"]; !ok {
			t.Errorf("skill %s: parameters schema missing properties", s.Name)
		}
	}
}

// TestListSkillsDeployDescriptionDynamic 部署技能的描述是动态拼接的流程清单：
// 新建流程后再次 ListSkills，描述里必须出现该流程（实时值，无需重启）。
func TestListSkillsDeployDescriptionDynamic(t *testing.T) {
	svc := newTestService(t)
	registerProdSkills(t, svc)

	deployDesc := func(t *testing.T) string {
		t.Helper()
		for _, s := range svc.ListSkills() {
			if s.Name == deploy.SkillName {
				return s.Description
			}
		}
		t.Fatal("deploy_run_flow not in ListSkills result")
		return ""
	}

	if d := deployDesc(t); !strings.Contains(d, "当前没有已定义的部署流程") {
		t.Fatalf("empty-registry description unexpected: %s", d)
	}

	// 建一个流程（走真实保存路径，含校验）
	err := svc.SaveDeployFlow(store.DeployFlow{
		Name:   "web-deploy",
		Target: store.TargetLocal,
		Steps:  []string{"docker restart web"},
	})
	if err != nil {
		t.Fatalf("save flow: %v", err)
	}
	if d := deployDesc(t); !strings.Contains(d, "web-deploy") {
		t.Fatalf("description should list the new flow, got: %s", d)
	}
}
