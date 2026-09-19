// Command switcher is a minimal local tool that switches your AI CLI
// logins between accounts on demand: one active account serves all
// traffic, you switch from the web UI, and exhausted accounts fail over
// automatically. Nothing else: no pooling, no rotation, no protocol
// translation.
//
// Subcommands:
//
//	switcher            serve the UI and the API proxy (default)
//	switcher install    point the Codex CLI at Switcher (idempotent)
//	switcher uninstall  remove Switcher from the Codex CLI config
package main

import (
	"embed"
	"flag"
	"fmt"
	"html"
	"io/fs"
	"log"
	"net/http"
	"os"

	"switcher/internal/codexcfg"
	"switcher/internal/config"
	"switcher/internal/login"
	"switcher/internal/provider"
	"switcher/internal/provider/codex"
	"switcher/internal/proxy"
	"switcher/internal/server"
	"switcher/internal/store"
)

// version is overridable at build time: -ldflags "-X main.version=x.y.z".
var version = "dev"

//go:embed web
var webFS embed.FS

func main() {
	port := flag.Int("port", config.DefaultPort, "port for the Switcher server (UI + proxy)")
	flag.Parse()

	args := flag.Args()
	switch {
	case len(args) > 0 && args[0] == "install":
		if err := codexcfg.Install(config.CodexConfigPath()); err != nil {
			log.Fatalf("switcher install: %v", err)
		}
		fmt.Println("Switcher installed into the codex config (backup: ~/.codex/config.toml.switcher-backup).")

	case len(args) > 0 && args[0] == "uninstall":
		if err := codexcfg.Uninstall(config.CodexConfigPath()); err != nil {
			log.Fatalf("switcher uninstall: %v", err)
		}
		fmt.Println("Switcher removed from the codex config.")

	case len(args) > 0 && args[0] == "version":
		fmt.Println("switcher " + version)

	default:
		run(*port)
	}
}

// run starts the OAuth callback listener and the main server, then blocks.
func run(port int) {
	st := store.New(config.Dir())
	providers := map[string]provider.Provider{
		codex.New().ID(): codex.New(),
	}
	proxyManager, err := proxy.New(st, providers)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	// Successful logins persist immediately from the callback goroutine;
	// the first account automatically becomes active.
	logins := login.New(func(a store.Account) error {
		if err := st.Save(a); err != nil {
			return err
		}
		if proxyManager.ActiveID() == "" {
			return proxyManager.Activate(a.ID)
		}
		return nil
	})

	api := &server.API{Store: st, Logins: logins, Proxy: proxyManager, Providers: providers}

	callbackMux := http.NewServeMux()
	callbackMux.HandleFunc("GET /auth/callback", callbackHandler(logins))
	go serveCallback(callbackMux)

	mux := http.NewServeMux()
	api.Register(mux)
	mux.Handle("/v1/", proxyManager) // the actual proxy: codex traffic

	// Dev mode serves the frontend straight from disk: edit web/, refresh
	// the browser, done. No rebuild, no restart.
	var static fs.FS
	if os.Getenv("SWITCHER_DEV") != "" {
		static = os.DirFS("web")
		log.Println("dev mode: serving ./web from disk; refresh the browser after edits")
	} else {
		var err error
		static, err = fs.Sub(webFS, "web")
		if err != nil {
			log.Fatalf("embed web assets: %v", err)
		}
	}
	// Cache-busting: always revalidate the frontend assets so a rebuilt
	// binary (or a dev-mode edit) shows up on a normal refresh.
	staticHandler := http.FileServerFS(static)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		staticHandler.ServeHTTP(w, r)
	}))

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	log.Printf("Switcher v%s running: http://127.0.0.1:%d (codex proxy on the same port under /v1)", version, port)
	if err := http.ListenAndServe(addr, server.LocalOnly(port, mux)); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// serveCallback starts the localhost OAuth callback listener. A failure to
// bind (e.g. the port is taken) only disables logins, not the server.
func serveCallback(mux *http.ServeMux) {
	addr := fmt.Sprintf("127.0.0.1:%d", config.CallbackPort)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("oauth callback listener on %s unavailable: %v", addr, err)
	}
}

// callbackHandler completes the OAuth flow and answers the browser with a
// tiny result page.
func callbackHandler(logins *login.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")
		err := logins.Complete(r.Context(), state, code)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err != nil {
			fmt.Fprintf(w, callbackPage, "Login failed", html.EscapeString(err.Error()))
			return
		}
		fmt.Fprintf(w, callbackPage, "Account added", "You can close this window and return to Switcher.")
	}
}

const callbackPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Switcher</title>
<style>body{font-family:system-ui;display:grid;place-items:center;height:100vh;margin:0;background:#0b0d10;color:#e6e8eb}main{text-align:center}h1{font-size:1.2rem}</style></head>
<body><main><h1>%s</h1><p>%s</p></main></body></html>`
