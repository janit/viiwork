package cost

import (
	"bufio"
	"os"
	"strings"
)

// LoadDotEnv reads a .env file and sets environment variables.
// Existing env vars are NOT overridden. Missing file is silently ignored.
func LoadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil { return }
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") { continue }
		key, val, ok := strings.Cut(line, "=")
		if !ok { continue }
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}
