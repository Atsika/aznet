package main

import (
	"fmt"
	"io"
	"log"
	"os"

	"github.com/atsika/aznet"
)

// Driver to use
var driver = "azblob"

// Blob URL
var u = "http://localhost:10000/devstoreaccount1?handshake=c2U9MjAyNi0wMi0wMlQxMSUzQTU1JTNBNDRaJnNpZz1GU3V0RVhETzM1UFNXdXVSYkNEZ0NRbHR2SSUyRkpDcHJERlZFRkpwVGpWTDAlM0Qmc3A9YWN3JnNwcj1odHRwcyUyQ2h0dHAmc3I9YyZzdD0yMDI2LTAyLTAxVDExJTNBNTAlM0E0NFomc3Y9MjAyNS0xMS0wNQ%3D%3D&token=c2U9MjAyNi0wMi0wMlQxMSUzQTU1JTNBNDRaJnNpZz1XQnRXdmFzZVdYMmROYkVOaUNmTTklMkJ3MEVUZ3ZxRGdqJTJCZGVJSHdVZ0w5QSUzRCZzcD1ybCZzcHI9aHR0cHMlMkNodHRwJnNyPWMmc3Q9MjAyNi0wMi0wMVQxMSUzQTUwJTNBNDRaJnN2PTIwMjUtMTEtMDU%3D"

// Queue URL
// var u = "http://localhost:10001/devstoreaccount1?handshake=c2U9MjAyNi0wMS0xNVQxMyUzQTI0JTNBMjhaJnNpZz14MzBpZFFnSCUyRjZnZyUyRmgza3piWnJxS01jVnFYeVNlTVh4RUE2ZHVxdm40WSUzRCZzcD1hJnNwcj1odHRwcyUyQ2h0dHAmc3Y9MjAyMC0wMi0xMA%3D%3D&token=c2U9MjAyNi0wMS0xNVQxMyUzQTI0JTNBMjhaJnNpZz1HaUxPTjV5R09BNVNZOGdNejdsaTE3N3NOUE9WVEJ5RkFteXdFJTJCNWdvdjglM0Qmc3A9ciZzcHI9aHR0cHMlMkNodHRwJnN2PTIwMjAtMDItMTA%3D"

// Table URL
// var u = "http://localhost:10002/devstoreaccount1?handshake=c2U9MjAyNi0wMS0xNVQxMyUzQTI1JTNBMTNaJnNpZz03Z0g0NGUzd0pqS3FZVjczenRGVDVOS3clMkI4VTRDTkg0Ym1vQzR3Qzhxd1klM0Qmc3A9YSZzcHI9aHR0cHMlMkNodHRwJnN2PTIwMTktMDItMDImdG49aGFuZHNoYWtl&token=c2U9MjAyNi0wMS0xNVQxMyUzQTI1JTNBMTNaJnNpZz1RYjN4VndlWjNVQ0FiU0toYVdVRVZkWkxid243cDRlWVRoc1dXJTJGSm5ycGclM0Qmc3A9ciZzcHI9aHR0cHMlMkNodHRwJnN2PTIwMTktMDItMDImdG49dG9rZW4%3D"

// Echo client that works with blob, queue, and table transports.
// Pipes standard input to aznet connection and prints server responses to stdout.
// Usage: go run ./examples/echo/client
// Type lines and press Enter – the server echoes them back.
// Use Ctrl-D to end input.
func main() {
	conn, err := aznet.Dial(driver, u)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	fmt.Println("[aznet] connected")
	fmt.Printf("[aznet] local → %s\n", conn.LocalAddr())
	fmt.Printf("[aznet] remote → %s\n", conn.RemoteAddr())

	// Copy server messages to stdout
	go func() {
		if _, err := io.Copy(os.Stdout, conn); err != nil {
			log.Printf("read error: %v", err)
		}
	}()

	// Stream stdin to the connection until EOF (Ctrl-D) or pipe close.
	if _, err := io.Copy(conn, os.Stdin); err != nil {
		log.Printf("write error: %v", err)
	}

	fmt.Println("[aznet] client stopped")
}
