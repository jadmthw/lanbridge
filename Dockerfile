# Relay server image:
#   docker build -t lanbridge-relay .
#   docker run -d --restart=always -p 7777:7777 -e LANBRIDGE_RELAY_TOKEN=pick-a-secret lanbridge-relay
FROM golang:alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(cat VERSION)" -o /out/lanbridge ./cmd/lanbridge

FROM scratch
COPY --from=build /out/lanbridge /lanbridge
USER 65534:65534
EXPOSE 7777
ENTRYPOINT ["/lanbridge", "relay"]
