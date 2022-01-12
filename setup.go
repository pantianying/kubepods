package kubepods

import (
	"context"
	"fmt"
	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/kubernetes/object"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	core "k8s.io/api/core/v1"
	coreapi "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"strconv"
)

const pluginName = "kubepods"

var log = clog.NewWithPlugin(pluginName)

func init() { plugin.Register(pluginName, setup) }

func setup(c *caddy.Controller) error {
	k, err := parse(c)
	if err != nil {
		return plugin.Error(pluginName, err)
	}

	onStart, onShut, err := k.InitAPIConn(context.Background())
	if err != nil {
		return plugin.Error(pluginName, err)
	}
	if onStart != nil {
		c.OnStartup(onStart)
	}
	if onShut != nil {
		c.OnShutdown(onShut)
	}

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		k.Next = next
		return k
	})

	return nil
}

func parse(c *caddy.Controller) (*KubePods, error) {
	var (
		kns *KubePods
		err error
	)

	i := 0
	for c.Next() {
		if i > 0 {
			return nil, plugin.ErrOnce
		}
		i++

		kns, err = parseStanza(c)
		if err != nil {
			return kns, err
		}
	}
	return kns, nil
}

// parseStanza parses a kubenodes stanza
func parseStanza(c *caddy.Controller) (*KubePods, error) {
	kns := New(plugin.OriginsFromArgsOrServerBlock(c.RemainingArgs(), c.ServerBlockKeys))

	for c.NextBlock() {
		switch c.Val() {
		case "external":
			kns.ipType = core.NodeExternalIP
			kns.dnsType = core.NodeExternalDNS
		case "endpoint":
			args := c.RemainingArgs()
			if len(args) != 1 {
				return nil, c.ArgErr()
			}
			kns.APIServer = args[0]
		case "tls": // cert key ca
			args := c.RemainingArgs()
			if len(args) == 3 {
				kns.APIClientCert, kns.APIClientKey, kns.APICertAuth = args[0], args[1], args[2]
				continue
			}
			return nil, c.ArgErr()
		case "fallthrough":
			kns.Fall.SetZonesFromArgs(c.RemainingArgs())
		case "ttl":
			args := c.RemainingArgs()
			if len(args) == 0 {
				return nil, c.ArgErr()
			}
			t, err := strconv.Atoi(args[0])
			if err != nil {
				return nil, err
			}
			if t < 0 || t > 3600 {
				return nil, c.Errf("ttl must be in range [0, 3600]: %d", t)
			}
			kns.ttl = uint32(t)
		case "kubeconfig":
			args := c.RemainingArgs()
			if len(args) != 1 && len(args) != 2 {
				return nil, c.ArgErr()
			}
			overrides := &clientcmd.ConfigOverrides{}
			if len(args) == 2 {
				overrides.CurrentContext = args[1]
			}
			config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
				&clientcmd.ClientConfigLoadingRules{ExplicitPath: args[0]},
				overrides,
			)
			kns.ClientConfig = config
		case "kubetoken":
			args := c.RemainingArgs()
			if len(args) == 0 {
				return nil, c.ArgErr()
			}
			kns.APIClientToken = args[0]
		default:
			return nil, c.Errf("unknown property '%s'", c.Val())
		}
	}

	return kns, nil
}

func (k *KubePods) getClientConfig() (*rest.Config, error) {
	if k.ClientConfig != nil {
		return k.ClientConfig.ClientConfig()
	}
	loadingRules := &clientcmd.ClientConfigLoadingRules{}
	overrides := &clientcmd.ConfigOverrides{}
	clusterinfo := api.Cluster{}
	authinfo := api.AuthInfo{}

	// Connect to API from in cluster
	if k.APIServer == "" {
		cc, err := rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
		cc.ContentType = "application/vnd.kubernetes.protobuf"
		return cc, err
	}

	// Connect to API from out of cluster
	clusterinfo.Server = k.APIServer

	if len(k.APICertAuth) > 0 {
		clusterinfo.CertificateAuthority = k.APICertAuth
	}
	if len(k.APIClientCert) > 0 {
		authinfo.ClientCertificate = k.APIClientCert
	}
	if len(k.APIClientKey) > 0 {
		authinfo.ClientKey = k.APIClientKey
	}

	overrides.ClusterInfo = clusterinfo
	overrides.AuthInfo = authinfo
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)

	cc, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, err
	}
	cc.ContentType = "application/vnd.kubernetes.protobuf"
	if k.APIClientToken != "" {
		cc.BearerToken = k.APIClientToken
	}
	return cc, err

}

