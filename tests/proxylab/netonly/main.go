//go:build windows

// Command netonly runs a command with network credentials of another account,
// like `runas /netonly`, but takes the password from the environment so it can
// be used from a non-interactive session (SSH). The process keeps the caller's
// local identity and presents the given account to remote services, which is
// what SSPI's "current user" credentials pick up. No domain membership or
// domain controller contact is needed.
//
//	set NETONLY_PASS=... & netonly CORP\alice cmd /c "your command"
package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	advapi                      = windows.NewLazySystemDLL("advapi32.dll")
	procCreateProcessWithLogonW = advapi.NewProc("CreateProcessWithLogonW")
)

const logonNetCredentialsOnly = 2

func main() {
	if len(os.Args) < 3 || os.Getenv("NETONLY_PASS") == "" {
		fmt.Fprintln(os.Stderr, `usage: set NETONLY_PASS=... & netonly DOMAIN\user command [args...]`)
		os.Exit(2)
	}
	domain, user, _ := strings.Cut(os.Args[1], `\`)
	if user == "" {
		fmt.Fprintln(os.Stderr, "the account must be DOMAIN\\user")
		os.Exit(2)
	}
	cmdline := windows.ComposeCommandLine(os.Args[2:])

	u16 := func(s string) *uint16 { p, _ := windows.UTF16PtrFromString(s); return p }
	si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var pi windows.ProcessInformation
	cl := u16(cmdline)
	r, _, err := procCreateProcessWithLogonW.Call(
		uintptr(unsafe.Pointer(u16(user))), uintptr(unsafe.Pointer(u16(domain))),
		uintptr(unsafe.Pointer(u16(os.Getenv("NETONLY_PASS")))), logonNetCredentialsOnly,
		0, uintptr(unsafe.Pointer(cl)), windows.CREATE_UNICODE_ENVIRONMENT,
		0, 0, uintptr(unsafe.Pointer(&si)), uintptr(unsafe.Pointer(&pi)))
	if r == 0 {
		fmt.Fprintf(os.Stderr, "CreateProcessWithLogonW: %v\n", err.(syscall.Errno))
		os.Exit(1)
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)
	if ev, err := windows.WaitForSingleObject(pi.Process, windows.INFINITE); err != nil || ev != windows.WAIT_OBJECT_0 {
		fmt.Fprintf(os.Stderr, "wait: %v\n", err)
		os.Exit(1)
	}
	var code uint32
	windows.GetExitCodeProcess(pi.Process, &code)
	os.Exit(int(code))
}
