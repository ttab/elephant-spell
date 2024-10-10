FROM golang:1.23.2-alpine3.20 AS build

WORKDIR /usr/src

ADD . ./

ARG TARGETOS TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -o /build/spell ./cmd/spell

FROM alpine:3.20

COPY --from=build /build/spell /usr/local/bin/spell

RUN apk upgrade --no-cache \
    && apk add tzdata

# API server
EXPOSE 1080

# Debug/profiling server
EXPOSE 1081

ENTRYPOINT ["spell", "run"]
