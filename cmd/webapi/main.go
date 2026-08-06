package main

import (
	"log"

	"github.com/adamrdrew/mosh/webapi"
)

func main() {
	config, err := webapi.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	api, err := webapi.New(config)
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(api.Run())
}
