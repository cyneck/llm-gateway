package main

import (
	_ "embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

//go:embed web/index.html
var indexHTML []byte

func main() {
	// 命令行参数
	configPath := flag.String("config", "", "配置文件路径（默认：程序同目录 config.json）")
	showHelp := flag.Bool("h", false, "显示帮助")
	flag.Parse()

	if *showHelp {
		printUsage()
		return
	}

	// 默认配置路径：程序同目录
	if *configPath == "" {
		exe, err := os.Executable()
		if err != nil {
			exe = "llm-gateway"
		}
		*configPath = filepath.Join(filepath.Dir(exe), "config.json")
	}

	store, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	gateway := NewGateway(store)
	cfg := store.Get()

	mux := http.NewServeMux()

	// 模型网关入口
	mux.HandleFunc("/v1/chat/completions", gateway.handleOpenAI)
	mux.HandleFunc("/v1/messages", gateway.handleAnthropic)
	mux.HandleFunc("/v1/models", gateway.handleModels)

	// 管理 API
	registerAdminRoutes(mux, store, gateway)

	// H5 图形界面
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})

	addr := fmt.Sprintf("%s:%d", cfg.Listen.Host, cfg.Listen.Port)
	log.Printf("llm-gateway 启动成功")
	log.Printf("  网关入口:   http://%s/v1", addr)
	log.Printf("  图形界面:   http://%s/", addr)
	log.Printf("  配置文件:   %s", *configPath)
	log.Printf("  上游数量:   %d", len(cfg.Upstreams))

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("启动失败: %v", err)
	}
}

func printUsage() {
	fmt.Println("llm-gateway - 本地多模型 API 网关（OpenAI ↔ Anthropic 双向互转）")
	fmt.Println()
	fmt.Println("用法:")
	fmt.Println("  llm-gateway [选项]")
	fmt.Println()
	fmt.Println("选项:")
	fmt.Println("  -config string   配置文件路径（默认：程序同目录 config.json）")
	fmt.Println("  -h               显示帮助")
	fmt.Println()
	fmt.Println("首次运行会自动生成默认 config.json，修改后重启生效，")
	fmt.Println("或通过图形界面 http://127.0.0.1:8318/ 在线配置。")
}
