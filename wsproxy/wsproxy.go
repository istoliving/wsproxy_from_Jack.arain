package wsproxy

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"

	"gitee.com/jackarain/wsproxy/websocket"
	"github.com/gobwas/ws"
)

var (
	// caCerts ...
	caCerts = ".wsproxy/certs/ca.crt"

	// ServerCert ...
	ServerCert = ".wsproxy/certs/server.crt"

	// ServerKey ...
	ServerKey = ".wsproxy/certs/server.key"

	// ClientCert ...
	ClientCert = ".wsproxy/certs/client.crt"

	// ClientKey ...
	ClientKey = ".wsproxy/certs/client.key"

	// UnixSockAddr ...
	UnixSockAddr = "wsproxy.sock"

	// JSONConfig ...
	JSONConfig = "config.json"

	// DisableProxy 表示只开启wss服务, 不启用socks5/http proxy服务.
	DisableProxy = false

	// ServerTLSConfig ...
	ServerTLSConfig *tls.Config

	// Users for auth ...
	Users map[string]string

	// ConnectionID ...
	ConnectionID uint64

	// Encoding ...
	Encoding string
)

// UserInfo ...
type UserInfo struct {
	User   string
	Passwd string
}

// Configuration ...
type Configuration struct {
	Servers                []string   `json:"Servers"`
	ServerVerifyClientCert bool       `json:"VerifyClientCert"`
	Listen                 string     `json:"ListenAddr"`
	DisableProxy           bool       `json:"DisableProxy"`
	Users                  []UserInfo `json:"Users"`
	UpstreamProxyServer    string     `json:"UpstreamProxyServer"`
	Encoding               string     `json:"Encoding"`
}

// AuthHandlerFunc ...
type AuthHandlerFunc func(string, string) bool

// AuthHander interface ...
type AuthHander interface {
	Auth(string, string) bool
}

// Server ...
type Server struct {
	config     Configuration
	listen     *net.TCPListener
	unixListen net.Listener

	authFunc AuthHandlerFunc
}

func makeUnixSockName() string {
	return filepath.Join(os.TempDir(), UnixSockAddr)
}

type bufferedConn struct {
	rw       *bufio.ReadWriter
	net.Conn // So that most methods are embedded
}

func newBufferedConn(c net.Conn) bufferedConn {
	return bufferedConn{bufio.NewReadWriter(bufio.NewReader(c), bufio.NewWriter(c)), c}
}

func newBufferedConnSize(c net.Conn, n int) bufferedConn {
	return bufferedConn{bufio.NewReadWriter(bufio.NewReaderSize(c, n), bufio.NewWriterSize(c, n)), c}
}

func (b bufferedConn) Peek(n int) ([]byte, error) {
	return b.rw.Peek(n)
}

func (b bufferedConn) Read(p []byte) (int, error) {
	return b.rw.Read(p)
}

