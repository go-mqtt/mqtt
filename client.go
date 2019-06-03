package mqtt

import (
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math/bits"
	"net"
	"time"
)

// Receive gets invoked for inbound messages. AtMostOnce ignores the return.
// ExactlyOnce repeates Receive until the return is true and AtLeastOnce may
// repeat Receive even after the return is true.
type Receive func(topic string, message []byte) bool

// Conner is an interface for network connection establishment.
type Connecter func(timeout time.Duration) (net.Conn, error)

// UnsecuredConnecter creates plain network connections.
// See net.Dial for details on the nework & address syntax.
func UnsecuredConnecter(network, address string) Connecter {
	return func(timeout time.Duration) (net.Conn, error) {
		dialer := net.Dialer{Timeout: timeout}
		return dialer.Dial(network, address)
	}
}

// SecuredConnecter creates TLS network connections.
// See net.Dial for details on the nework & address syntax.
func SecuredConnecter(network, address string, conf *tls.Config) Connecter {
	return func(timeout time.Duration) (net.Conn, error) {
		dialer := net.Dialer{Timeout: timeout}
		conn, err := dialer.Dial(network, address)
		if err != nil {
			return nil, err
		}
		return tls.Client(conn, conf), nil
	}
}

// Client manages a connection to one server.
type Client struct {
	packetIDs

	connecter Connecter
	conn      net.Conn
	attrs     Attributes

	writePacket packet

	storage Storage

	listener Receive

	pong   chan struct{}
	closed chan struct{}
}

func NewClient(transport Connecter, attrs *Attributes) *Client {
	c := &Client{
		packetIDs: packetIDs{
			inUse: make(map[uint]struct{}),
			limit: attrs.RequestLimit,
		},
		connecter: transport,
		attrs:     *attrs, // copy
		pong:      make(chan struct{}, 1),
		closed:    make(chan struct{}),
	}

	if c.attrs.Will != nil {
		// make (hidden) copy
		w := c.attrs.Will
		c.attrs.Will = new(Will)
		*c.attrs.Will = *w
	}

	const requestMax = 0x10000 // 16-bit address space
	if c.packetIDs.limit < 1 || c.packetIDs.limit > requestMax {
		c.packetIDs.limit = requestMax
	}

	return c
}

func (c *Client) write(p []byte) error {
	c.conn.SetWriteDeadline(time.Now().Add(c.attrs.WireTimeout))
	n, err := c.conn.Write(p)
	for err != nil {
		select {
		case <-c.closed:
			return ErrClosed
		default:
			break
		}

		if e, ok := err.(net.Error); !ok || !e.Temporary() {
			c.conn.Close()
			return err
		}

		delay := c.attrs.RetryDelay
		log.Print("mqtt: send retry in ", delay, " on temporary network error: ", err)
		time.Sleep(delay)
		c.conn.SetWriteDeadline(time.Now().Add(c.attrs.WireTimeout))

		var more int
		more, err = c.conn.Write(p[n:])
		// handle error in current loop
		n += more
	}

	return nil
}

