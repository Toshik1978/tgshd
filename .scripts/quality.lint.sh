#!/usr/bin/env bash

GOLANGCI_LINT_VERSION="2.11.3"
GOLANGCI_LINT_PATH="$(command -v golangci-lint)"

if ! [ -x "$GOLANGCI_LINT_PATH" ]; then
  curl -sfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b "$(go env GOPATH)"/bin v$GOLANGCI_LINT_VERSION
  GOLANGCI_LINT_PATH="$(go env GOPATH)"/bin/golangci-lint
fi

"$GOLANGCI_LINT_PATH" run ./...
