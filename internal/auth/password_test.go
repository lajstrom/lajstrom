package auth

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

// phc is the form of a new hash: the parameters of hashParams, a 16-byte salt and a 32-byte
// key in unpadded base64.
var phc = regexp.MustCompile(`^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`)

// TestHashPassword checks the hashing parameters: OWASP's first recommendation for argon2id,
// in a PHC string, with a random salt.
func TestHashPassword(t *testing.T) {
	const password = "correct horse battery"
	h1, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if !phc.MatchString(h1) {
		t.Errorf("hash %q is not $argon2id$v=19$m=19456,t=2,p=1$<16-byte salt>$<32-byte key>", h1)
	}
	h2, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("two hashes of one password are equal: the salt is not random")
	}
	for _, tt := range []struct {
		password string
		want     bool
	}{
		{password, true},
		{password + " ", false},
		{"Correct horse battery", false},
		{"", false},
	} {
		got, err := VerifyPassword(h1, tt.password)
		if err != nil || got != tt.want {
			t.Errorf("VerifyPassword(%q) = %v, %v; want %v", tt.password, got, err, tt.want)
		}
	}
}

// TestVerifyOtherParameters checks that the parameters are read from the hash: a hash made
// with other parameters verifies, among them the PHC string of the reference implementation's
// test vector for argon2id (phc-winner-argon2, src/test.c: "password", "somesalt", t=2,
// m=65536, p=1), which also proves that the key is argon2id's.
func TestVerifyOtherParameters(t *testing.T) {
	const reference = "$argon2id$v=19$m=65536,t=2,p=1$c29tZXNhbHQ$CTFhFdXPJO1aFaMaO6Mm5c8y7cJHAph8ArZWb2GRPPc"
	unicode := "ünïcödé pässwörd"
	tests := []struct {
		name, hash, password string
	}{
		{name: "reference vector", hash: reference, password: "password"},
		{name: "small memory, 3 passes, 2 threads, short salt and key",
			hash:     hashWith(argonParams{memory: 64, time: 3, threads: 2, saltLen: 8, keyLen: 16}, "hunter2hunter2"),
			password: "hunter2hunter2"},
		{name: "long key",
			hash:     hashWith(argonParams{memory: 32, time: 1, threads: 1, saltLen: 32, keyLen: 64}, unicode),
			password: unicode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if ok, err := VerifyPassword(tt.hash, tt.password); err != nil || !ok {
				t.Errorf("VerifyPassword(right password) = %v, %v; want true", ok, err)
			}
			if ok, err := VerifyPassword(tt.hash, tt.password+"x"); err != nil || ok {
				t.Errorf("VerifyPassword(wrong password) = %v, %v; want false", ok, err)
			}
		})
	}
}

