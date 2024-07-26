# Build stage
FROM golang:1.22 AS builder

# Ubuntu
FROM ubuntu:24.04
ENV GO111MODULE=on

RUN apt-get update && apt-get install -y ca-certificates

COPY --from=builder /usr/local/go/bin/go /bin/go
COPY toru.bin /bin/toru
COPY config.sample.toml /config/config.toml

EXPOSE 8888

CMD ["/bin/toru", "--config=/config/config.toml"]
