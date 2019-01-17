package engineio

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/divisionone/go-engine.io/message"
	"github.com/divisionone/go-engine.io/parser"
	"github.com/divisionone/go-engine.io/transport"
)

type MessageType message.MessageType

const (
	MessageBinary MessageType = MessageType(message.MessageBinary)
	MessageText   MessageType = MessageType(message.MessageText)
)

// Conn is the connection object of engine.io.
type Conn interface {

	// Id returns the session id of connection.
	Id() string

	// Request returns the first http request when established connection.
	Request() *http.Request

	// Close closes the connection.
	Close() error

	// NextReader returns the next message type, reader. If no message received, it will block.
	NextReader() (MessageType, io.ReadCloser, error)

	// NextWriter returns the next message writer with given message type.
	NextWriter(messageType MessageType) (io.WriteCloser, error)
}

type transportCreaters map[string]transport.Creater

func (c transportCreaters) Get(name string) transport.Creater {
	return c[name]
}

type serverCallback interface {
	configure() config
	transports() transportCreaters
	onClose(sid string)
}

type state int

const (
	stateUnknow state = iota
	stateNormal
	stateUpgrading
	stateClosing
	stateClosed
)

type serverConn struct {
	id              string
	request         *http.Request
	callback        serverCallback
	writerLocker    sync.Mutex
	transportLocker sync.RWMutex
	currentName     string
	current         transport.Server
	upgradingName   string
	upgrading       transport.Server
	state           state
	stateLocker     sync.RWMutex
	readerChan      chan *connReader
	readerShutdown  chan bool
	pingTimeout     time.Duration
	pingInterval    time.Duration
	pingChan        chan bool
	pingLocker      sync.Mutex
}

var InvalidError = errors.New("invalid transport")

func newServerConn(id string, w http.ResponseWriter, r *http.Request, callback serverCallback) (*serverConn, error) {
	transportName := r.URL.Query().Get("transport")
	creater := callback.transports().Get(transportName)
	if creater.Name == "" {
		return nil, InvalidError
	}
	ret := &serverConn{
		id:             id,
		request:        r,
		callback:       callback,
		state:          stateNormal,
		readerChan:     make(chan *connReader),
		readerShutdown: make(chan bool),
		pingTimeout:    callback.configure().PingTimeout,
		pingInterval:   callback.configure().PingInterval,
		pingChan:       make(chan bool),
	}
	transport, err := creater.Server(w, r, ret)
	if err != nil {
		return nil, err
	}
	ret.setCurrent(transportName, transport)
	if err := ret.onOpen(); err != nil {
		return nil, err
	}

	go ret.pingLoop()

	return ret, nil
}

func (c *serverConn) Id() string {
	return c.id
}

func (c *serverConn) Request() *http.Request {
	return c.request
}

func (c *serverConn) NextReader() (MessageType, io.ReadCloser, error) {
	switch c.getState() {
	case stateClosed, stateClosing:
		return MessageBinary, nil, io.EOF
	}
	/* was
	if c.getState() == stateClosed {
		return MessageBinary, nil, io.EOF
	}
	*/
	select {
	case ret := <-c.readerChan:
		if ret == nil {
			return MessageBinary, nil, io.EOF
		}
		return MessageType(ret.MessageType()), ret, nil
	case <-c.readerShutdown:
		fmt.Println("underlying socket has closed, and we missed the memo !!")
		return MessageBinary, nil, io.EOF
	}
	return MessageBinary, nil, io.EOF
}

func (c *serverConn) NextWriter(t MessageType) (io.WriteCloser, error) {
	if c == nil {
		return nil, fmt.Errorf("called nextWriter on nil serverConn NOT HEALTHY")
	}
	switch c.getState() {
	case stateUpgrading:
		for i := 0; i < 30; i++ {
			time.Sleep(50 * time.Millisecond)
			if c.getState() != stateUpgrading {
				break
			}
		}
		if c.getState() == stateUpgrading {
			return nil, fmt.Errorf("upgrading")
		}
	case stateNormal:
	default:
		return nil, io.EOF
	}
	c.writerLocker.Lock()
	// we got a crash that looks like this
	// (*serverConn).NextWriter(0xc42161fc70, 0x0, 0xc42462fcb0, 0x403355, 0xc420b90960, 0xc42462fcd0)

	if curr := c.getCurrent(); curr == nil {
		return nil, fmt.Errorf("current is nil, NOT HEALTHY")
	}

	ret, err := c.getCurrent().NextWriter(message.MessageType(t), parser.MESSAGE)
	if err != nil {
		c.writerLocker.Unlock()
		return ret, err
	}
	writer := newConnWriter(ret, &c.writerLocker)
	return writer, err
}

func (c *serverConn) Close() error {
	s := c.getState()
	switch s {
	case stateNormal, stateUpgrading:
		return nil
	}
	if c.upgrading != nil {
		c.upgrading.Close()
	}
	c.writerLocker.Lock()
	if w, err := c.getCurrent().NextWriter(message.MessageText, parser.CLOSE); err == nil {
		writer := newConnWriter(w, &c.writerLocker)
		writer.Close()
	} else {
		c.writerLocker.Unlock()
	}
	if err := c.getCurrent().Close(); err != nil {
		return err
	}
	c.setState(stateClosing)
	select {
	case c.readerShutdown <- true:
		// signal the nextReader to stop waiting for data that will never arrive.
	default:
		// we already have signalled that the reader should quit
	}

	return nil
}

