// transport-probe makes bounded HTTP/3 or authenticated forward-proxy requests.
// Credentials belong in a protected config file; diagnostics never contain URLs.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"github.com/quic-go/quic-go/http3"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

type config struct {
	Mode     string `json:"mode"`
	URL      string `json:"url"`
	ProxyURL string `json:"proxy_url"`
	Username string `json:"username"`
	Password string `json:"password"`
	Contains string `json:"contains"`
	CAFile   string `json:"ca_file,omitempty"`
}
type probeError struct {
	Code       string `json:"code"`
	HTTPStatus int    `json:"http_status,omitempty"`
}
type result struct {
	OK    bool        `json:"ok"`
	Error *probeError `json:"error,omitempty"`
}

func classify(err error) *probeError {
	var dns *net.DNSError
	var invalid x509.CertificateInvalidError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var verification *tls.CertificateVerificationError
	var timeout net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &probeError{Code: "timeout"}
	case errors.As(err, &dns):
		return &probeError{Code: "dns"}
	case errors.As(err, &invalid), errors.As(err, &unknown), errors.As(err, &hostname), errors.As(err, &verification):
		return &probeError{Code: "tls"}
	case errors.As(err, &timeout) && timeout.Timeout():
		return &probeError{Code: "timeout"}
	default:
		return &probeError{Code: "connection"}
	}
}
func probe(ctx context.Context, c config) result {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return result{Error: &probeError{Code: "probe"}}
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return result{Error: &probeError{Code: "probe"}}
		}
		tlsConfig.RootCAs = pool
	}
	target, err := url.Parse(c.URL)
	if err != nil || target.Scheme != "https" || target.Host == "" || target.User != nil {
		return result{Error: &probeError{Code: "probe"}}
	}
	var transport http.RoundTripper
	switch c.Mode {
	case "http3":
		tr := &http3.Transport{TLSClientConfig: tlsConfig}
		defer tr.Close()
		transport = tr
	case "proxy":
		proxy, err := url.Parse(c.ProxyURL)
		if err != nil || proxy.Host == "" || (proxy.Scheme != "http" && proxy.Scheme != "https") || proxy.User != nil {
			return result{Error: &probeError{Code: "probe"}}
		}
		proxy.User = url.UserPassword(c.Username, c.Password)
		tr := &http.Transport{Proxy: http.ProxyURL(proxy), TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 8 * time.Second, MaxResponseHeaderBytes: 16384}
		defer tr.CloseIdleConnections()
		transport = tr
	default:
		return result{Error: &probeError{Code: "probe"}}
	}
	client := &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, "GET", c.URL, nil)
	if err != nil {
		return result{Error: &probeError{Code: "probe"}}
	}
	response, err := client.Do(req)
	if err != nil {
		return result{Error: classify(err)}
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return result{Error: &probeError{Code: "http_status", HTTPStatus: response.StatusCode}}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil {
		return result{Error: classify(err)}
	}
	if len(body) > 65536 || !strings.Contains(string(body), c.Contains) || (c.Mode == "http3" && response.ProtoMajor != 3) {
		return result{Error: &probeError{Code: "content"}}
	}
	return result{OK: true}
}
func main() {
	debug.SetMemoryLimit(24 << 20)
	os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	path := flag.String("config", "", "protected JSON config path")
	flag.Parse()
	c := config{}
	b, err := os.ReadFile(*path)
	var r result
	if err != nil || len(b) > 16384 || json.Unmarshal(b, &c) != nil {
		r = result{Error: &probeError{Code: "probe"}}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		r = probe(ctx, c)
	}
	json.NewEncoder(os.Stdout).Encode(r)
}
