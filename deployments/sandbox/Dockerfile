FROM golang@sha256:622e56dbc11a8cfe87cafa2331e9a201877271cbff918af53d3be315f3da88cc

WORKDIR /build
COPY cmd/sandbox-helper/main.go /build/main.go
RUN GOTOOLCHAIN=local CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /usr/local/bin/agentdock-sandbox-helper /build/main.go \
    && rm -rf /build

WORKDIR /workspace
