package desktoprelay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

const maxSetupSettings = 1 << 20

var setupKeys = []string{"HTTPS_PROXY", "NODE_EXTRA_CA_CERTS"}

type setupDocument struct {
	root  map[string]json.RawMessage
	env   map[string]json.RawMessage
	shape string
}

func parseSetupDocument(data []byte, exists bool) (setupDocument, error) {
	d := setupDocument{root: make(map[string]json.RawMessage), env: make(map[string]json.RawMessage), shape: "absent"}
	if !exists {
		return d, nil
	}
	if len(data) > maxSetupSettings || !uniqueJSON(data) || json.Unmarshal(data, &d.root) != nil || d.root == nil || bytes.TrimSpace(data)[0] != '{' {
		return d, setupError("setup_invalid_settings", ErrUnavailable)
	}
	if raw, ok := d.root["env"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			d.shape = "null"
		} else {
			if json.Unmarshal(raw, &d.env) != nil || d.env == nil || bytes.TrimSpace(raw)[0] != '{' {
				return d, setupError("setup_invalid_settings", ErrUnavailable)
			}
			d.shape = "object"
		}
	}
	return d, nil
}

// RawMessage's normal MarshalJSON compacts nested values. Assemble only the
// changed objects so numbers, unknown extensions and nested values stay exact.
func setupObject(fields map[string]json.RawMessage) []byte {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i != 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		b.Write(key)
		b.WriteByte(':')
		b.Write(fields[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

func (d setupDocument) values() map[string]json.RawMessage {
	out := make(map[string]json.RawMessage)
	for _, k := range setupKeys {
		if v, ok := d.env[k]; ok {
			out[k] = append(json.RawMessage(nil), v...)
		}
	}
	return out
}

func setupStrings(env map[string]string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage)
	for k, v := range env {
		out[k], _ = json.Marshal(v)
	}
	return out
}

func (d setupDocument) matches(values map[string]json.RawMessage) bool {
	for _, k := range setupKeys {
		a, aok := d.env[k]
		b, bok := values[k]
		if aok != bok {
			return false
		}
		if aok {
			a, b = bytes.TrimSpace(a), bytes.TrimSpace(b)
			// Native writers can re-encode the same env string with escaped
			// slashes or Unicode. Ownership follows its decoded value.
			if len(a) > 0 && len(b) > 0 && a[0] == '"' && b[0] == '"' {
				var as, bs string
				if json.Unmarshal(a, &as) != nil || json.Unmarshal(b, &bs) != nil || as != bs {
					return false
				}
				continue
			}
			// Only strings/null are managed normally, but the baseline can hold
			// arbitrary JSON. Compare exact numeric spelling without float64.
			var ac, bc bytes.Buffer
			if json.Compact(&ac, a) != nil || json.Compact(&bc, b) != nil || !bytes.Equal(ac.Bytes(), bc.Bytes()) {
				return false
			}
		}
	}
	return true
}

func (d setupDocument) patch(values map[string]json.RawMessage, emptyShape string) ([]byte, error) {
	for _, k := range setupKeys {
		if v, ok := values[k]; ok {
			d.env[k] = v
		} else {
			delete(d.env, k)
		}
	}
	if len(d.env) == 0 && emptyShape != "object" {
		if emptyShape == "null" {
			d.root["env"] = json.RawMessage("null")
		} else {
			delete(d.root, "env")
		}
	} else {
		d.root["env"] = setupObject(d.env)
	}
	out := append(setupObject(d.root), '\n')
	if len(out) > maxSetupSettings {
		return nil, setupError("setup_invalid_settings", ErrUnavailable)
	}
	return out, nil
}

func setupDigest(data []byte, exists bool) string {
	b := byte(0)
	if exists {
		b = 1
	}
	h := sha256.New()
	h.Write([]byte{b})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func setupConflict() error { return setupError("setup_changed", ErrConflict) }

func setupFailure(err error) error {
	if err == nil {
		return nil
	}
	var own *SetupError
	if errors.As(err, &own) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, ErrBusy) {
		return setupError("setup_busy", ErrBusy)
	}
	if errors.Is(err, errCommitUncertain) {
		return errors.Join(setupError("setup_unavailable", ErrUnavailable), errCommitUncertain)
	}
	return setupError("setup_unavailable", ErrUnavailable)
}
