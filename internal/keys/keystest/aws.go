package keystest

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/lajstrom/lajstrom/internal/keys"
)

// AWSKMS returns an AWS KMS of the JSON 1.1 API over HTTP in front of k, for a client that goes
// to it with AWS_ENDPOINT_URL_KMS: DescribeKey (a key of ECC_NIST_P256), GetPublicKey and Sign.
// It does not check SigV4 beyond its presence. While k fails, it answers 503 ThrottlingException
// for an error that wraps keys.ErrKMSUnavailable and 400 AccessDeniedException for another.
func AWSKMS(k *KMS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			KeyID   string `json:"KeyId"`
			Message []byte
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") || json.NewDecoder(r.Body).Decode(&in) != nil {
			awsError(w, http.StatusBadRequest, "InvalidSignatureException")
			return
		}
		desc, err := k.Key(r.Context(), in.KeyID)
		if err != nil {
			failed(w, err)
			return
		}
		var out any
		switch r.Header.Get("X-Amz-Target") {
		case "TrentService.DescribeKey":
			out = map[string]any{"KeyMetadata": map[string]any{"Arn": desc.Name, "CreationDate": desc.Created.Unix(),
				"KeyUsage": "SIGN_VERIFY", "KeySpec": "ECC_NIST_P256", "KeyState": "Enabled", "SigningAlgorithms": []string{"ECDSA_SHA_256"}}}
		case "TrentService.GetPublicKey":
			der, _ := x509.MarshalPKIXPublicKey(desc.Public) // a P-256 key: never an error
			out = map[string]any{"KeyId": desc.Name, "PublicKey": der}
		case "TrentService.Sign":
			sig, err := k.Sign(r.Context(), in.KeyID, desc.Public, in.Message)
			if err != nil {
				failed(w, err)
				return
			}
			out = map[string]any{"KeyId": desc.Name, "Signature": sig}
		default:
			awsError(w, http.StatusBadRequest, "UnknownOperationException")
			return
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(out)
	})
}

// failed answers err of the KMS.
func failed(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, keys.ErrKMSUnavailable):
		awsError(w, http.StatusServiceUnavailable, "ThrottlingException")
	case strings.HasPrefix(err.Error(), "NotFoundException"):
		awsError(w, http.StatusBadRequest, "NotFoundException")
	default:
		awsError(w, http.StatusBadRequest, "AccessDeniedException")
	}
}

func awsError(w http.ResponseWriter, status int, typ string) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"__type": typ, "message": typ + " from keystest"})
}
