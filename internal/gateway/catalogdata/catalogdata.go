// Package catalogdata 内嵌构造 Codex 模型目录所需的官方基线数据。
//
// Codex 对 model_catalog_json 做**严格反序列化**：条目缺任何字段（包括嵌套的
// model_messages.token_budget.* 子字段）都会以 "missing field xxx" 拒绝启动。
// 这里内嵌一份官方模型的 model_messages 完整基线与一段指令模板作为兜底，
// 第三方模型条目直接复用，避免逐字段猜测 Codex 的 schema。
package catalogdata

import (
	_ "embed"
	"encoding/json"
	"sync"
)

//go:embed model_messages.json
var rawModelMessages []byte

//go:embed instructions_template.txt
var fallbackInstructions string

var (
	once      sync.Once
	baseline  map[string]any
	template_ string
)

// ModelMessages 返回可直接内联进目录条目的 model_messages 结构。
//
// 每次返回深拷贝，调用方改动不会污染基线。
func ModelMessages() map[string]any {
	once.Do(load)
	return cloneMap(baseline)
}

// InstructionsTemplate 返回官方模型指令模板
func InstructionsTemplate() string {
	once.Do(load)
	return template_
}

func load() {
	baseline = map[string]any{}
	if err := json.Unmarshal(rawModelMessages, &baseline); err != nil {
		baseline = map[string]any{}
	}
	if tpl, ok := baseline["instructions_template"].(string); ok && tpl != "" {
		template_ = tpl
		return
	}
	template_ = fallbackInstructions
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if nested, ok := v.(map[string]any); ok {
			out[k] = cloneMap(nested)
			continue
		}
		out[k] = v
	}
	return out
}
