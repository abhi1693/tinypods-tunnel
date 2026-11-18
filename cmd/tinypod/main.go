package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/abhi1693/tinypods-tunnel/internal/agent"
)

func main() {
	if len(os.Args) != 3 || (os.Args[1] != "http" && os.Args[1] != "https") {
		fmt.Fprintln(os.Stderr, "usage: tinypod {http|https} <port>")
		os.Exit(2)
	}
	port, err := strconv.Atoi(os.Args[2])
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintln(os.Stderr, "port must be an integer between 1 and 65535")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := agent.Config{
		Scheme:   os.Args[1],
		Port:     port,
		RelayURL: os.Getenv("TINYPOD_RELAY"),
		CAFile:   os.Getenv("TINYPOD_CA_FILE"),
	}
	err = agent.Run(ctx, cfg, func(publicURL string) {
		fmt.Println(publicURL)
	})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
