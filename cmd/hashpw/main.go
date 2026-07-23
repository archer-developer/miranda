// Command hashpw bcrypt-hashes a password for pasting into config.yaml's
// users[].password_hash — passwords never need to be stored or transmitted
// in plaintext, including at setup time.
//
// Usage: go run ./cmd/hashpw <password>
package main

import (
	"fmt"
	"os"

	"github.com/archer-developer/miranda/internal/users"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: hashpw <password>")
		os.Exit(1)
	}

	hash, err := users.HashPassword(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "hashpw:", err)
		os.Exit(1)
	}
	fmt.Println(hash)
}
