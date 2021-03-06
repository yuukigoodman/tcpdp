package reader

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/k1LoW/tcpdp/dumper"
	"github.com/rs/xid"
	"go.uber.org/zap"
)

const anyIP = "0.0.0.0"

var maxPacketLen = 0xFFFF // 65535

// ParseTarget parse target to host:port
func ParseTarget(target string) (string, uint16, error) {
	var port uint16
	var host string
	if target == "" {
		host = ""
		port = uint16(0)
	} else if strings.Contains(target, ":") {
		tAddr, err := net.ResolveTCPAddr("tcp", target)
		if err != nil {
			return "", uint16(0), nil
		}
		host = tAddr.IP.String()
		port = uint16(tAddr.Port)
	} else if strings.Contains(target, ".") {
		host = target
		port = uint16(0)
	} else {
		host = ""
		port64, err := strconv.ParseUint(target, 10, 64)
		if err != nil {
			return "", uint16(0), nil
		}
		port = uint16(port64)
	}
	return host, port, nil
}

// NewBPFFilterString return string for BPF
func NewBPFFilterString(host string, port uint16) string {
	f := fmt.Sprintf("tcp and host %s and port %d", host, port)
	if (host == "" || host == anyIP) && port > 0 {
		f = fmt.Sprintf("tcp port %d", port)
	} else if (host != "" && host != anyIP) && port == 0 {
		f = fmt.Sprintf("tcp and host %s", host)
	} else if (host == "" || host == anyIP) && port == 0 {
		f = "tcp"
	}
	return f
}

// PacketReader struct
type PacketReader struct {
	ctx          context.Context
	cancel       context.CancelFunc
	packetSource *gopacket.PacketSource
	dumper       dumper.Dumper
	pValues      []dumper.DumpValue
	logger       *zap.Logger
	packetBuffer chan gopacket.Packet
}

// NewPacketReader return PacketReader
func NewPacketReader(
	ctx context.Context,
	cancel context.CancelFunc,
	packetSource *gopacket.PacketSource,
	dumper dumper.Dumper,
	pValues []dumper.DumpValue,
	logger *zap.Logger,
	internalBufferLength int,
) PacketReader {
	internalPacketBuffer := make(chan gopacket.Packet, internalBufferLength)

	reader := PacketReader{
		ctx:          ctx,
		cancel:       cancel,
		packetSource: packetSource,
		dumper:       dumper,
		pValues:      pValues,
		logger:       logger,
		packetBuffer: internalPacketBuffer,
	}

	return reader
}

// ReadAndDump from gopacket.PacketSource
func (r *PacketReader) ReadAndDump(host string, port uint16) error {
	packetChan := r.packetSource.Packets()

	go r.handlePacket(host, port)
	go r.checkBufferdPacket(packetChan)

	for {
		select {
		case <-r.ctx.Done():
			return nil
		case packet := <-packetChan:
			r.packetBuffer <- packet
		}
	}
}

