package distchan

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
)

// ErrorInIsNotChannel raises when 'in' is not nil and not a channel
// ErrorOutIsNotChannel raises when 'out' is not nil and not a channel
var (
	ErrorInIsNotChannel  = errors.New("Parameter 'in' is not a channel")
	ErrorOutIsNotChannel = errors.New("Parameter 'out' is not a channel")
	ErrorBadRequest      = errors.New("Bad request format")
)

// sync.Pool for connection buffers
var bufPool = sync.Pool{}

func getBuffer() bytes.Buffer {
	b := bufPool.Get()
	if b != nil {
		return b.(bytes.Buffer)
	}
	return bytes.Buffer{}
}

func putBuffer(b bytes.Buffer) {
	b.Reset()
	bufPool.Put(b)
}

// NewServer registers a pair of channels with an active listener. Gob-encoded
// messages received on the listener will be passed to in; any values passed to
// out will be gob-encoded and written to one open connection. The server uses
// a simple round-robin strategy when deciding which connection to send the message
// to; no client is favored over any others.
//
// Note that the returned value's Start() method must be called before any
// messages will be passed. This gives the user an opportunity to register
// encoders and decoders before any data passes over the network.
func NewServer(ln net.Listener, out, in interface{}) (*Server, error) {
	if in != nil && reflect.ValueOf(in).Kind() != reflect.Chan {
		return nil, ErrorInIsNotChannel
	}
	if out != nil && reflect.ValueOf(out).Kind() != reflect.Chan {
		return nil, ErrorOutIsNotChannel
	}
	return &Server{
		ln:      ln,
		outv:    out,
		inv:     in,
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
		logger:  log.New(os.Stdout, "[distchan] ", log.Lshortfile),
		chconn:  make(chan clientConn),
		chbroad: make(chan net.Conn), //non-buffered
	}, nil
}

// Server represents a registration between a network listener and a pair
// of channels, one for input and one for output.
type Server struct {
	ln                 net.Listener
	inv, outv          interface{}
	mu                 sync.RWMutex
	chconn             chan clientConn
	conncnt            int32
	chbroad            chan net.Conn
	encoders, decoders []Transformer
	closed, done       chan struct{}
	logger             *log.Logger
}

type clientConn struct {
	c net.Conn
}

// Start instructs the server to begin serving messages.
func (s *Server) Start() *Server {
	go s.handleIncomingConnections()
	if s.outv != nil {
		go s.handleOutgoingMessages()
	}
	return s
}

// Stop instructs the server to stop serving messages.
func (s *Server) Stop() {
	if err := s.ln.Close(); err != nil {
		s.logger.Printf("error closing listener: %s\n", err)
	}
}

// AddEncoder adds a new encoder to the server. Any outbound messages
// will be passed through all registered encoders before being sent
// over the wire. See the tests for an example of encoding the data
// using AES encryption.
func (s *Server) AddEncoder(f Transformer) *Server {
	s.encoders = append(s.encoders, f)
	return s
}

// AddDecoder adds a new decoder to the server. Any inbound messages
// will be passed through all registered decoders before being sent
// to the channel. See the tests for an example of decoding the data
// using AES encryption.
func (s *Server) AddDecoder(f Transformer) *Server {
	s.decoders = append(s.decoders, f)
	return s
}

// Ready returns true if there are any clients currently connected.
func (s *Server) Ready() bool {
	return atomic.LoadInt32(&s.conncnt) > 0
}

// WaitUntilReady blocks until the server has at least one client available.
func (s *Server) WaitUntilReady() {
	for {
		runtime.Gosched()
		if s.Ready() {
			return
		}
	}
}

// Logger exposes the server's internal logger so that it can be configured.
// For example, if you want the logs to go somewhere besides standard output
// (the default), you can use s.Logger().SetOutput(...).
func (s *Server) Logger() *log.Logger {
	return s.logger
}

func (s *Server) handleIncomingConnections() {
	go func() {
		for {
			conn, err := s.ln.Accept()
			if err != nil {
				// for now, assume it's a "use of closed network connection" error
				close(s.closed)
				close(s.chconn)
				return
			}
			cc := clientConn{c: conn}
			s.chconn <- cc
		}
	}()

	for cc := range s.chconn {
		go s.handleIncomingMessages(cc)
	}
}

