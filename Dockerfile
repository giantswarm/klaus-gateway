FROM --platform=$BUILDPLATFORM golang:1.26.2 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-w -extldflags '-static'" \
    -o klaus-gateway ./cmd/klaus-gateway

FROM gsoci.azurecr.io/giantswarm/alpine:3.23.4

RUN apk add --no-cache ca-certificates

COPY --from=builder /app/klaus-gateway /klaus-gateway

USER 65532:65532

ENTRYPOINT ["/klaus-gateway"]
