package auth

import (
	"errors"
	"strings"
	"testing"
)

// TestPasswordPolicyCheck: a password is UTF-8 of at most 1024 bytes and at least minLength
// characters, counted as code points (8 by default), that does not start with a token's prefix;
// with requireLettersAndDigits, it holds a letter of Unicode's category L and a digit of Nd, of
// any script (docs/spec/rbac.md §1, Local passwords). The error names the rule, never the
// password.
func TestPasswordPolicyCheck(t *testing.T) {
	pci := PasswordPolicy{MinLength: 12, RequireLettersAndDigits: true}
	for _, tt := range []struct {
		name     string
		policy   PasswordPolicy
		password string
		wantErr  string
	}{
		{"8 characters", PasswordPolicy{}, "12345678", ""},
		{"7 characters", PasswordPolicy{}, "1234567", "password: must be at least 8 characters (auth.passwords.minLength)"},
		{"empty", PasswordPolicy{}, "", "at least 8 characters"},
		{"12 bytes of 4 characters", PasswordPolicy{}, "日本語字", "at least 8 characters"},
		{"8 characters of 24 bytes", PasswordPolicy{}, "日本語の文字です", ""},
		{"a combining mark counts", PasswordPolicy{}, "caféabc", ""},
		{"1024 bytes", PasswordPolicy{}, strings.Repeat("x", 1024), ""},
		{"1025 bytes", PasswordPolicy{}, strings.Repeat("x", 1025), "password: must be at most 1024 bytes"},
		{"not UTF-8", PasswordPolicy{}, "pass\xffword", "password: must be UTF-8"},
		{"a token's prefix", PasswordPolicy{}, PATPrefix + "something", "must not start with"},
		{"a deploy token's prefix", PasswordPolicy{}, DeployPrefix + "something", "must not start with"},
		{"12 of 12", pci, "password1234", ""},
		{"11 of 12", pci, "password123", "at least 12 characters"},
		{"no digit", pci, "correct horse battery", "password: must hold at least one letter and one digit, of any script " +
			"(auth.passwords.requireLettersAndDigits)"},
		{"no letter", pci, "1234-5678-9012", "one letter and one digit"},
		{"Cyrillic and digits", pci, "пароль пароль 7", ""},
		{"an Arabic-Indic digit is Nd", pci, "كلمةالسرالسرية٣", ""},
		{"a Roman numeral is Nl, no digit", pci, "passwordpassword Ⅻ", "one letter and one digit"},
		{"letters only, no rule", PasswordPolicy{MinLength: 12}, "correct horse battery", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy.Check(tt.password)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Check = %v, want nil", err)
			case tt.wantErr != "" && (!errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Check = %v, want ErrInvalid with %q", err, tt.wantErr)
			case err != nil && tt.password != "" && strings.Contains(err.Error(), tt.password):
				t.Errorf("the error %q holds the password", err)
			}
		})
	}
}
