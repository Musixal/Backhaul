package utils

import (
	"fmt"
	"log"
	"net/http"
)

// handler function to process requests
func handler(w http.ResponseWriter, r *http.Request) {
	// Simulate some processing time
	log.Printf("handling request for %s", r.URL.Path)

	// Handle the "/" route
	if r.URL.Path == "/" {
		fmt.Fprintln(w, "Hello, World!")
		return
	}

	// Handle the "/hello" route
	if r.URL.Path == "/hello" {
		name := r.URL.Query().Get("name")
		if name == "" {
			name = "World"
		}
		fmt.Fprintf(w, "Hello, %s!", name)
		return
	}

	// Handle 404 for other paths
	http.NotFound(w, r)
}

func RunHTTPServer() {
	// Set up the routes with a goroutine to handle each request
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		go handler(w, r) // Handle each request in a goroutine
	})

	// Start the server on port 8080
	log.Println("starting server on :8080")
	if err := http.ListenAndServe("0.0.0.0:8080", nil); err != nil {
		log.Fatal(err)
	}
}