func (r *PacketReader) handlePacket(host string, port uint16) error {
	mMap := map[string]*dumper.ConnMetadata{}        // metadata map per connection
	mssMap := map[string]int{}                       // TCP MSS map per connection
	bMap := map[string]map[dumper.Direction][]byte{} // long payload map per direction

	for {
		select {
		case <-r.ctx.Done():
			return nil
		case packet := <-r.packetBuffer:
			if packet == nil {
				r.cancel()
				return nil
			}
			ipLayer := packet.Layer(layers.LayerTypeIPv4)
			if ipLayer == nil {
				continue
			}
			tcpLayer := packet.Layer(layers.LayerTypeTCP)
			if tcpLayer == nil {
				continue
			}
			ip, _ := ipLayer.(*layers.IPv4)
			tcp, _ := tcpLayer.(*layers.TCP)

			var key string
			var direction dumper.Direction
			srcToDstKey := fmt.Sprintf("%s:%d->%s:%d", ip.SrcIP.String(), tcp.SrcPort, ip.DstIP.String(), tcp.DstPort)
			dstToSrcKey := fmt.Sprintf("%s:%d->%s:%d", ip.DstIP.String(), tcp.DstPort, ip.SrcIP.String(), tcp.SrcPort)
			if (host == "" || ip.DstIP.String() == host) && uint16(tcp.DstPort) == port {
				key = srcToDstKey
				direction = dumper.SrcToDst
			} else if (host == "" || ip.SrcIP.String() == host) && uint16(tcp.SrcPort) == port {
				key = dstToSrcKey
				direction = dumper.DstToSrc
			} else {
				key = "-"
				direction = dumper.Unknown
			}

			if tcp.SYN && !tcp.ACK {
				if direction == dumper.Unknown {
					key = srcToDstKey
				}

				// TCP connection start
				_, ok := mMap[key]
				if ok {
					delete(mMap, key)
				}

				// TCP connection start ( hex, mysql, pg )
				connID := xid.New().String()
				mss := int(binary.BigEndian.Uint16(tcp.LayerContents()[22:24]))
				connMetadata := r.dumper.NewConnMetadata()
				connMetadata.DumpValues = []dumper.DumpValue{
					dumper.DumpValue{
						Key:   "conn_id",
						Value: connID,
					},
				}
				mMap[key] = connMetadata
				mssMap[key] = mss
				bMap[key] = newByteMap()
			} else if tcp.SYN && tcp.ACK {
				if direction == dumper.Unknown {
					key = dstToSrcKey
				}

				_, ok := mMap[key]
				if !ok {
					// TCP connection start ( hex, mysql, pg )
					connID := xid.New().String()
					connMetadata := r.dumper.NewConnMetadata()
					connMetadata.DumpValues = []dumper.DumpValue{
						dumper.DumpValue{
							Key:   "conn_id",
							Value: connID,
						},
					}
					mMap[key] = connMetadata
				}

				mss := int(binary.BigEndian.Uint16(tcp.LayerContents()[22:24]))
				current, ok := mssMap[key]
				if !ok || mss < current {
					mssMap[key] = mss
				}
				mMap[key].DumpValues = append(mMap[key].DumpValues, dumper.DumpValue{
					Key:   "mss",
					Value: mss,
				})
			} else if tcp.FIN {
				// TCP connection end
				_, ok := mMap[key]
				if ok {
					delete(mMap, key)
				}
				_, ok = mssMap[key]
				if ok {
					delete(mssMap, key)
				}
				_, ok = bMap[key]
				if ok {
					delete(bMap, key)
				}
				if direction == dumper.Unknown {
					for _, key := range []string{srcToDstKey, dstToSrcKey} {
						_, ok := mMap[key]
						if ok {
							delete(mMap, key)
						}
					}
				}
			}

			_, ok := bMap[key]
			if !ok {
				bMap[key] = newByteMap()
			}

			in := tcpLayer.LayerPayload()
			if len(in) == 0 {
				continue
			}

			mss, ok := mssMap[key]
			if ok {
				maxPacketLen = mss - (len(tcp.LayerContents()) - 20)
			}
			if len(in) == maxPacketLen {
				bMap[key][direction] = append(bMap[key][direction], in...)
				continue
			}
			bb, ok := bMap[key][direction]
			if ok {
				in = append(bb, in...)
				bMap[key][direction] = nil
			}
			if direction == dumper.Unknown {
				for _, k := range []string{srcToDstKey, dstToSrcKey} {
					_, ok := mMap[k]
					if ok {
						key = k
					}
				}
			}

			connMetadata, ok := mMap[key]
			if !ok {
				connMetadata = r.dumper.NewConnMetadata()
			}

			ts := packet.Metadata().CaptureInfo.Timestamp

			values := []dumper.DumpValue{
				dumper.DumpValue{
					Key:   "ts",
					Value: ts,
				},
				dumper.DumpValue{
					Key:   "src_addr",
					Value: fmt.Sprintf("%s:%d", ip.SrcIP.String(), tcp.SrcPort),
				},
				dumper.DumpValue{
					Key:   "dst_addr",
					Value: fmt.Sprintf("%s:%d", ip.DstIP.String(), tcp.DstPort),
				},
			}

			read := r.dumper.Read(in, direction, connMetadata)
			mMap[key] = connMetadata
			if len(read) == 0 {
				continue
			}

			values = append(values, read...)
			values = append(values, r.pValues...)
			values = append(values, connMetadata.DumpValues...)

			r.dumper.Log(values)
		}
	}
}

func (r *PacketReader) checkBufferdPacket(packetChan chan gopacket.Packet) {
	t := time.NewTicker(1 * time.Second)
L:
	for {
		select {
		case <-r.ctx.Done():
			break L
		case <-t.C:
			gopacketBuffered := len(packetChan)
			internalPacketBuffered := len(r.packetBuffer)
			if internalPacketBuffered > (cap(r.packetBuffer)/10) || gopacketBuffered > (cap(packetChan)/10) {
				r.logger.Info("buffered packet stats", zap.Int("internal_buffered", internalPacketBuffered), zap.Int("gopacket_buffered", gopacketBuffered))
			}
		}
	}
	t.Stop()
}

func newByteMap() map[dumper.Direction][]byte {
	return map[dumper.Direction][]byte{
		dumper.SrcToDst: []byte{},
		dumper.DstToSrc: []byte{},
		dumper.Unknown:  []byte{},
	}
}