func (c *Client) readLoop() {
	// determine only here whether closed
	defer close(c.closed)

	buf := make([]byte, 128)
	var bufN, flushN int
	for {
		read, err := c.conn.Read(buf[bufN:])
		bufN += read
		// error handling delayed!
		// consume available first

		var offset int
		if flushN > 0 {
			if flushN >= bufN {
				flushN -= bufN
				bufN = 0
			} else {
				offset = flushN
			}
		}

		for offset+1 < bufN {
			const sizeErr = "mqtt: protocol violation: remaining length declaration exceeds 4 B—connection closed"
			sizeVal, sizeN := binary.Uvarint(buf[offset+1 : bufN])
			if sizeN == 0 {
				// not enough data
				if bufN-offset > 4 {
					c.conn.Close()
					log.Print(sizeErr)
					return
				}
				break
			}
			if sizeN < 0 || sizeN > 4 {
				c.conn.Close()
				log.Print(sizeErr)
				return
			}

			// won't overflow due to 4 byte varint limit
			packetSize := 1 + sizeN + int(sizeVal)
			if packetSize > c.attrs.InSizeLimit {
				log.Printf("mqtt: skipping %d B inbound packet; limit is %d B", packetSize, c.attrs.InSizeLimit)
				flushN = packetSize
				break
			}

			if packetSize < bufN-offset {
				// not enough data
				if packetSize > len(buf) {
					// buff too small
					grow := make([]byte, 1<<uint(bits.Len(uint(packetSize))))
					copy(grow, buf[:bufN])
					buf = grow
				}
				break
			}

			ok := c.inbound(buf[offset], buf[offset+1+sizeN:offset+packetSize])
			if !ok {
				c.conn.Close()
				return
			}

			offset += packetSize
		}

		if offset > 0 {
			// move to beginning of buffer
			copy(buf, buf[offset:bufN])
			bufN -= offset
		}

		switch err {
		case nil:
			break

		case io.EOF:
			return

		default:
			if e, ok := err.(net.Error); !ok || !e.Temporary() {
				log.Print("mqtt: closing connection on read error: ", err)
				c.conn.Close()
				return
			}

			delay := c.attrs.RetryDelay
			log.Print("mqtt: read retry on temporary network error in ", delay, ": ", err)
			time.Sleep(delay)
		}
	}
}

func (c *Client) inbound(a byte, p []byte) (ok bool) {
	switch packetType := a >> 4; packetType {
	case pubReq:
		// parse packet
		i := uint(p[0])<<8 | uint(p[1])
		topic := string(p[2:i])
		id := uint(p[i])<<8 | uint(p[i+1])
		message := p[i+2:]

		switch QoS(a>>1) & 3 {
		case AtMostOnce:
			c.listener(topic, message)
			return

		case AtLeastOnce:
			c.writePacket.pubAck(id)

		case ExactlyOnce:
			c.writePacket.pubReceived(id)

		default:
			log.Print("mqtt: close on protocol violation: publish request with reserved QoS 3")
			c.conn.Close()
			return
		}

		err := c.storage.Persist(id, message)
		if err != nil {
			log.Print("mqtt: reception persistence malfuncion: ", err)
			return
		}
		if err := c.write(c.writePacket.buf); err != nil {
			log.Print("mqtt: submission publish reception failed on fatal network error: ", err)
			return
		}
		return

	case pubReceived, pubRelease, pubComplete, pubAck, unsubAck:
		if len(p) != 2 {
			log.Print("mqtt: close on protocol violation: received packet type ", packetType, " with remaining length ", len(p))
			c.conn.Close()
			return
		}
		id := uint(binary.BigEndian.Uint16(p))

		if packetType == pubReceived {
			if err := c.storage.Persist(id, nil); err != nil {
				log.Print("mqtt: reception persistence malfuncion: ", err)
				return
			}

			c.writePacket.pubComplete(id)
			if err := c.write(c.writePacket.buf); err != nil {
				log.Print("mqtt: submission publish complete failed on fatal network error: ", err)
			}
		} else {
			c.storage.Delete(id)
		}

	case subAck:
		if len(p) != 3 {
			log.Print("mqtt: close on protocol violation: remaining length not 3")
			return
		}

	case pong:
		if len(p) != 0 {
			log.Print("mqtt: ping response packet remaining length not 0")
		}
		c.pong <- struct{}{}
		ok = true

	case connReq, subReq, unsubReq, ping, disconn:
		log.Print("mqtt: close on protocol violation: client received packet type ", packetType)

	case connAck:
		log.Print("mqtt: close on protocol violation: redunant connection acknowledge")

	default:
		log.Print("mqtt: close on protocol violation: received reserved packet type ", packetType)
	}

	return
}

