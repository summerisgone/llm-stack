FROM golang:1.26.4-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/hermes-broker ./cmd/hermes-broker

FROM alpine:3.22
RUN adduser -D -H -u 10002 app
USER 10002
COPY --from=build /out/hermes-broker /usr/local/bin/hermes-broker
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/hermes-broker"]
