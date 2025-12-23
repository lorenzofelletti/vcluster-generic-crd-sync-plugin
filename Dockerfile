# Build the manager binary
FROM golang:1.25 AS builder

ARG TARGETOS
ARG TARGETARCH

# Make sure we use go modules
WORKDIR /vcluster

COPY go.mod go.sum /vcluster/

RUN go mod download

# Copy the Go Modules manifests
COPY . .

# Build cmd
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GO111MODULE=on go build -o /plugin main.go

# we use alpine for easier debugging
FROM alpine

# Set root path as working directory
WORKDIR /

# if it does not work, create a /plugin dir and copy to /plugin/plugin
COPY --from=builder /plugin /plugin
