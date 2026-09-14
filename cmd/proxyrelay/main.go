// The stackr-proxyrelay binary: pipes TCP from its listen port to one fixed
// target, and exits once idle. Deliberately a separate main from the panel,
// a relay container must be structurally unable to boot server code.
package main

import (
	"flag"
	"log"
	"time"

	"github.com/FyrmForge/stackr/internal/proxyrelay"
)

func main() {
	listen := flag.String("listen", ":15000", "address to listen on")
	target := flag.String("target", "", "host:port to relay to (required)")
	idle := flag.Duration("idle", 60*time.Second, "exit after this long with no connections")
	flag.Parse()
	if *target == "" {
		log.Fatal("-target is required")
	}
	log.Printf("relaying %s -> %s (idle %s)", *listen, *target, *idle)
	if err := proxyrelay.Run(*listen, *target, *idle); err != nil {
		log.Fatal(err)
	}
	log.Print("idle, exiting")
}
