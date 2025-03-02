// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package udp_test

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"math/rand"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/time/rate"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/checker"
	"gvisor.dev/gvisor/pkg/tcpip/faketime"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/loopback"
	"gvisor.dev/gvisor/pkg/tcpip/link/sniffer"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/testutil"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Addresses and ports used for testing. It is recommended that tests stick to
// using these addresses as it allows using the testFlow helper.
// Naming rules: 'stack*'' denotes local addresses and ports, while 'test*'
// represents the remote endpoint.
const (
	v4MappedAddrPrefix    = "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xff\xff"
	stackV6Addr           = "\x0a\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01"
	testV6Addr            = "\x0a\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02"
	stackV4MappedAddr     = v4MappedAddrPrefix + stackAddr
	testV4MappedAddr      = v4MappedAddrPrefix + testAddr
	multicastV4MappedAddr = v4MappedAddrPrefix + multicastAddr
	broadcastV4MappedAddr = v4MappedAddrPrefix + broadcastAddr
	v4MappedWildcardAddr  = v4MappedAddrPrefix + "\x00\x00\x00\x00"

	stackAddr       = "\x0a\x00\x00\x01"
	stackPort       = 1234
	testAddr        = "\x0a\x00\x00\x02"
	testPort        = 4096
	invalidPort     = 8192
	multicastAddr   = "\xe8\x2b\xd3\xea"
	multicastV6Addr = "\xff\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"
	broadcastAddr   = header.IPv4Broadcast
	testTOS         = 0x80

	// defaultMTU is the MTU, in bytes, used throughout the tests, except
	// where another value is explicitly used. It is chosen to match the MTU
	// of loopback interfaces on linux systems.
	defaultMTU = 65536
)

// header4Tuple stores the 4-tuple {src-IP, src-port, dst-IP, dst-port} used in
// a packet header. These values are used to populate a header or verify one.
// Note that because they are used in packet headers, the addresses are never in
// a V4-mapped format.
type header4Tuple struct {
	srcAddr tcpip.FullAddress
	dstAddr tcpip.FullAddress
}

// testFlow implements a helper type used for sending and receiving test
// packets. A given test flow value defines 1) the socket endpoint used for the
// test and 2) the type of packet send or received on the endpoint. E.g., a
// multicastV6Only flow is a V6 multicast packet passing through a V6-only
// endpoint. The type provides helper methods to characterize the flow (e.g.,
// isV4) as well as return a proper header4Tuple for it.
type testFlow int

const (
	unicastV4         testFlow = iota // V4 unicast on a V4 socket
	unicastV4in6                      // V4-mapped unicast on a V6-dual socket
	unicastV6                         // V6 unicast on a V6 socket
	unicastV6Only                     // V6 unicast on a V6-only socket
	multicastV4                       // V4 multicast on a V4 socket
	multicastV4in6                    // V4-mapped multicast on a V6-dual socket
	multicastV6                       // V6 multicast on a V6 socket
	multicastV6Only                   // V6 multicast on a V6-only socket
	broadcast                         // V4 broadcast on a V4 socket
	broadcastIn6                      // V4-mapped broadcast on a V6-dual socket
	reverseMulticast4                 // V4 multicast src. Must fail.
	reverseMulticast6                 // V6 multicast src. Must fail.
)

func (flow testFlow) String() string {
	switch flow {
	case unicastV4:
		return "unicastV4"
	case unicastV6:
		return "unicastV6"
	case unicastV6Only:
		return "unicastV6Only"
	case unicastV4in6:
		return "unicastV4in6"
	case multicastV4:
		return "multicastV4"
	case multicastV6:
		return "multicastV6"
	case multicastV6Only:
		return "multicastV6Only"
	case multicastV4in6:
		return "multicastV4in6"
	case broadcast:
		return "broadcast"
	case broadcastIn6:
		return "broadcastIn6"
	case reverseMulticast4:
		return "reverseMulticast4"
	case reverseMulticast6:
		return "reverseMulticast6"
	default:
		return "unknown"
	}
}

// packetDirection explains if a flow is incoming (read) or outgoing (write).
type packetDirection int

const (
	incoming packetDirection = iota
	outgoing
)

// header4Tuple returns the header4Tuple for the given flow and direction. Note
// that the tuple contains no mapped addresses as those only exist at the socket
// level but not at the packet header level.
func (flow testFlow) header4Tuple(d packetDirection) header4Tuple {
	var h header4Tuple
	if flow.isV4() {
		if d == outgoing {
			h = header4Tuple{
				srcAddr: tcpip.FullAddress{Addr: stackAddr, Port: stackPort},
				dstAddr: tcpip.FullAddress{Addr: testAddr, Port: testPort},
			}
		} else {
			h = header4Tuple{
				srcAddr: tcpip.FullAddress{Addr: testAddr, Port: testPort},
				dstAddr: tcpip.FullAddress{Addr: stackAddr, Port: stackPort},
			}
		}
		if flow.isMulticast() {
			h.dstAddr.Addr = multicastAddr
		} else if flow.isBroadcast() {
			h.dstAddr.Addr = broadcastAddr
		}
	} else { // IPv6
		if d == outgoing {
			h = header4Tuple{
				srcAddr: tcpip.FullAddress{Addr: stackV6Addr, Port: stackPort},
				dstAddr: tcpip.FullAddress{Addr: testV6Addr, Port: testPort},
			}
		} else {
			h = header4Tuple{
				srcAddr: tcpip.FullAddress{Addr: testV6Addr, Port: testPort},
				dstAddr: tcpip.FullAddress{Addr: stackV6Addr, Port: stackPort},
			}
		}
		if flow.isMulticast() {
			h.dstAddr.Addr = multicastV6Addr
		}
	}
	if flow.isReverseMulticast() {
		h.srcAddr.Addr = flow.getMcastAddr()
	}
	return h
}

func (flow testFlow) getMcastAddr() tcpip.Address {
	if flow.isV4() {
		return multicastAddr
	}
	return multicastV6Addr
}

// mapAddrIfApplicable converts the given V4 address into its V4-mapped version
// if it is applicable to the flow.
func (flow testFlow) mapAddrIfApplicable(v4Addr tcpip.Address) tcpip.Address {
	if flow.isMapped() {
		return v4MappedAddrPrefix + v4Addr
	}
	return v4Addr
}

// netProto returns the protocol number used for the network packet.
func (flow testFlow) netProto() tcpip.NetworkProtocolNumber {
	if flow.isV4() {
		return ipv4.ProtocolNumber
	}
	return ipv6.ProtocolNumber
}

// sockProto returns the protocol number used when creating the socket
// endpoint for this flow.
func (flow testFlow) sockProto() tcpip.NetworkProtocolNumber {
	switch flow {
	case unicastV4in6, unicastV6, unicastV6Only, multicastV4in6, multicastV6, multicastV6Only, broadcastIn6, reverseMulticast6:
		return ipv6.ProtocolNumber
	case unicastV4, multicastV4, broadcast, reverseMulticast4:
		return ipv4.ProtocolNumber
	default:
		panic(fmt.Sprintf("invalid testFlow given: %d", flow))
	}
}

func (flow testFlow) checkerFn() func(*testing.T, []byte, ...checker.NetworkChecker) {
	if flow.isV4() {
		return checker.IPv4
	}
	return checker.IPv6
}

func (flow testFlow) isV6() bool { return !flow.isV4() }
func (flow testFlow) isV4() bool {
	return flow.sockProto() == ipv4.ProtocolNumber || flow.isMapped()
}

func (flow testFlow) isV6Only() bool {
	switch flow {
	case unicastV6Only, multicastV6Only:
		return true
	case unicastV4, unicastV4in6, unicastV6, multicastV4, multicastV4in6, multicastV6, broadcast, broadcastIn6, reverseMulticast4, reverseMulticast6:
		return false
	default:
		panic(fmt.Sprintf("invalid testFlow given: %d", flow))
	}
}

func (flow testFlow) isMulticast() bool {
	switch flow {
	case multicastV4, multicastV4in6, multicastV6, multicastV6Only:
		return true
	case unicastV4, unicastV4in6, unicastV6, unicastV6Only, broadcast, broadcastIn6, reverseMulticast4, reverseMulticast6:
		return false
	default:
		panic(fmt.Sprintf("invalid testFlow given: %d", flow))
	}
}

func (flow testFlow) isBroadcast() bool {
	switch flow {
	case broadcast, broadcastIn6:
		return true
	case unicastV4, unicastV4in6, unicastV6, unicastV6Only, multicastV4, multicastV4in6, multicastV6, multicastV6Only, reverseMulticast4, reverseMulticast6:
		return false
	default:
		panic(fmt.Sprintf("invalid testFlow given: %d", flow))
	}
}

func (flow testFlow) isMapped() bool {
	switch flow {
	case unicastV4in6, multicastV4in6, broadcastIn6:
		return true
	case unicastV4, unicastV6, unicastV6Only, multicastV4, multicastV6, multicastV6Only, broadcast, reverseMulticast4, reverseMulticast6:
		return false
	default:
		panic(fmt.Sprintf("invalid testFlow given: %d", flow))
	}
}

func (flow testFlow) isReverseMulticast() bool {
	switch flow {
	case reverseMulticast4, reverseMulticast6:
		return true
	default:
		return false
	}
}

type testContext struct {
	t      *testing.T
	linkEP *channel.Endpoint
	s      *stack.Stack
	nicID  tcpip.NICID

	ep tcpip.Endpoint
	wq waiter.Queue
}

func newDualTestContext(t *testing.T, mtu uint32) *testContext {
	t.Helper()
	return newDualTestContextWithHandleLocal(t, mtu, true)
}

func newDualTestContextWithHandleLocal(t *testing.T, mtu uint32, handleLocal bool) *testContext {
	const nicID = 1

	t.Helper()

	options := stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{udp.NewProtocol, icmp.NewProtocol6, icmp.NewProtocol4},
		HandleLocal:        handleLocal,
		Clock:              &faketime.NullClock{},
	}
	s := stack.New(options)
	// Disable ICMP rate limiter because we're using Null clock, which never advances time and thus
	// never allows ICMP messages.
	s.SetICMPLimit(rate.Inf)
	ep := channel.New(256, mtu, "")
	wep := stack.LinkEndpoint(ep)

	if testing.Verbose() {
		wep = sniffer.New(ep)
	}
	if err := s.CreateNIC(nicID, wep); err != nil {
		t.Fatalf("CreateNIC(%d, _): %s", nicID, err)
	}

	protocolAddrV4 := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.Address(stackAddr).WithPrefix(),
	}
	if err := s.AddProtocolAddress(nicID, protocolAddrV4, stack.AddressProperties{}); err != nil {
		t.Fatalf("AddProtocolAddress(%d, %+v, {}): %s", nicID, protocolAddrV4, err)
	}

	protocolAddrV6 := tcpip.ProtocolAddress{
		Protocol:          ipv6.ProtocolNumber,
		AddressWithPrefix: tcpip.Address(stackV6Addr).WithPrefix(),
	}
	if err := s.AddProtocolAddress(nicID, protocolAddrV6, stack.AddressProperties{}); err != nil {
		t.Fatalf("AddProtocolAddress(%d, %+v, {}): %s", nicID, protocolAddrV6, err)
	}

	s.SetRouteTable([]tcpip.Route{
		{
			Destination: header.IPv4EmptySubnet,
			NIC:         nicID,
		},
		{
			Destination: header.IPv6EmptySubnet,
			NIC:         nicID,
		},
	})

	return &testContext{
		t:      t,
		s:      s,
		nicID:  nicID,
		linkEP: ep,
	}
}

