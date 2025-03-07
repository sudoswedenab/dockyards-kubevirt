FROM docker.io/library/golang:1.24.1 AS builder
COPY . /src
WORKDIR /src
ENV CGO_ENABLED=0
RUN go build -o dockyards-kubevirt -ldflags="-s -w"

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /src/dockyards-kubevirt /usr/bin/dockyards-kubevirt
ENTRYPOINT ["/usr/bin/dockyards-kubevirt"]
