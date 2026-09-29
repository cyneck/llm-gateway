package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAugmentOfficialAuthError_JSON 官方 401 的 JSON 响应必须补上排查提示，且不能丢原始字段。
func TestAugmentOfficialAuthError_JSON(t *testing.T) {
	in := []byte(`{"detail":"Unauthorized"}`)
	out := augmentOfficialAuthError(in)

	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("补充提示后不再是合法 JSON: %v, 输出=%s", err, out)
	}
	if m["detail"] != "Unauthorized" {
		t.Errorf("原始错误字段被破坏: detail=%v", m["detail"])
	}
	hint, _ := m["hint"].(string)
	if hint == "" {
		t.Fatal("未附加 hint 字段")
	}
	for _, kw := range []string{"套餐", "codex login", "登录态"} {
		if !strings.Contains(hint, kw) {
			t.Errorf("hint 缺少排查关键词 %q: %s", kw, hint)
		}
	}
}

// TestAugmentOfficialAuthError_NonJSON 非 JSON 响应必须原样透传，绝不因为加提示而吞掉上游信息。
func TestAugmentOfficialAuthError_NonJSON(t *testing.T) {
	cases := [][]byte{
		[]byte("<html>login required</html>"),
		[]byte(""),
		[]byte("plain text error"),
	}
	for _, in := range cases {
		out := augmentOfficialAuthError(in)
		if string(out) != string(in) {
			t.Errorf("非 JSON 响应被改写: 输入=%q 输出=%q", in, out)
		}
	}
}

// TestAugmentOfficialAuthError_NullBody 上游返回 JSON null 时不能 panic，要原样透传。
func TestAugmentOfficialAuthError_NullBody(t *testing.T) {
	in := []byte(`null`)
	out := augmentOfficialAuthError(in)
	if string(out) != string(in) {
		t.Errorf("null 响应被改写: 输出=%q", out)
	}
}
