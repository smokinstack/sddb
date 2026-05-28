FROM golang:alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o /bin/sddb-dashboard ./cmd/dashboard

FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=builder /bin/sddb-dashboard /usr/local/bin/sddb-dashboard
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/sddb-dashboard"]
CMD ["-data-dir", "/data"]
