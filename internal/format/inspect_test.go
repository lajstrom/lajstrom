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

// TestReadInspectorPanic: a parser that panics, on its own goroutine, does not end the process:
// the write that finds it, or Close, panics on the caller's goroutine with the parser's value and
// stack, as a handler's recovery sees it, once; the goroutine has ended, and every call after
// fails.
func TestReadInspectorPanic(t *testing.T) {
	for _, tt := range []struct {
		name  string
		input string
	}{
		{"in a write", "a\nboom\n" + strings.Repeat("rest\n", 50_000)},
		{"at Close", "a\nboom"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			exited := make(chan struct{})
			insp := format.ReadInspector(func(r io.Reader) error {
				defer close(exited)
				sc := bufio.NewScanner(r)
				for sc.Scan() {
					if sc.Text() == "boom" {
						panic("the parser's bug")
					}
				}
				return nil
			})
			raised := func(f func() error) (v any, err error) {
				defer func() { v = recover() }()
				return nil, f()
			}
			var v any
			var err error
			for rest := tt.input; rest != "" && v == nil && err == nil; rest = rest[min(len(rest), 7):] {
				v, err = raised(func() error { _, err := insp.Write([]byte(rest[:min(len(rest), 7)])); return err })
			}
			if v == nil && err == nil {
				v, err = raised(insp.Close)
			}
			<-exited
			perr, ok := v.(error)
			if !ok || !strings.Contains(perr.Error(), "the parser's bug") || !strings.Contains(perr.Error(), "TestReadInspectorPanic") {
				t.Fatalf("the caller's goroutine got the panic %v, error %v; want the parser's, with its stack", v, err)
			}
			if v, err := raised(insp.Close); v != nil || err == nil {
				t.Errorf("Close after the panic: panic %v, error %v; want an error", v, err)
			}
		})
	}
}
