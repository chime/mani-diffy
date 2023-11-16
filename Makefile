test:
		go test ./...

test-verbose:
		go test -v ./...

benchmark:
		go test -bench=.

benchmark-all:
		go test ./... -bench=.

lint:
		golangci-lint run

build-binaries:
	  go build -o mani-diffy
