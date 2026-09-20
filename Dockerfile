FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /out/veeam-vb365-exporter .

FROM alpine:3.22

RUN apk add --no-cache ca-certificates && \
    addgroup -S exporter && \
    adduser -S -G exporter exporter

COPY --from=builder /out/veeam-vb365-exporter /usr/local/bin/veeam-vb365-exporter

USER exporter

EXPOSE 9810

ENTRYPOINT ["/usr/local/bin/veeam-vb365-exporter"]
