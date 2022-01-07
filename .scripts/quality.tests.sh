#!/usr/bin/env bash

if ! [ -x "$(command -v gocover-cobertura)" ]; then
  go install github.com/t-yuki/gocover-cobertura@latest
fi
if ! [ -x "$(command -v gocov)" ]; then
  go install github.com/axw/gocov/gocov@latest
fi

go test ./... -coverprofile=coverage.txt -covermode count
# Generate cobertura report
gocover-cobertura < coverage.txt > coverage.xml
# Generate total coverage for the badge
gocov convert coverage.txt | gocov report
