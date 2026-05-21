package main

import (
	"encoding/json"
	"strings"
)

func parseJSON(body string, v any) error {
	return json.Unmarshal([]byte(body), v)
}

func trim(s string) string               { return strings.TrimSpace(s) }
func splitString(s, sep string) []string { return strings.Split(s, sep) }
