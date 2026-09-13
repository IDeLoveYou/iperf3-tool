// iperf3-tool 是一个面向浏览器的 UDP/TCP 网络波动监测器。
//
// 程序本身不实现 iperf3 协议，而是安全地启动本机已有的 iperf3，解析
// --json-stream 输出，再通过 HTTP/SSE 将数据推送给网页。
package main

import (
	"log"
	"net"
	"net/http"
	"sort"

	"iperf3-tool/internal/monitor"
	webapp "iperf3-tool/internal/web"
)

// version 在本地开发时为 dev，发布构建通过 -ldflags 写入具体版本号。
// 使用变量而不是常量，是因为 Go 链接器只能覆盖变量。
var version = "dev"

func main() {
	hub := monitor.NewHub()
	server := webapp.NewServer(hub)

	// 监听 0.0.0.0 会接收所有本机 IPv4 网卡上的连接，因此同一局域网中的
	// 手机和电脑可以通过运行机器的局域网 IP 打开网页。端口仍固定为 8088。
	address := "0.0.0.0:8088"
	log.Printf("iperf3 网络监测器 %s 已启动，监听地址：%s", version, address)
	log.Printf("本机访问：http://localhost:8088")
	for _, ip := range localIPv4Addresses() {
		log.Printf("局域网访问：http://%s:8088", ip)
	}
	log.Printf("默认目标：%s:%d；请在网页中确认配置后开始监测", monitor.DefaultConfig().Host, monitor.DefaultConfig().Port)

	if err := http.ListenAndServe(address, server); err != nil {
		log.Fatal(err)
	}
}

// localIPv4Addresses 返回可供其他局域网设备访问的 IPv4 地址。这里排除回环地址，
// 因为 localhost 只能由运行程序的本机使用；排序后日志在不同启动之间更加稳定。
func localIPv4Addresses() []string {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err != nil || ip.IsLoopback() || ip.To4() == nil {
			continue
		}
		value := ip.String()
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
