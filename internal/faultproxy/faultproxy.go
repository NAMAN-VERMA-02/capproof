// Package faultproxy is a TCP proxy that breaks responses on purpose, in the ways
// networks do: resets, stalls, clean early closes and tiny fragments.
package faultproxy

import (
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

// Fault is what happens to one connection's response.
type Fault int

// Faults.
const (
	None       Fault = iota
	ResetEarly       // reset before anything reaches the provider
	ResetAfter       // reset partway through the response
	CloseAfter       // clean FIN partway through, which looks like a normal end
	StallAfter       // stop forwarding partway through, without closing
	Fragmented       // deliver the response in very small pieces
	faultCount
)

// Proxy forwards to a target, picking a fault for each connection.
type Proxy struct {
	listener net.Listener
	target   string
	wg       sync.WaitGroup

	mu    sync.Mutex
	rng   *rand.Rand
	conns map[net.Conn]struct{}
	done  bool
}

// Start listens on a free loopback port. The seed makes the sequence of faults
// repeatable, though scheduling still decides which call meets which one.
func Start(target string, seed uint64) (*Proxy, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{listener: l, target: target, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.accept()
	return p, nil
}

// Addr returns the proxy's address.
func (p *Proxy) Addr() string { return p.listener.Addr().String() }

// Close stops the proxy and every connection through it.
func (p *Proxy) Close() {
	p.mu.Lock()
	p.done = true
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	p.listener.Close()
	p.wg.Wait()
}

func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		c.Close()
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *Proxy) accept() {
	defer p.wg.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		fault, after := Fault(p.rng.IntN(int(faultCount))), p.rng.IntN(800)
		p.mu.Unlock()
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handle(client, fault, after)
		}()
	}
}

func reset(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		_ = t.SetLinger(0)
	}
	c.Close()
}

func (p *Proxy) handle(client net.Conn, fault Fault, after int) {
	if !p.track(client) {
		return
	}
	defer client.Close()
	if fault == ResetEarly {
		reset(client)
		return
	}
	upstream, err := net.DialTimeout("tcp", p.target, 2*time.Second)
	if err != nil {
		reset(client)
		return
	}
	if !p.track(upstream) {
		return
	}
	defer upstream.Close()

	go func() {
		_, _ = io.Copy(upstream, client)
		// The client is gone, so end the upstream side too. A stalled copy below
		// waits on this, which is what lets a stall finish.
		upstream.Close()
	}()

	switch fault {
	case ResetAfter, CloseAfter, StallAfter:
		_, _ = io.CopyN(client, upstream, int64(after))
		switch fault {
		case ResetAfter:
			reset(client)
		case StallAfter:
			_, _ = io.Copy(io.Discard, upstream) // the provider keeps going; the client hears nothing
		}
	case Fragmented:
		buf := make([]byte, 16)
		for {
			n, err := upstream.Read(buf)
			if n > 0 {
				if _, werr := client.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	default:
		_, _ = io.Copy(client, upstream)
	}
}
