package main

import "testing"

// listenIsLoopback decides whether the proxy may start with no webuiPassword.
// The management API answers with plaintext credentials, so a bind beyond
// loopback has to carry a password.
func TestListenIsLoopback(t *testing.T) {
	yes := []string{
		"127.0.0.1:8080",
		"127.0.0.1:8091",
		"[::1]:8080",
		"localhost:8080",
	}
	for _, addr := range yes {
		if !listenIsLoopback(addr) {
			t.Errorf("listenIsLoopback(%q) = false, want true", addr)
		}
	}

	no := []string{
		":8080",            // empty host means every interface
		"0.0.0.0:8080",     //
		"192.168.1.5:8091", //
		"example.com:8080", //
		"127.0.0.1",        // no port
		"::1:8080",         // unbracketed IPv6 is not a parseable address
		"",                 //
	}
	for _, addr := range no {
		if listenIsLoopback(addr) {
			t.Errorf("listenIsLoopback(%q) = true, want false", addr)
		}
	}
}
