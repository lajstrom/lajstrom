//go:build !linux && !darwin

package postgres

import "net"

// serverSent is false where Lajstrom does not peek at a socket (peek_unix.go): a pooled
// connection is pinged after a second of idling only, pgx's default.
func serverSent(net.Conn) bool { return false }
