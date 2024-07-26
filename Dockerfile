# Build stage
FROM golang:1.22 AS builder
ENV GO111MODULE=on

RUN apt-get update && apt-get install -y ca-certificates

COPY toru.bin /bin/toru
COPY config.sample.toml /config/config.toml

EXPOSE 8888

CMD ["/bin/toru", "--config=/config/config.toml"]
