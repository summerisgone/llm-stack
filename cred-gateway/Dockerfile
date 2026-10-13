FROM golang:1.26.4-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/cred-gateway ./cmd/cred-gateway

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -H -u 10003 app
USER 10003
COPY --from=build /out/cred-gateway /usr/local/bin/cred-gateway
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/cred-gateway"]
