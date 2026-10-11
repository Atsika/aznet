# A plain Linux machine on the lab network: MIT Kerberos tools and nothing else.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends krb5-user ca-certificates \
    && rm -rf /var/lib/apt/lists/*
