package api

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

// The keys of a repo's policy and their bounds (docs/api.md, the policy keys). Each key has a
// reader elsewhere: maxFileSize format.MaxFileSize (the uploads of the frontends and ingest),
// blobRedirect format.BlobRedirect (every blob read), maven.sessionTimeout the publication
// sessions of ingest. The readers are lenient; the API is where a policy is checked.
const (
	keyMaxFileSize    = "maxFileSize"
	keyBlobRedirect   = "blobRedirect"
	keyMaven          = "maven"
	keySessionTimeout = "sessionTimeout"

	minSessionTimeout = time.Minute
	maxSessionTimeout = 24 * time.Hour
)

// policyKeys are the keys of a policy, and mavenKeys those of policy.maven, as the messages
// list them.
var (
	policyKeys = []string{keyMaxFileSize, keyBlobRedirect, keyMaven}
	mavenKeys  = []string{keySessionTimeout}
)

// policyObject is a policy decoded as JSON with numbers kept as written (json.Number), so that a
// large maxFileSize is not rounded through float64.
type policyObject = map[string]any

// checkPolicy validates p, the whole policy of a repo of format formatName, and returns it
// compacted with its keys sorted, or nil when it is empty, the JSON null, or an object without
// keys: the repo has no policy. A member whose value is null is no key, as in a merge patch.
// Each error wraps format.ErrInvalid and its message starts with the key path, policy.<key>.
func checkPolicy(formatName string, p json.RawMessage) (json.RawMessage, error) {
	obj, err := decodePolicy(p)
	if err != nil || obj == nil {
		return nil, err
	}
	return encodePolicy(formatName, mergeObject(policyObject{}, obj))
}

// decodePolicy returns p as an object, nil for empty or the JSON null; any other JSON value, or
// more than one, is refused.
func decodePolicy(p json.RawMessage) (policyObject, error) {
	p = bytes.TrimSpace(p)
	if len(p) == 0 || string(p) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.UseNumber()
	var obj policyObject
	if p[0] != '{' || dec.Decode(&obj) != nil || dec.Decode(new(any)) != io.EOF {
		return nil, invalid("policy: must be a JSON object or null")
	}
	return obj, nil
}

// mergeObject merges patch into target as RFC 7396 says and returns target. An object that the
// merge leaves without keys is removed with its key.
func mergeObject(target, patch policyObject) policyObject {
	for k, v := range patch {
		sub, isObject := v.(policyObject)
		switch {
		case v == nil:
			delete(target, k)
		case isObject:
			old, _ := target[k].(policyObject)
			if old == nil {
				old = policyObject{}
			}
			if merged := mergeObject(old, sub); len(merged) > 0 {
				target[k] = merged
			} else {
				delete(target, k)
			}
		default:
			target[k] = v
		}
	}
	return target
}

// encodePolicy checks obj, a policy without null members, and returns its JSON (keys sorted),
// or nil when it has no key.
func encodePolicy(formatName string, obj policyObject) (json.RawMessage, error) {
	if len(obj) == 0 {
		return nil, nil
	}
	if err := checkPolicyKeys(formatName, obj); err != nil {
		return nil, err
	}
	b, err := json.Marshal(obj)
	if err != nil { // the values are what json decoded
		return nil, invalid("policy: %s", err.Error())
	}
	return b, nil
}

// checkPolicyKeys checks the keys of obj in sorted order and returns the first error.
func checkPolicyKeys(formatName string, obj policyObject) error {
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		var err error
		switch v := obj[k]; k {
		case keyMaxFileSize:
			err = checkMaxFileSize(formatName, v)
		case keyBlobRedirect:
			if s, _ := v.(string); s != "auto" && s != "never" {
				err = invalid(`policy.blobRedirect: must be "auto" or "never"`)
			}
		case keyMaven:
			err = checkMavenPolicy(formatName, v)
		default:
			err = unknownKey("policy."+k, policyKeys)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// checkMaxFileSize checks policy.maxFileSize: an integer number of bytes, at least 1, on a repo
// whose uploads are checked (every format but oci).
func checkMaxFileSize(formatName string, v any) error {
	if formatName == formatOCI {
		return invalid("policy.maxFileSize: an oci repo is a pass-through, and nothing checks the size of what it stores")
	}
	n, ok := v.(json.Number)
	if ok {
		i, err := n.Int64()
		ok = err == nil && i >= 1
	}
	if !ok {
		return invalid("policy.maxFileSize: must be an integer number of bytes, at least 1")
	}
	return nil
}

// checkMavenPolicy checks policy.maven: an object, on a maven repo, whose sessionTimeout is a
// duration from minSessionTimeout to maxSessionTimeout.
func checkMavenPolicy(formatName string, v any) error {
	obj, ok := v.(policyObject)
	switch {
	case formatName != "maven":
		return invalid("policy.maven: only a maven repo has it, not a %s repo", formatName)
	case !ok:
		return invalid("policy.maven: must be a JSON object")
	}
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		if k != keySessionTimeout {
			return unknownKey("policy.maven."+k, mavenKeys)
		}
		s, _ := obj[k].(string)
		if d, err := time.ParseDuration(s); err != nil || d < minSessionTimeout || d > maxSessionTimeout {
			return invalid(`policy.maven.sessionTimeout: must be a duration from 1m to 24h, e.g. "45m"`)
		}
	}
	return nil
}

// unknownKey is the error of a key a policy does not have.
func unknownKey(path string, known []string) error {
	return invalid("%s: unknown key; the keys are %s", path, strings.Join(known, ", "))
}
