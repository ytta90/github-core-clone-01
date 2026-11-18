package main

import (
	"fmt"
	"os"
)

func main() {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	st := newStore(token)
	sv := &server{st: st}
	ln, port, err := listenTCP()
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	fmt.Printf("listening on %d\n", port)
	sv.serve(ln)
}
