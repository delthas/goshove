//go:generate picopacker goshove.glade glade.go glade
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gotk3/gotk3/glib"

	"golang.org/x/time/rate"

	"github.com/gotk3/gotk3/gdk"
	"github.com/gotk3/gotk3/gtk"
)

const relay = "delthas.fr:14763"

type shoveMode int

const (
	modeConnecting shoveMode = iota
	modeWaiting
	modeSendWaiting
	modeSending
	modeReceiveWaiting
	modeReceiving
	modeFinishing
)

type congestionMode int

const (
	modeTurbo congestionMode = iota
	modeForcage
)

type packetType byte

const (
	packetPing        packetType = 0
	packetRequestSend packetType = 1
	packetAcceptSend  packetType = 2
	packetChunk       packetType = 3
	packetAck         packetType = 4
	packetFinished    packetType = 5
	packetFinishedAck packetType = 6
)

type RepeatBuffer struct {
	buffer []byte
	period time.Duration
}

var localIpMasks []*net.IPNet

var builder *gtk.Builder
var logBuffer *gtk.TextBuffer
var fileSend *gtk.Button
var progressBar *gtk.ProgressBar

var mode = modeConnecting
var repeatRefresh = make(chan RepeatBuffer, 1)

var localOffers = make(map[uint32]string)
var peerOffers = make(map[uint32]string)
var maxId uint32
var currentId uint32
var peerAddress *net.UDPAddr
var c *net.UDPConn

var ackChunkCh = make(chan uint32, 1)

var sendLimiter *rate.Limiter
var sendBandwidth uint64
var sendMode congestionMode
var sendChunks []bool
var sendSent int
var sendAckId uint32
var sendLastBandwidths = make([]uint64, 20)
var sendLastBandwidthsSum uint64
var sendLastBandwidthsIndex int

var receiveChunks []bool
var receiveNLastN uint32
var receiveFile *os.File

const mtu = 1500 - 20 - 8
const chunkMtu = mtu - 1 - 12
const ackMtu = mtu - 1 - 12
const defaultBandwidth = 512 * 1024 // 512 kiB
const minBandwidth = 64 * 1024      // 64 kiB