func (c *testContext) cleanup() {
	if c.ep != nil {
		c.ep.Close()
	}
}

func (c *testContext) createEndpoint(proto tcpip.NetworkProtocolNumber) {
	c.t.Helper()

	var err tcpip.Error
	c.ep, err = c.s.NewEndpoint(udp.ProtocolNumber, proto, &c.wq)
	if err != nil {
		c.t.Fatal("NewEndpoint failed: ", err)
	}
}

func (c *testContext) createEndpointForFlow(flow testFlow) {
	c.t.Helper()

	c.createEndpoint(flow.sockProto())
	if flow.isV6Only() {
		c.ep.SocketOptions().SetV6Only(true)
	} else if flow.isBroadcast() {
		c.ep.SocketOptions().SetBroadcast(true)
	}
}

// getPacketAndVerify reads a packet from the link endpoint and verifies the
// header against expected values from the given test flow. In addition, it
// calls any extra checker functions provided.
func (c *testContext) getPacketAndVerify(flow testFlow, checkers ...checker.NetworkChecker) []byte {
	c.t.Helper()

	p, ok := c.linkEP.Read()
	if !ok {
		c.t.Fatalf("Packet wasn't written out")
		return nil
	}

	if p.Proto != flow.netProto() {
		c.t.Fatalf("Bad network protocol: got %v, wanted %v", p.Proto, flow.netProto())
	}

	if got, want := p.Pkt.TransportProtocolNumber, header.UDPProtocolNumber; got != want {
		c.t.Errorf("got p.Pkt.TransportProtocolNumber = %d, want = %d", got, want)
	}

	vv := buffer.NewVectorisedView(p.Pkt.Size(), p.Pkt.Views())
	b := vv.ToView()

	h := flow.header4Tuple(outgoing)
	checkers = append(
		checkers,
		checker.SrcAddr(h.srcAddr.Addr),
		checker.DstAddr(h.dstAddr.Addr),
		checker.UDP(checker.DstPort(h.dstAddr.Port)),
	)
	flow.checkerFn()(c.t, b, checkers...)
	return b
}

// injectPacket creates a packet of the given flow and with the given payload,
// and injects it into the link endpoint. If badChecksum is true, the packet has
// a bad checksum in the UDP header.
func (c *testContext) injectPacket(flow testFlow, payload []byte, badChecksum bool) {
	c.t.Helper()

	h := flow.header4Tuple(incoming)
	if flow.isV4() {
		buf := c.buildV4Packet(payload, &h)
		if badChecksum {
			// Invalidate the UDP header checksum field, taking care to avoid
			// overflow to zero, which would disable checksum validation.
			for u := header.UDP(buf[header.IPv4MinimumSize:]); ; {
				u.SetChecksum(u.Checksum() + 1)
				if u.Checksum() != 0 {
					break
				}
			}
		}
		c.linkEP.InjectInbound(ipv4.ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{
			Data: buf.ToVectorisedView(),
		}))
	} else {
		buf := c.buildV6Packet(payload, &h)
		if badChecksum {
			// Invalidate the UDP header checksum field (Unlike IPv4, zero is
			// a valid checksum value for IPv6 so no need to avoid it).
			u := header.UDP(buf[header.IPv6MinimumSize:])
			u.SetChecksum(u.Checksum() + 1)
		}
		c.linkEP.InjectInbound(ipv6.ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{
			Data: buf.ToVectorisedView(),
		}))
	}
}

// buildV6Packet creates a V6 test packet with the given payload and header
// values in a buffer.
func (c *testContext) buildV6Packet(payload []byte, h *header4Tuple) buffer.View {
	// Allocate a buffer for data and headers.
	buf := buffer.NewView(header.UDPMinimumSize + header.IPv6MinimumSize + len(payload))
	payloadStart := len(buf) - len(payload)
	copy(buf[payloadStart:], payload)

	// Initialize the IP header.
	ip := header.IPv6(buf)
	ip.Encode(&header.IPv6Fields{
		TrafficClass:      testTOS,
		PayloadLength:     uint16(header.UDPMinimumSize + len(payload)),
		TransportProtocol: udp.ProtocolNumber,
		HopLimit:          65,
		SrcAddr:           h.srcAddr.Addr,
		DstAddr:           h.dstAddr.Addr,
	})

	// Initialize the UDP header.
	u := header.UDP(buf[header.IPv6MinimumSize:])
	u.Encode(&header.UDPFields{
		SrcPort: h.srcAddr.Port,
		DstPort: h.dstAddr.Port,
		Length:  uint16(header.UDPMinimumSize + len(payload)),
	})

	// Calculate the UDP pseudo-header checksum.
	xsum := header.PseudoHeaderChecksum(udp.ProtocolNumber, h.srcAddr.Addr, h.dstAddr.Addr, uint16(len(u)))

	// Calculate the UDP checksum and set it.
	xsum = header.Checksum(payload, xsum)
	u.SetChecksum(^u.CalculateChecksum(xsum))

	return buf
}

// buildV4Packet creates a V4 test packet with the given payload and header
// values in a buffer.
func (c *testContext) buildV4Packet(payload []byte, h *header4Tuple) buffer.View {
	// Allocate a buffer for data and headers.
	buf := buffer.NewView(header.UDPMinimumSize + header.IPv4MinimumSize + len(payload))
	payloadStart := len(buf) - len(payload)
	copy(buf[payloadStart:], payload)

	// Initialize the IP header.
	ip := header.IPv4(buf)
	ip.Encode(&header.IPv4Fields{
		TOS:         testTOS,
		TotalLength: uint16(len(buf)),
		TTL:         65,
		Protocol:    uint8(udp.ProtocolNumber),
		SrcAddr:     h.srcAddr.Addr,
		DstAddr:     h.dstAddr.Addr,
	})
	ip.SetChecksum(^ip.CalculateChecksum())

	// Initialize the UDP header.
	u := header.UDP(buf[header.IPv4MinimumSize:])
	u.Encode(&header.UDPFields{
		SrcPort: h.srcAddr.Port,
		DstPort: h.dstAddr.Port,
		Length:  uint16(header.UDPMinimumSize + len(payload)),
	})

	// Calculate the UDP pseudo-header checksum.
	xsum := header.PseudoHeaderChecksum(udp.ProtocolNumber, h.srcAddr.Addr, h.dstAddr.Addr, uint16(len(u)))

	// Calculate the UDP checksum and set it.
	xsum = header.Checksum(payload, xsum)
	u.SetChecksum(^u.CalculateChecksum(xsum))

	return buf
}

func newPayload() []byte {
	return newMinPayload(30)
}

func newMinPayload(minSize int) []byte {
	b := make([]byte, minSize+rand.Intn(100))
	for i := range b {
		b[i] = byte(rand.Intn(256))
	}
	return b
}

func TestBindToDeviceOption(t *testing.T) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{udp.NewProtocol},
		Clock:              &faketime.NullClock{},
	})

	ep, err := s.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &waiter.Queue{})
	if err != nil {
		t.Fatalf("NewEndpoint failed; %s", err)
	}
	defer ep.Close()

	opts := stack.NICOptions{Name: "my_device"}
	if err := s.CreateNICWithOptions(321, loopback.New(), opts); err != nil {
		t.Errorf("CreateNICWithOptions(_, _, %+v) failed: %s", opts, err)
	}

	// nicIDPtr is used instead of taking the address of NICID literals, which is
	// a compiler error.
	nicIDPtr := func(s tcpip.NICID) *tcpip.NICID {
		return &s
	}

	testActions := []struct {
		name                 string
		setBindToDevice      *tcpip.NICID
		setBindToDeviceError tcpip.Error
		getBindToDevice      int32
	}{
		{"GetDefaultValue", nil, nil, 0},
		{"BindToNonExistent", nicIDPtr(999), &tcpip.ErrUnknownDevice{}, 0},
		{"BindToExistent", nicIDPtr(321), nil, 321},
		{"UnbindToDevice", nicIDPtr(0), nil, 0},
	}
	for _, testAction := range testActions {
		t.Run(testAction.name, func(t *testing.T) {
			if testAction.setBindToDevice != nil {
				bindToDevice := int32(*testAction.setBindToDevice)
				if gotErr, wantErr := ep.SocketOptions().SetBindToDevice(bindToDevice), testAction.setBindToDeviceError; gotErr != wantErr {
					t.Errorf("got SetSockOpt(&%T(%d)) = %s, want = %s", bindToDevice, bindToDevice, gotErr, wantErr)
				}
			}
			bindToDevice := ep.SocketOptions().GetBindToDevice()
			if bindToDevice != testAction.getBindToDevice {
				t.Errorf("got bindToDevice = %d, want = %d", bindToDevice, testAction.getBindToDevice)
			}
		})
	}
}

// testReadInternal sends a packet of the given test flow into the stack by
// injecting it into the link endpoint. It then attempts to read it from the
// UDP endpoint and depending on if this was expected to succeed verifies its
// correctness including any additional checker functions provided.
func testReadInternal(c *testContext, flow testFlow, packetShouldBeDropped, expectReadError bool, checkers ...checker.ControlMessagesChecker) {
	c.t.Helper()

	payload := newPayload()
	c.injectPacket(flow, payload, false)

	// Try to receive the data.
	we, ch := waiter.NewChannelEntry(nil)
	c.wq.EventRegister(&we, waiter.ReadableEvents)
	defer c.wq.EventUnregister(&we)

	// Take a snapshot of the stats to validate them at the end of the test.
	epstats := c.ep.Stats().(*tcpip.TransportEndpointStats).Clone()

	var buf bytes.Buffer
	res, err := c.ep.Read(&buf, tcpip.ReadOptions{NeedRemoteAddr: true})
	if _, ok := err.(*tcpip.ErrWouldBlock); ok {
		// Wait for data to become available.
		select {
		case <-ch:
			res, err = c.ep.Read(&buf, tcpip.ReadOptions{NeedRemoteAddr: true})

		default:
			if packetShouldBeDropped {
				return // expected to time out
			}
			c.t.Fatal("timed out waiting for data")
		}
	}

	if expectReadError && err != nil {
		c.checkEndpointReadStats(1, epstats, err)
		return
	}

	if err != nil {
		c.t.Fatal("Read failed:", err)
	}

	if packetShouldBeDropped {
		c.t.Fatalf("Read unexpectedly received data from %s", res.RemoteAddr.Addr)
	}

	// Check the read result.
	h := flow.header4Tuple(incoming)
	if diff := cmp.Diff(tcpip.ReadResult{
		Count:      buf.Len(),
		Total:      buf.Len(),
		RemoteAddr: tcpip.FullAddress{Addr: h.srcAddr.Addr},
	}, res, checker.IgnoreCmpPath(
		"ControlMessages", // ControlMessages will be checked later.
		"RemoteAddr.NIC",
		"RemoteAddr.Port",
	)); diff != "" {
		c.t.Fatalf("Read: unexpected result (-want +got):\n%s", diff)
	}

	// Check the payload.
	v := buf.Bytes()
	if !bytes.Equal(payload, v) {
		c.t.Fatalf("got payload = %x, want = %x", v, payload)
	}

	// Run any checkers against the ControlMessages.
	for _, f := range checkers {
		f(c.t, res.ControlMessages)
	}

	c.checkEndpointReadStats(1, epstats, err)
}

