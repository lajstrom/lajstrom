package api

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/blobcrypt"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/policy"
)

// The keys of a repo's policy and their bounds (docs/api.md, the policy keys). Each key has a
// reader elsewhere: maxFileSize format.MaxFileSize (the uploads of the frontends and ingest),
// blobRedirect format.BlobRedirect (every blob read), maven.sessionTimeout and
// maven.maxSessionFiles the publication sessions of ingest, maven.versionPolicy the maven plugin
// (its deploy path and Admit), encryption format.EncryptionOf (every write, internal/keys). The
// readers are lenient but encryption's; the API is where a policy is checked.
const (
	keyMaxFileSize     = "maxFileSize"
	keyBlobRedirect    = "blobRedirect"
	keyMaven           = "maven"
	keySessionTimeout  = "sessionTimeout"
	keyMaxSessionFiles = "maxSessionFiles"
	keyVersionPolicy   = "versionPolicy"
	keyEnforcePull     = policy.KeyEnforcePull // docs/spec/policy.md §9
	keyQuarantine      = policy.KeyQuarantine
	keyEncryption      = "encryption"
	keyMode            = "mode"
	keyKey             = "key"
	keyRecipients      = "recipients"

	minSessionTimeout = time.Minute
	maxSessionTimeout = 24 * time.Hour
	// maxSessionFiles is the largest maven.maxSessionFiles: the manifest of a version has a layer
	// for each file and one for its metadata, and a manifest of /v2 has at most 1 000 layers
	// (facade.maxLayers), so a version stays one that /v2 takes back.
	maxSessionFiles = 999
	// maxRecipients bounds policy.encryption.recipients: each is one more JWE recipient of every
	// data key, in every layer's annotation.
	maxRecipients = 8
)

// policyKeys are the keys of a policy, and mavenKeys those of policy.maven, as the messages
// list them.
var (
	policyKeys     = []string{keyMaxFileSize, keyBlobRedirect, keyEncryption, keyMaven, keyEnforcePull, keyQuarantine}
	mavenKeys      = []string{keyMaxSessionFiles, keySessionTimeout, keyVersionPolicy}
	encryptionKeys = []string{keyKey, keyMode, keyRecipients}
)

// policyObject is a policy decoded as JSON with numbers kept as written (json.Number), so that a
// merge does not round a large maxFileSize through float64.
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

// mergePolicy applies patch, the policy of a PATCH, to stored, the repo's policy, as an RFC 7396
// JSON merge patch: a member of patch sets its key, a null member removes it, and objects merge
// key by key. The JSON null for patch removes the whole policy. The result is checked as
// checkPolicy checks a policy; nil means none is left.
func mergePolicy(formatName string, stored, patch json.RawMessage) (json.RawMessage, error) {
	obj, err := decodePolicy(patch)
	if err != nil || obj == nil {
		return nil, err
	}
	base, err := decodePolicy(stored)
	if err != nil || base == nil { // a stored policy that is not an object is replaced
		base = policyObject{}
	}
	return encodePolicy(formatName, mergeObject(base, obj))
}

// decodePolicy returns p as an object, nil for empty or the JSON null; any other JSON value, or
// more than one, is refused.
func decodePolicy(p json.RawMessage) (policyObject, error) { return decodeObject("policy", p) }

