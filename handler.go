package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/context"
)

const BAD_REQ_MSG = "Bad Request\n"

type AuthProvider func() string

type ProxyHandler struct {
	logger        *CondLogger
	dialer        ContextDialer
	httptransport http.RoundTripper
	auth          AuthProvider
}

func NewProxyHandler(dialer, requestDialer ContextDialer, auth AuthProvider, resolver *Resolver, logger *CondLogger) *ProxyHandler {
	dialer = NewRetryDialer(dialer, resolver, logger)
	httptransport := &http.Transport{
		Proxy: func(_ *http.Request) (*url.URL, error) {
			return &url.URL{
				Scheme: "http",
				Host:   "void",
			}, nil
		},
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext:           requestDialer.DialContext,
	}
	return &ProxyHandler{
		logger:        logger,
		dialer:        dialer,
		auth:          auth,
		httptransport: httptransport,
	}
}

func (s *ProxyHandler) handleTCPSocks(conn net.Conn, bufConn *bufio.Reader) {
	var err error
	var destEndPoint string
	if destEndPoint, err = readSocks5Address(bufConn); err != nil {
		msg := make([]byte, 10)
		msg[0] = 5
		msg[1] = 4 // host unreachable
		msg[2] = 0 // Reserved
		msg[3] = uint8(1)
		copy(msg[4:], []byte{0, 0, 0, 0})
		msg[8] = 0
		msg[9] = 0
		conn.Write(msg)

		fmt.Println("socks: Failed to read destination address:", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	proxyConn, err := s.dialer.DialContext(ctx, "tcp", destEndPoint)
	cancel()

	if err != nil {
		msg := make([]byte, 10)
		msg[0] = 5
		msg[1] = 4 // host unreachable
		msg[2] = 0 // Reserved
		msg[3] = uint8(1)
		copy(msg[4:], []byte{0, 0, 0, 0})
		msg[8] = 0
		msg[9] = 0
		conn.Write(msg)

		s.logger.Error("Can't satisfy CONNECT request: %v", err)
		return
	}
	defer proxyConn.Close()

	msg := make([]byte, 10)
	msg[0] = 5
	msg[1] = 0 // successfully connected
	msg[2] = 0 // Reserved
	msg[3] = uint8(1)
	copy(msg[4:], []byte{0, 0, 0, 0})
	msg[8] = 0
	msg[9] = 0
	conn.Write(msg)

	errCh := make(chan error, 2)

	tcpProxy := func(dst io.Writer, src io.Reader, errCh chan error) {
		type closeWriter interface {
			CloseWrite() error
		}

		_, err := io.Copy(dst, src)
		if tcpConn, ok := dst.(closeWriter); ok {
			tcpConn.CloseWrite()
		}
		errCh <- err
	}

	go tcpProxy(proxyConn, bufConn, errCh)
	go tcpProxy(conn, proxyConn, errCh)

	// Wait
	for i := 0; i < 2; i++ {
		e := <-errCh
		if e != nil {
			return
		}
	}

	return
}

func (s *ProxyHandler) InitialHandler(conn net.Conn) {
	defer conn.Close()
	bufConn := bufio.NewReader(conn)

	var err error

	// Read the version byte
	version := []byte{0}
	if _, err = bufConn.Read(version); err != nil {
		fmt.Println("socks: Failed to get version byte:", err)
		return
	}

	// Ensure we are compatible
	if version[0] != 5 {
		fmt.Printf("socks: Unsupported SOCKS version:", version)
		return
	}

	authHeader := []byte{0}
	if _, err = bufConn.Read(authHeader); err != nil {
		fmt.Println("socks: Failed to authenticate:", err)
		return
	}

	numMethods := int(authHeader[0])
	methods := make([]byte, numMethods)
	if _, err = io.ReadAtLeast(bufConn, methods, numMethods); err != nil {
		fmt.Println("socks: Failed to authenticate:", err)
		return
	}

	// no auth
	if _, err = conn.Write([]byte{5, 0}); err != nil {
		fmt.Println("socks: Failed to authenticate:", err)
		return
	}

	reqHeader := []byte{0, 0, 0}
	if _, err = io.ReadAtLeast(bufConn, reqHeader, 3); err != nil {
		fmt.Println("socks: Failed to get command version:", err)
		return
	}

	if reqHeader[0] != 5 {
		fmt.Println("socks: Unsupported command version:", reqHeader[0])
		return
	}

	method := reqHeader[1]

	switch method {
	case 1:
		s.handleTCPSocks(conn, bufConn)
	default:
		msg := make([]byte, 10)
		msg[0] = 5
		msg[1] = 7 // command not supported
		msg[2] = 0 // Reserved
		msg[3] = uint8(1)
		copy(msg[4:], []byte{0, 0, 0, 0})
		msg[8] = 0
		msg[9] = 0
		conn.Write(msg)

		fmt.Println("socks: Unsupported command type:", reqHeader[1])
		return
	}
}

func (s *ProxyHandler) Start(bindAddress string) error {
	listener, err := net.Listen("tcp", bindAddress)
	if err != nil {
		return err
	}
	defer listener.Close()

	for {
		c, err := listener.Accept()
		if err != nil {
			return err
		}

		go s.InitialHandler(c)
	}
}