// testRead sends a packet of the given test flow into the stack by injecting it
// into the link endpoint. It then reads it from the UDP endpoint and verifies
// its correctness including any additional checker functions provided.
func testRead(c *testContext, flow testFlow, checkers ...checker.ControlMessagesChecker) {
	c.t.Helper()
	testReadInternal(c, flow, false /* packetShouldBeDropped */, false /* expectReadError */, checkers...)
}

// testFailingRead sends a packet of the given test flow into the stack by
// injecting it into the link endpoint. It then tries to read it from the UDP
// endpoint and expects this to fail.
func testFailingRead(c *testContext, flow testFlow, expectReadError bool) {
	c.t.Helper()
	testReadInternal(c, flow, true /* packetShouldBeDropped */, expectReadError)
}

func TestBindEphemeralPort(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)

	if err := c.ep.Bind(tcpip.FullAddress{}); err != nil {
		t.Fatalf("ep.Bind(...) failed: %s", err)
	}
}

func TestBindReservedPort(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)

	if err := c.ep.Connect(tcpip.FullAddress{Addr: testV6Addr, Port: testPort}); err != nil {
		c.t.Fatalf("Connect failed: %s", err)
	}

	addr, err := c.ep.GetLocalAddress()
	if err != nil {
		t.Fatalf("GetLocalAddress failed: %s", err)
	}

	// We can't bind the address reserved by the connected endpoint above.
	{
		ep, err := c.s.NewEndpoint(udp.ProtocolNumber, ipv6.ProtocolNumber, &c.wq)
		if err != nil {
			t.Fatalf("NewEndpoint failed: %s", err)
		}
		defer ep.Close()
		{
			err := ep.Bind(addr)
			if _, ok := err.(*tcpip.ErrPortInUse); !ok {
				t.Fatalf("got ep.Bind(...) = %s, want = %s", err, &tcpip.ErrPortInUse{})
			}
		}
	}

	func() {
		ep, err := c.s.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &c.wq)
		if err != nil {
			t.Fatalf("NewEndpoint failed: %s", err)
		}
		defer ep.Close()
		// We can't bind ipv4-any on the port reserved by the connected endpoint
		// above, since the endpoint is dual-stack.
		{
			err := ep.Bind(tcpip.FullAddress{Port: addr.Port})
			if _, ok := err.(*tcpip.ErrPortInUse); !ok {
				t.Fatalf("got ep.Bind(...) = %s, want = %s", err, &tcpip.ErrPortInUse{})
			}
		}
		// We can bind an ipv4 address on this port, though.
		if err := ep.Bind(tcpip.FullAddress{Addr: stackAddr, Port: addr.Port}); err != nil {
			t.Fatalf("ep.Bind(...) failed: %s", err)
		}
	}()

	// Once the connected endpoint releases its port reservation, we are able to
	// bind ipv4-any once again.
	c.ep.Close()
	func() {
		ep, err := c.s.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &c.wq)
		if err != nil {
			t.Fatalf("NewEndpoint failed: %s", err)
		}
		defer ep.Close()
		if err := ep.Bind(tcpip.FullAddress{Port: addr.Port}); err != nil {
			t.Fatalf("ep.Bind(...) failed: %s", err)
		}
	}()
}

func TestV4ReadOnV6(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpointForFlow(unicastV4in6)

	// Bind to wildcard.
	if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	// Test acceptance.
	testRead(c, unicastV4in6)
}

func TestV4ReadOnBoundToV4MappedWildcard(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpointForFlow(unicastV4in6)

	// Bind to v4 mapped wildcard.
	if err := c.ep.Bind(tcpip.FullAddress{Addr: v4MappedWildcardAddr, Port: stackPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	// Test acceptance.
	testRead(c, unicastV4in6)
}

func TestV4ReadOnBoundToV4Mapped(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpointForFlow(unicastV4in6)

	// Bind to local address.
	if err := c.ep.Bind(tcpip.FullAddress{Addr: stackV4MappedAddr, Port: stackPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	// Test acceptance.
	testRead(c, unicastV4in6)
}

func TestV6ReadOnV6(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpointForFlow(unicastV6)

	// Bind to wildcard.
	if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	// Test acceptance.
	testRead(c, unicastV6)
}

// TestV4ReadSelfSource checks that packets coming from a local IP address are
// correctly dropped when handleLocal is true and not otherwise.
func TestV4ReadSelfSource(t *testing.T) {
	for _, tt := range []struct {
		name              string
		handleLocal       bool
		wantErr           tcpip.Error
		wantInvalidSource uint64
	}{
		{"HandleLocal", false, nil, 0},
		{"NoHandleLocal", true, &tcpip.ErrWouldBlock{}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := newDualTestContextWithHandleLocal(t, defaultMTU, tt.handleLocal)
			defer c.cleanup()

			c.createEndpointForFlow(unicastV4)

			if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
				t.Fatalf("Bind failed: %s", err)
			}

			payload := newPayload()
			h := unicastV4.header4Tuple(incoming)
			h.srcAddr = h.dstAddr

			buf := c.buildV4Packet(payload, &h)
			c.linkEP.InjectInbound(ipv4.ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{
				Data: buf.ToVectorisedView(),
			}))

			if got := c.s.Stats().IP.InvalidSourceAddressesReceived.Value(); got != tt.wantInvalidSource {
				t.Errorf("c.s.Stats().IP.InvalidSourceAddressesReceived got %d, want %d", got, tt.wantInvalidSource)
			}

			if _, err := c.ep.Read(ioutil.Discard, tcpip.ReadOptions{}); err != tt.wantErr {
				t.Errorf("got c.ep.Read = %s, want = %s", err, tt.wantErr)
			}
		})
	}
}

func TestV4ReadOnV4(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpointForFlow(unicastV4)

	// Bind to wildcard.
	if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	// Test acceptance.
	testRead(c, unicastV4)
}

// TestReadOnBoundToMulticast checks that an endpoint can bind to a multicast
// address and receive data sent to that address.
func TestReadOnBoundToMulticast(t *testing.T) {
	// FIXME(b/128189410): multicastV4in6 currently doesn't work as
	// AddMembershipOption doesn't handle V4in6 addresses.
	for _, flow := range []testFlow{multicastV4, multicastV6, multicastV6Only} {
		t.Run(fmt.Sprintf("flow:%s", flow), func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpointForFlow(flow)

			// Bind to multicast address.
			mcastAddr := flow.mapAddrIfApplicable(flow.getMcastAddr())
			if err := c.ep.Bind(tcpip.FullAddress{Addr: mcastAddr, Port: stackPort}); err != nil {
				c.t.Fatal("Bind failed:", err)
			}

			// Join multicast group.
			ifoptSet := tcpip.AddMembershipOption{NIC: 1, MulticastAddr: mcastAddr}
			if err := c.ep.SetSockOpt(&ifoptSet); err != nil {
				c.t.Fatalf("SetSockOpt(&%#v): %s", ifoptSet, err)
			}

			// Check that we receive multicast packets but not unicast or broadcast
			// ones.
			testRead(c, flow)
			testFailingRead(c, broadcast, false /* expectReadError */)
			testFailingRead(c, unicastV4, false /* expectReadError */)
		})
	}
}

// TestV4ReadOnBoundToBroadcast checks that an endpoint can bind to a broadcast
// address and can receive only broadcast data.
func TestV4ReadOnBoundToBroadcast(t *testing.T) {
	for _, flow := range []testFlow{broadcast, broadcastIn6} {
		t.Run(fmt.Sprintf("flow:%s", flow), func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpointForFlow(flow)

			// Bind to broadcast address.
			bcastAddr := flow.mapAddrIfApplicable(broadcastAddr)
			if err := c.ep.Bind(tcpip.FullAddress{Addr: bcastAddr, Port: stackPort}); err != nil {
				c.t.Fatalf("Bind failed: %s", err)
			}

			// Check that we receive broadcast packets but not unicast ones.
			testRead(c, flow)
			testFailingRead(c, unicastV4, false /* expectReadError */)
		})
	}
}

// TestReadFromMulticast checks that an endpoint will NOT receive a packet
// that was sent with multicast SOURCE address.
func TestReadFromMulticast(t *testing.T) {
	for _, flow := range []testFlow{reverseMulticast4, reverseMulticast6} {
		t.Run(fmt.Sprintf("flow:%s", flow), func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpointForFlow(flow)

			if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
				t.Fatalf("Bind failed: %s", err)
			}
			testFailingRead(c, flow, false /* expectReadError */)
		})
	}
}

// TestV4ReadBroadcastOnBoundToWildcard checks that an endpoint can bind to ANY
// and receive broadcast and unicast data.
func TestV4ReadBroadcastOnBoundToWildcard(t *testing.T) {
	for _, flow := range []testFlow{broadcast, broadcastIn6} {
		t.Run(fmt.Sprintf("flow:%s", flow), func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpointForFlow(flow)

			// Bind to wildcard.
			if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
				c.t.Fatalf("Bind failed: %s (", err)
			}

			// Check that we receive both broadcast and unicast packets.
			testRead(c, flow)
			testRead(c, unicastV4)
		})
	}
}

// testFailingWrite sends a packet of the given test flow into the UDP endpoint
// and verifies it fails with the provided error code.
func testFailingWrite(c *testContext, flow testFlow, wantErr tcpip.Error) {
	c.t.Helper()
	// Take a snapshot of the stats to validate them at the end of the test.
	epstats := c.ep.Stats().(*tcpip.TransportEndpointStats).Clone()
	h := flow.header4Tuple(outgoing)
	writeDstAddr := flow.mapAddrIfApplicable(h.dstAddr.Addr)

	var r bytes.Reader
	r.Reset(newPayload())
	_, gotErr := c.ep.Write(&r, tcpip.WriteOptions{
		To: &tcpip.FullAddress{Addr: writeDstAddr, Port: h.dstAddr.Port},
	})
	c.checkEndpointWriteStats(1, epstats, gotErr)
	if gotErr != wantErr {
		c.t.Fatalf("Write returned unexpected error: got %v, want %v", gotErr, wantErr)
	}
}

// testWrite sends a packet of the given test flow from the UDP endpoint to the
// flow's destination address:port. It then receives it from the link endpoint
// and verifies its correctness including any additional checker functions
// provided.
func testWrite(c *testContext, flow testFlow, checkers ...checker.NetworkChecker) uint16 {
	c.t.Helper()
	return testWriteAndVerifyInternal(c, flow, true, checkers...)
}

