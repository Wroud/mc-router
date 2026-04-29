.PHONY: test
test:
	go test ./...

.PHONY: snapshot
snapshot:
	goreleaser release --snapshot --clean --skip=publish
