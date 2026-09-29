package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/config"
)

// isolateCodexHome 把 CODEX_HOME 指到空临时目录，切断对真实 ~/.codex 的依赖。
// 否则本机若存在 models_cache.json / auth.json，用例会走缓存分支而根本不打网络，
// 测试结果随机器环境漂移。
func isolateCodexHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	return dir
}

// testStore 构造一个仅含内置官方上游的配置（本文件用例不依赖上游路由）
func testStore() *config.Store {
	store := &config.Store{}
	store.SetForTest(&config.Config{
		Listen: config.Listen{Host: "127.0.0.1", Port: 8318},
		Upstreams: []config.Upstream{
			{Name: "openai-official", Protocol: config.ProtocolOfficial,
				BaseURL: "https://chatgpt.com/backend-api/codex", Models: []string{}},
		},
		Routes: map[string]string{},
	})
	return store
}

// TestFetchOfficialModels_OK 官方端点可用时应解析出模型清单
func TestFetchOfficialModels_OK(t *testing.T) {
	isolateCodexHome(t)

	var gotAuth, gotAccount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("官方模型端点路径错误: %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("ChatGPT-Account-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.6-luna"},{"id":"gpt-5.6-sol"}]}`))
	}))
	defer srv.Close()

	g := New(testStore())
	models, err := g.fetchOfficialModels(context.Background(), srv.URL, "Bearer token-abc")
	if err != nil {
		t.Fatalf("拉取不应失败: %v", err)
	}
	if len(models) != 2 || models[0] != "gpt-5.6-luna" {
		t.Fatalf("模型清单解析错误: %v", models)
	}
	if gotAuth != "Bearer token-abc" {
		t.Errorf("未透传客户端凭证: %q", gotAuth)
	}
	_ = gotAccount
}

// TestFetchOfficialModels_Failure 失败时必须返回原因（禁止吞异常），且不 panic、不阻塞
func TestFetchOfficialModels_Failure(t *testing.T) {
	isolateCodexHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer srv.Close()

	g := New(testStore())

	t.Run("上游非 200 时给出状态码与响应片段", func(t *testing.T) {
		models, err := g.fetchOfficialModels(context.Background(), srv.URL, "Bearer bad")
		if err == nil {
			t.Fatal("失败应返回错误，不能静默吞掉")
		}
		if len(models) != 0 {
			t.Errorf("失败时不应返回模型, got %v", models)
		}
		if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "forbidden") {
			t.Errorf("错误信息应含状态码与响应片段, got %q", err.Error())
		}
	})

	t.Run("无凭证时明确说明原因", func(t *testing.T) {
		_, err := g.fetchOfficialModels(context.Background(), srv.URL, "")
		if err == nil || !strings.Contains(err.Error(), "凭证") {
			t.Errorf("无凭证应说明原因, got %v", err)
		}
	})

	t.Run("上游不可达时给出网络错误", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := dead.URL
		dead.Close() // 立即关闭，制造连接失败
		if _, err := g.fetchOfficialModels(context.Background(), url, "Bearer t"); err == nil {
			t.Error("连接失败应返回错误")
		}
	})
}

// TestFetchOfficialModels_Empty 200 但清单为空也要报错，避免误以为"刷新成功"
func TestFetchOfficialModels_Empty(t *testing.T) {
	isolateCodexHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	g := New(testStore())
	if _, err := g.fetchOfficialModels(context.Background(), srv.URL, "Bearer t"); err == nil {
		t.Error("空清单应返回错误")
	}
}

// TestDoUpstream_HeaderPolicy 官方轨整份透传客户端头；第三方上游只用自己的 Key
func TestDoUpstream_HeaderPolicy(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	src := http.Header{}
	src.Set("Authorization", "Bearer client-token")
	src.Set("ChatGPT-Account-Id", "acc-123")
	src.Set("Originator", "codex_cli_rs")
	src.Set("Session-Id", "sess-1")
	src.Set("Connection", "keep-alive") // 逐跳头，必须丢掉

	g := New(testStore())

	t.Run("官方轨透传客户端全部非逐跳头", func(t *testing.T) {
		got = nil
		up := config.Upstream{Name: "official", Protocol: config.ProtocolOfficial, BaseURL: srv.URL}
		resp, err := g.doUpstream(context.Background(), up, srv.URL+"/responses", []byte(`{}`), src)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		resp.Body.Close()

		// 官方端点靠这些头识别请求，丢一个就可能被判未授权
		for _, h := range []string{"Authorization", "ChatGPT-Account-Id", "Originator", "Session-Id"} {
			if got.Get(h) == "" {
				t.Errorf("官方轨应透传 %s, 实际头: %v", h, got)
			}
		}
		if _, ok := got["Connection"]; ok {
			t.Error("逐跳头 Connection 不应透传")
		}
	})

	t.Run("第三方上游不泄露客户端凭证", func(t *testing.T) {
		got = nil
		up := config.Upstream{Name: "third", Protocol: "openai", BaseURL: srv.URL, APIKey: "upstream-key"}
		resp, err := g.doUpstream(context.Background(), up, srv.URL+"/chat/completions", []byte(`{}`), src)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		resp.Body.Close()

		if got.Get("Authorization") != "Bearer upstream-key" {
			t.Errorf("应注入上游自己的 Key, got %q", got.Get("Authorization"))
		}
		if got.Get("ChatGPT-Account-Id") != "" {
			t.Error("客户端凭证不应泄露给第三方上游")
		}
	})
}