// testWriteWithoutDestination sends a packet of the given test flow from the
// UDP endpoint without giving a destination address:port. It then receives it
// from the link endpoint and verifies its correctness including any additional
// checker functions provided.
func testWriteWithoutDestination(c *testContext, flow testFlow, checkers ...checker.NetworkChecker) uint16 {
	c.t.Helper()
	return testWriteAndVerifyInternal(c, flow, false, checkers...)
}

func testWriteNoVerify(c *testContext, flow testFlow, setDest bool) buffer.View {
	c.t.Helper()
	// Take a snapshot of the stats to validate them at the end of the test.
	epstats := c.ep.Stats().(*tcpip.TransportEndpointStats).Clone()

	writeOpts := tcpip.WriteOptions{}
	if setDest {
		h := flow.header4Tuple(outgoing)
		writeDstAddr := flow.mapAddrIfApplicable(h.dstAddr.Addr)
		writeOpts = tcpip.WriteOptions{
			To: &tcpip.FullAddress{Addr: writeDstAddr, Port: h.dstAddr.Port},
		}
	}
	var r bytes.Reader
	payload := newPayload()
	r.Reset(payload)
	n, err := c.ep.Write(&r, writeOpts)
	if err != nil {
		c.t.Fatalf("Write failed: %s", err)
	}
	if n != int64(len(payload)) {
		c.t.Fatalf("Bad number of bytes written: got %v, want %v", n, len(payload))
	}
	c.checkEndpointWriteStats(1, epstats, err)
	return payload
}

func testWriteAndVerifyInternal(c *testContext, flow testFlow, setDest bool, checkers ...checker.NetworkChecker) uint16 {
	c.t.Helper()
	payload := testWriteNoVerify(c, flow, setDest)
	// Received the packet and check the payload.
	b := c.getPacketAndVerify(flow, checkers...)
	var udpH header.UDP
	if flow.isV4() {
		udpH = header.IPv4(b).Payload()
	} else {
		udpH = header.IPv6(b).Payload()
	}
	if !bytes.Equal(payload, udpH.Payload()) {
		c.t.Fatalf("Bad payload: got %x, want %x", udpH.Payload(), payload)
	}

	return udpH.SourcePort()
}

func testDualWrite(c *testContext) uint16 {
	c.t.Helper()

	v4Port := testWrite(c, unicastV4in6)
	v6Port := testWrite(c, unicastV6)
	if v4Port != v6Port {
		c.t.Fatalf("expected v4 and v6 ports to be equal: got v4Port = %d, v6Port = %d", v4Port, v6Port)
	}

	return v4Port
}

func TestDualWriteUnbound(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)

	testDualWrite(c)
}

func TestDualWriteBoundToWildcard(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)

	// Bind to wildcard.
	if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	p := testDualWrite(c)
	if p != stackPort {
		c.t.Fatalf("Bad port: got %v, want %v", p, stackPort)
	}
}

func TestDualWriteConnectedToV6(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)

	// Connect to v6 address.
	if err := c.ep.Connect(tcpip.FullAddress{Addr: testV6Addr, Port: testPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	testWrite(c, unicastV6)

	// Write to V4 mapped address.
	testFailingWrite(c, unicastV4in6, &tcpip.ErrNetworkUnreachable{})
	const want = 1
	if got := c.ep.Stats().(*tcpip.TransportEndpointStats).SendErrors.NoRoute.Value(); got != want {
		c.t.Fatalf("Endpoint stat not updated. got %d want %d", got, want)
	}
}

func TestDualWriteConnectedToV4Mapped(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)

	// Connect to v4 mapped address.
	if err := c.ep.Connect(tcpip.FullAddress{Addr: testV4MappedAddr, Port: testPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	testWrite(c, unicastV4in6)

	// Write to v6 address.
	testFailingWrite(c, unicastV6, &tcpip.ErrInvalidEndpointState{})
}

func TestV4WriteOnV6Only(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpointForFlow(unicastV6Only)

	// Write to V4 mapped address.
	testFailingWrite(c, unicastV4in6, &tcpip.ErrNoRoute{})
}

func TestV6WriteOnBoundToV4Mapped(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)

	// Bind to v4 mapped address.
	if err := c.ep.Bind(tcpip.FullAddress{Addr: stackV4MappedAddr, Port: stackPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	// Write to v6 address.
	testFailingWrite(c, unicastV6, &tcpip.ErrInvalidEndpointState{})
}

func TestV6WriteOnConnected(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)

	// Connect to v6 address.
	if err := c.ep.Connect(tcpip.FullAddress{Addr: testV6Addr, Port: testPort}); err != nil {
		c.t.Fatalf("Connect failed: %s", err)
	}

	testWriteWithoutDestination(c, unicastV6)
}

func TestV4WriteOnConnected(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)

	// Connect to v4 mapped address.
	if err := c.ep.Connect(tcpip.FullAddress{Addr: testV4MappedAddr, Port: testPort}); err != nil {
		c.t.Fatalf("Connect failed: %s", err)
	}

	testWriteWithoutDestination(c, unicastV4)
}

func TestWriteOnConnectedInvalidPort(t *testing.T) {
	protocols := map[string]tcpip.NetworkProtocolNumber{
		"ipv4": ipv4.ProtocolNumber,
		"ipv6": ipv6.ProtocolNumber,
	}
	for name, pn := range protocols {
		t.Run(name, func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpoint(pn)
			if err := c.ep.Connect(tcpip.FullAddress{Addr: stackAddr, Port: invalidPort}); err != nil {
				c.t.Fatalf("Connect failed: %s", err)
			}
			writeOpts := tcpip.WriteOptions{
				To: &tcpip.FullAddress{Addr: stackAddr, Port: invalidPort},
			}
			var r bytes.Reader
			payload := newPayload()
			r.Reset(payload)
			n, err := c.ep.Write(&r, writeOpts)
			if err != nil {
				c.t.Fatalf("c.ep.Write(...) = %s, want nil", err)
			}
			if got, want := n, int64(len(payload)); got != want {
				c.t.Fatalf("c.ep.Write(...) wrote %d bytes, want %d bytes", got, want)
			}

			{
				err := c.ep.LastError()
				if _, ok := err.(*tcpip.ErrConnectionRefused); !ok {
					c.t.Fatalf("expected c.ep.LastError() == ErrConnectionRefused, got: %+v", err)
				}
			}
		})
	}
}

// TestWriteOnBoundToV4Multicast checks that we can send packets out of a socket
// that is bound to a V4 multicast address.
func TestWriteOnBoundToV4Multicast(t *testing.T) {
	for _, flow := range []testFlow{unicastV4, multicastV4, broadcast} {
		t.Run(fmt.Sprintf("%s", flow), func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpointForFlow(flow)

			// Bind to V4 mcast address.
			if err := c.ep.Bind(tcpip.FullAddress{Addr: multicastAddr, Port: stackPort}); err != nil {
				c.t.Fatal("Bind failed:", err)
			}

			testWrite(c, flow)
		})
	}
}

// TestWriteOnBoundToV4MappedMulticast checks that we can send packets out of a
// socket that is bound to a V4-mapped multicast address.
func TestWriteOnBoundToV4MappedMulticast(t *testing.T) {
	for _, flow := range []testFlow{unicastV4in6, multicastV4in6, broadcastIn6} {
		t.Run(fmt.Sprintf("%s", flow), func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpointForFlow(flow)

			// Bind to V4Mapped mcast address.
			if err := c.ep.Bind(tcpip.FullAddress{Addr: multicastV4MappedAddr, Port: stackPort}); err != nil {
				c.t.Fatalf("Bind failed: %s", err)
			}

			testWrite(c, flow)
		})
	}
}

// TestWriteOnBoundToV6Multicast checks that we can send packets out of a
// socket that is bound to a V6 multicast address.
func TestWriteOnBoundToV6Multicast(t *testing.T) {
	for _, flow := range []testFlow{unicastV6, multicastV6} {
		t.Run(fmt.Sprintf("%s", flow), func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpointForFlow(flow)

			// Bind to V6 mcast address.
			if err := c.ep.Bind(tcpip.FullAddress{Addr: multicastV6Addr, Port: stackPort}); err != nil {
				c.t.Fatalf("Bind failed: %s", err)
			}

			testWrite(c, flow)
		})
	}
}

// TestWriteOnBoundToV6Multicast checks that we can send packets out of a
// V6-only socket that is bound to a V6 multicast address.
func TestWriteOnBoundToV6OnlyMulticast(t *testing.T) {
	for _, flow := range []testFlow{unicastV6Only, multicastV6Only} {
		t.Run(fmt.Sprintf("%s", flow), func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpointForFlow(flow)

			// Bind to V6 mcast address.
			if err := c.ep.Bind(tcpip.FullAddress{Addr: multicastV6Addr, Port: stackPort}); err != nil {
				c.t.Fatalf("Bind failed: %s", err)
			}

			testWrite(c, flow)
		})
	}
}

// TestWriteOnBoundToBroadcast checks that we can send packets out of a
// socket that is bound to the broadcast address.
func TestWriteOnBoundToBroadcast(t *testing.T) {
	for _, flow := range []testFlow{unicastV4, multicastV4, broadcast} {
		t.Run(fmt.Sprintf("%s", flow), func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpointForFlow(flow)

			// Bind to V4 broadcast address.
			if err := c.ep.Bind(tcpip.FullAddress{Addr: broadcastAddr, Port: stackPort}); err != nil {
				c.t.Fatal("Bind failed:", err)
			}

			testWrite(c, flow)
		})
	}
}

// TestWriteOnBoundToV4MappedBroadcast checks that we can send packets out of a
// socket that is bound to the V4-mapped broadcast address.
func TestWriteOnBoundToV4MappedBroadcast(t *testing.T) {
	for _, flow := range []testFlow{unicastV4in6, multicastV4in6, broadcastIn6} {
		t.Run(fmt.Sprintf("%s", flow), func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpointForFlow(flow)

			// Bind to V4Mapped mcast address.
			if err := c.ep.Bind(tcpip.FullAddress{Addr: broadcastV4MappedAddr, Port: stackPort}); err != nil {
				c.t.Fatalf("Bind failed: %s", err)
			}

			testWrite(c, flow)
		})
	}
}

func TestReadIncrementsPacketsReceived(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	// Create IPv4 UDP endpoint
	c.createEndpoint(ipv6.ProtocolNumber)

	// Bind to wildcard.
	if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	testRead(c, unicastV4)

	var want uint64 = 1
	if got := c.s.Stats().UDP.PacketsReceived.Value(); got != want {
		c.t.Fatalf("Read did not increment PacketsReceived: got %v, want %v", got, want)
	}
}

