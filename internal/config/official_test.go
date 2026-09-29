package config

import "testing"

func TestIsOfficialModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-5.6-luna", true},
		{"gpt-5.6-sol", true},
		{"codex-1", true},
		{"codex-mini", true},
		{"o1", true},
		{"o3", true},
		{"o4", true},
		{"o1-mini", true},
		{"o3-mini", true},
		{"o4-mini", true},
		{"o2", false},            // 非官方系列
		{"openai", false},        // 前缀匹配但不完整
		{"deepseek-chat", false}, // 第三方模型
		{"deepseek-v4-1-flash-260910", false},
		{"claude-sonnet-4-20250514", false},
		{"gemini-3.8-flash", false},
	}
	for _, c := range cases {
		if got := IsOfficialModel(c.model); got != c.want {
			t.Errorf("IsOfficialModel(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}

func TestResolveUpstreams_OfficialModel(t *testing.T) {
	store := &Store{
		path: "/tmp/test-config.json",
		cfg: &Config{
			Listen: Listen{Host: "127.0.0.1", Port: 8318},
			Upstreams: []Upstream{
				{Name: "official", Protocol: "openai-official", BaseURL: "https://chatgpt.com/backend-api/codex", Models: []string{}},
				{Name: "volcengine", Protocol: "openai", BaseURL: "https://ark.cn-beijing.volces.com/api/v3", Models: []string{"deepseek-v4-1-flash-260910"}},
			},
			Routes: map[string]string{},
		},
	}

	// 官方模型应路由到 openai-official 上游
	targets := store.ResolveUpstreams("gpt-5.6-luna")
	if len(targets) != 1 || targets[0].Upstream.Name != "official" {
		t.Fatalf("官方模型路由错误: %v", targets)
	}

	// 第三方模型应路由到 volcengine
	targets = store.ResolveUpstreams("deepseek-v4-1-flash-260910")
	if len(targets) != 1 || targets[0].Upstream.Name != "volcengine" {
		t.Fatalf("第三方模型路由错误: %v", targets)
	}

	// 没有官方上游时，官方模型走普通路由
	store2 := &Store{
		path: "/tmp/test-config2.json",
		cfg: &Config{
			Listen: Listen{Host: "127.0.0.1", Port: 8318},
			Upstreams: []Upstream{
				{Name: "volcengine", Protocol: "openai", BaseURL: "https://ark.cn-beijing.volces.com/api/v3", Models: []string{"deepseek-v4-1-flash-260910"}},
			},
			Routes: map[string]string{},
		},
	}
	targets = store2.ResolveUpstreams("gpt-5.6-luna")
	if len(targets) != 1 || targets[0].Upstream.Name != "volcengine" {
		t.Fatalf("无官方上游时应走兜底路由: %v", targets)
	}
}

func TestIsBuiltinUpstream(t *testing.T) {
	builtin := Upstream{Name: "openai-official", Protocol: ProtocolOfficial}
	normal := Upstream{Name: "volcengine", Protocol: "openai"}
	if !IsBuiltinUpstream(builtin) {
		t.Error("官方透传上游应判定为内置")
	}
	if IsBuiltinUpstream(normal) {
		t.Error("普通上游不应判定为内置")
	}
}

// TestMergeBuiltinUpstreams 内置上游只读：用户改动被忽略、被删除时被补回
func TestMergeBuiltinUpstreams(t *testing.T) {
	current := []Upstream{
		{Name: "openai-official", Protocol: ProtocolOfficial,
			BaseURL: "https://chatgpt.com/backend-api/codex", Models: []string{"gpt-5.6-luna"}},
		{Name: "volcengine", Protocol: "openai", BaseURL: "https://ark.example.com/api/v3",
			APIKey: "k", Models: []string{"deepseek-v4-1-flash-260910"}},
	}

	t.Run("用户改动内置上游字段被忽略", func(t *testing.T) {
		submitted := []Upstream{
			{Name: "openai-official", Protocol: ProtocolOfficial,
				BaseURL: "https://evil.example.com", APIKey: "leaked", Models: []string{"hacked"}},
			{Name: "volcengine", Protocol: "openai", BaseURL: "https://ark.example.com/api/v3",
				APIKey: "k", Models: []string{"deepseek-v4-1-flash-260910"}},
		}
		out := mergeBuiltinUpstreams(submitted, current)
		var got *Upstream
		for i := range out {
			if out[i].Name == "openai-official" {
				got = &out[i]
			}
		}
		if got == nil {
			t.Fatal("内置上游丢失")
		}
		if got.BaseURL != "https://chatgpt.com/backend-api/codex" {
			t.Errorf("内置上游 BaseURL 被篡改: %q", got.BaseURL)
		}
		if got.APIKey != "" {
			t.Errorf("内置上游不应被写入 API Key: %q", got.APIKey)
		}
		if len(got.Models) != 1 || got.Models[0] != "gpt-5.6-luna" {
			t.Errorf("内置上游模型列表被篡改: %v", got.Models)
		}
	})

	t.Run("内置上游被删除时补回", func(t *testing.T) {
		submitted := []Upstream{
			{Name: "volcengine", Protocol: "openai", BaseURL: "https://ark.example.com/api/v3",
				APIKey: "k", Models: []string{"deepseek-v4-1-flash-260910"}},
		}
		out := mergeBuiltinUpstreams(submitted, current)
		if len(out) != 2 {
			t.Fatalf("内置上游应被补回, 实际 %d 条: %+v", len(out), out)
		}
		found := false
		for _, up := range out {
			if up.Name == "openai-official" {
				found = true
			}
		}
		if !found {
			t.Error("内置上游未被补回")
		}
	})

	t.Run("无内置上游时原样返回", func(t *testing.T) {
		noBuiltin := []Upstream{current[1]}
		out := mergeBuiltinUpstreams([]Upstream{current[1]}, noBuiltin)
		if len(out) != 1 || out[0].Name != "volcengine" {
			t.Errorf("无内置上游时应原样返回, got %+v", out)
		}
	})
}
