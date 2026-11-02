package store

import "encoding/json"

// SecretMask stands for protect_password wherever a settings blob leaves the
// panel (the API): the password is never echoed. A blob written back with the
// mask keeps the stored password (KeepSecret).
const SecretMask = "********"

const protectKey = "protect_password"

// MaskSettings hides protect_password in a settings blob; a blob without one
// (or one that is not a JSON object) is returned as is.
func MaskSettings(blob string) string {
	return rewriteSecret(blob, func(v string) (string, bool) { return SecretMask, v != "" })
}

// KeepSecret returns blob with a masked protect_password put back to the one
// in stored, so a read-edit-write round trip does not clobber the password.
func KeepSecret(stored, blob string) string {
	var old map[string]json.RawMessage
	if json.Unmarshal([]byte(stored), &old) != nil {
		return blob
	}
	var pw string
	if json.Unmarshal(old[protectKey], &pw) != nil {
		return blob
	}
	return rewriteSecret(blob, func(v string) (string, bool) { return pw, v == SecretMask })
}

func rewriteSecret(blob string, f func(v string) (string, bool)) string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(blob), &m) != nil {
		return blob
	}
	var v string
	if json.Unmarshal(m[protectKey], &v) != nil {
		return blob
	}
	nv, ok := f(v)
	if !ok {
		return blob
	}
	m[protectKey], _ = json.Marshal(nv)
	b, err := json.Marshal(m)
	if err != nil {
		return blob
	}
	return string(b)
}