// InitAPIConn initializes a new KubeNodes connection.
func (k *KubePods) InitAPIConn(ctx context.Context) (onStart func() error, onShut func() error, err error) {
	if k.client == nil {
		config, err := k.getClientConfig()
		if err != nil {
			return nil, nil, err
		}

		kubeClient, err := kubernetes.NewForConfig(config)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create kubernetes notification controller: %q", err)
		}
		k.client = kubeClient
	}
	k.svcIndexer, k.svcController = cache.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc:  serviceListFunc(ctx, k.client, coreapi.NamespaceAll, nil),
			WatchFunc: serviceWatchFunc(ctx, k.client, coreapi.NamespaceAll, nil),
		},
		&core.Service{},
		0,
		cache.ResourceEventHandlerFuncs{},
		cache.Indexers{},
	)

	k.endpointIndex, k.endpointController = object.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc:  endpointListFunc(ctx, k.client, coreapi.NamespaceAll, nil),
			WatchFunc: endpointWatchFunc(ctx, k.client, coreapi.NamespaceAll, nil),
		},
		&core.Endpoints{},
		cache.ResourceEventHandlerFuncs{},
		cache.Indexers{epNameNamespaceIndex: epNameNamespaceIndexFunc, epIPIndex: epIPIndexFunc},
		object.DefaultProcessor(object.ToEndpoints, nil),
	)

	onStart = func() error {
		go k.svcController.Run(k.stopCh)
		go k.endpointController.Run(k.stopCh)
		return nil
	}

	onShut = func() error {
		k.stopLock.Lock()
		defer k.stopLock.Unlock()
		if !k.shutdown {
			close(k.stopCh)
			k.shutdown = true
			return nil
		}
		return fmt.Errorf("shutdown already in progress")
	}

	return onStart, onShut, err
}

func serviceListFunc(ctx context.Context, c kubernetes.Interface, ns string, s labels.Selector) func(meta.ListOptions) (runtime.Object, error) {
	return func(opts meta.ListOptions) (runtime.Object, error) {
		if s != nil {
			opts.LabelSelector = s.String()
		}
		listV1, err := c.CoreV1().Services(ns).List(ctx, opts)
		return listV1, err
	}
}

func serviceWatchFunc(ctx context.Context, c kubernetes.Interface, ns string, s labels.Selector) func(options meta.ListOptions) (watch.Interface, error) {
	return func(options meta.ListOptions) (watch.Interface, error) {
		if s != nil {
			options.LabelSelector = s.String()
		}
		w, err := c.CoreV1().Services(ns).Watch(ctx, options)
		return w, err
	}
}

func endpointListFunc(ctx context.Context, c kubernetes.Interface, ns string, s labels.Selector) func(meta.ListOptions) (runtime.Object, error) {
	return func(opts meta.ListOptions) (runtime.Object, error) {
		if s != nil {
			opts.LabelSelector = s.String()
		}
		listV1, err := c.CoreV1().Endpoints(ns).List(ctx, opts)
		return listV1, err
	}
}

func endpointWatchFunc(ctx context.Context, c kubernetes.Interface, ns string, s labels.Selector) func(options meta.ListOptions) (watch.Interface, error) {
	return func(options meta.ListOptions) (watch.Interface, error) {
		if s != nil {
			options.LabelSelector = s.String()
		}
		w, err := c.CoreV1().Endpoints(ns).Watch(ctx, options)
		return w, err
	}
}
