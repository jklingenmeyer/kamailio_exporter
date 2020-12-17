FROM golang:alpine AS build-env
LABEL version="1.2"

RUN apk upgrade
RUN apk add --update --no-cache alpine-sdk git

RUN mkdir -p /go/src/github.com/jklingenmeyer/kamailio_exporter
WORKDIR /go/src/github.com/jklingenmeyer/kamailio_exporter
RUN git clone https://github.com/jklingenmeyer/kamailio_exporter.git .
RUN go mod tidy
RUN go build


FROM alpine:latest
LABEL version="1.2"
RUN apk add --no-cache ca-certificates
RUN addgroup -S kamailio_exporter && adduser -S -G kamailio_exporter kamailio_exporter
COPY --from=build-env --chown=kamailio_exporter:kamailio_exporter /go/src/github.com/jklingenmeyer/kamailio_exporter/kamailio_exporter /usr/local/bin/kamailio_exporter
USER kamailio_exporter
ENTRYPOINT ["/usr/local/bin/kamailio_exporter"]
