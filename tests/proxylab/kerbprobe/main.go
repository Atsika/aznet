//go:build windows

// Command kerbprobe asks Windows' Kerberos SSPI package for a service ticket
// with explicit credentials, to show whether the client can reach a KDC and
// get a ticket, independently of any proxy.
//
//	set PROBE_PASS=... & kerbprobe <domain> <user> <spn>
//	kerbprobe "" alice@CORP.TEST HTTP/proxy-negotiate.corp.test
//	kerbprobe CORP.TEST alice HTTP/proxy-negotiate.corp.test
package main

import (
	"fmt"
	"os"

	"github.com/alexbrainman/sspi/kerberos"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: set PROBE_PASS=... & kerbprobe <domain> <user> <spn>")
		os.Exit(2)
	}
	domain, user, spn := os.Args[1], os.Args[2], os.Args[3]
	cred, err := kerberos.AcquireUserCredentials(domain, user, os.Getenv("PROBE_PASS"))
	if err != nil {
		fmt.Println("acquire credentials:", err)
		os.Exit(1)
	}
	defer cred.Release()
	ctx, _, token, err := kerberos.NewClientContext(cred, spn)
	if err != nil {
		fmt.Println("service ticket for", spn+":", err)
		os.Exit(1)
	}
	defer ctx.Release()
	fmt.Printf("ok: kerberos token of %d bytes for %s\n", len(token), spn)
}
