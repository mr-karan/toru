# Build stage
FROM ubuntu:24.04
ENV GO111MODULE=on
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends curl unzip \
    ca-certificates tzdata make \
    git wget \
    && apt-get clean && rm -fr /var/lib/apt/lists/*

# Install Go 1.22.5
RUN wget https://go.dev/dl/go1.22.5.linux-amd64.tar.gz
RUN tar -xvf go1.22.5.linux-amd64.tar.gz
RUN mv go /usr/local
ENV PATH $PATH:/usr/local/go/bin

COPY toru.bin /bin/toru
COPY config.sample.toml /config/config.toml

EXPOSE 8888

CMD ["/bin/toru", "--config=/config/config.toml"]