// TestLocalOfficialModelsCache 本机 Codex 缓存的读取与 hide 过滤
func TestLocalOfficialModelsCache(t *testing.T) {
	dir := isolateCodexHome(t)
	body := `{"fetched_at":"` + time.Now().UTC().Format(time.RFC3339) + `","models":[` +
		`{"slug":"gpt-6-luna","visibility":"list"},` +
		`{"slug":"codex-auto-review","visibility":"hide"}]}`
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("写缓存失败: %v", err)
	}

	models, at := localOfficialModelsCache()
	if len(models) != 1 || models[0] != "gpt-6-luna" {
		t.Fatalf("应只返回可见模型, got %v", models)
	}
	if at.IsZero() {
		t.Error("应解析出 fetched_at")
	}

	t.Run("文件不存在时返回空", func(t *testing.T) {
		empty := t.TempDir()
		t.Setenv("CODEX_HOME", empty)
		if got, _ := localOfficialModelsCache(); len(got) != 0 {
			t.Errorf("无缓存应返回空, got %v", got)
		}
	})
}

// TestFetchOfficialModels_PrefersLocalCache 有缓存时优先用缓存，不再依赖网络与凭证
func TestFetchOfficialModels_PrefersLocalCache(t *testing.T) {
	dir := isolateCodexHome(t)
	body := `{"fetched_at":"` + time.Now().UTC().Format(time.RFC3339) + `",` +
		`"models":[{"slug":"gpt-6-luna","visibility":"list"}]}`
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("写缓存失败: %v", err)
	}

	// 上游故意不可达：缓存优先的话根本不会去连它
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("有本机缓存时不该再打网络")
	}))
	defer srv.Close()

	g := New(testStore())
	models, err := g.fetchOfficialModels(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("有缓存时不应报错: %v", err)
	}
	if len(models) != 1 || models[0] != "gpt-6-luna" {
		t.Errorf("应优先返回缓存内容, got %v", models)
	}
}

// TestFetchOfficialModels_StaleCacheFallsBack 缓存过期且网络不可达时，仍要保住缓存内容
func TestFetchOfficialModels_StaleCacheFallsBack(t *testing.T) {
	dir := isolateCodexHome(t)
	stale := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	body := `{"fetched_at":"` + stale + `","models":[{"slug":"gpt-old-luna","visibility":"list"}]}`
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("写缓存失败: %v", err)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()

	g := New(testStore())
	models, err := g.fetchOfficialModels(context.Background(), url, "")
	if err != nil {
		t.Fatalf("缓存兜底时不应报错: %v", err)
	}
	if len(models) != 1 || models[0] != "gpt-old-luna" {
		t.Errorf("网络失败应退回缓存, got %v", models)
	}
}