// Connect initiates the protocol over a transport layer such as *net.TCP or
// *tls.Conn.
func (c *Client) connect(f Connecter) error {
	var err error
	c.conn, err = f(c.attrs.WireTimeout)
	if err != nil {
		return err
	}

	c.conn.SetDeadline(time.Now().Add(c.attrs.WireTimeout))

	// launch handshake
	c.writePacket.connReq(&c.attrs)
	if err := c.write(c.writePacket.buf); err != nil {
		c.conn.Close()
		return err
	}

	var buf [4]byte
	n, err := c.conn.Read(buf[:])
	if err != nil {
		c.conn.Close()
		return err
	}

	for {
		if n > 0 && buf[0] != connAck<<4 {
			c.conn.Close()
			return fmt.Errorf("mqtt: received packet type %#x on connect—connection closed", buf[0]>>4)
		}
		if n > 1 && buf[1] != 2 {
			c.conn.Close()
			return fmt.Errorf("mqtt: connect acknowledge remaining length is %d instead of 2—connection closed", buf[1])
		}
		if n > 2 && buf[2] > 1 {
			c.conn.Close()
			return fmt.Errorf("mqtt: received reserved connect acknowledge flags %#x—connection closed", buf[2])
		}
		if n > 3 {
			break
		}

		more, err := c.conn.Read(buf[n:])
		if err != nil {
			c.conn.Close()
			return err
		}
		n += more
	}

	if code := connectReturn(buf[3]); code != accepted {
		c.conn.Close()
		return code
	}

	c.conn.SetDeadline(time.Time{}) // clear

	go c.readLoop()

	return nil
}

// Publish persists the message (for network submission). Error returns other
// than ErrTopicName, ErrMessageSize and ErrRequestLimit signal fatal Storage
// malfunction. Thus the actual publication is decoupled from the invokation.
//
// Deliver AtMostOnce causes message to be send the server, and that'll be the
// end of operation. Subscribers may or may not receive the message when subject
// to error. Use AtLeastOnce or ExactlyOne for more protection, at the cost of
// higher (performance) overhead.
//
// Multiple goroutines may invoke Publish simultaneously.
func (c *Client) Publish(topic string, message []byte, deliver QoS) error {
	id, err := c.packetIDs.reserve()
	if err != nil {
		return err
	}

	c.writePacket.pub(id, topic, message, deliver)

	return c.storage.Persist(id|localPacketIDFlag, c.writePacket.buf)
}

// PublishRetained acts like Publish, but causes the message to be stored on the
// server, so that they can be delivered to future subscribers.
func (c *Client) PublishRetained(topic string, message []byte, deliver QoS) error {
	id, err := c.packetIDs.reserve()
	if err != nil {
		return err
	}

	c.writePacket.pub(id, topic, message, deliver)
	c.writePacket.buf[0] |= retainFlag

	return c.storage.Persist(id|localPacketIDFlag, c.writePacket.buf)
}

// Subscribe requests a subscription for all topics that match the filter.
// The requested quality of service is a maximum for the server.
func (c *Client) Subscribe(topicFilter string, max QoS) error {
	id, err := c.packetIDs.reserve()
	if err != nil {
		return err
	}

	c.writePacket.subReq(id, topicFilter, max)
	if err := c.write(c.writePacket.buf); err != nil {
		return err
	}

	panic("TODO: await ack")
}

// Unsubscribe requests a Subscribe cancelation.
func (c *Client) Unsubscribe(topicFilter string) error {
	id, err := c.packetIDs.reserve()
	if err != nil {
		return err
	}

	c.writePacket.unsubReq(id, topicFilter)
	if err := c.write(c.writePacket.buf); err != nil {
		return err
	}

	panic("TODO: await ack")
}

// Ping makes a roundtrip to validate the connection.
func (c *Client) Ping() error {
	return c.write(pingPacket)
}

// Disconnect is a graceful termination, which also discards the Will.
// The underlying connection is closed.
func (c *Client) Disconnect() error {
	_, err := c.conn.Write(disconnPacket)

	closeErr := c.conn.Close()
	if err == nil {
		err = closeErr
	}

	return err
}
