package ipc

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// UnixListener wraps a net.UnixListener with peer credential validation.
type UnixListener struct {
	net.Listener
}

// NewUnixListener creates a Unix socket listener with peer credential validation.
func NewUnixListener(addr string) (net.Listener, error) {
	ln, err := net.Listen("unix", addr)
	if err != nil {
		return nil, err
	}
	return &UnixListener{Listener: ln}, nil
}

// Accept waits for and returns the next connection with peer credential validation.
func (l *UnixListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	// Validate peer credentials
	if !validatePeerCred(conn) {
		conn.Close()
		return nil, fmt.Errorf("connection from different UID rejected")
	}

	return conn, nil
}

// validatePeerCred checks if the peer has the same effective UID as the process.
func validatePeerCred(conn net.Conn) bool {
	// Assert connection is a Unix socket
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}

	// Get the raw file descriptor for syscall access
	rawConn, err := unixConn.SyscallConn()
	if err != nil {
		return false
	}

	// Obtain peer credentials via SO_PEERCRED
	var ucred *syscall.Ucred
	var credErr error
	rawConn.Control(func(fd uintptr) {
		ucred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if credErr != nil {
		// Can't validate, reject connection
		return false
	}

	// Compare peer UID with current effective UID
	return ucred.Uid == uint32(os.Geteuid())
}
