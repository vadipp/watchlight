package main

import (
	"crypto/tls"
	"net"
)

var netDialer = net.Dialer{}
var splitHostPort = net.SplitHostPort

func tlsConfig(host string) *tls.Config {
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}
