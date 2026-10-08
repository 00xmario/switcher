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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"switcher/internal/codexcfg"
	"switcher/internal/config"
	"switcher/internal/desktoprelay"
	"switcher/internal/login"
	"switcher/internal/mgmtapi"
	"switcher/internal/phone"
	"switcher/internal/provider"
	"switcher/internal/provider/antigravity"
	"switcher/internal/provider/claude"
	"switcher/internal/provider/codex"
	"switcher/internal/provider/copilot"
	"switcher/internal/provider/gemini"
	"switcher/internal/provider/grok"
	"switcher/internal/provider/opencode"
	"switcher/internal/proxy"
	"switcher/internal/remote"
	"switcher/internal/server"
	"switcher/internal/sessionmeta"
	"switcher/internal/settings"
	"switcher/internal/store"
	"switcher/internal/update"
	"switcher/internal/usage"
)

// version is overridable at build time: -ldflags "-X main.version=x.y.z".
var version = "dev"

const defaultDesktopRelayPort = 8789

//go:embed web
var webFS embed.FS

//go:embed THIRD_PARTY_NOTICES.md
var thirdPartyNotices string

// ignoreOwnRelayProxy drops proxy settings that point at Switcher's own Desktop
// relay. Claude Code exports them to processes it starts, and a Switcher
// launched from such a shell would otherwise send its own Anthropic calls
// through its relay and fail TLS verification.
func ignoreOwnRelayProxy(relayPort int) {
	own := func(value string) bool {
		u, err := url.Parse(value)
		if err != nil || u.Port() != strconv.Itoa(relayPort) {
			return false
		}
		ip := net.ParseIP(u.Hostname())
		return u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()
	}
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if value := os.Getenv(key); value != "" && own(value) {
			os.Unsetenv(key)
		}
	}
}

func main() {
	port := flag.Int("port", config.DefaultPort, "port for the Switcher server (UI + proxy)")
	desktopRelayPort := flag.Int("desktop-relay-port", defaultDesktopRelayPort, "port for the opt-in Desktop task relay")
	// Command flags may also come before the command: switcher --json status.
	jsonOutput := flag.Bool("json", false, "machine-readable output for commands")
	serverURL := flag.String("url", "", "Switcher to talk to, for commands")
	flag.Usage = func() { runCommand([]string{"help"}, config.DefaultPort) }
	flag.Parse()
	ignoreOwnRelayProxy(*desktopRelayPort)

	args := flag.Args()
	switch {
	case len(args) == 0 || args[0] == "serve":
		if len(args) > 1 {
			log.Fatal("usage: switcher [-port N] [-desktop-relay-port N] serve")
		}
		if err := validateServerPorts(*port, *desktopRelayPort); err != nil {
			log.Fatal(err)
		}
		run(*port, *desktopRelayPort)

	case args[0] == "install":
		if len(args) > 2 || (len(args) == 2 && args[1] != "--reselect") {
			log.Fatal("usage: switcher [-port N] install [--reselect]")
		}
		path := config.CodexConfigPath()
		var err error
		if len(args) == 2 {
			err = codexcfg.ReselectAt(path, *port)
		} else {
			err = codexcfg.InstallAt(path, *port)
		}
		if err != nil {
			log.Fatalf("switcher install: %v", err)
		}
		fmt.Printf("Codex user config checked at %s; native requests not tested.\n", path)

	case args[0] == "uninstall":
		if len(args) > 2 || (len(args) == 2 && args[1] != "--legacy-remove") {
			log.Fatal("usage: switcher uninstall [--legacy-remove]")
		}
		path := config.CodexConfigPath()
		var err error
		if len(args) == 2 {
			err = codexcfg.UninstallLegacy(path)
		} else {
			err = codexcfg.Uninstall(path)
		}
		if errors.Is(err, codexcfg.ErrLegacyOwnershipUnknown) {
			log.Fatal("Switcher cannot recover the previous provider from this legacy config; use switcher uninstall --legacy-remove to remove only the known Switcher entry")
		}
		if err != nil {
			log.Fatalf("switcher uninstall: %v", err)
		}
		fmt.Printf("Switcher-owned Codex settings removed from %s.\n", path)

	case args[0] == "version":
		fmt.Println("switcher " + version)
	case args[0] == "licenses":
		fmt.Print(thirdPartyNotices)

	default:
		if *jsonOutput {
			args = append(args, "--json")
		}
		if *serverURL != "" {
			args = append(args, "--url", *serverURL)
		}
		os.Exit(runCommand(args, *port))
	}
}

