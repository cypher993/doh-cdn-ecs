package main

import (
	"bytes"
	"encoding/base64" // 【新增】用于解码 GET 请求中的 dns 参数
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/miekg/dns"
)

const googleDoHURL = "https://dns.google/dns-query"

// 从各大 CDN 提取真实 IP (保持不变)
func getRealIP(r *http.Request) string {
	headersToCheck := []string{
		"CF-Connecting-IP", // Cloudflare
		"Fastly-Client-IP", // Fastly
		"EO-Client-IP",     // Tencent EdgeOne
		"True-Client-IP",   // Cloudflare/Akamai
		"X-Real-IP",        // Generic Nginx/ESA
	}

	for _, header := range headersToCheck {
		if ip := r.Header.Get(header); ip != "" {
			return strings.TrimSpace(ip)
		}
	}

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ips := strings.Split(xff, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// 给 DNS 消息添加 ECS (保持不变)
func addECS(msg *dns.Msg, clientIP string) error {
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return fmt.Errorf("invalid IP address: %s", clientIP)
	}

	ecs := &dns.EDNS0_SUBNET{
		Code:        dns.EDNS0SUBNET,
		SourceScope: 0,
	}

	if ip4 := ip.To4(); ip4 != nil {
		ecs.Family = 1
		ecs.SourceNetmask = 24
		ecs.Address = ip4
	} else {
		ecs.Family = 2
		ecs.SourceNetmask = 56
		ecs.Address = ip
	}

	var opt *dns.OPT
	for _, extra := range msg.Extra {
		if o, ok := extra.(*dns.OPT); ok {
			opt = o
			break
		}
	}

	if opt == nil {
		opt = new(dns.OPT)
		opt.Hdr.Name = "."
		opt.Hdr.Rrtype = dns.TypeOPT
		opt.SetUDPSize(4096)
		msg.Extra = append(msg.Extra, opt)
	}

	filteredOptions := make([]dns.EDNS0, 0)
	for _, option := range opt.Option {
		if option.Option() != dns.EDNS0SUBNET {
			filteredOptions = append(filteredOptions, option)
		}
	}
	opt.Option = append(filteredOptions, ecs)

	return nil
}

// 处理 DoH 请求 (修改以支持 GET 和 POST)
func handleDoH(w http.ResponseWriter, r *http.Request) {
	var body []byte
	var err error

	// 【新增】路由分支：分别处理 GET 和 POST
	switch r.Method {
	case http.MethodPost:
		// 校验 POST 的 Content-Type
		if r.Header.Get("Content-Type") != "application/dns-message" {
			http.Error(w, "Only application/dns-message is supported", http.StatusUnsupportedMediaType)
			return
		}
		body, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}

	case http.MethodGet:
		// RFC 8484 GET 请求的报文在 URL 的 dns 参数中
		dnsParam := r.URL.Query().Get("dns")
		if dnsParam == "" {
			http.Error(w, "Missing 'dns' query parameter", http.StatusBadRequest)
			return
		}
		// RFC 8484 规定使用 Base64Url 且无填充 (RawURLEncoding)
		body, err = base64.RawURLEncoding.DecodeString(dnsParam)
		if err != nil {
			http.Error(w, "Invalid Base64Url encoding in 'dns' parameter", http.StatusBadRequest)
			return
		}

	default:
		// 拦截 GET 和 POST 之外的方法
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clientIP := getRealIP(r)
	log.Printf("Received %s request from IP: %s", r.Method, clientIP)

	msg := new(dns.Msg)
	if err := msg.Unpack(body); err != nil {
		http.Error(w, "Failed to unpack DNS message", http.StatusBadRequest)
		return
	}

	if err := addECS(msg, clientIP); err != nil {
		log.Printf("Failed to add ECS: %v", err)
	}

	packedMsg, err := msg.Pack()
	if err != nil {
		http.Error(w, "Failed to pack DNS message", http.StatusInternalServerError)
		return
	}

	// 无论客户端是 GET 还是 POST，统一使用 POST 向上游 (Google) 转发
	req, err := http.NewRequest(http.MethodPost, googleDoHURL, bytes.NewReader(packedMsg))
	if err != nil {
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Failed to contact upstream DoH", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "Failed to read upstream response", http.StatusInternalServerError)
		return
	}

	// 返回结果前，确保头部是正确的 DNS 消息格式
	w.Header().Set("Content-Type", "application/dns-message")
	
	// 如果是 GET 请求，可以考虑透传上游的 Cache-Control 头部（可选）
	if cacheControl := resp.Header.Get("Cache-Control"); cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}

	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func main() {
	dohPath := os.Getenv("DOH_PATH")
	if dohPath == "" {
		dohPath = "/dns-query"
	}

	if !strings.HasPrefix(dohPath, "/") {
		dohPath = "/" + dohPath
	}

	http.HandleFunc(dohPath, handleDoH)
	
	log.Printf("DoH server starting on :8080...")
	log.Printf("Private DoH Path is set to: %s", dohPath)
	
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}