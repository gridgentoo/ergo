package dist

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ergo-services/ergo/gen"
	"github.com/ergo-services/ergo/lib"
	"github.com/ergo-services/ergo/node"
)

const (
	DefaultEPMDPort uint16 = 4369

	epmdAliveReq      = 120
	epmdAliveResp     = 121
	epmdPortPleaseReq = 122
	epmdPortResp      = 119
	epmdNamesReq      = 110

	// wont be implemented
	// epmdDumpReq = 100
	// epmdKillReq = 107
	// epmdStopReq = 115

	ergoExtraMagic        = 4411
	ergoExtraVersion      = 1
	ergoExtraEnabledTLS   = 100
	ergoExtraEnabledProxy = 101
)

// epmd implements resolver
type epmdResolver struct {
	node.Resolver

	ctx context.Context

	enableServer bool
	host         string
	port         uint16

	nodePort         uint16
	nodeName         string
	nodeHost         string
	handshakeVersion HandshakeVersion

	staticOnly   bool
	staticRoutes map[string]gen.Route
	staticMutex  sync.Mutex

	extra []byte
}

func CreateResolver(ctx context.Context, enableServer bool, host string, port uint16) node.Resolver {
	resolver := &epmdResolver{
		ctx:          ctx,
		enableServer: enableServer,
		host:         host,
		port:         port,
	}
	if enableServer {
		startServerEPMD(ctx, host, port)
	}
	return epmd
}

func (e *epmdResolver) Register(name string, port uint16, options node.ResolverOptions) error {
	n := strings.Split(name, "@")
	if len(n) != 2 {
		return fmt.Errorf("(EMPD) FQDN for node name is required (example: node@hostname)")
	}

	e.nodeName = n[0]
	e.nodeHost = n[1]
	e.nodePort = port
	e.handshakeVersion = options.HandshakeVersion

	e.composeExtra(options)

	conn, err := e.registerNode(options)
	if err != nil {
		return err
	}
	go func() {
		buf := make([]byte, 1024)
		for {
			_, err := conn.Read(buf)
			if err == nil {
				continue
			}
			lib.Log("[%s] EPMD client: closing connection", name)

			// reconnect to the EPMD server
			for {
				if e.ctx.Err() != nil {
					// node is stopped
					return
				}

				// try to start embedded EPMD server
				if e.enableServer {
					startServerEPMD(e.ctx, e.host, e.port)
				}

				if c, err := e.registerNode(options); err != nil {
					lib.Log("[%s] EPMD client: can't register node %q (%s). Retry in 3 seconds...", name, err)
					time.Sleep(3 * time.Second)
				} else {
					conn = c
					break
				}
			}
		}
	}()

	go func() {
		<-e.ctx.Done()
		conn.Close()
	}()

	return nil
}

func (e *epmdResolver) Resolve(name string) (gen.Route, error) {
	return gen.Route{}, nil
}

func (e *epmdResolver) AddStaticRoute(name string, port uint16, options gen.RouteOptions) error {
	return nil
}

func (e *epmdResolver) RemoveStaticRoute(name string) error {
	return nil
}

func (e *epmdResolver) composeExtraVersion1(options node.ResolverOptions) {
	buf := make([]byte, 5)

	// 2 bytes: ergoExtraMagic
	binary.BigEndian.PutUint16(buff[0:2], uint16(ergoExtraMagic))
	// 1 byte Extra version
	buf[3] = ergoExtraVersion
	// 1 byte flag enabled TLS
	if options.EnabledTLS {
		buf[4] = 1
	}
	// 1 byte flag enabled proxy
	if options.EnabledProxy {
		buf[5] = 1
	}
	e.extra = buf
	return
}

func (e *epmdResolver) readExtra(buf []byte, info *nodeinfo) {
	if len(buf) < 5 {
		return
	}
	magic := binary.BigEndian.Uint16(buf[0:2])
	if uint16(ergoExtraMagic) != magic {
		return
	}

	if buf[3] != ergoExtraVersion {
		return
	}

	if buf[4] == 1 {
		route.EnabledTLS = true
	}

	if buf[5] == 1 {
		route.EnabledProxy = true
	}

	route.IsErgo = true

	return
}

func (e *epmdResolver) registerNode(options node.ResolverOptions) (net.Conn, error) {
	dsn := net.JoinHostPort(options.ServerHost, strconv.Itoa(int(options.ServerPort)))
	conn, err := net.Dial("tcp", dsn)
	if err != nil {
		return nil, err
	}

	if _, err := e.sendAliveReq(conn); err != nil {
		conn.Close()
		return nil, err
	}

	if err := e.readAliveResp(conn); err != nil {
		conn.Close()
		return nil, err
	}

	lib.Log("[%s] EPMD client: node registered", name)
	return conn, nil
}

