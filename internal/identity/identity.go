package identity

import (
	"crypto/rand"
	"fmt"
)

func GenerateNodeID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("viiwork-%x", b)
}
