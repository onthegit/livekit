package service

import (
	"crypto/tls"
	"net"
	"strconv"
)

// getListenerFromConfig will create TLS listener if TLS config is available.
func (s *LivekitServer) getListenerFromConfig(addr string) (net.Listener, error) {
	if s.config.TLS != nil {
		return tls.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(int(s.config.Port))), s.config.TLS)
	}
	return net.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(int(s.config.Port))))
}
