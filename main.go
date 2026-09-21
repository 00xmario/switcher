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
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"html"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"switcher/internal/codexcfg"
	"switcher/internal/config"
	"switcher/internal/login"
	"switcher/internal/mgmtapi"
	"switcher/internal/provider"
	"switcher/internal/provider/claude"
	"switcher/internal/provider/codex"
	"switcher/internal/provider/grok"
	"switcher/internal/provider/opencode"
	"switcher/internal/proxy"
	"switcher/internal/server"
	"switcher/internal/settings"
	"switcher/internal/store"
	"switcher/internal/update"
	"switcher/internal/usage"
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
	registered := []provider.Provider{
		codex.New(),
		claude.New(),
		grok.New(),
		opencode.New(),
	}
	providers := make(map[string]provider.Provider, len(registered))
	for _, p := range registered {
		providers[p.ID()] = p
	}
	proxyManager, err := proxy.New(st, providers)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	settingsStore := settings.New(config.Dir())
	// The device token exists for the menu bar app as soon as auth is on.
	if settingsStore.Enabled() {
		_, _ = settingsStore.EnsureDeviceToken()
	}

	// The management key lets tools like T3 Code talk to Switcher's
	// CLIProxyAPI-compatible hub surface. Generated once, shown in the UI.
	state, err := st.LoadState()
	if err != nil {
		log.Fatalf("load state: %v", err)
	}
	managementKey := state.ManagementKey
	if managementKey == "" {
		raw := make([]byte, 24)
		if _, err := rand.Read(raw); err != nil {
			log.Fatalf("generate management key: %v", err)
		}
		managementKey = hex.EncodeToString(raw)
		state.ManagementKey = managementKey
		if err := st.SaveState(state); err != nil {
			log.Fatalf("persist management key: %v", err)
		}
		proxyManager.SetManagementKey(managementKey)
	}
	// Successful logins persist immediately from the callback goroutine;
	// the first account of a provider automatically becomes active.
	logins := login.New(func(a store.Account) error {
		if err := st.Save(a); err != nil {
			return err
		}
		if proxyManager.ActiveID(a.Provider) == "" {
			return proxyManager.Activate(a.ID)
		}
		return nil
	}, st.Get)
	mgmtAPI := &mgmtapi.API{Store: st, Proxy: proxyManager, Logins: logins, ManagementKey: managementKey}

	updater := update.New(version)

	// Background usage sync: the server owns freshness, so the web UI and
	// the menu bar always agree without waiting for a client to poll.
	go func() {
		proxyManager.RefreshUsageAll(context.Background())
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			proxyManager.RefreshUsageAll(context.Background())
		}
	}()
	usageService := usage.NewService(config.Dir(), nil)
	// Cost/token usage: rescan the common windows periodically in the
	// background so the API always has a recent summary ready.
	go func() {
		usageService.Scan(30)
		usageService.Scan(90)
	}()
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			usageService.RefreshStale([]int{30, 90, 7}, 15*time.Minute)
		}
	}()
	api := &server.API{
		Store: st, Logins: logins, Proxy: proxyManager, Providers: providers,
		ManagementKey: managementKey, Version: version, Updater: updater, Usage: usageService,
		Settings: settingsStore,
	}

	// Each provider with a browser redirect has its own callback listener;
	// the ports are fixed by the OAuth clients' registered redirect URIs.
	callbackMux := http.NewServeMux()
	// Codex registers /auth/callback, Claude /callback; both complete the
	// same flow, so both paths are served on both listeners.
	callbackMux.HandleFunc("GET /auth/callback", callbackHandler(logins))
	callbackMux.HandleFunc("GET /callback", callbackHandler(logins))
	for _, port := range []int{config.CallbackPort, config.ClaudeCallbackPort} {
		go serveCallback(callbackMux, port)
	}

	mux := http.NewServeMux()
	api.Register(mux)
	mgmtAPI.Register(mux) // CLIProxyAPI-compatible hub surface for T3 Code
	mux.Handle("/codex/", proxyManager)
	mux.Handle("/claude/", proxyManager)
	mux.Handle("/grok/", proxyManager)
	mux.Handle("/opencode/", proxyManager)

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

	log.Printf("Switcher v%s running: http://127.0.0.1:%d (codex proxy on the same port under /v1)", version, port)

	// Listener topology: the loopback listener is always plain HTTP (the
	// CLIs, T3 Code, and the menu bar app need no configuration), and the
	// optional LAN listener is always TLS. Binding the LAN without a
	// password on disk is refused: no TOFU window. LocalOnly runs outside
	// the auth gate so rebinding checks happen pre-auth.
	gate := &server.AuthGate{Store: settingsStore}
	loopback := server.Listener{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: server.LocalOnlyWith(server.LocalOptions{Port: port}, gate.Wrap(mux)),
	}
	listeners := []server.Listener{loopback}
	if settingsStore.Load().BindLAN && settingsStore.Load().TLS && settingsStore.HasPassword() {
		if lan := server.LANAddress(); lan != "" {
			cert, err := server.EnsureTLSCert(config.Dir(), lan, port)
			if err != nil {
				log.Printf("lan listener: tls setup failed: %v", err)
			} else {
				listeners = append(listeners, server.Listener{
					Addr:    fmt.Sprintf("%s:%d", lan, port),
					TLS:     true,
					Cert:    cert,
					Handler: server.LocalOnlyWith(server.LocalOptions{Port: port, LANHost: lan}, gate.Wrap(mux)),
				})
				log.Printf("lan listener: https://%s:%d (self-signed)", lan, port)
			}
		} else {
			log.Printf("lan binding requested but no non-loopback IPv4 address found")
		}
	}
	if err := server.ServeAll(listeners); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// serveCallback starts a localhost OAuth callback listener. A failure to
// bind (e.g. the port is taken) only disables logins, not the server.
func serveCallback(mux *http.ServeMux, port int) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
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
		// OpenAI's authorize redirect does not echo the state back, and
		// Anthropic's returns it in a fragment the browser never sends.
		// With no state in the URL, a single in-flight login is the one.
		if state == "" {
			state = logins.SinglePending()
		}
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
