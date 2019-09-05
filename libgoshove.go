package libgoshove

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/ioutil"
	"math"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const relay = "delthas.fr:14763"

var ClosedErr = errors.New("connection is closed")
var OfferDoneErr = errors.New("file offer was already accepted or rejected")
var TimeoutErr = errors.New("connect or read timeout")
var RejectErr = errors.New("file offer rejected by peer")

type sendState int

const (
	modeSendNone sendState = iota
	modeSendAccept
	modeSend
)

type receiveState int

const (
	modeReceiveNone receiveState = iota
	modeReceive
)

type congestionMode int

const (
	modeTurbo congestionMode = iota
	modeForcage
)

type packetType byte

const (
	packetPing          packetType = 0
	packetRequestSend   packetType = 1
	packetAcceptSend    packetType = 2
	packetChunk         packetType = 3
	packetAck           packetType = 4
	packetFinished      packetType = 5
	packetAcceptSendAck packetType = 6
)

var localIpMasks []*net.IPNet

const mtu = 1500 - 20 - 8
const chunkMtu = mtu - 1 - 12
const ackMtu = mtu - 1 - 12
const defaultBandwidth = 512 * 1024 // 512 kiB
const minBandwidth = 64 * 1024      // 64 kiB

type Offer struct {
	Name string
	Size int64
	c    *Conn
	id   uint32
	done bool
}

type ConnSettings struct {
	ReadTimeout    time.Duration
	ConnectTimeout time.Duration
	ListenPort     int
}

type AcceptSettings struct {
	ConnSettings
	SkipFetchIp    bool
	ListenCallback func(ip string, port int)
}

type Conn struct {
	Offers chan Offer

	peerOffers     map[uint32]bool // true: accepted or pending, false: rejected, unset: new
	finishedOffers map[uint32]struct{}
	currentId      uint32
	relayAddr      *net.UDPAddr
	peerAddress    *net.UDPAddr
	c              *net.UDPConn
	localPort      int
	readTimeout    time.Duration
	connected      bool
	closed         bool

	connectedCh chan struct{}

	sendMutex               sync.Mutex
	sendState               sendState
	sendId                  uint32
	sendNextId              uint32
	sendLimiter             *rate.Limiter
	sendBandwidth           int64
	sendCongestion          congestionMode
	sendChunks              []bool
	sendSent                int
	sendSize                int64
	sendAckId               uint32
	sendLastBandwidths      []int64
	sendLastBandwidthsSum   int64
	sendLastBandwidthsIndex int
	sendAcceptCh            chan bool
	sendProgressCb          func(totalSent int64, bandwidth int64)
	sendLastN               int64

	receiveErrorCh chan error
	receiveAckCh   chan uint32
	receiveMutex   sync.Mutex
	receiveId      uint32
	receiveChunks  []bool
	receiveNLastN  uint32
	receiveSize    int64
	receiveFile    io.WriterAt
	receiveState   receiveState
}

func newConn() *Conn {
	return &Conn{
		Offers:             make(chan Offer, 1),
		peerOffers:         make(map[uint32]bool),
		finishedOffers:     make(map[uint32]struct{}),
		receiveAckCh:       make(chan uint32, 1),
		receiveErrorCh:     make(chan error, 1),
		sendLastBandwidths: make([]int64, 20),
		sendAcceptCh:       make(chan bool, 1),
		sendState:          modeSendNone,
		connectedCh:        make(chan struct{}, 1),
	}
}

func initConn(settings *ConnSettings) (*Conn, error) {
	c := newConn()
	var err error
	c.relayAddr, err = net.ResolveUDPAddr("udp4", relay)
	if err != nil {
		return nil, err
	}
	c.c, err = net.ListenUDP("udp4", &net.UDPAddr{
		Port: settings.ListenPort,
	})
	if err != nil {
		return nil, err
	}
	c.localPort = c.c.LocalAddr().(*net.UDPAddr).Port
	c.readTimeout = settings.ReadTimeout
	return c, nil
}

