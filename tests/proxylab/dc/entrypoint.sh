#!/bin/bash
# Samba Active Directory domain controller for CORP.TEST: the KDC and the
# NTLM/Basic backend for the proxies. Provisioned once, kept in a volume.
set -eu
REALM=CORP.TEST
DOMAIN=CORP
ADMIN_PASS='Adm1n-Passw0rd!'
IP=$(hostname -i | awk '{print $1}')

if [ ! -f /var/lib/samba/private/sam.ldb ]; then
  rm -f /etc/samba/smb.conf
  samba-tool domain provision --use-rfc2307 --realm=$REALM --domain=$DOMAIN \
    --adminpass="$ADMIN_PASS" --server-role=dc --dns-backend=SAMBA_INTERNAL \
    --host-ip="$IP" --option="dns forwarder=8.8.8.8" >/dev/null
  cp /var/lib/samba/private/krb5.conf /etc/krb5.conf
  samba-tool domain passwordsettings set --complexity=off --min-pwd-length=1 \
    --min-pwd-age=0 --max-pwd-age=0 >/dev/null
  # alice and bob are ordinary users; carol's password is expired-proof but
  # she is disabled, to test rejected logins.
  samba-tool user create alice 'Passw0rd!2024' >/dev/null
  samba-tool user create bob 'Passw0rd!2024' >/dev/null
  samba-tool user create carol 'Passw0rd!2024' >/dev/null
  samba-tool user disable carol
fi
cp /var/lib/samba/private/krb5.conf /etc/krb5.conf
exec samba -i --debug-stdout -d 1