func (s *Server) handleIncomingMessages(conn clientConn) {
	var (
		buf  = getBuffer()
		dec  = gob.NewDecoder(&buf)
		et   reflect.Type
		done = make(chan struct{})
	)

	if s.inv != nil {
		et = reflect.TypeOf(s.inv).Elem()
	}

	defer close(done)

	atomic.AddInt32(&s.conncnt, 1)

	go func(c net.Conn, d chan struct{}) {
		// wait for closing and send close to client
		select {
		case <-s.closed:
			if err := binary.Write(c, binary.LittleEndian, signature); err != nil {
				s.logger.Println(err)
			}
			if err := binary.Write(c, binary.LittleEndian, int32(-1)); err != nil {
				s.logger.Println(err)
			}
			// c.Close()
			<-d
		case <-d:
		}

		atomic.AddInt32(&s.conncnt, -1)

	}(conn.c, done)

	go func(c net.Conn, d chan struct{}) {
		// wait for broadcasting and send it to client
		for {
			select {
			case <-d:
				// close(s.chbroad)
				s.chbroad <- c
				return
			case s.chbroad <- c:
				// push client connection in broadcast queue
			}
		}
	}(conn.c, done)

	for {
		buf.Reset()

		err := readChunk(&buf, conn.c)
		if err != nil {
			if err != io.EOF {
				s.logger.Println(err)
			}
			break
		}

		if s.inv != nil {

			for _, decoder := range s.decoders {
				dc := decoder(&buf)
				buf.Reset()
				if _, err = io.Copy(&buf, dc); err != nil {
					s.logger.Panicln(err)
				}
			}

			x := reflect.New(et)
			if err := dec.DecodeValue(x); err != nil {
				if err == io.EOF {
					break
				}
				s.logger.Panicln(err)
			}

			reflect.ValueOf(s.inv).Send(x.Elem())
		}
	}

	if buf.Cap() <= 1<<22 {
		putBuffer(buf)
	}
}

func (s *Server) handleOutgoingMessages() {
	var (
		buf = getBuffer()
		enc = gob.NewEncoder(&buf)
	)

	for {
		buf.Reset()

		x, ok := reflect.ValueOf(s.outv).Recv()
		if !ok {
			break
		}

		if err := enc.EncodeValue(x); err != nil {
			s.logger.Println(err)
			continue
		}

		for _, encoder := range s.encoders {
			ec := encoder(&buf)
			buf.Reset()
			if _, err := io.Copy(&buf, ec); err != nil {
				s.logger.Panicln(err)
			}
		}

		seen := make(map[net.Conn]bool)

		for i := int32(0); i < atomic.LoadInt32(&s.conncnt); {
			select {
			// case <-s.closed:
			// 	break
			case c, ok := <-s.chbroad:
				if ok {
					// we need only different connections
					if _, ok := seen[c]; !ok {
						seen[c] = true
						bts := buf
						if err := writeChunk(c, &bts, bts.Len()); err != nil {
							s.logger.Println(err)
						}
						i++
					}
				}
			}
		}
	}

	if err := s.ln.Close(); err != nil {
		s.logger.Printf("error closing listener: %s\n", err)
	}

	if buf.Cap() <= 1<<22 {
		putBuffer(buf)
	}
}

func NewClient(conn net.Conn, out, in interface{}) (*Client, error) {
	if in != nil && reflect.ValueOf(in).Kind() != reflect.Chan {
		return nil, ErrorInIsNotChannel
	}
	if out != nil && reflect.ValueOf(out).Kind() != reflect.Chan {
		return nil, ErrorOutIsNotChannel
	}
	return &Client{
		conn:   conn,
		outv:   out,
		inv:    in,
		logger: log.New(os.Stdout, "[distchan] ", log.Lshortfile),
		done:   make(chan struct{}),
	}, nil
}

// Transformer represents a function that does an arbitrary transformation
// on a piece of data. It's used for defining custom encoders and decoders
// for modifying how data is sent across the wire.
type Transformer func(src io.Reader) io.Reader

// Client represents a registration between a network connection and a pair
// of channels. See the documentation for Server for more details.
type Client struct {
	conn               net.Conn
	inv, outv          interface{}
	encoders, decoders []Transformer
	started            bool
	logger             *log.Logger
	done               chan struct{}
}

