# kubepods
coredns plugin for k8s svc to pod ip
Used for some special purposes
## how to build coredns with this plugin
```
git clone git@github.com:coredns/coredns.git
cd plugin
ln -s {kubepods_code_path} .
cd ..
vi plugin.cfg # add kubepods:kubepods
go generate
make
```

## how to start coredns with this plugin
```
./coredns -conf ./CoreFile
```

### CoreFile example
```
.:53 {
    ready
    kubepods dian-stable.svc.cluster.local svc.cluster.local {
       endpoint https://your-apiservice-url
       kubeconfig ~/.kube/config
       # kubetoken xxxxxxxx
    }
    log . "{proto} Request: {name} {type} {>id} {rcode}"
}
```
