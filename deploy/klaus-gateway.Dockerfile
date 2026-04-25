# Dockerfile used by the compose smoke harness only.
#
# The production image (`../Dockerfile`) pulls its runtime base from
# `gsoci.azurecr.io`, which requires giantswarm credentials. This variant
# uses the public Docker Hub Alpine so the compose build works on any
# CI runner (CircleCI machine executors included).
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26-bookworm AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
      -ldflags "-w -extldflags '-static'" \
      -o klaus-gateway ./cmd/klaus-gateway

FROM docker.io/library/alpine:3.23
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/klaus-gateway /klaus-gateway
USER 65532:65532
ENTRYPOINT ["/klaus-gateway"]
