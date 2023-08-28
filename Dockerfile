FROM alpine:3.17 as base
RUN apk update && apk upgrade && \
    apk add ca-certificates && \
    apk --no-cache add tzdata

FROM scratch as log-ingestor
COPY --from=base /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=base /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY dist/log-ingestor /bin/log-ingestor

ENTRYPOINT [ "/bin/log-ingestor" ]