func (s *Server) startWSS(ID uint64, bc bufferedConn) {
	fmt.Println(ID, "* Start tls connection", bc.Conn.RemoteAddr())

	// 转换成TLS connection对象.
	TLSConn := tls.Server(bc, ServerTLSConfig)

	// 开始握手.
	err := TLSConn.Handshake()
	if err != nil {
		fmt.Println(ID, "tls connection handshake fail", err.Error())
		return
	}

	// 创建websocket连接.
	wsconn, err := websocket.NewWebsocket(TLSConn)
	if err != nil {
		fmt.Println(ID, "tls connection Upgrade to websocket", err.Error())
		return
	}

	network := "unix"
	addr := makeUnixSockName()

	if s.config.UpstreamProxyServer != "" {
		network = "tcp"
		addr = s.config.UpstreamProxyServer
	}

	c, err := net.Dial(network, addr)
	if err != nil {
		fmt.Println(ID, "tls connect to target socket", err.Error())
		return
	}
	defer c.Close()

	errCh := make(chan error, 2)
	go func(c net.Conn, wsconn *websocket.Websocket) {
		buf := make([]byte, 256*1024)
		var err error
		var sbuf []byte

		for {
			nr, er := c.Read(buf)
			sbuf = buf
			if nr > 0 {

				if wsconn.Encoding == "zlib" {
					var gbuf bytes.Buffer
					w := zlib.NewWriter(&gbuf)
					nz, ez := w.Write(buf[0:nr])
					if nz != nr {
						err = io.ErrShortWrite
						break
					}
					if ez != nil {
						err = ez
						break
					}
					w.Close()

					sbuf = gbuf.Bytes()
					nr = len(sbuf)
				}

				ew := wsconn.WriteMessage(ws.OpBinary, sbuf[0:nr])
				if ew != nil {
					err = ew
					break
				}
				bc.rw.Flush()
			}

			if er != nil {
				err = er
				break
			}
		}

		errCh <- err
	}(c, wsconn)

	go func(wsconn *websocket.Websocket, c net.Conn) {
		var err error
		sbuf := make([]byte, 512*1024)

		for {
			_, msg, er := wsconn.ReadMessage()
			nr := len(msg)
			if len(msg) > 0 {

				if wsconn.Encoding == "zlib" {
					b := bytes.NewReader(msg)
					r, ez := zlib.NewReader(b)
					if ez != nil {
						err = ez
						break
					}
					nn, ez := r.Read(sbuf)
					if ez != nil && ez != io.EOF {
						err = ez
						break
					}
					nr = nn
					r.Close()
				} else {
					sbuf = msg
				}

				nw, ew := c.Write(sbuf[0:nr])
				if nw != nr {
					err = io.ErrShortWrite
					break
				}

				if ew != nil {
					err = ew
					break
				}
			}

			if er != nil {
				err = er
				break
			}
		}

		errCh <- err
	}(wsconn, c)

	for i := 0; i < 2; i++ {
		e := <-errCh
		if e != nil {
			break
		}
	}
}

func (s *Server) handleClientConn(conn *net.TCPConn) {
	// 计算连接id.
	ID := atomic.AddUint64(&ConnectionID, 1)

	// 创建带buffer的Connection.
	bc := newBufferedConn(conn)
	defer bc.Close()

	reader := bc.rw.Reader
	peek, err := reader.Peek(1)
	if err != nil {
		fmt.Println(ID, "Peek first byte error", err.Error())
		return
	}

	writer := bc.rw.Writer

	remoteAddr := conn.RemoteAddr()
	idx := -1
	server := ""
	verify := s.config.ServerVerifyClientCert

	if len(s.config.Servers) > 0 {
		idx = rand.Intn(len(s.config.Servers))
		server = s.config.Servers[idx]
	}

	if peek[0] == 0x05 && !DisableProxy {
		// 如果是socks5协议, 则调用socks5协议库, 若是client模式直接使用tls转发到服务器.
		if idx >= 0 {
			// 随机选择一个上游服务器用于转发socks5协议.
			insize, tosize := StartConnectServer(ID, verify, conn, reader, writer, server)
			fmt.Println(ID, "- Exit proxy with client:", remoteAddr, insize, tosize)
		} else {
			// 没有配置上游服务器地址, 直接作为socks5服务器提供socks5服务.
			StartSocks5Proxy(ID, bc.rw, s.authFunc, reader, writer)
			fmt.Println(ID, "- Leave socks5 proxy with client:", remoteAddr)
		}
	} else if (peek[0] == 0x47 || peek[0] == 0x43) && !DisableProxy {
		// 如果'G' 或 'C', 则按http proxy处理, 若是client模式直接使用tls转发到服务器.
		if idx >= 0 {
			// 随机选择一个上游服务器用于转发http proxy协议.
			insize, tosize := StartConnectServer(ID, verify, conn, reader, writer, server)
			fmt.Println(ID, "- Exit proxy with client:", remoteAddr, insize, tosize)
		} else {
			StartHTTPProxy(ID, bc.rw, s.authFunc, reader, writer)
			fmt.Println(ID, "- Leave http proxy with client:", remoteAddr)
		}
	} else if peek[0] == 0x16 {
		s.startWSS(ID, bc)
		fmt.Println(ID, "- WSS Proxy disconnect...")
	} else {
		fmt.Println(ID, "- Unknown protocol!")
	}
}

