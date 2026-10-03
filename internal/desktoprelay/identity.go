package desktoprelay

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

type identityView struct {
	SessionID string
	ParentID  string
	AgentID   string
	Model     string
	Thread    bool
}

var errIdentity = errors.New("ambiguous or unsupported session identity")
var errDecodedBounds = errors.New("decoded request body exceeds relay bounds")

// Only routing fields are decoded. The forwarding owner retains the original
// bytes, including compression, unknown extensions and signed content.
func identity(r *http.Request, body []byte) (identityView, error) {
	var v identityView
	encoding := r.Header.Values("Content-Encoding")
	if len(encoding) > 1 {
		return v, errIdentity
	}
	if len(encoding) == 1 && encoding[0] != "" && encoding[0] != "identity" {
		if encoding[0] != "gzip" {
			return v, errIdentity
		}
		z, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return v, errIdentity
		}
		decoded, err := io.ReadAll(io.LimitReader(z, MaxBodyBytes+1))
		z.Close()
		if len(decoded) > MaxBodyBytes {
			return v, errDecodedBounds
		}
		if err != nil {
			return v, errIdentity
		}
		body = decoded
	}
	top, err := uniqueObject(body)
	if err != nil {
		return v, errIdentity
	}
	_, v.Thread = top["thread"]
	if raw, ok := top["model"]; ok {
		if json.Unmarshal(raw, &v.Model) != nil || len(v.Model) > 256 {
			return v, errIdentity
		}
	}
	var fromMetadata string
	if raw, ok := top["metadata"]; ok && string(raw) != "null" {
		meta, err := uniqueObject(raw)
		if err != nil {
			return v, errIdentity
		}
		if raw, ok := meta["user_id"]; ok {
			var inner string
			if json.Unmarshal(raw, &inner) != nil || len(inner) > 8192 {
				return v, errIdentity
			}
			user, err := uniqueObject([]byte(inner))
			if err != nil {
				return v, errIdentity
			}
			if raw, ok := user["session_id"]; ok {
				if json.Unmarshal(raw, &fromMetadata) != nil {
					return v, errIdentity
				}
				fromMetadata = strings.ToLower(fromMetadata)
				if !validUUID(fromMetadata) {
					return v, errIdentity
				}
			} else {
				return v, errIdentity
			}
			if raw, ok := user["parent_session_id"]; ok {
				if json.Unmarshal(raw, &v.ParentID) != nil {
					return v, errIdentity
				}
				v.ParentID = strings.ToLower(v.ParentID)
				if !validUUID(v.ParentID) {
					return v, errIdentity
				}
			}
			if raw, ok := user["agent_id"]; ok {
				if json.Unmarshal(raw, &v.AgentID) != nil || len(v.AgentID) > 128 {
					return v, errIdentity
				}
			}
		}
	}
	values := r.Header.Values("X-Claude-Code-Session-Id")
	if len(values) > 1 {
		return v, errIdentity
	}
	if len(values) == 1 {
		v.SessionID = strings.ToLower(values[0])
		if !validUUID(v.SessionID) {
			return v, errIdentity
		}
	}
	if fromMetadata != "" {
		if v.SessionID != "" && v.SessionID != fromMetadata {
			return v, errIdentity
		}
		v.SessionID = fromMetadata
	}
	return v, nil
}

func uniqueObject(b []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errIdentity
	}
	out := make(map[string]json.RawMessage)
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, errIdentity
		}
		s, ok := key.(string)
		if !ok {
			return nil, errIdentity
		}
		if _, exists := out[s]; exists {
			return nil, errIdentity
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, errIdentity
		}
		out[s] = value
	}
	if _, err = d.Token(); err != nil {
		return nil, errIdentity
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errIdentity
	}
	return out, nil
}
