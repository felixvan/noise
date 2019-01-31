package noise

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"github.com/perlin-network/noise/callbacks"
	"github.com/perlin-network/noise/log"
	"github.com/perlin-network/noise/payload"
	"github.com/pkg/errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
)

type Peer struct {
	node *Node
	conn net.Conn

	onConnErrorCallbacks  *callbacks.SequentialCallbackManager
	onDisconnectCallbacks *callbacks.SequentialCallbackManager

	onEncodeHeaderCallbacks *callbacks.ReduceCallbackManager
	onEncodeFooterCallbacks *callbacks.ReduceCallbackManager

	onDecodeHeaderCallbacks *callbacks.SequentialCallbackManager
	onDecodeFooterCallbacks *callbacks.SequentialCallbackManager

	beforeMessageSentCallbacks     *callbacks.ReduceCallbackManager
	beforeMessageReceivedCallbacks *callbacks.ReduceCallbackManager

	afterMessageSentCallbacks     *callbacks.SequentialCallbackManager
	afterMessageReceivedCallbacks *callbacks.SequentialCallbackManager

	onMessageReceivedCallbacks *callbacks.OpcodeCallbackManager

	kill     chan struct{}
	killOnce sync.Once

	workersRunning uint32

	metadata sync.Map
}

func newPeer(node *Node, conn net.Conn) *Peer {
	return &Peer{
		node: node,
		conn: conn,

		onConnErrorCallbacks:  callbacks.NewSequentialCallbackManager(),
		onDisconnectCallbacks: callbacks.NewSequentialCallbackManager(),

		onEncodeHeaderCallbacks: callbacks.NewReduceCallbackManager(),
		onEncodeFooterCallbacks: callbacks.NewReduceCallbackManager(),

		onDecodeHeaderCallbacks: callbacks.NewSequentialCallbackManager(),
		onDecodeFooterCallbacks: callbacks.NewSequentialCallbackManager(),

		beforeMessageReceivedCallbacks: callbacks.NewReduceCallbackManager().Reverse(),
		beforeMessageSentCallbacks:     callbacks.NewReduceCallbackManager(),

		afterMessageReceivedCallbacks: callbacks.NewSequentialCallbackManager(),
		afterMessageSentCallbacks:     callbacks.NewSequentialCallbackManager(),

		onMessageReceivedCallbacks: callbacks.NewOpcodeCallbackManager(),

		kill: make(chan struct{}, 1),
	}
}

func (p *Peer) init() {
	go p.spawnReceiveWorker()
}

func (p *Peer) spawnReceiveWorker() {
	atomic.AddUint32(&p.workersRunning, 1)

	reader := bufio.NewReader(p.conn)

	for {
		select {
		case <-p.kill:
			p.onDisconnectCallbacks.RunCallbacks(p.node)
			close(p.kill)
			return
		default:
		}

		size, err := binary.ReadUvarint(reader)
		if err != nil {
			// TODO(kenta): Hacky fix, but any errors w/ Error() = use of closed network connection is not considered a conn error.
			if errors.Cause(err) != io.EOF && !strings.Contains(errors.Cause(err).Error(), "use of closed network connection") && !strings.Contains(errors.Cause(err).Error(), "read: connection reset by peer") {
				p.onConnErrorCallbacks.RunCallbacks(p.node, errors.Wrap(err, "failed to read message size"))
			}

			p.Disconnect()
			continue
		}

		if size > p.node.maxMessageSize {
			p.onConnErrorCallbacks.RunCallbacks(p.node, errors.Errorf("exceeded max message size; got size %d", size))

			p.Disconnect()
			continue
		}

		buf := make([]byte, int(size))

		seen, err := io.ReadFull(reader, buf)
		if err != nil {
			p.onConnErrorCallbacks.RunCallbacks(p.node, errors.Wrap(err, "failed to read remaining message contents"))

			p.Disconnect()
			continue
		}

		if seen < int(size) {
			p.onConnErrorCallbacks.RunCallbacks(p.node, errors.Errorf("only read %d bytes when expected to read %d from peer", seen, size))

			p.Disconnect()
			continue
		}

		buf = p.beforeMessageReceivedCallbacks.MustRunCallbacks(buf, p.node).([]byte)

		opcode, msg, err := p.DecodeMessage(buf)

		if opcode == OpcodeNil || err != nil {
			p.onConnErrorCallbacks.RunCallbacks(p.node, errors.Wrap(err, "failed to decode message"))

			p.Disconnect()
			continue
		}

		errs := p.onMessageReceivedCallbacks.RunCallbacks(byte(opcode), p.node, msg)

		if len(errs) > 0 {
			log.Error().Errs("errors", errs).Msg("Got errors running OnMessageReceived callbacks.")
		}
		p.afterMessageReceivedCallbacks.RunCallbacks(p.node)
	}
}

