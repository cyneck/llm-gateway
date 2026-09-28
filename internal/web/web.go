// Package web 内嵌 H5 管理界面资源。
package web

import _ "embed"

// IndexHTML 内嵌的管理界面 HTML。
//
//go:embed index.html
var IndexHTML []byte
