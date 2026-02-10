package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

func main() {
	// Backend 8081 - HTTP standard
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second + time.Second/2)
		fmt.Println("8081 alive")
		fmt.Fprintf(w, "Hello from backend on port 8081")
	})
	go func() {
		log.Println("Starting HTTP backend on :8081")
		if err := http.ListenAndServe(":8081", nil); err != nil {
			log.Fatal("8081 error:", err)
		}
	}()

	// Backend 8082 - TCP raw
	ln8082, err := net.Listen("tcp", "localhost:8082")
	if err != nil {
		log.Fatal("error during tcp listener 8082: ", err)
	}
	defer ln8082.Close()
	log.Println("Starting TCP backend on :8082")

	for {
		conn, err := ln8082.Accept()
		if err != nil {
			fmt.Println("8082 connection error:", err)
			continue
		}
		fmt.Println("8082: Accepted new connection, spawning goroutine")
		go func(c net.Conn) {
			defer c.Close()
			time.Sleep(time.Second)
		}(conn)
		// handleTCPConnection(conn, "8083")
	}
}

func handleTCPConnection(conn net.Conn, port string) {
	defer conn.Close()

	fmt.Printf("%s: New connection from %s\n", port, conn.RemoteAddr())

	// Read the HTTP request FIRST
	reader := bufio.NewReader(conn)
	requestLine, err := reader.ReadString('\n')
	if err != nil {
		fmt.Printf("%s: Error reading request: %v\n", port, err)
		return
	}

	// Read headers (until empty line)
	for {
		line, err := reader.ReadString('\n')
		if err != nil || line == "\r\n" || line == "\n" {
			break
		}
	}

	fmt.Printf("%s alive - Request: %s", port, strings.TrimSpace(requestLine))

	// Simulate processing delay
	time.Sleep(time.Second + time.Second/2)

	// Send HTTP response
	response := fmt.Sprintf("HTTP/1.1 200 OK\r\n"+
		"Content-Type: text/plain\r\n"+
		"Content-Length: %d\r\n"+
		"Connection: close\r\n"+
		"\r\n"+
		"Hello from backend on port %s", len("Hello from backend on port "+port), port)

	conn.Write([]byte(response))
}
