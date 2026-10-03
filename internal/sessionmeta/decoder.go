package sessionmeta

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"unicode"
)

const maxJSONDepth = 64

// Validate before typed decoding can apply encoding/json's last-key-wins rule.
// Every object key must be unique under the decoder's Unicode case folding,
// even when repeated values are identical. A duplicate anywhere rejects this
// metadata file, including a CLI index with an ambiguous nested row.
func decodeMetadata(ctx context.Context, data []byte, dst any) bool {
	return uniqueKeys(ctx, data, 0) && ctx.Err() == nil && json.Unmarshal(data, dst) == nil
}

func uniqueKeys(ctx context.Context, data []byte, depth int) bool {
	if ctx.Err() != nil {
		return false
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return false
	}
	if data[0] != '{' && data[0] != '[' {
		// Scalar values have already been syntax checked by the containing
		// decoder, or will be by the final typed decode. Do not decode ignored
		// values such as firstPrompt into strings or use their contents.
		return true
	}
	if depth >= maxJSONDepth {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return false
	}
	object := opening == json.Delim('{')
	seen := make(map[string]bool)
	for decoder.More() {
		if ctx.Err() != nil {
			return false
		}
		if object {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok {
				return false
			}
			key = foldKey(key)
			if seen[key] {
				return false
			}
			seen[key] = true
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || !uniqueKeys(ctx, value, depth+1) {
			return false
		}
	}
	closing, err := decoder.Token()
	if err != nil || object && closing != json.Delim('}') || !object && closing != json.Delim(']') {
		return false
	}
	return decoder.Decode(new(json.RawMessage)) == io.EOF && ctx.Err() == nil
}

// Choosing the smallest rune in each SimpleFold cycle matches encoding/json's
// folded field-name comparison, including non-ASCII aliases such as long s.
func foldKey(key string) string {
	return strings.Map(func(r rune) rune {
		for {
			next := unicode.SimpleFold(r)
			if next <= r {
				return next
			}
			r = next
		}
	}, key)
}
