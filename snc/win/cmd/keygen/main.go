// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

// keygen â€” ShortNerdCat activation key generator.
//
// Usage:
//
//	keygen -user <email> -pass <password> -key <api_key> \
//	       -server https://vpn.example.com [-server https://vpn2.example.com] \
//	       [-node <node_id>]
//
// Prints the encrypted key-string to stdout.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"tunnel_cat/snc/core"
)

type multiFlag []string

func (m *multiFlag) String() string  { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	var servers multiFlag

	user   := flag.String("user",   "", "username / email (required)")
	pass   := flag.String("pass",   "", "password (required)")
	apiKey := flag.String("key",    "", "Camerlengo API key (required)")
	nodeID := flag.String("node",   "", "node ID (optional, auto-generated if empty)")
	flag.Var(&servers, "server", "server URL, e.g. https://vpn.example.com (repeatable)")
	flag.Parse()

	if *user == "" || *pass == "" || *apiKey == "" {
		fmt.Fprintln(os.Stderr, "error: -user, -pass and -key are required")
		flag.Usage()
		os.Exit(1)
	}
	if len(servers) == 0 {
		fmt.Fprintln(os.Stderr, "error: at least one -server is required")
		flag.Usage()
		os.Exit(1)
	}

	kd := &core.KeyData{
		Username: *user,
		Password: *pass,
		Servers:  []string(servers),
		NodeID:   *nodeID,
		APIKey:   *apiKey,
	}

	ks, err := core.EncodeKeyString(kd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(ks)
}
