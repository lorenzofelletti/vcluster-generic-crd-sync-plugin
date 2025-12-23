# Modified by Lorenzo Felletti (2025) under Apache 2.0.
# Original code from vcluster-generic-crd-sync-plugin by Loft Labs.

FROM golang:1.25 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /vcluster

COPY go.mod go.sum /vcluster/

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GO111MODULE=on go build -o /plugin main.go

FROM alpine

WORKDIR /

RUN mkdir /plugin

COPY --from=builder /plugin /plugin/plugin
