#!/usr/bin/env bash

go test -coverprofile=coverage.txt -covermode count ./...
# Generate cobertura report
go install github.com/boumenot/gocover-cobertura@latest
gocover-cobertura < coverage.txt > coverage.xml
# Generate total coverage for the badge
go tool cover -func coverage.txt