// decodeObject is decodePolicy for the member field of a body: an object, nil for empty or the
// JSON null, and an error naming field for anything else.
func decodeObject(field string, p json.RawMessage) (policyObject, error) {
	p = bytes.TrimSpace(p)
	if len(p) == 0 || string(p) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.UseNumber()
	var obj policyObject
	if p[0] != '{' || dec.Decode(&obj) != nil || dec.Decode(new(any)) != io.EOF {
		return nil, invalid("%s: must be a JSON object or null", field)
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
		case keyEnforcePull:
			if _, ok := v.(bool); !ok {
				err = invalid("policy.enforcePull: must be true or false")
			}
		case keyQuarantine:
			err = checkQuarantine(v)
		case keyEncryption:
			err = checkEncryption(formatName, v)
		default:
			err = unknownKey("policy."+k, policyKeys)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// checkQuarantine checks policy.quarantineNewVersions: a duration from policy.MinQuarantine to
// policy.MaxQuarantine, or "0s", none.
func checkQuarantine(v any) error {
	s, _ := v.(string)
	if d, err := time.ParseDuration(s); err != nil || (d != 0 && (d < policy.MinQuarantine || d > policy.MaxQuarantine)) {
		return invalid(`policy.quarantineNewVersions: must be a duration from 1m to 8760h, or "0s", e.g. "72h"`)
	}
	return nil
}

// checkMaxFileSize checks policy.maxFileSize: an integer number of bytes, at least 1, on a repo of
// any format, oci included: it bounds the blobs of an oci repo's /v2 uploads too (#246), and a
// repo of ML models raises it.
func checkMaxFileSize(_ string, v any) error {
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

// checkMavenPolicy checks policy.maven: an object, on a maven repo, whose keys are those of
// mavenKeys, each checked by checkMavenKey.
func checkMavenPolicy(formatName string, v any) error {
	obj, ok := v.(policyObject)
	switch {
	case formatName != "maven":
		return invalid("policy.maven: only a maven repo has it, not a %s repo", formatName)
	case !ok:
		return invalid("policy.maven: must be a JSON object")
	}
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		if err := checkMavenKey(k, obj[k]); err != nil {
			return err
		}
	}
	return nil
}

// checkMavenKey checks the key k of policy.maven: sessionTimeout is a duration from
// minSessionTimeout to maxSessionTimeout, maxSessionFiles an integer from 1 to maxSessionFiles,
// versionPolicy "release" or "snapshot"; any other key is unknown.
func checkMavenKey(k string, v any) error {
	switch k {
	case keySessionTimeout:
		s, _ := v.(string)
		if d, err := time.ParseDuration(s); err != nil || d < minSessionTimeout || d > maxSessionTimeout {
			return invalid(`policy.maven.sessionTimeout: must be a duration from 1m to 24h, e.g. "45m"`)
		}
	case keyMaxSessionFiles:
		n, ok := v.(json.Number)
		if ok {
			i, err := n.Int64()
			ok = err == nil && i >= 1 && i <= maxSessionFiles
		}
		if !ok {
			return invalid("policy.maven.maxSessionFiles: must be an integer from 1 to %d", maxSessionFiles)
		}
	case keyVersionPolicy:
		if s, _ := v.(string); s != "release" && s != "snapshot" {
			return invalid(`policy.maven.versionPolicy: must be "release" or "snapshot"`)
		}
	default:
		return unknownKey("policy.maven."+k, mavenKeys)
	}
	return nil
}

// checkEncryption checks policy.encryption (docs/api.md, ADR 0009): an object whose mode is
// "none" or "at-rest", at-rest not on an oci repo; key "" or a key URI of a KMS; recipients at
// most maxRecipients PEM public keys that blobcrypt takes, and at least one with at-rest, the
// recovery recipient (decided on #114). A group has no policy at all. The policy checked is the
// one the repo will have, a change merged in (mergePolicy), so that a change to at-rest and the
// removal of the last recipient are refused alike.
func checkEncryption(formatName string, v any) error {
	obj, ok := v.(policyObject)
	if !ok {
		return invalid("policy.encryption: must be a JSON object")
	}
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		if !slices.Contains(encryptionKeys, k) {
			return unknownKey("policy.encryption."+k, encryptionKeys)
		}
	}
	switch mode, _ := obj[keyMode].(string); {
	case mode == format.EncryptionAtRest && formatName == "oci":
		return invalid("policy.encryption.mode: an oci repo passes its manifests through and cannot encrypt them")
	case mode != format.EncryptionNone && mode != format.EncryptionAtRest:
		return invalid(`policy.encryption.mode: must be "none" or "at-rest"`)
	}
	if err := checkEncryptionKey(obj); err != nil {
		return err
	}
	if err := checkRecipients(obj); err != nil {
		return err
	}
	if list, _ := obj[keyRecipients].([]any); obj[keyMode] == format.EncryptionAtRest && len(list) == 0 {
		return invalid("policy.encryption.recipients: %s", config.AtRestNeedsRecipient)
	}
	return nil
}

// checkEncryptionKey checks policy.encryption.key: "" or a key URI of a KMS.
func checkEncryptionKey(obj policyObject) error {
	if v, ok := obj[keyKey]; ok {
		s, isString := v.(string)
		if !isString {
			return invalid(`policy.encryption.key: must be a key URI of a KMS, or "" for a key Lajstrom makes`)
		}
		if s != "" {
			if _, err := config.ParseKMSKey(s); err != nil {
				return invalid("policy.encryption.key: %s", err.Error())
			}
		}
	}
	return nil
}

// checkRecipients checks policy.encryption.recipients: at most maxRecipients PEM public keys that
// blobcrypt takes.
func checkRecipients(obj policyObject) error {
	if v, ok := obj[keyRecipients]; ok {
		list, isList := v.([]any)
		if !isList || len(list) > maxRecipients {
			return invalid("policy.encryption.recipients: must be a list of at most %d PEM public keys", maxRecipients)
		}
		for i, r := range list {
			s, _ := r.(string)
			if _, err := blobcrypt.ParseRecipient(s); err != nil {
				return invalid("policy.encryption.recipients[%d]: %s", i, strings.TrimPrefix(err.Error(), "blobcrypt: "))
			}
		}
	}
	return nil
}

// atRest reports whether the policy p, as checkPolicy returns it, has encryption mode at-rest.
func atRest(p json.RawMessage) bool {
	var pol struct {
		Encryption *format.EncryptionPolicy `json:"encryption"`
	}
	return len(p) > 0 && json.Unmarshal(p, &pol) == nil && pol.Encryption != nil && pol.Encryption.AtRest()
}

// unknownKey is the error of a key a policy does not have.
func unknownKey(path string, known []string) error {
	return invalid("%s: unknown key; the keys are %s", path, strings.Join(known, ", "))
}

// checkPolicyKind refuses the keys of a policy that a repo of kind does not have: quarantineNewVersions
// is a proxy repo's (docs/spec/policy.md §9).
func checkPolicyKind(kind string, p json.RawMessage) error {
	if len(p) == 0 || kind == index.KindProxy {
		return nil
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(p, &keys) != nil {
		return nil
	}
	if _, ok := keys[keyQuarantine]; ok {
		return invalid("policy.quarantineNewVersions: only a proxy repo has it, not a %s repo", kind)
	}
	return nil
}
