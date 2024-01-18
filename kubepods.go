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

	APIServer           string
	APICertAuth         string
	APIClientCert       string
	APIClientKey        string
	APIClientToken      string
	ClientConfig        clientcmd.ClientConfig
	Fall                fall.F
	ttl                 uint32
	DefaultDNSNamespace string
	ipType, dnsType     core.NodeAddressType

	// Kubernetes API interface
	client        kubernetes.Interface
	svcController cache.Controller
	svcIndex      cache.Indexer

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
	log.Info("svcController ", k.svcController.HasSynced())
	log.Info("endpointController ", k.endpointController.HasSynced())
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
	ss := strings.Split(name, ".")
	var svcName, svcNamespace string
	if len(ss) == 2 {
		svcName, svcNamespace = ss[0], ss[1]
	} else if len(ss) == 1 {
		svcName, svcNamespace = ss[0], k.DefaultDNSNamespace
	} else {
		log.Warning("err when get svcName/svcNamespace")
		return dns.RcodeServerFailure, nil
	}

	svcKey := object.ServiceKey(svcName, svcNamespace)
	svcs, err := k.svcIndex.ByIndex(svcNameNamespaceIndex, svcKey)
	if err != nil {
		log.Warning("found svc err", err)
		return dns.RcodeServerFailure, err
	}
	// when no svc found , try DefaultDNSNamespace again
	if len(svcs) < 1 {
		if svcNamespace != k.DefaultDNSNamespace && strings.HasPrefix(svcNamespace, "alter-") {
			svcKey = object.ServiceKey(svcName, k.DefaultDNSNamespace)
			svcs, err = k.svcIndex.ByIndex(svcNameNamespaceIndex, svcKey)
			if err != nil {
				log.Warning("found svc err", err)
				return dns.RcodeServerFailure, err
			}
			if len(svcs) < 1 {
				log.Warning("found no svc :", svcKey)
				return dns.RcodeServerFailure, nil
			}
			svcNamespace = k.DefaultDNSNamespace
		} else {
			log.Warning("found no svc :", svcKey)
			return dns.RcodeServerFailure, nil
		}
	}
	if len(svcs) > 1 {
		log.Warningf("found svc count > 1")
		return dns.RcodeServerFailure, nil
	}

	svc := svcs[0].(*object.Service)
	var ips []string

	switch svc.Type {
	case core.ServiceTypeClusterIP:
		// get the node by key name from the indexer
		epKey := object.EndpointsKey(svcName, svcNamespace)
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
		log.Infof("zone:{%v} key:{%v} svcNamespace:{%v} QType:{%v} , ip:[%v]", zone, name, svcNamespace, state.QType(), ips)
		writeResponse(w, r, records, nil, nil, dns.RcodeSuccess)
	default:
		log.Infof("the type of service {%v} is not supported ", svc.Type)
		return dns.RcodeServerFailure, nil
	}

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
