package sessionmeta

import (
	"context"
	"time"
)

// ResolveCached is the nonblocking request-path view. It performs no I/O,
// refresh, or cache mutation. Cold, expired, or busy snapshots yield no authority.
// known has the same trusted-core-only contract as ResolveHistorical. A status
// or management lookup must warm the snapshot outside the normal request path.
func (i *Index) ResolveCached(ids []string, known map[string]string) map[string]string {
	out := make(map[string]string)
	if i == nil || len(ids) == 0 {
		return out
	}
	select {
	case i.gate <- struct{}{}:
		defer func() { <-i.gate }()
	default:
		return out
	}
	if !time.Now().Before(i.expires) {
		return out
	}
	prior, disputed := canonicalKnown(context.Background(), known)
	for _, original := range ids {
		if alias := uuid(original); alias != "" {
			if group := i.cache.association(alias, prior, disputed); group != "" {
				out[original] = group
			}
		}
	}
	if !time.Now().Before(i.expires) {
		return make(map[string]string)
	}
	return out
}
