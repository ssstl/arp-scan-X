package arp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
)

type arpTable struct {
	IP           net.IP
	HardwareAddr net.HardwareAddr
}

type arpTables []arpTable

type arpStruct struct {
	iface *net.Interface
}

/*
 * IfaceToName is func....
 * INPUT  : interfaceNames
 *        : ex1, "eth1"
 *        : ex2, "eth1,eth2,eth3"
 *        : ex3, "all"
 * OUTPUT : []string
 *        : ex1, []string{"eth1"}
 *        : ex2, []string{"eth1", "eth2", "eth3"}
 *        : ex3, []string{"eth1", "eth2", "eth3", ... all interface}
 */
func IfaceToName(interfaceNames string) ([]string, error) {
	var r []string
	if interfaceNames == "all" {
		ifaces, err := net.Interfaces()
		if err != nil {
			return r, err
		}
		for _, iface := range ifaces {
			r = append(r, iface.Name)
		}
		return r, nil
	}
	r = strings.Split(interfaceNames, ",")
	for _, interfaceName := range r {
		_, err := net.InterfaceByName(interfaceName)
		if err != nil {
			return r, fmt.Errorf("interface %v: unkown", interfaceName)
		}
	}
	return r, nil
}

/*
 * New is ...
 */
func New(interfaceName string) (arpStruct, error) {
	a := arpStruct{}
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		return a, fmt.Errorf("interface %v: unkown", interfaceName)
	}
	a.iface = iface
	return a, nil
}

// scan scans an individual interface's local network for machines using ARP requests/replies.  scan loops forever, sending packets out regularly.  It returns an error if
// it's ever unable to write a packet.
func (a arpStruct) Scan() (arpTables, error) {
	// We just look for IPv4 addresses, so try to find if the interface has one.
	var addr *net.IPNet
	var at arpTables
	addrs, err := a.iface.Addrs()
	if err != nil {
		return at, err
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				addr = &net.IPNet{
					IP:   ip4,
					Mask: ipnet.Mask[len(ipnet.Mask)-4:],
				}
				break
			}
		}
	}
	if len(a.iface.HardwareAddr) == 0 {
		return at, errors.New("Could not obtain MAC address")
	}
	// Sanity-check that the interface has a good address.
	if addr == nil {
		return at, errors.New("no good IP network found")
	} else if addr.IP[0] == 127 {
		return at, errors.New("skipping localhost")
	} else if addr.Mask[0] != 0xff || addr.Mask[1] != 0xff {
		return at, errors.New("mask means network is too large")
	}
	log.Printf("Using network range %v for interface %v", addr, a.iface.Name)

	// Open up a pcap handle for packet reads/writes.
	handle, err := pcap.OpenLive(a.iface.Name, 65536, true, pcap.BlockForever)
	if err != nil {
		return at, err
	}
	defer handle.Close()

	stop := make(chan bool)
	go readARP(handle, a.iface, &at, stop)
	defer close(stop)
	// go readARP(handle, a.iface, &at)
	if err := writeARP(handle, a.iface, addr); err != nil {
		log.Printf("error writing packets on %v: %v", a.iface.Name, err)
		return at, err
	}
	// We don't know exactly how long it'll take for packets to be
	// sent back to us, but 2 seconds should be more than enough
	// time ;)
	time.Sleep(2 * time.Second)
	stop <- true
	return at, nil
}

func readARP(handle *pcap.Handle, iface *net.Interface, arpTables *arpTables, stop chan bool) {
	src := gopacket.NewPacketSource(handle, layers.LayerTypeEthernet)
	in := src.Packets()
	for {
		var packet gopacket.Packet
		select {
		case <-stop:
			return
		case packet = <-in:
			arpLayer := packet.Layer(layers.LayerTypeARP)
			if arpLayer == nil {
				continue
			}
			arp := arpLayer.(*layers.ARP)
			if arp.Operation != layers.ARPReply || bytes.Equal([]byte(iface.HardwareAddr), arp.SourceHwAddress) {
				// This is a packet I sent.
				continue
			}
			// Note:  we might get some packets here that aren't responses to ones we've sent,
			// if for example someone else sends US an ARP request.  Doesn't much matter, though...
			// all information is good information :)
			*arpTables = append(*arpTables, arpTable{
				IP:           net.IP(arp.SourceProtAddress),
				HardwareAddr: net.HardwareAddr(arp.SourceHwAddress),
			})
			// log.Printf("IP %v is at %v", net.IP(arp.SourceProtAddress), net.HardwareAddr(arp.SourceHwAddress))
		}
	}
}

// writeARP writes an ARP request for each address on our local network to the
// pcap handle.
func writeARP(handle *pcap.Handle, iface *net.Interface, addr *net.IPNet) error {
	// Set up all the layers' fields we can.
	eth := layers.Ethernet{
		SrcMAC:       iface.HardwareAddr,
		DstMAC:       net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		EthernetType: layers.EthernetTypeARP,
	}
	arp := layers.ARP{
		AddrType:          layers.LinkTypeEthernet,
		Protocol:          layers.EthernetTypeIPv4,
		HwAddressSize:     6,
		ProtAddressSize:   4,
		Operation:         layers.ARPRequest,
		SourceHwAddress:   []byte(iface.HardwareAddr),
		SourceProtAddress: []byte(addr.IP),
		DstHwAddress:      []byte{0, 0, 0, 0, 0, 0},
	}
	// Set up buffer and options for serialization.
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}
	// Send one packet for every address.
	for _, ip := range ips(addr) {
		arp.DstProtAddress = []byte(ip)
		gopacket.SerializeLayers(buf, opts, &eth, &arp)
		if err := handle.WritePacketData(buf.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

// ips is a simple and not very good method for getting all IPv4 addresses from a
// net.IPNet.  It returns all IPs it can over the channel it sends back, closing
// the channel when done.
func ips(n *net.IPNet) (out []net.IP) {
	num := binary.BigEndian.Uint32([]byte(n.IP))
	mask := binary.BigEndian.Uint32([]byte(n.Mask))
	num &= mask
	for mask < 0xffffffff {
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], num)
		out = append(out, net.IP(buf[:]))
		mask++
		num++
	}
	return
}
