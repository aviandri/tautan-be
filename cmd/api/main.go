// Command api runs the Tautan HTTP API against DATABASE_URL.
package main

import (
	"log"

	"github.com/aviandri/tautan-be/internal/config"
	"github.com/aviandri/tautan-be/internal/server"
)

func main() {
	if err := server.Run(config.Load()); err != nil {
		log.Fatal(err)
	}
}