func validateServerPorts(port, desktopRelayPort int) error {
	if port < 1 || port > 65535 || desktopRelayPort < 1 || desktopRelayPort > 65535 {
		return errors.New("server ports must be between 1 and 65535")
	}
	if port == desktopRelayPort {
		return errors.New("desktop relay port must differ from the UI port")
	}
	return nil
}

// Construction and resume are lazy. Only the relay's persisted explicit opt-in
// may start its listener. A failed resume retains the manager for public status
// and an explicit later retry from local management.
func resumeDesktopRelay(ctx context.Context, cfg desktoprelay.Config) (*desktoprelay.Manager, error) {
	m, err := desktoprelay.New(cfg)
	if err != nil {
		return nil, err
	}
	return m, m.Resume(ctx)
}

// Resolve only in main. The API and manager have no implicit HOME fallback.
// Relative overrides fail closed instead of targeting the working directory.
func desktopSettingsPath() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		if !filepath.IsAbs(dir) || strings.ContainsAny(dir, "\x00\r\n") {
			return ""
		}
		return filepath.Join(dir, "settings.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) || strings.ContainsAny(home, "\x00\r\n") {
		return ""
	}
	return filepath.Join(home, ".claude", "settings.json")
}

// Resolve metadata roots only at the production composition point. This helper
// is pure; the shared index scans lazily for trusted resolution or a local GET.
func sessionMetadataRoots(goos, home, configDir string) sessionmeta.Config {
	valid := func(path string) bool {
		return filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\r\n")
	}
	var cfg sessionmeta.Config
	if goos == "darwin" && valid(home) {
		cfg.DesktopRoot = filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions")
	}
	if configDir != "" {
		if valid(configDir) {
			cfg.ProjectsRoot = filepath.Join(configDir, "projects")
		}
	} else if valid(home) {
		cfg.ProjectsRoot = filepath.Join(home, ".claude", "projects")
	}
	return cfg
}

