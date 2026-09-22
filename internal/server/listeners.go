package server

import (
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// lanServing tracks whether the optional LAN listener is actually up. The
// settings file records the request; this flag is the truth.
var lanServing atomic.Bool

// LANListenerActive reports whether the LAN listener is currently serving.
func LANListenerActive() bool { return lanServing.Load() }

// Listener is one HTTP listener: the loopback listener is plain HTTP, the
// optional LAN listener is TLS-only. Both share the same mux.
type Listener struct {
	Addr    string
	Handler http.Handler
	TLS     bool
	Cert    tls.Certificate
}

// ServeAll runs every listener. The first listener (loopback) is fatal on
// failure; the optional LAN listener only logs its failure so a moved IP or
// a taken port cannot take the local UI and proxy down with it. The LAN
// flag flips on only after the bind succeeded: a failed bind must not
// report the LAN as active.
func ServeAll(listeners []Listener) error {
	errs := make(chan error, len(listeners))
	for i, l := range listeners {
		l := l
		fatal := i == 0
		go func() {
			srv := &http.Server{
				Addr:              l.Addr,
				Handler:           l.Handler,
				ReadHeaderTimeout: 5 * time.Second,
				IdleTimeout:       120 * time.Second,
			}
			var err error
			if l.TLS {
				srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{l.Cert}, MinVersion: tls.VersionTLS12}
				log.Printf("listening on https://%s", l.Addr)
				var ln net.Listener
				ln, err = net.Listen("tcp", l.Addr)
				if err == nil {
					lanServing.Store(true)
					err = srv.ServeTLS(ln, "", "")
				}
			} else {
				err = srv.ListenAndServe()
			}
			if err != nil && err != http.ErrServerClosed {
				if fatal {
					errs <- err
				} else {
					log.Printf("optional listener on %s stopped: %v", l.Addr, err)
				}
			}
		}()
	}
	return <-errs
}
