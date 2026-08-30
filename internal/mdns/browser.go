// Package mdns discovers BMC endpoints advertised over mDNS/DNS-SD.
package mdns

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/libp2p/zeroconf/v2"
)

// Endpoint is a BMC service instance discovered over mDNS.
type Endpoint struct {
	Instance string // mDNS instance name (unique on the network)
	Service  string // DNS-SD service type, e.g. "_redfish._tcp"
	Hostname string // mDNS hostname, e.g. "bmc.local."
	IP       netip.Addr
	Port     int
}

// Key identifies an endpoint across browse cycles.
func (e Endpoint) Key() string {
	return e.Service + "/" + e.Instance
}

// Browser emits discovered endpoints on a channel until the context is done.
type Browser interface {
	Run(ctx context.Context, events chan<- Endpoint) error
}

// ZeroconfBrowser browses a set of DNS-SD service types in cycles.
type ZeroconfBrowser struct {
	Log          logr.Logger
	ServiceTypes []string
	Domain       string
	Interval     time.Duration // time between browse cycles
	Window       time.Duration // duration of each browse cycle
}

// Run browses until ctx is done. Browse failures are logged, never fatal.
func (b *ZeroconfBrowser) Run(ctx context.Context, events chan<- Endpoint) error {
	for {
		b.browseOnce(ctx, events)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(b.Interval):
		}
	}
}

func (b *ZeroconfBrowser) browseOnce(ctx context.Context, events chan<- Endpoint) {
	bctx, cancel := context.WithTimeout(ctx, b.Window)
	defer cancel()

	var wg sync.WaitGroup
	for _, svc := range b.ServiceTypes {
		// zeroconf.Browse blocks until bctx is done and closes entries via
		// the mainloop; the forwarder must not close it (double close) and
		// must not rely on it being closed (Browse can fail before the
		// mainloop starts), hence the select on bctx.Done().
		entries := make(chan *zeroconf.ServiceEntry, 16)

		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case entry, ok := <-entries:
					if !ok {
						return
					}
					ep, ok := EntryToEndpoint(entry)
					if !ok {
						continue
					}
					select {
					case events <- ep:
					case <-bctx.Done():
						return
					}
				case <-bctx.Done():
					return
				}
			}
		}()

		wg.Add(1)
		go func(svc string) {
			defer wg.Done()
			if err := zeroconf.Browse(bctx, svc, b.Domain, entries); err != nil && ctx.Err() == nil {
				b.Log.Error(err, "mDNS browse failed", "service", svc)
			}
		}(svc)
	}
	wg.Wait()
}

// EntryToEndpoint converts a zeroconf entry, preferring IPv4 addresses.
// It returns false when the entry carries no address.
func EntryToEndpoint(entry *zeroconf.ServiceEntry) (Endpoint, bool) {
	ep := Endpoint{
		Instance: entry.Instance,
		Service:  entry.Service,
		Hostname: entry.HostName,
		Port:     entry.Port,
	}
	for _, ip := range append(append([]net.IP{}, entry.AddrIPv4...), entry.AddrIPv6...) {
		addr, ok := netip.AddrFromSlice(ip)
		if ok {
			ep.IP = addr.Unmap()
			return ep, true
		}
	}
	return Endpoint{}, false
}
