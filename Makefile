.PHONY: build test vet docker-build

build:
	CGO_ENABLED=0 go build -ldflags="-w -s" -o bin/franken-grpc server.go

test:
	go test ./... -v

vet:
	go vet ./...

docker-build:
	docker build -t franken-grpc:dev .
