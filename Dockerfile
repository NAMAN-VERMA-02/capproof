# Runs the mock provider and the test runner. Holds no secrets.
FROM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /capproof ./cmd/capproof

FROM alpine:3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
RUN apk add --no-cache curl jq
COPY --from=build /capproof /usr/local/bin/capproof
USER nobody
ENTRYPOINT ["capproof"]
