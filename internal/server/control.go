package server

import (
	"net/http"
	"strings"
	"time"
)

const controlBodyTimeout = 10 * time.Second

// ControlRequests bounds non-inference request bodies, including management
// controls and requests rejected by the outer guards. Provider routes retain
// their streaming reads.
func ControlRequests(next http.Handler) http.Handler {
	return controlRequests(next, controlBodyTimeout)
}

func controlRequests(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if nativeRoute(r.URL.Path) && !strings.HasPrefix(r.URL.Path, "/v0/management/") {
			next.ServeHTTP(w, r)
			return
		}
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(time.Now().Add(timeout))
		defer func() {
			// Close while the deadline is still set. net/http may otherwise
			// drain an unfinished body indefinitely after a rejected request.
			_ = r.Body.Close()
			_ = controller.SetReadDeadline(time.Time{})
		}()
		next.ServeHTTP(w, r)
	})
}