// TestProxyUsedForOfficialFetch 配置了统一代理后，官方拉取必须真正走代理
func TestProxyUsedForOfficialFetch(t *testing.T) {
	isolateCodexHome(t)

	var hitProxy bool
	var gotURI string
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitProxy = true
		gotURI = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"slug":"gpt-5.6-luna"}]}`))
	}))
	defer proxySrv.Close()

	store := testStore()
	cfg := store.Get()
	cfg.Proxy = config.Proxy{URL: proxySrv.URL}
	store.SetForTest(cfg)

	g := New(store)
	models, err := g.fetchOfficialModels(context.Background(), "http://chatgpt.com/backend-api/codex", "Bearer t")
	if err != nil {
		t.Fatalf("走代理拉取不应失败: %v", err)
	}
	if !hitProxy {
		t.Fatal("请求没有经过统一代理")
	}
	if !strings.HasPrefix(gotURI, "http://") {
		t.Errorf("走代理时请求应为绝对 URI, got %q", gotURI)
	}
	if len(models) != 1 || models[0] != "gpt-5.6-luna" {
		t.Errorf("模型解析错误: %v", models)
	}
}

// TestParseModelList 兼容两种返回形态
func TestParseModelList(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		wantN int
		want0 string
	}{
		{"data 包裹", `{"data":[{"id":"a"},{"id":"b"},{"id":"c"}]}`, 3, "a"},
		{"裸数组", `[{"id":"x"},{"id":"y"}]`, 2, "x"},
		{"slug 字段", `{"data":[{"slug":"m1"}]}`, 1, "m1"},
		// 实测：ChatGPT 官方 codex/models 用的就是 models 包裹 + slug
		{"models 包裹", `{"models":[{"slug":"gpt-5.6-luna"},{"slug":"gpt-5.6-sol"}]}`, 2, "gpt-5.6-luna"},
		{"models 空优先回退 data", `{"data":[{"id":"a"}],"models":[]}`, 1, "a"},
		// 官方标记为 hide 的是内部模型，不该出现在选择器里
		{"过滤 hide 模型", `{"models":[{"slug":"a","visibility":"list"},{"slug":"hide-me","visibility":"hide"}]}`, 1, "a"},
		{"全部 hide 则为空", `{"models":[{"slug":"x","visibility":"hide"}]}`, 0, ""},
		{"非法 JSON", `not json`, 0, ""},
		{"空列表", `{"data":[]}`, 0, ""},
	}
	for _, c := range cases {
		got := parseModelList([]byte(c.body))
		if len(got) != c.wantN {
			t.Errorf("%s: 期望 %d 个, 实际 %d (%v)", c.name, c.wantN, len(got), got)
			continue
		}
		if c.wantN > 0 && got[0] != c.want0 {
			t.Errorf("%s: 期望首个 %q, 实际 %q", c.name, c.want0, got[0])
		}
	}
}

// TestBearerToken 从 Authorization 头提取裸 token
func TestBearerToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Bearer abc123", "abc123"},
		{"bearer abc123", "abc123"}, // 大小写不敏感
		{"abc123", "abc123"},        // 无前缀
		{"", ""},
	}
	for _, c := range cases {
		if got := bearerToken(c.in); got != c.want {
			t.Errorf("bearerToken(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestUpstreamModelsURL 端点拼接：不产生双斜杠，且必须带 client_version 参数
func TestUpstreamModelsURL(t *testing.T) {
	want := "https://chatgpt.com/backend-api/codex/models?client_version=" + officialClientVersion
	if got := officialModelsURL("https://chatgpt.com/backend-api/codex/"); got != want {
		t.Errorf("尾斜杠处理错误:\n got=%q\nwant=%q", got, want)
	}
	if got := officialModelsURL("https://chatgpt.com/backend-api/codex"); got != want {
		t.Errorf("拼接错误:\n got=%q\nwant=%q", got, want)
	}
}

// TestProxyFor 统一出站代理的生效与绕过规则
func TestProxyFor(t *testing.T) {
	p := config.Proxy{URL: "http://127.0.0.1:7897", NoProxy: "localhost,127.0.0.1"}

	t.Run("普通域名走代理", func(t *testing.T) {
		u, err := p.ProxyFor("chatgpt.com")
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if u == nil || u.String() != "http://127.0.0.1:7897" {
			t.Errorf("期望走代理, got %v", u)
		}
	})

	t.Run("no_proxy 主机直连", func(t *testing.T) {
		for _, host := range []string{"127.0.0.1", "localhost"} {
			u, err := p.ProxyFor(host)
			if err != nil {
				t.Fatalf("%s 不应报错: %v", host, err)
			}
			if u != nil {
				t.Errorf("%s 应直连, got %v", host, u)
			}
		}
	})

	t.Run("子域名匹配", func(t *testing.T) {
		p2 := config.Proxy{URL: "http://127.0.0.1:7897", NoProxy: ".example.com"}
		if u, _ := p2.ProxyFor("api.example.com"); u != nil {
			t.Errorf("子域名应直连, got %v", u)
		}
		if u, _ := p2.ProxyFor("chatgpt.com"); u == nil {
			t.Error("其他域名应走代理")
		}
	})

	t.Run("未配置代理时直连", func(t *testing.T) {
		empty := config.Proxy{}
		u, err := empty.ProxyFor("chatgpt.com")
		if err != nil || u != nil {
			t.Errorf("未配置代理应直连, got %v err %v", u, err)
		}
	})

	t.Run("代理地址非法时报错而非静默直连", func(t *testing.T) {
		bad := config.Proxy{URL: "://bad"}
		if _, err := bad.ProxyFor("chatgpt.com"); err == nil {
			t.Error("非法代理地址应返回错误，避免原因被掩盖")
		}
	})
}