func TestReadIPPacketInfo(t *testing.T) {
	tests := []struct {
		name    string
		proto   tcpip.NetworkProtocolNumber
		flow    testFlow
		checker func(tcpip.NICID) checker.ControlMessagesChecker
	}{
		{
			name:  "IPv4 unicast",
			proto: header.IPv4ProtocolNumber,
			flow:  unicastV4,
			checker: func(id tcpip.NICID) checker.ControlMessagesChecker {
				return checker.ReceiveIPPacketInfo(tcpip.IPPacketInfo{
					NIC:             id,
					LocalAddr:       stackAddr,
					DestinationAddr: stackAddr,
				})
			},
		},
		{
			name:  "IPv4 multicast",
			proto: header.IPv4ProtocolNumber,
			flow:  multicastV4,
			checker: func(id tcpip.NICID) checker.ControlMessagesChecker {
				return checker.ReceiveIPPacketInfo(tcpip.IPPacketInfo{
					NIC: id,
					// TODO(gvisor.dev/issue/3556): Check for a unicast address.
					LocalAddr:       multicastAddr,
					DestinationAddr: multicastAddr,
				})
			},
		},
		{
			name:  "IPv4 broadcast",
			proto: header.IPv4ProtocolNumber,
			flow:  broadcast,
			checker: func(id tcpip.NICID) checker.ControlMessagesChecker {
				return checker.ReceiveIPPacketInfo(tcpip.IPPacketInfo{
					NIC: id,
					// TODO(gvisor.dev/issue/3556): Check for a unicast address.
					LocalAddr:       broadcastAddr,
					DestinationAddr: broadcastAddr,
				})
			},
		},
		{
			name:  "IPv6 unicast",
			proto: header.IPv6ProtocolNumber,
			flow:  unicastV6,
			checker: func(id tcpip.NICID) checker.ControlMessagesChecker {
				return checker.ReceiveIPv6PacketInfo(tcpip.IPv6PacketInfo{
					NIC:  id,
					Addr: stackV6Addr,
				})
			},
		},
		{
			name:  "IPv6 multicast",
			proto: header.IPv6ProtocolNumber,
			flow:  multicastV6,
			checker: func(id tcpip.NICID) checker.ControlMessagesChecker {
				return checker.ReceiveIPv6PacketInfo(tcpip.IPv6PacketInfo{
					NIC:  id,
					Addr: multicastV6Addr,
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpoint(test.proto)

			bindAddr := tcpip.FullAddress{Port: stackPort}
			if err := c.ep.Bind(bindAddr); err != nil {
				t.Fatalf("Bind(%+v): %s", bindAddr, err)
			}

			if test.flow.isMulticast() {
				ifoptSet := tcpip.AddMembershipOption{NIC: 1, MulticastAddr: test.flow.getMcastAddr()}
				if err := c.ep.SetSockOpt(&ifoptSet); err != nil {
					c.t.Fatalf("SetSockOpt(&%#v): %s:", ifoptSet, err)
				}
			}

			switch f := test.flow.netProto(); f {
			case header.IPv4ProtocolNumber:
				c.ep.SocketOptions().SetReceivePacketInfo(true)
			case header.IPv6ProtocolNumber:
				c.ep.SocketOptions().SetIPv6ReceivePacketInfo(true)
			default:
				t.Fatalf("unhandled protocol number = %d", f)
			}

			testRead(c, test.flow, test.checker(c.nicID))

			if got := c.s.Stats().UDP.PacketsReceived.Value(); got != 1 {
				t.Fatalf("Read did not increment PacketsReceived: got = %d, want = 1", got)
			}
		})
	}
}

func TestReadRecvOriginalDstAddr(t *testing.T) {
	tests := []struct {
		name                    string
		proto                   tcpip.NetworkProtocolNumber
		flow                    testFlow
		expectedOriginalDstAddr tcpip.FullAddress
	}{
		{
			name:                    "IPv4 unicast",
			proto:                   header.IPv4ProtocolNumber,
			flow:                    unicastV4,
			expectedOriginalDstAddr: tcpip.FullAddress{NIC: 1, Addr: stackAddr, Port: stackPort},
		},
		{
			name:  "IPv4 multicast",
			proto: header.IPv4ProtocolNumber,
			flow:  multicastV4,
			// This should actually be a unicast address assigned to the interface.
			//
			// TODO(gvisor.dev/issue/3556): This check is validating incorrect
			// behaviour. We still include the test so that once the bug is
			// resolved, this test will start to fail and the individual tasked
			// with fixing this bug knows to also fix this test :).
			expectedOriginalDstAddr: tcpip.FullAddress{NIC: 1, Addr: multicastAddr, Port: stackPort},
		},
		{
			name:  "IPv4 broadcast",
			proto: header.IPv4ProtocolNumber,
			flow:  broadcast,
			// This should actually be a unicast address assigned to the interface.
			//
			// TODO(gvisor.dev/issue/3556): This check is validating incorrect
			// behaviour. We still include the test so that once the bug is
			// resolved, this test will start to fail and the individual tasked
			// with fixing this bug knows to also fix this test :).
			expectedOriginalDstAddr: tcpip.FullAddress{NIC: 1, Addr: broadcastAddr, Port: stackPort},
		},
		{
			name:                    "IPv6 unicast",
			proto:                   header.IPv6ProtocolNumber,
			flow:                    unicastV6,
			expectedOriginalDstAddr: tcpip.FullAddress{NIC: 1, Addr: stackV6Addr, Port: stackPort},
		},
		{
			name:  "IPv6 multicast",
			proto: header.IPv6ProtocolNumber,
			flow:  multicastV6,
			// This should actually be a unicast address assigned to the interface.
			//
			// TODO(gvisor.dev/issue/3556): This check is validating incorrect
			// behaviour. We still include the test so that once the bug is
			// resolved, this test will start to fail and the individual tasked
			// with fixing this bug knows to also fix this test :).
			expectedOriginalDstAddr: tcpip.FullAddress{NIC: 1, Addr: multicastV6Addr, Port: stackPort},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpoint(test.proto)

			bindAddr := tcpip.FullAddress{Port: stackPort}
			if err := c.ep.Bind(bindAddr); err != nil {
				t.Fatalf("Bind(%#v): %s", bindAddr, err)
			}

			if test.flow.isMulticast() {
				ifoptSet := tcpip.AddMembershipOption{NIC: 1, MulticastAddr: test.flow.getMcastAddr()}
				if err := c.ep.SetSockOpt(&ifoptSet); err != nil {
					c.t.Fatalf("SetSockOpt(&%#v): %s:", ifoptSet, err)
				}
			}

			c.ep.SocketOptions().SetReceiveOriginalDstAddress(true)

			testRead(c, test.flow, checker.ReceiveOriginalDstAddr(test.expectedOriginalDstAddr))

			if got := c.s.Stats().UDP.PacketsReceived.Value(); got != 1 {
				t.Fatalf("Read did not increment PacketsReceived: got = %d, want = 1", got)
			}
		})
	}
}

func TestWriteIncrementsPacketsSent(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)

	testDualWrite(c)

	var want uint64 = 2
	if got := c.s.Stats().UDP.PacketsSent.Value(); got != want {
		c.t.Fatalf("Write did not increment PacketsSent: got %v, want %v", got, want)
	}
}

func TestNoChecksum(t *testing.T) {
	for _, flow := range []testFlow{unicastV4, unicastV6} {
		t.Run(fmt.Sprintf("flow:%s", flow), func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpointForFlow(flow)

			// Disable the checksum generation.
			c.ep.SocketOptions().SetNoChecksum(true)
			// This option is effective on IPv4 only.
			testWrite(c, flow, checker.UDP(checker.NoChecksum(flow.isV4())))

			// Enable the checksum generation.
			c.ep.SocketOptions().SetNoChecksum(false)
			testWrite(c, flow, checker.UDP(checker.NoChecksum(false)))
		})
	}
}

var _ stack.NetworkInterface = (*testInterface)(nil)

type testInterface struct {
	stack.NetworkInterface
}

func (*testInterface) ID() tcpip.NICID {
	return 0
}

func (*testInterface) Enabled() bool {
	return true
}

func TestTTL(t *testing.T) {
	for _, flow := range []testFlow{unicastV4, unicastV4in6, unicastV6, unicastV6Only, multicastV4, multicastV4in6, multicastV6, broadcast, broadcastIn6} {
		t.Run(fmt.Sprintf("flow:%s", flow), func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpointForFlow(flow)

			const multicastTTL = 42
			if err := c.ep.SetSockOptInt(tcpip.MulticastTTLOption, multicastTTL); err != nil {
				c.t.Fatalf("SetSockOptInt failed: %s", err)
			}

			var wantTTL uint8
			if flow.isMulticast() {
				wantTTL = multicastTTL
			} else {
				var p stack.NetworkProtocolFactory
				var n tcpip.NetworkProtocolNumber
				if flow.isV4() {
					p = ipv4.NewProtocol
					n = ipv4.ProtocolNumber
				} else {
					p = ipv6.NewProtocol
					n = ipv6.ProtocolNumber
				}
				s := stack.New(stack.Options{
					NetworkProtocols: []stack.NetworkProtocolFactory{p},
					Clock:            &faketime.NullClock{},
				})
				ep := s.NetworkProtocolInstance(n).NewEndpoint(&testInterface{}, nil)
				wantTTL = ep.DefaultTTL()
				ep.Close()
			}

			testWrite(c, flow, checker.TTL(wantTTL))
		})
	}
}

func TestSetTTL(t *testing.T) {
	for _, flow := range []testFlow{unicastV4, unicastV4in6, unicastV6, unicastV6Only, broadcast, broadcastIn6} {
		t.Run(fmt.Sprintf("flow:%s", flow), func(t *testing.T) {
			for _, wantTTL := range []uint8{1, 2, 50, 64, 128, 254, 255} {
				t.Run(fmt.Sprintf("TTL:%d", wantTTL), func(t *testing.T) {
					c := newDualTestContext(t, defaultMTU)
					defer c.cleanup()

					c.createEndpointForFlow(flow)

					if err := c.ep.SetSockOptInt(tcpip.TTLOption, int(wantTTL)); err != nil {
						c.t.Fatalf("SetSockOptInt(TTLOption, %d) failed: %s", wantTTL, err)
					}

					testWrite(c, flow, checker.TTL(wantTTL))
				})
			}
		})
	}
}

var v4PacketFlows = [...]testFlow{unicastV4, multicastV4, broadcast, unicastV4in6, multicastV4in6, broadcastIn6}

func TestSetTOS(t *testing.T) {
	for _, flow := range v4PacketFlows {
		t.Run(fmt.Sprintf("flow:%s", flow), func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpointForFlow(flow)

			const tos = testTOS
			v, err := c.ep.GetSockOptInt(tcpip.IPv4TOSOption)
			if err != nil {
				c.t.Errorf("GetSockOptInt(IPv4TOSOption) failed: %s", err)
			}
			// Test for expected default value.
			if v != 0 {
				c.t.Errorf("got GetSockOptInt(IPv4TOSOption) = 0x%x, want = 0x%x", v, 0)
			}

			if err := c.ep.SetSockOptInt(tcpip.IPv4TOSOption, tos); err != nil {
				c.t.Errorf("SetSockOptInt(IPv4TOSOption, 0x%x) failed: %s", tos, err)
			}

			v, err = c.ep.GetSockOptInt(tcpip.IPv4TOSOption)
			if err != nil {
				c.t.Errorf("GetSockOptInt(IPv4TOSOption) failed: %s", err)
			}

			if v != tos {
				c.t.Errorf("got GetSockOptInt(IPv4TOSOption) = 0x%x, want = 0x%x", v, tos)
			}

			testWrite(c, flow, checker.TOS(tos, 0))
		})
	}
}

