//go:build linux || darwin

package postgres

import (
	"crypto/tls"
	"errors"
	"net"
	"syscall"
)

// serverSent reports whether the server has written to c or closed it, without reading
// anything: a recv with MSG_PEEK and MSG_DONTWAIT on its socket finds a byte, the end of the
// stream, or an error such as a reset. An idle connection of the pool has nothing to read, and
// the call returns at once with EAGAIN. Under TLS it peeks at the socket under it, where a
// record or the close of a server that ended the session is as good a reason to ping. It runs
// under RawConn.Control, which takes no read lock, so it never waits for another reader of c.
// A connection that is not a socket (syscall.Conn) is reported as silent.
func serverSent(c net.Conn) bool {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	sc, ok := c.(syscall.Conn)
	if !ok {
		return false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return false
	}
	sent := false
	var b [1]byte
	err = raw.Control(func(fd uintptr) {
		n, _, rerr := syscall.Recvfrom(int(fd), b[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
		// n > 0: bytes wait; no error with n == 0: the server closed the stream; EAGAIN:
		// nothing, as an idle connection should be; another error: the socket is broken.
		sent = n > 0 || rerr == nil ||
			!errors.Is(rerr, syscall.EAGAIN) && !errors.Is(rerr, syscall.EWOULDBLOCK) && !errors.Is(rerr, syscall.EINTR)
	})
	return sent || err != nil
}
