package kubepods

import (
	"errors"
	"github.com/coredns/coredns/plugin/kubernetes/object"
)

const (
	podIPIndex            = "PodIP"
	svcNameNamespaceIndex = "SvcNameNamespace"
	svcIPIndex            = "ServiceIP"
	epNameNamespaceIndex  = "EndpointNameNamespace"
	epIPIndex             = "EndpointsIP"
)

var errObj = errors.New("obj was not of the correct type")

func epNameNamespaceIndexFunc(obj interface{}) ([]string, error) {
	s, ok := obj.(*object.Endpoints)
	if !ok {
		return nil, errObj
	}
	return []string{s.Index}, nil
}

func svcNameNamespaceIndexFunc(obj interface{}) ([]string, error) {
	s, ok := obj.(*object.Service)
	if !ok {
		return nil, errObj
	}
	return []string{s.Index}, nil
}

func epIPIndexFunc(obj interface{}) ([]string, error) {
	ep, ok := obj.(*object.Endpoints)
	if !ok {
		return nil, errObj
	}
	return ep.IndexIP, nil
}

func svcIPIndexFunc(obj interface{}) ([]string, error) {
	svc, ok := obj.(*object.Service)
	if !ok {
		return nil, errObj
	}
	if len(svc.ExternalIPs) == 0 {
		return svc.ClusterIPs, nil
	}

	return append(svc.ClusterIPs, svc.ExternalIPs...), nil
}

func (k *KubePods) EpIndex(idx string) (ep []*object.Endpoints) {
	os, err := k.endpointIndex.ByIndex(epNameNamespaceIndex, idx)
	if err != nil {
		return nil
	}
	for _, o := range os {
		e, ok := o.(*object.Endpoints)
		if !ok {
			continue
		}
		ep = append(ep, e)
	}
	return ep
}
