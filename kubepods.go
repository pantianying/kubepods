package kubepods

import (
	"context"
	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/kubernetes/object"
	"github.com/coredns/coredns/plugin/pkg/dnsutil"
	"github.com/coredns/coredns/plugin/pkg/fall"
	"github.com/coredns/coredns/plugin/pkg/upstream"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
	core "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"net"
	"strings"
	"sync"
	"time"
)

type KubePods struct {
	Next     plugin.Handler
	Zones    []string
	Upstream upstreamer

	APIServer       string
	APICertAuth     string
	APIClientCert   string
	APIClientKey    string
	APIClientToken  string
	ClientConfig    clientcmd.ClientConfig
	Fall            fall.F
	ttl             uint32
	ipType, dnsType core.NodeAddressType

	// Kubernetes API interface
	client        kubernetes.Interface
	svcController cache.Controller
	svcIndexer    cache.Indexer

	endpointController cache.Controller
	endpointIndex      cache.Indexer

	// concurrency control to stop controller
	stopLock sync.Mutex
	shutdown bool
	stopCh   chan struct{}
}

type upstreamer interface {
	Lookup(ctx context.Context, state request.Request, name string, typ uint16) (*dns.Msg, error)
}

// New returns a initialized KubeNodes.
func New(zones []string) *KubePods {
	k := new(KubePods)
	k.Zones = zones
	k.Upstream = upstream.New()
	k.ttl = defaultTTL
	k.stopCh = make(chan struct{})
	k.ipType = core.NodeInternalIP
	k.dnsType = core.NodeInternalDNS
	return k
}

const (
	// defaultTTL to apply to all answers.
	defaultTTL = 5
)

// Name implements the Handler interface.
func (k KubePods) Name() string { return pluginName }

// Ready implements the ready.Readiness interface.
func (k *KubePods) Ready() bool {
	return k.svcController.HasSynced() && k.endpointController.HasSynced()
}

// ServeDNS implements the plugin.Handler interface.
func (k KubePods) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}

	qname := state.Name()
	zone := plugin.Zones(k.Zones).Matches(qname)
	if zone == "" || !supportedQtype(state.QType()) {
		return plugin.NextOrFailure(k.Name(), k.Next, ctx, w, r)
	}
	zone = state.QName()[len(qname)-len(zone):] // maintain case of original query
	state.Zone = zone

	if len(zone) == len(qname) {
		writeResponse(w, r, nil, nil, []dns.RR{k.soa()}, dns.RcodeSuccess)
		return dns.RcodeSuccess, nil
	}

	name := state.Name()[0 : len(qname)-len(zone)-1]
	if zone == "." {
		name = state.Name()[0 : len(qname)-len(zone)]
	}
	ss := strings.Split(state.Name(), ".")
	if len(ss) < 2 {
		return dns.RcodeServerFailure, nil
	}

	epKey := object.EndpointsKey(name, ss[1])

	log.Infof("zone:%v , epKey:%v , QType:%v", zone, epKey, state.QType())

	// get the node by key name from the indexer
	item, err := k.endpointIndex.ByIndex(epNameNamespaceIndex, epKey)
	if err != nil {
		return dns.RcodeServerFailure, err
	}
	if len(item) == 0 {
		if k.Fall.Through(state.Name()) {
			return plugin.NextOrFailure(k.Name(), k.Next, ctx, w, r)
		}
		writeResponse(w, r, nil, nil, []dns.RR{k.soa()}, dns.RcodeNameError)
		return dns.RcodeNameError, nil
	}

	// extract IPs from the node
	var ips []string

	for _, o := range item {
		e, ok := o.(*object.Endpoints)
		if !ok {
			continue
		}
		for _, eps := range e.Subsets {
			for _, addr := range eps.Addresses {
				ips = append(ips, addr.IP)
			}
		}
	}

	// build response records
	var records []dns.RR
	if state.QType() == dns.TypeA {
		for _, ip := range ips {
			if strings.Contains(ip, ":") {
				continue
			}
			if netIP := net.ParseIP(ip); netIP != nil {
				records = append(records, &dns.A{A: netIP,
					Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: k.ttl}})
			}
		}
	}
	if state.QType() == dns.TypeAAAA {
		for _, ip := range ips {
			if !strings.Contains(ip, ":") {
				continue
			}
			if netIP := net.ParseIP(ip); netIP != nil {
				records = append(records, &dns.AAAA{AAAA: netIP,
					Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: k.ttl}})
			}
		}
	}

	writeResponse(w, r, records, nil, nil, dns.RcodeSuccess)
	return dns.RcodeSuccess, nil
}

func supportedQtype(qtype uint16) bool {
	switch qtype {
	case dns.TypeA, dns.TypeAAAA, dns.TypePTR:
		return true
	default:
		log.Info("no support Qtype:", qtype)
		return false
	}
}

func (k KubePods) soa() *dns.SOA {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: k.Zones[0], Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: k.ttl},
		Ns:      dnsutil.Join("ns.dns", k.Zones[0]),
		Mbox:    dnsutil.Join("hostmaster.dns", k.Zones[0]),
		Serial:  uint32(time.Now().Unix()),
		Refresh: 7200,
		Retry:   1800,
		Expire:  86400,
		Minttl:  k.ttl,
	}
}

func writeResponse(w dns.ResponseWriter, r *dns.Msg, answer, extra, ns []dns.RR, rcode int) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Rcode = rcode
	m.Authoritative = true
	m.Answer = answer
	m.Extra = extra
	m.Ns = ns
	w.WriteMsg(m)
}
