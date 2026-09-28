package main

import (
	"crypto/rand"
	"encoding/hex"
)

// newID 生成 8 字节随机 hex ID（16 字符）
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
