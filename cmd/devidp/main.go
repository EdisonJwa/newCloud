// Command devidp runs the fake IdP as a standalone process for manual testing.
//
// It is not part of the shipped binary and must never be deployed: it authenticates
// anyone who asks. Its purpose is to let the real `newcloud serve` process be
// exercised end to end on a developer machine, including the parts that only fail
// with a real database and a real HTTP redirect.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/EdisonJwa/newCloud/internal/fakeidp"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:19000", "listen address")
	clientID := flag.String("client-id", "newcloud", "OIDC client id")
	issuer := flag.String("issuer", "", "advertised issuer URL (required when -addr binds 0.0.0.0)")
	flag.Parse()

	idp, err := fakeidp.NewAt(*clientID, *addr, *issuer)
	if err != nil {
		log.Fatalf("devidp: %v", err)
	}
	defer idp.Close()

	fmt.Fprintf(os.Stderr, "devidp listening on http://%s\n", *addr)
	fmt.Fprintf(os.Stderr, "issuer: %s\n", idp.URL())
	fmt.Fprintf(os.Stderr, "client_id: %s\n", *clientID)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}
