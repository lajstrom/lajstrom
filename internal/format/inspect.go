package format

import (
	"errors"
	"fmt"
	"io"
	"runtime/debug"
)

// ReadInspector returns the Inspector of a parser that reads the file rather than being written
// to (gzip, tar): read runs on a goroutine of its own from the first Write on, with the bytes
// written as its reader, and returns once it found what it looks for, or failed. Write returns
// once read took its bytes; after read returned, Write discards them, or fails with read's error
// so that a refused file stops streaming. Close ends read's reader (io.EOF), waits for read to
// return and returns its error. Read's goroutine ends with Close, which ingest calls also when
// the body fails (Inspector); an inspector never written starts none. A panic of read, which no
// handler's recovery would see on its goroutine, is raised again, with read's stack, on the
// goroutine of the Write or Close that finds it, and every call after fails.
func ReadInspector(read func(r io.Reader) error) Inspector {
	return &readInspector{read: read}
}

// readInspector is the Inspector of ReadInspector.
type readInspector struct {
	read func(io.Reader) error
	pw   *io.PipeWriter
	done chan struct{} // closed when read returned
	err  error         // read's error, once done is closed
	// panicked is what read panicked with, with its stack, once done is closed; wait raises it
	// once.
	panicked error
}

// errRead is the error of the pipe's writes after read returned.
var errRead = errors.New("format: the inspector read what it needs")

// errPanicked is read's error once it panicked.
var errPanicked = errors.New("format: the inspector's parser panicked")

// start runs read on the reader of a new pipe.
func (i *readInspector) start() {
	pr, pw := io.Pipe()
	i.pw, i.done = pw, make(chan struct{})
	go func() {
		defer close(i.done)
		defer func() {
			if v := recover(); v != nil {
				i.err, i.panicked = errPanicked, fmt.Errorf("%w: %v\n%s", errPanicked, v, debug.Stack())
			}
			_ = pr.CloseWithError(errRead) // a PipeReader's CloseWithError is always nil
		}()
		i.err = i.read(pr)
	}()
}

// wait waits for read to return and returns its error, or panics with read's panic, once.
func (i *readInspector) wait() error {
	<-i.done
	if p := i.panicked; p != nil {
		i.panicked = nil
		panic(p)
	}
	return i.err
}

// Write implements io.Writer.
func (i *readInspector) Write(p []byte) (int, error) {
	if i.pw == nil {
		i.start()
	}
	if _, err := i.pw.Write(p); err != nil {
		if err := i.wait(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Close implements Inspector.
func (i *readInspector) Close() error {
	if i.pw == nil {
		i.start()
	}
	_ = i.pw.Close() // a PipeWriter's Close is always nil
	return i.wait()
}
