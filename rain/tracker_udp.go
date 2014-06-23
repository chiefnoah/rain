package rain

// http://bittorrent.org/beps/bep_0015.html
// http://xbtt.sourceforge.net/udp_tracker_protocol.html
// http://www.rasterbar.com/products/libtorrent/udp_tracker_protocol.html

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/cenkalti/backoff"
)

const connectionIDMagic = 0x41727101980

type trackerAction int32

// Tracker Actions
const (
	trackerActionConnect trackerAction = iota
	trackerActionAnnounce
	trackerActionScrape
	trackerActionError
)

type udpTracker struct {
	URL           *url.URL
	peerID        peerID
	port          uint16
	conn          *net.UDPConn
	transactions  map[int32]*transaction
	transactionsM sync.Mutex
	writeC        chan trackerRequest
	log           logger
}

type transaction struct {
	request  trackerRequest
	response []byte
	err      error
	done     chan struct{}
}

func newTransaction(req trackerRequest) *transaction {
	return &transaction{
		request: req,
		done:    make(chan struct{}),
	}
}

func (t *transaction) Done() {
	defer recover()
	close(t.done)
}

func (r *Rain) newUDPTracker(u *url.URL) *udpTracker {
	return &udpTracker{
		URL:          u,
		peerID:       r.peerID,
		port:         r.port(),
		transactions: make(map[int32]*transaction),
		writeC:       make(chan trackerRequest),
		log:          newLogger("tracker " + u.String()),
	}
}

func (t *udpTracker) Dial() error {
	serverAddr, err := net.ResolveUDPAddr("udp", t.URL.Host)
	if err != nil {
		return err
	}
	t.conn, err = net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		return err
	}
	go t.readLoop()
	go t.writeLoop()
	return nil
}

// Close the tracker connection.
// TODO end all goroutines.
func (t *udpTracker) Close() error {
	return t.conn.Close()
}

// readLoop reads datagrams from connection, finds the transaction and
// sends the bytes to the transaction's response channel.
func (t *udpTracker) readLoop() {
	// Read buffer must be big enough to hold a UDP packet of maximum expected size.
	// Current value is: 320 = 20 + 50*6 (AnnounceResponse with 50 peers)
	buf := make([]byte, 320)
	for {
		n, err := t.conn.Read(buf)
		if err != nil {
			t.log.Error(err)
			if nerr, ok := err.(net.Error); ok && !nerr.Temporary() {
				t.log.Debug("End of tracker read loop")
				return
			}
			continue
		}
		t.log.Debug("Read ", n, " bytes")

		var header trackerMessageHeader
		if n < binary.Size(header) {
			t.log.Error("response is too small")
			continue
		}

		err = binary.Read(bytes.NewReader(buf), binary.BigEndian, &header)
		if err != nil {
			t.log.Error(err)
			continue
		}

		t.transactionsM.Lock()
		trx, ok := t.transactions[header.TransactionID]
		delete(t.transactions, header.TransactionID)
		t.transactionsM.Unlock()
		if !ok {
			t.log.Error("unexpected transaction_id")
			continue
		}

		// Tracker has sent and error.
		if header.Action == trackerActionError {
			// The part after the header is the error message.
			trx.err = trackerError(buf[binary.Size(header):])
			trx.Done()
			continue
		}

		// Copy data into a new slice because buf will be overwritten at next read.
		trx.response = make([]byte, n)
		copy(trx.response, buf)
		trx.Done()
	}
}

// writeLoop receives a request from t.transactionC, sets a random TransactionID
// and sends it to the tracker.
func (t *udpTracker) writeLoop() {
	var connectionID int64
	var connectionIDtime time.Time

	for req := range t.writeC {
		if time.Now().Sub(connectionIDtime) > 60*time.Second {
			connectionID = t.connect()
			connectionIDtime = time.Now()
		}
		req.SetConnectionID(connectionID)

		if err := binary.Write(t.conn, binary.BigEndian, req); err != nil {
			t.log.Error(err)
		}
	}
}

func (t *udpTracker) request(req trackerRequest, cancel <-chan struct{}) ([]byte, error) {
	action := func(req trackerRequest) { t.writeC <- req }
	return t.retry(req, action, cancel)
}

func (t *udpTracker) retry(req trackerRequest, action func(trackerRequest), cancel <-chan struct{}) ([]byte, error) {
	id := rand.Int31()
	req.SetTransactionID(id)

	trx := newTransaction(req)
	t.transactionsM.Lock()
	t.transactions[id] = trx
	t.transactionsM.Unlock()

	ticker := backoff.NewTicker(new(trackerUDPBackOff))
	for {
		select {
		case <-ticker.C:
			action(req)
		case <-trx.done:
			return trx.response, trx.err
		case <-cancel:
			return nil, errors.New("transaction cancelled")
		}
	}
}

type trackerMessage interface {
	GetAction() trackerAction
	SetAction(trackerAction)
	GetTransactionID() int32
	SetTransactionID(int32)
}

// trackerMessageHeader contains the common fields in all trackerMessage structs.
type trackerMessageHeader struct {
	Action        trackerAction
	TransactionID int32
}

func (h *trackerMessageHeader) GetAction() trackerAction  { return h.Action }
func (h *trackerMessageHeader) SetAction(a trackerAction) { h.Action = a }
func (h *trackerMessageHeader) GetTransactionID() int32   { return h.TransactionID }
func (h *trackerMessageHeader) SetTransactionID(id int32) { h.TransactionID = id }

