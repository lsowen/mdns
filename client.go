package mdns

import (
	"code.google.com/p/go.net/ipv4"
	"code.google.com/p/go.net/ipv6"
	"fmt"
	"github.com/miekg/dns"
	"log"
	"net"
	"sync"
	"time"
	//"strings"
	"bytes"
)

// QueryParam is used to customize how a Lookup is performed
type QueryParam struct {
	RecordName   string               // RecordName to lookup
	Timeout   time.Duration        // Lookup timeout, default 1 second
	Interface *net.Interface       // Multicast interface to use
	QueryType uint16               // dns Type Constant to use
	Entries   chan<- dns.RR // Entries Channel
}

type operationType string
const (
	SUBSCRIBE operationType = "SUBSCRIBE"
	UNSUBSCRIBE operationType = "UNSUBSCRIBE"
	CLOSE operationType = "CLOSE"
)

type subscriptionMessage struct {
	Operation operationType
	Channel chan<- dns.RR
}

// DefaultParams is used to return a default set of QueryParam's
func DefaultParams(recordName string) *QueryParam {
	return &QueryParam{
		RecordName: recordName,
		QueryType: dns.TypeANY,
		Timeout: time.Second,
		Entries: make(chan dns.RR),
	}
}

func EscapeName(name string) string {
	var outputBuffer bytes.Buffer

	previousIsSlash := false
	for _, c := range name {
		if c == ' ' && !previousIsSlash {
			outputBuffer.WriteRune('\\')
		}

		outputBuffer.WriteRune(c)

		if c == '\\' {
			previousIsSlash = true
		} else {
			previousIsSlash = false
		}
	}

	return outputBuffer.String()
}

// Query looks up a given recordName, in a domain, waiting at most
// for a timeout before finishing the query. The results are streamed
// to a channel. Sends will not block, so clients should make sure to
// either read or buffer.
func Query(params *QueryParam) error {
	// Create a new client
	client, err := NewClient()
	if err != nil {
		return err
	}
	defer client.Close()

	// Set the multicast interface
	if params.Interface != nil {
		if err := client.SetInterface(params.Interface); err != nil {
			return err
		}
	}

	if params.Timeout == 0 {
		params.Timeout = time.Second
	}

	// Run the query
	return client.Query(params)
}

// Lookup is the same as Query, however it uses all the default parameters
func Lookup(recordName string, entries chan<- dns.RR) error {
	params := DefaultParams(recordName)
	params.Entries = entries
	return Query(params)
}

// Client provides a query interface that can be used to
// search for recordName providers using mDNS
type client struct {
	ipv4List *net.UDPConn
	ipv6List *net.UDPConn

	closed    bool
	closedCh  chan struct{}
	closeLock sync.Mutex

	msgChan chan *dns.Msg
	subscriptionChannel chan subscriptionMessage
	//subscriberChans []chan dns.RR
}

// NewClient creates a new mdns Client that can be used to query
// for records
func NewClient() (*client, error) {
	// Create a IPv4 listener
	ipv4, err := net.ListenMulticastUDP("udp4", nil, ipv4Addr)
	if err != nil {
		log.Printf("[ERR] mdns: Failed to bind to udp4 port: %v", err)
	}
	ipv6, err := net.ListenMulticastUDP("udp6", nil, ipv6Addr)
	if err != nil {
		log.Printf("[ERR] mdns: Failed to bind to udp6 port: %v", err)
	}

	if ipv4 == nil && ipv6 == nil {
		return nil, fmt.Errorf("Failed to bind to any udp port!")
	}

	c := &client{
		ipv4List: ipv4,
		ipv6List: ipv6,
		closedCh: make(chan struct{}),
		msgChan: make(chan *dns.Msg, 32),
		subscriptionChannel: make(chan subscriptionMessage, 32),
	}
	go c.broadcastAll()
	return c, nil
}

