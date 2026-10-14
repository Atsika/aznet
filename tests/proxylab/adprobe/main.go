// Command adprobe logs in to a Kerberos realm with gokrb5, the library aznet
// uses on macOS and Linux, and optionally asks for a service ticket. It shows
// whether a Unix client can talk to a given KDC, independently of any proxy.
//
//	adprobe <kdc host:port> <realm> <user> <password> [<spn>]
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/jcmturner/gokrb5/v8/client"
	"github.com/jcmturner/gokrb5/v8/config"
)

func main() {
	if len(os.Args) < 5 {
		fmt.Fprintln(os.Stderr, "usage: adprobe <kdc host:port> <realm> <user> <password> [<spn>]")
		os.Exit(2)
	}
	kdc, realm, user, pass := os.Args[1], strings.ToUpper(os.Args[2]), os.Args[3], os.Args[4]
	cfg, err := config.NewFromString(fmt.Sprintf("[libdefaults]\n default_realm = %s\n dns_lookup_kdc = false\n udp_preference_limit = 1\n[realms]\n %s = {\n  kdc = %s\n }\n", realm, realm, kdc))
	if err != nil {
		fmt.Println("config:", err)
		os.Exit(1)
	}
	cl := client.NewWithPassword(user, realm, pass, cfg, client.DisablePAFXFAST(true))
	if err := cl.Login(); err != nil {
		fmt.Println("login failed:", err)
		os.Exit(1)
	}
	fmt.Printf("login ok: %s@%s\n", user, realm)
	if len(os.Args) > 5 {
		tkt, key, err := cl.GetServiceTicket(os.Args[5])
		if err != nil {
			fmt.Println("service ticket failed:", err)
			os.Exit(1)
		}
		fmt.Printf("service ticket ok: %s, enctype %d, kvno %d\n", tkt.SName.PrincipalNameString(), key.KeyType, tkt.EncPart.KVNO)
	}
}
