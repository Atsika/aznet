#!/bin/bash
# A Squid that accepts Kerberos tickets for a real Active Directory using only
# a keytab, as a proxy appliance configured by an admin would. It is not joined
# to the domain and never contacts the domain controller: the service ticket is
# decrypted locally with the key in the keytab.
set -eu
REALM="${AD_REALM:-ECORP.LOCAL}"
SPN="${AD_SPN:-HTTP/proxy.ecorp.local}"

cat > /etc/krb5.conf <<KRB
[libdefaults]
  default_realm = $REALM
  dns_lookup_kdc = false
  rdns = false
KRB
install -o proxy -g proxy -m 0400 /ad/proxy.keytab /etc/squid/proxy.keytab
klist -ke /etc/squid/proxy.keytab

cat > /etc/squid/squid.conf <<CONF
auth_param negotiate program /usr/lib/squid/negotiate_kerberos_auth -s $SPN@$REALM -k /etc/squid/proxy.keytab
auth_param negotiate children 10 startup=2 idle=1
auth_param negotiate keep_alive on
acl authenticated proxy_auth REQUIRED
acl SSL_ports port 443 10000 10001 10002
acl CONNECT method CONNECT
http_access deny CONNECT !SSL_ports
http_access allow authenticated
http_access deny all
http_port 3128
visible_hostname proxy
pid_filename /run/squid.pid
cache deny all
access_log stdio:/var/log/squid/access.log
cache_log /var/log/squid/cache.log
CONF
mkdir -p /var/log/squid && chown proxy:proxy /var/log/squid
touch /var/log/squid/access.log /var/log/squid/cache.log && chown proxy:proxy /var/log/squid/*.log
tail -F /var/log/squid/access.log /var/log/squid/cache.log &
exec squid -N -f /etc/squid/squid.conf
