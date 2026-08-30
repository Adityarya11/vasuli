package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// loadDotEnv reads KEY=VALUE pairs from path into the process environment.
//
// A missing file is not an error: credentials may equally come from the
// shell or from flags, and requiring a .env would break those paths.
//
// Variables already present in the environment are left alone, so an
// explicitly exported value beats a stale entry in the file.
func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Tolerate `export KEY=VALUE`, which is what a file written for a
		// POSIX shell looks like.
		line = strings.TrimPrefix(line, "export ")

		key, value, found := strings.Cut(line, "=")
		if !found {
			return fmt.Errorf("%s line %d: expected KEY=VALUE", path, lineNo)
		}

		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)

		if key == "" {
			return fmt.Errorf("%s line %d: empty key", path, lineNo)
		}
		if _, alreadySet := os.LookupEnv(key); alreadySet {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("%s line %d: set %s: %w", path, lineNo, key, err)
		}
	}

	return scanner.Err()
}