var v6PacketFlows = [...]testFlow{unicastV6, unicastV6Only, multicastV6}

func TestSetTClass(t *testing.T) {
	for _, flow := range v6PacketFlows {
		t.Run(fmt.Sprintf("flow:%s", flow), func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpointForFlow(flow)

			const tClass = testTOS
			v, err := c.ep.GetSockOptInt(tcpip.IPv6TrafficClassOption)
			if err != nil {
				c.t.Errorf("GetSockOptInt(IPv6TrafficClassOption) failed: %s", err)
			}
			// Test for expected default value.
			if v != 0 {
				c.t.Errorf("got GetSockOptInt(IPv6TrafficClassOption) = 0x%x, want = 0x%x", v, 0)
			}

			if err := c.ep.SetSockOptInt(tcpip.IPv6TrafficClassOption, tClass); err != nil {
				c.t.Errorf("SetSockOptInt(IPv6TrafficClassOption, 0x%x) failed: %s", tClass, err)
			}

			v, err = c.ep.GetSockOptInt(tcpip.IPv6TrafficClassOption)
			if err != nil {
				c.t.Errorf("GetSockOptInt(IPv6TrafficClassOption) failed: %s", err)
			}

			if v != tClass {
				c.t.Errorf("got GetSockOptInt(IPv6TrafficClassOption) = 0x%x, want = 0x%x", v, tClass)
			}

			// The header getter for TClass is called TOS, so use that checker.
			testWrite(c, flow, checker.TOS(tClass, 0))
		})
	}
}

func TestReceiveTosTClass(t *testing.T) {
	const RcvTOSOpt = "ReceiveTosOption"
	const RcvTClassOpt = "ReceiveTClassOption"

	testCases := []struct {
		name  string
		tests []testFlow
	}{
		{
			name:  RcvTOSOpt,
			tests: v4PacketFlows[:],
		},
		{
			name:  RcvTClassOpt,
			tests: v6PacketFlows[:],
		},
	}
	for _, testCase := range testCases {
		for _, flow := range testCase.tests {
			t.Run(fmt.Sprintf("%s:flow:%s", testCase.name, flow), func(t *testing.T) {
				c := newDualTestContext(t, defaultMTU)
				defer c.cleanup()

				c.createEndpointForFlow(flow)
				name := testCase.name

				if flow.isMulticast() {
					netProto := flow.netProto()
					addr := flow.getMcastAddr()
					if err := c.s.JoinGroup(netProto, c.nicID, addr); err != nil {
						c.t.Fatalf("JoinGroup(%d, %d, %s): %s", netProto, c.nicID, addr, err)
					}
				}

				var optionGetter func() bool
				var optionSetter func(bool)
				switch name {
				case RcvTOSOpt:
					optionGetter = c.ep.SocketOptions().GetReceiveTOS
					optionSetter = c.ep.SocketOptions().SetReceiveTOS
				case RcvTClassOpt:
					optionGetter = c.ep.SocketOptions().GetReceiveTClass
					optionSetter = c.ep.SocketOptions().SetReceiveTClass
				default:
					t.Fatalf("unkown test variant: %s", name)
				}

				// Verify that setting and reading the option works.
				v := optionGetter()
				// Test for expected default value.
				if v != false {
					c.t.Errorf("got GetSockOptBool(%s) = %t, want = %t", name, v, false)
				}

				const want = true
				optionSetter(want)

				got := optionGetter()
				if got != want {
					c.t.Errorf("got GetSockOptBool(%s) = %t, want = %t", name, got, want)
				}

				// Verify that the correct received TOS or TClass is handed through as
				// ancillary data to the ControlMessages struct.
				if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
					c.t.Fatalf("Bind failed: %s", err)
				}
				switch name {
				case RcvTClassOpt:
					testRead(c, flow, checker.ReceiveTClass(testTOS))
				case RcvTOSOpt:
					testRead(c, flow, checker.ReceiveTOS(testTOS))
				default:
					t.Fatalf("unknown test variant: %s", name)
				}
			})
		}
	}
}

func TestMulticastInterfaceOption(t *testing.T) {
	for _, flow := range []testFlow{multicastV4, multicastV4in6, multicastV6, multicastV6Only} {
		t.Run(fmt.Sprintf("flow:%s", flow), func(t *testing.T) {
			for _, bindTyp := range []string{"bound", "unbound"} {
				t.Run(bindTyp, func(t *testing.T) {
					for _, optTyp := range []string{"use local-addr", "use NICID", "use local-addr and NIC"} {
						t.Run(optTyp, func(t *testing.T) {
							h := flow.header4Tuple(outgoing)
							mcastAddr := h.dstAddr.Addr
							localIfAddr := h.srcAddr.Addr

							var ifoptSet tcpip.MulticastInterfaceOption
							switch optTyp {
							case "use local-addr":
								ifoptSet.InterfaceAddr = localIfAddr
							case "use NICID":
								ifoptSet.NIC = 1
							case "use local-addr and NIC":
								ifoptSet.InterfaceAddr = localIfAddr
								ifoptSet.NIC = 1
							default:
								t.Fatal("unknown test variant")
							}

							c := newDualTestContext(t, defaultMTU)
							defer c.cleanup()

							c.createEndpoint(flow.sockProto())

							if bindTyp == "bound" {
								// Bind the socket by connecting to the multicast address.
								// This may have an influence on how the multicast interface
								// is set.
								addr := tcpip.FullAddress{
									Addr: flow.mapAddrIfApplicable(mcastAddr),
									Port: stackPort,
								}
								if err := c.ep.Connect(addr); err != nil {
									c.t.Fatalf("Connect failed: %s", err)
								}
							}

							if err := c.ep.SetSockOpt(&ifoptSet); err != nil {
								c.t.Fatalf("SetSockOpt(&%#v): %s", ifoptSet, err)
							}

							// Verify multicast interface addr and NIC were set correctly.
							// Note that NIC must be 1 since this is our outgoing interface.
							var ifoptGot tcpip.MulticastInterfaceOption
							if err := c.ep.GetSockOpt(&ifoptGot); err != nil {
								c.t.Fatalf("GetSockOpt(&%T): %s", ifoptGot, err)
							} else if ifoptWant := (tcpip.MulticastInterfaceOption{NIC: 1, InterfaceAddr: ifoptSet.InterfaceAddr}); ifoptGot != ifoptWant {
								c.t.Errorf("got multicast interface option = %#v, want = %#v", ifoptGot, ifoptWant)
							}
						})
					}
				})
			}
		})
	}
}

// TestV4UnknownDestination verifies that we generate an ICMPv4 Destination
// Unreachable message when a udp datagram is received on ports for which there
// is no bound udp socket.
func TestV4UnknownDestination(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	testCases := []struct {
		flow         testFlow
		icmpRequired bool
		// largePayload if true, will result in a payload large enough
		// so that the final generated IPv4 packet is larger than
		// header.IPv4MinimumProcessableDatagramSize.
		largePayload bool
		// badChecksum if true, will set an invalid checksum in the
		// header.
		badChecksum bool
	}{
		{unicastV4, true, false, false},
		{unicastV4, true, true, false},
		{unicastV4, false, false, true},
		{unicastV4, false, true, true},
		{multicastV4, false, false, false},
		{multicastV4, false, true, false},
		{broadcast, false, false, false},
		{broadcast, false, true, false},
	}
	checksumErrors := uint64(0)
	for _, tc := range testCases {
		t.Run(fmt.Sprintf("flow:%s icmpRequired:%t largePayload:%t badChecksum:%t", tc.flow, tc.icmpRequired, tc.largePayload, tc.badChecksum), func(t *testing.T) {
			payload := newPayload()
			if tc.largePayload {
				payload = newMinPayload(576)
			}
			c.injectPacket(tc.flow, payload, tc.badChecksum)
			if tc.badChecksum {
				checksumErrors++
				if got, want := c.s.Stats().UDP.ChecksumErrors.Value(), checksumErrors; got != want {
					t.Fatalf("got stats.UDP.ChecksumErrors.Value() = %d, want = %d", got, want)
				}
			}
			if !tc.icmpRequired {
				if p, ok := c.linkEP.Read(); ok {
					t.Fatalf("unexpected packet received: %+v", p)
				}
				return
			}

			// ICMP required.
			p, ok := c.linkEP.Read()
			if !ok {
				t.Fatalf("packet wasn't written out")
				return
			}

			vv := buffer.NewVectorisedView(p.Pkt.Size(), p.Pkt.Views())
			pkt := vv.ToView()
			if got, want := len(pkt), header.IPv4MinimumProcessableDatagramSize; got > want {
				t.Fatalf("got an ICMP packet of size: %d, want: sz <= %d", got, want)
			}

			hdr := header.IPv4(pkt)
			checker.IPv4(t, hdr, checker.ICMPv4(
				checker.ICMPv4Type(header.ICMPv4DstUnreachable),
				checker.ICMPv4Code(header.ICMPv4PortUnreachable)))

			// We need to compare the included data part of the UDP packet that is in
			// the ICMP packet with the matching original data.
			icmpPkt := header.ICMPv4(hdr.Payload())
			payloadIPHeader := header.IPv4(icmpPkt.Payload())
			incomingHeaderLength := header.IPv4MinimumSize + header.UDPMinimumSize
			wantLen := len(payload)
			if tc.largePayload {
				// To work out the data size we need to simulate what the sender would
				// have done. The wanted size is the total available minus the sum of
				// the headers in the UDP AND ICMP packets, given that we know the test
				// had only a minimal IP header but the ICMP sender will have allowed
				// for a maximally sized packet header.
				wantLen = header.IPv4MinimumProcessableDatagramSize - header.IPv4MaximumHeaderSize - header.ICMPv4MinimumSize - incomingHeaderLength
			}

			// In the case of large payloads the IP packet may be truncated. Update
			// the length field before retrieving the udp datagram payload.
			// Add back the two headers within the payload.
			payloadIPHeader.SetTotalLength(uint16(wantLen + incomingHeaderLength))

			origDgram := header.UDP(payloadIPHeader.Payload())
			if got, want := len(origDgram.Payload()), wantLen; got != want {
				t.Fatalf("unexpected payload length got: %d, want: %d", got, want)
			}
			if got, want := origDgram.Payload(), payload[:wantLen]; !bytes.Equal(got, want) {
				t.Fatalf("unexpected payload got: %d, want: %d", got, want)
			}
		})
	}
}