type trackerRequestHeader struct {
	ConnectionID int64
	trackerMessageHeader
}

type trackerRequest interface {
	trackerMessage
	GetConnectionID() int64
	SetConnectionID(int64)
}

func (h *trackerRequestHeader) GetConnectionID() int64   { return h.ConnectionID }
func (h *trackerRequestHeader) SetConnectionID(id int64) { h.ConnectionID = id }

type trackerUDPBackOff int

func (b *trackerUDPBackOff) NextBackOff() time.Duration {
	defer func() { *b++ }()
	if *b > 8 {
		*b = 8
	}
	return time.Duration(15*(2^*b)) * time.Second
}

func (b *trackerUDPBackOff) Reset() { *b = 0 }

type connectRequest struct {
	trackerRequestHeader
}

type connectResponse struct {
	trackerMessageHeader
	ConnectionID int64
}

// connect sends a connectRequest and returns a ConnectionID given by the tracker.
// On error, it backs off with the algorithm described in BEP15 and retries.
// It does not return until tracker sends a ConnectionID.
func (t *udpTracker) connect() int64 {
	req := new(connectRequest)
	req.SetConnectionID(connectionIDMagic)
	req.SetAction(trackerActionConnect)

	write := func(req trackerRequest) {
		binary.Write(t.conn, binary.BigEndian, req)
	}

	// TODO wait before retry
	for {
		data, err := t.retry(req, write, nil)
		if err != nil {
			t.log.Error(err)
			continue
		}

		var response connectResponse
		err = binary.Read(bytes.NewReader(data), binary.BigEndian, &response)
		if err != nil {
			t.log.Error(err)
			continue
		}

		if response.Action != trackerActionConnect {
			t.log.Error("invalid action in connect response")
			continue
		}

		t.log.Debugf("connect Response: %#v\n", response)
		return response.ConnectionID
	}
}

type announceRequest struct {
	trackerRequestHeader
	InfoHash   infoHash
	PeerID     peerID
	Downloaded int64
	Left       int64
	Uploaded   int64
	Event      trackerEvent
	IP         uint32
	Key        uint32
	NumWant    int32
	Port       uint16
	Extensions uint16
}

type announceResponseBase struct {
	trackerMessageHeader
	Interval int32
	Leechers int32
	Seeders  int32
}

type peerAddr struct {
	IP   [net.IPv4len]byte
	Port uint16
}

func (p peerAddr) TCPAddr() *net.TCPAddr {
	ip := make(net.IP, net.IPv4len)
	copy(ip, p.IP[:])
	return &net.TCPAddr{
		IP:   ip,
		Port: int(p.Port),
	}
}

// Announce announces d to t periodically.
func (t *udpTracker) Announce(d *transfer, cancel <-chan struct{}, event <-chan trackerEvent, responseC chan<- *announceResponse) {
	defer func() {
		if responseC != nil {
			close(responseC)
		}
	}()

	err := t.Dial()
	if err != nil {
		// TODO retry connecting to tracker
		t.log.Fatal(err)
	}

	request := &announceRequest{
		InfoHash:   d.torrentFile.Info.Hash,
		PeerID:     t.peerID,
		Event:      trackerEventNone,
		IP:         0, // Tracker uses sender of this UDP packet.
		Key:        0, // TODO set it
		NumWant:    numWant,
		Port:       t.port,
		Extensions: 0,
	}
	request.SetAction(trackerActionAnnounce)
	response := new(announceResponse)
	var nextAnnounce time.Duration = time.Nanosecond // Start immediately.
	for {
		select {
		// TODO send first without waiting
		case <-time.After(nextAnnounce):
			t.log.Debug("Time to announce")
			// TODO update on every try.
			request.update(d)

			// t.request may block, that's why we pass cancel as argument.
			reply, err := t.request(request, cancel)
			if err != nil {
				t.log.Error(err)
				continue
			}

			if err = t.Load(response, reply); err != nil {
				t.log.Error(err)
				continue
			}
			t.log.Debugf("Announce response: %#v", response)

			// TODO calculate time and adjust.
			nextAnnounce = time.Duration(response.Interval) * time.Second

			// may block if caller does not receive from it.
			select {
			case responseC <- response:
			case <-cancel:
				return
			}
		case <-cancel:
			return
			// case request.e = <-event:
			// request.update(d)
		}
	}
}

func (r *announceRequest) update(d *transfer) {
	r.Downloaded = d.Downloaded()
	r.Uploaded = d.Uploaded()
	r.Left = d.Left()
}

func (t *udpTracker) Load(r *announceResponse, data []byte) error {
	if len(data) < binary.Size(r) {
		return errors.New("response is too small")
	}

	reader := bytes.NewReader(data)

	err := binary.Read(reader, binary.BigEndian, &r.announceResponseBase)
	if err != nil {
		return err
	}
	t.log.Debugf("r.announceResponseBase: %#v", r.announceResponseBase)

	if r.Action != trackerActionAnnounce {
		return errors.New("invalid action")
	}

	t.log.Debugf("len(rest): %#v", reader.Len())
	if reader.Len()%6 != 0 {
		return errors.New("invalid peer list")
	}

	count := reader.Len() / 6
	t.log.Debugf("count of peers: %#v", count)
	r.Peers = make([]peerAddr, count)
	for i := 0; i < count; i++ {
		if err = binary.Read(reader, binary.BigEndian, &r.Peers[i]); err != nil {
			return err
		}
	}
	t.log.Debugf("r.Peers: %#v\n", r.Peers)

	return nil
}
