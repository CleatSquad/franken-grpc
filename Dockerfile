# Stage 1: build the static Go binary.
FROM golang:1.23-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o /usr/local/bin/franken-grpc .

# Stage 2: minimal runtime image.
FROM alpine:3.20

RUN apk add --no-cache ca-certificates

COPY --from=builder /usr/local/bin/franken-grpc /usr/local/bin/franken-grpc

EXPOSE 9090

ENTRYPOINT ["/usr/local/bin/franken-grpc"]
