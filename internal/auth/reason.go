package auth

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxChangeReason bounds the reason of a change, in bytes: changeReason of the API
// (docs/spec/rbac.md §13, #1285) and --reason of the command line (§6), which the audit rows
// keep as changeReason (Origin.ChangeReason).
const MaxChangeReason = 1024

// CheckChangeReason returns nil when s is the reason of a change by docs/spec/rbac.md §13: 1 to
// MaxChangeReason bytes of UTF-8 without control characters, a ticket or a sentence, the
// reference of the change's approval in the operator's records. Otherwise its error says why,
// for a message that names the field.
func CheckChangeReason(s string) error {
	switch {
	case s == "":
		return errors.New("empty: a ticket or a sentence, 1 byte at least")
	case len(s) > MaxChangeReason:
		return fmt.Errorf("%d bytes, at most %d", len(s), MaxChangeReason)
	case !utf8.ValidString(s):
		return errors.New("not UTF-8")
	case strings.ContainsFunc(s, unicode.IsControl):
		return errors.New("a control character (a line break, a tab)")
	}
	return nil
}
