package main

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
)

func TestRealHTTP3AndStatusWithoutTCPFallback(t *testing.T) {
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer fixture.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http3.Server{TLSConfig: &tls.Config{Certificates: fixture.TLS.Certificates}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, "HTTP/3.0")
	})}
	defer server.Close()
	go server.Serve(packet)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := config{Mode: "http3", URL: "https://" + packet.LocalAddr().String() + "/probe", Contains: "HTTP/3.0", CAFile: ca}
	if result := probe(ctx, c); !result.OK {
		t.Fatalf("HTTP/3 failed: %+v", result.Error)
	}
	c.URL = "https://" + packet.LocalAddr().String() + "/fail"
	if result := probe(ctx, c); result.Error == nil || result.Error.HTTPStatus != 500 {
		t.Fatalf("status lost: %+v", result)
	}
	c.CAFile = ""
	if result := probe(ctx, c); result.Error == nil || result.Error.Code != "tls" {
		t.Fatalf("untrusted certificate accepted: %+v", result)
	}
}

func TestAuthenticatedCONNECTProxy(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "example-body") }))
	defer target.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: target.Certificate().Raw}), 0600)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			w.WriteHeader(407)
			return
		}
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		downstream, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		fmt.Fprint(downstream, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { defer upstream.Close(); defer downstream.Close(); io.Copy(upstream, downstream) }()
		go func() { defer upstream.Close(); defer downstream.Close(); io.Copy(downstream, upstream) }()
	}))
	defer proxy.Close()
	c := config{Mode: "proxy", URL: target.URL, ProxyURL: proxy.URL, Username: "user", Password: "pass", CAFile: ca, Contains: "example-body"}
	if result := probe(context.Background(), c); !result.OK {
		t.Fatalf("proxy failed: %+v", result.Error)
	}
	c.URL = "http://example.invalid/"
	if result := probe(context.Background(), c); result.Error == nil || result.Error.Code != "probe" {
		t.Fatal("invalid target accepted")
	}
}