func (s *Server) handleUnixConn(conn net.Conn) {
	bc := newBufferedConn(conn)
	defer bc.Close()
	reader := bc.rw.Reader
	peek, err := reader.Peek(1)
	if err != nil {
		return
	}

	writer := bc.rw.Writer

	ID := atomic.AddUint64(&ConnectionID, 1)
	fmt.Println(ID, "Start Unix connection...")

	if peek[0] == 0x05 {
		StartSocks5Proxy(ID, bc.rw, s.authFunc, reader, writer)
	} else if peek[0] == 0x47 || peek[0] == 0x43 {
		StartHTTPProxy(ID, bc.rw, s.authFunc, reader, writer)
	} else {
		fmt.Println(ID, "Unknown protocol!")
		return
	}

	fmt.Println(ID, "Exit Unix connection!")
}

func (s *Server) initTLSServer() {
	// Server ca cert pool.
	CertPool := x509.NewCertPool()
	ca, err := ioutil.ReadFile(caCerts)
	if err == nil {
		CertPool.AppendCertsFromPEM(ca)
	} else if s.config.ServerVerifyClientCert {
		fmt.Println("Open ca file error", err.Error())
	}

	serverCert, err := tls.LoadX509KeyPair(ServerCert, ServerKey)
	if err != nil {
		fmt.Println("Open server cert file error", err.Error())
	}

	ServerTLSConfig = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      CertPool,
		Certificates: []tls.Certificate{serverCert},
	}
}

// NewServer ...
func NewServer(serverList []string) *Server {
	// Make server.
	s := &Server{}

	// 初始化连接ID.
	ConnectionID = 0

	// 在tmp目录下pid目录创建unix domain socket文件.
	dir := "wsproxy-" + strconv.Itoa(os.Getpid())
	os.Mkdir(os.TempDir()+"/"+dir, os.ModeDir)
	UnixSockAddr = dir + "/" + UnixSockAddr

	// Open config json file.
	file, err := os.Open(JSONConfig)
	defer file.Close()
	if err != nil {
		fmt.Println("Configuration open error:", err)
		return nil
	}

	configuration := Configuration{
		ServerVerifyClientCert: true,
	}

	decoder := json.NewDecoder(file)
	err = decoder.Decode(&configuration)
	if err != nil {
		fmt.Println("Configuration decode error:", err)
		return nil
	}

	// 添加到Users容器中.
	Users = make(map[string]string)
	for _, v := range configuration.Users {
		Users[v.User] = v.Passwd
	}

	s.config = configuration
	if configuration.DisableProxy {
		DisableProxy = true
	}
	Encoding = configuration.Encoding

	fmt.Println(s.config)

	// Init tls server.
	s.initTLSServer()
	return s
}

// Start start wserver...
func (s *Server) Start(addr string) error {
	go s.StartUnixSocket()
	return s.StartWithAuth(addr, nil)
}

// AuthHandleFunc ...
func (s *Server) AuthHandleFunc(handler func(string, string) bool) {
	s.authFunc = handler
}

// StartUnixSocket ...
func (s *Server) StartUnixSocket() error {
	unixSockName := makeUnixSockName()
	if err := os.RemoveAll(unixSockName); err != nil {
		log.Fatal(err)
	}

	listen, err := net.Listen("unix", unixSockName)
	if err != nil {
		log.Fatal("listen error:", err)
	}

	s.unixListen = listen

	for {
		c, err := listen.Accept()
		if err != nil {
			fmt.Println("StartUnixSocket, accept: ", err.Error())
			break
		}

		go s.handleUnixConn(c)
	}

	return nil
}

// StartWithAuth start wserver...
func (s *Server) StartWithAuth(addr string, handler AuthHander) error {
	if s.config.Listen != "" {
		addr = s.config.Listen
	}

	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	listen, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return err
	}

	s.listen = listen
	if handler != nil {
		s.authFunc = handler.Auth
	}

	// 如果没设置用户认证列表, 则表示无需认证.
	if len(Users) == 0 {
		s.authFunc = nil
	}

	for {
		c, err := s.listen.AcceptTCP()
		if err != nil {
			fmt.Println("StartWithAuth, accept: ", err.Error())
			break
		}

		// start a new goroutine to handle the new connection.
		go s.handleClientConn(c)
	}

	return nil
}

// Stop stop socks5 server ...
func (s *Server) Stop() {
	s.listen.Close()
	s.unixListen.Close()
}
