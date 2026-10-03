package sessionmeta

import (
	"bytes"
	"encoding/json"
	"io"
)

// Claims are negative evidence only. Inspect every explicit top-level CLI ID,
// including duplicates, before display/schema decoding can discard a record.
// This never creates an association or decodes ignored prompt/string values.
// An unreadable or malformed record cannot prove alias absence, so historical
// fallback is unavailable until a complete readable snapshot can be obtained.
func (m *membership) trackClaims(data []byte) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		m.historyReadable = false
		return
	}
	for decoder.More() {
		if m.ctx.Err() != nil {
			m.historyReadable = false
			return
		}
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			m.historyReadable = false
			return
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			m.historyReadable = false
			return
		}
		if foldKey(key) == "CLISESSIONID" {
			value = bytes.TrimSpace(value)
			var id string
			// Null decodes into a Go string without an error. Require an actual
			// usable UUID string before treating this reserved field as readable.
			if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, &id) != nil || uuid(id) == "" {
				m.historyReadable = false
				continue
			}
			m.claimed[uuid(id)] = true
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || decoder.Decode(new(json.RawMessage)) != io.EOF {
		m.historyReadable = false
	}
}
