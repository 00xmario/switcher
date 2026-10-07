package remote

import (
	"bufio"
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bonjour runs through macOS's own dns-sd tool, so Switcher needs no mDNS
// library. Other systems simply skip discovery; pairing by address works.
const serviceType = "_switcher._tcp"
const dnssd = "/usr/bin/dns-sd"

// Found is a Switcher host advertised on the local network.
type Found struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Port    int    `json:"port"`
	Self    bool   `json:"self,omitempty"`
	fp      string
}

// advertise registers this host until the returned stop function runs.
func advertise(name string, port int, fp string) func() {
	if runtime.GOOS != "darwin" {
		return func() {}
	}
	cmd := exec.Command(dnssd, "-R", name, serviceType, "local", strconv.Itoa(port), "v=1", "fp="+fp[:16])
	if err := cmd.Start(); err != nil {
		return func() {}
	}
	go cmd.Wait()
	return func() { cmd.Process.Kill() }
}

var (
	browseLine  = regexp.MustCompile(`\sAdd\s+\d+\s+\d+\s+\S+\s+` + regexp.QuoteMeta(serviceType) + `\.\s+(.+)$`)
	reachedLine = regexp.MustCompile(`can be reached at (\S+?)\.?:(\d+)`)
	fpField     = regexp.MustCompile(`\bfp=([0-9a-f]{16})\b`)
)

// Discover lists Switcher hosts on the local network within about two seconds.
// selfFP marks this Mac's own advertisement.
func Discover(ctx context.Context, selfFP string) []Found {
	if runtime.GOOS != "darwin" {
		return nil
	}
	names := map[string]bool{}
	run(ctx, 1500*time.Millisecond, func(line string) {
		if m := browseLine.FindStringSubmatch(line); m != nil {
			names[strings.TrimSpace(m[1])] = true
		}
	}, "-B", serviceType, "local")
	var mu sync.Mutex
	var found []Found
	var wg sync.WaitGroup
	for name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			f := Found{Name: name}
			run(ctx, 1500*time.Millisecond, func(line string) {
				if m := reachedLine.FindStringSubmatch(line); m != nil {
					f.Address = m[1]
					f.Port, _ = strconv.Atoi(m[2])
				}
				if m := fpField.FindStringSubmatch(line); m != nil {
					f.fp = m[1]
				}
			}, "-L", name, serviceType, "local")
			if f.Address == "" {
				return
			}
			f.Self = selfFP != "" && f.fp != "" && strings.HasPrefix(selfFP, f.fp)
			mu.Lock()
			found = append(found, f)
			mu.Unlock()
		}(name)
	}
	wg.Wait()
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	return found
}

// run streams dns-sd output for at most d; dns-sd itself never exits.
func run(ctx context.Context, d time.Duration, line func(string), args ...string) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	cmd := exec.CommandContext(ctx, dnssd, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if cmd.Start() != nil {
		return
	}
	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		line(scanner.Text())
	}
	cmd.Wait()
}
