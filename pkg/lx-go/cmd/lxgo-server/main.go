// Command lxgo-server 是 pkg/lx-go 库的独立可执行版本：加载 js/ 目录下的音源脚本，
// 起一个标准 HTTP 服务，兼容 pkg/music（LXClient）期望的 "LX Sync Server" 协议。
//
// 如果是从 pkg/music 里内嵌使用（不想手动起这个独立进程），直接 import
// "github.com/cxjava/open-xiaoai/pkg/lx-go" 这个库包即可，不需要跑这个命令行程序，
// 参见 pkg/music/lx_embedded.go。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	lxgo "github.com/cxjava/open-xiaoai/pkg/lx-go"
)

func main() {
	jsDir := flag.String("js", "./js", "音源脚本目录")
	addr := flag.String("addr", ":8080", "监听地址")
	flag.Parse()

	registry, err := lxgo.LoadSources(*jsDir)
	if err != nil {
		panic(err)
	}
	defer registry.Close()

	srv := lxgo.NewServer(registry)
	defer srv.Close()

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	httpServer := &http.Server{Addr: *addr, Handler: mux}

	// 监听 SIGINT/SIGTERM 优雅关闭：http.ListenAndServe 正常情况下永远不会返回，
	// 之前那种 "defer registry.Close(); http.ListenAndServe(...)" 的写法里，
	// defer 语句永远没机会执行——进程被 kill 的时候是直接终止，不会跑到
	// ListenAndServe 之后。改成先 Shutdown() 再走正常函数返回路径，让所有
	// defer（关闭每个音源脚本的 goja 事件循环）都能真正执行到。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("已加载 %d 个音源，server started at %s", len(registry.Sources), *addr)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Printf("❌ [lx-go] HTTP 服务异常退出: %v", err)
		}
	case <-ctx.Done():
		log.Printf("🛑 [lx-go] 收到退出信号，正在优雅关闭...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("⚠️  [lx-go] HTTP 优雅关闭超时/失败: %v", err)
		}
	}
}