func Accept(settings *AcceptSettings) (*Conn, error) {
	c, err := initConn(&settings.ConnSettings)
	if err != nil {
		return nil, err
	}
	ip := ""
	if !settings.SkipFetchIp {
		ip, err = fetchIp()
		if err != nil {
			c.Close()
			return nil, err
		}
	}
	if settings.ListenCallback != nil {
		go settings.ListenCallback(ip, c.localPort)
	}

	var after <-chan time.Time = nil
	if settings.ConnectTimeout > 0 {
		after = time.After(settings.ConnectTimeout)
	}
	go c.run()
	select {
	case <-c.connectedCh:
		return c, nil
	case <-after:
		c.Close()
		return nil, TimeoutErr
	}
}

func Dial(address string, settings *ConnSettings) (*Conn, error) {
	c, err := initConn(settings)
	if err != nil {
		return nil, err
	}
	c.peerAddress, err = net.ResolveUDPAddr("udp4", address)
	if err != nil {
		return nil, err
	}
	c.peerAddress.IP = c.peerAddress.IP.To4()

	var after <-chan time.Time = nil
	if settings.ConnectTimeout > 0 {
		after = time.After(settings.ConnectTimeout)
	}
	go c.run()
	select {
	case <-c.connectedCh:
		return c, nil
	case <-after:
		c.Close()
		return nil, TimeoutErr
	}
}

func (c *Conn) read(buf []byte) (int, *net.UDPAddr, error) {
	n, addr, err := c.c.ReadFromUDP(buf)
	if c.closed {
		return n, addr, ClosedErr
	} else if os.IsTimeout(err) {
		c.Close()
		return n, addr, TimeoutErr
	} else if err != nil {
		return 0, nil, nil
	}
	return n, addr, nil
}

func (c *Conn) write(buf []byte, addr *net.UDPAddr) error {
	c.c.WriteToUDP(buf, addr)
	if c.closed {
		return ClosedErr
	}
	return nil
}

func (c *Conn) Send(name string, size int64, r io.ReaderAt, progressCb func(totalSent int64, bandwidth int64)) error {
	c.sendMutex.Lock()
	defer c.sendMutex.Unlock()
	if c.closed {
		return ClosedErr
	}

	c.sendId = c.sendNextId
	c.sendNextId++

	buf := make([]byte, 1+4+8+len(name))
	buf[0] = byte(packetRequestSend)
	binary.BigEndian.PutUint32(buf[1:5], c.sendId)
	binary.BigEndian.PutUint64(buf[5:13], uint64(size))
	copy(buf[13:], name)

	c.sendState = modeSendAccept
	if err := c.write(buf, c.peerAddress); err != nil {
		return err
	}
outer:
	for {
		select {
		case <-time.After(500 * time.Millisecond):
			if err := c.write(buf, c.peerAddress); err != nil {
				return err
			}
		case accept := <-c.sendAcceptCh:
			if !accept {
				c.sendState = modeSendNone
				return RejectErr
			}
			break outer
		}
	}
	c.sendState = modeSend

	c.sendProgressCb = progressCb
	c.sendChunks = make([]bool, (size-1)/chunkMtu+1)
	c.sendSent = 0
	c.sendSize = size

	buffer := make([]byte, mtu)
	buffer[0] = byte(packetChunk)
	binary.BigEndian.PutUint32(buffer[1:], c.sendId)

	// TODO reuse previous bandwidth
	c.sendCongestion = modeTurbo
	c.sendBandwidth = defaultBandwidth
	c.sendLimiter = rate.NewLimiter(rate.Limit(float64(c.sendBandwidth)), 10000)

	for c.sendSent < len(c.sendChunks) {
		for i := int64(0); i < size; i += chunkMtu {
			if c.sendChunks[i/chunkMtu] {
				continue
			}
			switch c.sendCongestion {
			case modeTurbo:
				binary.BigEndian.PutUint32(buffer[5:], 10) // in 100ms
			case modeForcage:
				binary.BigEndian.PutUint32(buffer[5:], 30) // in 100ms
			default:
				panic(errors.New("unknown congestion control mode"))
			}
			binary.BigEndian.PutUint32(buffer[9:], uint32(i/chunkMtu))
			var n int64 = chunkMtu
			if size-i < chunkMtu {
				n = size - i
			}
			_, err := r.ReadAt(buffer[13:13+n], i)
			if err != nil {
				c.Close()
				return err
			}
			if n == 0 {
				break
			}
			n += 13
			c.sendLimiter.WaitN(context.Background(), int(n))
			if err := c.write(buffer[:n], c.peerAddress); err != nil {
				return err
			}
		}
	}

	c.sendState = modeSendNone
	return nil
}

