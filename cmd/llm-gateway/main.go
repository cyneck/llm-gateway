// Command llm-gateway 是本地多模型 API 网关的入口。
//
// 启动一个 HTTP 服务，对外暴露 OpenAI / Anthropic / Responses 三种协议入口，
// 并提供内嵌的 H5 管理界面。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"llm-gateway/internal/config"
	"llm-gateway/internal/gateway"
	"llm-gateway/internal/web"
)

func main() {
	configPath := flag.String("config", "", "配置文件路径（默认：程序同目录 config.json）")
	listenHost := flag.String("host", "", "监听地址（覆盖配置文件，Docker 容器中设为 0.0.0.0）")
	listenPort := flag.Int("port", 0, "监听端口（覆盖配置文件）")
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

	store, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	g := gateway.New(store)
	cfg := store.Get()

	// 命令行覆盖监听地址（Docker 容器中需监听 0.0.0.0）
	if *listenHost != "" {
		cfg.Listen.Host = *listenHost
	}
	if *listenPort != 0 {
		cfg.Listen.Port = *listenPort
	}

	mux := http.NewServeMux()

	// 模型网关入口
	mux.HandleFunc("/v1/chat/completions", g.HandleOpenAI)
	mux.HandleFunc("/v1/messages", g.HandleAnthropic)
	mux.HandleFunc("/v1/responses", g.HandleResponses)
	mux.HandleFunc("/v1/models", g.HandleModels)

	// 管理 API
	gateway.RegisterAdminRoutes(mux, store, g)

	// H5 图形界面
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(web.IndexHTML)
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
	fmt.Println("llm-gateway - 本地多模型 API 网关（OpenAI ↔ Anthropic 双向互转 + Responses 协议）")
	fmt.Println()
	fmt.Println("用法:")
	fmt.Println("  llm-gateway [选项]")
	fmt.Println()
	fmt.Println("选项:")
	fmt.Println("  -config string   配置文件路径（默认：程序同目录 config.json）")
	fmt.Println("  -host string     监听地址（覆盖配置文件，Docker 中设为 0.0.0.0）")
	fmt.Println("  -port int        监听端口（覆盖配置文件）")
	fmt.Println("  -h               显示帮助")
	fmt.Println()
	fmt.Println("首次运行会自动生成默认 config.json，修改后重启生效，")
	fmt.Println("或通过图形界面 http://127.0.0.1:8318/ 在线配置。")
}
