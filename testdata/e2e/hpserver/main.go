// hpserver exposes the hooks used by the stock-provider interoperability test.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/Deln0r/ygo/server"
)

func main() {
	noAuth := flag.Bool("no-auth", false, "accept keys without a token")
	flag.Parse()
	opts := server.Options{}
	if !*noAuth {
		opts.OnAuthenticateRequest = func(_ *http.Request, _ string, token string) (bool, error) {
			if token == "bad" {
				return false, errors.New("bad-token")
			}
			return token == "ro", nil
		}
	}
	var s *server.Server
	opts.OnStateless = func(name, payload string) { s.SendStateless(name, "reply:"+payload) }
	s = server.New(opts)
	defer s.Close(context.Background())
	mux := http.NewServeMux()
	mux.Handle("/collab", s.HocuspocusHandler())
	mux.Handle("/collab/", s.Handler())
	mux.Handle("/", s.Handler())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	fmt.Println("ws://" + ln.Addr().String())
	h := &http.Server{Handler: mux}
	go func() { _ = h.Serve(ln) }()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	_ = h.Close()
}
