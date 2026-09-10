package backend

import "net"

// FreePort asks the kernel for a loopback port nobody is using. There is a
// window between this and the VM binding it, which is accepted: the failure
// is loud (QEMU refuses to start) and the retry is a new port.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
