package usage

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Service owns the scan cache, the rate table, and the latest summaries per
// window. Scans are done in the background; readers always get the last
// summary without blocking.
type Service struct {
	mu       sync.Mutex
	summ     map[int]*Summary
	fresh    map[int]time.Time
	scanning map[int]bool
	cache    *ScanCache
	rates    RateTable
	ratesOK  bool
	dataDir  string
	client   *http.Client
}

// NewService builds the service; dataDir holds usage-scan-cache.json and
// model-rates.json.
func NewService(dataDir string, client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		// continue: cache writes will fail but scans still work
	}
	s := &Service{
		summ:     map[int]*Summary{},
		fresh:    map[int]time.Time{},
		scanning: map[int]bool{},
		cache:    LoadScanCache(filepath.Join(dataDir, "usage-scan-cache.json")),
		dataDir:  dataDir,
		client:   client,
	}
	return s
}

// Days returns the newest summary for a window, or nil when none exists.
// The caller decides staleness with Age.
func (s *Service) Get(days int) *Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.summ[days]
}

// Age reports how old the newest summary for a window is.
func (s *Service) Age(days int) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.fresh[days]
	if !ok {
		return 1<<63 - 1
	}
	return time.Since(t)
}

// Scan recomputes the summary for a window synchronously. Callers should
// run it in a goroutine; Get stays lock-free for readers.
func (s *Service) Scan(days int) *Summary {
	s.mu.Lock()
	if s.scanning[days] {
		s.mu.Unlock()
		return nil
	}
	s.scanning[days] = true
	rates := s.rates
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.scanning[days] = false
		s.mu.Unlock()
	}()

	if !s.ratesReady() {
		table, _ := LoadRates(filepath.Join(s.dataDir, "model-rates.json"), s.client)
		s.mu.Lock()
		s.rates = table
		s.ratesOK = len(table) > 0
		rates = table
		s.mu.Unlock()
	}

	until := time.Now()
	windowStart := until.AddDate(0, 0, -(days - 1))
	summary := SummaryFor(ResolveSources(), s.cache, rates, windowStart, until, time.Local)
	s.mu.Lock()
	s.summ[days] = summary
	s.fresh[days] = time.Now()
	s.mu.Unlock()
	return summary
}

func (s *Service) ratesReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ratesOK
}

// RefreshStale rescans windows older than maxAge. It returns the windows it
// refreshed. Intended for a background goroutine.
func (s *Service) RefreshStale(windows []int, olderThan time.Duration) []int {
	var refreshed []int
	for _, days := range windows {
		s.mu.Lock()
		t, ok := s.fresh[days]
		busy := s.scanning[days]
		s.mu.Unlock()
		if !ok || (time.Since(t) > olderThan && !busy) {
			s.Scan(days)
			refreshed = append(refreshed, days)
		}
	}
	return refreshed
}

// Sort helpers used by the API layer.
func SortStrings(v []string) { sort.Strings(v) }