// AddEncoder adds a new encoder to the client. Any outbound messages
// will be passed through all registered encoders before being sent
// over the wire. See the tests for an example of encoding the data
// using AES encryption.
func (c *Client) AddEncoder(f Transformer) *Client {
	c.encoders = append(c.encoders, f)
	return c
}

// AddDecoder adds a new decoder to the client. Any inbound messages
// will be passed through all registered decoders before being sent
// to the channel. See the tests for an example of decoding the data
// using AES encryption.
func (c *Client) AddDecoder(f Transformer) *Client {
	c.decoders = append(c.decoders, f)
	return c
}

// Start instructs the client to begin serving messages.
func (c *Client) Start() *Client {
	if c.inv != nil {
		go c.handleIncomingMessages()
	}
	if c.outv != nil {
		go c.handleOutgoingMessages()
	}
	c.started = true
	return c
}

// Done returns a channel that will be closed once all in-flight data has been
// handled.
func (c *Client) Done() <-chan struct{} {
	return c.done
}

// Logger exposes the client's internal logger so that it can be configured.
// For example, if you want the logs to go somewhere besides standard output
// (the default), you can use c.Logger().SetOutput(...).
func (c *Client) Logger() *log.Logger {
	return c.logger
}

func (c *Client) handleIncomingMessages() {
	var (
		buf = getBuffer()
		// The gob decoder uses a buffer because its underlying reader
		// can't change without running into an "unknown type id" error.
		dec = gob.NewDecoder(&buf)
		et  = reflect.TypeOf(c.inv).Elem()
	)

	defer func() {
		reflect.ValueOf(c.inv).Close()

		// A panic can happen if the underlying channel was closed
		// and we tried to send on it, or if there was a decryption
		// failure. We don't want the panic to go all the way to the
		// top, but we do want to stop processing and log the error.
		if r := recover(); r != nil {
			c.logger.Println(r)
		}
	}()

	for {
		buf.Reset()

		err := readChunk(&buf, c.conn)
		if err != nil {
			if err == io.EOF {
				break
			}
			panic(err)
		}

		for _, decoder := range c.decoders {
			dc := decoder(&buf)
			buf.Reset()
			if _, err = io.Copy(&buf, dc); err != nil {
				c.logger.Panicln(err)
			}
		}

		x := reflect.New(et)
		if err := dec.DecodeValue(x); err != nil {
			if err == io.EOF {
				break
			}
			panic(err)
		}

		reflect.ValueOf(c.inv).Send(x.Elem())
	}

	if buf.Cap() <= 1<<22 {
		putBuffer(buf)
	}
}

func (c *Client) handleOutgoingMessages() {
	var (
		buf = getBuffer()
		enc = gob.NewEncoder(&buf)
	)

	for {
		buf.Reset()

		x, ok := reflect.ValueOf(c.outv).Recv()
		if !ok {
			break
		}
		if err := enc.EncodeValue(x); err != nil {
			c.logger.Panicln(err)
		}

		for _, encoder := range c.encoders {
			ec := encoder(&buf)
			buf.Reset()
			if _, err := io.Copy(&buf, ec); err != nil {
				c.logger.Panicln(err)
			}
		}

		if err := writeChunk(c.conn, &buf, buf.Len()); err != nil {
			c.logger.Printf("error writing value to connection: %s\n", err)
		}
	}

	if buf.Cap() <= 1<<22 {
		putBuffer(buf)
	}

	close(c.done)
}

var signature int32 = 0x7f38b034 // protecting from bad incoming data

func readChunk(buf io.Writer, r io.Reader) error {
	var n int32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return err
	}

	if n != signature {
		return ErrorBadRequest
	}

	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return err
	}

	if n == -1 {
		return io.EOF
	}

	if _, err := io.CopyN(buf, r, int64(n)); err != nil {
		return err
	}

	return nil
}

func writeChunk(w io.Writer, buf io.Reader, lenbuf int) error {
	if err := binary.Write(w, binary.LittleEndian, signature); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, int32(lenbuf)); err != nil {
		return err
	}
	if _, err := io.Copy(w, buf); err != nil {
		return err
	}
	return nil
}
