FROM golang:1.26.1-bookworm AS go-runtime

FROM debian:bookworm-slim

ENV GO111MODULE=on \
    DEBIAN_FRONTEND=noninteractive \
    PATH=/usr/local/go/bin:${PATH}

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    git \
    tzdata \
    && rm -rf /var/lib/apt/lists/*

COPY --from=go-runtime /usr/local/go /usr/local/go
COPY toru.bin /usr/local/bin/toru
COPY config.sample.toml /config/config.toml

EXPOSE 8888

ENTRYPOINT ["/usr/local/bin/toru"]
CMD ["--config=/config/config.toml"]