func (p *Peer) SendMessage(opcode Opcode, message Message) error {
	payload, err := p.EncodeMessage(opcode, message)
	if err != nil {
		return errors.Wrap(err, "failed to serialize message contents to be sent to a peer")
	}

	payload = p.beforeMessageSentCallbacks.MustRunCallbacks(payload, p.node).([]byte)

	size := len(payload)

	// Prepend message length to packet.
	buf := make([]byte, binary.MaxVarintLen64)
	prepended := binary.PutUvarint(buf[:], uint64(size))

	buf = append(buf[:prepended], payload[:]...)

	copied, err := io.Copy(p.conn, bytes.NewReader(buf))

	if copied != int64(size+prepended) {
		return errors.Errorf("only written %d bytes when expected to write %d bytes to peer", copied, size+prepended)
	}

	if err != nil {
		return errors.Wrap(err, "failed to send message to peer")
	}

	p.afterMessageSentCallbacks.RunCallbacks(p.node)

	return nil
}

func (p *Peer) BeforeMessageSent(c beforeMessageSentCallback) {
	p.beforeMessageSentCallbacks.RegisterCallback(func(in interface{}, params ...interface{}) (i interface{}, e error) {
		if len(params) != 1 {
			panic(errors.Errorf("noise: BeforeMessageSent received unexpected args %v", params))
		}

		node, ok := params[0].(*Node)
		if !ok {
			return in.([]byte), nil
		}

		return c(node, p, in.([]byte))
	})
}

func (p *Peer) BeforeMessageReceived(c beforeMessageReceivedCallback) {
	p.beforeMessageReceivedCallbacks.RegisterCallback(func(in interface{}, params ...interface{}) (i interface{}, e error) {
		if len(params) != 1 {
			panic(errors.Errorf("noise: BeforeMessageReceived received unexpected args %v", params))
		}

		node, ok := params[0].(*Node)
		if !ok {
			return in.([]byte), nil
		}

		return c(node, p, in.([]byte))
	})
}

func (p *Peer) AfterMessageSent(c afterMessageSentCallback) {
	p.afterMessageSentCallbacks.RegisterCallback(func(params ...interface{}) error {
		if len(params) != 1 {
			panic(errors.Errorf("noise: AfterMessageSent received unexpected args %v", params))
		}

		node, ok := params[0].(*Node)
		if !ok {
			return nil
		}

		return c(node, p)
	})
}

func (p *Peer) AfterMessageReceived(c afterMessageReceivedCallback) {
	p.afterMessageReceivedCallbacks.RegisterCallback(func(params ...interface{}) error {
		if len(params) != 1 {
			panic(errors.Errorf("noise: AfterMessageReceived received unexpected args %v", params))
		}

		node, ok := params[0].(*Node)
		if !ok {
			return nil
		}

		return c(node, p)
	})
}

func (p *Peer) OnDecodeHeader(c onPeerDecodeHeaderCallback) {
	p.onDecodeHeaderCallbacks.RegisterCallback(func(params ...interface{}) error {
		if len(params) != 2 {
			panic(errors.Errorf("noise: OnDecodeHeader received unexpected args %v", params))
		}

		node, ok := params[0].(*Node)
		if !ok {
			return nil
		}

		reader, ok := params[1].(payload.Reader)

		if !ok {
			return nil
		}

		return c(node, p, reader)
	})
}

func (p *Peer) OnDecodeFooter(c onPeerDecodeFooterCallback) {
	p.onDecodeFooterCallbacks.RegisterCallback(func(params ...interface{}) error {
		if len(params) != 3 {
			panic(errors.Errorf("noise: OnDecodeFooter received unexpected args %v", params))
		}

		node, ok := params[0].(*Node)
		if !ok {
			return nil
		}

		msg, ok := params[1].([]byte)

		if !ok {
			return nil
		}

		reader, ok := params[2].(payload.Reader)

		if !ok {
			return nil
		}

		return c(node, p, msg, reader)
	})
}