// TestV6UnknownDestination verifies that we generate an ICMPv6 Destination
// Unreachable message when a udp datagram is received on ports for which there
// is no bound udp socket.
func TestV6UnknownDestination(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	testCases := []struct {
		flow         testFlow
		icmpRequired bool
		// largePayload if true will result in a payload large enough to
		// create an IPv6 packet > header.IPv6MinimumMTU bytes.
		largePayload bool
		// badChecksum if true, will set an invalid checksum in the
		// header.
		badChecksum bool
	}{
		{unicastV6, true, false, false},
		{unicastV6, true, true, false},
		{unicastV6, false, false, true},
		{unicastV6, false, true, true},
		{multicastV6, false, false, false},
		{multicastV6, false, true, false},
	}
	checksumErrors := uint64(0)
	for _, tc := range testCases {
		t.Run(fmt.Sprintf("flow:%s icmpRequired:%t largePayload:%t badChecksum:%t", tc.flow, tc.icmpRequired, tc.largePayload, tc.badChecksum), func(t *testing.T) {
			payload := newPayload()
			if tc.largePayload {
				payload = newMinPayload(1280)
			}
			c.injectPacket(tc.flow, payload, tc.badChecksum)
			if tc.badChecksum {
				checksumErrors++
				if got, want := c.s.Stats().UDP.ChecksumErrors.Value(), checksumErrors; got != want {
					t.Fatalf("got stats.UDP.ChecksumErrors.Value() = %d, want = %d", got, want)
				}
			}
			if !tc.icmpRequired {
				if p, ok := c.linkEP.Read(); ok {
					t.Fatalf("unexpected packet received: %+v", p)
				}
				return
			}

			// ICMP required.
			p, ok := c.linkEP.Read()
			if !ok {
				t.Fatalf("packet wasn't written out")
				return
			}

			vv := buffer.NewVectorisedView(p.Pkt.Size(), p.Pkt.Views())
			pkt := vv.ToView()
			if got, want := len(pkt), header.IPv6MinimumMTU; got > want {
				t.Fatalf("got an ICMP packet of size: %d, want: sz <= %d", got, want)
			}

			hdr := header.IPv6(pkt)
			checker.IPv6(t, hdr, checker.ICMPv6(
				checker.ICMPv6Type(header.ICMPv6DstUnreachable),
				checker.ICMPv6Code(header.ICMPv6PortUnreachable)))

			icmpPkt := header.ICMPv6(hdr.Payload())
			payloadIPHeader := header.IPv6(icmpPkt.Payload())
			wantLen := len(payload)
			if tc.largePayload {
				wantLen = header.IPv6MinimumMTU - header.IPv6MinimumSize*2 - header.ICMPv6MinimumSize - header.UDPMinimumSize
			}
			// In case of large payloads the IP packet may be truncated. Update
			// the length field before retrieving the udp datagram payload.
			payloadIPHeader.SetPayloadLength(uint16(wantLen + header.UDPMinimumSize))

			origDgram := header.UDP(payloadIPHeader.Payload())
			if got, want := len(origDgram.Payload()), wantLen; got != want {
				t.Fatalf("unexpected payload length got: %d, want: %d", got, want)
			}
			if got, want := origDgram.Payload(), payload[:wantLen]; !bytes.Equal(got, want) {
				t.Fatalf("unexpected payload got: %v, want: %v", got, want)
			}
		})
	}
}

// TestIncrementMalformedPacketsReceived verifies if the malformed received
// global and endpoint stats are incremented.
func TestIncrementMalformedPacketsReceived(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)
	// Bind to wildcard.
	if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	payload := newPayload()
	h := unicastV6.header4Tuple(incoming)
	buf := c.buildV6Packet(payload, &h)

	// Invalidate the UDP header length field.
	u := header.UDP(buf[header.IPv6MinimumSize:])
	u.SetLength(u.Length() + 1)

	c.linkEP.InjectInbound(ipv6.ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{
		Data: buf.ToVectorisedView(),
	}))

	const want = 1
	if got := c.s.Stats().UDP.MalformedPacketsReceived.Value(); got != want {
		t.Errorf("got stats.UDP.MalformedPacketsReceived.Value() = %d, want = %d", got, want)
	}
	if got := c.ep.Stats().(*tcpip.TransportEndpointStats).ReceiveErrors.MalformedPacketsReceived.Value(); got != want {
		t.Errorf("got EP Stats.ReceiveErrors.MalformedPacketsReceived stats = %d, want = %d", got, want)
	}
}

// TestShortHeader verifies that when a packet with a too-short UDP header is
// received, the malformed received global stat gets incremented.
func TestShortHeader(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)
	// Bind to wildcard.
	if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	h := unicastV6.header4Tuple(incoming)

	// Allocate a buffer for an IPv6 and too-short UDP header.
	const udpSize = header.UDPMinimumSize - 1
	buf := buffer.NewView(header.IPv6MinimumSize + udpSize)
	// Initialize the IP header.
	ip := header.IPv6(buf)
	ip.Encode(&header.IPv6Fields{
		TrafficClass:      testTOS,
		PayloadLength:     uint16(udpSize),
		TransportProtocol: udp.ProtocolNumber,
		HopLimit:          65,
		SrcAddr:           h.srcAddr.Addr,
		DstAddr:           h.dstAddr.Addr,
	})

	// Initialize the UDP header.
	udpHdr := header.UDP(buffer.NewView(header.UDPMinimumSize))
	udpHdr.Encode(&header.UDPFields{
		SrcPort: h.srcAddr.Port,
		DstPort: h.dstAddr.Port,
		Length:  header.UDPMinimumSize,
	})
	// Calculate the UDP pseudo-header checksum.
	xsum := header.PseudoHeaderChecksum(udp.ProtocolNumber, h.srcAddr.Addr, h.dstAddr.Addr, uint16(len(udpHdr)))
	udpHdr.SetChecksum(^udpHdr.CalculateChecksum(xsum))
	// Copy all but the last byte of the UDP header into the packet.
	copy(buf[header.IPv6MinimumSize:], udpHdr)

	// Inject packet.
	c.linkEP.InjectInbound(ipv6.ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{
		Data: buf.ToVectorisedView(),
	}))

	if got, want := c.s.Stats().NICs.MalformedL4RcvdPackets.Value(), uint64(1); got != want {
		t.Errorf("got c.s.Stats().NIC.MalformedL4RcvdPackets.Value() = %d, want = %d", got, want)
	}
}

// TestBadChecksumErrors verifies if a checksum error is detected,
// global and endpoint stats are incremented.
func TestBadChecksumErrors(t *testing.T) {
	for _, flow := range []testFlow{unicastV4, unicastV6} {
		t.Run(flow.String(), func(t *testing.T) {
			c := newDualTestContext(t, defaultMTU)
			defer c.cleanup()

			c.createEndpoint(flow.sockProto())
			// Bind to wildcard.
			if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
				c.t.Fatalf("Bind failed: %s", err)
			}

			payload := newPayload()
			c.injectPacket(flow, payload, true /* badChecksum */)

			const want = 1
			if got := c.s.Stats().UDP.ChecksumErrors.Value(); got != want {
				t.Errorf("got stats.UDP.ChecksumErrors.Value() = %d, want = %d", got, want)
			}
			if got := c.ep.Stats().(*tcpip.TransportEndpointStats).ReceiveErrors.ChecksumErrors.Value(); got != want {
				t.Errorf("got EP Stats.ReceiveErrors.ChecksumErrors stats = %d, want = %d", got, want)
			}
		})
	}
}

// TestPayloadModifiedV4 verifies if a checksum error is detected,
// global and endpoint stats are incremented.
func TestPayloadModifiedV4(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv4.ProtocolNumber)
	// Bind to wildcard.
	if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	payload := newPayload()
	h := unicastV4.header4Tuple(incoming)
	buf := c.buildV4Packet(payload, &h)
	// Modify the payload so that the checksum value in the UDP header will be
	// incorrect.
	buf[len(buf)-1]++
	c.linkEP.InjectInbound(ipv4.ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{
		Data: buf.ToVectorisedView(),
	}))

	const want = 1
	if got := c.s.Stats().UDP.ChecksumErrors.Value(); got != want {
		t.Errorf("got stats.UDP.ChecksumErrors.Value() = %d, want = %d", got, want)
	}
	if got := c.ep.Stats().(*tcpip.TransportEndpointStats).ReceiveErrors.ChecksumErrors.Value(); got != want {
		t.Errorf("got EP Stats.ReceiveErrors.ChecksumErrors stats = %d, want = %d", got, want)
	}
}

// TestPayloadModifiedV6 verifies if a checksum error is detected,
// global and endpoint stats are incremented.
func TestPayloadModifiedV6(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)
	// Bind to wildcard.
	if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	payload := newPayload()
	h := unicastV6.header4Tuple(incoming)
	buf := c.buildV6Packet(payload, &h)
	// Modify the payload so that the checksum value in the UDP header will be
	// incorrect.
	buf[len(buf)-1]++
	c.linkEP.InjectInbound(ipv6.ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{
		Data: buf.ToVectorisedView(),
	}))

	const want = 1
	if got := c.s.Stats().UDP.ChecksumErrors.Value(); got != want {
		t.Errorf("got stats.UDP.ChecksumErrors.Value() = %d, want = %d", got, want)
	}
	if got := c.ep.Stats().(*tcpip.TransportEndpointStats).ReceiveErrors.ChecksumErrors.Value(); got != want {
		t.Errorf("got EP Stats.ReceiveErrors.ChecksumErrors stats = %d, want = %d", got, want)
	}
}

// TestChecksumZeroV4 verifies if the checksum value is zero, global and
// endpoint states are *not* incremented (UDP checksum is optional on IPv4).
func TestChecksumZeroV4(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv4.ProtocolNumber)
	// Bind to wildcard.
	if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	payload := newPayload()
	h := unicastV4.header4Tuple(incoming)
	buf := c.buildV4Packet(payload, &h)
	// Set the checksum field in the UDP header to zero.
	u := header.UDP(buf[header.IPv4MinimumSize:])
	u.SetChecksum(0)
	c.linkEP.InjectInbound(ipv4.ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{
		Data: buf.ToVectorisedView(),
	}))

	const want = 0
	if got := c.s.Stats().UDP.ChecksumErrors.Value(); got != want {
		t.Errorf("got stats.UDP.ChecksumErrors.Value() = %d, want = %d", got, want)
	}
	if got := c.ep.Stats().(*tcpip.TransportEndpointStats).ReceiveErrors.ChecksumErrors.Value(); got != want {
		t.Errorf("got EP Stats.ReceiveErrors.ChecksumErrors stats = %d, want = %d", got, want)
	}
}

// TestChecksumZeroV6 verifies if the checksum value is zero, global and
// endpoint states are incremented (UDP checksum is *not* optional on IPv6).
func TestChecksumZeroV6(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)
	// Bind to wildcard.
	if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	payload := newPayload()
	h := unicastV6.header4Tuple(incoming)
	buf := c.buildV6Packet(payload, &h)
	// Set the checksum field in the UDP header to zero.
	u := header.UDP(buf[header.IPv6MinimumSize:])
	u.SetChecksum(0)
	c.linkEP.InjectInbound(ipv6.ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{
		Data: buf.ToVectorisedView(),
	}))

	const want = 1
	if got := c.s.Stats().UDP.ChecksumErrors.Value(); got != want {
		t.Errorf("got stats.UDP.ChecksumErrors.Value() = %d, want = %d", got, want)
	}
	if got := c.ep.Stats().(*tcpip.TransportEndpointStats).ReceiveErrors.ChecksumErrors.Value(); got != want {
		t.Errorf("got EP Stats.ReceiveErrors.ChecksumErrors stats = %d, want = %d", got, want)
	}
}

