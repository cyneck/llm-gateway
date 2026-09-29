package gateway

import (
	"strings"
	"testing"
)

// TestTruncateBody_Short 短响应体必须原样保留，不能动它。
func TestTruncateBody_Short(t *testing.T) {
	in := `{"detail":"Unauthorized"}`
	if got := truncateBody([]byte(in)); got != in {
		t.Errorf("短响应体被改写: %q", got)
	}
}

// TestTruncateBody_Empty 空响应体返回空串，不要在界面上显示 "(空)" 之类的噪音。
func TestTruncateBody_Empty(t *testing.T) {
	if got := truncateBody(nil); got != "" {
		t.Errorf("空响应体应返回空串，实际 %q", got)
	}
	if got := truncateBody([]byte{}); got != "" {
		t.Errorf("空响应体应返回空串，实际 %q", got)
	}
}

// TestTruncateBody_Long 超长响应体要截断，并且必须标注完整长度，避免"看起来被吞了"。
func TestTruncateBody_Long(t *testing.T) {
	long := []byte(strings.Repeat("x", maxLoggedBody+500))
	got := truncateBody(long)
	if len(got) <= maxLoggedBody {
		t.Errorf("未被截断: 输出长度 %d，上限 %d", len(got), maxLoggedBody)
	}
	if !strings.Contains(got, "已截断") {
		t.Error("截断后未标注提示")
	}
	if !strings.Contains(got, "2500") {
		t.Errorf("未标注完整响应长度(应为 2500): %s", got[len(got)-40:])
	}
}

// TestTruncateBody_ExactlyAtLimit 刚好等于上限时不截断、不追加提示。
func TestTruncateBody_ExactlyAtLimit(t *testing.T) {
	in := []byte(strings.Repeat("y", maxLoggedBody))
	got := truncateBody(in)
	if got != string(in) {
		t.Errorf("刚好达到上限时不应截断，长度=%d 是否含提示=%v", len(got), strings.Contains(got, "已截断"))
	}
}

// TestReqRecord_ToMapResponse 非 200 的响应体要能进日志 API；没有时不能塞空字段。
func TestReqRecord_ToMapResponse(t *testing.T) {
	withBody := (&ReqRecord{Status: 401, Response: `{"detail":"Unauthorized"}`, Err: "上游返回 401"}).toMap()
	if withBody["response"] != `{"detail":"Unauthorized"}` {
		t.Errorf("响应体未进日志: %#v", withBody["response"])
	}
	noBody := (&ReqRecord{Status: 200}).toMap()
	if _, ok := noBody["response"]; ok {
		t.Error("没有响应体时不该出现 response 字段")
	}
}
