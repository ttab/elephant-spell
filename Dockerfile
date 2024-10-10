FROM golang:1.23.2-bookworm AS build

WORKDIR /usr/src

RUN apt-get update && apt-get upgrade -y && \
    apt-get install -y build-essential libhunspell-dev && \
    rm -rf /var/lib/apt/lists/*

ADD . ./

ARG TARGETOS TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -o /build/spell ./cmd/spell

FROM debian:bookworm-slim

RUN apt-get update && apt-get upgrade -y && \
    apt-get install -y libhunspell-1.7-0 && \
    rm -rf /var/lib/apt/lists/*

COPY --from=build /build/spell /usr/local/bin/spell

# API server
EXPOSE 1080

# Debug/profiling server
EXPOSE 1081

ENTRYPOINT ["spell", "run"]
