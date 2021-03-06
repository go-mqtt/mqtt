/*
 * Copyright (c) 2013 IBM Corp.
 *
 * All rights reserved. This program and the accompanying materials
 * are made available under the terms of the Eclipse Public License v1.0
 * which accompanies this distribution, and is available at
 * http://www.eclipse.org/legal/epl-v10.html
 *
 * Contributors:
 *    Seth Hoenig
 *    Allan Stockdill-Mander
 *    Mike Robertson
 */

package mqtt

import (
	"golang.org/x/net/websocket"
	"crypto/tls"
	"errors"
	"gopkg.in/mqtt.v0/packets"
	"net"
	"net/url"
	"reflect"
	"time"
)

func openConnection(uri *url.URL, tlsc *tls.Config) (net.Conn, error) {
	switch uri.Scheme {
	case "ws":
		conn, err := websocket.Dial(uri.String(), "mqtt", "ws://localhost")
		if err != nil {
			return nil, err
		}
		conn.PayloadType = websocket.BinaryFrame
		return conn, err
	case "wss":
		config, _ := websocket.NewConfig(uri.String(), "ws://localhost")
		config.Protocol = []string{"mqtt"}
		config.TlsConfig = tlsc
		conn, err := websocket.DialConfig(config)
		if err != nil {
			return nil, err
		}
		conn.PayloadType = websocket.BinaryFrame
		return conn, err
	case "tcp":
		conn, err := net.Dial("tcp", uri.Host)
		if err != nil {
			return nil, err
		}
		return conn, nil
	case "ssl":
		fallthrough
	case "tls":
		fallthrough
	case "tcps":
		conn, err := tls.Dial("tcp", uri.Host, tlsc)
		if err != nil {
			return nil, err
		}
		return conn, nil
	}
	return nil, errors.New("Unknown protocol")
}

// actually read incoming messages off the wire
// send Message object into ibound channel
func incoming(c *Client) {
	defer c.workers.Done()
	var err error
	var cp packets.ControlPacket

	DEBUG.Println(NET, "incoming started")

	for {
		if cp, err = packets.ReadPacket(c.conn); err != nil {
			break
		}
		DEBUG.Println(NET, "Received Message")
		c.ibound <- cp
	}
	// We received an error on read.
	// If disconnect is in progress, swallow error and return
	select {
	case <-c.stop:
		DEBUG.Println(NET, "incoming stopped")
		return
		// Not trying to disconnect, send the error to the errors channel
	default:
		ERROR.Println(NET, "incoming stopped with error")
		c.errors <- err
		return
	}
}

// receive a Message object on obound, and then
// actually send outgoing message to the wire
func outgoing(c *Client) {
	defer c.workers.Done()
	DEBUG.Println(NET, "outgoing started")

	for {
		DEBUG.Println(NET, "outgoing waiting for an outbound message")
		select {
		case <-c.stop:
			DEBUG.Println(NET, "outgoing stopped")
			return
		case pub := <-c.obound:
			msg := pub.p.(*packets.PublishPacket)
			if msg.Qos != 0 && msg.MessageID == 0 {
				msg.MessageID = c.getID(pub.t)
				pub.t.(*PublishToken).messageID = msg.MessageID
			}
			//persist_obound(c.persist, msg)

			if c.options.WriteTimeout > 0 {
				c.conn.SetWriteDeadline(time.Now().Add(c.options.WriteTimeout))
			}

			if err := msg.Write(c.conn); err != nil {
				ERROR.Println(NET, "outgoing stopped with error")
				c.errors <- err
				return
			}

			if c.options.WriteTimeout > 0 {
				// If we successfully wrote, we don't want the timeout to happen during an idle period
				// so we reset it to infinite.
				c.conn.SetWriteDeadline(time.Time{})
			}

			if msg.Qos == 0 {
				pub.t.flowComplete()
			}

			c.lastContact.update()
			DEBUG.Println(NET, "obound wrote msg, id:", msg.MessageID)
		case msg := <-c.oboundP:
			switch msg.p.(type) {
			case *packets.SubscribePacket:
				msg.p.(*packets.SubscribePacket).MessageID = c.getID(msg.t)
			case *packets.UnsubscribePacket:
				msg.p.(*packets.UnsubscribePacket).MessageID = c.getID(msg.t)
			}
			DEBUG.Println(NET, "obound priority msg to write, type", reflect.TypeOf(msg.p))
			if err := msg.p.Write(c.conn); err != nil {
				ERROR.Println(NET, "outgoing stopped with error")
				c.errors <- err
				return
			}
			c.lastContact.update()
			switch msg.p.(type) {
			case *packets.DisconnectPacket:
				msg.t.(*DisconnectToken).flowComplete()
				c.conn.Close()
				DEBUG.Println(NET, "outbound wrote disconnect, stopping")
				return
			}
		}
	}
}

