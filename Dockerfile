# The Go binary is built by CircleCI (architect/go-build) and attached to the
# build context as <binary>-<os>-<arch>; this image only assembles the runtime.
# For a local build, produce the binary first:
#   CGO_ENABLED=0 go build -o klaus-gateway-linux-amd64 .
FROM gsoci.azurecr.io/giantswarm/alpine:3.24.1

RUN apk add --no-cache ca-certificates

ARG TARGETOS
ARG TARGETARCH
COPY klaus-gateway-${TARGETOS}-${TARGETARCH} /klaus-gateway

USER 65532:65532

ENTRYPOINT ["/klaus-gateway"]