func (c *serverConn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	transportName := r.URL.Query().Get("transport")
	if c.currentName != transportName {
		creater := c.callback.transports().Get(transportName)
		if creater.Name == "" {
			http.Error(w, fmt.Sprintf("invalid transport %s", transportName), http.StatusBadRequest)
			return
		}
		u, err := creater.Server(w, r, c)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.setUpgrading(creater.Name, u)
		return
	}
	c.current.ServeHTTP(w, r)
}

func (c *serverConn) OnPacket(r *parser.PacketDecoder) {
	if s := c.getState(); s != stateNormal && s != stateUpgrading {
		return
	}
	switch r.Type() {
	case parser.OPEN:
	case parser.CLOSE:
		c.getCurrent().Close()
	case parser.PING:
		c.writerLocker.Lock()
		t := c.getCurrent()
		u := c.getUpgrade()
		newWriter := t.NextWriter
		if u != nil {
			if w, _ := t.NextWriter(message.MessageText, parser.NOOP); w != nil {
				w.Close()
			}
			newWriter = u.NextWriter
		}
		if w, _ := newWriter(message.MessageText, parser.PONG); w != nil {
			io.Copy(w, r)
			w.Close()
		}
		c.writerLocker.Unlock()
		fallthrough
	case parser.PONG:
		c.pingLocker.Lock()
		defer c.pingLocker.Unlock()
		if s := c.getState(); s != stateNormal && s != stateUpgrading {
			return
		}
		c.pingChan <- true
	case parser.MESSAGE:
		closeChan := make(chan struct{})
		c.readerChan <- newConnReader(r, closeChan)
		<-closeChan
		close(closeChan)
		r.Close()
	case parser.UPGRADE:
		c.upgraded()
	case parser.NOOP:
	}
}

func safeRun(fn func()) {
	defer func() {
		recover()
	}()
	fn()
}

func (c *serverConn) OnClose(server transport.Server) {
	if t := c.getUpgrade(); server == t {
		c.setUpgrading("", nil)
		t.Close()
		return
	}
	t := c.getCurrent()
	if server != t {
		return
	}
	t.Close()
	if t := c.getUpgrade(); t != nil {
		t.Close()
		c.setUpgrading("", nil)
	}
	c.setState(stateClosed)
	safeRun(func() { close(c.readerChan) })
	c.pingLocker.Lock()
	safeRun(func() { close(c.pingChan) })
	c.pingLocker.Unlock()
	c.callback.onClose(c.id)
}

func (s *serverConn) onOpen() error {
	upgrades := []string{}
	for name := range s.callback.transports() {
		if name == s.currentName {
			continue
		}
		upgrades = append(upgrades, name)
	}
	type connectionInfo struct {
		Sid          string        `json:"sid"`
		Upgrades     []string      `json:"upgrades"`
		PingInterval time.Duration `json:"pingInterval"`
		PingTimeout  time.Duration `json:"pingTimeout"`
	}
	resp := connectionInfo{
		Sid:          s.Id(),
		Upgrades:     upgrades,
		PingInterval: s.callback.configure().PingInterval / time.Millisecond,
		PingTimeout:  s.callback.configure().PingTimeout / time.Millisecond,
	}
	w, err := s.getCurrent().NextWriter(message.MessageText, parser.OPEN)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(w)
	if err := encoder.Encode(resp); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return nil
}

func (c *serverConn) getCurrent() transport.Server {
	c.transportLocker.RLock()
	defer c.transportLocker.RUnlock()

	return c.current
}

func (c *serverConn) getUpgrade() transport.Server {
	c.transportLocker.RLock()
	defer c.transportLocker.RUnlock()

	return c.upgrading
}

func (c *serverConn) setCurrent(name string, s transport.Server) {
	c.transportLocker.Lock()
	defer c.transportLocker.Unlock()

	c.currentName = name
	c.current = s
}

func (c *serverConn) setUpgrading(name string, s transport.Server) {
	c.transportLocker.Lock()
	defer c.transportLocker.Unlock()

	c.upgradingName = name
	c.upgrading = s
	c.setState(stateUpgrading)
}

func (c *serverConn) upgraded() {
	c.transportLocker.Lock()

	//  prevent double upgrade from killing the connection
	if c.upgrading == nil {
		c.transportLocker.Unlock()
		return
	}

	current := c.current
	c.current = c.upgrading
	c.currentName = c.upgradingName
	c.upgrading = nil
	c.upgradingName = ""

	c.transportLocker.Unlock()

	current.Close()
	c.setState(stateNormal)
}

func (c *serverConn) getState() state {
	c.stateLocker.RLock()
	defer c.stateLocker.RUnlock()
	return c.state
}

func (c *serverConn) setState(state state) {
	c.stateLocker.Lock()
	defer c.stateLocker.Unlock()
	c.state = state
}

func (c *serverConn) pingLoop() {
	lastPing := time.Now()
	lastTry := lastPing
	for {
		now := time.Now()
		pingDiff := now.Sub(lastPing)
		tryDiff := now.Sub(lastTry)
		select {
		case ok := <-c.pingChan:
			if !ok {
				return
			}
			lastPing = time.Now()
			lastTry = lastPing
		case <-time.After(c.pingInterval - tryDiff):
			c.writerLocker.Lock()
			if w, _ := c.getCurrent().NextWriter(message.MessageText, parser.PING); w != nil {
				writer := newConnWriter(w, &c.writerLocker)
				writer.Close()
			} else {
				c.writerLocker.Unlock()
			}
			lastTry = time.Now()
		case <-time.After(c.pingTimeout - pingDiff):
			c.Close()
			return
		}
	}
}
