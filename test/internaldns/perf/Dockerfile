# k6 load generator for internal DNS, built with the xk6-dns extension because
# k6 has no DNS protocol support of its own.
FROM golang:1.27@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS build
ARG XK6_VERSION=v1.4.14
ARG K6_VERSION=v2.3.0
ARG XK6_DNS_VERSION=v1.2.1
RUN go install go.k6.io/xk6/cmd/xk6@${XK6_VERSION}
WORKDIR /build
# xk6 takes the k6 version from the K6_VERSION build arg's environment.
RUN CGO_ENABLED=0 xk6 build \
      --with github.com/grafana/xk6-dns@${XK6_DNS_VERSION} \
      --output /build/k6

FROM alpine:3.22@sha256:d406446bc05b82480756ce8f1287964d28768c23955fa9480d2a74a2eae7a654
RUN apk add --no-cache bind-tools ca-certificates
COPY --from=build /build/k6 /usr/local/bin/k6
COPY dns.js entrypoint.sh /perf/
WORKDIR /perf
USER 65534
ENTRYPOINT ["/perf/entrypoint.sh"]