// run starts the OAuth callback listener and the main server, then blocks.
func run(port, desktopRelayPort int) {
	// A second Switcher on this Mac stops here, before it refreshes accounts,
	// resumes the Desktop relay or starts the Tailscale add-on.
	if probe, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err != nil {
		log.Fatalf("Switcher is already running here, or port %d is in use: %v", port, err)
	} else {
		probe.Close()
	}
	st := store.New(config.Dir())
	claudeProvider := claude.New()
	if err := claudeProvider.ConfigureNative(st, config.Dir()); err != nil {
		log.Printf("native Claude switching unavailable: %v", err)
	}
	registered := []provider.Provider{
		codex.New(),
		claudeProvider,
		grok.New(),
		opencode.New(),
		antigravity.New(),
		gemini.New(),
		copilot.New(),
	}
	providers := make(map[string]provider.Provider, len(registered))
	registrationOrder := make([]string, 0, len(registered))
	for _, p := range registered {
		providers[p.ID()] = p
		registrationOrder = append(registrationOrder, p.ID())
	}
	proxyManager, err := proxy.New(st, providers, registrationOrder)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}
	home, _ := os.UserHomeDir()
	metadataIndex := sessionmeta.New(sessionMetadataRoots(runtime.GOOS, home, os.Getenv("CLAUDE_CONFIG_DIR")))
	relayCtx, relayCancel := context.WithTimeout(context.Background(), 10*time.Second)
	desktopRelay, relayErr := resumeDesktopRelay(relayCtx, desktoprelay.Config{
		DataRoot: filepath.Join(config.Dir(), "desktop-relay"), Port: desktopRelayPort,
		Source: proxy.NewDesktopCredentialSource(proxyManager), Conversations: metadataIndex,
	})
	relayCancel()
	if relayErr != nil {
		log.Print("desktop relay unavailable; inspect local relay status")
	}

	settingsStore := settings.New(config.Dir())
	// Spend a banked reset only when the user allows it: the per-account
	// override wins over the global preference, and the feature is off
	// until one of them is turned on.
	proxyManager.SetAutoUseResetPolicy(func(a store.Account) bool {
		return a.AutoUseResetEnabled(settingsStore.Load().AutoUseReset)
	})
	// Claude Code's login and Claude Desktop conversations move off an
	// account that runs out of usage, unless the user turned that off.
	proxyManager.SetClaudeAutoSwitch(settingsStore.AutoSwitchClaude)
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
		if err := proxyManager.SetManagementKey(managementKey); err != nil {
			log.Fatalf("persist proxy management key: %v", err)
		}
	}
	// Successful logins persist immediately from the callback goroutine;
	// the first account of a provider automatically becomes active.
	logins := login.New(func(a *store.Account, reloginTarget string) error {
		var err error
		if reloginTarget != "" {
			err = proxyManager.ReplaceReloginAccount(a, reloginTarget)
		} else {
			err = proxyManager.ReplaceAccount(*a)
		}
		if err != nil {
			return err
		}
		if proxyManager.ActiveID(a.Provider) == "" {
			return proxyManager.Activate(a.ID)
		}
		return nil
	})
	mgmtAPI := &mgmtapi.API{Store: st, Proxy: proxyManager, Logins: logins, ManagementKey: managementKey}

	updater := update.New(version)

	// Background usage sync: the server owns freshness, so the web UI and
	// the menu bar always agree without waiting for a client to poll.
	// A Mac that uses another Switcher's accounts leaves token refreshes and
	// usage polling to that host, so the same account is never refreshed twice.
	remoteClient := remote.NewClient(filepath.Join(config.Dir(), "remote"), managementKey)
	go func() {
		if !remoteClient.Connected() {
			proxyManager.RefreshUsageAll(context.Background())
		}
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if !remoteClient.Connected() {
				proxyManager.RefreshUsageAll(context.Background())
			}
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
		DesktopRelay: desktopRelay, DesktopSettingsPath: desktopSettingsPath(),
		DesktopSessionMetadata: metadataIndex,
		ManagementKey:          managementKey, Version: version, Updater: updater, Usage: usageService,
		Settings: settingsStore, Port: port, RemoteClient: remoteClient,
	}

	// Each provider with a browser redirect has its own callback listener;
	// the ports are fixed by the OAuth clients' registered redirect URIs.
	// Codex registers /auth/callback, Claude /callback, Antigravity
	// /oauth-callback, and Gemini /oauth2callback; all complete the same
	// flow, so every path is served on every listener.
	callbackMux := http.NewServeMux()
	for _, path := range []string{"/auth/callback", "/callback", "/oauth-callback", "/oauth2callback"} {
		callbackMux.HandleFunc("GET "+path, callbackHandler(logins))
	}
	for _, port := range []int{
		config.CallbackPort, config.ClaudeCallbackPort,
		config.AntigravityCallbackPort, config.GeminiCallbackPort,
	} {
		go serveCallback(callbackMux, port)
	}

	mux := http.NewServeMux()
	api.Register(mux)
	mgmtAPI.Register(mux) // CLIProxyAPI-compatible hub surface for T3 Code
	mux.Handle("/codex/", proxyManager)
	mux.Handle("/claude/", proxyManager)
	mux.Handle("/grok/", proxyManager)
	mux.Handle("/opencode/", proxyManager)
	mux.Handle("/antigravity/", proxyManager)
	mux.Handle("/gemini/", proxyManager)
	mux.Handle("/copilot/", proxyManager)

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
	registerWebRoutes(mux, static, settingsStore)

	// Sharing: paired devices reach account views, provider proxies, the hub
	// (with this Switcher's management key) and Claude inference here.
	shared := http.NewServeMux()
	shared.HandleFunc("/remote/anthropic/", func(w http.ResponseWriter, r *http.Request) {
		// Paired Macs may send Claude messages and token counts, nothing else.
		path := strings.TrimPrefix(r.URL.Path, "/remote/anthropic")
		if r.Method != http.MethodPost || (path != "/v1/messages" && path != "/v1/messages/count_tokens") {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
		account := r.Header.Get(remote.AccountHeader)
		r.Header.Del(remote.AccountHeader)
		if account == "" {
			account = proxyManager.ActiveID("claude")
		}
		// An account that ran out of usage hands over to the one with the
		// most usage left, as it does for this Mac's own Desktop.
		account = proxyManager.ClaudeRoute(account)
		if desktopRelay == nil || account == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"The Switcher host has no Claude account selected"}}`)
			return
		}
		desktopRelay.ServeAccount(w, r, path, account)
	})
	shared.Handle("/v0/management/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+managementKey)
		mux.ServeHTTP(w, r)
	}))
	shared.Handle("/", mux)
	remoteHost, err := remote.NewHost(filepath.Join(config.Dir(), "remote"), remote.DefaultPort, shared)
	if err != nil {
		log.Printf("sharing unavailable: %v", err)
	} else {
		api.RemoteHost = remoteHost
		// The optional Tailscale add-on is downloaded only when turned on.
		tailnet := remote.NewTailnet(filepath.Join(config.Dir(), "remote"), version, remote.DefaultPort)
		api.Tailnet = tailnet
		// The phone dashboard: the add-on passes requests from the owner's
		// devices to this loopback listener with their identity and its key.
		// Its handler talks to the API without the local browser's trust.
		phoneAccess := phone.New(filepath.Join(config.Dir(), "remote", "phone.json"))
		if phoneListener, err := net.Listen("tcp", "127.0.0.1:0"); err != nil {
			log.Printf("phone access unavailable: %v", err)
		} else {
			api.Phone = phoneAccess
			machine := remote.MachineName()
			phoneServer := &http.Server{Handler: &phone.Handler{Access: phoneAccess, Key: tailnet.PhoneKey,
				Inner: remoteClient.Middleware(api.LocalStateFields, mux), Static: static, Mac: func() string { return machine }},
				ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
			go phoneServer.Serve(phoneListener)
			tailnet.ServePhone(phoneListener.Addr().String(), phoneAccess.Enabled)
		}
		remoteHost.AlsoReachableAt(tailnet.Addresses)
		remoteClient.UseTailnet(tailnet.Dial)
		remoteHost.Start()
		tailnet.ForwardWhen(remoteHost.Listening)
		tailnet.Start()
		defer remoteHost.Close()
		defer tailnet.Close()
	}
	if desktopRelay != nil {
		remoteClient.OnChange(func(connected bool) {
			if connected {
				desktopRelay.SetRemote(remoteClient.Inference)
			} else {
				desktopRelay.SetRemote(nil)
			}
		})
	}

	// Hardening headers on every response from the main listeners. The UI
	// has no inline scripts (the theme init lives in app.js), so the CSP
	// stays strict; styles need unsafe-inline for the SVG chart styling.
	hardened := hardenedHeaders(remoteClient.Middleware(api.LocalStateFields, mux))

	log.Printf("Switcher v%s running: http://127.0.0.1:%d (codex proxy on the same port under /v1)", version, port)

	// Listener topology: the loopback listener is always plain HTTP (the
	// CLIs, T3 Code, and the menu bar app need no configuration), and the
	// optional LAN listener is always TLS. Binding the LAN without a
	// password on disk is refused: no TOFU window. LocalOnly runs outside
	// the auth gate so rebinding checks happen pre-auth.
	gate := &server.AuthGate{Store: settingsStore, DesktopManagementKey: managementKey}
	loopback := server.Listener{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: server.ControlRequests(server.LocalOnlyWith(server.LocalOptions{Port: port}, gate.Wrap(hardened))),
	}
	listeners := []server.Listener{loopback}
	network, settingsErr := settingsStore.Snapshot()
	if settingsErr == nil && network.AuthEnabled && network.BindLAN && network.TLS && network.PasswordHash != "" {
		if lan := server.LANAddress(); lan != "" {
			cert, err := server.EnsureTLSCert(config.Dir(), lan, port)
			if err != nil {
				log.Printf("lan listener: tls setup failed: %v", err)
			} else {
				listeners = append(listeners, server.Listener{
					Addr:    fmt.Sprintf("%s:%d", lan, port),
					TLS:     true,
					Cert:    cert,
					Handler: server.ControlRequests(server.LocalOnlyWith(server.LocalOptions{Port: port, LANHost: lan}, gate.WrapLAN(hardened))),
				})
				log.Printf("lan listener: https://%s:%d (self-signed)", lan, port)
			}
		} else {
			log.Printf("lan binding requested but no non-loopback IPv4 address found")
		}
	}
	shutdown, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.ServeAll(listeners) }()
	var serveErr error
	select {
	case <-shutdown.Done():
	case serveErr = <-serveErrors:
	}
	if desktopRelay != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := desktopRelay.Close(closeCtx); err != nil {
			log.Print("desktop relay shutdown incomplete")
		}
		cancel()
	}
	if serveErr != nil {
		log.Fatalf("server: %v", serveErr)
	}
}

// registerWebRoutes serves a standalone login without exposing index.html or
// app.js. AuthGate decides whether the app and its assets may be requested.
func registerWebRoutes(mux *http.ServeMux, static fs.FS, preferences *settings.Store) {
	loginFile := func(name, contentType string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if name == "login.html" && (preferences == nil || !preferences.Enabled()) {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			content, err := fs.ReadFile(static, name)
			if err != nil {
				http.Error(w, "login page unavailable", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(content)
		}
	}
	mux.HandleFunc("GET /login", loginFile("login.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /login.css", loginFile("login.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /login.js", loginFile("login.js", "text/javascript; charset=utf-8"))
	staticHandler := http.FileServerFS(static)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Authenticated HTML must not linger as a back/forward cached page
		// after logout. Without a password, keep the historic revalidation.
		if preferences != nil && preferences.Enabled() {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		staticHandler.ServeHTTP(w, r)
	}))
}

// hardenedHeaders sets the static-UI hardening headers on every response:
// nosniff and a strict CSP. The UI ships no inline scripts (app.js is an
// external module), so script-src allows 'self' only.
func hardenedHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// serveCallback starts a localhost OAuth callback listener. A failure to
// bind (e.g. the port is taken) only disables logins, not the server.
func serveCallback(mux *http.ServeMux, port int) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if err := callbackServer(mux, addr).ListenAndServe(); err != nil {
		log.Printf("oauth callback listener on %s unavailable: %v", addr, err)
	}
}

func callbackServer(handler http.Handler, addr string) *http.Server {
	return &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second}
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
		// `switcher login finish` delivers an address pasted from another
		// device and reads the outcome as JSON.
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			w.Header().Set("Content-Type", "application/json")
			answer := map[string]any{"ok": err == nil, "state": state}
			if err != nil {
				answer["error"] = err.Error()
			}
			json.NewEncoder(w).Encode(answer)
			return
		}
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