// TestShutdownRead verifies endpoint read shutdown and error
// stats increment on packet receive.
func TestShutdownRead(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)

	// Bind to wildcard.
	if err := c.ep.Bind(tcpip.FullAddress{Port: stackPort}); err != nil {
		c.t.Fatalf("Bind failed: %s", err)
	}

	if err := c.ep.Connect(tcpip.FullAddress{Addr: testV6Addr, Port: testPort}); err != nil {
		c.t.Fatalf("Connect failed: %s", err)
	}

	if err := c.ep.Shutdown(tcpip.ShutdownRead); err != nil {
		t.Fatalf("Shutdown failed: %s", err)
	}

	testFailingRead(c, unicastV6, true /* expectReadError */)

	var want uint64 = 1
	if got := c.s.Stats().UDP.ReceiveBufferErrors.Value(); got != want {
		t.Errorf("got stats.UDP.ReceiveBufferErrors.Value() = %v, want = %v", got, want)
	}
	if got := c.ep.Stats().(*tcpip.TransportEndpointStats).ReceiveErrors.ClosedReceiver.Value(); got != want {
		t.Errorf("got EP Stats.ReceiveErrors.ClosedReceiver stats = %v, want = %v", got, want)
	}
}

// TestShutdownWrite verifies endpoint write shutdown and error
// stats increment on packet write.
func TestShutdownWrite(t *testing.T) {
	c := newDualTestContext(t, defaultMTU)
	defer c.cleanup()

	c.createEndpoint(ipv6.ProtocolNumber)

	if err := c.ep.Connect(tcpip.FullAddress{Addr: testV6Addr, Port: testPort}); err != nil {
		c.t.Fatalf("Connect failed: %s", err)
	}

	if err := c.ep.Shutdown(tcpip.ShutdownWrite); err != nil {
		t.Fatalf("Shutdown failed: %s", err)
	}

	testFailingWrite(c, unicastV6, &tcpip.ErrClosedForSend{})
}

func (c *testContext) checkEndpointWriteStats(incr uint64, want tcpip.TransportEndpointStats, err tcpip.Error) {
	got := c.ep.Stats().(*tcpip.TransportEndpointStats).Clone()
	switch err.(type) {
	case nil:
		want.PacketsSent.IncrementBy(incr)
	case *tcpip.ErrMessageTooLong, *tcpip.ErrInvalidOptionValue:
		want.WriteErrors.InvalidArgs.IncrementBy(incr)
	case *tcpip.ErrClosedForSend:
		want.WriteErrors.WriteClosed.IncrementBy(incr)
	case *tcpip.ErrInvalidEndpointState:
		want.WriteErrors.InvalidEndpointState.IncrementBy(incr)
	case *tcpip.ErrNoRoute, *tcpip.ErrBroadcastDisabled, *tcpip.ErrNetworkUnreachable:
		want.SendErrors.NoRoute.IncrementBy(incr)
	default:
		want.SendErrors.SendToNetworkFailed.IncrementBy(incr)
	}
	if got != want {
		c.t.Errorf("Endpoint stats not matching for error %s got %+v want %+v", err, got, want)
	}
}

func (c *testContext) checkEndpointReadStats(incr uint64, want tcpip.TransportEndpointStats, err tcpip.Error) {
	got := c.ep.Stats().(*tcpip.TransportEndpointStats).Clone()
	switch err.(type) {
	case nil, *tcpip.ErrWouldBlock:
	case *tcpip.ErrClosedForReceive:
		want.ReadErrors.ReadClosed.IncrementBy(incr)
	default:
		c.t.Errorf("Endpoint error missing stats update err %v", err)
	}
	if got != want {
		c.t.Errorf("Endpoint stats not matching for error %s got %+v want %+v", err, got, want)
	}
}

func TestOutgoingSubnetBroadcast(t *testing.T) {
	const nicID1 = 1

	ipv4Addr := tcpip.AddressWithPrefix{
		Address:   "\xc0\xa8\x01\x3a",
		PrefixLen: 24,
	}
	ipv4Subnet := ipv4Addr.Subnet()
	ipv4SubnetBcast := ipv4Subnet.Broadcast()
	ipv4Gateway := testutil.MustParse4("192.168.1.1")
	ipv4AddrPrefix31 := tcpip.AddressWithPrefix{
		Address:   "\xc0\xa8\x01\x3a",
		PrefixLen: 31,
	}
	ipv4Subnet31 := ipv4AddrPrefix31.Subnet()
	ipv4Subnet31Bcast := ipv4Subnet31.Broadcast()
	ipv4AddrPrefix32 := tcpip.AddressWithPrefix{
		Address:   "\xc0\xa8\x01\x3a",
		PrefixLen: 32,
	}
	ipv4Subnet32 := ipv4AddrPrefix32.Subnet()
	ipv4Subnet32Bcast := ipv4Subnet32.Broadcast()
	ipv6Addr := tcpip.AddressWithPrefix{
		Address:   "\x20\x0a\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01",
		PrefixLen: 64,
	}
	ipv6Subnet := ipv6Addr.Subnet()
	ipv6SubnetBcast := ipv6Subnet.Broadcast()
	remNetAddr := tcpip.AddressWithPrefix{
		Address:   "\x64\x0a\x7b\x18",
		PrefixLen: 24,
	}
	remNetSubnet := remNetAddr.Subnet()
	remNetSubnetBcast := remNetSubnet.Broadcast()

	tests := []struct {
		name                 string
		nicAddr              tcpip.ProtocolAddress
		routes               []tcpip.Route
		remoteAddr           tcpip.Address
		requiresBroadcastOpt bool
	}{
		{
			name: "IPv4 Broadcast to local subnet",
			nicAddr: tcpip.ProtocolAddress{
				Protocol:          header.IPv4ProtocolNumber,
				AddressWithPrefix: ipv4Addr,
			},
			routes: []tcpip.Route{
				{
					Destination: ipv4Subnet,
					NIC:         nicID1,
				},
			},
			remoteAddr:           ipv4SubnetBcast,
			requiresBroadcastOpt: true,
		},
		{
			name: "IPv4 Broadcast to local /31 subnet",
			nicAddr: tcpip.ProtocolAddress{
				Protocol:          header.IPv4ProtocolNumber,
				AddressWithPrefix: ipv4AddrPrefix31,
			},
			routes: []tcpip.Route{
				{
					Destination: ipv4Subnet31,
					NIC:         nicID1,
				},
			},
			remoteAddr:           ipv4Subnet31Bcast,
			requiresBroadcastOpt: false,
		},
		{
			name: "IPv4 Broadcast to local /32 subnet",
			nicAddr: tcpip.ProtocolAddress{
				Protocol:          header.IPv4ProtocolNumber,
				AddressWithPrefix: ipv4AddrPrefix32,
			},
			routes: []tcpip.Route{
				{
					Destination: ipv4Subnet32,
					NIC:         nicID1,
				},
			},
			remoteAddr:           ipv4Subnet32Bcast,
			requiresBroadcastOpt: false,
		},
		// IPv6 has no notion of a broadcast.
		{
			name: "IPv6 'Broadcast' to local subnet",
			nicAddr: tcpip.ProtocolAddress{
				Protocol:          header.IPv6ProtocolNumber,
				AddressWithPrefix: ipv6Addr,
			},
			routes: []tcpip.Route{
				{
					Destination: ipv6Subnet,
					NIC:         nicID1,
				},
			},
			remoteAddr:           ipv6SubnetBcast,
			requiresBroadcastOpt: false,
		},
		{
			name: "IPv4 Broadcast to remote subnet",
			nicAddr: tcpip.ProtocolAddress{
				Protocol:          header.IPv4ProtocolNumber,
				AddressWithPrefix: ipv4Addr,
			},
			routes: []tcpip.Route{
				{
					Destination: remNetSubnet,
					Gateway:     ipv4Gateway,
					NIC:         nicID1,
				},
			},
			remoteAddr: remNetSubnetBcast,
			// TODO(gvisor.dev/issue/3938): Once we support marking a route as
			// broadcast, this test should require the broadcast option to be set.
			requiresBroadcastOpt: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := stack.New(stack.Options{
				NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
				TransportProtocols: []stack.TransportProtocolFactory{udp.NewProtocol},
				Clock:              &faketime.NullClock{},
			})
			e := channel.New(0, defaultMTU, "")
			if err := s.CreateNIC(nicID1, e); err != nil {
				t.Fatalf("CreateNIC(%d, _): %s", nicID1, err)
			}
			if err := s.AddProtocolAddress(nicID1, test.nicAddr, stack.AddressProperties{}); err != nil {
				t.Fatalf("AddProtocolAddress(%d, %+v, {}): %s", nicID1, test.nicAddr, err)
			}

			s.SetRouteTable(test.routes)

			var netProto tcpip.NetworkProtocolNumber
			switch l := len(test.remoteAddr); l {
			case header.IPv4AddressSize:
				netProto = header.IPv4ProtocolNumber
			case header.IPv6AddressSize:
				netProto = header.IPv6ProtocolNumber
			default:
				t.Fatalf("got unexpected address length = %d bytes", l)
			}

			wq := waiter.Queue{}
			ep, err := s.NewEndpoint(udp.ProtocolNumber, netProto, &wq)
			if err != nil {
				t.Fatalf("NewEndpoint(%d, %d, _): %s", udp.ProtocolNumber, netProto, err)
			}
			defer ep.Close()

			var r bytes.Reader
			data := []byte{1, 2, 3, 4}
			to := tcpip.FullAddress{
				Addr: test.remoteAddr,
				Port: 80,
			}
			opts := tcpip.WriteOptions{To: &to}
			expectedErrWithoutBcastOpt := func(err tcpip.Error) tcpip.Error {
				if _, ok := err.(*tcpip.ErrBroadcastDisabled); ok {
					return nil
				}
				return &tcpip.ErrBroadcastDisabled{}
			}
			if !test.requiresBroadcastOpt {
				expectedErrWithoutBcastOpt = nil
			}

			r.Reset(data)
			{
				n, err := ep.Write(&r, opts)
				if expectedErrWithoutBcastOpt != nil {
					if want := expectedErrWithoutBcastOpt(err); want != nil {
						t.Fatalf("got ep.Write(_, %#v) = (%d, %s), want = (_, %s)", opts, n, err, want)
					}
				} else if err != nil {
					t.Fatalf("got ep.Write(_, %#v) = (%d, %s), want = (_, nil)", opts, n, err)
				}
			}

			ep.SocketOptions().SetBroadcast(true)

			r.Reset(data)
			if n, err := ep.Write(&r, opts); err != nil {
				t.Fatalf("got ep.Write(_, %#v) = (%d, %s), want = (_, nil)", opts, n, err)
			}

			ep.SocketOptions().SetBroadcast(false)

			r.Reset(data)
			{
				n, err := ep.Write(&r, opts)
				if expectedErrWithoutBcastOpt != nil {
					if want := expectedErrWithoutBcastOpt(err); want != nil {
						t.Fatalf("got ep.Write(_, %#v) = (%d, %s), want = (_, %s)", opts, n, err, want)
					}
				} else if err != nil {
					t.Fatalf("got ep.Write(_, %#v) = (%d, %s), want = (_, nil)", opts, n, err)
				}
			}
		})
	}
}
