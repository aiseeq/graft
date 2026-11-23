package testutil

import (
	"fmt"
	"net"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

const loopbackIPv4Octet = 127

// RefusedTCPAddr reserves a loopback TCP address without listening on it.
type RefusedTCPAddr struct {
	fd       int
	port     int
	close    sync.Once
	closeErr error
}

// ReserveRefusedTCPAddr binds a port without listening; Listen+Close leaves a
// race where another process can listen before a failed-start check connects.
func ReserveRefusedTCPAddr() (*RefusedTCPAddr, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("creating reserved TCP socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{loopbackIPv4Octet, 0, 0, 1}}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("binding reserved TCP socket: %w", err)
	}
	sockaddr, err := unix.Getsockname(fd)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("reading reserved TCP socket address: %w", err)
	}
	addr, ok := sockaddr.(*unix.SockaddrInet4)
	if !ok {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("reserved socket address has type %T, want *unix.SockaddrInet4", sockaddr)
	}
	return &RefusedTCPAddr{fd: fd, port: addr.Port}, nil
}

func (r *RefusedTCPAddr) Addr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(r.port))
}

func (r *RefusedTCPAddr) Port() int { return r.port }

func (r *RefusedTCPAddr) Close() error {
	r.close.Do(func() { r.closeErr = unix.Close(r.fd) })
	return r.closeErr
}
