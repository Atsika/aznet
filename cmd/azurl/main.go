package main

import (
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/atsika/aznet"
)

func main() {
	driverFlag := flag.String("driver", "azblob", "The driver type (azblob, azqueue, aztable)")
	urlFlag := flag.String("url", "http://localhost:10000/devstoreaccount1", "The service URL (e.g., account.blob.core.windows.net)")
	accountFlag := flag.String("account", "devstoreaccount1", "The Azure Storage account name")
	keyFlag := flag.String("key", "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==", "The Azure Storage account key")
	handshakeFlag := flag.String("handshake", aznet.DefaultHandshakeEndpoint, "Handshake endpoint name (container/queue/table)")
	tokenFlag := flag.String("token", aznet.DefaultTokenEndpoint, "Token endpoint name (container/queue/table)")
	expiryFlag := flag.Duration("expiry", 24*time.Hour, "SAS token expiry duration (e.g., 24h, 1h, 30m)")
	envFlag := flag.Bool("env", false, "Use credentials from environment variables (AZURE_STORAGE_ACCOUNT, AZURE_STORAGE_ACCOUNT_KEY)")

	flag.Usage = printUsage
	flag.Parse()

	urlStr := *urlFlag
	driver := strings.ToLower(*driverFlag)
	account := *accountFlag
	key := *keyFlag
	handshake := *handshakeFlag
	token := *tokenFlag
	expiry := *expiryFlag

	// Parse the URL - must be a valid URL with http:// or https:// scheme
	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		log.Fatalf("Invalid URL: %v", err)
	}

	// Validate that the scheme is http or https
	scheme := strings.ToLower(parsedURL.Scheme)
	if scheme != "http" && scheme != "https" {
		log.Fatalf("URL must have http:// or https:// scheme, got: %s", parsedURL.Scheme)
	}

	if parsedURL.Host == "" {
		log.Fatalf("URL must contain a valid host")
	}

	// Set credentials in environment if flags provided, so NewEndpoint can find them
	if !*envFlag {
		if account != "" {
			os.Setenv("AZURE_STORAGE_ACCOUNT", account)
		}
		if key != "" {
			os.Setenv("AZURE_STORAGE_ACCOUNT_KEY", key)
		}
	} else {
		// If -env is set, ignore flag values and let NewEndpoint find them in environment
		account = ""
		key = ""
	}

	// Create a listener to generate the connection string and endpoints
	l, err := aznet.Listen(driver, urlStr,
		aznet.WithEndpoints(handshake, token),
		aznet.WithSASExpiry(expiry),
	)
	if err != nil {
		log.Fatalf("Failed to create listener: %v", err)
	}
	defer l.Close() // This will cleanup handshake/token endpoints

	connStr, err := l.(*aznet.Listener).ConnectionString()
	if err != nil {
		log.Fatalf("Failed to generate connection string: %v", err)
	}

	fmt.Println(connStr)
}

func printUsage() {
	fmt.Println("azurl - Azure Storage Client URL Builder")
	fmt.Println("Usage:")
	fmt.Println("  azurl [-driver <type>] -url <url> -account <account> -key <key> [-handshake <name>] [-token <name>] [-expiry <duration>] [-env]")
	fmt.Println()
	fmt.Println("Example:")
	fmt.Println("  azurl -driver aztable -url http://localhost:10002/devstoreaccount1 -account devstoreaccount1 -key Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==")
	fmt.Println("  azurl -url https://account.blob.core.windows.net -account account -key key -expiry 1h")
	fmt.Println("  azurl -url https://account.blob.core.windows.net -env")
}
