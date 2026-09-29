package gateway

import (
	"time"
)

// ReqRecord 单条请求日志
type ReqRecord struct {
	Time        time.Time
	Method      string
	Path        string
	Entry       string
	Model       string
	Upstream    string
	UpstreamURL string
	Convert     string
	Status      int
	Duration    time.Duration
	Err         string
	Stream      bool
	// Response 非 200 时上游返回的响应体（截断后），判断问题最直接的依据
	Response string
	// Hint 诊断提示：出问题时直接告诉用户往哪儿查，避免对着一个状态码干猜
	Hint string
}

func (r *ReqRecord) toMap() map[string]any {
	m := map[string]any{
		"time":         r.Time.Format("15:04:05"),
		"method":       r.Method,
		"path":         r.Path,
		"entry":        r.Entry,
		"model":        r.Model,
		"upstream":     r.Upstream,
		"upstream_url": r.UpstreamURL,
		"convert":      r.Convert,
		"status":       r.Status,
		"duration":     r.Duration.Round(time.Millisecond).String(),
		"duration_ms":  r.Duration.Milliseconds(),
		"stream":       r.Stream,
	}
	if r.Err != "" {
		m["error"] = r.Err
	}
	if r.Response != "" {
		m["response"] = r.Response
	}
	if r.Hint != "" {
		m["hint"] = r.Hint
	}
	return m
}

// LogBuffer 环形缓冲请求日志
type LogBuffer struct {
	mu   chan struct{}
	buf  []map[string]any
	head int
	size int
}

// NewLogBuffer 创建指定容量的日志环形缓冲
func NewLogBuffer(size int) *LogBuffer {
	return &LogBuffer{
		mu:   make(chan struct{}, 1),
		buf:  make([]map[string]any, 0, size),
		size: size,
	}
}

// Add 追加一条日志
func (l *LogBuffer) Add(r *ReqRecord) {
	l.mu <- struct{}{}
	defer func() { <-l.mu }()
	if len(l.buf) < l.size {
		l.buf = append(l.buf, r.toMap())
	} else {
		l.buf = append(l.buf, r.toMap())
		l.buf = l.buf[1:]
	}
}

// List 返回日志快照（最新的在前）
func (l *LogBuffer) List() []map[string]any {
	l.mu <- struct{}{}
	defer func() { <-l.mu }()
	out := make([]map[string]any, len(l.buf))
	for i, v := range l.buf {
		out[len(l.buf)-1-i] = v
	}
	return out
}