func (c *Conn) Close() {
	if c.closed {
		return
	}
	c.closed = true
	c.c.Close()
	close(c.Offers)
}

func (o Offer) Accept(w io.WriterAt, progressCb func(totalReceived int64, bandwidth int64)) error {
	o.c.receiveMutex.Lock()
	defer o.c.receiveMutex.Unlock()
	if o.done {
		return OfferDoneErr
	}
	o.done = true
	if o.c.closed {
		return ClosedErr
	}

	o.c.receiveId = o.id
	o.c.receiveFile = w
	o.c.receiveSize = o.Size
	o.c.receiveState = modeReceive

	// TODO send accept immediately?

	var ackId uint32 = 0
	var receivedNew []uint32
	o.c.receiveChunks = make([]bool, (o.Size-1)/chunkMtu+1)
	var received uint32 = 0

	receiveBuckets := make([]uint32, 10*10) // 100ms/bucket
	var lastStampReceive int

	maxChunksPerAck := ackMtu / 4

	buffer := make([]byte, mtu)
	buffer[0] = byte(packetAck)
	binary.BigEndian.PutUint32(buffer[1:], o.c.receiveId)

	var timeout <-chan time.Time
	for received < uint32(len(o.c.receiveChunks)) {
		var chunk uint32
		var receivedChunk bool
		select {
		case chunk = <-o.c.receiveAckCh:
			receivedChunk = true
		case err := <-o.c.receiveErrorCh:
			return err
		case <-timeout: // can intentionally be nil before any packets are received
			receivedChunk = false
		}
		now := time.Now()
		stampReceive := (now.Second()%10)*10 + now.Nanosecond()/100000000 // 100ms
		if stampReceive != lastStampReceive {
			if stampReceive > lastStampReceive {
				for i := lastStampReceive + 1; i <= stampReceive; i++ {
					receiveBuckets[i] = 0
				}
			} else {
				for i := lastStampReceive + 1; i < len(receiveBuckets); i++ {
					receiveBuckets[i] = 0
				}
				for i := 0; i <= stampReceive; i++ {
					receiveBuckets[i] = 0
				}
			}
			lastStampReceive = stampReceive
		}
		if receivedChunk {
			receiveBuckets[stampReceive]++
			o.c.receiveChunks[chunk] = true
			receivedNew = append(receivedNew, chunk)
			received++
			if received < uint32(len(o.c.receiveChunks)) && len(receivedNew) < maxChunksPerAck {
				if received == 1 {
					timeout = time.After(500 * time.Millisecond)
				}
				continue
			}
		}

		var lastN uint32 = 0
		var cReceive uint32 = 0
		for i := stampReceive; i >= 0 && cReceive < o.c.receiveNLastN && lastN < received; i, cReceive = i-1, cReceive+1 {
			lastN += receiveBuckets[i]
		}
		for i := len(receiveBuckets) - 1; i > stampReceive && cReceive < o.c.receiveNLastN && lastN < received; i, cReceive = i-1, cReceive+1 {
			lastN += receiveBuckets[i]
		}
		lastN = lastN * 10 / cReceive // convert from 100ms to 1s

		if progressCb != nil {
			totalReceived := int64(received) * chunkMtu
			if totalReceived > o.Size {
				totalReceived = o.Size
			}
			progressCb(totalReceived, int64(lastN)*chunkMtu)
		}

		binary.BigEndian.PutUint32(buffer[5:], ackId)
		ackId++
		binary.BigEndian.PutUint32(buffer[9:], lastN)

		for i, v := range receivedNew {
			binary.BigEndian.PutUint32(buffer[13+4*i:], v)
		}
		timeout = time.After(500 * time.Millisecond)

		if err := o.c.write(buffer[:13+4*len(receivedNew)], o.c.peerAddress); err != nil {
			return err
		}
		receivedNew = nil
	}

	o.c.finishedOffers[o.c.receiveId] = struct{}{}

	o.c.receiveState = modeReceiveNone
	return nil
}

func (o Offer) Reject() error {
	o.c.receiveMutex.Lock()
	defer o.c.receiveMutex.Unlock()
	if o.done {
		return OfferDoneErr
	}
	o.done = true
	if o.c.closed {
		return ClosedErr
	}

	o.c.peerOffers[o.id] = false
	// TODO synchornisation for map?
	// TODO send extra reject immediately?
	return nil
}

