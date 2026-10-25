package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// env reads a setting, falling back to def.
func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

// required reads a setting that has no default.
func required(name string) (string, error) {
	v := env(name, "")
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return v, nil
}

func envInt(name string, def int) (int, error) {
	v := env(name, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

// partnerKeys are the keys partner services present on admin routes,
// comma-separated so a key can be rotated.
func partnerKeys() ([]string, error) {
	raw, err := required("AETHER_PARTNER_KEYS")
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, k := range strings.Split(raw, ",") {
		k = strings.TrimSpace(k)
		if len(k) < 32 {
			return nil, fmt.Errorf("AETHER_PARTNER_KEYS: each key needs at least 32 characters")
		}
		keys = append(keys, k)
	}
	return keys, nil
}