func (e *epmdResolver) sendAliveReq(conn net.Conn) error {
	buf := make([]byte, 2+14+len(e.nodeName)+len(e.Extra))
	binary.BigEndian.PutUint16(buf[0:2], uint16(len(buf)-2))
	buf[2] = byte(epmdAlive2Req)
	binary.BigEndian.PutUint16(buf[3:5], e.nodePort)
	// http://erlang.org/doc/reference_manual/distributed.html (section 13.5)
	// 77 — regular public node, 72 — hidden
	// We use a regular one
	buf[5] = 77
	// Protocol TCP
	buf[6] = 0
	// HighestVersion
	binary.BigEndian.PutUint16(buf[7:9], uint16(DistHandshakeVersion6))
	// LowestVersion
	binary.BigEndian.PutUint16(buf[9:11], uint16(DistHandshakeVersion5))
	// length Node name
	l := len(e.nodeName)
	binary.BigEndian.PutUint16(reply[11:13], uint16(l))
	// Node name
	offset := (13 + l)
	copy(buf[13:offset], e.nodeName)
	// Extra data
	l = len(e.Extra)
	binary.BigEndian.PutUint16(buf[offset:offset+2], uint16(l))
	copy(buf[offset+2:offset+2+l], e.Extra)
	// Send
	if _, err := conn.Write(buf); err != nil {
		return err
	}
	return nil
}

func (e *epmdResolver) readAliveResp(conn net.Conn) error {
	buf := make([]byte, 16)
	if _, err := conn.Read(buf); err != nil {
		return err
	}
	if buf[0] != epmdAlive2Resp {
		return fmt.Errorf("Malformed EMPD response")
	}
	if buf[1] != 0 {
		return fmt.Errorf("Can't register. Code: %d", e.nodeName, buf[1])
	}
	return nil
}

func (e *epmdResolver) addStaticRoute(name string, port uint16, cookie string, tls bool) error {
	ns := strings.Split(name, "@")
	if len(ns) == 1 {
		ns = append(ns, "localhost")
	}
	if len(ns) != 2 {
		return fmt.Errorf("wrong FQDN")
	}
	if _, err := net.LookupHost(ns[1]); err != nil {
		return err
	}

	if e.staticOnly && port == 0 {
		return fmt.Errorf("EMPD is disabled. Port must be > 0")
	}

	e.mtx.Lock()
	defer e.mtx.Unlock()
	if _, ok := e.staticRoutes[name]; ok {
		// already exist
		return fmt.Errorf("already exist")
	}
	e.staticRoutes[name] = NetworkRoute{int(port), cookie, tls}

	return nil
}

// RemoveStaticRoute
func (e *epmdResolver) removeStaticRoute(name string) {
	e.mtx.Lock()
	defer e.mtx.Unlock()
	delete(e.staticRoutes, name)
	return
}

func (e *epmdResolver) resolve(name string) (NetworkRoute, error) {
	// chech static routes first
	e.mtx.RLock()
	defer e.mtx.RUnlock()
	nr, ok := e.staticRoutes[name]
	if ok && nr.Port > 0 {
		return nr, nil
	}

	if e.staticOnly {
		return nr, fmt.Errorf("Can't resolve %s", name)
	}

	// no static route for the given name. go the regular way
	port, err := e.resolvePort(name)
	if err != nil {
		return nr, err
	}
	return NetworkRoute{port, nr.Cookie, nr.TLS}, nil
}

func (e *epmdResolver) resolvePort(name string) (int, error) {
	ns := strings.Split(name, "@")
	if len(ns) != 2 {
		return 0, fmt.Errorf("incorrect FQDN node name (example: node@localhost)")
	}
	conn, err := net.Dial("tcp", net.JoinHostPort(ns[1], fmt.Sprintf("%d", e.Port)))
	if err != nil {
		return 0, err
	}

	defer conn.Close()

	buf := compose_PORT_PLEASE2_REQ(ns[0])
	_, err = conn.Write(buf)
	if err != nil {
		return -1, fmt.Errorf("initiate connection - %s", err)
	}

	buf = make([]byte, 1024)
	_, err = conn.Read(buf)
	if err != nil && err != io.EOF {
		return -1, fmt.Errorf("reading from link - %s", err)
	}

	if buf[0] == EPMD_PORT2_RESP && buf[1] == 0 {
		p := binary.BigEndian.Uint16(buf[2:4])
		// we don't use all the extra info for a while. FIXME (do we need it?)
		return int(p), nil
	} else if buf[1] > 0 {
		return -1, fmt.Errorf("desired node not found")
	} else {
		return -1, fmt.Errorf("malformed reply - %#v", buf)
	}
}

func compose_PORT_PLEASE2_REQ(name string) (reply []byte) {
	replylen := uint16(2 + len(name) + 1)
	reply = make([]byte, replylen)
	binary.BigEndian.PutUint16(reply[0:2], uint16(len(reply)-2))
	reply[2] = byte(EPMD_PORT_PLEASE2_REQ)
	copy(reply[3:replylen], name)
	return
}
