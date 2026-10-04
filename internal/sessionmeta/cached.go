package sessionmeta

import "time"

// ResolveCached answers from the current snapshot without I/O, for the
// request path. A cold or busy snapshot returns nothing.
func (i *Index) ResolveCached(ids []string) map[string]string {
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
	for _, original := range ids {
		if conversation := i.cache.conversations[uuid(original)]; conversation != "" {
			out[original] = conversation
		}
	}
	return out
}
