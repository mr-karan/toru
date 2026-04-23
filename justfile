set shell := ["bash", "-cu"]

default:
    @just --list

build:
    @mkdir -p bin
    @last_commit=$(git rev-parse --short HEAD); \
      last_commit_date=$(git show -s --format=%ci $last_commit); \
      version=$(git describe --tags --always); \
      buildstr="$version (Commit: $last_commit_date ($last_commit), Build: $(date +'%Y-%m-%d %H:%M:%S %z'))"; \
      CGO_ENABLED=0 go build -o ./bin/toru -ldflags="-X 'main.buildString=$buildstr'" ./

run: build
    ./bin/toru --config config.toml

lint:
    golangci-lint run

test:
    go test ./...

dist: build
    mkdir -p dist
    cp -R ./bin/toru ./dist/
    cp -R ./config.toml ./dist/

clean:
    rm -rf ./bin/toru dist
