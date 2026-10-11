FROM ubuntu/squid:latest
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      samba winbind krb5-user smbclient dnsutils libnss-winbind \
    && rm -rf /var/lib/apt/lists/*
COPY entrypoint.sh /lab-entrypoint.sh
RUN chmod +x /lab-entrypoint.sh
ENTRYPOINT ["/lab-entrypoint.sh"]
