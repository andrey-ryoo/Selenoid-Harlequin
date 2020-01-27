FROM golang:buster AS build

COPY . /go/src/selenoid
WORKDIR /go/src/selenoid
ENV GO111MODULE=on GOOS=linux GOARCH=amd64 CGO_ENABLED=0

RUN go get && rm /go/pkg/mod/github.com/coreos/etcd@v3.3.10+incompatible/client/keys.generated.go && go build

FROM ubuntu:18.04

RUN echo $TZ > /etc/timezone && apt-get update && apt-get install -y curl tzdata ca-certificates && \
    rm /etc/localtime && \
    ln -snf /usr/share/zoneinfo/$TZ /etc/localtime && \
    dpkg-reconfigure -f noninteractive tzdata && \
    apt-get clean

WORKDIR /reactive-selenoid

COPY --from=build /go/src/selenoid/ /reactive-selenoid

EXPOSE 4444
ENTRYPOINT ["./selenoid"]