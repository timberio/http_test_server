package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type key int

const (
	requestIDKey key = 0
)

var (
	healthy int32
)

var summaryPath = "/tmp/http_test_server_summary.json"

type ESVersion struct {
	Number string `json:"number"`
}

type ESMeta struct {
	Version *ESVersion `json:"version"`
}

type Server struct {
	address      string
	ByteTotal    int64 `json:"byte_total"`
	file         *os.File
	FirstMessage string `json:"first_message"`
	LastMessage  string `json:"last_message"`
	logger       *log.Logger
	MessageCount int64 `json:"message_count"`
	RequestCount int64 `json:"request_count"`
	server       *http.Server
}

func (s *Server) Listen() {
	var gracefulStop = make(chan os.Signal)
	signal.Notify(gracefulStop, syscall.SIGTERM)
	signal.Notify(gracefulStop, syscall.SIGINT)

	go func() {
		sig := <-gracefulStop
		s.logger.Printf("Caught sig: %+v", sig)

		s.WriteSummary()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		s.server.SetKeepAlivesEnabled(false)
		if err := s.server.Shutdown(ctx); err != nil {
			s.logger.Fatalf("Could not gracefully shutdown the server: %v\n", err)
		}

		s.logger.Println("Server stopped")
		os.Exit(0)
	}()

	// Print debug output on an interval. This helps with providing insight
	// into activity without saturating IO.
	ticker := time.NewTicker(5 * time.Second)
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				log.Printf("Received %v messages across %v requests", s.MessageCount, s.RequestCount)
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()

	s.logger.Println("Server is ready to handle requests at", s.address)
	atomic.StoreInt32(&healthy, 1)
	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		s.logger.Fatalf("Could not listen on %s: %v\n", s.address, err)
	}
}

func (s *Server) WriteSummary() {
	sBytes, err := json.Marshal(s)
	if err != nil {
		log.Fatal(err)
	}

	err = ioutil.WriteFile(summaryPath, sBytes, 0644)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Wrote activity summary to %s", summaryPath)
}

func (s *Server) Index() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.RequestCount++

		contentType := r.Header.Get("Content-Type")
		contentLength := r.Header.Get("Content-Length")
		s.logger.Printf("Received request: content-type: %v, content-length: %v", contentType, contentLength)

		bodyBytes, err := ioutil.ReadAll(r.Body)
		if err != nil {
			s.logger.Printf("Error reading body: %v", err)
			http.Error(w, "can't read body", http.StatusBadRequest)
			return
		}

		byteLen := len(bodyBytes)
		body := string(bodyBytes)
		messages := []string{}

		switch contentType {
		// Unfortunately fluentbit does not use the proper content type when sending
		// new line delimited JSON :(
		case "application/json":
			messages = strings.Split(body, "\n")
		case "application/ndjson":
			messages = strings.Split(body, "\n")
		case "application/x-ndjson":
			messages = strings.Split(body, "\n")
		case "text/plain":
			messages = strings.Split(body, "\n")
		}

		messageCount := len(messages)

		if messageCount > 0 {
			s.ByteTotal = s.ByteTotal + int64(byteLen)
			s.MessageCount = s.MessageCount + int64(messageCount)

			firstMessage := messages[0]
			lastMessage := messages[messageCount-1]

			if s.FirstMessage == "" && firstMessage != "" {
				s.FirstMessage = messages[0]
			}

			if lastMessage != "" {
				s.LastMessage = lastMessage
			}
		}

		w.WriteHeader(http.StatusNoContent)
		fmt.Fprintln(w, "")
	})
}

func (s *Server) Health() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&healthy) == 1 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
}

func NewServer(address string) *Server {
	os.Remove(summaryPath)

	logger := log.New(os.Stdout, "http: ", log.LstdFlags)
	logger.Println("Server is starting...")

	router := http.NewServeMux()

	nextRequestID := func() string {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}

	httpServer := &http.Server{
		Addr:         address,
		Handler:      tracing(nextRequestID)(logging(logger)(router)),
		ErrorLog:     logger,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  15 * time.Second,
	}

	server := &Server{
		address:      address,
		ByteTotal:    0,
		logger:       logger,
		MessageCount: 0,
		RequestCount: 0,
		server:       httpServer,
	}

	router.Handle("/", server.Index())
	router.Handle("/_health", server.Health())

	return server
}

func logging(logger *log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				requestID, ok := r.Context().Value(requestIDKey).(string)
				if !ok {
					requestID = "unknown"
				}
				logger.Println(requestID, r.Method, r.URL.Path, r.RemoteAddr, r.UserAgent())
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func tracing(nextRequestID func() string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get("X-Request-Id")
			if requestID == "" {
				requestID = nextRequestID()
			}
			ctx := context.WithValue(r.Context(), requestIDKey, requestID)
			w.Header().Set("X-Request-Id", requestID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
