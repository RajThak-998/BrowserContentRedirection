package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"
	"bcr_client/internal"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	eng := engine.New(
		engine.Config{ListenAddr: ":8081"},
		engine.Callbacks{
			// Keep noisy telemetry disabled while validating connection/shadow flow.
			OnVideoChunk:  nil,
			OnVideoUpdate: nil,
			OnLog: func(message string) {
				log.Println(message)
			},
		},
	)

	if err := eng.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
