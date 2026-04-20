FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod .
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/klaus-instance-stub ./...

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/klaus-instance-stub /klaus-instance-stub
EXPOSE 8080
ENTRYPOINT ["/klaus-instance-stub"]
