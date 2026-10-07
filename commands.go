package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"switcher/internal/cli"
	"switcher/internal/config"
	"switcher/internal/settings"
)

// runCommand runs a command-line command against the local server.
func runCommand(args []string, port int) int {
	exe, _ := os.Executable()
	return cli.Run(args, cli.Options{
		Port:          port,
		Version:       version,
		Interactive:   isTerminal(os.Stdin) && isTerminal(os.Stdout),
		StdinTerminal: isTerminal(os.Stdin),
		DeviceToken: func() string {
			token, _ := settings.New(config.Dir()).ReadDeviceToken()
			return token
		},
		Start:         func() error { return startInBackground(port) },
		CallbackPorts: []int{config.CallbackPort, config.ClaudeCallbackPort, config.AntigravityCallbackPort, config.GeminiCallbackPort},
		Executable:    exe,
	})
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// startInBackground opens the Switcher app, or runs this binary as a
// detached server when there is no app or no desktop session (plain SSH).
// Proxy variables are dropped so Switcher's own requests never go through
// its Desktop relay.
func startInBackground(port int) error {
	env := []string{}
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(key) {
		case "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "NODE_EXTRA_CA_CERTS":
			continue
		}
		env = append(env, kv)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Through the switcher link, find the app the binary really lives in.
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	if runtime.GOOS == "darwin" && port == config.DefaultPort {
		app := "/Applications/Switcher.app"
		if i := strings.Index(exe, ".app/Contents/MacOS/"); i >= 0 {
			app = exe[:i+len(".app")]
		}
		if _, err := os.Stat(app); err == nil {
			open := exec.Command("/usr/bin/open", "-g", app)
			open.Env = env
			if open.Run() == nil {
				return nil
			}
		}
	}
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(config.Dir(), "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	server := exec.Command(exe, "--port", strconv.Itoa(port))
	server.Env, server.Stdout, server.Stderr = env, logFile, logFile
	server.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := server.Start(); err != nil {
		return err
	}
	return server.Process.Release()
}
