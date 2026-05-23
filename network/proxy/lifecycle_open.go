// This file holds the per-listener open/start helpers split out of
// lifecycle.go to keep the reconciler file under the LOC budget. The
// helpers do not access ProxyManager state outside their parameters
// (apart from m.stopped under m.mu, which the reconciler validates
// after openListener returns); keeping them here also makes the
// TCP-vs-UDP construction symmetry easier to scan.
package proxy

import (
	"fmt"
	"net"
)

// openListener materialises one listener and stores it under
// m.listeners. Picks TCP vs UDP from the port's protocol field.
func (m *ProxyManager) openListener(spec ServiceSpec, key listenerKey) error {
	port, ok := findPort(spec, key.portName)
	if !ok {
		return fmt.Errorf("port %q not in spec", key.portName)
	}
	listenAddr := net.JoinHostPort(key.bridgeIP.String(), fmt.Sprintf("%d", port.Port))

	stats := m.stats.GetOrCreate(spec.ID)
	selector := m.selectorBuilder(spec.ID)
	resolver := m.resolverBuilder(spec.ID)
	visibility := ServiceVisibility{Scope: spec.Visibility, Group: spec.GroupID}

	entry := &proxyEntry{key: key, protocol: port.Protocol}

	switch port.Protocol {
	case "tcp":
		if err := m.startTCP(entry, listenAddr, selector, resolver, stats, port.Name, visibility); err != nil {
			return err
		}
	case "udp":
		if err := m.startUDP(entry, listenAddr, selector, resolver, stats, port.Name, visibility); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported protocol %q", port.Protocol)
	}

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		closeEntry(entry)
		return ErrManagerStopped
	}
	if _, ok := m.listeners[key]; ok {
		// Race lost: close ours, keep the existing one in the map.
		m.mu.Unlock()
		closeEntry(entry)
		return nil
	}
	m.listeners[key] = entry
	m.mu.Unlock()
	return nil
}

// startTCP opens a TCP listener and wires it to the TCPListener.
func (m *ProxyManager) startTCP(entry *proxyEntry, addr string, selector *Selector, resolver ServiceResolver, stats *Stats, portName string, visibility ServiceVisibility) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("tcp listen %s: %w", addr, err)
	}
	tl := NewTCPListener(
		WithTCPLogger(m.logger),
		WithTCPSelector(selector),
		WithTCPResolver(resolver),
		WithTCPStats(stats),
		WithTCPPortName(portName),
		WithTCPSampler(m.sampler),
		WithTCPSourceResolver(m.srcResolver),
		WithTCPVisibility(visibility),
	)
	if err := tl.Start(lis); err != nil {
		_ = lis.Close()
		return fmt.Errorf("tcp start %s: %w", addr, err)
	}
	entry.tcp = tl
	entry.listener = lis
	return nil
}

// startUDP opens a UDP packet socket and wires it to the UDPListener.
func (m *ProxyManager) startUDP(entry *proxyEntry, addr string, selector *Selector, resolver ServiceResolver, stats *Stats, portName string, visibility ServiceVisibility) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("udp listen %s: %w", addr, err)
	}
	ul := NewUDPListener(
		WithUDPLogger(m.logger),
		WithUDPSelector(selector),
		WithUDPResolver(resolver),
		WithUDPStats(stats),
		WithUDPPortName(portName),
		WithUDPSampler(m.sampler),
		WithUDPSourceResolver(m.srcResolver),
		WithUDPVisibility(visibility),
	)
	if err := ul.Start(pc); err != nil {
		_ = pc.Close()
		return fmt.Errorf("udp start %s: %w", addr, err)
	}
	entry.udp = ul
	entry.pconn = pc
	return nil
}
