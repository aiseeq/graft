// Package pgsql writes values into PostgreSQL statements that cannot take
// parameters: DO blocks, DDL and psql scripts.
package pgsql

import (
	"strconv"
	"strings"
)

// Literal is s dollar-quoted, $graft$...$graft$ with a tag s does not hold:
// no escapes are read inside, whatever the session's
// standard_conforming_strings.
func Literal(s string) string {
	tag := "$graft$"
	for i := 1; strings.Contains(s+"$", tag); i++ {
		tag = "$graft" + strconv.Itoa(i) + "$"
	}
	return tag + s + tag
}
