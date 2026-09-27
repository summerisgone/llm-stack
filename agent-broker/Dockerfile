FROM golang:1.26.4-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/agent-broker ./cmd/agent-broker

FROM alpine:3.22
RUN adduser -D -H -u 10002 app
USER 10002
COPY --from=build /out/agent-broker /usr/local/bin/agent-broker
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/agent-broker"]