// receive Message objects on ibound
// store messages if necessary
// send replies on obound
// delete messages from store if necessary
func alllogic(c *Client) {

	DEBUG.Println(NET, "logic started")

	for {
		DEBUG.Println(NET, "logic waiting for msg on ibound")

		select {
		case msg := <-c.ibound:
			DEBUG.Println(NET, "logic got msg on ibound")
			//persist_ibound(c.persist, msg)
			switch msg.(type) {
			case *packets.PingrespPacket:
				DEBUG.Println(NET, "received pingresp")
				c.pingOutstanding = false
			case *packets.SubackPacket:
				sa := msg.(*packets.SubackPacket)
				DEBUG.Println(NET, "received suback, id:", sa.MessageID)
				token := c.getToken(sa.MessageID).(*SubscribeToken)
				DEBUG.Println(NET, "granted qoss", sa.GrantedQoss)
				for i, qos := range sa.GrantedQoss {
					token.subResult[token.subs[i]] = qos
				}
				token.flowComplete()
				go c.freeID(sa.MessageID)
			case *packets.UnsubackPacket:
				ua := msg.(*packets.UnsubackPacket)
				DEBUG.Println(NET, "received unsuback, id:", ua.MessageID)
				token := c.getToken(ua.MessageID).(*UnsubscribeToken)
				token.flowComplete()
				go c.freeID(ua.MessageID)
			case *packets.PublishPacket:
				pp := msg.(*packets.PublishPacket)
				DEBUG.Println(NET, "received publish, msgId:", pp.MessageID)
				DEBUG.Println(NET, "putting msg on onPubChan")
				switch pp.Qos {
				case 2:
					c.incomingPubChan <- pp
					DEBUG.Println(NET, "done putting msg on incomingPubChan")
					pr := packets.NewControlPacket(packets.Pubrec).(*packets.PubrecPacket)
					pr.MessageID = pp.MessageID
					DEBUG.Println(NET, "putting pubrec msg on obound")
					c.oboundP <- &PacketAndToken{p: pr, t: nil}
					DEBUG.Println(NET, "done putting pubrec msg on obound")
				case 1:
					c.incomingPubChan <- pp
					DEBUG.Println(NET, "done putting msg on incomingPubChan")
					pa := packets.NewControlPacket(packets.Puback).(*packets.PubackPacket)
					pa.MessageID = pp.MessageID
					DEBUG.Println(NET, "putting puback msg on obound")
					c.oboundP <- &PacketAndToken{p: pa, t: nil}
					DEBUG.Println(NET, "done putting puback msg on obound")
				case 0:
					select {
					case c.incomingPubChan <- pp:
						DEBUG.Println(NET, "done putting msg on incomingPubChan")
					case err, ok := <-c.errors:
						DEBUG.Println(NET, "error while putting msg on pubChanZero")
						// We are unblocked, but need to put the error back on so the outer
						// select can handle it appropriately.
						if ok {
							go func(errVal error, errChan chan error) {
								errChan <- errVal
							}(err, c.errors)
						}
					}
				}
			case *packets.PubackPacket:
				pa := msg.(*packets.PubackPacket)
				DEBUG.Println(NET, "received puback, id:", pa.MessageID)
				// c.receipts.get(msg.MsgId()) <- Receipt{}
				// c.receipts.end(msg.MsgId())
				c.getToken(pa.MessageID).flowComplete()
				c.freeID(pa.MessageID)
			case *packets.PubrecPacket:
				prec := msg.(*packets.PubrecPacket)
				DEBUG.Println(NET, "received pubrec, id:", prec.MessageID)
				prel := packets.NewControlPacket(packets.Pubrel).(*packets.PubrelPacket)
				prel.MessageID = prec.MessageID
				select {
				case c.oboundP <- &PacketAndToken{p: prel, t: nil}:
				case <-time.After(time.Second):
				}
			case *packets.PubrelPacket:
				pr := msg.(*packets.PubrelPacket)
				DEBUG.Println(NET, "received pubrel, id:", pr.MessageID)
				pc := packets.NewControlPacket(packets.Pubcomp).(*packets.PubcompPacket)
				pc.MessageID = pr.MessageID
				select {
				case c.oboundP <- &PacketAndToken{p: pc, t: nil}:
				case <-time.After(time.Second):
				}
			case *packets.PubcompPacket:
				pc := msg.(*packets.PubcompPacket)
				DEBUG.Println(NET, "received pubcomp, id:", pc.MessageID)
				c.getToken(pc.MessageID).flowComplete()
				c.freeID(pc.MessageID)
			}
		case <-c.stop:
			WARN.Println(NET, "logic stopped")
			return
		case err := <-c.errors:
			ERROR.Println(NET, "logic got error")
			c.internalConnLost(err)
			return
		}
		c.lastContact.update()
	}
}