func main() {
	runtime.LockOSThread()
	rand.Seed(time.Now().UnixNano())

	gtk.Init(nil)

	var err error
	builder, err = gtk.BuilderNew()
	if err != nil {
		panic(err)
	}
	if err := builder.AddFromString(string(glade)); err != nil {
		panic(err)
	}

	logBuffer = getTextBuffer("log-buffer")
	fileChooser := getFileChooserButton("file-chooser")
	fileSend = getButton("file-send")
	addressEntry := getEntry("address-entry")
	connect := getDialog("dialog-connect")
	incoming := getDialog("dialog-incoming")
	ipDialog := getMessageDialog("dialog-ip")
	fileName := getLabel("file-name")
	fileSize := getLabel("file-size")
	progressBar = getProgressBar("progress")

	relayAddr, err := net.ResolveUDPAddr("udp4", relay)
	if err != nil {
		panic(err)
	}
	c, err = net.ListenUDP("udp4", nil)
	if err != nil {
		panic(err)
	}
	localPort := c.LocalAddr().(*net.UDPAddr).Port

	peerAddressStr := ""
	res := connect.Run()
	if res == -2 {
		peerAddressStr, err = addressEntry.GetText()
		if err != nil {
			panic(err)
		}
		peerAddressStr = strings.TrimSpace(peerAddressStr)
	}
	connect.Destroy()
	if res == -1 {
		ip, err := getIp()
		if err != nil {
			panic(err)
		}

		setClipboard(fmt.Sprintf("%s:%d", ip, localPort))
		logf("IP: %s:%d", ip, localPort)

		_ = ipDialog.Run()
		ipDialog.Destroy()
	} else if res == -2 {
		peerAddress, err = net.ResolveUDPAddr("udp4", peerAddressStr)
		if err != nil {
			panic(err)
		}
		peerAddress.IP = peerAddress.IP.To4()
	} else {
		panic(errors.New("unknown dialog response id"))
	}

	win := getWindow("window")
	win.Connect("destroy", func() {
		gtk.MainQuit()
	})
	win.ShowAll()

	go func() {
		buffer := make([]byte, mtu)

		peerAddressCh := make(chan *net.UDPAddr)
		go func() {
			for {
				n, addr, err := c.ReadFromUDP(buffer)
				if err != nil {
					panic(err)
				}
				if addrEqual(addr, relayAddr) && n == 8 {
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

		if peerAddressStr == "" || !localAddr(peerAddress.IP) {
			var relayPayload []byte
			if peerAddressStr == "" {
				relayPayload = []byte{byte(localPort >> 8), byte(localPort)}
			} else {
				relayPayload = []byte{byte(localPort >> 8), byte(localPort), peerAddress.IP[0], peerAddress.IP[1], peerAddress.IP[2], peerAddress.IP[3], byte(peerAddress.Port >> 8), byte(peerAddress.Port)}
			}
		outer1:
			for {
				c.WriteToUDP(relayPayload, relayAddr)
				select {
				case <-time.After(500 * time.Millisecond):
				case peerAddress = <-peerAddressCh:
					break outer1
				}
			}
		}

		go func() {
			rb := <-repeatRefresh
			for {
				if rb.buffer != nil {
					c.WriteToUDP(rb.buffer, peerAddress)
				}
				select {
				case <-time.After(rb.period):
				case rb = <-repeatRefresh:
				}
			}
		}()
		setMode(modeConnecting)

		connectedCh := make(chan struct{})
		go func() {
			for {
				n, addr, err := c.ReadFromUDP(buffer)
				if err != nil {
					panic(err)
				}
				if !addrEqual(peerAddress, addr) || n == 0 {
					continue
				}
				packetType := packetType(buffer[0])
				buffer := buffer[1:n]
				n--
				switch packetType {
				case packetPing: // ping
					if mode == modeConnecting {
						setMode(modeWaiting)
						connectedCh <- struct{}{}
					}
				case packetRequestSend: // request_send uint32:id, uint64:size, []byte:name
					if n < 13 {
						continue
					}
					if mode == modeWaiting {
						id := binary.BigEndian.Uint32(buffer[:4])
						if _, ok := peerOffers[id]; !ok {
							size := binary.BigEndian.Uint64(buffer[4:12])
							name := string(buffer[12:])
							peerOffers[id] = name
							go func() {
								resCh := make(chan gtk.ResponseType)
								glib.IdleAdd(func() {
									fileName.SetText(name)
									fileSize.SetText(fmt.Sprintf("%d kio", size/1024))
									res := incoming.Run()
									incoming.Hide()
									resCh <- res
								})
								res := <-resCh
								if res == gtk.RESPONSE_ACCEPT {
									currentId = id

									receiveFile, err = os.OpenFile(name, os.O_TRUNC|os.O_CREATE|os.O_RDWR, 0600)
									if err != nil {
										panic(err)
									}
									if err := receiveFile.Truncate(int64(size)); err != nil {
										panic(err)
									}

									setMode(modeReceiveWaiting)
									buf := make([]byte, 5)
									buf[0] = byte(packetAcceptSend)
									binary.BigEndian.PutUint32(buf[1:5], id)
									setRepeatSend(buf, 500*time.Millisecond)

									var ackId uint32 = 0
									var receivedNew []uint32
									receiveChunks = make([]bool, (size-1)/chunkMtu+1)
									var received uint32 = 0

									receiveBuckets := make([]uint32, 10*10) // 100ms/bucket
									var lastStampReceive int

									maxChunksPerAck := ackMtu / 4

									buffer := make([]byte, mtu)
									buffer[0] = byte(packetAck)
									binary.BigEndian.PutUint32(buffer[1:], id)

									var timeout <-chan time.Time
									for received < uint32(len(receiveChunks)) {
										var chunk uint32
										var receivedChunk bool
										select {
										case chunk = <-ackChunkCh:
											receivedChunk = true
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
											receiveChunks[chunk] = true
											receivedNew = append(receivedNew, chunk)
											received++
											if received < uint32(len(receiveChunks)) && len(receivedNew) < maxChunksPerAck {
												if received == 1 {
													timeout = time.After(500 * time.Millisecond)
												}
												continue
											}
										}

										glib.IdleAdd(func() {
											progressBar.SetFraction(float64(received) / float64(len(receiveChunks)))
										})

										var lastN uint32 = 0
										var cReceive uint32 = 0
										for i := stampReceive; i >= 0 && cReceive < receiveNLastN && lastN < received; i, cReceive = i-1, cReceive+1 {
											lastN += receiveBuckets[i]
										}
										for i := len(receiveBuckets) - 1; i > stampReceive && cReceive < receiveNLastN && lastN < received; i, cReceive = i-1, cReceive+1 {
											lastN += receiveBuckets[i]
										}
										lastN = lastN * 10 / cReceive // convert from 100ms to 1s

										logf("download speed: %5dko/s, averaging over %d seconds\n", lastN*chunkMtu/1000, receiveNLastN/10)

										binary.BigEndian.PutUint32(buffer[5:], ackId)
										ackId++
										binary.BigEndian.PutUint32(buffer[9:], lastN)

										for i, v := range receivedNew {
											binary.BigEndian.PutUint32(buffer[13+4*i:], v)
										}
										timeout = time.After(500 * time.Millisecond)

										c.WriteToUDP(buffer[:13+4*len(receivedNew)], peerAddress)
										receivedNew = nil
									}

									setMode(modeFinishing)
									buffer = make([]byte, 5)
									buffer[0] = byte(packetFinished)
									binary.BigEndian.PutUint32(buffer[1:], id)
									setRepeatSend(buffer, 250*time.Millisecond)

									receiveFile.Close()
									receiveFile = nil
								}
							}()
						}
					}
				case packetAcceptSend: // accept_send uint32:id
					if n < 4 {
						continue
					}
					if mode == modeSendWaiting {
						id := binary.BigEndian.Uint32(buffer[:4])
						path, ok := localOffers[id]
						if !ok {
							continue
						}
						currentId = id
						setMode(modeSending)
						setRepeatSend(nil, 0)

						go func() {
							send(path)
						}()
					}
				case packetChunk: // chunk uint32:id, uint32: NlastN, uint32:chunk, []byte: data (update chunkMtu if this is changed!)
					if n < 12 {
						continue
					}
					id := binary.BigEndian.Uint32(buffer)
					if currentId != id {
						continue
					}
					if mode == modeReceiveWaiting || mode == modeReceiving {
						if mode == modeReceiveWaiting {
							setMode(modeReceiving)
						}
						NlastN := binary.BigEndian.Uint32(buffer[4:])
						receiveNLastN = NlastN

						chunk := binary.BigEndian.Uint32(buffer[8:])
						if !receiveChunks[chunk] {
							if _, err := receiveFile.WriteAt(buffer[12:], int64(chunk)*chunkMtu); err != nil {
								panic(err)
							}
						}

						ackChunkCh <- chunk
					}
				case packetAck: // ack uint32:id, uint32: ackid, uint32:lastN, uint32..:lost (update chunkMtu if this is changed!)
					if n < 12 {
						continue
					}
					if mode == modeSending {
						id := binary.BigEndian.Uint32(buffer)
						if currentId != id {
							continue
						}
						for i := 12; i < n; i += 4 {
							sendChunks[binary.BigEndian.Uint32(buffer[i:])] = true
						}
						sendSent += (n - 12) / 4
						glib.IdleAdd(func() {
							progressBar.SetFraction(float64(sendSent) / float64(len(sendChunks)))
						})
						ackId := binary.BigEndian.Uint32(buffer[4:])
						if ackId == sendAckId ||
							(ackId > sendAckId && ackId-sendAckId < math.MaxUint32/2) ||
							(ackId < sendAckId && sendAckId-ackId > math.MaxUint32/2) {
							sendAckId = ackId
							lastN := uint64(binary.BigEndian.Uint32(buffer[8:])) * chunkMtu
							var modeString string
							switch sendMode {
							case modeTurbo:
								modeString = "turbo"
								sendBandwidth = 5 * lastN
							case modeForcage:
								modeString = "forcage"
								sendBandwidth = 120 * lastN / 100
							default:
								panic(errors.New("unknown congestion control mode"))
							}
							if sendBandwidth < minBandwidth {
								sendBandwidth = minBandwidth
							}
							if sendMode == modeTurbo && sendLastBandwidthsSum > uint64(len(sendLastBandwidths)-1)*sendBandwidth {
								sendMode = modeForcage
							}
							sendLastBandwidthsSum += -sendLastBandwidths[sendLastBandwidthsIndex] + sendBandwidth
							sendLastBandwidths[sendLastBandwidthsIndex] = sendBandwidth
							sendLastBandwidthsIndex = (sendLastBandwidthsIndex + 1) % len(sendLastBandwidths)
							logf("upload speed: targeting: %5dko/s, received: %5dko/s, mode: %s\n", sendBandwidth/1000, lastN/1000, modeString)
							sendLimiter.SetLimit(rate.Limit(float64(sendBandwidth)))
						}
					}
				case packetFinished: // uint32:id
					if n < 4 {
						continue
					}
					id := binary.BigEndian.Uint32(buffer)
					if id != currentId {
						continue
					}
					buffer := make([]byte, 5)
					buffer[0] = byte(packetFinishedAck)
					binary.BigEndian.PutUint32(buffer[1:], id)
					c.WriteToUDP(buffer, peerAddress)
					if mode == modeSending {
						setMode(modeWaiting)
					}
				case packetFinishedAck: // uint32:id
					if n < 4 {
						continue
					}
					id := binary.BigEndian.Uint32(buffer)
					if id != currentId {
						continue
					}
					if mode == modeFinishing {
						setMode(modeWaiting)
					}
				default:
					panic(errors.New("unknown packet type"))
				}
			}
		}()

		<-connectedCh
		fileSend.Connect("clicked", func(_ *gtk.Button) {
			if mode != modeWaiting {
				return
			}
			file := fileChooser.GetFilename()
			if file == "" {
				return
			}
			fi, err := os.Stat(file)
			if err != nil {
				panic(err)
			}
			setMode(modeSendWaiting)

			name := []byte(filepath.Base(file))
			buf := make([]byte, 1+4+8+len(name))
			buf[0] = byte(packetRequestSend)
			binary.BigEndian.PutUint32(buf[1:5], maxId)
			localOffers[maxId] = file
			maxId++
			binary.BigEndian.PutUint64(buf[5:13], uint64(fi.Size()))
			copy(buf[13:], name)
			setRepeatSend(buf, 500*time.Millisecond)
		})
	}()

	gtk.Main()
}

func send(path string) {
	f, err := os.Open(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		panic(err)
	}
	size := uint64(fi.Size())
	id := currentId

	sendChunks = make([]bool, (size-1)/chunkMtu+1)
	sendSent = 0

	buffer := make([]byte, mtu)
	buffer[0] = byte(packetChunk)
	binary.BigEndian.PutUint32(buffer[1:], id)

	sendMode = modeTurbo
	sendBandwidth = defaultBandwidth
	sendLimiter = rate.NewLimiter(rate.Limit(float64(sendBandwidth)), 10000)

	for mode == modeSending && currentId == id && sendSent < len(sendChunks) {
		for i := uint64(0); i < size; i += chunkMtu {
			if sendChunks[i/chunkMtu] {
				continue
			}
			switch sendMode {
			case modeTurbo:
				binary.BigEndian.PutUint32(buffer[5:], 10) // in 100ms
			case modeForcage:
				binary.BigEndian.PutUint32(buffer[5:], 30) // in 100ms
			default:
				panic(errors.New("unknown congestion control mode"))
			}
			binary.BigEndian.PutUint32(buffer[9:], uint32(i/chunkMtu))
			var n uint64 = chunkMtu
			if size-i < chunkMtu {
				n = size - i
			}
			_, err := f.ReadAt(buffer[13:13+n], int64(i))
			if err != nil {
				panic(err)
			}
			if n == 0 {
				break
			}
			n += 13
			sendLimiter.WaitN(context.Background(), int(n))
			if _, err = c.WriteToUDP(buffer[:n], peerAddress); err != nil {
				panic(err)
			}
		}
	}
}

func logf(format string, args ...interface{}) {
	glib.IdleAdd(func() {
		logBuffer.Insert(logBuffer.GetEndIter(), fmt.Sprintf(format, args...))
	})
}

func setMode(newMode shoveMode) {
	mode = newMode
	switch mode {
	case modeConnecting:
		setRepeatSend([]byte{byte(packetPing)}, 500*time.Millisecond)
	case modeWaiting:
		fallthrough
	case modeReceiving:
		setRepeatSend([]byte{byte(packetPing)}, 10*time.Second)
	case modeFinishing:
	case modeSending:
	case modeReceiveWaiting:
	case modeSendWaiting:
		// custom buffer
	}
	glib.IdleAdd(func() {
		fileSend.SetSensitive(mode == modeWaiting)
		if mode == modeFinishing {
			progressBar.SetFraction(1.0)
		} else {
			progressBar.SetFraction(0.0)
		}
	})
}

func setRepeatSend(buffer []byte, period time.Duration) {
	repeatRefresh <- RepeatBuffer{
		buffer: buffer,
		period: period,
	}
}

func setClipboard(text string) {
	clipboard, _ := gtk.ClipboardGet(gdk.GdkAtomIntern("CLIPBOARD", false))
	clipboard.SetText(text)
}

func getIp() (string, error) {
	r, err := http.Get("https://api.ipify.org/")
	if err != nil {
		return "", err
	}
	ip, err := ioutil.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return "", err
	}
	return string(ip), nil
}

func getProgressBar(name string) *gtk.ProgressBar {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.ProgressBar)
}

func getTextBuffer(name string) *gtk.TextBuffer {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.TextBuffer)
}

func getButton(name string) *gtk.Button {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.Button)
}

func getLabel(name string) *gtk.Label {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.Label)
}

func getFileChooserButton(name string) *gtk.FileChooserButton {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.FileChooserButton)
}

func getEntry(name string) *gtk.Entry {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.Entry)
}

func getDialog(name string) *gtk.Dialog {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.Dialog)
}

func getMessageDialog(name string) *gtk.MessageDialog {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.MessageDialog)
}

func getWindow(name string) *gtk.Window {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.Window)
}

func addrEqual(v1 *net.UDPAddr, v2 *net.UDPAddr) bool {
	return v1.IP.Equal(v2.IP) && v1.Port == v2.Port
}

func localAddr(ip net.IP) bool {
	if localIpMasks == nil {
		for _, cidr := range []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
			_, block, _ := net.ParseCIDR(cidr)
			localIpMasks = append(localIpMasks, block)
		}
	}
	for _, v := range localIpMasks {
		if v.Contains(ip) {
			return true
		}
	}
	return false
}
