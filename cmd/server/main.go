// Command dns-change-server 启动 DNS 区域变更服务的 HTTP API（内存存储）。
//
// 仅用于本地演示与测试；生产环境应替换为持久化 Store 实现。
package main

import (
	"flag"
	"log"
	"net/http"

	dns "github.com/chris64233/go-dns-change"
	"github.com/chris64233/go-dns-change/httpapi"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	flag.Parse()

	store := dns.NewMemoryStore()
	svc := dns.NewService(store)
	handler := httpapi.NewHandler(svc)

	log.Printf("dns-change server listening on %s", *addr)
	if err := http.ListenAndServe(*addr, handler); err != nil {
		log.Fatal(err)
	}
}
