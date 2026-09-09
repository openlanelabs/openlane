// OpenLane worker — River job processors + agent runners (spec §15).
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.Println("openlane worker starting (River client lands with first migration)")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("worker stopped")
}