func (p *Peer) OnEncodeHeader(c afterMessageEncodedCallback) {
	p.onEncodeHeaderCallbacks.RegisterCallback(func(header interface{}, params ...interface{}) (i interface{}, e error) {
		if len(params) != 2 {
			panic(errors.Errorf("noise: OnEncodeHeader received unexpected args %v", params))
		}

		node, ok := params[0].(*Node)
		if !ok {
			return header.([]byte), errors.New("noise: OnEncodeHeader did not receive 1st param (node *noise.Node)")
		}

		msg, ok := params[1].([]byte)

		if !ok {
			return header.([]byte), errors.New("noise: OnEncodeHeader did not receive 2nd param (msg []byte)")
		}

		return c(node, p, header.([]byte), msg)
	})
}

func (p *Peer) OnEncodeFooter(c afterMessageEncodedCallback) {
	p.onEncodeFooterCallbacks.RegisterCallback(func(footer interface{}, params ...interface{}) (i interface{}, e error) {
		if len(params) != 2 {
			panic(errors.Errorf("noise: OnEncodeFooter received unexpected args %v", params))
		}

		node, ok := params[0].(*Node)
		if !ok {
			return footer.([]byte), errors.New("noise: OnEncodeHeader did not receive 1st param (node *noise.Node)")
		}

		msg, ok := params[1].([]byte)

		if !ok {
			return footer.([]byte), errors.New("noise: OnEncodeHeader did not receive (msg []byte)")
		}

		return c(node, p, footer.([]byte), msg)
	})
}

// OnConnError registers a callback for whenever somethings wrong with our peers connection
func (p *Peer) OnConnError(c onPeerErrorCallback) {
	p.onConnErrorCallbacks.RegisterCallback(func(params ...interface{}) error {
		if len(params) != 2 {
			panic(errors.Errorf("noise: OnConnError received unexpected args %v", params))
		}

		node, ok := params[0].(*Node)
		if !ok {
			return nil
		}

		err, ok := params[1].(error)

		if !ok {
			return nil
		}

		return c(node, p, errors.Wrap(err, "peer conn reported error"))
	})
}

// OnDisconnect registers a callback for whenever the peer disconnects.
func (p *Peer) OnDisconnect(c onPeerDisconnectCallback) {
	p.onDisconnectCallbacks.RegisterCallback(func(params ...interface{}) error {
		node, ok := params[0].(*Node)
		if !ok {
			return nil
		}

		return c(node, p)
	})
}

// OnMessageReceived registers a callback for whenever a peer sends a message to our node.
func (p *Peer) OnMessageReceived(o Opcode, c onMessageReceivedCallback) {
	p.onMessageReceivedCallbacks.RegisterCallback(byte(o), func(params ...interface{}) error {
		if len(params) != 2 {
			panic(errors.Errorf("noise: OnMessageReceived received unexpected args %+v", params))
		}

		node, ok := params[0].(*Node)
		if !ok {
			return nil
		}

		message, ok := params[1].(Message)
		if !ok {
			return nil
		}

		return c(node, o, p, message)
	})
}

func (p *Peer) Disconnect() {
	//_, file, no, ok := runtime.Caller(1)
	//if ok {
	//	log.Debug().Msf("Disconnect() called from %s#%d.", file, no)
	//}

	p.killOnce.Do(func() {
		workersRunning := atomic.LoadUint32(&p.workersRunning)

		for i := 0; i < int(workersRunning); i++ {
			p.kill <- struct{}{}
		}

		if err := p.conn.Close(); err != nil {
			p.onConnErrorCallbacks.RunCallbacks(p.node, errors.Wrapf(err, "got errors closing peer connection"))
		}
	})
}

func (p *Peer) LocalIP() net.IP {
	return p.node.transport.IP(p.conn.LocalAddr())
}

func (p *Peer) LocalPort() uint16 {
	return p.node.transport.Port(p.conn.LocalAddr())
}

func (p *Peer) RemoteIP() net.IP {
	return p.node.transport.IP(p.conn.RemoteAddr())
}

func (p *Peer) RemotePort() uint16 {
	return p.node.transport.Port(p.conn.RemoteAddr())
}

// Set sets a metadata entry given a key-value pair on our node.
func (p *Peer) Set(key string, val interface{}) {
	p.metadata.Store(key, val)
}

// Get returns the value to a metadata key from our node, or otherwise returns nil should
// there be no corresponding value to a provided key.
func (p *Peer) Get(key string) interface{} {
	val, _ := p.metadata.Load(key)
	return val
}

func (p *Peer) LoadOrStore(key string, val interface{}) interface{} {
	val, _ = p.metadata.LoadOrStore(key, val)
	return val
}

func (p *Peer) Has(key string) bool {
	_, exists := p.metadata.Load(key)
	return exists
}

func (p *Peer) Delete(key string) {
	p.metadata.Delete(key)
}

func (p *Peer) Node() *Node {
	return p.node
}
