test:
		go test -v -cover -short ./...

benchmark:
		go test -bench=.

benchmark-all:
		go test ./... -bench=.

lint:
		golangci-lint run

build-binaries:
	  go build -o mani-diffy

coverage:
	go test -v -coverprofile=coverage.out -short ./... && go tool cover -html=coverage.out
