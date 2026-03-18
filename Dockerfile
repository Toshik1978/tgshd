FROM golang:1.26-alpine3.23 AS builder

RUN apk add --no-cache git make

# Go modules
WORKDIR /app
COPY go.mod /app
COPY go.sum /app
COPY Makefile /app
RUN make code.deps

# Build app
COPY . /app
RUN make app.build

FROM alpine:3.23

WORKDIR /app

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/bin/server-bot .

CMD ["/app/server-bot"]