// TestVerifyMalformed checks that a hash VerifyPassword cannot use is an error, never false,
// and that the error does not contain the hash.
func TestVerifyMalformed(t *testing.T) {
	const (
		salt = "c29tZXNhbHQ"                                 // 8 bytes
		key  = "CTFhFdXPJO1aFaMaO6Mm5c8y7cJHAph8ArZWb2GRPPc" // 32 bytes
	)
	tests := []struct{ name, hash, want string }{
		{"empty", "", "want $argon2id$"},
		{"bcrypt", "$2a$10$abcdefghijklmnopqrstuuabcdefghijklmnopqrstuvwxyzABCDE", "want $argon2id$"},
		{"no leading dollar", "argon2id$v=19$m=64,t=1,p=1$" + salt + "$" + key + "$", "want $argon2id$"},
		{"argon2i", "$argon2i$v=19$m=64,t=1,p=1$" + salt + "$" + key, "not argon2id"},
		{"version 16", "$argon2id$v=16$m=64,t=1,p=1$" + salt + "$" + key, "version is not v=19"},
		{"no version", "$argon2id$m=64,t=1,p=1$" + salt + "$" + key, "want $argon2id$"},
		{"two parameters", "$argon2id$v=19$m=64,t=1$" + salt + "$" + key, "not m=…,t=…,p=…"},
		{"parameters out of order", "$argon2id$v=19$t=1,m=64,p=1$" + salt + "$" + key, "parameter m"},
		{"zero passes", "$argon2id$v=19$m=64,t=0,p=1$" + salt + "$" + key, "parameter t"},
		{"zero threads", "$argon2id$v=19$m=64,t=1,p=0$" + salt + "$" + key, "parameter p"},
		{"256 threads", "$argon2id$v=19$m=64,t=1,p=256$" + salt + "$" + key, "p is more than 255"},
		{"negative memory", "$argon2id$v=19$m=-64,t=1,p=1$" + salt + "$" + key, "parameter m"},
		{"memory beyond 32 bits", "$argon2id$v=19$m=4294967296,t=1,p=1$" + salt + "$" + key, "parameter m"},
		{"padded salt", "$argon2id$v=19$m=64,t=1,p=1$" + salt + "=$" + key, "salt"},
		{"short salt", "$argon2id$v=19$m=64,t=1,p=1$c29tZQ$" + key, "salt"},
		{"short key", "$argon2id$v=19$m=64,t=1,p=1$" + salt + "$c29tZXNhbHQ", "key"},
		{"key not base64", "$argon2id$v=19$m=64,t=1,p=1$" + salt + "$" + strings.Repeat("!", 43), "key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, err := VerifyPassword(tt.hash, "password")
			if !errors.Is(err, ErrMalformedHash) || ok {
				t.Fatalf("VerifyPassword = %v, %v; want ErrMalformedHash", ok, err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not say %q", err, tt.want)
			}
			if tt.hash != "" && strings.Contains(err.Error(), tt.hash) {
				t.Errorf("error %q contains the hash", err)
			}
		})
	}
}

// TestPasswordBounds checks that passwords are 8 to 1024 bytes of UTF-8, that HashPassword
// refuses the others, and that no error contains the password.
func TestPasswordBounds(t *testing.T) {
	tests := []struct {
		name, password, want string // want: "" for valid, else a substring of the error
	}{
		{"8 bytes", "12345678", ""},
		{"1024 bytes", strings.Repeat("a", 1024), ""},
		{"multibyte", "пароль!!", ""}, // 6 Cyrillic letters, 14 bytes
		{"spaces and colons", "a: b: c: d", ""},
		{"empty", "", "at least 8 bytes"},
		{"7 bytes", "1234567", "at least 8 bytes"},
		{"1025 bytes", strings.Repeat("a", 1025), "at most 1024 bytes"},
		{"not UTF-8", "abcdefg\xff", "must be UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckPassword(tt.password)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("CheckPassword: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) || !strings.HasPrefix(err.Error(), "password: ") {
				t.Fatalf("CheckPassword = %v, want ErrInvalid saying password: … %s", err, tt.want)
			}
			if tt.password != "" && strings.Contains(err.Error(), tt.password) {
				t.Errorf("error %q contains the password", err)
			}
			if h, err := HashPassword(tt.password); !errors.Is(err, ErrInvalid) || h != "" {
				t.Errorf("HashPassword = %q, %v; want ErrInvalid", h, err)
			}
		})
	}
}

// TestCheckName checks the user names: 1 to 64 bytes of [a-z0-9._-], the first a letter or a
// digit, deploy reserved.
func TestCheckName(t *testing.T) {
	tests := []struct {
		name string
		want string // "" for valid, else a substring of the error
	}{
		{"alice", ""},
		{"a", ""},
		{"0", ""},
		{"ci.bot_2-x", ""},
		{"deployer", ""},
		{strings.Repeat("a", 64), ""},
		{"", "must not be empty"},
		{strings.Repeat("a", 65), "at most 64 bytes"},
		{"Alice", "lowercase"},
		{"al ice", "lowercase"},
		{"al:ice", "lowercase"},
		{"-alice", "starting with a letter or a digit"},
		{".alice", "starting with a letter or a digit"},
		{"_alice", "starting with a letter or a digit"},
		{"alicé", "lowercase"},
		{"alice/bob", "lowercase"},
		{"deploy", "reserved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckName(tt.name)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("CheckName(%q): %v", tt.name, err)
			case tt.want != "" && (!errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) || !strings.HasPrefix(err.Error(), "name: ")):
				t.Fatalf("CheckName(%q) = %v, want ErrInvalid saying name: … %s", tt.name, err, tt.want)
			}
		})
	}
}
