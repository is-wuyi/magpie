package gui

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/gateway"
)

// startCloudWeb serves only the cloud portal and the authenticated gateway.
// The legacy owner-key middleware and local-management handler are not
// registered anywhere on this path.
func startCloudWeb(addr, version string) (*Web, error) {
	Version = version
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	runtime, err := openCloudRuntime(ctx)
	cancel()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		runtime.Close()
		return nil, err
	}
	if ln, err = gateway.ServeTLS(ln); err != nil {
		runtime.Close()
		return nil, err
	}
	w := &Web{Addr: ln.Addr().String(), quit: make(chan struct{}), onClose: runtime.Close}
	host, port, _ := net.SplitHostPort(w.Addr)
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	scheme := "http"
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
		scheme = gateway.Scheme()
	}
	w.Link = scheme + "://" + net.JoinHostPort(host, port) + "/"
	if u, err := url.Parse(gateway.PublicURL()); err == nil && u.Host != "" {
		w.Link = strings.TrimRight(u.String(), "/") + "/"
	}
	gw := startBackend()
	if gw != nil {
		served.Store(gw)
	}
	w.srv = &http.Server{
		Handler:           closesWhenDraining(onePort(webCompressed(runtime.Handler()))),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go w.srv.Serve(ln)
	return w, nil
}
