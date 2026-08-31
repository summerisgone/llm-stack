FROM golang:1.26.4-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/pat-service ./cmd/pat-service

FROM alpine:3.22
RUN adduser -D -H -u 10001 app
USER app
COPY --from=build /out/pat-service /usr/local/bin/pat-service
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/pat-service"]
