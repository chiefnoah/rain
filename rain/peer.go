package rain

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"
)

// All current implementations use 2^14 (16 kiB), and close connections which request an amount greater than that.
const blockSize = 16 * 1024

const bitTorrent10pstrLen = 19

var bitTorrent10pstr = []byte("BitTorrent protocol")

// Peer message types
const (
	msgChoke = iota
	msgUnchoke
	msgInterested
	msgNotInterested
	msgHave
	msgBitfield
	msgRequest
	msgPiece
	msgCancel
	msgPort
)

var peerMessageTypes = [...]string{
	"choke",
	"unchoke",
	"interested",
	"not interested",
	"have",
	"bitfield",
	"request",
	"piece",
	"cancel",
	"port",
}

type peerConn struct {
	conn net.Conn

	unchokeM       sync.Mutex    // protects unchokeC
	unchokeC       chan struct{} // will be closed when and "unchoke" message is received
	onceInterested sync.Once     // for sending "interested" message only once

	amChoking      bool // this client is choking the peer
	amInterested   bool // this client is interested in the peer
	peerChoking    bool // peer is choking this client
	peerInterested bool // peer is interested in this client
	// peerRequests   map[uint64]bool      // What remote peer requested
	// ourRequests    map[uint64]time.Time // What we requested, when we requested it

	log logger
}

func newPeerConn(conn net.Conn) *peerConn {
	return &peerConn{
		conn:        conn,
		unchokeC:    make(chan struct{}),
		amChoking:   true,
		peerChoking: true,
		log:         newLogger("peer " + conn.RemoteAddr().String()),
	}
}

const connReadTimeout = 3 * time.Minute

// run processes incoming messages after handshake.
func (p *peerConn) run(t *transfer) {
	p.log.Debugln("Communicating peer", p.conn.RemoteAddr())

	bitField := newBitField(nil, t.bitField.Len())

	err := p.sendBitField(t.bitField)
	if err != nil {
		p.log.Error(err)
		return
	}

	first := true
	for {
		err = p.conn.SetReadDeadline(time.Now().Add(connReadTimeout))
		if err != nil {
			p.log.Error(err)
			return
		}

		var length int32
		err = binary.Read(p.conn, binary.BigEndian, &length)
		if err != nil {
			p.log.Error(err)
			return
		}

		if length == 0 { // keep-alive message
			p.log.Debug("Received message of type \"keep alive\"")
			continue
		}

		var msgType byte
		err = binary.Read(p.conn, binary.BigEndian, &msgType)
		if err != nil {
			p.log.Error(err)
			return
		}
		length--
		p.log.Debugf("Received message of type %q", peerMessageTypes[msgType])

		switch msgType {
		case msgChoke:
			p.peerChoking = true
		case msgUnchoke:
			p.unchokeM.Lock()
			p.peerChoking = false
			close(p.unchokeC)
			p.unchokeC = make(chan struct{})
			p.unchokeM.Unlock()
		case msgInterested:
			p.peerInterested = true
		case msgNotInterested:
			p.peerInterested = false
		case msgHave:
			var i int32
			err = binary.Read(p.conn, binary.BigEndian, &i)
			if err != nil {
				p.log.Error(err)
				return
			}
			bitField.Set(i)
			p.log.Debug("Peer ", p.conn.RemoteAddr(), " has piece #", i)
			p.log.Debugln("new bitfield:", bitField.Hex())

			// TODO goroutine may leak
			go func() { t.pieces[i].haveC <- p }()
		case msgBitfield:
			if !first {
				p.log.Error("bitfield can only be sent after handshake")
				return
			}

			if int64(length) != int64(len(bitField.Bytes())) {
				p.log.Error("invalid bitfield length")
				return
			}

			_, err = p.conn.Read(bitField.Bytes())
			if err != nil {
				p.log.Error(err)
				return
			}
			p.log.Debugln("Received bitfield:", bitField.Hex())

			for i := int32(0); i < bitField.Len(); i++ {
				if bitField.Test(i) {
					// TODO goroutine may leak
					go func(i int32) { t.pieces[i].haveC <- p }(i)
				}
			}
		case msgRequest:
		case msgPiece:
			msg := &peerPieceMessage{ID: msgPiece}
			err = binary.Read(p.conn, binary.BigEndian, &msg.Index)
			if err != nil {
				p.log.Error(err)
				return
			}
			err = binary.Read(p.conn, binary.BigEndian, &msg.Begin)
			if err != nil {
				p.log.Error(err)
				return
			}
			if msg.Begin%blockSize != 0 {
				p.log.Error("unexpected piece offset")
				return
			}
			length -= 8
			if length != t.pieces[msg.Index].blocks[msg.Begin/blockSize].length {
				p.log.Error("unexpected block size")
				return
			}
			msg.Block = make([]byte, length)
			_, err = io.LimitReader(p.conn, int64(length)).Read(msg.Block)
			if err != nil {
				p.log.Error(err)
				return
			}
			t.pieces[msg.Index].pieceC <- msg
		case msgCancel:
		case msgPort:
		default:
			p.log.Debugf("Unknown message type: %d", msgType)
		}

		first = false
	}
}

func (p *peerConn) sendBitField(b bitField) error {
	var buf bytes.Buffer
	length := int32(1 + len(b.Bytes()))
	buf.Grow(4 + int(length))
	err := binary.Write(&buf, binary.BigEndian, length)
	if err != nil {
		return err
	}
	if err = buf.WriteByte(msgBitfield); err != nil {
		return err
	}
	if _, err = buf.Write(b.Bytes()); err != nil {
		return err
	}
	p.log.Debugf("Sending message: \"bitfield\" %#v", buf.Bytes())
	_, err = io.Copy(p.conn, &buf)
	return err
}

// beInterested sends "interested" message to peer (once) and
// returns a channel that will be closed when an "unchoke" message is received.
func (p *peerConn) beInterested() (unchokeC chan struct{}, err error) {
	p.unchokeM.Lock()
	defer p.unchokeM.Unlock()

	unchokeC = p.unchokeC

	if !p.peerChoking {
		return
	}

	p.onceInterested.Do(func() { err = p.sendMessage(msgInterested) })
	return
}

func (p *peerConn) sendMessage(msgType byte) error {
	var msg = struct {
		Length      int32
		MessageType byte
	}{1, msgType}
	p.log.Debugf("Sending message: %q", peerMessageTypes[msgType])
	return binary.Write(p.conn, binary.BigEndian, &msg)
}

type peerRequestMessage struct {
	ID                   byte
	Index, Begin, Length int32
}

func newPeerRequestMessage(index, begin, length int32) *peerRequestMessage {
	return &peerRequestMessage{msgRequest, index, begin, length}
}

func (p *peerConn) sendRequest(m *peerRequestMessage) error {
	var msg = struct {
		Length  int32
		Message peerRequestMessage
	}{13, *m}
	p.log.Debugf("Sending message: %q %#v", "request", msg)
	return binary.Write(p.conn, binary.BigEndian, &msg)
}

type peerPieceMessage struct {
	ID           byte
	Index, Begin int32
	Block        []byte
}
