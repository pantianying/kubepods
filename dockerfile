FROM quay.xiaodiankeji.net/grizzlybear/golang-runtime:latest
WORKDIR /
COPY  ./coredns .
COPY  ./CoreFile .
ENTRYPOINT ["/coredns","-conf","./CoreFile"]