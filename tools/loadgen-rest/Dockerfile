FROM alpine:3.20
# Tiny REST pinger for load testing the gateway's Mongo-backed endpoints.
WORKDIR /
COPY tools/loadgen-rest/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
