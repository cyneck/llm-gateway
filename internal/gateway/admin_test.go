package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"llm-gateway/internal/config"
)

func TestAdminAuth_Unauthorized(t *testing.T) {
	store := &config.Store{}
	store.SetForTest(&config.Config{
		Listen:   config.Listen{Host: "127.0.0.1", Port: 0},
		AdminKey: "secret-token",
	})
	g := New(store)

	mux := http.NewServeMux()
	RegisterAdminRoutes(mux, store, g)

	req := httptest.NewRequest("GET", "/api/config", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("未带 token 应返回 401，实际 %d", rec.Code)
	}
}

func TestAdminAuth_Authorized(t *testing.T) {
	store := &config.Store{}
	store.SetForTest(&config.Config{
		Listen:   config.Listen{Host: "127.0.0.1", Port: 0},
		AdminKey: "secret-token",
	})
	g := New(store)

	mux := http.NewServeMux()
	RegisterAdminRoutes(mux, store, g)

	req := httptest.NewRequest("GET", "/api/config", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("正确 token 应返回 200，实际 %d", rec.Code)
	}
}
