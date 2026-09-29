package config

import "testing"

func TestResolveUpstreams_ModelRewrite(t *testing.T) {
	store := &Store{}
	store.SetForTest(&Config{
		Upstreams: []Upstream{
			{Name: "deepseek", Protocol: "openai", Models: []string{"deepseek-chat"}},
		},
		Routes: map[string]string{
			"claude-sonnet-4": "deepseek:deepseek-chat",
			"raw-route":       "deepseek",
		},
	})

	cands := store.ResolveUpstreams("claude-sonnet-4")
	if len(cands) != 1 {
		t.Fatalf("应返回 1 个候选，实际 %d", len(cands))
	}
	if cands[0].Upstream.Name != "deepseek" {
		t.Errorf("上游解析失败: %v", cands[0].Upstream.Name)
	}
	if cands[0].TargetModel != "deepseek-chat" {
		t.Errorf("目标模型改写失败: %v", cands[0].TargetModel)
	}

	cands2 := store.ResolveUpstreams("raw-route")
	if cands2[0].TargetModel != "raw-route" {
		t.Errorf("无冒号时应使用原模型名: %v", cands2[0].TargetModel)
	}
}

func TestResolveUpstreams_AutoMatchAndFailover(t *testing.T) {
	store := &Store{}
	store.SetForTest(&Config{
		Upstreams: []Upstream{
			{Name: "a", Protocol: "openai", Models: []string{"m1"}},
			{Name: "b", Protocol: "openai", Models: []string{"m1"}},
		},
	})
	cands := store.ResolveUpstreams("m1")
	if len(cands) != 2 {
		t.Fatalf("自动匹配应返回 2 个候选: %d", len(cands))
	}
	if cands[0].Upstream.Name != "a" || cands[1].Upstream.Name != "b" {
		t.Errorf("顺序错误: %v", cands)
	}
}

func TestResolveUpstream_BackwardCompat(t *testing.T) {
	store := &Store{}
	store.SetForTest(&Config{
		Upstreams: []Upstream{{Name: "only", Protocol: "openai", Models: []string{"x"}}},
	})
	up := store.ResolveUpstream("x")
	if up == nil || up.Name != "only" {
		t.Errorf("单上游兜底失败")
	}
}
