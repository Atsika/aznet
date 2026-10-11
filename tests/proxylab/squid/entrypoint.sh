#!/bin/bash
# A domain-joined Squid, like the proxy of a company that uses Active
# Directory. SCHEMES (comma separated, in the order offered) selects what it
# asks clients to authenticate with: negotiate, ntlm, basic.
set -eu
REALM=CORP.TEST
DOMAIN=CORP
ADMIN_PASS='Adm1n-Passw0rd!'
FQDN="$(hostname).corp.test"
SCHEMES="${SCHEMES:-negotiate,ntlm,basic}"
PORT="${PORT:-3128}"

cat > /etc/krb5.conf <<KRB
[libdefaults]
  default_realm = $REALM
  dns_lookup_kdc = false
  rdns = false
[realms]
  $REALM = {
    kdc = dc.corp.test
    admin_server = dc.corp.test
  }
[domain_realm]
  .corp.test = $REALM
KRB

cat > /etc/samba/smb.conf <<SMB
[global]
  workgroup = $DOMAIN
  realm = $REALM
  security = ads
  kerberos method = dedicated keytab
  dedicated keytab file = /etc/squid/proxy.keytab
  winbind use default domain = yes
  winbind offline logon = no
  idmap config * : backend = tdb
  idmap config * : range = 3000-7999
  log level = 1
SMB

echo "waiting for the domain controller..."
until getent hosts dc.corp.test >/dev/null && (exec 3<>/dev/tcp/dc.corp.test/88) 2>/dev/null; do sleep 2; done
until echo "$ADMIN_PASS" | kinit "administrator@$REALM" >/dev/null 2>&1; do sleep 2; done
net ads join -k -S dc.corp.test --no-dns-updates >/dev/null 2>&1 \
  || net ads join -U "administrator%$ADMIN_PASS" -S dc.corp.test --no-dns-updates
# HTTP/<fqdn> is the service name clients ask for.
if ! net ads setspn list -U "administrator%$ADMIN_PASS" -S dc.corp.test | grep -qi "HTTP/$FQDN"; then
  net ads setspn add "HTTP/$FQDN" -U "administrator%$ADMIN_PASS" -S dc.corp.test
fi
net ads keytab create -U "administrator%$ADMIN_PASS" -S dc.corp.test
net ads keytab add HTTP -U "administrator%$ADMIN_PASS" -S dc.corp.test
klist -k /etc/squid/proxy.keytab | head -8
chown proxy:proxy /etc/squid/proxy.keytab && chmod 640 /etc/squid/proxy.keytab

winbindd -D
sleep 2
chgrp proxy /var/lib/samba/winbindd_privileged && chmod 750 /var/lib/samba/winbindd_privileged

{
  for s in ${SCHEMES//,/ }; do
    case $s in
      negotiate)
        echo "auth_param negotiate program /usr/lib/squid/negotiate_kerberos_auth -s HTTP/$FQDN@$REALM -k /etc/squid/proxy.keytab"
        echo "auth_param negotiate children 10 startup=2 idle=1"
        echo "auth_param negotiate keep_alive on" ;;
      ntlm)
        echo "auth_param ntlm program /usr/bin/ntlm_auth --helper-protocol=squid-2.5-ntlmssp --domain=$DOMAIN"
        echo "auth_param ntlm children 10 startup=2 idle=1"
        echo "auth_param ntlm keep_alive on" ;;
      basic)
        echo "auth_param basic program /usr/bin/ntlm_auth --helper-protocol=squid-2.5-basic --domain=$DOMAIN"
        echo "auth_param basic children 10 startup=2 idle=1"
        echo "auth_param basic realm Corporate proxy"
        echo "auth_param basic credentialsttl 2 hours" ;;
    esac
  done
  cat <<CONF
acl authenticated proxy_auth REQUIRED
acl SSL_ports port 443 10000 10001 10002
acl CONNECT method CONNECT
http_access deny CONNECT !SSL_ports
http_access allow authenticated
http_access deny all
http_port $PORT
visible_hostname $(hostname)
pid_filename /run/squid.pid
cache deny all
access_log stdio:/var/log/squid/access.log
cache_log /var/log/squid/cache.log
CONF
} > /etc/squid/squid.conf

echo "squid $(hostname): offering $SCHEMES on $PORT"
mkdir -p /var/log/squid && chown proxy:proxy /var/log/squid
touch /var/log/squid/access.log /var/log/squid/cache.log && chown proxy:proxy /var/log/squid/*.log
tail -F /var/log/squid/access.log /var/log/squid/cache.log &
exec squid -N -f /etc/squid/squid.conf
