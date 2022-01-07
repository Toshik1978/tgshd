.PHONY: code.generate code.deps quality.lint quality.tests app.build app.run clean
.DEFAULT_GOAL := all

all: quality.lint quality.tests app.build

code.generate:
	@echo "+ $@"
	go generate ./...

code.deps:
	@echo "+ $@"
	go mod download

quality.lint:
	@echo "+ $@"
	./.scripts/quality.lint.sh

quality.tests:
	@echo "+ $@"
	go test ./...

app.build:
	@echo "+ $@"
	rm -rf bin/server-bot
	GOGC=off go build -v -ldflags "\
		-X main.Buildstamp=$(shell date +%Y/%m/%d_%H:%M:%S) \
		-X main.Commit=$(shell git rev-parse --short HEAD) \
	" -o bin/server-bot cmd/main.go

app.run: app.build
	@echo "+ $@"
	bin/server-bot

clean:
	@echo "+ $@"
	go clean -testcache
	rm -rf bin/server-bot
