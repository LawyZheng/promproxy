package main

import (
	"net/http"

	"github.com/lawyzheng/promproxy/server"
)

func main() {
	// any router engine you like
	mux := http.NewServeMux()
	s := server.New(mux)

	http.ListenAndServe(":8080", s)
}
