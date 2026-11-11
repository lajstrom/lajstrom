package format_test

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
)

// lineInspector is a ReadInspector that looks for the line want and reports in exited when its
// read returned; it fails at a line "bad".
func lineInspector(want string, exited chan<- struct{}) (format.Inspector, *bool) {
	found := new(bool)
	return format.ReadInspector(func(r io.Reader) error {
		defer close(exited)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			switch sc.Text() {
			case want:
				*found = true
				return nil
			case "bad":
				return errors.New("a bad line")
			}
		}
		return errors.New("no " + want)
	}), found
}

// TestReadInspector: the parser reads the bytes written, in order, across writes; once it has
// what it looks for, the writes after are taken and discarded, and Close returns its nil; a
// parser that fails fails the write that it fails in, or a later one, and Close returns the error;
// a parser that does not find what it looks for fails at Close. Its goroutine has ended once
// Close returned, also after a body that failed, and an inspector never written starts none.
func TestReadInspector(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		writeErr    bool // a write fails
		closeErr    bool
		found       bool
	}{
		{name: "found, the rest discarded", input: "a\nb\nPKG-INFO\n" + strings.Repeat("rest\n", 50_000), found: true},
		{name: "a write fails", input: "a\nbad\n" + strings.Repeat("rest\n", 50_000), writeErr: true, closeErr: true},
		{name: "not found", input: "a\nb\n", closeErr: true},
		{name: "nothing written", closeErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			exited := make(chan struct{})
			insp, found := lineInspector("PKG-INFO", exited)
			var werr error
			for rest := tt.input; rest != "" && werr == nil; {
				n := min(len(rest), 7)
				var wrote int
				wrote, werr = insp.Write([]byte(rest[:n]))
				if werr == nil && wrote != n {
					t.Fatalf("Write of %d bytes = %d", n, wrote)
				}
				rest = rest[n:]
			}
			cerr := insp.Close()
			select {
			case <-exited:
			default:
				t.Error("the parser runs after Close")
			}
			if (werr != nil) != tt.writeErr || (cerr != nil) != tt.closeErr || *found != tt.found {
				t.Errorf("Write %v, Close %v, found %v; want a write error %v, a close error %v, found %v",
					werr, cerr, *found, tt.writeErr, tt.closeErr, tt.found)
			}
			if werr != nil && cerr != werr { //nolint:errorlint // the same error, not one that wraps it
				t.Errorf("Close = %v, want the write's %v", cerr, werr)
			}
		})
	}
	t.Run("a body that fails", func(t *testing.T) {
		exited := make(chan struct{})
		insp, _ := lineInspector("PKG-INFO", exited)
		if _, err := insp.Write([]byte("a\nb")); err != nil {
			t.Fatal(err)
		}
		_ = insp.Close() // as ingest closes it when the body fails
		<-exited
	})
}
