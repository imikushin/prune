.PHONY: test
test:
	go test ./...

.PHONY: build
build: clean test
	go build -o ./bin/prune

.PHONY: clean
clean:
	rm -rf ./bin