func (c *client) broadcastAll() {
	go c.recv(c.ipv4List, c.msgChan)
	go c.recv(c.ipv6List, c.msgChan)

	entryCache := make([]dns.RR, 0)
	subscriberChans := make([]chan<- dns.RR, 0)

	for {
		select {
		case msg := <- c.msgChan:
			for _, answer := range msg.Answer {
				entryCache = append(entryCache, answer)
				for _, channel := range subscriberChans {
					channel <- answer
				}
			}
			for _, answer := range msg.Extra {
				entryCache = append(entryCache, answer)
				for _, channel := range subscriberChans {
					channel <- answer
				}
			}

		case msg := <- c.subscriptionChannel:
			switch msg.Operation {
			case SUBSCRIBE:
				//fmt.Println("Subscribe")
				subscriberChans = append(subscriberChans, msg.Channel)
				for _, entry := range entryCache {
					msg.Channel <- entry
				}
				break
			case UNSUBSCRIBE:
				//fmt.Println("Unsubscribe")
				for idx, channel := range subscriberChans {
					if channel == msg.Channel {
						subscriberChans = append(subscriberChans[:idx],subscriberChans[idx + 1:]...)
						close(channel)
						break
					}
				}
				break
			case CLOSE:
				return
			}
		}
	}
}

// Close is used to cleanup the client
func (c *client) Close() error {
	c.closeLock.Lock()
	defer c.closeLock.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true
	close(c.closedCh)

	if c.ipv4List != nil {
		c.ipv4List.Close()
	}
	if c.ipv6List != nil {
		c.ipv6List.Close()
	}
	return nil
}

// setInterface is used to set the query interface, uses system
// default if not provided
func (c *client) SetInterface(iface *net.Interface) error {
	p := ipv4.NewPacketConn(c.ipv4List)
	if err := p.SetMulticastInterface(iface); err != nil {
		return err
	}
	p2 := ipv6.NewPacketConn(c.ipv6List)
	if err := p2.SetMulticastInterface(iface); err != nil {
		return err
	}
	return nil
}

func (c *client) Subscribe() chan dns.RR {
	channel := make(chan dns.RR)
	c.subscriptionChannel <- subscriptionMessage{
		Operation: SUBSCRIBE,
		Channel: channel,
	}
	return channel
}

func (c *client) Unsubscribe(channel chan dns.RR) {
	c.subscriptionChannel <- subscriptionMessage{
		Operation: UNSUBSCRIBE,
		Channel: channel,
	}
}

// query is used to perform a lookup and stream results
func (c *client) Query(params *QueryParam) error {
	// Create the recordName name
	recordName := EscapeName(params.RecordName)
	answerChan := c.Subscribe()

	go func() {
		for answer := range answerChan {
			if (answer.Header().Name == recordName) && (params.QueryType == dns.TypeANY || answer.Header().Rrtype == params.QueryType) {
				params.Entries <- answer
			}
		}
	}()

	// Send the query
	m := new(dns.Msg)
	m.SetQuestion(recordName, params.QueryType)
	if err := c.sendQuery(m); err != nil {
		return nil
	}

	select {
	case <- time.After(params.Timeout):
		c.Unsubscribe(answerChan)
		return nil
	}
}

// sendQuery is used to multicast a query out
func (c *client) sendQuery(q *dns.Msg) error {
	buf, err := q.Pack()
	if err != nil {
		return err
	}
	if c.ipv4List != nil {
		c.ipv4List.WriteTo(buf, ipv4Addr)
	}
	if c.ipv6List != nil {
		c.ipv6List.WriteTo(buf, ipv6Addr)
	}
	return nil
}

// recv is used to receive until we get a shutdown
func (c *client) recv(l *net.UDPConn, msgCh chan *dns.Msg) {
	if l == nil {
		return
	}
	buf := make([]byte, 65536)
	for !c.closed {
		n, err := l.Read(buf)
		if err != nil {
			continue
		}
		msg := new(dns.Msg)
		if err := msg.Unpack(buf[:n]); err != nil {
			log.Printf("[ERR] mdns: Failed to unpack packet: %v", err)
			continue
		}
		select {
		case msgCh <- msg:
		case <-c.closedCh:
			return
		}
	}
}