func (c *Conn) run() {
	buffer := make([]byte, mtu)

	peerAddressCh := make(chan *net.UDPAddr) // close means error
	go func() {
		for {
			n, addr, err := c.read(buffer)
			if err != nil {
				close(peerAddressCh)
				break
			}
			if addrEqual(addr, c.relayAddr) && n == 8 {
				peerAddressCh <- &net.UDPAddr{
					IP:   net.IPv4(buffer[4], buffer[5], buffer[6], buffer[7]),
					Port: int(buffer[2])<<8 | int(buffer[3]),
				}
				break
			}
			if localAddr(addr.IP) {
				peerAddressCh <- addr
				break
			}
		}
	}()

	if c.peerAddress == nil || !localAddr(c.peerAddress.IP) {
		var relayPayload []byte
		if c.peerAddress == nil {
			relayPayload = []byte{byte(c.localPort >> 8), byte(c.localPort)}
		} else {
			relayPayload = []byte{byte(c.localPort >> 8), byte(c.localPort), c.peerAddress.IP[0], c.peerAddress.IP[1], c.peerAddress.IP[2], c.peerAddress.IP[3], byte(c.peerAddress.Port >> 8), byte(c.peerAddress.Port)}
		}
	outer1:
		for {
			if err := c.write(relayPayload, c.relayAddr); err != nil {
				return
			}
			select {
			case <-time.After(500 * time.Millisecond):
			case addr, ok := <-peerAddressCh:
				if !ok {
					return
				}
				c.peerAddress = addr
				break outer1
			}
		}
	}

	go func() {
		for {
			if err := c.write([]byte{byte(packetPing)}, c.peerAddress); err != nil {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()

	for {
		n, addr, err := c.read(buffer)
		if err != nil {
			return
		}
		if !addrEqual(c.peerAddress, addr) || n == 0 {
			continue
		}
		if c.readTimeout > 0 {
			c.c.SetReadDeadline(time.Now().Add(c.readTimeout))
		}
		packetType := packetType(buffer[0])
		buffer := buffer[1:n]
		n--
		switch packetType {
		case packetPing: // ping
			if !c.connected {
				c.connected = true
				c.connectedCh <- struct{}{}
			}
		case packetRequestSend: // request_send uint32:id, uint64:size, []byte:name
			if n < 13 {
				continue
			}
			id := binary.BigEndian.Uint32(buffer[:4])
			if accepted, ok := c.peerOffers[id]; ok {
				if !accepted || c.receiveState != modeReceiveNone {
					buffer[0] = byte(packetAcceptSend)
					binary.BigEndian.PutUint32(buffer[1:], id)
					if accepted {
						buffer[5] = 1
					} else {
						buffer[5] = 0
					}
					if err := c.write(buffer[:6], c.peerAddress); err != nil {
						return
					}
				}
				continue
			}
			if c.receiveState != modeReceiveNone {
				continue
			}
			size := binary.BigEndian.Uint64(buffer[4:12])
			name := string(buffer[12:])
			c.peerOffers[id] = true
			c.Offers <- Offer{
				Name: name,
				Size: int64(size),
				c:    c,
				id:   id,
			}
		case packetAcceptSend: // accept_send uint32:id uint8:accepted(bool)
			if n < 5 {
				continue
			}
			id := binary.BigEndian.Uint32(buffer[:4])
			accepted := buffer[4] != 0
			if !accepted {
				buffer[0] = byte(packetAcceptSendAck)
				binary.BigEndian.PutUint32(buffer[1:], id)
				if err := c.write(buffer[:5], c.peerAddress); err != nil {
					return
				}
			}
			if c.sendState != modeSendAccept {
				continue
			}
			if id != c.sendId {
				continue
			}
			c.sendAcceptCh <- accepted
		case packetChunk: // chunk uint32:id, uint32: NlastN, uint32:chunk, []byte: data (update chunkMtu if this is changed!)
			if n < 12 {
				continue
			}
			id := binary.BigEndian.Uint32(buffer)
			if c.currentId != id {
				continue
			}
			if c.receiveState == modeReceiveNone {
				if _, ok := c.finishedOffers[id]; ok {
					buffer[0] = byte(packetFinished)
					binary.BigEndian.PutUint32(buffer[1:], id)
					if err := c.write(buffer[:5], c.peerAddress); err != nil {
						return
					}
					continue
				}
			}
			NlastN := binary.BigEndian.Uint32(buffer[4:])
			c.receiveNLastN = NlastN
			chunk := binary.BigEndian.Uint32(buffer[8:])
			if int64(chunk)*chunkMtu+int64(len(buffer))-12 > c.receiveSize {
				c.receiveErrorCh <- errors.New("invalid file chunk received")
				c.Close()
			}
			if !c.receiveChunks[chunk] {
				if _, err := c.receiveFile.WriteAt(buffer[12:], int64(chunk)*chunkMtu); err != nil {
					c.receiveErrorCh <- err
					c.Close()
				}
			}
			c.receiveAckCh <- chunk
		case packetAck: // ack uint32:id, uint32: ackid, uint32:lastN, uint32..:lost (update chunkMtu if this is changed!)
			if n < 12 {
				continue
			}
			if c.sendState != modeSend {
				continue
			}
			id := binary.BigEndian.Uint32(buffer)
			if c.sendId != id {
				continue
			}
			for i := 12; i < n; i += 4 {
				// TODO check index
				c.sendChunks[binary.BigEndian.Uint32(buffer[i:])] = true
			}
			c.sendSent += (n - 12) / 4

			ackId := binary.BigEndian.Uint32(buffer[4:])
			if ackId == c.sendAckId ||
				(ackId > c.sendAckId && ackId-c.sendAckId < math.MaxUint32/2) ||
				(ackId < c.sendAckId && c.sendAckId-ackId > math.MaxUint32/2) {
				c.sendAckId = ackId
				c.sendLastN = int64(binary.BigEndian.Uint32(buffer[8:])) * chunkMtu
				switch c.sendCongestion {
				case modeTurbo:
					c.sendBandwidth = 5 * c.sendLastN
				case modeForcage:
					c.sendBandwidth = 120 * c.sendLastN / 100
				default:
					panic(errors.New("unknown congestion control mode"))
				}
				if c.sendBandwidth < minBandwidth {
					c.sendBandwidth = minBandwidth
				}
				if c.sendCongestion == modeTurbo && c.sendLastBandwidthsSum > int64(len(c.sendLastBandwidths)-1)*c.sendBandwidth {
					c.sendCongestion = modeForcage
				}
				c.sendLastBandwidthsSum += -c.sendLastBandwidths[c.sendLastBandwidthsIndex] + c.sendBandwidth
				c.sendLastBandwidths[c.sendLastBandwidthsIndex] = c.sendBandwidth
				c.sendLastBandwidthsIndex = (c.sendLastBandwidthsIndex + 1) % len(c.sendLastBandwidths)
				c.sendLimiter.SetLimit(rate.Limit(float64(c.sendBandwidth)))
			}
			if c.sendProgressCb != nil {
				// TODO extra info: targeting c.sendBandwidth, mode c.sendCongestion
				totalSent := int64(c.sendSent) * chunkMtu
				if totalSent > c.sendSize {
					totalSent = c.sendSize
				}
				c.sendProgressCb(totalSent, c.sendLastN)
			}
		case packetFinished: // uint32:id
			if n < 4 {
				continue
			}
			if c.sendState != modeSend {
				continue
			}
			id := binary.BigEndian.Uint32(buffer)
			if id != c.sendId {
				continue
			}
			c.sendSent = len(c.sendChunks)
		default:
			// TODO log unknown packet type?
		}
	}
}

func fetchIp() (string, error) {
	r, err := http.Get("https://api.ipify.org/")
	if err != nil {
		return "", err
	}
	ip, err := ioutil.ReadAll(r.Body)
	if err != nil {
		_ = r.Body.Close()
		return "", err
	}
	if err = r.Body.Close(); err != nil {
		return "", err
	}
	return string(ip), nil
}

func addrEqual(v1 *net.UDPAddr, v2 *net.UDPAddr) bool {
	return v1.IP.Equal(v2.IP) && v1.Port == v2.Port
}

func localAddr(ip net.IP) bool {
	for _, v := range localIpMasks {
		if v.Contains(ip) {
			return true
		}
	}
	return false
}

func init() {
	localIp := []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	localIpMasks = make([]*net.IPNet, len(localIp))
	for i, cidr := range localIp {
		_, block, _ := net.ParseCIDR(cidr)
		localIpMasks[i] = block
	}
}
