// Command origin serves the loopback application used by the Locust workload.
package main

import (
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

func webhook(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" || id != r.Header.Get("X-Delivery-ID") {
		http.Error(w, "delivery ID mismatch", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "could not read body", http.StatusBadRequest)
		}
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Delivery-ID", id)
	w.WriteHeader(http.StatusAccepted)
	w.Write(body)
}

func handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, r.URL.Query().Get("id"))
	})
	mux.HandleFunc("POST /webhook", webhook)
	return mux
}

func main() {
	port := flag.Int("port", 3000, "loopback port")
	cert := flag.String("cert", "", "TLS certificate file for an HTTPS fixture")
	key := flag.String("key", "", "TLS private key file for an HTTPS fixture")
	flag.Parse()
	if (*cert == "") != (*key == "") {
		log.Fatal("supply both -cert and -key for HTTPS")
	}
	srv := &http.Server{
		Addr:              "127.0.0.1:" + strconv.Itoa(*port),
		Handler:           handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if *cert != "" {
		log.Fatal(srv.ListenAndServeTLS(*cert, *key))
	}
	log.Fatal(srv.ListenAndServe())
}
