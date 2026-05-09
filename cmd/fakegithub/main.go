package main

import (
	"log"
	"net/http"
	"os"

	"fenced/internal/fakegh"
)

func main() {
	s := &fakegh.Server{Root: "/repos", Backend: "/usr/libexec/git-core/git-http-backend", LLMKey: os.Getenv("LLM_KEY")}
	if _, err := os.Stat("/repos/demo/shop.git"); err != nil {
		if err := s.Seed("demo/shop", "/seed"); err != nil {
			log.Fatal(err)
		}
	}
	log.Fatal(http.ListenAndServe(":9000", s.Handler()))
}
